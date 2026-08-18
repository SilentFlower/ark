# DNSMgr Integration Guidelines

> ark 与 dnsmgr fork 的恢复后 DNS 切换、凭证、补偿和 provider 兼容契约。

---

## Scenario: 恢复完成后通过 dnsmgr 切换 DNS

### 1. Scope / Trigger

以下改动必须遵守本规约：

- 修改清单中的 `dnsmgr` 顶层连接配置或 host 级记录关联；
- 修改 `internal/dnsmgr` client、签名、凭证加载、切换或补偿逻辑；
- 修改恢复计划中的 DNS 动作、doctor 门槛、completion marker 后置时序或结果输出；
- 修改 dnsmgr fork 的 `/api/auth/check`、`/api/record/value/:id` 或 provider 记录定位逻辑。

DNS 关联是当前部署意图，不是历史备份事实，不得写入 backup manifest。ark 只编排恢复后切换；
记录详情、provider 差异和 Value-only 更新由 dnsmgr fork 负责。

### 2. Signatures

```yaml
dnsmgr:
  base_url: https://dns.example.com
  env_file: /etc/ark/dnsmgr.env

hosts:
  - host: web-dr
    dnsmgr:
      value: 203.0.113.10
      records:
        - domain_id: 12
          record_id: "provider-record-id"
```

```text
POST /api/auth/check
POST /api/record/value/:domain_id

recordid=<provider-record-id>
value=<IPv4-or-IPv6>
expected_value=<optional-current-value>
```

```go
func New(baseURL string, envFile string) (*Client, error)
func (c *Client) CheckAuth(ctx context.Context) error
func (c *Client) SetRecordValue(
    ctx context.Context,
    domainID int64,
    recordID string,
    value string,
    expectedValue *string,
) (ValueResult, error)
func Switch(
    ctx context.Context,
    setter ValueSetter,
    plan Plan,
) (SwitchResult, error)
```

### 3. Contracts

#### 清单与凭证

- 顶层 `dnsmgr` 只保存 `base_url` 与绝对路径 `env_file`；host 级配置保存一个目标 IP 和有序
  `{domain_id, record_id}`。未配置时 schema v2、backup、verify 和普通恢复行为不变。
- `Config.Validate` 只做静态校验，不读文件、不连网络。凭证文件的存在性、owner、权限和内容由
  `internal/secretfile`、`internal/dnsmgr` 与 `doctor.RunDNSMgr` 校验。
- 凭证文件只允许 `ARK_DNSMGR_UID`、`ARK_DNSMGR_API_KEY`，必须是当前 UID 所有的普通文件，
  以 `O_NOFOLLOW` 打开且权限不宽于 `0600`。未知键、非正整数 UID 或空 API key 必须拒绝。
- `base_url` 默认只允许 HTTPS；HTTP 仅允许 loopback。client 必须设置有限超时、响应大小上限，
  且不跟随 3xx，错误不得包含 UID、API key、签名、完整 URL 或外部响应正文。

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

#### 恢复时序与补偿

- `restore.Plan.DNS` 只包含目标 IP 和有序记录标识，参与 JSON、dry-run、inspect 和 preview digest；
  不包含 base URL、env file 或凭证。`WithIsolation` 必须清空 DNS 计划。
- dry-run、inspect、同机恢复、隔离恢复以及任何数据恢复失败路径都不得加载凭证或调用 HTTP。
- 真实跨机原位恢复只有在 `restore.Execute` 成功写入 completion marker 并返回 `ok` 或无执行错误的
  `warn` 后才能调用 `dnsmgr.Switch`。DNS 成功保留原状态；DNS 失败把整体状态改为 `fail`。
- 多记录按清单顺序执行，只有 `changed=true` 进入补偿栈。前向失败后停止后续记录，并逆序补偿；
  补偿写回 `previous_value`，同时传本轮目标 IP 作为 `expected_value`。
- 补偿必须继续尝试全部栈项，并使用独立、有上限的 context；失败项进入 `manual_records`。
  DNS 失败不得删除 completion marker，重跑必须跳过已完成数据写入并只重试 DNS。
- `ark doctor` 可独立检查 dnsmgr；backup/verify 不调用该检查，restore 仅在 Plan 含 DNS 时把它作为门槛。

### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| host 配置 DNS 但顶层 `dnsmgr` 缺失 | `hosts[i](name).dnsmgr` 静态校验失败 |
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

### 5. Good / Base / Bad Cases

- **Good**：跨机恢复写入 completion marker 后，两条记录按顺序切换；第二条失败，第一条用
  `expected_value=目标IP` 补偿成功，结果为 `rolled_back`，命令仍返回非零。
- **Base**：未配置 dnsmgr，Plan 没有 `dns` 字段，所有命令保持零新增凭证读取和 HTTP 调用。
- **Good**：补偿重跑时记录已由管理员恢复为旧值，即使 expected 已过期也返回 `changed=false`，
  不把已达成状态误报为冲突。
- **Bad**：ark 清单保存 Name、TTL、Line 等可变 provider 元数据，并调用旧 `record/update`；字段漂移
  会导致恢复时覆盖错误配置。
- **Bad**：DNS 失败后删除 completion marker；重跑会重复破坏性数据恢复，而不是只重试切流。

### 6. Tests Required

- `internal/config/config_test.go`：旧清单兼容、顶层/host 组合、IP、URL、绝对路径、空/重复记录和
  `hosts[i](name).dnsmgr...` 错误前缀。
- `internal/dnsmgr/client_test.go`：固定签名向量、请求路径与表单、响应字段/地址族、真实 HTTP 超时、
  3xx 不跟随、响应上限，以及每个失败分支的秘密泄漏断言。
- `internal/dnsmgr/switch_test.go`：顺序调用、`changed=false` 不入栈、逆序补偿、补偿继续、
  expected 参数和人工记录。
- `internal/restore/plan_test.go`、`internal/cli/restore_test.go`：DNSPlan 深拷贝与 digest、同机/隔离清除、
  dry-run/inspect 零凭证零 HTTP、completion marker 后置顺序、失败路径零调用和重跑只重试 DNS。
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
