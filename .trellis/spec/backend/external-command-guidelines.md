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
新增的 `internal/restic/`（P1-1）等包应沿用同样的形态。

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

**全项目不应该出现 `sh -c`。** 需要管道时用 Go 的 `io.Pipe` 或
把上游命令的 `Stdout` 接到下游的 `Stdin`，不要交给 shell。

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
（roadmap P1-1 已定的设计点）。

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
- **用 `sh -c` 拼命令**，清单里的路径变成注入点。
- **忘记超时**，docker 守护进程异常时 ark 挂死，systemd 也看不出是卡住还是在跑。
- **给数据流用 `CombinedOutput`**，stderr 的一行警告污染转储。
- **解析 restic 的人类可读输出**，下次升级就崩。
- **`docker compose exec` 漏掉 `-T`**，二进制流被 TTY 损坏。
- **用 `RESTIC_PASSWORD` 或命令行参数传密码**，同机其他进程可见。
- **无条件回显子进程输出**，某些失败路径下工具会把凭证打出来。
