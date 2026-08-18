# P4-4 代码与协议证据

## 当前代码边界

- `internal/config/config.go:90` 的 `Config` 当前只有 `version`、`repo`、`defaults`、`hosts`；
  `Validate` 在 `internal/config/config.go:425` 明确保持纯静态校验，因此新增秘密文件只能校验绝对路径，存在性与权限必须留给 doctor 或运行时加载器。
- `internal/envfile/envfile.go:18` 已提供受限 `KEY=VALUE` 解析，错误不包含变量值；监控配置应复用该包，不再实现第二套 env 解析器。
- `internal/hub/health.go:33` 的 `projectHosts` 是 hosts 与 alerts 共用的唯一健康投影；
  `deriveAlerts` 在 `internal/hub/health.go:177` 固定产出三个 `<host>:<kind>` 告警 ID。
- `internal/hub/serve.go:67` 管理 listener、HTTP server、operation manager 与 Store 的启动/停止顺序；
  告警后台循环必须接入这条生命周期，不能成为脱离 context 的孤立 goroutine。
- `internal/store/store.go:30` 当前 schema 版本为 2，迁移在 `internal/store/store.go:522`
  使用同连接 `BEGIN IMMEDIATE`；P4-4 应新增独立 `schema_v3.sql`，不能改写历史迁移。
- `internal/cli/backup.go:120` 在 `RunE` 中形成最终摘要并打印；`runBackup` 在
  `internal/cli/backup.go:235` 返回后 Store、锁和备份事实已经收尾，适合在打印前附加独立心跳结果。
- `internal/hub/operation.go:366` 对 Ark 子进程 JSON 做未知字段白名单校验；backup 摘要新增
  `heartbeat_status` 时必须同步 `backupOperationResult` 与 `operationResultFields`。

## 外部协议证据

- Healthchecks.io 官方 Pinging API 支持基础成功 URL、`/fail` 失败 URL和 `/<exit-status>`；
  本任务只采用供应商无关的成功/失败双端点，不把这些路径规则写进 Ark。
  参考：https://healthchecks.io/docs/http_api/
- Healthchecks.io 官方建议给请求设置明确超时并进行有限重试。
  参考：https://healthchecks.io/docs/reliability_tips/
- 钉钉开放平台文档入口为 Webhook 机器人和自定义机器人安全设置；实现阶段必须以当前官方文档
  核对 Markdown 请求、业务错误与可选加签契约，不从历史记忆猜字段。
  参考：https://pre-open.dingtalk.com/document/dingstart/webhook-robot.md
  参考：https://pre-open.dingtalk.com/document/dingstart/customize-robot-security-settings.md

## 结论

- 新增单一可选 `monitoring.env_file`，同时承载钉钉与心跳秘密，避免在 YAML 中放 token URL。
- 新增 `internal/monitoring` 作为 CLI 与 Hub 共用的出站 HTTP 和秘密加载边界。
- 告警判定仍归 Hub 健康投影；生命周期和静默状态归 Hub + Store，不进入前端或 backup 包。
- 心跳归 `ark backup` 命令终态；监控网络故障不得污染备份 run 的真实结果。
