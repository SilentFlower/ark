# Brief — P5-3 维护窗口联动

## Goal

- 在真实原位恢复首次写入前自动暂停目标 host 关联的 dnsmgr dmonitor 任务，并在数据恢复、DNS 切换及所有普通失败返回路径结束后可靠恢复，避免恢复窗口触发错误故障切换。

## Scope

- 在 host 现有 `dnsmgr` 配置中增加有序 `task_ids`，支持只维护、只切 DNS 或两者组合，保持 schema v2 和旧 P5-2 清单兼容。
- 把非秘密维护计划加入 `restore.Plan`、JSON、dry-run、inspect 和 preview digest；同机与跨机真实原位恢复触发，隔离恢复清除。
- 扩展 `internal/dnsmgr.Client` 调用 P5-1 的单任务 `setactive` API，并实现按序暂停、失败短路、逆序补偿和逆序恢复。
- 在 `restore.Execute` 的全部预检与 safety backup 之后、首次目标写入之前开启维护窗口，并让窗口覆盖数据恢复和 P5-2 DNS 切换。
- 使用脱离命令取消的有限超时 context 执行恢复兜底；合并结构化 maintenance 结果、原始错误和精确人工处理项。
- 更新配置示例、运维说明、设计、路线图、dnsmgr 集成规范及完整测试矩阵。

## Non-Goals

- 不修改 dnsmgr fork 的 P5-1 API，不新增任务查询、批量启停、租约或服务端维护窗口状态。
- 不保留任务任意运行时旧状态；配置的任务约定在非维护期应为 `active=1`。
- 不为 `SIGKILL`、掉电或 hub 主机崩溃实现分布式自动恢复。
- 不改变 P5-2 DNS Value-only API、补偿和 completion marker 语义，也不实现 P5-4 证书部署。

## Key Decisions

- 复用 host 的 `dnsmgr` 段新增 `task_ids`，不再引入第二套连接配置；维护能力与 DNS 能力独立可选。
- P5-1 接口不返回旧状态，因此维护结束固定写回 `active=1`；只有正常状态应启用的任务才能加入清单。
- 同机与跨机真实原位恢复都需要维护窗口；dry-run、inspect、isolate、backup 和 verify 保持零任务启停 HTTP。
- 维护窗口在 `ExecuteOptions.OnPlanReady` 开启：它晚于目标预检、preview digest 校验和 safety backup，早于 marker、停容器及任何目标写入。
- 暂停中途失败时禁止开始数据恢复；先幂等恢复结果未知的当前失败任务，再逆序恢复本轮已暂停任务。正常窗口通过 defer 在 DNS 阶段结束后恢复。
- 任务恢复使用 `context.WithoutCancel` 加固定总超时，单项失败继续处理其余任务；恢复不完整必须让整体命令失败。

## Key Context

- 清单与校验入口：`internal/config/config.go` 的 `Config.DNSMgr`、`Host.DNSMgr`、`HostDNSMgr` 和 `validateHostDNSMgr`。
- 客户端与现有安全边界：`internal/dnsmgr/client.go`，复用 AuthApi 签名、受限凭证文件、HTTP timeout、禁止重定向和响应脱敏。
- 恢复计划：`internal/restore/plan.go`；隔离深拷贝与清除：`internal/restore/isolation.go`。
- 精确联动点：`internal/restore/execute.go` 的 `ExecuteOptions.OnPlanReady` 位于 safety backup 后、首次目标写入前。
- CLI 生命周期：`internal/cli/restore.go`；维护任务与 DNS 共享 client，恢复 defer 必须先于全局锁释放完成。
- dnsmgr 已部署契约：`POST /api/dmonitor/task/setactive`，表单为 `id` 与 `active=0|1`，成功返回 `code=0`。

## Risks / Deferred

- `SIGKILL`、掉电或进程所在主机崩溃无法执行 defer；运维文档和结果必须保留按 task ID 人工设回 `active=1` 的路径。
- 误把原本应长期关闭的任务加入 `task_ids` 会在维护结束后启用它；通过配置说明、上线核对和测试部署验证控制该风险。
- 服务端租约、旧状态查询和崩溃后自动过期恢复明确延后，不在本阶段扩大 dnsmgr 接口范围。

## Acceptance

- 旧清单兼容，`task_ids` 的组合、正整数、重复项和顶层依赖均有字段级校验。
- 维护计划进入 JSON 与 digest；同机/跨机触发，隔离与所有只读/非恢复命令零 HTTP。
- 所有前置检查和 safety backup 先于暂停；暂停全部成功后才允许首次写入。
- 暂停失败会短路后续任务，补偿结果未知的当前任务并逆序恢复此前任务，恢复执行次数为 0。
- restore 成功/失败、DNS 成功/失败和 context 取消均逆序恢复所有已暂停任务；单项失败不阻断其余恢复。
- 恢复不完整返回非零，保留原始错误并列出人工任务；没有 `task_ids` 时继续显示人工暂停项。
- client 安全矩阵、CLI 时序、P5-2 回归、全量测试、示例校验和运维文档全部通过。

## Next Step

- 本地 Full Check-All 重检已通过；下一步在测试 dnsmgr 任务上完成暂停/恢复与部署前验收。
