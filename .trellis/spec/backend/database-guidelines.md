# Database Guidelines

> ark 自身 SQLite 状态库与业务数据库备份/恢复的边界。

---

## Overview

ark 有两条必须隔离的数据库路径：

1. **ark 自身状态库**：`internal/store` 独占管理 `/var/lib/ark/ark.db`，
   使用 `database/sql` 和纯 Go `modernc.org/sqlite` 驱动，保存运行、target、
   doctor 与恢复演练结果。
2. **被备份的业务数据库**：仍然只通过容器内 CLI 交互，不在 Go 代码里
   引入 PostgreSQL、Redis 等数据库驱动，也不由状态库包管理业务凭证。

不要混淆这两个边界。SQLite 状态库的存在不改变业务数据库的备份原则；
业务数据库发生的错误往往只会在真正恢复时暴露，因此下面两组契约都必须严格执行。

---

## Scenario: ark 本地状态库

### 1. Scope / Trigger

- 修改 `internal/store`、`schema.sql`、schema 版本或迁移逻辑时，必须遵守本节。
- P2/P3/P4 的调用方只能通过 `store.Store` 的公开 API 读写状态，不能持有
  `*sql.DB`、拼 SQL 或依赖 SQLite 实现细节。
- `internal/store` 只负责连接、迁移、字段校验与 SQL；不得反向依赖
  config、doctor、backup 等业务包。
- 生产默认路径是 `/var/lib/ark/ark.db`。在线一致性导出属于 `Store` 的公开边界，
  但不得向调用方暴露 `*sql.DB` 或 SQLite driver connection。

### 2. Signatures

当前 schema 版本是 `1`，包含 `runs`、`run_targets`、`doctor_reports`、
`verifications` 四张表。公开入口必须保持为：

```go
const DefaultPath = "/var/lib/ark/ark.db"

func Open(ctx context.Context, path string) (*Store, error)
func (s *Store) Close() error
func (s *Store) CreateRun(ctx context.Context, run Run) error
func (s *Store) GetRun(ctx context.Context, id string) (Run, error)
func (s *Store) FinishRun(ctx context.Context, id string, result RunResult) error
func (s *Store) RecordRunTarget(ctx context.Context, target RunTarget) error
func (s *Store) LastSuccessfulTargetBytes(
    ctx context.Context,
    host string,
    targetID string,
) (bytes int64, found bool, err error)
func (s *Store) RecordDoctorReport(ctx context.Context, report DoctorReport) error
func (s *Store) RecordVerification(ctx context.Context, verification Verification) error
func (s *Store) ExportSnapshot(ctx context.Context) (io.ReadCloser, error)
```

新增公开 API 时必须补齐 Javadoc 风格注释、输入校验、错误链与针对性测试。

### 3. Contracts

- `Open` 创建父目录和数据库文件；目录权限是 `0700`，数据库文件权限是
  `0600`。DSN 必须用结构化 URL 构造，不能手工拼接用户提供的路径。
- 必须启用并验证 WAL。每条物理连接都必须启用 `foreign_keys=ON`，默认
  `busy_timeout=5000`；不要通过 `SetMaxOpenConns(1)` 隐藏并发问题，
  也不要擅自降低 SQLite 的同步级别。
- 当前驱动 `modernc.org/sqlite v1.36.1` 支持 Go 1.22 和无 CGO 构建。
  升级驱动时必须重新验证 race、无 CGO 构建、WAL 与锁等待行为。
- `modernc` 的 `sqlite3_busy_timeout` 在等待锁时不会及时响应
  `sqlite3_interrupt`。每次操作必须临时使用 25ms 的短 busy timeout，
  由 Go 层重试至最多 5 秒，并在连接归还连接池前恢复为 5000ms；
  调用方 context 取消或超时必须优先终止重试。
- 迁移必须在同一条连接上执行 `BEGIN IMMEDIATE`，取得写锁后重新读取
  `PRAGMA user_version`，按版本顺序完成全部 SQL、更新版本并提交。
  任一步失败都要回滚；数据库版本高于程序支持版本时必须拒绝启动。
  不要用分号拆分迁移 SQL，交给 SQLite 按完整脚本执行。
- 时间统一以 UTC Unix 毫秒持久化，耗时统一为非负毫秒。状态值固定为
  `running`、`ok`、`warn`、`fail`；schema CHECK 与 Go 校验必须一致。
- `runs` 表示一次整体运行；host 维度放在 `run_targets`，唯一键是
  `(run_id, host, target_id)`，不能为每台 host 伪造一条 run。
- 调用方写入 `error`、`report_json`、`detail_json` 前必须完成脱敏；
  状态库负责格式与约束校验，不负责猜测或清洗业务秘密。
- WAL 模式下 `ark.db-wal` 和 `ark.db-shm` 是持久化协议的一部分。
  禁止在运行中只复制 `ark.db` 主文件；必须使用 `ExportSnapshot` 的 SQLite
  Online Backup 语义。
