# P5-2 恢复后自动切换 dnsmgr DNS

## Goal

让 ark 在跨机原位恢复完成后，把目标主机关联的 A 或 AAAA 记录自动切换到清单显式声明的新 IP；整个过程必须可预览、可重试、失败可补偿，并且不把 dnsmgr 凭证或可变 DNS 元数据写入清单和输出。

## Background

- 当前恢复流程在全部步骤完成后写入 completion marker，再返回 `ok` 或 `warn`；`warn` 只表示运行中的 Compose 服务缺少 healthcheck，已有 healthcheck、容器状态和镜像 digest 均已通过验证。
- `restore --dry-run` 只构建计划，`restore --dry-run --inspect` 只读检查目标状态；两者都不得产生外部写入。
- `ssh.address` 允许使用主机名，不能作为新 DNS IP 的权威来源。
- dnsmgr 现有 `POST /api/record/update/:id` 要求调用方提交 Name、Type、Value、Line、TTL、MX、Weight 等完整字段，不适合作为只切换 IP 的稳定集成契约。
- dnsmgr 的 DNS provider 接口声明了 `getDomainRecordInfo` 和 `updateDomainRecord`，但 qingcloud、dnsmgr、powerdns 的详情方法当前直接返回 `false`；服务端需要用标准化记录列表作为兼容回退。
- P5-3 尚未实现，恢复前暂停、结束后恢复 dmonitor 任务仍保留为人工操作。

## Requirements

### R1. 清单模型与兼容性

- 顶层可选 `dnsmgr` 配置保存 API `base_url` 与凭证 `env_file` 绝对路径。
- 目标 host 可选 `dnsmgr` 配置保存一个显式 IP `value`，以及一条或多条 `{domain_id, record_id}` 记录关联。
- `value` 必须是合法 IPv4 或 IPv6；同一 host 内不得重复声明相同的 `{domain_id, record_id}`。
- host 声明记录关联时，顶层 `dnsmgr` 必须存在；未配置 dnsmgr 的现有 schema v2 清单保持完全兼容。
- 清单不保存 UID、API key，也不保存记录的 Name、Type、Line、TTL、MX、Weight 或 Remark。

### R2. 恢复计划与触发边界

- 仅真实的跨机原位恢复生成并执行 DNS 动作；同机恢复和 `--isolate` 不切换 DNS。
- DNS 动作作为非秘密计划数据进入 `restore.Plan`，使普通 dry-run、inspect、JSON 输出和 preview digest 都能展示并绑定目标 IP 与记录标识。
- dry-run 和 inspect 不读取 dnsmgr 凭证、不创建 HTTP client、不调用 dnsmgr。
- DNS 切换只能发生在恢复 completion marker 成功写入之后；恢复步骤、健康检查或 marker 写入失败时不得调用 dnsmgr。
- 恢复结果为 `ok` 或无执行错误的 `warn` 时均可切换 DNS；DNS 成功后保留原 `ok`/`warn` 状态。
- 自动 DNS 计划成功后移除“确认 DNS 指向目标主机”人工项；P5-3 完成前继续保留 dmonitor 暂停/恢复相关人工项。

### R3. dnsmgr AuthApi 契约

- dnsmgr fork 新增 `POST /api/auth/check`，仅依赖 `AuthApi` 完成无副作用的凭证检查。
- dnsmgr fork 新增 `POST /api/record/value/:id`；路径 `id` 为域名 ID，表单参数包含 `recordid`、`value`，补偿调用可额外提供 `expected_value`。
- Value-only 接口必须校验域名存在、调用者权限、记录存在、记录类型仅为 A/AAAA，且新 IP 与记录类型匹配。
- 更新前优先通过 provider 的 `getDomainRecordInfo` 读取权威记录；详情能力不可用时分页扫描 `getDomainRecords` 的标准化列表定位记录。对于 qingcloud 这类返回带 `Count` 分组行的 provider，再通过 `getSubDomainRecords` 展开实际记录。调用 `updateDomainRecord` 时保留 Name、Type、Line、TTL、MX、Weight、Remark，仅替换 Value。
- 当前 Value 已等于目标值时优先返回幂等成功且 `changed=false`，不调用 provider 更新；即使补偿请求携带的 `expected_value` 已过期，也视为目标状态已经达成。
- 需要实际更新且提供了 `expected_value` 时，当前 Value 不匹配则拒绝更新，避免补偿覆盖期间发生的其他 DNS 切换。
- 成功响应返回非秘密记录标识、`previous_value`、当前 `value` 与 `changed`；失败响应不得暴露 provider 凭证或堆栈。

