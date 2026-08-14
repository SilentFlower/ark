# 路线图

> 这是一份**可以照着做**的执行计划。每个任务都写清了要动哪些文件、
> 关键设计已经定了什么、以及怎么算做完。
>
> 隔一段时间回来接手时，先读 [`design.md`](design.md) 的 §3 架构和 §4 ADR，
> 再从下面第一个未打勾的任务开始。

## 开发约定

- 提交前跑 `make check`（格式化 + vet + 测试），不绿不提交。
- 提交信息用中文，格式 `feat: / fix: / docs: / refactor: 简述`。
- **改变了某个设计决策，先更新 `design.md` 的 ADR，再改代码。**
  代码和文档不一致时，以文档为准去修代码——反过来会让这个项目在半年后失去可维护性。
- 新增外部依赖要在 `doctor` 里加对应检查。
- 涉及密钥的代码路径，错误信息里绝不能回显密钥内容。
- **任何经过 SSH 的数据流，都必须能检出中途截断**（ADR-011）。
  这是 agentless 架构下最容易产生「成功的坏备份」的地方，没有例外。

## 阶段排序的理由

**先把地基改对。** agentless 架构（ADR-002）让清单模型从单机变成多机、
让所有执行从本地变成远程。在旧模型上写备份逻辑，P1 的每一行都要返工。

**再让备份产出真实数据，最后做界面。** hub 页面展示的是备份跑出来的数据；
备份还没跑起来时做前端只能对着假数据调样式，等真实数据结构定下来还要返工。

**P4 能排在后面，是 ADR-005 换来的。** 调度留在 systemd 手里，
意味着没有 `ark-hub` 也能正常备份——界面是纯增益，不是前提。
如果当初把调度做进常驻进程，P4 就会变成 P2 的前置。

**P3 是整个项目的成败点。** 备份写出去容易，能在一台陌生机器上恢复回来才是目的。
P2 做完先别急着做 P4，把 P3 走通再说。

---

## P0 — 项目基础 ✅ 已完成（部分需返工）

- [x] 项目骨架、Makefile、构建与测试流水线
- [x] 备份清单数据模型（5 种 target 类型）
- [x] `ark validate`：静态语义校验，拒绝未知字段和错位字段
- [x] `ark doctor`：运行环境检查
- [x] 设计文档与 ADR

**返工范围**：架构从「每机 agent」改为「hub 集中编排 + agentless」后，
`internal/config` 的清单模型、`internal/doctor` 的检查方式、
`examples/ark.yaml` 都需要重做。5 种 target 类型的定义、
`KnownFields(true)` 的严格解析、错位字段拒绝、`Target.ID()`
这些核心逻辑可以原样保留——它们与「谁来执行」无关。

**返工进度**：`internal/config` 与 `examples/ark.yaml` 已在 P1-1 完成；
`internal/doctor` 已在 P1-3 拆分 hub 本地检查与单 host 环境检查。

---

## P1 — 地基改造

**阶段目标**：`ark validate` 能校验一份多机清单，
`ark doctor --host web-01` 能通过 SSH 把那台机器体检一遍。

粗估 2–3 天。

### P1-1 清单模型升级到多机 ✅ 已完成

**改动文件**：`internal/config/config.go`、`internal/config/config_test.go`

结构调整：

```go
type Config struct {
    Version  int          // 1 → 2，不兼容变更
    Repo     Repo         // 全局唯一仓库，URL 不再带 /<host>
    Defaults Defaults     // schedule + retention 的默认值
    Hosts    []Host
}

type Host struct {
    Host      string
    Local     bool        // true 表示 hub 自己，不走 SSH
    SSH       SSH         // Local 为 true 时必须为空
    Project   Project     // 沿用现有定义
    Targets   []Target    // 沿用现有定义
    Schedule  *Schedule   // 指针，nil 表示套用 Defaults
    Retention *Retention
}

type SSH struct {
    Address        string  // host:port
    User           string
    IdentityFile   string  // 绝对路径
    KnownHostsFile string  // 绝对路径，必填
    HostKeyPolicy  string  // accept-new（默认）或 strict
}
```

**已定的设计点**：

