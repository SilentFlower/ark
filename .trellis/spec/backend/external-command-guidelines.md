# External Command Guidelines

> 调用 docker / restic / systemd-analyze 等外部命令的规约，以及凭证注入的红线。

---

## Overview

ark 几乎不自己实现功能，它的价值在**编排层**（ADR-001）：
知道该备份什么、按什么顺序恢复。真正干活的是 `restic`、`docker compose`、
`pg_dump`、`systemd-analyze` 这些外部二进制。

这意味着**子进程调用是本项目最主要的代码形态，也是最集中的风险点**：
凭证要传给子进程但不能泄漏给别的进程，命令可能挂死，
输出可能含密钥，参数可能被注入。这份文档把这些约束写死。

现有的参考实现是 `internal/doctor/doctor.go:286` 的 `runCommand`。
新增的 `internal/sshexec/`（P1-2）、`internal/restic/`（P2-2）等包应沿用同样的形态。

---

## Command Construction

### 用 argv 切片，绝不拼 shell 字符串

```go
// ✅ internal/doctor/doctor.go:262
argv := []string{"docker", "compose", "-f", cfg.Project.ComposeFile}
if cfg.Project.ProjectName != "" {
    argv = append(argv, "-p", cfg.Project.ProjectName)
}
argv = append(argv, "config", "--services")

// ❌ 绝不
exec.Command("sh", "-c", "docker compose -f "+path+" config --services")
```

清单里的路径、卷名、服务名都来自用户手写的 YAML。
一旦经过 shell，`;`、`$()`、空格、引号都会被重新解释。
`exec.Command` 直接 execve，参数原样传递，不存在这个问题。

**本地子进程里不应该出现 `sh -c`。** 需要管道时用 Go 的 `io.Pipe` 或
把上游命令的 `Stdout` 接到下游的 `Stdin`，不要交给 shell。

### 远程命令：shell 无法避开，改为强制转义

上面那条红线**只管本地**。走 SSH 时它不成立，因为 `sshd` 执行远程命令的方式
固定是 `$SHELL -c "<命令串>"`——**不管你写不写 `bash -c`，远程端一定会过一次 shell**。

因此远程侧的防线从「避开 shell」换成「**每个插值进去的值都必须转义**」：

```go
// ✅ 转义后再拼进远程命令串
func shellQuote(s string) string {
    return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

remote := fmt.Sprintf("docker compose -f %s exec -T %s pg_dump -d %s",
    shellQuote(cfg.Project.ComposeFile), shellQuote(t.Service), shellQuote(t.Database))
argv := []string{"ssh", target, remote}   // ssh 本身仍用 argv 切片

// ❌ 绝不
remote := "docker compose exec -T " + t.Service + " pg_dump -d " + t.Database
```

`service`、`database`、`name`、`paths` 全部来自用户手写的 YAML。
本地 `exec.Command` 靠 execve 天然免疫，远程没有这个保护——
一个卷名写成 `data; rm -rf /` 就会在生产机上执行。

**同时不要额外包一层 `bash -c`。** 远程端已经有一层 shell，再包一层
只是把转义做两遍，出错概率翻倍而安全性不变。

**也不需要 `set -o pipefail`。** 当前 5 种 target 的远程命令都是单条命令，
远程侧没有管道；管道在 hub 本地（ssh 的 `Stdout` 接 restic 的 `Stdin`），
退出码由 Go 侧的 `Wait` 拿（ADR-011）。将来真出现远程管道再单独论证。

**必查的测试断言**：给 `service` / `name` 传入含 `;`、`$(...)`、空格、单引号的值，
断言生成的远程命令串里这些字符全部落在引号内、且远程侧不产生额外命令。

### Scenario: 本地与 SSH 统一执行层

#### 1. Scope / Trigger

- 当上层需要在 hub 本地或目标机执行同一条命令时，统一依赖
  `internal/sshexec.Runner`，不要在业务代码中重复判断 `Host.Local`。
- 该边界承载命令注入防护、流式数据纯度和 SSH 退出状态，任何签名或生命周期
  变更都必须同步更新本节与测试。

#### 2. Signatures

