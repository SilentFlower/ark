# 执行计划

## 1. 清单、秘密配置与监控 HTTP 边界

- 为 v2 清单增加可选 `monitoring.env_file` 结构与纯静态校验。
- 新增 `internal/monitoring`，复用 `internal/envfile` 严格解析四个允许键，验证键组合与安全 URL。
- 实现通用 heartbeat 双端点和钉钉 Markdown sender，加入固定超时、响应上限、有限重试和脱敏错误。
- 在 local doctor 中检查文件类型、owner、`0600` 权限与内容；配置问题记 warn，不中断备份。
- 更新 config、monitoring、doctor 单元测试和 `examples/ark.yaml` 注释示例。

重点文件：

- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/envfile/envfile.go`
- `internal/monitoring/monitoring.go`
- `internal/monitoring/dingtalk.go`
- `internal/monitoring/heartbeat.go`
- `internal/doctor/doctor.go`
- `internal/doctor/doctor_test.go`
- `examples/ark.yaml`

验证：

```bash
go test ./internal/config ./internal/envfile ./internal/monitoring ./internal/doctor -race -count=10
```

## 2. 状态库 schema v3 与告警状态 API

- 新增 `schema_v3.sql` 和第三段顺序迁移，历史 schema 文件保持不变。
- 增加 `AlertState`、严格字段校验、全量查询和单条 upsert。
- 覆盖 v2→v3、空库、并发迁移、失败回滚、高版本拒绝、NULL 时间还原和 context 取消。

重点文件：

- `internal/store/schema_v3.sql`
- `internal/store/store.go`
- `internal/store/alert.go`
- `internal/store/store_test.go`
- `internal/store/query_test.go`

验证：

```bash
go test ./internal/store -race -count=10
CGO_ENABLED=0 go test ./internal/store -count=1
```

回滚点：migration 与 Store round-trip 未通过前不接入 Hub；迁移失败必须保留
`user_version=2` 和全部原表数据。

## 3. Hub 告警 manager 与生命周期

- 增加 `alertStore` 最小接口和 `alertManager`，串行执行首次、持续、恢复、复发与 host 删除对账。
- 每分钟重新加载清单并复用 `projectHosts`，同轮到期事件批量发送一条 Markdown。
- 成功发送后更新静默时间；失败下一周期重试；重启从 Store 恢复状态。
- 接入 `serve.Run` 的启动、取消、等待和错误聚合，保持 HTTP server、operations、Store 关闭顺序。
- 使用可注入 clock、interval、sender、reporter，覆盖 race 和 context 取消。

重点文件：

- `internal/hub/alert.go`
- `internal/hub/health.go`
- `internal/hub/http.go`
- `internal/hub/serve.go`
- `internal/hub/serve_test.go`
- `internal/hub/api_test.go`

验证：

```bash
go test ./internal/hub -run 'TestAlert|TestRun|TestAPI' -race -count=10
```

必须覆盖：首次立即、24 小时前后边界、恢复一次、复发立即、发送失败不静默、Hub 重启、
host 删除不发恢复、schedule failure 不伪造 overdue、并发评估不重复。

## 4. Backup 心跳与 CLI JSON 契约

- 在 backup 最终摘要形成后加载 heartbeat 设置并发送成功/失败端点。
- 增加 `heartbeat_status=disabled|sent|failed`，同步人类输出和 JSON 输出。
- 心跳失败不进入 `backupFailureSet`，不改变 run、manifest、`runErr` 或退出码。
- context 取消使用有界 `WithoutCancel` 收尾；dry-run 和清单加载失败零请求。
- 同步 Hub `backupOperationResult`、未知字段白名单和状态校验，保证手工 backup 仍能持久化结果。

重点文件：

- `internal/cli/backup.go`
- `internal/cli/backup_test.go`
- `internal/cli/root_test.go`
- `internal/hub/operation.go`
- `internal/hub/api_test.go`

验证：

```bash
go test ./internal/cli ./internal/hub -run 'TestBackup|TestOperation' -race -count=10
```

必须覆盖：ok/warn 走成功端点、fail/取消/锁失败走失败端点、请求失败时退出码保持、
disabled/sent/failed JSON、dry-run 零网络和 URL/secret 不出现在任何错误中。

## 5. 文档与规范同步

- 更新 `docs/design.md` 的状态库、Hub 告警与死人开关设计。
- 更新 `docs/roadmap.md`，完成 P4-4 并记录真实验收结果；同步 README 当前状态。
- 更新 `docs/operations.md`，写明监控 env 文件、权限、Healthchecks.io 双 URL 配法、静默语义、
  Hub 重启和 at-least-once 残余重复风险。
- 更新 backend spec：manifest、database、hub、backup orchestration、logging、directory structure。
- 不把真实 webhook、secret、心跳 URL、响应内容或生产清单写进仓库、任务记录或 journal。

## 6. 自动化质量门禁

```bash
gofmt -w .
go test ./internal/config ./internal/envfile ./internal/monitoring ./internal/doctor ./internal/store ./internal/cli ./internal/hub -race -count=10
make check
make build
make hub
CGO_ENABLED=0 go test ./internal/store ./internal/monitoring ./internal/hub -count=1
CGO_ENABLED=0 go build -trimpath -o bin/ark-nocgo ./cmd/ark
CGO_ENABLED=0 go build -trimpath -o bin/ark-hub-nocgo ./cmd/ark-hub
go mod verify
git diff --check
```

- 对 config→monitoring→CLI/Hub→Store→API JSON 做跨层正向与反向覆盖扫描。
- 对全部 HTTP 错误链做 secret fixture 扫描，确认 token、query 和签名不泄漏。
- 检查公共类型和方法均有中文 Javadoc，复杂状态机注释解释静默与 at-least-once 原因。

## 7. 真实环境验收

- 使用仓库外的 root-only 监控文件连接现有钉钉机器人和外部心跳服务。
- 人为制造同一 host 连续两次备份失败：页面与钉钉在一分钟内同时出现告警。
- 保持故障并重复评估，确认 24 小时内不刷群；通过可控 clock 的自动化测试覆盖完整 24 小时重发，
  真机不等待 24 小时。
- 修复故障后确认只发一次恢复通知，随后复发立即通知。
- 分别验证 ok/warn 成功端点、fail 失败端点和心跳网络故障不改变 backup 退出码。
- 停止 `ark-hub` 后手动启动 backup service，确认备份与心跳仍执行；停止 backup timer 后由外部服务
  按配置宽限期报告失联。

上线前保留 ark.db 在线导出副本和旧二进制；schema v3 回滚必须恢复数据库副本，不能让旧二进制
直接打开更高版本数据库。
