# 技术设计：P4-2 ark-hub HTTP API

## 1. 架构边界

```text
cmd/ark-hub/main.go
  -> internal/hub
       -> config.LoadAndValidate(/etc/ark/ark.yaml)
       -> store.Store
            -> query DTO
            -> manual_operations schema v2
       -> health projection
            -> internal/schedule.Analyze(systemd-analyze calendar)
       -> operation manager
            -> exec /usr/local/bin/ark --config ... <command> --json
       -> authenticated net/http API

ark CLI
  -> backup / verify / restore
       -> doctor report persistence
       -> restore read-only preview + expected preview digest enforcement
```

- `internal/hub` 持有 HTTP DTO、鉴权后的请求校验、健康派生、确认凭据和子进程生命周期。
- `internal/store` 独占 SQLite schema、SQL、分页、NULL 还原和手动操作持久化，不依赖 config 或 hub。
- `internal/schedule` 只负责把 systemd `OnCalendar` 转为结构化的下次触发时间和有效周期，
  供 doctor 与 Hub 复用。
- `internal/restore` 持有恢复计划、目标冲突预检和预检摘要；Hub 不直接导入恢复业务包，
  只消费 `ark restore --json` 的稳定输出。
- `ark-hub` 不承载调度。停止 Hub 不影响任何现有 timer；手动长任务仍由独立 `ark` 进程执行。

## 2. Hub 启动与配置

`ServeOptions` 增加：

```go
type ServeOptions struct {
    ListenAddress string
    StateDBPath   string
    AuthFile      string
    ConfigPath    string
    ArkBinaryPath string
    SecureCookie  bool
}
```

CLI 契约：

```text
ark-hub serve
  --listen 127.0.0.1:8080
  --state-db /var/lib/ark/ark.db
  --auth-file /var/lib/ark-hub/auth.json
  --config /etc/ark/ark.yaml
  --ark-binary /usr/local/bin/ark
  [--secure-cookie]
```

- 凭证文件、清单、Ark 二进制和状态库都在创建 listener 前验证。
- Ark 二进制和清单必须是绝对路径；二进制必须是普通可执行文件。不得依赖当前目录或 PATH 猜测。
- 启动时加载一次清单确认可用；每次业务 API 请求重新严格加载，保证清单修改无需重启即可生效。
  运行期清单损坏时返回 `503`，不继续使用静默过期缓存。
- `ark-hub install` 与 `systemd.HubInstallOptions` 同步增加两个路径，生成的 `ExecStart` 显式携带。

## 3. 状态库 schema v2

保留 v1 SQL 不变，新增独立 `schema_v2.sql` 并把 `currentSchemaVersion` 提升为 2：

```sql
CREATE TABLE manual_operations (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (
        kind IN ('backup', 'verify', 'restore_preview', 'restore')
    ),
    host TEXT NOT NULL CHECK (length(trim(host)) > 0),
    status TEXT NOT NULL CHECK (
        status IN ('running', 'ok', 'fail', 'interrupted')
    ),
    started_at INTEGER NOT NULL CHECK (started_at >= 0),
    finished_at INTEGER,
    duration_ms INTEGER,
    request_json TEXT NOT NULL CHECK (json_valid(request_json)),
    result_json TEXT CHECK (result_json IS NULL OR json_valid(result_json)),
    error TEXT NOT NULL DEFAULT '',
    exit_code INTEGER,
    parent_operation_id TEXT,
    CHECK (
        (status = 'running' AND finished_at IS NULL AND duration_ms IS NULL) OR
        (status != 'running' AND finished_at IS NOT NULL AND duration_ms IS NOT NULL)
    ),
    FOREIGN KEY (parent_operation_id) REFERENCES manual_operations(id) ON DELETE SET NULL
);

CREATE INDEX idx_manual_operations_started
    ON manual_operations(started_at DESC, id DESC);
CREATE INDEX idx_manual_operations_host_started
    ON manual_operations(host, started_at DESC, id DESC);
CREATE INDEX idx_manual_operations_status_started
    ON manual_operations(status, started_at DESC, id DESC);
```

Go 类型使用独立枚举，避免把 `interrupted` 混入现有 `store.Status`：

```go
type OperationKind string
type OperationStatus string

type ManualOperation struct {
    ID                string
    Kind              OperationKind
    Host              string
    Status            OperationStatus
    StartedAt         time.Time
    FinishedAt        time.Time
    Duration          time.Duration
    RequestJSON       json.RawMessage
    ResultJSON        json.RawMessage
    Error             string
    ExitCode          *int
    ParentOperationID string
}
```

- `request_json` 和 `result_json` 只保存白名单 API/CLI DTO；确认 token、Cookie、环境变量和 stderr
  永不入库。
