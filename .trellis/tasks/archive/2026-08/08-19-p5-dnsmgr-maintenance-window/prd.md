# P5-3 维护窗口联动

## Goal

让 ark 在真实原位恢复首次写入目标机前，自动暂停目标 host 显式关联的 dnsmgr dmonitor 任务，并在数据恢复、DNS 切换及所有失败返回路径结束后可靠恢复这些任务，避免恢复期间的预期不可用被误判为故障并触发 DNS 切换。

## Background

- P5-1 已在 dnsmgr fork 提供受 `AuthApi` 保护的 `POST /api/dmonitor/task/setactive`，请求只接受单个 `id` 与 `active=0|1`，重复设置保持幂等。
- P5-2 已在 ark 建立顶层 dnsmgr 连接配置、安全凭证加载、doctor、客户端和恢复后 DNS 切换能力。
- 当前恢复计划仍要求管理员人工暂停并恢复 dmonitor；遗漏恢复会让生产检测长期失效。
- dnsmgr 现有接口不返回任务原状态。P5-3 按路线图既定边界只实现 ark 侧联动：清单中声明的 `task_ids` 代表正常状态应启用、维护结束后应恢复为 `active=1` 的任务。

## Requirements

### R1. 清单模型与兼容性

- 在 host 现有可选 `dnsmgr` 段增加有序 `task_ids`，每项是大于 0 的 dmonitor 任务 ID。
- `task_ids` 不得重复；声明后顶层 `dnsmgr` 必须存在。
- host 的 dnsmgr 段允许只配置 DNS 切换、只配置维护任务，或同时配置两者；整个段不能没有任何有效能力。
- 现有只含 `value` 与 `records` 的 schema v2 清单继续有效，行为保持不变。
- `task_ids` 是当前目标 host 的部署意图，不写入 backup manifest；它进入恢复计划、JSON、dry-run、inspect 与 preview digest。

### R2. 触发边界与时序

- 同机或跨机的真实原位恢复，只要目标 host 配置了 `task_ids`，都必须进入维护窗口。
- `--isolate`、dry-run、inspect、backup、verify 不加载凭证、不创建 dnsmgr client、不调用任务启停 API。
- 维护窗口必须在全部 doctor、目标预检、preview digest 校验和可选 safety backup 成功后开启，并且早于恢复状态 marker、停容器或任何目标写入。
- 所有任务暂停成功后才能继续真实恢复；任一暂停失败时禁止首次目标写入，并立即尝试恢复本轮已暂停任务及当前结果未知的失败任务。
- 维护窗口覆盖数据恢复和 P5-2 的 DNS 切换；只有二者都结束后才恢复 dmonitor 任务。

### R3. dnsmgr 客户端契约

- 复用 P5-2 的 `Client`、AuthApi 签名、凭证文件、URL、超时、重定向和脱敏约束。
- 客户端新增按任务 ID 调用 `POST /api/dmonitor/task/setactive` 的窄方法，只提交 `id` 与 `active=0|1`。
- 客户端同时校验参数、HTTP 状态、JSON 格式和明确存在的业务 `code=0`；`null`、缺少 `code` 或类型错误均不得视为成功。错误不得包含 UID、API key、签名、完整请求或响应正文。
- `ark doctor` 继续通过 `/api/auth/check` 验证共享凭证，不为每个 task ID 发有副作用的探测请求。

### R4. 暂停、恢复与兜底

