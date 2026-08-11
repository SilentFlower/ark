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

### Scenario: doctor 的 hub 与 host 环境探测

#### 1. Scope / Trigger

- 当 `doctor` 新增或修改 hub 本地检查、单台 host 检查、CLI 范围选择、凭证环境或
  远程文件元数据判定时，必须同步维护本节。
- `config.Validate` 只负责清单静态语义；真实文件、命令、仓库、compose 和 target
  状态只能由 `doctor` 检查，不能把运行环境访问下沉到 `config`。

#### 2. Signatures

```go
func RunLocal(ctx context.Context, cfg *config.Config) *Report
func RunHost(ctx context.Context, cfg *config.Config, host *config.Host) *Report
```

CLI 范围固定为：`ark doctor` 只调用 `RunLocal`；`--host <name>` 只调用匹配的
`RunHost`；`--all` 先调用 `RunLocal`，再按 `cfg.Hosts` 顺序逐台调用 `RunHost`。
`--host` 与 `--all` 互斥。

#### 3. Contracts

- `RunLocal` 只检查 hub：`restic`、`ssh`、`systemd-analyze`、仓库密码与 env 文件、
  SSH identity/known_hosts、OnCalendar 语法，以及 `restic cat config` 仓库解锁。
- 仓库密码、repo env 和 SSH identity 权限不得宽于 `0600`。repo env 只接受
  `KEY=VALUE`、可选 `export `、空行和整行注释；不得执行 shell 展开、命令替换或
  变量插值，错误不得包含 value。
- restic 环境以 `cmd.Env` 隔离注入；`RESTIC_REPOSITORY` 和
  `RESTIC_PASSWORD_FILE` 最终必须由清单值覆盖。禁止 `os.Setenv`，失败时禁止回显
  完整环境或 restic 输出。
- `RunHost` 对 local host 使用 `sshexec.NewLocal()`，对远程 host 使用
  `sshexec.NewSSH(*host.SSH)`；所有目标机短命令都通过 `Runner.Run` 执行，并各自使用
  15 秒 context。doctor 不自行拼 SSH 或 shell 命令。
- 远程文件元数据命令固定为 `stat -L -c "%f %a" -- <path>`。`-L` 必须保留，
  因为本地 `os.Stat` 会跟随符号链接；两条路径统一归一化为文件类型与权限位后，
  再走同一判定函数。
- 时钟检查执行 `date +%s`，用命令前后 hub 时间的中点计算绝对偏移；偏移大于
  60 秒为 warn，不阻断其它检查。
- `--all` 串行执行且单台失败不阻止后续 host。合并报告继续使用 `checks` JSON
  字段和 `ok` / `warn` / `fail` 三态；warn 不触发检查失败退出码。

#### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| `RunLocal` 的 config 为空 | 记录 `config` fail，不 panic |
| `RunHost` 的 config 或 host 为空 | 记录可定位的 fail，不 panic |
| restic、密码文件或 repo env 前置检查失败 | 前置项 fail，`repo.access` warn 并跳过解锁 |
| SSH 登录失败 | connection fail；clock、docker、compose、项目文件和全部 target 为 warn |
| docker 不可用 | docker fail；compose 与依赖 docker 的 target 为 warn；项目文件和 files target 继续 |
| compose 文件或服务解析失败 | 对应项 fail；service/image target 为 warn；volume/files target 独立检查 |
| service、volume 或 path 确定不存在 | 对应 target fail；同一 host 的其它独立检查继续 |
| 时钟命令失败、输出无效或偏移大于 60 秒 | clock warn；其它检查继续 |
| `--host` 与 `--all` 同时使用，或 host 不存在 | 返回工具错误，退出码 1，不执行范围内检查 |
| 已执行报告包含 fail | 完整输出报告后返回检查失败，退出码 2 |

#### 5. Good / Base / Bad Cases

- Good：repo env 中同名对象存储键只进入 restic 子进程，清单仍强制覆盖仓库地址和
  密码文件；其它 doctor 子进程看不到新增凭证。
- Base：`ark doctor --all --json` 按 local、host 清单顺序合并为一份报告，某台
  host fail 后仍继续下一台。
