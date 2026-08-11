# 技术设计：远程 doctor

## 1. 边界与调用流

本任务把当前单入口 `doctor.Run` 拆成两个独立用例：

```text
ark doctor             ──► doctor.RunLocal ──► hub 文件 / 本地二进制 / restic 仓库
ark doctor --host X    ──► doctor.RunHost  ──► sshexec.Runner ──► local 或 SSH host
ark doctor --all       ──► RunLocal + 按清单顺序逐个 RunHost
```

`internal/doctor` 继续拥有检查项、依赖降级和三态报告；`internal/cli` 只选择范围、
合并报告并维持退出码。`internal/config` 仍只做静态校验，不新增文件系统或网络访问。

新增 `internal/doctor/doctor_remote.go` 放置 Runner 驱动的 host 检查；
`doctor.go` 保留报告类型、`RunLocal`、hub 检查和本地子进程辅助逻辑。

## 2. 公共 API 与报告合并

```go
func RunLocal(ctx context.Context, cfg *config.Config) *Report
func RunHost(ctx context.Context, cfg *config.Config, host *config.Host) *Report
```

旧 `Run` 删除，不保留兼容包装，因为它是 `internal` API，继续保留会让“默认检查全部”
和“默认只检查 hub”同时存在。CLI 合并报告时按实际执行顺序 append `Checks`：

1. `RunLocal` 的 hub 检查；
2. `--all` 时按 `cfg.Hosts` 顺序追加每个 `RunHost`；
3. `--host` 时只追加指定 host 的报告。

报告 JSON 仍只有 `checks` 字段，`Status`、`Check`、`Report.Failed` 和 `Counts`
不改变。检查项统一用 `<host> / <item>` 前缀；hub 全局项继续使用无 host 前缀的名称。

## 3. RunLocal

### 3.1 hub 能力与本地文件

`RunLocal` 按以下顺序执行：

1. 探测 `restic version`、`ssh -V`、`systemd-analyze --version`；
2. 检查 `repo.password_file` 与可选 `repo.env_file` 的存在性、普通文件类型和权限；
3. 检查每个远程 host 在 hub 上的 identity 与 known_hosts；
4. 用 `systemd-analyze calendar` 校验每个 host 的生效 schedule；
5. 前置条件满足时执行 `restic cat config`。

`RunLocal` 不再检查 docker、compose 或任何 host target。即使清单中的 hub host 是
`local: true`，它的项目环境也由 `RunHost` 负责；完整体检使用 `--all`。

### 3.2 restic 仓库探测

restic 探测使用独立 `exec.Cmd`，环境从 `os.Environ()` 复制后追加：

- `RESTIC_REPOSITORY=<repo.url>`；
- `RESTIC_PASSWORD_FILE=<repo.password_file>`；
- `repo.env_file` 中的对象存储凭证。

环境文件解析保持严格且最小：忽略空行和整行 `#` 注释，接受可选 `export ` 前缀，
按第一个 `=` 拆分并校验环境变量名；同名键以后出现的值覆盖前值。
值不做 shell 展开、命令替换或变量插值，避免把凭证文件变成代码执行入口。
匹配的单引号或双引号只去掉外层引号；格式错误直接让仓库探测 fail。

子进程环境通过 key 合并，保证 env 文件可以覆盖父进程中的同名对象存储变量，
而 `RESTIC_REPOSITORY` 和 `RESTIC_PASSWORD_FILE` 最终由清单值强制覆盖。
报告和错误只写 env 文件路径与解析行号，不写值。

依赖降级：restic 不可用、密码文件失败或 env 文件失败时，
`repo.access` 记 warn“前置检查未通过，跳过仓库解锁”，避免重复制造第二个 fail。
`restic cat config` 自身失败时 `repo.access` 为 fail。

## 4. RunHost 与 Runner 选择

`RunHost` 先构造 Runner：

- `host.Local` 为 true：`sshexec.NewLocal()`；
- 否则：`sshexec.NewSSH(*host.SSH)`。

公共入口负责防御 nil config、nil host 或无效 SSH 配置，并把它们转成可定位的 fail，
不 panic。内部实现接收已经构造好的 Runner，便于单元测试注入可控假 Runner，
不新增新的导出接口。

远程 host 首先执行 `true` 作为 `connection` 检查。失败时 connection 为 fail，
并为 clock、docker、compose、项目文件和 targets 各写 warn；本地 host 的 connection
直接记 ok“本机执行”，不启动无意义的探测命令。

所有 Runner 短命令都由 doctor 在调用前派生 15 秒 context；Runner 本身不增加超时。

## 5. Host 检查依赖图

连接成功后按独立依赖执行：

```text
connection
├── clock: date +%s
├── project files: stat compose/env
├── docker: docker --version
│   ├── compose plugin: docker compose version
│   │   └── compose services: docker compose ... config --services
│   │       ├── postgres / redis target
│   │       └── image_digest target
│   └── volume target: docker volume inspect
└── files target: stat each path
```