- 按清单顺序暂停任务；暂停失败后停止后续暂停。由于传输失败不能证明服务端未写入，补偿必须先把当前失败任务幂等恢复为 `active=1`，再按逆序恢复本轮已成功暂停的任务。
- 全部暂停成功后注册恢复兜底；根命令必须把 `SIGINT`/`SIGTERM` 转为命令 context 取消，成功、恢复失败、DNS 失败、命令取消和普通信号中断均必须进入恢复逻辑。
- 恢复逻辑使用脱离已取消命令 context 的独立有限超时 context，按逆序尝试所有已暂停任务；单个恢复失败不得阻止其余任务恢复。
- 维护结束时把已暂停任务恢复为 `active=1`。清单只能关联正常状态应启用的任务；保留任意运行时旧状态不属于本阶段范围。
- 进程被 `SIGKILL`、宿主机掉电或内核直接终止时无法执行进程内兜底，结果与运维文档必须保留按 task ID 人工恢复路径。

### R5. 结果、错误与人工处理

- 结构化恢复结果增加非秘密 maintenance 摘要，至少包含每个 task ID 的暂停状态、恢复状态和需要人工处理的任务。
- 暂停失败时命令返回非零，并明确数据恢复尚未开始；若回滚暂停不完整，列出需要人工恢复的 task ID。
- 数据恢复或 DNS 切换失败后仍恢复 dmonitor，并保留原始失败原因；恢复任务失败通过 `errors.Join` 合并，不能覆盖原错误。
- 数据恢复和 DNS 均成功但任务恢复不完整时，整体状态改为 `fail`，命令返回非零，并追加精确人工检查项。
- 配置了有效维护计划时移除原有“确认已暂停 dnsmgr 对目标主机的检测，并在恢复结束后重新启用”人工项；未配置 `task_ids` 的旧清单继续保留该人工项。

### R6. 文档与运维边界

- 示例清单说明 `task_ids` 的来源、正常启用假设及与 `value/records` 的独立关系。
- 运维文档说明上线前验证、维护恢复失败处理、强制终止后的人工恢复和回滚步骤。
- 路线图和设计文档在实现完成后更新 P5-3 状态与最终时序。

## Non-Goals

- 不修改 dnsmgr fork 的 P5-1 API，不新增任务查询、批量启停、租约或维护窗口服务端状态。
- 不保留或恢复任务任意运行时旧状态；配置的任务约定在非维护期应为 `active=1`。
- 不为 `SIGKILL`、掉电或 hub 主机崩溃提供分布式租约与自动过期恢复。
- 不改变 P5-2 DNS Value-only API、补偿语义或 completion marker 行为。
- 不触发证书部署；该能力属于 P5-4。

## Acceptance Criteria

- [ ] AC1：旧 schema v2 清单继续通过；`task_ids` 的正整数、重复项、顶层依赖和空 dnsmgr 段得到字段级聚合校验。
- [ ] AC2：同机与跨机真实原位恢复生成维护计划；隔离恢复清除维护计划，dry-run、inspect、backup、verify 均保持零凭证读取和零任务启停 HTTP。
- [ ] AC3：维护计划进入 JSON 和 preview digest；修改 task ID 顺序或内容会导致 expected preview digest 不匹配。
- [ ] AC4：全部 doctor、目标预检和 safety backup 在暂停前完成；所有任务暂停成功后才允许恢复 marker、停容器或目标写入。
- [ ] AC5：任务按声明顺序暂停；中途失败停止前向暂停，先补偿结果未知的当前任务，再逆序恢复已暂停任务，数据恢复执行次数为 0。
- [ ] AC6：恢复成功、恢复执行失败、DNS 失败和 context 取消路径都使用独立有限超时 context 逆序恢复全部已暂停任务。
- [ ] AC7：单个恢复任务失败不阻断其余恢复；整体返回非零，保留原始错误并列出需人工恢复的 task ID。
- [ ] AC8：客户端请求路径、表单、幂等成功、HTTP/JSON/业务失败、缺失业务码、超时、3xx 和秘密泄漏断言均有测试。
- [ ] AC9：配置维护计划后移除人工暂停项；未配置 `task_ids` 的旧清单仍保留人工项，P5-2 DNS 行为不回归。
- [ ] AC10：`make check`、示例清单校验和相关定向测试通过，文档包含上线验证与回滚操作。
