# Brief — P2-1 状态库

## Goal

- 新增基于 SQLite WAL 的 `internal/store`，持久化备份运行、target 结果、doctor 报告和恢复演练结果，为 P2 后续流程及未来 `ark-hub` 查询提供稳定边界。

## Scope

- 新增 `internal/store/store.go`、`schema.sql`、`store_test.go`，提供 `DefaultPath`、打开/关闭、schema 迁移和最小状态读写 API。
- 固定使用兼容 Go 1.22 且无需 cgo 的 `modernc.org/sqlite v1.36.1`；创建 `0700` 父目录和 `0600` 数据库文件。
- 启用并验证 WAL；为每个物理连接启用 foreign key 和 5 秒 busy timeout，同时保留调用方 context 的取消语义。
- 使用 `PRAGMA user_version` 和单连接 `BEGIN IMMEDIATE` 实现顺序迁移；schema v1 包含 `runs`、`run_targets`、`doctor_reports`、`verifications` 四张表及必要约束和索引。
- 实现 run 创建、查询、完成，target 最终结果写入与最近成功 bytes 查询，以及 doctor report、verification 追加。
- 覆盖首次建库、重复打开、权限、schema、CRUD、校验、外键级联、双 Store 并发读写、并发首次迁移和较新 schema 拒绝。
- 保留已补齐的 `docs/roadmap.md` P1-2 完成标记；实现收尾时更新 P2-1 标记和数据库规范。

## Non-Goals

- 不接入 `ark backup`、doctor CLI、systemd timer 或 `ark-hub`。
- 不实现 restic 封装、target 执行器、流完整性业务、历史清理、分页、聚合、告警或 HTTP API。
- 不升级 Go 版本，不引入 ORM 或第三方迁移框架。
- 不解决 WAL 数据库在线一致性备份；checkpoint、SQLite backup API 或 files target 特判继续归 P2-3。

## Key Decisions

- `runs` 表示一次整体多机备份运行，可选记录请求的单 host 范围；实际 host 归属放在 `run_targets`，避免延续旧单机模型。
- 状态固定为 `running`、`ok`、`warn`、`fail`；时间使用 UTC Unix 毫秒，duration 使用毫秒，bytes 不允许负数。
- 迁移以完整 `schema.sql` 为单位执行，不按分号拆 SQL；拿到写锁后重新读取版本，拒绝高于当前程序支持版本的数据库。
- 不限制连接池为单连接，以保留 WAL 下 ark 写、未来 ark-hub 读的并发能力；锁竞争失败必须保持错误链和可见性。
- `LastSuccessfulTargetBytes` 在无历史时返回 `found=false`，不把正常空结果当作故障。
- `Store` 不暴露原始 `*sql.DB`；调用方负责传入已脱敏错误文本，store 只负责校验和持久化。

## Key Context

- 默认数据库路径为 `/var/lib/ark/ark.db`，项目当前工具链为 Go 1.22。
- `docs/design.md` §9 定义 SQLite WAL、ark 写与未来 ark-hub 读；`docs/roadmap.md` P2-1 定义固定路径、四表、迁移和并发验收。
- `internal/store` 不依赖 `config`、`doctor`、`restic` 或 `backup`，避免底层持久化包反向依赖业务层。
- 所有导出类型和方法必须有中文 Javadoc/Docstring 风格注释；SQL 错误使用 `%w` 保留错误链，并检查 `RowsAffected`、`rows.Err()`、`Close()` 与 rollback 错误。
- 实现上下文以 `implement.jsonl` 为准，检查上下文以 `check.jsonl` 为准；数据库规范在实现完成后的 Update-Spec 阶段同步。

## Risks / Deferred

- WAL 文件属于数据库持久状态，只复制 `ark.db` 可能丢失已提交事务；本任务只记录风险，P2-3 必须选择一致性备份方案。
- schema v1 会成为 P2/P3/P4 的持久化契约，实施时需重点反查多机 run 语义、target 唯一键和最近成功 bytes 查询。
- 连接级 PRAGMA、迁移锁或失败清理若处理错误，可能造成偶发锁失败或半套 schema；必须通过真实磁盘文件上的独立 Store 并发测试验证。

## Acceptance

- Go 1.22 与 `CGO_ENABLED=0` 下可构建并运行 `internal/store` 测试，且仅引入 `modernc.org/sqlite v1.36.1`。
- 首次打开自动创建正确权限的目录、数据库和完整 schema v1；重复打开幂等，较新版本明确失败。
- WAL、foreign key、busy timeout 在实际连接上生效，四张表、外键、CHECK 和索引与设计一致。
- run、run target、doctor report、verification 的最小 API 可用；最近成功 bytes 查询在无历史时返回 `found=false`。
- 两个独立 Store 并发一写一读不出现 `database is locked`，并发首次 `Open` 不产生迁移冲突或半套 schema。
- `go test ./internal/store -race -count=1`、`CGO_ENABLED=0 go test ./internal/store -count=1`、`make check`、`make build`、`CGO_ENABLED=0 go build ./cmd/ark` 和 `git diff --check` 全部通过。

## Next Step

- 用户确认本 Brief 后运行 `task.py start`，再按 Trellis 路由进入 `internal/store` 实现。