```go
type Runner interface {
    Run(ctx context.Context, argv ...string) (string, error)
    Stream(ctx context.Context, argv ...string) (io.ReadCloser, func() error, error)
    Feed(ctx context.Context, stdin io.Reader, argv ...string) error
}

func NewLocal() Runner
func NewSSH(cfg config.SSH) (Runner, error)
```

`Run` 面向短命令；`Stream` 面向备份方向的 stdout 数据流；`Feed` 面向恢复方向的
stdin 数据流。具体实现保持非导出，上层只持有 `Runner`。

#### 3. Contracts

- `NewLocal` 使用 `exec.CommandContext(ctx, argv[0], argv[1:]...)`，不经过 shell。
- `NewSSH` 接收 `config.SSH` 的 `address`、`user`、`identity_file`、
  `known_hosts_file`，并固定 `-T`、`Compression=no`、`BatchMode=yes`、
  `StrictHostKeyChecking=yes`、`UserKnownHostsFile`、`IdentitiesOnly=yes`、
  `-i`、`-p`、`-l` 和 `--`。
- SSH 远程 argv 的每个元素分别做 POSIX 单引号转义，最终只向系统 `ssh`
  传递一条远程命令串；不得增加第二层 `bash -c`。
- `Run` 等待结束并返回 stdout/stderr 合并输出；非零退出时仍返回已产生的输出。
- `Stream` 只暴露 stdout。调用方必须先读到 EOF，再调用且只调用一次 `Wait`；
  stderr 仅在失败时进入诊断，不能混入业务数据。
- `Feed` 把 reader 直接连接到 stdin，不得整读或落临时文件；stdout 丢弃，
  stderr 仅用于失败诊断。
- 三种模式都使用调用方 context，本包不统一设置时长。取消或超时错误必须保留
  `errors.Is(err, context.Canceled/DeadlineExceeded)` 语义。
- 生产环境变量：无。localhost 集成测试使用可选的
  `ARK_SSH_TEST_ADDRESS`、`ARK_SSH_TEST_USER`、`ARK_SSH_TEST_IDENTITY_FILE`、
  `ARK_SSH_TEST_KNOWN_HOSTS_FILE`；缺失任一项时跳过。
- 错误可以包含命令、身份文件路径和 known_hosts 路径，但不得读取或输出文件内容，
  也不得打印完整环境变量集合。

#### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| argv 为空或命令名为空 | 三种入口立即返回中文错误，不 panic |
| `address` 不是 `host:port`、host 为空、端口越界 | `NewSSH` 返回包含字段上下文的错误 |
| user 为空 | `NewSSH` 返回 `ssh.user` 校验错误 |
| identity / known_hosts 为空或不是绝对路径 | `NewSSH` 返回对应字段校验错误，不读取文件 |
| 命令启动失败 | 返回带命令上下文且保留 `%w` 的错误 |
| context 取消或超时 | 返回错误可被 `errors.Is` 识别 |
| `Run` 非零退出 | 返回合并输出和非 nil error |
| `Stream` 非零退出或被信号终止 | stdout 读取结束后，`Wait` 返回非 nil error |
| `Feed` 非零退出 | 返回包含 stderr 诊断的非 nil error |

#### 5. Good / Base / Bad Cases

- Good：远程参数包含 `safe; $(touch /tmp/x); x'y with spaces`，远端 `printf`
  原样输出该值，且 `/tmp/x` 不存在。
- Base：`Stream(ctx, "pg_dump", "-d", database)` 返回纯 stdout，调用方读完后
  `Wait()` 为 nil。
- Bad：调用方只读 stdout、不调用 `Wait`，会把 SSH 中断或远程命令失败误判为成功。
- Bad：把恢复输入先 `io.ReadAll`，会让大体积备份占满内存并破坏流式约束。

#### 6. Tests Required

- 单元测试断言本地 argv 原样传递，shell 元字符不会创建额外文件。
- 单元测试断言 SSH 的全部固定选项、host/port/user/路径和最终远程命令串。
- 转义测试覆盖空字符串、空格、`;`、`$(...)`、换行和单引号。
- `Run` 覆盖成功、合并输出、非零退出和 context 取消。
- `Stream` 覆盖 stdout/stderr 隔离、成功、非零退出、SIGKILL 和 context 超时；
  断言失败由 `Wait` 返回。