- `hostPattern` 与 `Target.ID()` 原样保留，`ID()` 的返回值前面加 host 段
  用作 `--stdin-filename`（见 P2-3）。
- **`known_hosts_file` 必填**。默认 `accept-new` 只允许首次连接自动记录，已记录主机
  的密钥变化仍会拒绝；`strict` 要求管理员预先写入。不给完全跳过校验的选项——
  hub 会把生产数据流经这条连接，中间人劫持意味着数据泄露和恢复投毒。
- `Local: true` 与 `SSH` 非空必须互斥，校验时拒绝。
- **host 名称全局唯一**，重复要报错（它是 restic tag 和恢复时的检索键）。
- `Version == 1` 时给出明确的迁移提示，而不是含糊的字段缺失错误。
  形如「检测到 v1 单机清单，请参考 examples/ark.yaml 迁移到 v2」。
- `Defaults` 与 per-host 覆盖用指针区分「没写」和「写了 0」。
  现有 `applyDefaults` 里「retention 三项同时为 0 才套默认值」的逻辑要相应重写。

**验收**：一份含 3 台机器（含一台 `local: true`）的清单能通过校验；
host 重名、`local` 与 `ssh` 并存、缺 `known_hosts_file`、v1 清单
这四种情况都有专门的测试用例并给出可读的错误。

---

### P1-2 SSH 执行层 `internal/sshexec/` ✅ 已完成

**新增文件**：`internal/sshexec/client.go`、`client_test.go`

把「在某台机器上跑一条命令」抽象掉，让上层执行器不关心本地还是远程。

```go
type Runner interface {
    // Run 执行命令并等待结束，返回合并输出。用于短命令。
    Run(ctx context.Context, argv ...string) (string, error)
    // Stream 执行命令并把 stdout 交给调用方读取。
    // 返回的 Wait 必须在读完 stdout 之后调用，它会返回远程退出码。
    Stream(ctx context.Context, argv ...string) (io.ReadCloser, func() error, error)
    // Feed 执行命令并把 stdin 喂给它，用于恢复方向。
    Feed(ctx context.Context, stdin io.Reader, argv ...string) error
}

func NewSSH(cfg config.SSH) (Runner, error)  // 远程
func NewLocal() Runner                        // local: true 的 host
```

**已定的设计点**：

- 用 `golang.org/x/crypto/ssh` 还是直接 exec `ssh` 命令？
  **倾向直接 exec `ssh`**：能复用系统的 known_hosts、ProxyJump、
  连接复用（ControlMaster）等成熟能力，不用自己实现一遍主机密钥校验。
  代价是多一个运行时依赖，`doctor` 的 hub 本地检查加一条检查。
- 固定参数：`-o Compression=no`（restic 会压，SSH 再压是浪费 CPU）、
  `-o BatchMode=yes`（禁止交互式密码提示，否则会挂死）、
  `-o StrictHostKeyChecking=<accept-new|yes>`、`-o UserKnownHostsFile=<配置值>`。
- **`Stream` 的 `Wait` 返回值就是 ADR-011 的第一道防线**，
  接口设计上要让调用方无法忽略它——不要提供一个「只拿 reader 不拿 wait」的重载。
- **不要额外包 `bash -c`。** `ssh host cmd args...` 的远程端本来就由登录 shell 解析，
  再套一层只增加转义面。当前 5 种 target 的远程命令都是单条命令，没有远程管道，
  用不上 `pipefail`（ADR-011）。
- **每个插值进远程命令的值必须逐参数转义**（路径、卷名、服务名、数据库名都来自
  用户手写的 YAML）。本地 `exec.Command` 靠 execve 天然免疫，远程没有这个保护，
  必须显式转义。红线见 `.trellis/spec/backend/external-command-guidelines.md`。
- 错误信息里不能出现 `identity_file` 的内容。

**验收**：对 `localhost` 的集成测试（`testing.Short()` 跳过）覆盖
正常执行、远程命令非零退出、远程进程被 kill 三种情况，
后两种必须让 `Wait` 返回非 nil 错误。

---

### P1-3 doctor 拆分为本地检查与远程检查 ✅ 已完成

**改动文件**：`internal/doctor/doctor.go`、新增 `doctor_remote.go`