- Bad：SSH 失败后把未执行的 target 标为 fail 或 ok，会把“无法判断”伪装成确定结论。
- Bad：远程使用不带 `-L` 的 `stat -c`，符号链接在 local 与 SSH host 上会得到不同
  文件类型或权限结论。

#### 6. Tests Required

- `RunLocal` 覆盖二进制、密钥权限、OnCalendar、repo env 严格解析、环境覆盖、
  `restic cat config` 成败，以及敏感值不进入错误或其它子进程。
- `RunHost` 用可控 Runner 覆盖 local/SSH 选择、登录失败降级、docker/compose 依赖、
  service/volume/path/image target、60 秒边界和每条命令 15 秒超时。
- 文件元数据测试必须断言远程 argv 精确包含 `stat -L -c "%f %a" --`，并用符号链接
  证明本地与远程判定一致。
- 参数边界测试必须证明 compose 路径、项目名、service、volume 和 files path 始终是
  独立 argv，shell 元字符不被 doctor 重新解释。
- CLI 测试覆盖默认 local、`--host`、`--all`、互斥、未知 host、顺序合并、JSON
  结构和退出码 0/1/2。
- 真实 host 集成测试必须由 `testing.Short()` 和专用环境变量保护，默认检查不得依赖
  SSH 或 docker 环境。

#### 7. Wrong vs Correct

##### Wrong

```go
// 不跟随符号链接，和本地 os.Stat 的语义不一致。
out, err := runRunner(ctx, runner, "stat", "-c", "%f %a", "--", path)
```

##### Correct

```go
// -L 让 local 与 SSH host 共用同一套文件类型和权限判定。
out, err := runRunner(ctx, runner, "stat", "-L", "-c", "%f %a", "--", path)
```

### Scenario: restic 仓库命令边界

#### 1. Scope / Trigger

- 当上层需要初始化仓库、写入 stdin 快照、查询/删除快照、dump 文件或检查仓库时，
  统一依赖 `internal/restic.Repo`，不得在 backup、restore 或 CLI 中重复构造 restic 命令。
- 该边界同时负责凭证环境隔离、JSON 契约、退出码判定和 dump 子进程生命周期；
  任一签名或行为变化都必须同步更新本节与测试。

#### 2. Signatures

```go
type Snapshot struct {
    ID       string
    Time     time.Time
    Hostname string
    Paths    []string
    Tags     []string
}

func New(cfg *config.Repo) (*Repo, error)
func (r *Repo) EnsureInit(ctx context.Context) error
func (r *Repo) BackupStdin(ctx context.Context, stdin io.Reader, filename string, tags []string) (Snapshot, error)
func (r *Repo) Snapshots(ctx context.Context, tags []string) ([]Snapshot, error)
func (r *Repo) Forget(ctx context.Context, policy config.Retention, tags []string) error
func (r *Repo) ForgetPolicy(ctx context.Context, policy config.Retention, tags []string) error
func (r *Repo) Prune(ctx context.Context) error
func (r *Repo) ForgetSnapshot(ctx context.Context, id string) error
func (r *Repo) Dump(ctx context.Context, snapshotID, path string) (io.ReadCloser, error)
func (r *Repo) Check(ctx context.Context) error
```

#### 3. Contracts

- `repo.env_file` 沿用受限 `KEY=VALUE` 语法；解析和环境合并由 `internal/envfile`
  统一持有，doctor 与 restic 不得各写一套语义。
- 父进程中的 `RESTIC_REPOSITORY`、`RESTIC_REPOSITORY_FILE`、`RESTIC_PASSWORD`、
  `RESTIC_PASSWORD_FILE`、`RESTIC_PASSWORD_COMMAND` 先移除；清单再强制注入
  `RESTIC_REPOSITORY` 与 `RESTIC_PASSWORD_FILE`。
- env 文件不得设置 `RESTIC_PASSWORD`、`RESTIC_PASSWORD_COMMAND` 或
  `RESTIC_REPOSITORY_FILE`；错误只报告 key 和文件路径，不报告 value。
- `BackupStdin` 使用 `backup --json --stdin --stdin-filename`，标签逐项追加，
  只接受唯一且含非空 `snapshot_id` 的 summary；不得整读 stdin 或落临时文件。
