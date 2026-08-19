# DNSMgr Integration Guidelines

> ark 与 dnsmgr fork 的恢复维护窗口、DNS 切换、凭证、补偿和 provider 兼容契约。

---

## Scenario: 恢复期间维护 dmonitor 并在完成后切换 DNS

### 1. Scope / Trigger

以下改动必须遵守本规约：

- 修改清单中的 `dnsmgr` 顶层连接配置、host 级任务或记录关联；
- 修改 `internal/dnsmgr` client、签名、凭证加载、任务维护、切换或补偿逻辑；
- 修改恢复计划中的 maintenance/DNS 动作、doctor 门槛、completion marker 后置时序或结果输出；
- 修改 dnsmgr fork 的 `/api/auth/check`、`/api/dmonitor/task/setactive`、
  `/api/record/value/:id` 或 provider 记录定位逻辑。

任务与记录关联是当前部署意图，不是历史备份事实，不得写入 backup manifest。ark 只编排维护窗口与
恢复后切换；记录详情、provider 差异和 Value-only 更新由 dnsmgr fork 负责。

### 2. Signatures

```yaml
dnsmgr:
  base_url: https://dns.example.com
  env_file: /etc/ark/dnsmgr.env

hosts:
  - host: web-dr
    dnsmgr:
      task_ids: [21, 34]
      value: 203.0.113.10
      records:
        - domain_id: 12
          record_id: "provider-record-id"
```

```text
POST /api/auth/check
POST /api/dmonitor/task/setactive
POST /api/record/value/:domain_id

id=<positive-task-id>
active=<0-or-1>

recordid=<provider-record-id>
value=<IPv4-or-IPv6>
expected_value=<optional-current-value>
```

```go
func New(baseURL string, envFile string) (*Client, error)
func (c *Client) CheckAuth(ctx context.Context) error
func (c *Client) SetTaskActive(ctx context.Context, taskID int64, active bool) error
func (c *Client) SetRecordValue(
    ctx context.Context,
    domainID int64,
    recordID string,
    value string,
    expectedValue *string,
) (ValueResult, error)
func PauseTasks(
    ctx context.Context,
    activator TaskActivator,
    plan MaintenancePlan,
) (MaintenanceResult, error)
func ResumeTasks(
    ctx context.Context,
    activator TaskActivator,
    result MaintenanceResult,
) (MaintenanceResult, error)
func Switch(
    ctx context.Context,
    setter ValueSetter,
    plan Plan,
) (SwitchResult, error)
```

### 3. Contracts

#### 清单与凭证

- 顶层 `dnsmgr` 只保存 `base_url` 与绝对路径 `env_file`；host 级配置可独立或组合保存有序
  `task_ids`，以及一个目标 IP 和有序 `{domain_id, record_id}`。未配置时 schema v2、backup、verify
  和普通恢复行为不变。
- `task_ids` 必须为正整数且 host 内唯一；配置至少包含 maintenance 或 DNS 一种能力。只配置
  `task_ids`、只配置 `value/records` 以及两者同时配置都合法。任务在恢复前必须是运维意图上的启用
  任务，ark 结束时固定设置 `active=1`，不保存也不推断历史关闭状态。
- `Config.Validate` 只做静态校验，不读文件、不连网络。凭证文件的存在性、owner、权限和内容由
  `internal/secretfile`、`internal/dnsmgr` 与 `doctor.RunDNSMgr` 校验。
- 凭证文件只允许 `ARK_DNSMGR_UID`、`ARK_DNSMGR_API_KEY`，必须是当前 UID 所有的普通文件，
  以 `O_NOFOLLOW` 打开且权限不宽于 `0600`。未知键、非正整数 UID 或空 API key 必须拒绝。
- `base_url` 默认只允许 HTTPS；HTTP 仅允许 loopback。client 必须设置有限超时、响应大小上限，
  且不跟随 3xx。成功信封必须明确包含整数 `code=0`；顶层 `null`、缺少 `code`、`code=null` 或
  类型错误都必须 fail closed。错误不得包含 UID、API key、签名、完整 URL 或外部响应正文。

#### dnsmgr Value-only API

- `/api/auth/check` 只验证现有 `AuthApi`，不得查询 provider 或写数据库。
- `/api/record/value/:id` 只允许 A/AAAA，必须校验域名、权限、记录、目标地址族和可选
  `expected_value` 地址族。
- 服务端优先 `getDomainRecordInfo(recordid)`；详情不可用时分页扫描标准化 `getDomainRecords`，
  按 `RecordId` 精确定位。带 `Count` 的 qingcloud 分组行必须通过 `getSubDomainRecords` 展开；
  分页按 total、短页或重复页收敛，并设置明确上限。
- 更新必须从 provider 当前记录保留 `Name`、`Type`、`Line`、`TTL`、`MX`、`Weight`、`Remark`，
  只替换 `Value`。qingcloud 更新还必须保留详情响应中的当前 `mode`，不能依赖新接口未提交的 mode。
