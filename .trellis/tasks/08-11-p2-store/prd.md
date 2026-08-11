# P2-1 状态库

## Goal

新增 `internal/store/`，用本地 SQLite 状态库持久化 ark 的备份运行、target 结果、
doctor 报告和恢复演练结果，为 P2 后续备份流程与 P4 `ark-hub` 查询提供稳定的数据边界。

状态库默认位于 `/var/lib/ark/ark.db`，必须支持 ark oneshot 写入与未来 ark-hub
常驻读取并发存在，且首次运行能够自动创建数据库和 schema。

## Background

`docs/design.md` §9 已取消早期 `_status/<host>.json` 对象存储上报，改为由 hub
直接写本地状态库。P2-4 需要查询 target 上一次成功的字节数判断异常跌幅，P2-6
需要记录整轮备份结果，P3/P4 还需要读取 doctor 和演练历史。

当前仓库没有 `internal/store/`，也没有 SQLite 依赖。`go.mod` 固定 Go 1.22；
`modernc.org/sqlite` 最新版本已要求更高 Go 版本，因此本任务固定使用仍兼容 Go 1.22
的 `v1.36.1`，不顺带升级项目工具链。

## Requirements

### R1 包边界与生命周期

- 新增 `internal/store/store.go`、`schema.sql`、`store_test.go`。
- 导出 `DefaultPath = "/var/lib/ark/ark.db"`、`Open(ctx, path)`、`Close()` 和状态记录 API。
- `Open` 接受显式路径以便测试；路径为空时返回中文错误，不 panic。
- 首次打开时创建缺失的父目录和数据库文件，目录权限为 `0700`、数据库权限为 `0600`。
- 初始化任何一步失败时关闭已打开的数据库并返回带上下文且保留错误链的错误。
- 不向调用方暴露原始 `*sql.DB`，SQL 与迁移逻辑归 `store` 包所有。

### R2 SQLite 连接契约

- 使用纯 Go 驱动 `modernc.org/sqlite v1.36.1`，保持 `CGO_ENABLED=0` 可构建。
- 数据库启用 WAL，并在打开后读取 `PRAGMA journal_mode` 确认实际值为 `wal`。
- 每个物理连接都启用 `foreign_keys=ON` 和 5 秒 `busy_timeout`；调用方 context
  更早取消时仍必须优先响应 context。
- 不把连接池限制为单连接，否则同进程内读写并发会被人为串行化。
- 本轮不降低 SQLite 默认同步级别；状态记录宁可慢一点，也不能为性能静默牺牲持久性。

### R3 Schema 与迁移

- 使用 `PRAGMA user_version` 作为 schema 版本，不引入迁移框架或额外元数据表。
- 初始 schema 版本为 1，包含 `runs`、`run_targets`、`doctor_reports`、
  `verifications` 四张表及必要索引。
- 迁移使用单连接 `BEGIN IMMEDIATE` 串行化；拿到写锁后重新读取版本，按版本顺序
  执行缺失迁移并在同一事务内更新 `user_version`。
- 多个进程同时首次打开同一数据库时只能有一方执行建表，其他打开者等待后复核版本。
- 数据库版本高于当前程序支持版本时必须失败，不能尝试降级或忽略。
- 不按分号手工拆 SQL；初始 `schema.sql` 作为一个迁移单元执行。

### R4 数据模型

- `runs` 表示一次整体 `ark backup`，可记录可选的 `--host` 范围；多机 run 的 host
  归属放在 `run_targets`，不能沿用旧单机设计把 `runs.host` 当成唯一主机。
- `run_targets` 以 `(run_id, host, target_id)` 唯一标识一个结果，记录 target 类型、
  状态、字节数、耗时、restic snapshot ID 和脱敏错误。
- `doctor_reports` 记录 local/host 范围、host、时间、总体状态、可选下次运行时间和
  完整 JSON 报告。
- `verifications` 记录演练 ID、host、关联 run/snapshot、起止时间、状态、脱敏错误和
  JSON 详情；本轮只提供持久化边界，不实现演练业务。