```go
func RunLocal(ctx context.Context, cfg *config.Config) *Report
func RunHost(ctx context.Context, cfg *config.Config, host *config.Host) *Report
```

`RunLocal` 检查 hub 自己：

- `restic`、`ssh`、`systemd-analyze` 三个二进制存在且可执行
- `repo.password_file`、`repo.env_file` 权限为 0600
- 每个 host 的 `identity_file` 权限为 0600；`strict` 要求 `known_hosts_file` 已存在，
  默认 `accept-new` 允许文件尚不存在但父目录必须可写且可进入
- 对象存储可达、restic 仓库能解锁（`restic cat config`）
- 每个 host 的 `on_calendar` 表达式合法（沿用现有 `checkOnCalendar`）

`RunHost` 通过 SSH 检查目标机：

- 能否登录（这是所有后续检查的前提，失败则其余降级为 warn）
- `docker` 与 compose v2 是否可用
- compose 文件是否存在、`compose config --services` 能否列出服务
- 每个 target 引用的 service / volume / 路径是否真实存在

**已定的设计点**：

- **目标机不再检查 `restic`**——agentless 之后它不需要。
- 现有 `checkFile` 只能查本地。远程改用 `stat -c '%a'` 经 SSH 取权限位，
  两条路径的判定逻辑要共用，避免本地严格远程宽松。
- 保持现有的「无法判断降级为 warn，不伪造成 fail」原则。
  SSH 登录失败时后续检查是 warn 不是 fail——区分「确定有问题」和「无法判断」
  对运维决策很重要。
- 加一条 **NTP / 时钟偏移检查**：hub 与目标机时间差超过 60 秒时告警。
  时间不同步会让 `LASTSAVE` 轮询和快照时间戳都变得不可信。

**验收**：`ark doctor` 在缺少 restic 时报 fail；
`ark doctor --host X` 对一台真实机器能列出全部检查项；
故意把目标机的 compose service 改名后，对应 target 检查必须 fail。

---

### P1-4 CLI 与示例清单适配 ✅ 已完成

**改动文件**：`internal/cli/root.go`、`examples/ark.yaml`、`README.md`

- [x] `validate` 输出改为逐 host 摘要（当前打印的 `cfg.Host` / `cfg.Project.Name`
  在多机模型下已不存在）。
- [x] `doctor` 加 `--host <name>` 与 `--all` 标志。不带标志时只检查 hub。
  退出码语义保持不变：`0` 全通过 / `1` 工具出错 / `2` 检查未通过。
  该项已随 P1-3 的 `RunLocal` / `RunHost` 拆分一起完成。
- [x] `examples/ark.yaml` 重写为多机清单，含一台 `local: true` 的 hub 条目。
- [x] README 的命令用法段随 CLI 改动同步（`validate` 的新输出）。
  架构图、状态表和依赖说明已在架构重排那轮更新，这里不用再动。

**验收**：`make check` 全绿；`./bin/ark validate -c examples/ark.yaml`
在示例清单上通过（示例里的路径不必真实存在——validate 不碰文件系统）。

---

## P2 — 备份可用 ✅ 已完成

**阶段目标**：hub 上一条 `ark backup` 跑完，对象存储里出现所有机器的加密快照，
并且由 hub 上的 systemd timer 自动执行。

粗估 4–5 天。

### P2-1 状态库 `internal/store/` ✅ 已完成

**新增文件**：`internal/store/store.go`、`schema.sql`、`store_test.go`

SQLite（WAL 模式），文件固定在 `/var/lib/ark/ark.db`。

表：`runs`（一次 `ark backup` 的整体）、`run_targets`（每个 target 的结果）、
`doctor_reports`、`verifications`。

**已定的设计点**：

- 选 SQLite 而不是 MySQL/Postgres：单文件便于整体备份（ADR-012），
  零运维。`ark`（oneshot）写，`ark-hub`（常驻）读，WAL 模式下并发安全。
- 用 `modernc.org/sqlite`（纯 Go）而不是 `mattn/go-sqlite3`（cgo），
  保住「单个静态二进制」这条目标。
- schema 用简单的版本号 + 顺序迁移，不引入迁移框架。

**验收**：并发一写一读的测试不报 `database is locked`；
删掉 db 文件后首次运行能自动建表。

---