- `Feed` 覆盖大输入原样传递、非零退出和 context 超时。
- localhost SSH 集成测试用 `testing.Short()` 和四个可选环境变量保护，并实际断言
  正常执行、参数不注入、远程非零退出和远程进程被 kill。

#### 7. Wrong vs Correct

##### Wrong

```go
remote := "docker compose exec -T " + service + " pg_dump -d " + database
cmd := exec.CommandContext(ctx, "ssh", host, "bash", "-c", remote)
stdout, _ := cmd.StdoutPipe()
_ = cmd.Start()
return stdout // 丢失 Wait，截断流可能被当作成功
```

##### Correct

```go
runner, err := sshexec.NewSSH(*host.SSH)
if err != nil {
    return err
}
stdout, wait, err := runner.Stream(ctx, "docker", "compose", "exec", "-T",
    service, "pg_dump", "-d", database)
if err != nil {
    return err
}
// 调用方先消费 stdout，再用 wait 校验 SSH 与远程命令的最终退出状态。
if err := consume(stdout); err != nil {
    return err
}
return wait()
```

### 可选参数用条件 append

见上面的 `-p` 和 `--env-file`。不要为了省事传空字符串——
`docker compose -p "" ...` 与不传 `-p` 语义不同。

---

## Timeouts

**每一条外部命令都必须有超时。** 探测类命令用统一常量：

```go
// internal/doctor/doctor.go:24
// commandTimeout 是单条外部命令的超时时间。
// docker 在守护进程异常时可能长时间无响应，不设超时会让 doctor 整个卡死。
const commandTimeout = 15 * time.Second
```

```go
// internal/doctor/doctor.go:287
ctx, cancel := context.WithTimeout(ctx, commandTimeout)
defer cancel()
cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
```

15 秒只适用于 `--version`、`config --services` 这类瞬时探测。
**备份和恢复的实际执行不能套用这个值**——`pg_dump` 一个大库、
`restic backup` 上传几十 GB，都可能跑几十分钟。
这类命令的超时应该由调用方按 target 规模决定，或者干脆只依赖
外层 context 的取消（systemd 的 `TimeoutStartSec` 是最后一道防线）。

用 `exec.CommandContext` 而不是自己起 goroutine 数秒——
前者会在 context 取消时真的杀掉子进程。

### 先确认二进制存在

`exec.LookPath` 的错误信息比「exec: not found」清楚得多：

```go
// internal/doctor/doctor.go:132
if _, err := exec.LookPath(argv[0]); err != nil {
    r.add(name, StatusFail, "未找到可执行文件 %s", argv[0])
    return false
}
```

---

## Credentials

这一节是硬红线，违反会导致凭证泄漏到非预期的进程。

### 凭证只注入子进程的 Env，绝不用 os.Setenv

```go
// ✅ 只影响这一个子进程
cmd := exec.CommandContext(ctx, "restic", argv...)
cmd.Env = append(os.Environ(),
    "AWS_ACCESS_KEY_ID="+id,
    "AWS_SECRET_ACCESS_KEY="+secret,
)

// ❌ 绝不
os.Setenv("AWS_SECRET_ACCESS_KEY", secret)
```

`os.Setenv` 修改的是 ark 进程自己的环境，而 Go 的 `exec` 默认让子进程
**继承父进程环境**。设一次，之后所有 `docker compose exec`、
`pg_dump`、任何子进程都会带上对象存储凭证——包括那些跑在
用户容器里、可能被应用代码读到的进程。

`repo.env_file` 的内容读进内存后只能走这条路径。

### restic 密码用文件而不是环境变量

用 `RESTIC_PASSWORD_FILE` 指向 `repo.password_file`，
**不要用 `RESTIC_PASSWORD`**。

理由：环境变量在很多系统上可以被同用户的其他进程通过 `/proc/<pid>/environ`
读到，某些 `ps` 配置下也会显示。密码文件由 `doctor` 强制检查为 `0600`
（`internal/doctor/doctor.go:110`），权限边界更清晰。

同理，任何密钥都不要作为命令行参数传递——argv 对同机所有用户可见。

### 密钥文件权限由 doctor 兜底

`checkFile` 的 `forbiddenPerm` 参数表示「不允许出现的权限位」：

```go
// internal/doctor/doctor.go:110
checkFile(r, "repo.password_file", cfg.Repo.PasswordFile, 0o077)
```