- 状态值固定为 `running`、`ok`、`warn`、`fail`；时间统一存 UTC Unix 毫秒，
  duration 存毫秒，bytes 不得为负数。
- `run_targets.run_id` 使用外键并在 run 删除时级联删除；常用时间、host/target 查询
  建立索引。

### R5 最小读写 API

- 支持创建 run、查询单个 run、完成 run。
- 支持记录最终 target 结果，并按 host + target ID 查询上一次成功结果的字节数，
  直接服务 P2-4 的体积跌幅判断。
- 支持追加 doctor 报告和 verification 结果。
- Go 层在写入前校验必填字段、状态、非负数和 JSON 合法性；数据库 CHECK/FOREIGN KEY
  作为第二道防线。
- 查询无历史成功 target 时返回 `found=false`，不把正常的“无历史”包装成故障。

### R6 并发与失败可见性

- 测试必须使用同一文件的两个独立 Store 实例模拟 ark 写、ark-hub 读。
- 写事务进行时读取不得返回 `database is locked`；真正超过 busy timeout 或 context
  的竞争错误必须原样进入错误链，不能静默重试成成功。
- 同一缺失数据库的并发 `Open` 必须全部成功，并最终只得到完整的 schema v1。
- `Close` 错误必须返回；测试清理路径可以显式忽略，但要写明原因。

### R7 文档同步

- 将 `docs/roadmap.md` 的 P1-2 标题补为“✅ 已完成”。
- P2-1 完成后更新 roadmap 状态，并修正 `.trellis/spec/backend/database-guidelines.md`
  中“ark 自己没有数据库、状态写对象存储”的过期假设。
- 记录 WAL 数据库不能只复制主文件的风险，但数据库一致性备份方案仍归 P2-3，
  本任务不实现 checkpoint、SQLite backup API 或 `files` target 特判。

### R8 测试与构建

- 覆盖首次建库、重复打开、schema/索引、WAL/foreign key/busy timeout、CRUD、校验失败、
  外键级联、无历史 target、并发读写、并发迁移和较新 schema 拒绝。
- `go test ./internal/store -race -count=1`、`make check`、`make build` 全绿。
- `CGO_ENABLED=0 go test ./internal/store -count=1` 和 `CGO_ENABLED=0 go build ./cmd/ark`
  通过，证明没有引入 cgo 依赖。

## Non-Goals

- 不接入 `ark backup`、doctor CLI、systemd timer 或 `ark-hub`。
- 不实现 P2-2 restic 封装、P2-3 target 执行器、P2-4 流完整性业务逻辑。
- 不实现数据库历史清理、分页、统计聚合、告警判断或 HTTP 查询 API。
- 不升级 Go 版本，不引入 ORM 或第三方迁移框架。
- 不解决 `/var/lib/ark/ark.db` 的在线一致性备份；该问题按既有记录留给 P2-3。

## Acceptance Criteria

- [ ] `internal/store` 使用 `modernc.org/sqlite v1.36.1`，Go 1.22 与无 cgo 构建保持可用
- [ ] 首次打开缺失路径自动创建 `0700` 目录、`0600` 数据库和完整 schema v1
- [ ] 重复打开不重复执行迁移，较新 `user_version` 返回可读错误
- [ ] WAL、foreign key 和 5 秒 busy timeout 在实际连接上生效
- [ ] 四张表、外键、CHECK 和索引与设计一致
- [ ] run、run target、doctor report、verification 的最小读写 API 可用
- [ ] 能查询指定 host/target 上一次成功结果的字节数，无历史时返回 `found=false`
- [ ] 两个独立 Store 并发一写一读不出现 `database is locked`
- [ ] 并发首次 Open 不产生迁移冲突或半套 schema
- [ ] `docs/roadmap.md` 的 P1-2 完成标记已补齐，P2-1 完成状态与数据库 spec 在收尾阶段同步
- [ ] `go test ./internal/store -race -count=1`、`make check`、`make build` 和无 cgo 构建全绿