- `ExportSnapshot` 在同一物理连接上先建立 WAL 读快照，再通过 modernc v1.36.1
  的 `NewBackup` / `Step` / `Finish` 分批复制。固定读快照防止持续 writer 让 backup
  反复从头开始；WAL reader 不能长期阻塞 ark writer 或未来 ark-hub reader。
- 导出临时目录固定 `0700`、文件固定 `0600`。副本必须归一化为 DELETE journal
  单文件，并在返回 reader 前通过 `integrity_check` 与 `foreign_key_check`。
  reader 的 `Close` 负责删除整个临时目录，关闭或清理错误必须返回给调用方。

### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| 路径为空或父目录/文件不可创建 | `Open` 返回带上下文的错误，不降级到其他路径 |
| 数据库 schema 版本高于程序版本 | 拒绝启动，并同时报告两个版本号 |
| 多进程并发首次打开 | 迁移串行化，等待者在锁内重读版本，不重复建表 |
| 迁移中任一 SQL 失败 | 回滚全部本次变更，`user_version` 不前移 |
| 状态、必填字段、时间、耗时或 JSON 非法 | API 在写入前返回字段级错误；DB CHECK 继续兜底 |
| target 没有成功历史 | 返回 `bytes=0, found=false, err=nil` |
| 写锁竞争未超过 5 秒 | 按短间隔重试，不立即把瞬时锁竞争当成失败 |
| 写锁竞争超过 5 秒 | 返回保留 SQLite 错误链的失败 |
| context 更早取消或超时 | 立即返回保留 context 与 SQLite 错误链的失败 |
| `Close` 或连接恢复失败 | 返回错误，不静默吞掉资源清理失败 |
| 导出期间源库持续写入 | 固定 WAL 读快照并完成一致副本，writer 仍可提交 |
| Online Backup 返回 busy/locked | 25ms 间隔重试，单次无进展最多 5 秒，context 优先 |
| 导出副本 integrity/foreign key 失败 | 不返回 reader，删除临时目录并返回阶段错误 |
| 调用方关闭导出 reader | 关闭文件并删除临时目录；任一清理错误可见 |

### 5. Good / Base / Bad Cases

- **Good**：两个 `Store` 实例打开同一文件，WAL 下读写并行；短暂写锁释放后
  操作成功，连接归还后 `busy_timeout` 仍是 5000ms。
- **Good**：writer 持续更新 WAL 时导出副本，副本可独立只读打开、包含导出前已提交数据，
  且不依赖 `-wal` / `-shm`。
- **Base**：没有历史 target 记录时，调用方通过 `found=false` 展示“暂无基线”，
  而不是把它当作数据库错误。
- **Bad**：向业务层暴露 `*sql.DB`、引入 ORM、把连接池限制为单连接、
  运行中只复制 `ark.db`、把 WAL/SHM 拼成恢复材料，或因为已有 SQLite 就用 Go 驱动
  直连被备份的 PostgreSQL。

### 6. Tests Required

状态库改动至少运行：

```bash
go test ./internal/store -race -count=10
make check
make build
CGO_ENABLED=0 go test ./internal/store -count=1
CGO_ENABLED=0 go build ./cmd/ark
go mod verify
```

测试必须覆盖目录/文件权限、四张表与索引、每条物理连接的 PRAGMA、重复打开、
高版本拒绝、迁移回滚、并发首次迁移、CRUD 与历史查询、外键及级联、非法输入、
WAL 写期间读取、锁等待中的 context 取消、操作后连接配置恢复，以及 Online Backup 的
并发 writer、一致性/外键校验、0700/0600 权限、取消/失败清理、独立单文件恢复。

### 7. Wrong vs Correct

```go
// 错误：业务层直接管理 SQLite，并用单连接掩盖并发与 PRAGMA 问题。
db, _ := sql.Open("sqlite", path)
db.SetMaxOpenConns(1)

// 正确：调用稳定的状态库边界，并显式处理初始化失败。
state, err := store.Open(ctx, store.DefaultPath)
if err != nil {
    return err
}
// 使用完成后显式处理关闭错误；需要 defer 时也要把关闭错误并入返回值。
if err := state.Close(); err != nil {
    return err
}
```

```go
// 错误：运行中的 WAL 数据库只复制主文件，可能漏掉已提交页。
reader, err := os.Open(store.DefaultPath)

// 正确：让 Store 固定 WAL 读快照并返回受控的单文件导出流。
reader, err := state.ExportSnapshot(ctx)
if err != nil {
    return err
}
defer reader.Close()
```

上面的正确示例只适用于 ark 自身状态。备份 PostgreSQL、Redis 等业务数据库时，
仍然按下一节调用容器内 CLI，不能替换为 Go 数据库驱动。

---

## Business Database Access Pattern