关键点：docker 或 compose 失败不会阻止 files target；compose service 列表失败不会
阻止 volume target。只有真正依赖失败项的数据才降级为 warn，其余继续检查。

检查项名称固定为：

- `<host> / connection`
- `<host> / clock`
- `<host> / docker`
- `<host> / docker compose`
- `<host> / project.compose_file`
- `<host> / project.env_file`
- `<host> / compose.services`
- `<host> / target <target-id>`

## 6. 文件元数据共用判定

本地与远程路径读取方式不同，但判定只保留一份：

```text
本地 os.Stat ─────┐
                  ├─► pathMetadata{mode, perm} ─► 共用存在/类型/权限判定
远程 stat -c ... ─┘
```

远程执行 `stat -L -c "%f %a" -- <path>`：`-L` 与本地 `os.Stat` 一样跟随符号链接，
`%f` 提供十六进制原始 mode，`%a` 提供八进制权限位。解析后归一化为非导出的 metadata 结构，再交给和 `os.FileInfo` 相同的判定函数。
这样普通文件、目录和 `0600` 边界不会在本地与远程两条路径中漂移。

项目 compose/env 文件要求普通文件；files target 只要求路径存在，可以是文件或目录。
多个缺失 path 聚合到同一 target 的 fail 详情中。

## 7. 时钟偏移

远程 host 执行 `date +%s`。hub 在命令前后各记录一次时间，以两者中点近似远端响应
对应的 hub 时间，减少 SSH 往返延迟的影响：

```text
offset = abs(remoteEpoch - midpoint(localStart, localEnd))
```

- `offset <= 60s`：clock 为 ok；
- `offset > 60s`：clock 为 warn；
- 命令失败或输出无法解析：clock 为 warn，其他检查继续。

内部实现注入 `now func() time.Time` 供测试固定时间，不引入公共 clock 抽象。

## 8. CLI 范围选择

`newDoctorCmd` 新增 `--host string` 与 `--all bool`，并用 Cobra
`MarkFlagsMutuallyExclusive("host", "all")` 声明互斥。无标志时只跑 `RunLocal`。

为避免 CLI 测试依赖真实 SSH，增加非导出的 orchestration helper，参数包含
`runLocal` / `runHost` 函数；生产调用传入 `doctor.RunLocal` / `doctor.RunHost`，
测试传入记录调用顺序的 stub。该 helper 负责：

- 查找 host，未知名称返回中文错误；
- 默认、单 host、all 三种执行序列；
- 合并 `Report.Checks`；
- 不因一台 host 的 fail 停止 `--all` 后续 host。

命令使用 `cmd.Context()`，保留 `--json`、文本打印和退出码 0/1/2 语义。

## 9. 测试策略

### doctor 单元测试

- 通过临时 PATH 中的 helper 可执行文件验证 RunLocal 的二进制、schedule 和 restic；
- helper 把收到的 restic 环境写到测试文件，断言 env 文件值进入 restic，
  未进入其它命令，且清单的 repository/password 强制覆盖同名父环境；
- 假 Runner 记录 argv 并按命令返回结果，覆盖连接失败、依赖降级、文件 mode、
  service/volume/path、时钟偏移和 shell 元字符参数边界；
- local host 用临时文件与假命令验证和远程元数据判定一致。

### CLI 测试

- 默认只调用 RunLocal；
- `--host` 只调用匹配 host；
- `--all` 的调用顺序为 local 后逐 host；
- 互斥标志、未知 host、JSON 合并结果和 fail 到退出码 2 的映射。

### 可选集成测试

使用 `ARK_DOCTOR_TEST_CONFIG` 与 `ARK_DOCTOR_TEST_HOST` 指向专用清单和 host。
`testing.Short()`、未配置环境或缺少真实 docker/SSH 前提时跳过；显式运行时断言
报告包含 connection、docker、compose、项目文件和每个 target 检查项。

## 10. 文档、兼容与回滚

- README 的当前状态、快速开始和 doctor 用法更新为 local/host/all；
- `examples/ark.yaml` 只改顶部操作注释，完整体检使用 `ark doctor --all`；
- roadmap 勾选 doctor CLI 项并删除 P1-1 过渡说明；
- 清单 schema、Report JSON 和退出码不变；默认 doctor 从“过渡期检查本地 host target”
  调整为 ADR-008 定义的“只检查 hub”，这是 roadmap 明确要求的行为变化；
- 回滚时同时恢复旧 `doctor.Run` 和 CLI 默认调用，不涉及数据迁移。

## 11. 风险与延后

- repo env parser 本轮只承担 doctor 探测；P2-2 建立 `internal/restic` 后应复用或迁移，
  避免两套凭证解析行为。
- `stat -c` 与 `date +%s` 假设目标机是 roadmap 规定的 Linux；非 GNU/Linux 目标不支持。
- `--all` 串行可能较慢，但输出确定、资源压力低；并行检查延后到有真实规模数据后。
- SSH 登录成功不保证后续命令权限足够，具体权限问题由各检查项独立报告。