- `CreateManualOperation` 只创建 `running`；`FinishManualOperation` 只允许一次终态转换。
- `InterruptRunningOperations(ctx, now)` 在 Hub 启动监听前用一条 UPDATE 把遗留 `running`
  变成 `interrupted`，写入完成时间、耗时和固定脱敏错误。
- v1 -> v2、空库直接到 v2、并发首次迁移和失败回滚沿用现有 `BEGIN IMMEDIATE` 迁移内核。

## 4. Store 查询契约

新增公开查询 DTO 和方法，所有输入都有上限校验与中文 Javadoc：

```go
type RunListOptions struct {
    Host      string
    Status    Status
    BeforeAt  time.Time
    BeforeID  string
    Limit     int
}

type HostRun struct {
    Run     Run
    Status  Status
    Targets []RunTarget
}

type OperationListOptions struct {
    Host      string
    Kind      OperationKind
    Status    OperationStatus
    BeforeAt  time.Time
    BeforeID  string
    Limit     int
}

func (s *Store) ListRuns(ctx context.Context, options RunListOptions) ([]Run, bool, error)
func (s *Store) ListHostRuns(ctx context.Context, host string, limit int) ([]HostRun, error)
func (s *Store) LatestDoctorReport(ctx context.Context, scope DoctorScope, host string) (DoctorReport, bool, error)
func (s *Store) ListVerifications(ctx context.Context, host string, limit int) ([]Verification, error)
func (s *Store) CreateManualOperation(ctx context.Context, operation ManualOperation) error
func (s *Store) FinishManualOperation(ctx context.Context, id string, result ManualOperationResult) error
func (s *Store) InterruptRunningOperations(ctx context.Context, finishedAt time.Time) (int64, error)
func (s *Store) GetManualOperation(ctx context.Context, id string) (ManualOperation, error)
func (s *Store) ListManualOperations(ctx context.Context, options OperationListOptions) ([]ManualOperation, bool, error)
```

- 列表使用 `(started_at, id)` 倒序 keyset pagination，不使用随并发写入漂移的 OFFSET。
- `ListHostRuns` 只返回已完成 run，并按 target 状态聚合 host 状态：任一 fail 为 fail，
  否则任一 warn 为 warn，否则为 ok。`warn` 表示已产生可用备份，计入最近成功时间。
- Store 返回领域记录，不返回 HTTP cursor 或 API JSON；cursor 编解码属于 Hub。

## 5. Schedule 与 doctor 数据

新增 `internal/schedule`：

```go
type Window struct {
    NextRunAt time.Time
    Interval  time.Duration
}

func Analyze(ctx context.Context, expression string, baseTime time.Time) (Window, error)
```

实现固定调用：

```text
LC_ALL=C systemd-analyze calendar
  --iterations=2
  --base-time=<UTC timestamp>
  <expression>
```

- 解析两条 `(in UTC): ... UTC`；第一条是下次运行时间，两条之差是当前有效周期。
- 输出缺失、时间倒序、周期为零、context 取消或命令失败都返回错误，不猜测 cron 近似值。
- `doctor.checkOnCalendar` 改为复用 `schedule.Analyze`，继续展示表达式和 next run。
- backup 在 local doctor 前打开状态库，记录 local 报告；每台 host doctor 后记录 host 报告和
  结构化 `next_run_at`。报告写入失败时在创建对应快照前显式失败。
- verify 同样记录实际执行过的 local/host doctor。单独运行 `ark doctor` 仍保持不创建状态库，
  避免普通诊断命令因 `/var/lib/ark` 写权限产生新副作用。

## 6. API 路由与响应

现有 auth 路由保持不变，新增：

```text
GET  /api/hosts
GET  /api/hosts/{host}
GET  /api/runs
GET  /api/alerts
GET  /api/operations
GET  /api/operations/{id}
POST /api/hosts/{host}/backup
POST /api/hosts/{host}/verify
POST /api/hosts/{host}/restore
```

分页：

- `limit` 默认 50，最大 100。
- cursor 是 base64url 编码的版本化 JSON，包含 `started_at_ms` 和 `id`；非法、过期格式或与当前
  filter 不兼容时返回 `400`。
- 响应统一为 `{"items": [...], "next_cursor": null|string}`。

时间和空值：

- 时间使用 UTC RFC 3339Nano。
- duration 使用 `duration_ms`。
- 空 slice 强制为 `[]`；未知时间、结果和 doctor 使用 JSON `null`。

`GET /api/session` 增加 `csrf_token`，供 P4-3 对 POST 请求设置 `X-CSRF-Token`；Cookie 继续 HttpOnly。
P4-1 已有的 `401 {"authenticated":false}` 和运行期 auth 故障 `503` 形状保持兼容。