- 当前值已经等于目标值时先返回 `changed=false`，不调用 provider；即使 `expected_value` 已过期，
  目标状态也已达成。只有需要实际写入时才比较 `expected_value`，不匹配则 fail closed。
- 成功结果固定返回 `recordid`、`previous_value`、`value`、`changed`。HTTP、JSON、业务码或字段
  不符合契约时 ark client 必须返回脱敏错误。

#### dmonitor 维护 API

- `/api/dmonitor/task/setactive` 由 `AuthApi` 保护并执行管理员权限检查，只接受单个正整数 `id` 与
  `active=0|1`；重复设置必须幂等，不得通过该接口暴露任务新增、编辑、删除或批量操作。
- `PauseTasks` 按清单顺序设置 `active=0`。第 N 项失败后停止前向调用，把后续项标为
  `not_attempted`。传输失败不能证明服务端未写入，因此必须先把第 N 项幂等恢复为 `active=1`，再逆序
  恢复此前已暂停任务；补偿必须继续尝试全部任务，失败项进入 `manual_task_ids`。
- 完整暂停成功后，CLI 保存实际已暂停的任务 ID。`ResumeTasks` 必须逆序设置 `active=1`，单项失败
  不得阻止其余任务恢复；失败 ID 进入 `manual_task_ids`，整体状态为 `restore_failed`。
- 根命令必须用 `signal.NotifyContext` 把 `SIGINT`/`SIGTERM` 转为 Cobra context 取消。恢复兜底使用
  `context.WithoutCancel` 和固定总超时，保证父 context 取消后仍发恢复请求；公开维护函数必须拒绝
  nil context 而不是 panic。该保证不覆盖 `SIGKILL`、断电或宿主机崩溃，运维必须按清单和结构化
  结果人工恢复 `active=1`。

#### 恢复时序与补偿

- `restore.Plan.Maintenance` 只包含有序任务 ID；`restore.Plan.DNS` 只包含目标 IP 和有序记录标识。
  两者参与 JSON、dry-run、inspect 和 preview digest，不包含 base URL、env file 或凭证。
  `WithIsolation` 必须清空 Maintenance 与 DNS 计划。
- dry-run、inspect、隔离恢复、backup 和 verify 都不得加载 dnsmgr 凭证或调用 HTTP。同机真实原位
  恢复可以打开维护窗口，但不生成 DNS 动作。
- `restore.Execute` 完成 preflight、expected digest 与安全备份后，CLI 在 `OnPlanReady` 中先输出计划，
  再创建共享 client 并暂停任务。暂停失败必须在目标写入前返回；安全备份或更早失败不得暂停任务。
- 维护窗口必须覆盖数据恢复和 P5-2 DNS 切换，并在全局锁释放前关闭。普通返回路径通过 `defer`
  逆序恢复；恢复失败把整体状态改为 `fail`，但不得覆盖更早的 restore/DNS 主错误。
- 真实跨机原位恢复只有在 `restore.Execute` 成功写入 completion marker 并返回 `ok` 或无执行错误的
  `warn` 后才能调用 `dnsmgr.Switch`。DNS 成功保留原状态；DNS 失败把整体状态改为 `fail`。
- 多记录按清单顺序执行，只有 `changed=true` 进入补偿栈。前向失败后停止后续记录，并逆序补偿；
  补偿写回 `previous_value`，同时传本轮目标 IP 作为 `expected_value`。
- 补偿必须继续尝试全部栈项，并使用独立、有上限的 context；失败项进入 `manual_records`。
  DNS 失败不得删除 completion marker，重跑必须跳过已完成数据写入并只重试 DNS。
- `ark doctor` 可独立检查 dnsmgr；backup/verify 不调用该检查，restore 在 Plan 含 Maintenance 或 DNS
  时把它作为门槛。两种能力必须复用同一个安全 client。

### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| host 配置 maintenance 或 DNS 但顶层 `dnsmgr` 缺失 | `hosts[i](name).dnsmgr` 静态校验失败 |
| host dnsmgr 无任何能力，或 task ID 非正数/重复 | 聚合字段级错误，零文件和网络访问 |
| 目标值不是 IP、记录为空、`domain_id <= 0`、`record_id` 空或重复 | 聚合字段级错误，零文件和网络访问 |
| 凭证文件是相对路径、symlink、owner 不符、权限过宽或含未知键 | client/doctor fail closed，不回显内容 |
| 非 loopback HTTP、3xx、超时、非 2xx、非法 JSON、业务码非 0 或响应过大 | 请求失败且错误脱敏 |
| provider 详情不可用 | 扫描标准化列表；qingcloud 分组先展开，无法定位则返回通用业务错误 |
| 记录不是 A/AAAA、IP 地址族不匹配或元数据不完整 | provider 更新调用次数为 0 |
| 当前值等于目标值且 `expected_value` 已过期 | `changed=false` 幂等成功，provider 更新调用次数为 0 |
| 当前值是第三个值且需要实际写入 | `expected_value` 冲突，拒绝覆盖 |
| 前向第 N 条失败 | N+1 之后为 `not_attempted`，已变更记录逆序补偿，命令返回非零 |
| 补偿部分失败 | 继续剩余补偿，状态 `rollback_failed`，列出人工记录 |
| DNS 失败后重跑 | completion marker 保留，数据写入次数不增加，DNS 再调用一次 |
| 安全备份或 preflight 失败 | 任务 API 调用次数为 0 |
| 暂停第 N 个任务失败 | 不开始目标写入，后续任务不调用，先恢复结果未知的第 N 项，再逆序恢复已暂停任务 |
| restore/DNS 失败且恢复任务失败 | 保留原始主错误，附加恢复错误并列出人工任务，命令非零 |
| 父 context 已取消 | 使用独立有限 context 继续逆序恢复全部已暂停任务 |