- `Snapshots` 解析 JSON 数组，只暴露 ark 实际使用的稳定字段，并按时间、ID 升序返回。
- `Forget` 映射非零 daily/weekly/monthly 值并统一 `--prune`；`ForgetSnapshot`
  只删除明确 ID，不猜测其它快照。
- 多 host 编排使用 `ForgetPolicy` 只执行不带 `--prune` 的 forget，全部 policy
  结束后调用一次 `Prune`；不得在 host 循环内反复 prune。
- `Dump` 的 `ReadCloser` 在读到 EOF 时执行 Wait；提前 Close 时关闭 pipe 后执行 Wait。
  非零退出必须通过最终 Read 或 Close 返回，不能只暴露 stdout。
- 实际 backup、forget、dump、check 只使用调用方 context，不套 doctor 的 15 秒超时。

#### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| config 为空、URL/密码文件为空、后端类型不是 restic | `New` 返回中文错误，不启动命令 |
| env 文件语法非法或含禁用控制变量 | 返回只含路径、行号或 key 的错误，不泄漏 value |
| `cat config` 成功 | `EnsureInit` 幂等返回，不执行 init |
| `cat config` 退出码为 10 | 才允许执行 init |
| `cat config` 为 1、11、12 或未知退出码 | fail closed，不执行 init，保留错误链 |
| filename、tag、snapshot ID、dump path 为空 | 立即返回参数错误 |
| backup JSON 损坏、缺 summary、重复 summary 或缺 snapshot ID | 备份失败并完成 pipe/Wait 清理 |
| restic stderr 可能含凭证 | 不无条件拼进错误；错误仍包含脱敏命令与退出状态 |
| context 取消或超时 | 返回错误可被 `errors.Is` 识别 |
| dump 非零退出或提前 Close | 最终 Read 或 Close 返回错误，不遗留子进程 |

#### 5. Good / Base / Bad Cases

- Good：父进程和 env 文件都带同名仓库变量时，只有清单 URL/密码文件与对象存储
  凭证进入当前 restic 子进程，后续 ssh/docker 子进程看不到新增凭证。
- Base：`EnsureInit` 连续调用两次，第一次只在退出码 10 时 init，第二次只执行
  `cat config`；stdin backup 能从 JSON summary 返回快照 ID。
- Bad：把退出码 1 一律当成“未初始化”，会在鉴权、网络或仓库损坏时错误执行 init。
- Bad：`Dump` 直接返回 `StdoutPipe` 而不接管 Wait，会把截断或后端失败伪装成 EOF。

#### 6. Tests Required

- 精确断言所有 API 的 argv、重复 `--tag`、保留策略和 `--prune`。
- 精确断言 `ForgetPolicy` 不带 `--prune`，`Prune` 单独执行 `prune --json`。
- 断言父进程冲突变量被移除、env 文件值只进入 restic、强制变量由清单覆盖，
  且错误不含 env value 或 restic stderr 内容。
- `EnsureInit` 覆盖成功、退出码 10 后 init 和密码错误不 init。
- backup 覆盖 status + summary、损坏 JSON、非零退出、context 取消和流式 stdin。
- snapshots 覆盖 JSON 字段和时间/ID 稳定排序。
- dump 覆盖成功、最终 Read 非零退出、提前 Close 和运行中 context 取消。
- 真实本地仓库测试覆盖 init → 重复 init → backup → snapshots → forget → check，
  并由 `testing.Short()` 与 `exec.LookPath("restic")` 保护。

#### 7. Wrong vs Correct

##### Wrong

```go
if err := exec.CommandContext(ctx, "restic", "cat", "config").Run(); err != nil {
    // 任何错误都 init，会把密码错误、锁冲突或损坏仓库误判成不存在。
    return exec.CommandContext(ctx, "restic", "init").Run()
}
```

##### Correct

```go
err := cmd.Run()
if err == nil {
    return nil
}
if code, ok := commandExitCode(err); !ok || code != 10 {
    return wrapCommandError(ctx, display, err)
}
return repo.run(ctx, "init")
```

### Scenario: target 产流执行器

#### 1. Scope / Trigger

- 修改 `internal/backup` 的 postgres、redis、volume、files 或 image_digest 执行器时，
  必须遵守本节。