### P2-2 restic 封装 `internal/restic/` ✅ 已完成

**新增文件**：`internal/restic/repo.go`、`repo_test.go`

```go
type Repo struct{ /* url, passwordFile, env */ }

func New(cfg *config.Repo) (*Repo, error)
func (r *Repo) EnsureInit(ctx context.Context) error
func (r *Repo) BackupStdin(ctx context.Context, stdin io.Reader, filename string, tags []string) (Snapshot, error)
func (r *Repo) Snapshots(ctx context.Context, tags []string) ([]Snapshot, error)
func (r *Repo) Forget(ctx context.Context, policy config.Retention, tags []string) error
func (r *Repo) ForgetSnapshot(ctx context.Context, id string) error   // ADR-011 撤销坏快照用
func (r *Repo) Dump(ctx context.Context, snapshotID, path string) (io.ReadCloser, error)
func (r *Repo) Check(ctx context.Context) error
```

**已定的设计点**：

- 一律用 `--json` 解析输出。restic 的人类可读输出格式会变，JSON 不会。
- 对象存储凭证从 `repo.env_file` 读入后**只注入到 restic 子进程的环境**，
  不要写进 `os.Setenv`——否则会泄漏给后续所有子进程（包括 `ssh`）。
- 密码用 `RESTIC_PASSWORD_FILE`，不要用 `RESTIC_PASSWORD`（会出现在 `ps` 里）。
- 包装错误时确保不回显 `env_file` 的内容。
- `BackupPaths` **不再需要**——agentless 之后所有目标都走 stdin（ADR-010）。

**验收**：以 `/tmp/ark-test-repo` 本地目录作为 repo 的集成测试，
能完成 init → backup stdin → snapshots 列出 → forget，且重复 `EnsureInit` 不报错。
测试用 `testing.Short()` 跳过，避免 CI 里必须装 restic。

---

### P2-3 target 执行器 `internal/backup/` ✅ 已完成

**新增文件**：`internal/backup/executor.go`（接口 + 分发）、
`postgres.go`、`redis.go`、`volume.go`、`files.go`、`image.go`，各配测试。

每个执行器接收一个 `sshexec.Runner`，产出一个字节流交给 restic。

| 类型 | 远程命令 | 注意事项 |
|---|---|---|
| `postgres` | `docker compose exec -T <svc> pg_dump -U <user> -d <db> --no-owner --no-acl --clean --if-exists` | `-T` 必须有，否则 TTY 控制字符会污染输出流 |
| `redis` | `exec -T <svc> redis-cli BGSAVE` → 轮询 `LASTSAVE` 变化 → `exec -T <svc> cat /data/dump.rdb` | BGSAVE 是异步的，必须等 `LASTSAVE` 时间戳变化才能取文件 |
| `volume` | `docker run --rm -v <vol>:/src:ro alpine tar -cpf - -C /src .` | `:ro` 不能省；`-p` 不能省 |
| `files` | `tar -cpf - <paths>` | **改为走 stdin**（ADR-010），`-p` 保留权限属主 |
| `image_digest` | `docker compose ps --format json` → `docker image inspect` 取 `RepoDigests` | 见下 |

**关键：不要在送进 restic 之前压缩。** restic 自己会压缩，并且靠内容分块做去重——
预先 gzip 会让每次 dump 的字节流完全不同，去重率直接归零，每天都在传一份全量。

**`--stdin-filename` 必须跨次运行保持稳定**，格式固定为 `<host>/<target-id>`，
例如 `web-01/postgres/sub2api.sql`。文件名变了 restic 会当成新文件，去重效果归零。

**`image_digest` 的硬性契约**：

- 必须从**正在运行的容器**的镜像 ID 反查 digest，不能直接用 compose 里写的 tag。
- `RepoDigests` 为空、或匹配到多个仓库时**必须失败**，不能猜。
  拿不到确定的 digest，就意味着这份备份恢复时无法保证跑的是同一个版本。

**验收**：每个执行器有单测（用假 `Runner` 注入）；
对一台真实机器能产出非空输出流。

---

### P2-4 流完整性保障（ADR-011） ✅ 已完成

**改动文件**：`internal/backup/executor.go`、`internal/store/`

