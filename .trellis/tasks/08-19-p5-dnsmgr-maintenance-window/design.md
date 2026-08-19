# P5-3 技术设计

## 1. 边界与前提

本任务只修改 `/root/project/ark`。dnsmgr fork 已提供稳定的单任务启停接口：

```text
POST /api/dmonitor/task/setactive
id=<positive-integer>
active=0|1
```

接口由 `AuthApi` 和管理员权限共同保护，重复设置幂等，成功信封只有 `code=0`，不返回旧状态。因此 Ark 将清单中声明的任务视为“非维护期应启用”的任务，维护结束固定恢复为 `active=1`。这保持 P5-1 的最小接口和 P5-3“ark 侧配置、客户端调用与恢复兜底”的路线图边界。

## 2. 清单模型

复用 host 现有 `dnsmgr` 段，新增有序任务 ID：

```yaml
dnsmgr:
  base_url: https://dns.example.com
  env_file: /etc/ark/dnsmgr.env

hosts:
  - host: app-dr
    dnsmgr:
      task_ids:
        - 21
        - 34
      value: 203.0.113.10
      records:
        - domain_id: 12
          record_id: "provider-record-id"
```

`config.HostDNSMgr` 增加：

```go
TaskIDs []int64 `yaml:"task_ids,omitempty"`
```

三种组合都有效：

1. 只有 `task_ids`：自动维护窗口，不自动切 DNS。
2. 只有 `value/records`：保持 P5-2 行为，继续显示人工暂停项。
3. 两者都有：自动维护窗口并在恢复完成后自动切 DNS。

校验把 DNS 能力视为 `value` 与 `records` 成对出现，把维护能力视为非空 `task_ids`。至少存在一种能力；任一能力存在时顶层 `Config.DNSMgr` 必须存在。每个 task ID 必须大于 0 且在 host 内唯一。

## 3. 恢复计划

`restore.Plan` 增加非秘密维护计划：

```go
Maintenance *dnsmgr.MaintenancePlan `json:"maintenance,omitempty"`
```

`MaintenancePlan` 只保存有序 `TaskIDs`。`BuildPlan` 对同机和跨机目标都从 destination host 构建维护计划；它不依赖 DNS 计划。`WithIsolation` 清空维护计划，因为隔离恢复不停止生产项目，也不应操作生产检测。

维护计划参与规范 JSON 与 preview digest。这样 dry-run/inspect 能审计任务范围，执行前 task ID 漂移也会被 `--expected-preview-sha256` 拒绝。

人工项按能力分别处理：

- 有维护计划：移除人工暂停/恢复 dmonitor 项。
- 无维护计划：保留人工暂停/恢复项。
- 有 DNS 计划：移除人工 DNS 指向项。
- 无 DNS 计划：保留人工 DNS 指向项。
- 隔离恢复不需要 dmonitor 或 DNS 人工操作，`WithIsolation` 过滤这两项。

## 4. dnsmgr 客户端

在现有 `internal/dnsmgr.Client` 上增加：

```go
func (c *Client) SetTaskActive(ctx context.Context, taskID int64, active bool) error
```

请求复用 `Client.post`，表单为 `id` 与 `active=0|1`。方法先校验 `taskID > 0`，再调用固定路由。现有 HTTP timeout、禁止重定向、响应大小、JSON/业务码和秘密脱敏约束自动复用。

新增最小接口供编排与测试注入：

```go
type TaskActivator interface {
    SetTaskActive(context.Context, int64, bool) error
}
```

## 5. 维护窗口编排

在 `internal/dnsmgr/maintenance.go` 实现纯编排，职责与 DNS `Switch` 对称：

- `PauseTasks` 按计划顺序调用 `active=false`。
- 结果按计划记录全部 task ID，并用 `PauseStatus` 标识本轮实际暂停成功的任务。
- 暂停中途失败时停止后续任务。HTTP/传输错误无法证明服务端未写入，因此先对当前失败项调用幂等的 `active=true`，再逆序补偿已暂停项。
- `ResumeTasks` 逆序恢复所有已暂停项，单项失败后继续后续恢复。
- 结果记录每个 task ID 的 `paused`、`failed`、`not_attempted`、`restored` 等状态及人工任务列表；整体恢复失败使用 `restore_failed`。

根命令通过 `signal.NotifyContext` 把 `SIGINT` 与 `SIGTERM` 转为 Cobra context 取消。恢复兜底 context 使用 `context.WithoutCancel` 脱离命令取消，再套固定总超时。这样 Ctrl+C、systemd 停止、上游 context 取消或恢复函数返回错误时，defer 仍有机会完成 API 调用，同时不会无限阻塞命令退出。公开维护函数显式拒绝 nil context，避免在兜底路径触发 panic。

## 6. 恢复执行时序

维护窗口必须晚于只读和可回滚前置动作、早于首次目标写入。现有 `restore.Execute` 已把 `ExecuteOptions.OnPlanReady` 放在以下动作之后：

