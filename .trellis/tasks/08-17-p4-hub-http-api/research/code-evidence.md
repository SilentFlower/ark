# P4-2 代码证据与约束

## 已有边界

- `internal/hub/http.go` 已建立默认鉴权路由、session CSRF、统一安全响应头和 API 未认证
  `401` 行为。新路由应继续挂在同一个 `application`，不能另建绕过鉴权的 mux。
- `internal/hub/serve.go` 当前只把 `stateStore` 当作可关闭资源，`ServeOptions` 只有监听、
  状态库、凭证文件和 Secure Cookie。P4-2 需要把受限查询接口、清单、Ark 二进制和操作管理器
  注入应用，但 `Run` 仍只能通过 `store.Open` 获得状态库。
- `internal/hub/install.go` 与 `internal/systemd.HubInstallOptions` 尚未携带 Ark 二进制和清单路径；
  P4-2 修改 `serve` 参数时必须同步修改 `ark-hub install` 与生成的 `ExecStart`。
- `internal/cli/backup.go`、`verify.go`、`restore.go` 已提供稳定 `--json` 输出和真实业务编排。
  Hub 只能通过无 shell argv 启动这些命令，不能导入内部未导出的 CLI 类型或复制业务逻辑。

## 状态库现状

- `internal/store/schema.sql` 当前 schema v1 包含 `runs`、`run_targets`、`doctor_reports`、
  `verifications`，没有手动操作表。
- `internal/store/store.go` 的迁移数组按版本顺序执行完整 SQL，使用同连接
  `BEGIN IMMEDIATE` 和 `PRAGMA user_version`。新增操作表应作为独立 schema v2 迁移，不能修改
  v1 SQL 后假装旧数据库会自动获得新表。
- Store 当前公开读取只有 `GetRun` 和 `LastSuccessfulTargetBytes`，没有列表、分页、最新状态、
  doctor 或 verification 查询。P4-2 必须把 SQL 留在 Store 内并提供稳定 DTO。
- `RecordDoctorReport` 与 `doctor_reports.next_run_at` 已存在，但生产代码没有调用记录方法；
  当前只有测试写入。设计文档却明确要求状态库保存 doctor 和下一次计划时间，因此这是 P4-2
  查询链路必须补齐的前置数据缺口。

## 查询数据流

```text
/etc/ark/ark.yaml
  -> config.LoadAndValidate
  -> host / target / effective schedule
                         \
ark.db -> store query DTO -> hub projection -> /api/hosts, /api/runs, /api/alerts
```

- 清单只提供声明事实；运行健康来自状态库。Hub projection 负责把两者按 host 名关联。
- API 不得序列化完整 `config.Host`，否则会泄露 SSH identity、known_hosts、compose/env 路径等
  不属于控制台展示面的信息。
- hosts 和 alerts 必须调用同一个健康派生函数，避免同一 host 在两个接口得到不同结论。

## 异步操作数据流

```text
POST + authenticated session + session CSRF
  -> strict request decode
  -> validate host / mode / snapshot
  -> create persistent manual_operation(running)
  -> exec ark --config ... <backup|verify|restore> --json
  -> bounded stdout/stderr capture
  -> strict JSON decode + redact
  -> finish manual_operation(ok|fail)
  -> GET /api/operations/{id}
```

- HTTP `WriteTimeout` 是 30 秒，真实命令不能同步占用响应；POST 应返回 `202 Accepted` 和操作 ID。
- 同一 Hub 进程只允许一个手动长任务；现有 `/run/ark.lock` 继续作为与 systemd oneshot 之间的
  最终互斥边界。
- 用户已确认操作状态持久化。启动恢复时先把遗留 `running` 记录原子更新为 `interrupted`，
  以区分业务失败和 Hub 生命周期中断。
- 直接 `exec.CommandContext` 使用 argv，禁止 `sh -c`。命令输出必须有大小上限，错误响应不回显
  原始 stderr；持久化结果只保存白名单字段和脱敏摘要。

## 恢复确认

- CLI 的 `ark restore --dry-run --json` 已返回完整 `restore.Plan`，是真实恢复前唯一可信预览来源。
- 确认凭据应由高熵随机 ID 标识，服务端内存保存规范化请求、计划 SHA-256、session 绑定、
  到期时间和消费状态。数据库只保存最终操作请求摘要，不保存确认 token。
- 执行确认时重新运行 dry-run，并比较新计划摘要；snapshot 或清单发生变化时必须拒绝旧确认。
  只比较原请求参数不足以发现仓库 latest 指向或当前清单已经变化。
- token 成功消费与创建 operation 必须在同一临界区完成，避免两个并发确认启动两次恢复。

## 计划周期与告警

- `config.Schedule.OnCalendar` 是任意 systemd calendar 表达式，不能自行写近似 cron parser。
- 项目现有决定是委托 `systemd-analyze calendar`。健康判定需要最近两个相邻触发时间的差值，
  并以该有效周期的两倍作为超时阈值；命令失败时状态应为未知或告警，不得伪造健康。
- `doctor.Report` 当前只把 `Next elapse` 拼进人类可读 detail，没有结构化时间。
  P4-2 应新增单一 schedule probe，供 doctor 持久化和 Hub 健康计算复用，避免解析展示文本。
- 本机 systemd 255 支持 `calendar --iterations=N --base-time=TIMESTAMP`，但不支持 calendar JSON。
  在 `LC_ALL=C` 下，每次迭代都提供稳定的 `(in UTC): ... UTC` 行。探测器可请求两次迭代，
  解析两条 UTC 时间：第一条作为 `next_run_at`，两条之差作为当前有效计划周期。
- 探测命令应使用显式 `--base-time=<UTC RFC3339>`，让测试可注入基准时间；输出缺少两次 UTC
  时间、第二次不晚于第一次或命令失败时返回错误，Hub 将健康状态标记为未知而不是自行猜周期。

## 测试重点

- schema v1 -> v2 迁移、并发首次迁移、遗留 running -> interrupted、WAL 写时分页读取。
- API 空库、分页、NULL、未知 host、敏感字段缺失和 hosts/alerts 判定一致性。
- POST session CSRF、未知字段、body 上限、并发互斥、argv、stdout/stderr 上限、JSON 损坏。
- restore 预览零副作用、计划变化、过期、session 不匹配、重复消费和并发双确认。
- `ark-hub install` 新参数、systemd verify、旧 timer 零改动，以及常规/无 CGO 构建。

## 相关规范

- `.trellis/spec/backend/hub-guidelines.md`
- `.trellis/spec/backend/database-guidelines.md`
- `.trellis/spec/backend/external-command-guidelines.md`
- `.trellis/spec/backend/error-handling.md`
- `.trellis/spec/backend/logging-guidelines.md`
- `.trellis/spec/guides/cross-layer-thinking-guide.md`