- 执行器只把已校验清单转换为纯数据流和稳定元数据；restic、状态库、CLI、
  target 完整性判定和失败快照撤销属于调用方。
- local 与 SSH 目标必须使用调用方传入的 `sshexec.Runner`，本包不得自行创建
  `exec.Cmd`、SSH 连接、shell 或临时文件。

#### 2. Signatures

公开入口和结果字段保持为：

```go
type Result struct {
    Host          string
    TargetID      string
    TargetType    config.TargetType
    StdinFilename string
    Reader        io.ReadCloser
    Wait          func() error
    ImageDigests  map[string]string
}

func Execute(
    ctx context.Context,
    host config.Host,
    target config.Target,
    runner sshexec.Runner,
) (*Result, error)
```

`Reader` 消费完成后必须关闭；`Wait` 必须在读完数据后调用。结果包装层保证底层
`Close` 和 `Wait` 各至多执行一次，重复调用返回第一次结果。

#### 3. Contracts

- `StdinFilename` 固定为 `<host>/<target.ID()><suffix>`；suffix 分别是 `.sql`、
  `.rdb`、`.tar`、`.tar`、`.json`。禁止加入日期、run ID 或 snapshot ID。
- compose 前缀固定为 `docker compose -f <compose_file>`，随后条件追加
  `-p <project_name>`、`--env-file <env_file>`，顺序与 doctor 一致。
- 命令矩阵固定为：

| target | argv 尾部或完整 argv |
|---|---|
| postgres | `exec -T <service> pg_dump [-U <user>] -d <database> --no-owner --no-acl --clean --if-exists` |
| redis 基线/轮询 | `exec -T <service> redis-cli LASTSAVE` |
| redis 触发 | `exec -T <service> redis-cli BGSAVE` |
| redis 产流 | `exec -T <service> cat /data/dump.rdb` |
| volume | `docker run --rm -v <name>:/src:ro alpine tar -cpf - -C /src .` |
| files | `tar -cpf - -- <paths...>`，每条路径保持独立 argv |
| image_digest | compose `ps --format json`，再按容器 ID 和实际 image ID 做 inspect |

- Redis 必须在 BGSAVE 前读取 LASTSAVE 基线；触发后按 context 轮询，时间戳发生变化
  才能读取 `/data/dump.rdb`。不得直接复制正在写入的 RDB。
- image_digest 只接受 `State=running` 的 compose 容器。先从容器 inspect 读取实际
  image ID 与 `Config.Image` 仓库引用，再对实际 image ID 读取 `RepoDigests`；
  `Config.Image` 只用于选择仓库，最终值必须来自 `RepoDigests`。
- 同一 service 有多个运行容器时，所有容器必须解析出同一个 RepoDigest；JSON 输出
  是 `service -> RepoDigest` 的稳定键序对象，`ImageDigests` 与流内容语义一致。
- 实际 dump/tar 命令只使用调用方 context，不套 doctor 的 15 秒探测超时。

#### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| context、runner、host 或 compose_file 无效 | 启动任何命令前返回中文参数错误 |
| target 类型未知 | 返回包含 target ID 与类型的错误 |
| `Runner.Stream` 失败 | 不返回半初始化 Result，保留原错误链 |
| `Runner.Stream` 成功但 Reader 或 Wait 缺失 | 回收已有资源并返回组合错误 |
| Reader Close 或 Wait 失败 | 重复调用仍返回第一次错误，底层动作只执行一次 |
| Redis LASTSAVE 命令失败或输出不是非负整数 | 当前 target 失败，不触发后续阶段 |
| Redis 轮询期间 context 取消 | 停止轮询并保留 `errors.Is` 语义 |
| compose ps JSON 无效或目标 service 没有运行容器 | image_digest 失败，不回退到 compose tag |
| 容器 image ID / image ref 为空 | image_digest 失败，不猜测仓库 |
| RepoDigests 为空、目标仓库无匹配或匹配多项 | image_digest 失败，不任选候选 |
| 同一 service 的运行容器解析出多个 digest | image_digest 失败，不输出不确定映射 |

#### 5. Good / Base / Bad Cases

- Good：带空格、引号或 shell 元字符的 service、路径和 volume 名仍各自作为单个 argv
  交给 Runner；SSH 层统一转义，数据流 stdout 不混入 stderr。
