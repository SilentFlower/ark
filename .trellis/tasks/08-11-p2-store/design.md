# 技术设计：P2-1 状态库

## 1. 职责边界

`internal/store` 只负责本地状态数据库的连接、迁移、数据校验和 SQL 读写：

```text
P2-6 backup / P3 verify / P4 hub
                │
                ▼
        internal/store
                │
                ▼
      /var/lib/ark/ark.db
```

它不依赖 `config`、`doctor`、`restic` 或 `backup`，避免底层状态包反向认识业务包。
调用方把 target type、doctor report 等转换成 store 自己的稳定字段或 JSON。

## 2. 公共 API

```go
const DefaultPath = "/var/lib/ark/ark.db"

type Store struct { /* 非导出 *sql.DB */ }

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
func (s *Store) RecordVerification(ctx context.Context, result Verification) error
```

记录类型使用 `time.Time` / `time.Duration` / `json.RawMessage`，落库时转换为 Unix 毫秒、
毫秒 duration 和 TEXT JSON。`GetRun` 找不到记录时包装并保留 `sql.ErrNoRows`；
`LastSuccessfulTargetBytes` 的无历史是正常分支，使用 `found=false`。

## 3. 依赖版本

项目 `go.mod` 使用 Go 1.22。2026-08-11 查询结果显示：

- `modernc.org/sqlite v1.56.0` 要求 Go 1.25；
- `v1.36.2` 起要求 Go 1.23；
- `v1.36.1` 的 `go.mod` 为 Go 1.21，且未被 retract。

因此本任务固定 `modernc.org/sqlite v1.36.1`。依赖升级与 Go 工具链升级以后独立处理，
本任务不把数据库功能和工具链迁移绑在一起。

## 4. 打开流程与文件权限

`Open` 的顺序固定为：

1. 校验 path 非空；
2. `MkdirAll(parent, 0700)`；
3. 以 `0600` 预创建数据库文件，已有文件不截断；
4. 用 `net/url` 构造 file URI，避免手拼路径与 query；
5. DSN 为每条物理连接设置 `busy_timeout(5000)`、`foreign_keys(ON)`；
6. `PingContext` 建立真实连接；
7. 执行并验证 `PRAGMA journal_mode=WAL`；
8. 执行迁移；
9. 任一步失败都关闭 DB。

不设置 `SetMaxOpenConns(1)`。单连接虽然能减少部分锁竞争，却会让同进程读取被写事务
阻塞，和“ark 写、ark-hub 读”的目标相反。WAL 允许 reader 与 writer 并存，
`busy_timeout` 负责短暂锁竞争；调用方 context 负责更严格的截止时间。

## 5. Schema v1

### runs

| 字段 | 类型 | 约束 |
|---|---|---|
| `id` | TEXT | PRIMARY KEY |
| `requested_host` | TEXT | NULL 表示全量运行 |
| `status` | TEXT | running/ok/warn/fail |
| `started_at` | INTEGER | UTC Unix 毫秒 |
| `finished_at` | INTEGER | 可空 |
| `duration_ms` | INTEGER | 非负，可空 |
| `ark_version` | TEXT | 非空 |
| `error` | TEXT | 已脱敏，默认空串 |

### run_targets

| 字段 | 类型 | 约束 |
|---|---|---|
| `run_id` | TEXT | FK -> runs(id), ON DELETE CASCADE |
| `host` | TEXT | 非空 |
| `target_id` | TEXT | 非空，与 `config.Target.ID()` 对齐 |
| `target_type` | TEXT | 非空 |
| `status` | TEXT | ok/warn/fail |
| `bytes` | INTEGER | >= 0 |
| `duration_ms` | INTEGER | >= 0 |
| `snapshot_id` | TEXT | 默认空串 |
| `error` | TEXT | 已脱敏，默认空串 |

主键为 `(run_id, host, target_id)`。索引 `(host, target_id, status, run_id)` 支持
P2-4 查询最近一次成功字节数。

### doctor_reports

记录自增 ID、`scope`（local/host）、可空 host、创建时间、总体状态、可空
`next_run_at` 和完整 `report_json`。JSON 用于保留逐项检查；常用筛选字段单独成列。

### verifications

记录 verification ID、host、可空 run ID、snapshot ID、起止时间、duration、状态、
已脱敏错误和 `detail_json`。P3 可以通过顺序迁移增加演练环境字段，不在 v1 预埋未知列。

## 6. 迁移算法

`schema.sql` 通过 `go:embed` 作为版本 1 的完整迁移单元。Go 中维护按版本下标排列的
migration 列表，当前只有一项。

迁移必须绑定到一条 `sql.Conn`：

```text
BEGIN IMMEDIATE
  -> 重新读取 PRAGMA user_version
  -> 版本过新则失败
  -> 顺序执行 version+1 ... current
  -> 每步更新 PRAGMA user_version
COMMIT
```

`BEGIN IMMEDIATE` 的目的不是加速，而是让两个进程同时首次启动时先争夺唯一写锁；
等待者拿到锁后重新读取版本，不能沿用加锁前的旧值。发生错误时显式 ROLLBACK，
不删除已有数据库文件，也不尝试自动降级。

## 7. 数据校验与错误

- 必填 ID/host/target 字段为空：Go 层立即返回中文错误。
- 未知状态：Go 层拒绝，数据库 CHECK 再兜底。
- bytes/duration 为负：Go 层拒绝，数据库 CHECK 再兜底。
- doctor/verification JSON 无效：写入前用 `json.Valid` 拒绝。
- 完成不存在的 run：检查 `RowsAffected == 1`，否则返回可定位错误。
- SQL 错误统一补充动作上下文并用 `%w` 保留错误链，不比较错误字符串。
- error/detail 字段只接受调用方已经脱敏的文本；store 不读取任何凭证文件。

## 8. 并发测试

并发验收使用真实磁盘临时文件，不使用 `:memory:`：

1. 打开两个独立 Store，模拟两个进程的连接池；
2. writer 开启事务并写入，reader 同时查询已经提交的数据；
3. 重复多轮，断言没有 `database is locked` 且数据完整；
4. 对同一个不存在的 DB 并发执行多个 `Open`，断言全部成功、四表齐全、
   `user_version=1`。

另外验证外键级联、较新 schema 拒绝、重复打开幂等和 PRAGMA 实际值。

## 9. 兼容、回滚与延后

- 本任务新增本地状态文件，但没有 CLI 调用方，因此回滚代码即可；未接入前数据库可删除重建。
- schema 一旦被后续版本写入真实数据，不允许通过降低 `user_version` 回滚。
- WAL 文件是数据库持久状态的一部分，只复制 `ark.db` 可能丢提交。P2-3 必须决定使用
  SQLite backup API、checkpoint 后复制，或一致地处理主文件/WAL/SHM；P2-1 只记录风险。
- `database-guidelines.md` 当前“ark 自己没有数据库”的描述在 P2-1 完成后必须改写，
  同时保留“业务数据库仍只通过容器内 CLI 备份”的既有规则。

## 10. 文档同步

- 规划阶段立即补 `docs/roadmap.md` 的 P1-2 “✅ 已完成”标记。
- 实现完成后再标记 P2-1 完成，并通过 Update-Spec 更新数据库规范；不能提前宣称完成。
