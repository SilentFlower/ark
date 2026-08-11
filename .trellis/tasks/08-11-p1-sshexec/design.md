# 技术设计：SSH 执行层

## 1. 包边界与依赖方向

新增 `internal/sshexec/client.go`：

```text
config.SSH ──► sshexec.NewSSH ──► 系统 ssh
                  ▲
doctor / backup / restore（后续任务）
```

`sshexec` 只负责进程启动、输入输出连接、远程 argv 转义和退出状态。
它不知道 docker、restic、target 或备份语义，也不读取任何密钥文件。

包依赖 `internal/config` 只是为了接收既有的 `config.SSH` 数据结构；
`config` 不反向依赖执行层。P1-3 起，上层根据 `Host.Local` 选择
`NewLocal()` 或 `NewSSH(*Host.SSH)`，选好后统一持有 `Runner`。

## 2. Runner 契约

```go
type Runner interface {
    Run(ctx context.Context, argv ...string) (string, error)
    Stream(ctx context.Context, argv ...string) (io.ReadCloser, func() error, error)
    Feed(ctx context.Context, stdin io.Reader, argv ...string) error
}
```

roadmap 明确要求这个接口，它解决的是本地/远程执行分歧，而不是为了 mock。
实现仍返回两个简单的非导出具体类型 `localRunner` / `sshRunner`，不再拆出
command builder、transport、session 等公共抽象。

公共约束：

- 空 argv 立即返回中文错误；
- context 原样交给 `exec.CommandContext`；
- 本包不创建统一超时，因为探测命令和数十 GB 流任务的时长不可共用；
- 命令失败时保留 `%w` 错误链；context 已结束时优先包装 `ctx.Err()`，
  让调用方能用 `errors.Is` 判断取消或超时。

## 3. 本地命令构造

本地路径始终是：

```go
exec.CommandContext(ctx, argv[0], argv[1:]...)
```

不经过 shell，不对 argv 做转义。转义是 shell 语法；本地 `execve` 已经天然保留
参数边界，额外转义反而会把引号当成真实参数内容。

## 4. SSH 命令构造

### 4.1 连接参数

`NewSSH` 用 `net.SplitHostPort` 拆 `config.SSH.Address`，并校验 user、
identity_file、known_hosts_file 非空。虽然清单加载路径已经调用 `Config.Validate`，
构造函数仍要防御手工创建的 `config.SSH`，避免生成含空安全参数的 ssh 命令。

每次调用构造的本地 argv 形态：

```text
ssh
  -T
  -o Compression=no
  -o BatchMode=yes
  -o StrictHostKeyChecking=yes
  -o UserKnownHostsFile=<known_hosts_file>
  -o IdentitiesOnly=yes
  -i <identity_file>
  -p <port>
  -l <user>
  -- <host>
  <quoted remote command>
```

使用 `-l` 而不是拼 `<user>@<host>`，让 user 与 host 始终是独立 argv；
`--` 终止本地 ssh 选项解析，避免以 `-` 开头的异常 host 被当作选项。
`IdentitiesOnly=yes` 保证显式配置的 identity 不会被 ssh-agent 中其它 key 替代，
让无人值守任务的认证行为可重复。

不设置固定 `ConnectTimeout`：调用方 context 是统一取消来源，后续 doctor 会给短命令
15 秒超时，备份与恢复使用更长的外层 deadline。也不在包内创建 ControlMaster；
OpenSSH 仍会读取用户配置，ProxyJump / ControlMaster 可按部署环境配置。

### 4.2 远程命令转义

OpenSSH 会把 host 后面的命令发给目标用户的登录 shell。为保持 argv 边界，
每个远程参数都转换为单引号包裹的 POSIX shell word：

```text
abc        -> 'abc'
a b        -> 'a b'
            -> ''
a'b        -> 'a'\''b'
$(id); x   -> '$(id); x'
```

然后只用单个空格连接这些已转义 word，作为 ssh 的最后一个 argv。
不添加 `bash -c`：远端已经有登录 shell，再包一层只会产生第二套转义语义。

测试必须直接检查最终远程命令串，并通过伪 SSH 执行器验证元字符没有变成额外命令。

## 5. 三种执行模式

### 5.1 Run