以下规则只适用于被备份的业务数据库。ark 与它们交互的唯一方式是
**通过 `docker compose exec -T` 调用容器内自带的 CLI 工具**：

| 数据库 | 备份 | 恢复 |
|---|---|---|
| PostgreSQL | `docker compose exec -T <service> pg_dump` | `docker compose exec -T <service> psql` |
| Redis | `docker compose exec -T <service> redis-cli BGSAVE` 后取 RDB | 放回 RDB 文件后再起容器 |

不用 Go 数据库驱动直连，理由有三条：

1. **版本天然匹配**。容器内的 `pg_dump` 与该容器的 PostgreSQL 服务端同版本。
   宿主机上装的 `pg_dump` 可能比服务端旧，会直接拒绝导出。
2. **不需要暴露端口**。数据库通常只监听 compose 内部网络，
   走 `exec` 不必为了备份把端口开到宿主机。
3. **不需要在 ark 里管理数据库凭证**。容器内通常已有 `.pgpass`、
   `POSTGRES_USER` 或 trust 认证，ark 不必再存一份密码。

`-T` 参数必须带上。不带 `-T` 时 compose 会分配 TTY，
输出里会混入控制字符并做 CRLF 转换，**二进制转储会被静默损坏**——
备份能跑完、能上传，只有恢复时才发现导入失败。

---

## Backup Rules

### 逻辑备份，绝不打包 PGDATA（ADR-003）

`postgres` 类型固定走 `pg_dump`。**任何情况下都不要改成直接打包 PGDATA 卷。**

运行中的 PostgreSQL 数据目录做热拷贝，得到的是撕裂的、不一致的快照，
恢复时可能根本起不来，而且在真正恢复之前你无从知道它是坏的。
Redis 同理：必须先 `BGSAVE` 再取 RDB，不能直接拷贝正在写入的 dump.rdb。

只有确定与数据库无关的卷（用户上传文件、静态资源）才允许用 `volume` 类型直接打包。
`internal/config/config.go:49` 起的 `TargetPostgres` 注释记录了这条约束的理由，
改动该类型的实现前先读它。

### 转储走 stdin 流式进 restic，不落临时文件

`pg_dump` 的 stdout 直接接到 `restic backup --stdin`，中间不写宿主机磁盘。
备份一个几十 GB 的库时，宿主机往往没有那么多余量放临时文件；
更重要的是，临时文件是明文的数据库全量转储，多存在一秒就多一秒风险。

### --stdin-filename 必须跨次运行保持稳定

这是最容易被无意破坏的一条。restic 按文件名做内容寻址去重，
文件名一变就当成全新文件，**去重效果直接归零**，仓库体积会随备份次数线性膨胀。

```
✅ postgres/sub2api.sql           固定值
❌ postgres/sub2api-20260811.sql  含日期，每天都是新文件
```

快照的时间信息由 restic 自己的 snapshot 元数据承载，不需要编进文件名。
`Target.ID()`（`internal/config/config.go:250`）返回的就是这种稳定标识，
新增 target 类型时沿用它，不要另造带时间戳的命名。

### 不要预压缩

restic 自带压缩且基于内容分块去重。在送进 restic 之前先 `gzip`，
会让每次转储的字节流完全不同，去重和压缩双双失效。
`pg_dump` 也不要加 `-Fc`（自定义格式自带压缩），用默认的纯文本 SQL。

---

## Restore Rules

恢复顺序是有严格依赖的，写恢复逻辑时按此执行（详见 `docs/design.md` 第 7 节）：

1. 先按 digest 拉取镜像（ADR-004），不要用 tag。
2. 只启动数据库容器，**等它真正 ready 之后再导入**——
   容器状态 running 不等于 PostgreSQL 已经接受连接，
   需要轮询 `pg_isready` 而不是 `sleep` 一个固定秒数。
3. 导入数据库转储。
4. 恢复数据卷与配置文件。
5. 最后才启动应用容器。

顺序颠倒的典型后果：应用先起来，发现表不存在，自动跑了一次初始化建表，
随后的转储导入撞上已存在的对象而失败——**而且部分表已经被应用写脏了**。

---

## Common Mistakes

- **忘记 `-T`**，二进制流被 TTY 转换损坏，直到恢复时才发现。
- **在 `--stdin-filename` 里放日期或快照 ID**，去重失效，仓库体积失控。
- **把 `pg_dump` 输出先落成临时文件再上传**，磁盘可能不够，且明文转储落盘。
- **改用打包 PGDATA 卷「省事」**，得到一份看起来正常、实际无法恢复的备份。
- **用 `sleep 10` 代替 `pg_isready` 轮询**，在慢机器上间歇性失败。
- **恢复时按 tag 拉镜像**，半年后的 `:latest` 与备份的 schema 已不兼容。
- **在 ark 里存数据库密码**，没必要——容器内已有认证配置，
  多存一份就是多一个泄漏点。