这是 agentless 架构最危险的地方，单列出来是因为它有独立的、可验证的验收标准。

两道防线：

1. `restic` 完成后**必须检查 `Stream` 的 `Wait` 返回值**。非零则该 target 判失败，
   并调 `ForgetSnapshot` 撤销刚产生的快照——宁可没有，不要有一个假的。
2. 记录字节数写入 `run_targets`，与该 target 上一次成功的字节数比对，
   跌幅超过阈值（默认 50%）时标记 warn 并进告警。
   截断未必伴随非零退出码，体积突变是第二道防线。

**验收**（这条必须真的做，不能只写代码）：

- [x] 备份进行中 `kill -9` 掉远程 `pg_dump`，本次 target 必须报失败
- [x] 上述情况下，仓库里**不能残留**那个被截断的快照
- [x] 人为造一个体积腰斩的 dump，必须产生 warn

---

### P2-5 快照清单 `internal/backup/manifest.go` ✅ 已完成

一次备份 = 多个 restic 快照（每个 target 一个）+ 一份把它们串起来的 manifest。

**manifest 内容**：schema 版本、run-id、hub 上的 ark 版本、开始/结束时间、
每个 host 的每个 target 的 `{id, type, snapshot_id, bytes, duration, error}`、
镜像 digest 映射。

manifest 本身也作为一个 restic 快照存入，打 tag `ark-manifest` + `run:<run-id>`。
恢复时第一步就是取最新的 manifest。

**验收**：manifest 能序列化/反序列化往返；能从 repo 里按 tag 取回最新一份。

---

### P2-6 `ark backup` 命令与 systemd timer ✅ 已完成

**新增文件**：`internal/cli/backup.go`、`internal/systemd/unit.go`（模板）、
`internal/cli/install.go`

流程：

```
加载清单 → doctor（默认 local，失败即中止）→ EnsureInit
  → 逐 host 串行：doctor --host → 逐 target 执行
  → 写 manifest → 统一 forget --prune → 写状态库 → 推心跳
```

**已定的设计点**：

- **单实例锁**：`flock` 于 `/run/ark.lock`。手动触发和 timer 撞车会让两个
  `pg_dump` 同时跑，白白加倍数据库压力。拿不到锁就直接退出，不排队。
- **host 串行执行**（ADR-009）：`prune` 需要仓库排他锁，串行也顺带避免
  多台机器同时 dump 打爆 hub 带宽。
- **前置 `doctor` 本地检查失败即中止**，不产生半成品快照。提供 `--skip-doctor` 用于应急。
- **单个 host 的 `doctor --host` 失败则跳过该 host，不中止其余**；
  单个 target 失败不中止同 host 的其余 target。半份备份也比没有强；
  但整体退出码非零，失败信息进 manifest 和状态库。
- `--host <name>` 只跑一台，`--dry-run` 打印将要执行的命令。
- `ark install` 在 hub 上生成 `ark-backup.service`（`Type=oneshot`）和
  `ark-backup.timer`：
  - `OnCalendar` 取自清单的 `defaults.schedule`
  - **`Persistent=true`**：机器关机错过了执行窗口，开机后补跑一次。
    没有这行，关机一晚就等于跳过一天备份。
  - `RandomizedDelaySec=600`
- 跑完向外部监控推一次心跳（死人开关，见 design.md §9）。

**验收**：在 hub 上执行一次，对象存储里能看到加密对象，
`restic snapshots --tag host:web-01` 能列出该机器全部 target 和 manifest；
`ark install` 后 `systemd-analyze verify` 无告警，
手动 `systemctl start ark-backup.service` 能跑通。

---

### P2-7 hub 自备份与对象锁 ✅ 已完成

**改动文件**：`internal/store`、`internal/cli`、`internal/doctor`、
`examples/ark.yaml`、`docs/operations.md`

- 清单里的 `local: true` 条目实际配起来：备份 `/etc/ark/ark.yaml`、
  `/var/lib/ark/ark.db`，以及 hub 上其它服务的数据（dnsmgr 的 MySQL 等）。
- **密钥不进备份**（ADR-012）：SSH 私钥和 restic 密码走离线介质。
  清单里要有注释明确说明这一点，防止后来者「顺手」把它们加进 paths。