`0o077` 即禁止同组和其他用户访问。新增密钥类文件时照此登记检查项。

---

## Output Handling

### 探测用 CombinedOutput，数据流用 Stdout

```go
// internal/doctor/doctor.go:291
out, err := cmd.CombinedOutput()
```

`CombinedOutput` 合并 stdout 和 stderr，适合拿版本号、诊断信息。
但**绝不能用它接数据流**：`pg_dump` 的转储必须走纯 `Stdout` 管道，
混进 stderr 的一行警告就会污染 SQL、损坏备份。

数据流场景把 `cmd.Stdout` 接到目标 writer（或下游命令的 `Stdin`），
`cmd.Stderr` 单独接一个 buffer 用于出错时诊断。

### 机器可读输出一律加 --json

restic 的人类可读输出格式会随版本变化，JSON 不会。
所有 restic 调用都带 `--json` 并解析结构化结果，不要用正则去抓文本
（roadmap P2-2 已定的设计点）。

### 错误信息带上命令，但要考虑脱敏

```go
// internal/doctor/doctor.go:293
return "", fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
```

带上 argv 是必要的——「docker 命令失败」这种信息无法排查。
当前所有调用点的 argv 都不含密钥（密码走文件、凭证走 Env），所以直接拼接是安全的。

**新增调用点时必须重新确认这一点**：如果某条命令的 argv 或输出可能
包含凭证，就不能无条件回显。错误信息会进 systemd journal，
而 journal 的读取权限比 `0600` 的密钥文件宽得多。

### 取第一行展示版本

```go
// internal/doctor/doctor.go:299
func firstLine(s string) string { /* ... */ }
```

`docker --version` 之类的输出只有第一行有用，其余是噪音。

---

## Docker Compose Specifics

### exec 必须带 -T

`docker compose exec -T <service> <cmd>`。不带 `-T` 时 compose 分配 TTY，
输出会混入控制字符并做 CRLF 转换，**二进制流被静默损坏**。
详见 `database-guidelines.md`。

### compose 定位三件套

`-f`（compose 文件）、`-p`（项目名）、`--env-file` 要与清单保持一致，
构造逻辑见 `internal/doctor/doctor.go:261` 的 `composeServices`。

`project_name` 显式声明是为了避免误操作同一台机器上的另一套部署
（`internal/config/config.go:91` 的注释）——compose 默认按目录名推导项目名，
两套部署放在同名目录下时会撞车。

### docker 可用不代表 compose 可用

v2 的 compose 是 docker 的插件，装了 docker 不等于装了它。
必须单独探测（`internal/doctor/doctor.go:147` 的 `checkDockerCompose`）。

---

## Delegating Validation

能让外部工具做的校验就不要自己实现。

`OnCalendar` 表达式的语法校验直接调 `systemd-analyze calendar`
（`internal/doctor/doctor.go:184`），而不是自己写一个近似的解析器：

```go
// internal/config/config.go:327
// validateSchedule 只检查非空。
// OnCalendar 的语法校验交给 doctor 调用 systemd-analyze 完成——
// 那里有 systemd 本身作为权威，比自己写一个近似的解析器可靠。
```

自己写的近似实现会在边缘语法上与 systemd 产生分歧，
而分歧的方向通常是「ark 说合法、systemd 拒绝」——
结果是 timer 根本没装上，备份从第一天起就没跑过。

---

## Common Mistakes

- **`os.Setenv` 注入凭证**，泄漏给后续所有子进程。
- **用 `sh -c` 拼本地命令**，清单里的路径变成注入点。
- **拼远程命令时不转义**，卷名或服务名里的 `;`、`$()` 在生产机上被执行。
- **给远程命令再包一层 `bash -c`**，转义做两遍，出错概率翻倍。
- **忘记超时**，docker 守护进程异常时 ark 挂死，systemd 也看不出是卡住还是在跑。
- **给数据流用 `CombinedOutput`**，stderr 的一行警告污染转储。
- **解析 restic 的人类可读输出**，下次升级就崩。
- **`docker compose exec` 漏掉 `-T`**，二进制流被 TTY 损坏。
- **用 `RESTIC_PASSWORD` 或命令行参数传密码**，同机其他进程可见。
- **无条件回显子进程输出**，某些失败路径下工具会把凭证打出来。