新业务 API 错误使用：

```json
{
  "error": {
    "code": "invalid_request",
    "message": "请求参数无效"
  }
}
```

错误码固定为 `invalid_request`、`not_found`、`conflict`、`confirmation_required`、
`confirmation_expired`、`operation_failed`、`service_unavailable`。响应不包含底层 stderr。

## 7. Host 健康与告警

Hub 每次请求加载当前清单，按唯一 schedule 表达式分组调用一次 `schedule.Analyze`，再与 Store 事实合并：

```text
config host + effective schedule
  + recent HostRun
  + latest doctor
  + latest verification
  -> HostHealth
  -> /api/hosts and /api/alerts
```

判定：

- 最近成功：最新 host status 为 `ok` 或 `warn` 的完成时间。
- `backup_overdue`：没有成功记录，或 `now - last_success > interval * 2`。
- `backup_consecutive_failures`：最近两个已完成 host run 都是 `fail`。
- `verification_failed`：最近一次 verification 为 `fail`。
- schedule 无法分析时 host 健康为 `unknown`，返回 `schedule_unavailable` 诊断事实，但不伪造
  `backup_overdue`；doctor 已记录的 next run 可作为“最后已知值”展示，不能参与当前超时判定。

告警 ID 使用稳定的 `<host>:<kind>`，为 P4-5 静默期复用；P4-2 不写入告警表。
`/api/hosts` 与 `/api/alerts` 必须调用同一个 projection，不能复制判定条件。

## 8. 异步操作管理器

```text
HTTP request context
  -> validate + persist running + cmd.Start
  -> 202 Accepted

application context
  -> cmd.Wait goroutine
  -> bounded JSON decode
  -> persist ok/fail
```

- 子进程使用 application 生命周期 context，客户端断开不会取消任务。
- 本地命令固定用 `exec.CommandContext` 和 argv，不经过 shell；`SysProcAttr.Pdeathsig=SIGTERM`
  让 Hub 异常退出时 Ark 收到终止信号并把取消传给自己的子进程。
- stdout 上限 4 MiB，stderr 上限 64 KiB。stdout 超限、JSON 损坏或缺必填字段都标记 fail；
  stderr 只用于内存中的即时诊断，不写数据库、不回给 HTTP 客户端。
- 同一 Hub 进程最多一个活动手动操作。第二个请求返回 `409 conflict`；外部 systemd oneshot
  仍可能抢先持有 `/run/ark.lock`，这种情况由 Ark 非零退出并持久化为 fail。
- POST 在 operation 成功持久化且子进程成功启动后返回 `202` 和 operation ID；启动失败时
  先把记录完成为 fail，再返回 `500`。
- graceful shutdown 先停止接收新请求，再取消活动 Ark、等待其退出并写 `interrupted`，最后关闭 Store。
  shutdown 超时、kill 和状态写入错误都进入 `errors.Join`。

命令映射：

```text
backup:
  ark --config <path> backup --host <host> --json

verify:
  ark --config <path> verify --host <host> --snapshot <value> --json

restore_preview:
  ark --config <path> restore --host <source> --to <destination>
      --snapshot <value> --dry-run --inspect [--force|--isolate] --json

restore:
  ark --config <path> restore --host <source> --to <destination>
      --snapshot <exact-manifest-id> [--force|--isolate]
      --expected-preview-sha256 <digest> --json
```

Web API 不暴露 `--skip-doctor`、`--keep-on-failure` 或 cleanup。

## 9. 恢复只读预检与确认

`internal/restore` 增加稳定预检 DTO：

```go
type Conflict struct {
    Resource     string `json:"resource"`
    Detail       string `json:"detail"`
    ForceAllowed bool   `json:"force_allowed"`
}

type Preview struct {
    Plan        Plan       `json:"plan"`
    Force       bool       `json:"force"`
    Resume      bool       `json:"resume"`
    Destructive bool       `json:"destructive"`
    Conflicts   []Conflict `json:"conflicts"`
    Digest      string     `json:"digest"`
}

type InspectOptions struct {
    Force          bool
    RawFileTargets map[string]string
}

func Inspect(ctx context.Context, plan Plan, runner sshexec.Runner, options InspectOptions) (Preview, error)
```

- `Inspect` 复用 Execute 的目标机读取逻辑，只读 marker、容器、volume、network 和目标路径；
  不创建目录、不写 marker、不拉镜像、不执行 safety backup。
- 冲突按 resource/detail 排序，Plan、force、resume 和冲突集合一起做 SHA-256；digest 使用 64 位小写十六进制。
- `ark restore --dry-run --inspect --json` 输出 `Preview`。只有带 `--inspect` 时允许 dry-run 与
  `--force` 组合；`--force` 仅影响预览授权，不产生写入。