- Base：postgres 未配置 user 时完全省略 `-U`；image target 的 services 顺序不同，
  仍产生相同 JSON 字节和 service 映射。
- Bad：把 files 路径拼成一条 shell 字符串，会重新引入命令注入并破坏含空格路径。
- Bad：直接用 `Config.Image` 的 tag 作为恢复版本；tag 可变，不能证明备份时实际运行镜像。

#### 6. Tests Required

- 五类执行器都用表驱动 fake Runner 测试，精确断言 argv、调用顺序、稳定文件名和
  `Host` / `TargetID` / `TargetType` 元数据。
- postgres 断言 `-T`、可选 `-U`、无 `-Fc` / gzip；volume 断言 `:ro` 和
  `tar -cpf`；files 断言 `--` 后路径保持独立 argv。
- Redis 覆盖基线、BGSAVE、未变化轮询、变化后 Stream、context 取消和各阶段错误。
- image_digest 覆盖 JSON Lines、稳定 service 排序、Docker Hub 别名、registry 端口、
  空 RepoDigests、仓库不匹配、多候选、多运行 digest 以及每级 inspect/解析失败。
- 共享流结果覆盖 Stream 启动失败、半初始化回收、Wait/Close 错误链和重复调用次数。
- 至少运行 `go test ./internal/backup -race -count=1`、`make check`、标准构建、
  `CGO_ENABLED=0 go build ./cmd/ark` 和 `git diff --check`。

#### 7. Wrong vs Correct

##### Wrong

```go
// tag 不是运行版本证明；拼 shell 也绕过了 Runner 的逐参数转义。
command := "docker compose exec " + target.Service + " pg_dump -d " + target.Database
digest := configuredImageTag
```

##### Correct

```go
argv := append(composeArgv(host.Project),
    "exec", "-T", target.Service, "pg_dump", "-d", target.Database)
reader, wait, err := runner.Stream(ctx, argv...)

// image_digest 从运行容器的 image ID 反查 RepoDigests；Config.Image 只筛选仓库。
```

### Scenario: target 流完整性与状态持久化

#### 1. Scope / Trigger

- 修改 `internal/backup.BackupTarget`、`TargetResult`、上游 Wait、坏快照撤销、
  历史字节比较或 `store.RunTarget` 映射时，必须遵守本节。
- 本场景是 ADR-011 的备份方向实现：restic 成功只证明下游接收结束，不能证明
  SSH/远程命令完整退出。
- manifest 和整体 backup 编排只能消费这里产生的最终结果，不得绕过完整性判定
  重新把 snapshot、bytes 或 status 拼成另一套事实。

#### 2. Signatures

```go
type TargetResult struct {
    Host         string
    TargetID     string
    TargetType   config.TargetType
    Status       store.Status
    Bytes        int64
    Duration     time.Duration
    SnapshotID   string
    Error        string
    ImageDigests map[string]string
}

func BackupTarget(
    ctx context.Context,
    runID string,
    source *Result,
    repo *restic.Repo,
    state *store.Store,
) (TargetResult, error)
```

公开入口使用具体 `*restic.Repo` 和 `*store.Store`；测试只在 backup 包内注入窄函数，
不得为了 mock 扩大公开接口体系。

#### 3. Contracts

- 固定顺序是 counting reader → `BackupStdin` → source Wait → source Close →
  必要时 `ForgetSnapshot` → 历史比较 → `RecordRunTarget`。
- restic 提前失败时顺序改为 Close → Wait，先切断上游 stdout，避免远程进程继续阻塞。
- 快照标签固定为 `host:<host>`、`target:<target-id>`、`run:<run-id>`；filename
  直接使用执行器的 `StdinFilename`，调用层不得改名。
- `BackupStdin` 成功后必须调用且只调用一次 Wait。Wait 非零且 snapshot ID 明确时，
  必须精确 `ForgetSnapshot(id)`；撤销失败与 Wait/Close 错误使用 `errors.Join` 并列返回。
- counting reader 只统计 restic 实际读取的字节，不整读、不缓存、不落临时文件。
- 历史基线来自 `LastSuccessfulTargetBytes(host, targetID)`。无历史或历史为 0 不比较；
  `current * 2 < previous` 才 warn，恰好 50% 不 warn。实现使用向上取整的一半避免乘法溢出。
