# Release Operations

## Conclusion

Release operations exist.

## Evidence Checked

- `task.json`、`prd.md`、`design.md`、`implement.md`
- `implement.jsonl`、`check.jsonl`
- 业务提交 `dc68c7e feat: 实现 P4-4 告警与死人开关`
- `docs/operations.md`、`examples/ark.yaml`
- `internal/store/schema_v3.sql`、`internal/store/store.go`
- `internal/config/config.go`、`internal/monitoring/monitoring.go`

## Drift Check

Missing release.md. 已根据任务材料、实现提交与运维文档补齐上线操作。

## SQL Changes

- `[08-18-p4-alerting-deadman]` 状态库会在新二进制首次打开时自动从 schema v2 迁移到 v3，新增 `alert_states` 表和索引；无需手工执行 SQL。
- `[08-18-p4-alerting-deadman]` 上线前必须保留 `/var/lib/ark/ark.db` 的一致性在线导出副本，供回滚时恢复。

## Configuration Changes

- `[08-18-p4-alerting-deadman]` 如启用主动告警或外部心跳，在清单增加 `monitoring.env_file` 的绝对路径。
- `[08-18-p4-alerting-deadman]` 创建由 Ark 运行用户所有、权限不超过 `0600`、非符号链接的监控秘密文件；只允许 `ARK_DINGTALK_WEBHOOK_URL`、`ARK_DINGTALK_SECRET`、`ARK_HEARTBEAT_SUCCESS_URL`、`ARK_HEARTBEAT_FAILURE_URL`。
- `[08-18-p4-alerting-deadman]` 心跳成功与失败 URL 必须同时配置；生产端点必须使用 HTTPS。真实 URL、token 和签名密钥不得进入仓库、日志、工单或命令行历史。
- `[08-18-p4-alerting-deadman]` 配置变更后执行 `ark validate` 和 `ark doctor --all`，确认清单、文件所有者、权限和端点格式均通过校验。

## Batch / Deployment Scripts / Data Repair

None. 不需要一次性数据修复脚本；schema v3 由应用启动时顺序迁移。

## External Systems / Dependent Platforms

- `[08-18-p4-alerting-deadman]` 在钉钉侧准备现有群机器人 Webhook；启用加签时同步配置签名密钥。
- `[08-18-p4-alerting-deadman]` 在外部心跳服务配置成功、失败端点及失联宽限期；Ark 不负责判断外部服务是否已经报警。

## Release Order

1. 导出状态库一致性副本，并保留当前旧二进制。
2. 部署包含提交 `dc68c7e` 的新二进制。
3. 在仓库外创建监控秘密文件并更新清单，再运行 `ark validate` 和 `ark doctor --all`。
4. 重启 `ark-hub`，确认监听成功且状态库已迁移到 schema v3。
5. 触发一次备份并完成下述上线后验证；测试结束后恢复所有 timer 和故障注入。

## Rollback Notes

- 回滚新二进制前先停止 `ark-hub` 和相关 Ark 进程。
- schema v3 数据库不能由只识别旧 schema 的二进制直接打开；必须同时恢复上线前的状态库一致性副本，再启用旧二进制。
- 监控配置可从清单移除以关闭新网络请求，但这不能替代数据库回滚步骤。

## Post-release Verification

- 人为制造同一 host 连续两次备份失败，确认页面与钉钉在一个评估周期内出现相同 kind，且静默期内不重复发送。
- 修复故障后确认只发送一次恢复通知，随后复发会立即通知。
- 分别验证 `ok`/`warn` 使用成功端点、`fail` 使用失败端点，并确认心跳网络失败不改变备份 run、manifest 或退出码。
- 停止 `ark-hub` 后确认 backup timer 与外部心跳仍工作；恢复 Hub 后停止 backup timer，确认外部监控按配置宽限期报告失联。
