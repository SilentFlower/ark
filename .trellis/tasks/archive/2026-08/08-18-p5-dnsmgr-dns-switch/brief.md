# Brief — P5-2 恢复后自动切换 dnsmgr DNS

## Goal

- 让 ark 在跨机原位恢复完成后，把目标主机关联的 A 或 AAAA 记录自动切换到清单显式声明的新 IP，并保证 dry-run 零外部写、失败可补偿、重跑可恢复且凭证不泄漏。

## Scope

- 在 dnsmgr fork 增加受 `AuthApi` 保护的无副作用 `/api/auth/check` 和 Value-only `/api/record/value/:id`。
- Value-only 接口从 provider 读取当前记录，保留 Name、Type、Line、TTL、MX、Weight、Remark，只修改 Value，并支持幂等与 `expected_value` 冲突保护。
- 在 ark schema v2 增加可选顶层 dnsmgr 连接配置，以及 host 级目标 IP 和多记录关联；旧清单保持兼容。
- 把非秘密 DNS 动作加入 restore plan、JSON、dry-run、inspect 和 preview digest，只在跨机原位恢复 completion marker 成功后执行。
- 新增安全凭证加载、AuthApi client、独立 dnsmgr doctor、顺序切换、逆序补偿、结构化结果、人类输出和相关文档。
- 先部署 dnsmgr API，再部署 ark，最后启用清单配置；提供移除 host 关联和镜像回退的操作步骤。

## Non-Goals

- 不自动暂停或恢复 dmonitor 任务，该能力属于 P5-3。
- 不等待 DNS TTL 生效、不轮询公网解析、不做流量调度。
- 不支持一个 host 同时配置不同 IPv4 与 IPv6 目标值，也不修改记录类型。
- 不触发证书部署，该能力属于 P5-4。
- 不修改 backup manifest。

## Key Decisions

- 不直接使用要求完整元数据的现有 `record/update`；增加只接收稳定记录标识和新 Value 的窄接口，避免清单字段漂移。
- 新 DNS IP 由目标 host 清单显式声明，不从允许主机名的 `ssh.address` 推导，也不要求每次传 CLI 参数。
- 多记录部分失败时自动逆序补偿；dnsmgr 返回旧值，补偿使用本轮目标值作为 `expected_value`，避免覆盖期间发生的其他切换。
- DNS 动作位于 completion marker 之后；DNS 失败保留 marker，使再次运行 restore 能跳过已完成数据步骤并重试 DNS。
- 恢复 `ok` 或现有语义下仅缺 healthcheck 的 `warn` 都可切 DNS；DNS 成功后保留原状态，DNS 或补偿失败则整体返回 `fail`。
- dnsmgr 可达性使用独立 doctor 检查，不能反向阻断 backup 或 verify。

## Key Context

- ark 主要入口：`internal/config/config.go`、`internal/restore/plan.go`、`internal/restore/execute.go`、`internal/cli/restore.go`、`internal/doctor`。
- ark 新边界：`internal/dnsmgr` client，以及供 monitoring/dnsmgr 共用的最小 `internal/secretfile` 安全打开 helper。
- dnsmgr 主要入口：`route/app.php`、`app/middleware/AuthApi.php`、`app/controller/Auth.php`、`app/controller/Domain.php`、`app/lib/DnsInterface.php`。
- 凭证 env 文件只允许 `ARK_DNSMGR_UID` 和 `ARK_DNSMGR_API_KEY`，要求绝对路径、NOFOLLOW、普通文件、当前 UID 所有、权限不宽于 `0600`。
- `base_url` 只允许 HTTPS，HTTP 仅允许 loopback；HTTP client 设置有限超时并拒绝重定向认证表单。
- P5-3 前仍需人工暂停并恢复相关 dmonitor 检测；自动 DNS 成功后不再保留“确认 DNS 指向目标主机”人工项。

## Risks / Deferred

- P5-3 未完成时 dmonitor 可能与恢复联动竞争；当前通过人工暂停/恢复和补偿 `expected_value` 降低风险。
- qingcloud、dnsmgr、powerdns 的 `getDomainRecordInfo` 当前不可用；实现需在详情失败时分页扫描标准化记录列表，qingcloud 的带 `Count` 分组行还需展开子域名记录，并覆盖分页收敛及 null/缺省字段。
- dnsmgr 没有现成测试框架；需要增加可测纯逻辑并结合 PHP 语法检查、容器级 API 冒烟验证真实路由。
- 双栈目标值下沉到 record 级配置明确延后，不在本任务预埋模型。

## Acceptance

- 未配置 dnsmgr 的 schema v2 清单和现有 backup、verify、同机/跨机恢复行为不变。
- 静态校验、dry-run、inspect、preview digest 和输出覆盖 DNS 计划，且 dry-run/inspect 不读取凭证或调用 HTTP。
- 只有跨机原位恢复 completion marker 成功后才更新 DNS；同机、隔离及任何恢复失败路径均不调用 dnsmgr。
- Value-only API 对 A/AAAA 保留全部原元数据，支持幂等、地址族校验、expected 冲突保护和无副作用认证检查。
- 多记录失败会停止前向更新并逆序补偿；完整回滚与回滚不完整都返回非零，后者能定位人工处理记录。
- 凭证、签名、HTTP/JSON、超时、重定向和错误输出均有测试与秘密泄漏断言。
- `ark doctor` 能检查 dnsmgr，backup/verify 不依赖其可用性；ark 与 dnsmgr 相关验证、文档和部署回滚说明完成。

## Next Step

- 首轮实现、dnsmgr API 部署与 Full Check-All 修复重检已完成；下一步进入项目规范沉淀。