- `ExecuteOptions` 增加 `ExpectedPreviewSHA256`。真实执行第一次目标预检后先比较 digest；
  force safety backup 完成后、首次目标写入前再次预检并比较，任一次变化都 fail closed。
- Hub 的首次 restore POST body：

```json
{
  "action": "preview",
  "destination_host": "web-02",
  "snapshot": "latest",
  "mode": "force"
}
```

`mode` 固定为 `normal`、`force`、`isolate`。路径中的 `{host}` 是 source host，destination 为空时同 source。

- 预检作为 `restore_preview` operation 异步运行。成功后 operation result 保存 Preview，但不保存确认 token。
- 内存确认项保存随机 token 的 SHA-256、请求 session 的 token hash、preview operation ID、preview digest、
  精确 manifest snapshot ID、规范化请求和 10 分钟到期时间。
- operation GET 只有同一有效 session 第一次读取成功 Preview 时才生成并返回确认 token；服务端只保存
  token hash，后续不会再次展示原 token。响应丢失或 Hub 重启后必须重新预览。
- 确认请求只接受：

```json
{
  "action": "execute",
  "preview_operation_id": "...",
  "confirmation_token": "..."
}
```

不允许再次提交 host/snapshot/mode，避免确认时偷偷换参数。token 校验与创建 restore operation 在同一 mutex
临界区内完成并单次消费；真实命令使用 Preview 中的精确 manifest snapshot ID，不再使用 `latest`。

## 10. 安全与脱敏

- 所有新路由先 `requireSession`；POST 再用 constant-time 比较 `X-CSRF-Token` 和 session CSRF。
- JSON body 上限 16 KiB，`DisallowUnknownFields`，并拒绝尾随第二个 JSON 值。
- API DTO 不直接嵌入 `config.Host`、`config.Repo` 或 `config.SSH`。允许展示 host、local、项目逻辑名、
  target ID/type 和 schedule 表达式；不展示 compose/env/identity/known_hosts/password 路径。
- operation request/result 采用白名单结构；不持久化或响应原始 argv、环境、stderr、Cookie、token。
- `restore.Preview` 中目标路径属于管理员确认所需的覆盖范围，可以在已鉴权、no-store 的 API 返回；
  不进入普通日志或未认证错误。
- 本任务不引入日志框架。

## 11. 兼容、文档与回滚

- 现有 `ark backup/verify/restore` 的默认 CLI 行为和 JSON 结果保持兼容；新增 restore flags 均为可选。
- 现有 `/api/session` 字段只追加 `csrf_token`，不删除 `authenticated` 或 `username`。
- schema v2 是向前迁移；旧二进制看到高版本数据库会按现有契约拒绝启动。回滚二进制前必须恢复 v1
  数据库快照，不能让旧程序忽略新 schema。
- 更新 `docs/design.md`、`docs/roadmap.md`、`README.md`、`docs/operations.md` 和 backend specs，记录 API、
  schema v2、手动操作审计、恢复确认与 Hub 新启动参数。
- P4-2 完成时把 P4-1/P4-2 roadmap 标题标记为已完成；P4-3/P4-4/P4-5 不提前实现。

## 12. 失败矩阵

| 条件 | 结果 |
| --- | --- |
| 清单或 Ark 二进制启动前非法 | Hub 拒绝监听 |
| 运行期清单损坏 | 业务 API `503`，登录和 healthz 保持可用 |
| schema v1 -> v2 迁移失败 | 全部回滚，Hub 拒绝启动 |
| 上次 Hub 留有 running operation | 启动前原子改为 interrupted |
| Store 查询锁竞争 | 短 busy retry；context 优先取消 |
| schedule 输出无法解析 | host health unknown，不伪造 overdue |
| 未认证或 POST 缺 CSRF | `401` / `403`，不创建 operation |
| 已有活动手动操作 | `409`，不启动第二进程 |
| Ark 启动失败 | operation 持久化 fail，HTTP 返回失败 |
| Ark 非零退出或 JSON 损坏 | operation fail，不回显 stderr |
| preview 过期、session 不同或已消费 | `409/422`，不启动恢复 |
| snapshot、清单或目标冲突变化 | expected digest 不匹配，首次写入前失败 |
| safety backup 后目标状态变化 | 第二次 digest 不匹配，零恢复写入 |
| Hub graceful shutdown 时有活动命令 | 取消、等待、持久化 interrupted，再关闭 Store |
| Hub SIGKILL | Pdeathsig 终止 Ark；下次启动恢复 interrupted 状态 |