- 设置 stdin 为默认空输入；
- 调用 `CombinedOutput`，成功与失败都把合并输出作为第一个返回值交给调用方；
- error 只增加命令和退出上下文，不重复嵌入整份输出，避免同一内容被打印两遍。

`Run` 只面向版本、stat、compose service 列表等短命令，不用于数据库转储。

### 5.2 Stream

1. `StdoutPipe()` 获取纯数据 reader；
2. stderr 接独立 `bytes.Buffer`；
3. `Start()` 成功后返回 reader 与 `wait` closure；
4. 调用方读到 EOF 后调用 `wait`，closure 内执行 `cmd.Wait()`；
5. Wait 失败时把经过 trim 的 stderr 加进诊断，但绝不读取或回显 stdout。

`Start` 失败时立即关闭已经创建的 stdout pipe，避免描述符泄漏。
调用方必须且只能调用一次 `wait`；接口不提供“只拿 reader”的重载，
这是 ADR-011 的第一道防线。

### 5.3 Feed

- `cmd.Stdin = stdin`，输入流不做缓冲或整读；
- stdout 丢弃，stderr 独立捕获用于失败诊断；
- 同步等待进程结束并返回退出状态。

恢复数据可能很大，任何 `io.ReadAll(stdin)` 或临时文件中转都违反 ADR-010。

## 6. 错误与敏感信息

错误上下文包含 runner 类型、可读命令和底层错误链。当前项目的凭证不得进入 argv：
SSH 私钥与 known_hosts 只作为**路径**传给系统 ssh，文件内容从不由本包读取；
未来数据库或对象存储凭证也必须通过文件、环境或 stdin 传递。

流模式只捕获 stderr。远程程序的 stderr 仍按“可能敏感”处理：
不在本包主动打印，只作为返回 error 的诊断文本交给顶层统一输出。

## 7. 测试策略

单元测试在 `client_test.go` 中通过非导出的 command factory 注入 Go helper process，
不引入新的公共接口，也不依赖机器上的 sshd。测试分组：

- `shellQuote` 表驱动：普通值、空值、空格、`;`、`$(...)`、单引号、换行；
- SSH argv：全部固定选项、IPv4/IPv6 host、port、user、identity 和 known_hosts 路径；
- Local Run：stdout/stderr 合并、参数不经 shell、非零退出；
- Stream：stdout/stderr 隔离、成功、非零退出、SIGKILL、context 取消；
- Feed：大于一个 buffer 的输入原样传递、非零退出、context 取消；
- 构造与空 argv 的错误路径。

真实 localhost 集成测试仍保留，但只有在非 short 且显式提供
`ARK_SSH_TEST_ADDRESS`、`ARK_SSH_TEST_USER`、`ARK_SSH_TEST_IDENTITY_FILE`、
`ARK_SSH_TEST_KNOWN_HOSTS_FILE` 时运行。它对同一连接用 `Stream` 覆盖：

1. 正常输出并成功 Wait；
2. 含 `;`、`$(...)`、单引号和空格的参数原样返回，且不执行额外命令；
3. `sh -c 'exit 7'`，Wait 非 nil；
4. `sh -c 'kill -KILL $$'`，Wait 非 nil。

这样仓库保留真实 OpenSSH/sshd 协议覆盖，同时 `make check` 不绑定个人机器配置。

## 8. doctor 的最小改动

在当前 `doctor.Run` 的 hub 全局二进制检查中加入：

```go
checkBinary(ctx, r, "ssh", "ssh", "-V")
```

OpenSSH 的版本输出通常写 stderr，但现有 `runCommand` 使用 `CombinedOutput`，
可以正确读取。本轮不改变 doctor 的报告结构，也不提前实现远程 target 检查。

## 9. 风险与边界

- 单引号转义假设目标用户的登录 shell 兼容 POSIX 单引号语义；当前目标机为 Linux，
  与 roadmap 的 docker/sshd 前提一致。支持非 POSIX 登录 shell 不在本轮范围。
- context 取消会终止本地 ssh 客户端并关闭 session；已经自行 daemonize、脱离 session
  的远程进程不保证被杀，本包也不通过额外管理通道追踪它。
- stderr 可能很大；当前命令形态的 stderr 是诊断流而非业务数据，先用内存 buffer。
  如后续真实负载证明会失控，再引入有明确截断提示的限长 writer。