1. 目标 inspect。
2. expected preview digest 校验。
3. 冲突授权与可选 safety backup。
4. safety backup 后二次 inspect。

同时它位于恢复状态 marker、停容器和所有恢复 step 之前。因此扩展 `OnPlanReady` 的职责最合适：CLI 在非 JSON 输出计划后开启维护窗口；JSON 模式也安装同一回调，只跳过人类计划输出。

目标时序：

```text
load config -> lock -> repo/manifest -> BuildPlan
-> doctor(local/target/dnsmgr)
-> restore.Execute preflight + optional safety backup
-> OnPlanReady: print plan -> pause dmonitor tasks
-> marker / stop containers / restore steps / completion marker
-> DNS switch or compensation
-> deferred resume dmonitor tasks with bounded cleanup context
-> merge maintenance result and errors -> unlock
```

`OnPlanReady` 暂停失败会让 `restore.Execute` 在首次写入前返回。由于 callback 现有错误前缀“输出恢复计划失败”过窄，应同步改为能覆盖“恢复计划就绪回调失败”的通用错误，并更新现有测试断言。

## 7. CLI 生命周期与结果合并

`restoreDependencies` 增加可注入的任务启停编排函数；client 创建条件从 `plan.DNS != nil` 扩展为“DNS 或 maintenance 任一存在”。dnsmgr doctor 采用同一条件。

CLI 持有以下运行态：

- client：共享给维护任务和 DNS 切换。
- maintenance result：记录暂停与恢复状态，并通过 `PauseStatus` 标识本轮暂停成功项和结果未知的失败项。
- maintenance opened：控制 defer 是否需要恢复。

defer 在全局锁释放前执行任务恢复，确保一次 restore 的所有 dnsmgr 联动仍在同一进程级互斥区间内。恢复完成后把 maintenance 摘要挂到 `restore.Result`：

- 原 restore/DNS 已失败：保留原 `Status/Error`，用 `errors.Join` 追加恢复任务错误。
- 原流程成功但恢复任务失败：把状态改为 `fail`，错误摘要改为“恢复已完成，但 dmonitor 任务恢复不完整”。
- 追加 `人工启用 dnsmgr dmonitor 任务 <id>`，不输出凭证或请求内容。

暂停失败发生在 `restore.Execute` 返回前；结果仍需保留 plan 的身份字段和 maintenance 摘要，明确目标写入未开始。

## 8. 失败矩阵

| 条件 | 行为 |
|---|---|
| 未配置 `task_ids` | 不创建维护动作，保留人工暂停项 |
| dry-run / inspect / isolate / backup / verify | 零维护 HTTP |
| dnsmgr doctor 失败 | 恢复在目标预检和写入前中止 |
| client 创建失败 | 恢复在目标写入前中止，任务状态未改变 |
| 第 N 个任务暂停失败 | N+1 之后不调用，先幂等恢复结果未知的第 N 个，再逆序恢复前 N-1 个，恢复步骤为 0 |
| safety backup 失败 | 尚未暂停任务，直接失败 |
| 数据恢复失败 | defer 逆序恢复全部已暂停任务，合并错误 |
| DNS 切换或补偿失败 | defer 仍恢复全部已暂停任务，保留 DNS 结果 |
| 命令 context 取消 | cleanup context 独立尝试恢复，限时退出 |
| 单个任务恢复失败 | 继续其余任务，整体失败并列出人工任务 |
| SIGKILL / 掉电 | defer 无法执行，按输出/清单 task ID 人工设为 active=1 |

## 9. 兼容性与回滚

- schema 版本保持 v2；所有新增字段和结果字段均为可选。
- P5-2 只配置 DNS 的清单、DNS 补偿和 completion marker 语义不变。
- 回滚时先从 host 删除 `task_ids` 并执行 `ark validate`，后续恢复即回到人工维护流程；再回滚 ark 二进制。
- dnsmgr 无需发布新版本，只要求已部署 P5-1 固定路由。

## 10. 验证策略

- config：组合兼容、空能力、task ID 正数/重复、顶层依赖、错误路径。
- client：路径与表单、真假 active、HTTP/JSON/业务错误、缺失业务码、超时、3xx、响应上限、秘密泄漏。
- maintenance：顺序暂停、失败请求未知副作用补偿、逆序补偿、逆序恢复、继续收集失败、nil/已取消 context。
- plan/isolation：同机/跨机计划、深拷贝、人工项、JSON/digest、隔离清除。
- CLI：doctor 与 client 条件、OnPlanReady 精确时序、暂停失败零写入、execute/DNS 各失败路径的 defer、`SIGINT`/`SIGTERM` context 传递、取消后的恢复、错误合并和 JSON/人类输出。
- 全量：`gofmt`、`go vet ./...`、`go test ./...`、`make check`、示例清单校验。