- ok、warn、fail 都尝试写 `RunTarget`。context 已取消时，最终状态写入使用
  `context.WithoutCancel` 加 10 秒超时，避免取消事实丢失；restic 撤销仍使用原 context，
  不在取消后启动无界外部命令。
- 返回 error 保留底层错误链；`TargetResult.Error` 和 `RunTarget.Error` 只能写阶段级
  脱敏摘要，禁止复制上游 stderr、restic 底层详情或 SQLite detail。
- 状态库写入失败时，返回 `TargetResult.Status` 必须降为 fail；已产生的 snapshot ID
  和 bytes 保留用于审计，不能把“未持久化”伪装成成功。

#### 4. Validation & Error Matrix

| 条件 | 最终行为 |
|---|---|
| ctx、run ID、source、repo、store 或内部依赖无效 | 启动备份前返回参数错误 |
| restic 失败 | Close + Wait 回收；不猜 snapshot、不撤销；记录 fail |
| restic 成功但 snapshot ID 为空 | Wait + Close 后记录 fail，不调用 forget |
| restic 成功且 Wait 失败 | 精确撤销该 snapshot；记录 fail |
| Wait 与 forget 同时失败 | 两条错误链都可 `errors.Is`，脱敏摘要只列失败阶段 |
| Wait 成功但 Close 失败 | snapshot 保留，target 记录 fail，Close 错误可识别 |
| 历史查询失败 | snapshot 保留，target 记录 fail，返回查询错误链 |
| 无历史或历史 bytes=0 | status=ok，不做比例判断 |
| 当前 bytes 恰好为历史 50% | status=ok |
| 当前 bytes 低于历史 50% | status=warn，snapshot 保留，Error 写安全告警原因 |
| `RecordRunTarget` 失败 | 返回 fail 结果和持久化错误；安全摘要不含底层详情 |
| 原 context 已取消 | 清理仍执行；最终记录用最多 10 秒的收尾 context |

#### 5. Good / Base / Bad Cases

- Good：restic 返回 snapshot 后，远程 pg_dump Wait 非零；代码只撤销这个 ID，
  Store 记录 fail，返回错误同时识别 Wait 和可选 forget 错误。
- Base：首次备份没有历史，正常记录 ok；后续恰好 50% 仍是 ok，49% 才是 warn。
- Bad：`BackupStdin` 返回 nil 就直接标记成功，会把 SSH 截断产生的快照留作恢复输入。
- Bad：把 `cause.Error()` 原样写数据库，会把上游 stderr 或底层存储详情扩散到 hub/API。

#### 6. Tests Required

- 断言成功调用顺序、稳定 filename、三类标签、bytes、duration、snapshot ID、
  image digest 拷贝和完整 `RunTarget` 字段。
- 阈值表覆盖无历史、零基线、高于一半、恰好一半、低于一半、奇数基线和 int64 最大值。
- restic 失败断言 Close → Wait 且不 forget；Wait 失败断言 Wait → Close → 精确 forget。
- Wait/Close/Forget 多错误用 `errors.Is` 分别识别，底层动作各只调用一次。
- 覆盖 snapshot ID 缺失、history 失败、record 失败、context 取消及取消后的收尾写库。
- secret sentinel 必须仍存在于返回错误链，但不得出现在 `TargetResult.Error` 或
  `RunTarget.Error`。
- 使用临时 SQLite Store 验证 warn 会持久化且不会替代最近一次 ok 历史；运行
  `go test ./internal/backup ./internal/store -race -count=1`、`make check` 和双构建。

#### 7. Wrong vs Correct

##### Wrong

```go
snapshot, err := repo.BackupStdin(ctx, source.Reader, source.StdinFilename, tags)
if err == nil {
    return TargetResult{Status: store.StatusOK, SnapshotID: snapshot.ID}, nil
}
```

##### Correct

```go
snapshot, backupErr := repo.BackupStdin(ctx, counter, source.StdinFilename, tags)
waitErr := source.Wait()
if backupErr == nil && waitErr != nil {
    forgetErr := repo.ForgetSnapshot(ctx, snapshot.ID)
    return failedResult, errors.Join(waitErr, forgetErr)
}
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