- 在对象存储侧开启对象锁 / 仅追加保留，保留期与月备保留时长对齐。
  这一步是控制台操作，但**必须写进文档并在 `doctor` 的 hub 本地检查里加一条提示性检查**
  （能否检测到桶的保留策略取决于后端 API，取不到就输出 warn 提醒人工确认）。
- SQLite 状态库必须通过 online backup 导出一致的单文件副本；禁止直接复制运行中的
  `ark.db`，也禁止把旧 `-wal` / `-shm` 当作恢复材料。

**验收**：用 hub 上那把有写权限的凭证尝试删除一个保留期内的对象，必须失败。

---

### P2-8 SSH 主机密钥易用性 ✅ 已完成

**改动文件**：`internal/config`、`internal/sshexec`、`internal/hostkey`、
`internal/cli`、`internal/doctor`、`examples/ark.yaml`、`docs/operations.md`

- SSH 主机密钥策略支持默认 `accept-new` 和显式 `strict`，不提供完全关闭校验的模式。
- `sshexec` 映射为 OpenSSH 的 `StrictHostKeyChecking=accept-new|yes`；首次连接可以
  建立信任，但已记录主机的密钥变化始终拒绝，并给出刷新命令提示。
- 新增 `ark host-key refresh --host <name>`：默认只预览已记录与扫描到的 SHA256 指纹，
  带外核对后显式加 `--apply` 才原子更新该主机记录。
- 刷新只调用 `ssh-keyscan` / `ssh-keygen`，不读取身份私钥、不尝试账号认证；扫描结果
  不是身份验证，不能替代云控制台或服务器本地核对。
- `doctor` 根据策略检查信任库：`strict` 缺文件为 fail；默认策略缺文件时，只要父目录
  可写且可进入就给 warn，让首次连接完成记录。

**验收**：默认策略首次连接能建立信任；密钥变化仍被拒绝并提示刷新；预览零写入；
`--apply` 只替换目标主机记录、保留其它记录并保持 `0600`；非法策略和本机 host 均被拒绝。

---

## P3 — 恢复可用（成败点）

**阶段目标**：在一台**全新的、什么都没有的** VPS 上，只用
「hub + 对象存储凭证 + restic 密码」把服务完整拉起来。

粗估 4–6 天。**这一步不通过，P2 的所有工作都没有意义。**

### P3-1 恢复计划 `internal/restore/plan.go`

从 manifest 生成一个纯数据结构的 `Plan`：要恢复哪些 target、恢复到哪台机器、
要拉哪些镜像 digest、按什么顺序执行。

`Plan` 必须能被完整打印。`ark restore --dry-run` 就是「生成 Plan 并打印」，
全程只读，不创建容器、不写文件、不拉镜像。

**验收**：对一份真实 manifest 能输出完整可读的计划；`--dry-run` 跑完
目标机上 `docker ps -a` 和文件系统均无变化。

---

### P3-2 恢复执行 `internal/restore/execute.go`

严格按 `design.md` §7 的顺序执行，数据方向是 hub → SSH → 目标机：

```
files → 镜像 digest → volume → 起数据库 → 灌数据 → 起应用 → 健康检查
```

**已定的硬性要求**：

- **幂等**：每步先检查当前状态再决定做不做。中途失败后重跑同一条命令
  必须能继续，而不是留下一个半坏的环境。
- **拒绝隐式覆盖**：目标机上已存在同名 volume 或容器时必须报错退出，
  除非显式加 `--force`。恢复操作把生产数据覆盖掉是不可逆的。
- **破坏性恢复前先给现状打一份快照**。目标机上有正在运行的服务时，
  恢复前自动跑一次备份，失败则中止。误操作因此从不可逆变成可回滚。
- **恢复顺序不能并行化**。数据库没起来就灌数据只会失败得很难诊断。
- **ADR-011 同样适用于恢复方向**：`restic dump | ssh 'psql'` 中途断开会
  灌进半份数据，`Feed` 必须校验远程退出码。
- `--to <host>` 支持恢复到另一台机器，这是跨机重建的入口。
- 结束时输出**人工确认清单**：DNS 指向、TLS 证书、防火墙端口、
  以及 `.env` 里需要按新环境调整的项。
  暂时也包括「请先暂停 dnsmgr 对该主机的检测」，直到 P5 自动化。

