# P1-2 SSH 执行层

## Goal

新增 `internal/sshexec/`，把“在某台机器上执行一条命令”统一为本地与 SSH
两种 `Runner`。后续 `doctor`、备份和恢复执行器只依赖同一套命令接口，
同时确保远程 shell 参数不会成为命令注入点，流式任务的 SSH 退出状态不会被忽略。

来源：`docs/roadmap.md` §P1-2。
架构依据：`docs/design.md` ADR-002（hub 集中编排、目标机 agentless）、
ADR-010（数据流式经过 hub）、ADR-011（SSH 流截断必须主动检出）。

## Background

ark 现在只能通过 `internal/doctor` 的 `exec.CommandContext` 在 hub 本地执行短命令。
后续远程 doctor、备份和恢复需要在目标机执行同样的 docker / tar / pg_dump 命令，
而且备份方向要读取远程 stdout，恢复方向要把 restic 输出写入远程 stdin。

直接执行系统 `ssh` 是既定方向：复用 OpenSSH 的 `known_hosts`、ProxyJump、
ControlMaster 等成熟能力，不引入 `golang.org/x/crypto/ssh`。目标机仍然只需要
`sshd`，不安装 ark 或 restic。

## Requirements

### R1 统一 Runner 契约

`internal/sshexec` 导出以下接口和构造函数：

```go
type Runner interface {
    Run(ctx context.Context, argv ...string) (string, error)
    Stream(ctx context.Context, argv ...string) (io.ReadCloser, func() error, error)
    Feed(ctx context.Context, stdin io.Reader, argv ...string) error
}

func NewSSH(cfg config.SSH) (Runner, error)
func NewLocal() Runner
```

- `Run` 用于短命令，等待进程结束并返回合并输出。
- `Stream` 只把 stdout 暴露给调用方；调用方读完后必须调用返回的 `Wait`，
  `Wait` 必须反映 SSH/远程命令的真实退出状态。
- `Feed` 把调用方提供的 reader 原样接到命令 stdin，并等待命令结束。
- 三种入口都拒绝空 argv，不允许靠 panic 暴露调用错误。

这个接口是本地/远程执行的业务边界，不是为了测试而提前抽象；
上层选择 `Runner` 后不再分支判断 host 是否为本机。

### R2 本地执行不经过 shell

本地 Runner 使用 `exec.CommandContext(ctx, argv[0], argv[1:]...)`，
参数按 argv 原样传递，不使用 `sh -c` / `bash -c`，不做 shell 拼接。

### R3 SSH 使用系统 OpenSSH 且固定安全参数

SSH Runner 从 `config.SSH` 构造系统 `ssh` 命令，至少固定：

- `-T`：明确禁用伪终端，避免流内容被终端转换；
- `-o Compression=no`：数据由 restic 压缩，不重复消耗 hub CPU；
- `-o BatchMode=yes`：禁止无人值守任务卡在交互式密码提示；
- `-o StrictHostKeyChecking=yes`：不接受未知或变化的主机密钥；
- `-o UserKnownHostsFile=<known_hosts_file>`：使用清单指定的主机密钥库；
- `-o IdentitiesOnly=yes` 与 `-i <identity_file>`：只使用清单指定的身份；
- 从 `address` 拆出 host / port，并通过独立的 `-l` / `-p` argv 传入 user / port。

`NewSSH` 对手工构造的无效 `config.SSH` 返回可读错误；不读取私钥或
known_hosts 的内容，文件存在性与权限仍属于 `doctor`。

### R4 远程命令逐参数转义

OpenSSH 远程端必然通过登录 shell 执行命令串，因此 SSH Runner 必须：

- 对 `argv` 的每一个元素分别做 POSIX 单引号转义后再用空格连接；
- 正确处理空字符串、空格、`;`、`$(...)`、换行和单引号；
- 不额外包装 `bash -c`，不添加当前命令用不到的远程管道或 `pipefail`；
- 只把最终的一条远程命令串交给 `ssh`。

### R5 进程生命周期和取消必须可观察

- 三种模式都使用调用方传入的 context；短探测由调用方设置短超时，
  长时间备份/恢复由外层 context 或 systemd 超时控制，本包不强加统一时长。
- context 取消或超时时，返回错误必须可通过 `errors.Is` 识别对应的
  `context.Canceled` / `context.DeadlineExceeded`。
- `Stream` 不允许把 stderr 混入 stdout 数据流；stderr 仅用于失败诊断。
- `Stream` 的远程命令非零退出或被信号终止时，`Wait` 必须返回非 nil 错误。
- `Feed` 的远程命令非零退出或被取消时必须返回非 nil 错误。

### R6 错误信息遵守密钥边界

- 错误带上可定位的执行上下文和退出状态，但不打印环境变量集合。
- 可以报告 `identity_file` / `known_hosts_file` 的路径，不得读取或输出文件内容。
- 流式模式只把 stderr 用于诊断，绝不能把 stdout 数据片段拼进错误。

### R7 doctor 识别新增运行时依赖

在现有 `doctor.Run` 的 hub 全局检查中加入系统 `ssh` 二进制检查。
本任务不拆分 `RunLocal` / `RunHost`，该结构调整仍属于 P1-3。

### R8 测试覆盖高风险行为

- 单元测试使用可控的伪进程覆盖本地与 SSH Runner，不要求开发机启动 sshd。
- SSH 参数测试必须断言固定选项、host/port/user/identity 路径和远程命令串。
- 转义测试必须覆盖 `;`、`$(...)`、空格、单引号、空字符串等输入。
- `Stream` / `Feed` 覆盖成功、非零退出、进程被 kill、context 取消和数据不串流。
- 提供真实 localhost SSH 集成测试；`testing.Short()` 时跳过，且未提供专用测试
  连接配置时也跳过，避免 `make check` 依赖开发机 sshd 配置。

## Non-Goals

- 不实现 P1-3 的远程 doctor 检查和 `RunLocal` / `RunHost` 拆分。
- 不实现 P1-4 的 `doctor --host` / `--all` CLI 标志。
- 不实现 P2 的 restic、backup、restore 调用方或 target 命令构造。
- 不引入 SSH 连接池或自行实现 ControlMaster；连接复用交给用户的 OpenSSH 配置。
- 不承诺 context 取消能终止已经脱离 SSH session 的远程守护进程。

## Acceptance Criteria

- [ ] `NewLocal` 与 `NewSSH` 都满足 `Runner`，三种方法对空 argv 返回错误
- [ ] 本地 Runner 使用 argv 直执行；测试证明含 shell 元字符的参数不会产生额外命令
- [ ] SSH Runner 使用系统 `ssh`，固定禁用 TTY/压缩/交互并强制主机密钥校验与指定身份
- [ ] 远程 argv 的每个参数都被单独转义；`;`、`$(...)`、空格、单引号和空字符串均有测试
- [ ] `Run` 返回合并输出并保留非零退出错误
- [ ] `Stream` 的 stdout 不混入 stderr，且正常退出、非零退出、进程被 kill、context 取消均有测试
- [ ] `Feed` 原样传递 stdin，且正常退出、非零退出、context 取消均有测试
- [ ] context 取消/超时错误可通过 `errors.Is` 判断
- [ ] 错误和测试均不读取或输出 SSH 私钥、known_hosts 的内容
- [ ] `doctor` 新增 `ssh` 二进制检查，其他远程检查仍保持当前 warn 过渡行为
- [ ] localhost SSH 集成测试存在，并覆盖正常执行、远程命令非零退出、远程进程被 kill；短模式跳过
- [ ] 不新增 Go 依赖，`make check` 全绿