### R4. ark 客户端与凭证安全

- 凭证文件只允许受限 `KEY=VALUE` 语法，必需键为 `ARK_DNSMGR_UID` 和 `ARK_DNSMGR_API_KEY`，未知键拒绝加载。
- 凭证文件必须是绝对路径、非符号链接、普通文件、当前进程用户所有，权限不得宽于 `0600`。
- AuthApi 请求按现有契约生成 `md5(uid + timestamp + apiKey)` 签名，认证字段通过表单 POST 发送。
- `base_url` 默认只允许 HTTPS；HTTP 只允许 loopback。客户端必须设置有限超时，并拒绝把认证表单重定向到其他请求。
- 客户端同时校验 HTTP 状态、JSON 格式和业务 `code`；错误与结果不得包含 UID、API key、签名或完整认证请求。

### R5. 多记录补偿与重试

- 多条记录按清单顺序切换，只把 `changed=true` 的成功记录加入补偿栈。
- 任一记录失败后停止后续切换，并按逆序把补偿栈中的记录恢复为各自 `previous_value`；补偿请求以本轮目标 IP 作为 `expected_value`。
- DNS 更新或补偿任一失败时，恢复命令返回非零，整体状态为 `fail`。
- 结果必须区分“切换失败且已全部回滚”和“回滚不完整”；回滚不完整时列出需要人工处理的 `{domain_id, record_id}`，但不泄漏认证信息。
- DNS 失败不删除已经写入的恢复 completion marker；同一 restore 重跑时允许跳过已完成恢复步骤并重新尝试 DNS。

### R6. doctor、输出与运维边界

- `ark doctor` 在配置 dnsmgr 时检查凭证文件和 `/api/auth/check`，并以独立检查项输出。
- 备份与 verify 不得因为 dnsmgr 不可达而失败；真实跨机恢复只在存在 DNS 计划时执行 dnsmgr 专用前置检查。
- `--skip-doctor` 可以跳过前置探测，但不能绕过真实请求时的凭证文件、URL、响应和超时校验。
- 人类输出和 JSON 结果包含每条记录的切换/幂等/回滚状态，以及需要人工处理的记录标识。

## Non-Goals

- 不在本任务自动暂停或恢复 dmonitor 任务；该能力属于 P5-3。
- 不等待 DNS TTL 生效，不轮询公网解析结果，不提供流量调度能力。
- 不修改记录类型，不支持一次 host 配置同时声明不同的 IPv4 与 IPv6 目标值。
- 不触发证书部署；该能力属于 P5-4。
- 不改变 backup manifest；DNS 关联属于当前目标主机部署配置，不是历史备份事实。

## Acceptance Criteria

- [ ] AC1：未配置 dnsmgr 的 schema v2 清单、备份、verify、同机恢复和跨机恢复保持原行为。
- [ ] AC2：静态校验覆盖顶层配置、目标 IP、记录标识、重复关联、URL 安全和跨字段依赖，并使用 `hosts[i](name).dnsmgr...` 错误前缀。
- [ ] AC3：dry-run 与 inspect 能展示 DNS 计划并进入 preview digest，测试证明不会读取凭证或调用 HTTP。
- [ ] AC4：同机、隔离、恢复失败、健康检查失败和 completion marker 失败均不会执行 DNS 更新。
- [ ] AC5：真实跨机恢复 `ok` 或符合现有语义的 `warn` 后执行 DNS 更新，成功时保留原状态并移除 DNS 人工确认项。
- [ ] AC6：dnsmgr Value-only 接口对 A/AAAA 更新保持所有原元数据，支持幂等、`expected_value` 冲突保护和无副作用认证检查。
- [ ] AC7：多记录中途失败会停止后续更新并逆序补偿；完整回滚和回滚不完整均返回非零，后者能定位需人工处理的记录。
- [ ] AC8：凭证文件、签名、HTTP 状态、JSON 业务码、超时和重定向边界均有测试，所有错误与输出通过秘密泄漏断言。
- [ ] AC9：`ark doctor` 可检查 dnsmgr；backup/verify 不依赖其可用性，restore 只在需要切 DNS 时把专用检查作为阻断项。
- [ ] AC10：ark 与 dnsmgr 相关验证通过，并更新清单示例、设计/路线图说明及部署回滚步骤。