**验收**：见 P3-3。

---

### P3-3 跨机重建实测（一次性任务，但最重要）

开一台干净的 VPS，加进清单，从 hub 完整走一遍恢复，把踩到的每个坑写回 `design.md`。

**必须验证的点**：

- [ ] 业务数据完整（抽查若干张表的行数和最新记录）
- [ ] 应用能正常登录、正常读写
- [ ] 加密字段能正常解密（验证 `.env` 里的密钥确实被正确恢复）
- [ ] 恢复出来的镜像 digest 和备份时一致
- [ ] 新机器上**只装了 docker 和 sshd**，没有 ark、没有 restic
- [ ] 全程只用了「hub + 对象存储凭证 + restic 密码」，
      没有依赖原机器上的任何东西

最后一条最容易翻车：过程中一旦发现「还需要从老机器上拷点什么」，
说明备份范围有缺口，回头补进清单。

---

### P3-4 hub 自身重建实测

> **本轮取消（2026-08-13）**：当前没有额外干净机器，现有宿主机也没有创建隔离 VM 的空间。
> 同宿主机目录或容器隔离无法证明恢复过程脱离旧 hub 的磁盘残留，且与 P3-3 的受限隔离验证
> 高度重复，因此经确认不执行、不计为通过，也不阻塞本轮后续任务。未来需要验证 ADR-012 时，
> 应在具备独立故障域的机器上重新立项。

以下保留原验收口径，未来重新立项时必须完整执行，不能用同宿主机隔离演练替代。

在一台干净机器上：装 ark 和 restic → 从离线介质取回 restic 密码和对象存储凭证
→ `restic restore` 取回 `/etc/ark/ark.yaml` 和 `/var/lib/ark/ark.db`
→ 从离线介质取回 SSH 私钥 → 验证 `ark doctor --all` 全绿。

**验收**：新 hub 能对所有原有机器执行一次成功的备份。

---

### P3-5 `ark verify` 自动演练

定期挑一个快照恢复到隔离环境，跑健康检查，通过后销毁。

- 复用隔离恢复派生独立 compose project、container、volume、network 与 files root
- 删除全部 published ports，只通过 Compose health、容器内数据库 CLI 与 digest 做内部检查
- **绝对不能碰生产项目的任何资源**——所有操作前校验
  `com.docker.compose.project` 标签
- 演练环境不绑定生产域名和生产 IP，避免被 dnsmgr 的健康检测扫到
- 结果写入状态库，在 hub 页面作为一等公民展示

**验收**：`ark verify` 跑完后生产环境无任何变化；
人为破坏一份快照后 verify 能报失败。

---

## P4 — hub 服务化

**阶段目标**：打开一个页面，一眼看出所有机器的备份健康度，并能从页面发起操作。

粗估 5–7 天。

### P4-1 `cmd/ark-hub/` 骨架与鉴权

- 常驻 HTTP 服务，读 `/var/lib/ark/ark.db`
- **必须有鉴权**。它能发起覆盖生产数据的恢复，内网部署也不例外。
  先做本地账号 + TOTP，与 dnsmgr 保持一致的体验。
- **不承载调度**（ADR-005），**不执行长任务**：需要执行时起一个 `ark` 子进程。

### P4-2 HTTP API

- `GET /api/hosts`、`GET /api/hosts/:host`、`GET /api/runs`、`GET /api/alerts`
- `POST /api/hosts/:host/backup`、`/verify`、`/restore`
- 恢复类接口要求二次确认参数，且把「将要覆盖什么」在响应里明确列出
- 健康判定：`最近成功备份时间 > 计划周期 × 2` 判为超时告警

### P4-3 前端 `web/`

Vue 3 + Vite + TypeScript + Pinia + Tailwind（与现有项目同栈，便于长期维护）。

- **总览**：每台机器一张卡片——最近备份时间、状态、大小趋势、下次计划时间
- **主机详情**：target 明细、历史快照、doctor 报告、演练结果
- **告警**：超时未备份、连续失败、演练失败
- **操作**：触发备份 / 演练 / 恢复，带确认对话框

### P4-4 内嵌打包

