# SQLite Research

## 本地约束

- 项目 `go.mod` 使用 Go 1.22。
- `internal/store/` 尚不存在，当前没有 SQLite、ORM 或迁移依赖。
- `docs/design.md` §9 与 `docs/roadmap.md` P2-1 已确定 SQLite WAL、固定路径、四张表和
  `modernc.org/sqlite`。

## 驱动兼容性

2026-08-11 通过 Go module proxy 与模块元数据核对：

| 版本 | Go 要求 | 结论 |
|---|---|---|
| `v1.56.0` | Go 1.25 | 不兼容当前项目 |
| `v1.36.2` | Go 1.23 | 不兼容当前项目 |
| `v1.36.1` | Go 1.21 | 当前可用，未 retract |

选择 `v1.36.1`，不在 P2-1 同时升级 Go 工具链。

## WAL 与连接

- SQLite 官方文档说明 WAL 允许 reader 与 writer 并发，但少数连接清理、恢复或排他锁
  场景仍可能返回 `SQLITE_BUSY`，因此必须配置 busy timeout 并保留错误可见性。
- `modernc.org/sqlite v1.36.1` 支持 DSN 重复 `_pragma`，可让 `foreign_keys` 与
  `busy_timeout` 在 database/sql 打开的每条物理连接上生效。
- `journal_mode=WAL` 是数据库持久属性；Open 后仍应读取返回值确认真正进入 WAL。
- WAL 文件属于数据库持久状态。只复制主文件可能丢失已提交事务，这一风险继续留给
  P2-3 的 hub files target 一致性备份方案。

## 迁移结论

- 使用 SQLite 原生 `PRAGMA user_version`，不引入迁移框架。
- 两个进程可能同时首次启动；迁移必须先 `BEGIN IMMEDIATE`，再在锁内重新读取版本。
- 初始 `schema.sql` 作为一个完整迁移执行，不自行按分号切割 SQL。

## Sources

- https://pkg.go.dev/modernc.org/sqlite
- https://proxy.golang.org/modernc.org/sqlite/@v/v1.36.1.mod
- https://www.sqlite.org/wal.html
- https://www.sqlite.org/c3ref/busy_timeout.html
- https://pkg.go.dev/database/sql
