# Brief — P1-2 SSH 执行层

## Goal

- 新增统一的本地/SSH `Runner`，让后续 doctor、备份和恢复在不区分远近的前提下安全执行命令，并可靠获知流式 SSH 命令的退出状态。

## Scope

- 新增 `internal/sshexec/client.go` 与 `client_test.go`。
- 实现 `Run`、`Stream`、`Feed` 三种执行模式，以及 `NewLocal` / `NewSSH`。
- 本地命令使用 argv 直接执行；SSH 使用系统 OpenSSH，逐参数生成安全的远程命令串。
- SSH 固定禁用 TTY、压缩和交互，强制主机密钥校验并只使用清单指定的 identity。
- 保持 stdout/stderr 分离，确保非零退出、进程被 kill、context 取消都不会伪装成功。
- 在现有 `doctor.Run` 中增加 `ssh -V` 运行时依赖检查。
- 增加伪进程单元测试和显式环境配置的 localhost SSH 集成测试。

## Non-Goals

- 不实现 P1-3 的远程 doctor 检查或 `RunLocal` / `RunHost` 拆分。
- 不实现 P1-4 的 `doctor --host` / `--all`。
- 不实现 restic、backup、restore 调用方或 target 命令构造。
- 不自行实现 SSH 协议、连接池或 ControlMaster。
- 不保证 context 取消能终止已经脱离 SSH session 的远程守护进程。

## Key Decisions

- 使用系统 `ssh`，不引入 `golang.org/x/crypto/ssh`，以复用 known_hosts、ProxyJump 和 ControlMaster。
- 保留 roadmap 指定的 `Runner` 接口；它是本地/远程执行的业务边界，不是仅为 mock 增加的抽象。
- 本地执行绝不经过 shell；远程端无法避开登录 shell，因此每个 argv 分别做 POSIX 单引号转义，且不额外包 `bash -c`。
- `Stream` 必须同时返回 reader 和显式 `Wait`；调用方读完 stdout 后检查 Wait，这是 ADR-011 防止截断快照的第一道防线。
- 不在执行层强加统一超时；短探测和长流任务均使用调用方 context，由上层选择 deadline。
- SSH 增加 `-T` 与 `IdentitiesOnly=yes`，避免 TTY 改写数据流，并保证认证只使用清单指定的 identity。

## Key Context

- `docs/roadmap.md` §P1-2 定义公共接口、OpenSSH 参数和集成验收。
- `docs/design.md` ADR-002、ADR-010、ADR-011 分别约束 agentless 架构、纯流式传输和 SSH 退出码检查。
- `config.SSH` 已存在，字段为 `Address`、`User`、`IdentityFile`、`KnownHostsFile`；`Address` 是显式 `host:port`。
- `.trellis/spec/backend/external-command-guidelines.md` 禁止本地 shell 拼接，要求远程逐参数转义和流数据/诊断分离。
- `internal/doctor/doctor.go` 已有 `exec.CommandContext` + 15 秒探测超时，本轮只增加 ssh 二进制检查，不改变过渡态远程 warn。
- 不新增 Go 模块依赖；系统 `ssh` 成为 hub 的运行时依赖。

## Risks / Deferred

- 远程转义依赖目标 Linux 用户的登录 shell 支持 POSIX 单引号语义；非 POSIX shell 不在本轮支持范围。
- stderr 当前用内存 buffer 保存诊断；若真实负载证明会产生超大 stderr，再引入带明确截断提示的限长实现。
- localhost 集成测试需要专用地址、用户、identity 和 known_hosts 环境变量；默认 `make check` 不依赖本机 sshd 配置。
- P1-3 才会让 `doctor --host` 真正消费该执行层并验证远程 compose/target。

## Acceptance

- `NewLocal` / `NewSSH` 满足 `Runner`，空 argv 返回错误而非 panic。
- 本地 argv 不经过 shell；SSH 固定参数、host/port/user/identity 与 known_hosts 均有断言。
- `;`、`$(...)`、空格、单引号、空字符串等远程参数不会产生额外命令。
- `Run` 返回合并输出并保留非零退出错误。
- `Stream` 保持 stdout/stderr 隔离；成功、非零退出、SIGKILL、context 取消均有测试，失败时 Wait 非 nil。
- `Feed` 原样流式传递 stdin；成功、非零退出和 context 取消均有测试。
- context 取消/超时可由 `errors.Is` 识别，错误不读取或输出密钥文件内容。
- `doctor` 报告新增 ssh 二进制检查，远程 host 的既有 warn 行为不变。
- localhost 集成测试覆盖正常、非零退出、远程进程被 kill，短模式跳过。
- `make check` 全绿，且不新增 Go 依赖。

## Next Step

- 经用户确认后运行 `task.py start`，再通过 `trellis-route(target=implement)` 进入实现。