`go:embed` 把 `web/dist` 打进 `ark-hub` 单二进制，部署时不需要 node 环境。
Makefile 增加 `make hub`（先 pnpm build 再 go build）。

### P4-5 告警与死人开关

- 复用现有钉钉机器人。触发条件：超时未备份、连续两次失败、演练失败。
  **告警要有静默期**，否则一台机器坏掉会每轮刷一次群。
- `ark backup` 每次跑完推外部心跳，由外部监控发现「ark 整个没动静了」。
  这条不能由 `ark-hub` 自己做——它自己死了就不会告警。

**验收**：人为让一台机器备份失败，页面和钉钉都能在下一个周期内报出来；
`systemctl stop ark-hub` 之后，当晚的备份照常执行。

---

## P5 — dnsmgr 联动

**阶段目标**：恢复完成后 DNS 和证书自动就位，且恢复期间不会被健康检测误切。

粗估 1–2 天。依赖 P3 完成——要先有真实的恢复流程，才知道联动点确切在哪。

### P5-1 dnsmgr fork 的 API 补丁

`/dmonitor/task/:action` 目前挂在 `CheckLogin` 会话组，不在 `AuthApi` 组，
ark 用 API token 调不到。需要在 `SilentFlower/dnsmgr` 的 `route/app.php`
把 dmtask 的启停暴露到 API 组。

改动很小，但尽量做成能回馈上游的形态，避免每次追 upstream 都要处理冲突。

### P5-2 恢复后自动切 DNS

调 dnsmgr 的 `POST /api/record/update/:id`，把域名指到新机器 IP。
配置在清单的 host 条目下增加可选的 dnsmgr 记录关联。

### P5-3 维护窗口联动

ark 发起恢复前暂停对应 dmtask，完成后恢复。
**失败也要恢复**——用 defer 保证，不能因为恢复失败就把检测永久关着。

### P5-4 证书部署联动

重建后触发 dnsmgr 的证书部署（`ssh` / `local` 目标），把证书推到新机器。

**验收**：跨机重建一次，全程不需要手工碰 DNS 和证书；
恢复期间 dnsmgr 没有产生切换记录。

---

## P6 — 运维打磨

- [ ] 单机多 compose 项目支持（当前清单假设一机一项目）
- [ ] 第二备份后端（防止单一存储商账号被封）
- [ ] goreleaser 发布多架构二进制
- [ ] `ark` 自身的升级流程（manifest 要有 schema 版本兼容策略）
- [ ] SSH 连接复用（ControlMaster），减少多 target 场景下的握手开销
- [ ] 大 target 的断点续传评估（当前失败即整个 target 重来）

---

## 暂不做

- **WAL 归档 / PITR**：秒级 RPO 需要 pgBackRest 或 wal-g，复杂度高一个量级
  且只覆盖 PostgreSQL。等 P3 稳定运行一段时间后再评估。
- **自动故障切换**：ark 不参与流量调度，不做 fencing，不自动提升节点。
  DNS 层的故障转移由 dnsmgr 负责。
- **非 compose 部署形态**（k8s、裸机 systemd 服务）：范围外。
- **多租户 / 细粒度权限**：hub 假定由一个团队独占。

---

## 待决问题汇总

动手到对应阶段时需要先拍板：

| 问题 | 阶段 | 现在的倾向 |
|---|---|---|
| SSH 用 `x/crypto/ssh` 还是 exec `ssh` 命令 | P1-2 | exec `ssh`，复用系统的主机密钥校验与连接复用 |
| 对象锁保留期与 restic 保留策略如何对齐 | P2-7 | 保留期 = 月备保留时长，prune 交给生命周期规则 |
| `ark verify` 的频率与执行位置（原机 / 专用隔离机） | P3-5 | 每周一次；先在原机跑通，有条件再上专用机 |
| `ark-hub` 的鉴权形式（本地账号 / OIDC / 反代托管） | P4-1 | 本地账号 + TOTP，与 dnsmgr 体验一致 |
| dnsmgr fork 的 API 补丁能否回馈上游 | P5-1 | 尽量做成通用形态提 PR，避免长期维护分叉 |
| manifest schema 的向后兼容策略 | P6 | 新版必须能读旧 manifest，写入始终用最新版 |
