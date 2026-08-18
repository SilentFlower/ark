# Brief — P4-4 告警与死人开关

## Goal

- 让 Ark 在控制台之外主动暴露备份超时、连续失败、演练失败和整体停止运行风险：Hub 向钉钉发送具备静默期的告警，`ark backup` 向外部监控发送独立心跳。

## Scope

- 为 v2 清单增加可选 `monitoring.env_file`，用 root-only 文件保存钉钉 webhook、可选签名密钥及心跳成功/失败 URL。
- 新增 `internal/monitoring`，实现严格秘密解析、安全 URL 校验、钉钉 Markdown 投递和供应商无关的双端点心跳。
- 将状态库升级到 schema v3，持久化告警首次发现、最近投递、活动/恢复状态，使静默期跨 Hub 重启生效。
- 在 `ark-hub` 中增加每分钟串行告警评估，复用现有健康投影并管理首次通知、24 小时重发、恢复通知和复发。
- 在 `ark backup` 终态形成后发送心跳，并向人类摘要和 JSON 增加 `heartbeat_status=disabled|sent|failed`。
- 同步 doctor、Hub operation JSON 白名单、示例清单、README、设计、运维文档、路线图和后端规范。

## Non-Goals

- 不把 backup/verify 调度迁入 `ark-hub`，不改变 systemd timer 所有权。
- 不增加告警历史、确认、手工静默或新的前端管理页面。
- 不支持短信、电话、邮件、DING 等其它通知渠道。
- 不实现 Healthchecks.io 专用 `/start`、run ID、运行时长协议或由 Ark 判断外部监控是否报警。
- 不包含 P5 dnsmgr 联动、DNS/证书切换或自动故障转移。

## Key Decisions

- 告警生命周期固定为：首次立即发送，持续故障每 24 小时最多重发一次，恢复发送一次通知，恢复后复发立即发送。
- 只有成功投递才刷新静默期；发送失败按受限策略重试，不能因失败进入 24 小时静默。
- 死人开关采用供应商无关的成功/失败双端点：`ok/warn` 走成功端点，`fail` 走失败端点；两个 URL 可以相同。
- 心跳网络失败只记为 `heartbeat_status=failed`，不改变备份 run、manifest 或既有退出码，避免把成功备份误判为失败。
- 钉钉与心跳秘密不进入 YAML；统一放在 `monitoring.env_file`，未知键、非法组合和不安全 URL 明确拒绝。
- Hub 告警严格复用 `projectHosts` 的三个稳定告警 kind，不在通知层复制健康判定。
- 单轮到期告警和恢复事件合并为一条 Markdown；状态机采用 at-least-once 取舍，优先避免永久漏报。

## Key Context

- 当前告警投影在 `internal/hub/health.go`，稳定 ID 为 `<host>:<kind>`，kind 为 `backup_overdue`、`backup_consecutive_failures`、`verification_failed`。
- 状态库基线为 schema v2；本任务已新增独立 `schema_v3.sql` 并升级到 v3，迁移继续使用同连接 `BEGIN IMMEDIATE`，历史迁移未改写。
- `internal/envfile` 已提供不会回显变量值的受限 env 解析，应直接复用。
- Hub 生命周期由 `internal/hub/serve.go` 管理；告警 manager 必须随 listener/HTTP/operations/Store 有界启动和停止。
- Backup 心跳接入 `newBackupCmdWithDependencies` 的最终摘要边界；Hub 对 Ark JSON 严格拒绝未知字段，必须同步 `heartbeat_status` 白名单。
- `config.Validate` 继续保持纯静态，文件类型、owner 和 `0600` 权限由 doctor/运行时检查。

## Risks / Deferred

- 钉钉发送成功后、状态提交前若 Hub 崩溃，重启后可能重复一条消息；这是 at-least-once 的已知残余风险，避免为单实例 Hub 引入分布式租约。
- schema v3 升级后旧二进制会拒绝打开数据库；上线前必须保留 ark.db 一致性副本，回滚时同时恢复数据库副本。
- 真机验收需要仓库外的现有钉钉机器人和外部心跳凭证，任何真实秘密不得进入仓库、任务记录或 journal。
- 钉钉可选加签的具体请求字段必须在实现时按当前官方 Webhook 安全文档再次核对，并用固定向量测试，不能凭历史记忆实现。

## Acceptance

- API 与钉钉使用完全一致的三类告警；schedule 无法解析时不伪造 overdue。
- 首次通知、24 小时静默/重发、恢复一次、复发立即和 Hub 重启恢复状态均有 Store/Hub race 测试。
- 钉钉超时、重试、非 2xx、超大响应、业务错误和全部秘密不泄漏路径均有测试。
- Backup 的 ok/warn 调成功端点，fail/取消/前置失败调失败端点；dry-run 和未配置场景零网络请求。
- 心跳失败时原 ok/warn 仍退出 0、原 fail 仍退出 1，JSON 明确返回 `heartbeat_status=failed`。
- v2→v3、空库、并发迁移、失败回滚、高版本拒绝和 `CGO_ENABLED=0` 均验证通过。
- `make check`、相关 race 测试、构建、`make hub`、`go mod verify`、`git diff --check` 全部通过。
- 真机连续两次备份失败能在一分钟内同时出现在页面和钉钉；停止 Hub 后备份与心跳仍执行，停止 backup timer 后外部监控按宽限期报告失联。

## Next Step

- Check-All 修复重检已通过；下一步进入规范同步，再由提交阶段生成精确提交计划。