### 5. Good / Base / Bad Cases

- **Good**：跨机恢复写入 completion marker 后，两条记录按顺序切换；第二条失败，第一条用
  `expected_value=目标IP` 补偿成功，结果为 `rolled_back`，命令仍返回非零。
- **Base**：未配置 dnsmgr，Plan 没有 `dns` 字段，所有命令保持零新增凭证读取和 HTTP 调用。
- **Good**：补偿重跑时记录已由管理员恢复为旧值，即使 expected 已过期也返回 `changed=false`，
  不把已达成状态误报为冲突。
- **Good**：两个 dmonitor 任务按 `21,34` 暂停，恢复失败后仍按 `34,21` 恢复；任务 34 恢复失败时
  继续恢复任务 21，并把 34 写入 `manual_task_ids`。
- **Bad**：ark 清单保存 Name、TTL、Line 等可变 provider 元数据，并调用旧 `record/update`；字段漂移
  会导致恢复时覆盖错误配置。
- **Bad**：DNS 失败后删除 completion marker；重跑会重复破坏性数据恢复，而不是只重试切流。

### 6. Tests Required

- `internal/config/config_test.go`：旧清单兼容、只维护、只 DNS、组合能力、IP、URL、绝对路径、
  空能力、非法/重复任务与记录，以及 `hosts[i](name).dnsmgr...` 错误前缀。
- `internal/dnsmgr/client_test.go`：固定签名向量、请求路径与表单、响应字段/地址族、真实 HTTP 超时、
  3xx 不跟随、响应上限、缺失业务码、`setactive` 的 `id/active` 表单，以及每个失败分支的秘密泄漏断言。
- `internal/dnsmgr/maintenance_test.go`：顺序暂停、失败请求未知副作用补偿、逆序恢复、恢复失败继续、
  人工任务收集、nil context 拒绝，以及父 context 取消后仍执行的有限兜底。
- `internal/dnsmgr/switch_test.go`：顺序调用、`changed=false` 不入栈、逆序补偿、补偿继续、
  expected 参数和人工记录。
- `internal/restore/plan_test.go`、`internal/cli/restore_test.go`：Maintenance/DNS 深拷贝与 digest、同机/
  隔离差异、dry-run/inspect 零凭证零 HTTP、暂停/写入/marker/DNS/恢复/解锁顺序、暂停失败零写入、
  主错误与恢复错误合并，以及 DNS 失败重跑只重试后置步骤。
- dnsmgr fork 的可执行夹具：详情优先、无 total 分页、重复页收敛、qingcloud 分组展开、A/AAAA、
  元数据保留、幂等优先、expected 冲突、provider 失败、缺失字段和 qingcloud mode 保留。
- 提交前运行 `make check`、示例清单校验、dnsmgr PHP 语法与行为夹具、Composer 严格校验，
  并在测试部署上先验证 auth check，再使用可回滚记录验证 forward 和 expected compensation。

### 7. Wrong vs Correct

#### Wrong

```go
type DNSPlan struct {
    // 错误：把 provider 当前可变元数据复制进 ark 的恢复计划。
    Name  string `json:"name"`
    Type  string `json:"type"`
    Line  string `json:"line"`
    TTL   int    `json:"ttl"`
    Value string `json:"value"`
}
```

这会让 ark 与 provider 字段耦合，记录在备份后发生的 TTL、线路或模式调整可能被恢复流程覆盖。

#### Correct

```go
result, err := restore.Execute(ctx, plan, repo, runner, options)
if err != nil || (result.Status != store.StatusOK && result.Status != store.StatusWarn) {
    return result, err
}
dnsResult, dnsErr := dnsmgr.Switch(ctx, client, *plan.DNS)
result.DNS = &dnsResult
if dnsErr != nil {
    // DNS 失败保留 completion marker，让同一恢复只重试后置切流。
    result.Status = store.StatusFail
    result.Error = "数据恢复已完成，但 DNS 切换失败"
    return result, dnsErr
}
return result, nil
```

ark 只传稳定记录标识和目标 IP；dnsmgr 从 provider 读取当前元数据并执行 Value-only 更新。
