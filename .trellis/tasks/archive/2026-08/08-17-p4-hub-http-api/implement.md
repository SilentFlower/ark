# 执行计划

## 1. 状态库 schema v2 与查询 API

- 新增 `internal/store/schema_v2.sql`，注册第二段顺序迁移并更新 schema 版本测试。
- 增加 `OperationKind`、`OperationStatus`、`ManualOperation`、终态写入参数和列表 options。
- 实现 operation 创建、完成、启动恢复中断、单条查询和 keyset 分页列表。
- 实现 runs 分页、host run + targets 聚合、latest doctor、verifications 列表。
- 所有查询复用 `withBusyRetry`，校验 limit、cursor facts、status、时间和 JSON。

重点文件：

- `internal/store/schema.sql`
- `internal/store/schema_v2.sql`
- `internal/store/store.go`
- `internal/store/store_test.go`

验证：

```bash
go test ./internal/store -race -count=10
CGO_ENABLED=0 go test ./internal/store -count=1
```

回滚点：v2 migration 未完整通过前不修改 Hub 打开逻辑；迁移失败必须保留 v1 数据库和
`user_version=1`。

## 2. Schedule 探测与 doctor 持久化

- 新建 `internal/schedule`，实现可注入命令执行器的两次 UTC elapse 解析。
- 将 doctor 的 OnCalendar 校验切换到 schedule 边界，保持原有三态报告与中文展示。
- 在 backup 中前移 Store 打开，在 local/host doctor 后持久化报告；host 报告写入 next run。
- 在 verify 中复用同一映射记录实际执行的 local/host doctor。
- 增加 report -> store status/JSON 的 CLI 私有转换，禁止 doctor 反向依赖 store。

重点文件：

- `internal/schedule/schedule.go`
- `internal/schedule/schedule_test.go`
- `internal/doctor/doctor.go`
- `internal/doctor/doctor_test.go`
- `internal/cli/backup.go`
- `internal/cli/backup_test.go`
- `internal/cli/verify.go`
- `internal/cli/verify_test.go`

验证：

```bash
go test ./internal/schedule ./internal/doctor ./internal/cli -race -count=1
```

回滚点：schedule helper 必须先通过 daily/weekly/monthly、输出损坏和 context 取消测试，再替换
doctor 现有直接命令调用。

## 3. 恢复只读预检与摘要强制

- 把 `inspectDestination` 的安全事实投影为导出的 `Conflict` / `Preview`，保持底层命令非导出。
- 实现 `Inspect`、稳定排序和 SHA-256 digest，证明全程只读。
- 为 `ExecuteOptions` 增加 expected digest；首次预检和 force safety backup 后各校验一次。
- 扩展 restore CLI：`--inspect`、`--expected-preview-sha256`，并增加 Preview JSON 输出。
- 保持普通 dry-run、普通真实恢复、isolate、force 和 cleanup 现有行为兼容。

重点文件：

- `internal/restore/execute.go`
- `internal/restore/execute_test.go`
- `internal/restore/plan.go`
- `internal/cli/restore.go`
- `internal/cli/restore_test.go`

验证：

```bash
go test ./internal/restore ./internal/cli -race -count=1
```

必须覆盖：

- 预检不调用写 marker、创建目录、拉镜像、stop/up、safety backup；
- 冲突顺序变化不改变 digest，冲突内容变化必须改变 digest；
- expected digest 不匹配时零 safety backup、零目标写入；
- safety backup 后冲突变化时零恢复写入；
- 不带新 flag 的原有 CLI 测试全部保持通过。

回滚点：Execute 重构期间原有 force safety backup 顺序和 isolation resume 语义不得漂移；若预检
无法独立证明只读，先回退为内部 refactor，不开放 HTTP 确认。

## 4. Hub 启动参数与生命周期

- 扩展 `ServeOptions`、serve flags、install flags 和 `systemd.HubInstallOptions`。
- 启动监听前校验 auth、config、Ark binary、Store，并恢复遗留 operations。
- 把 `stateStore` 扩为 Hub 所需的最小查询/operation 接口，测试继续使用 fake。
- 为 application 增加可关闭的 operation manager；Run shutdown 顺序固定为 HTTP -> operation -> Store。
- 更新 systemd unit 的 ExecStart 和真实 `systemd-analyze verify` 测试，不触碰 timer。

重点文件：

- `internal/hub/serve.go`
- `internal/hub/root.go`
- `internal/hub/install.go`
- `internal/hub/serve_test.go`
- `internal/systemd/unit.go`
- `internal/systemd/unit_test.go`

验证：

```bash
go test ./internal/hub ./internal/systemd -race -count=1
go test -v ./internal/systemd -run 'SystemdAnalyzeVerify' -count=1
```

## 5. 查询 projection 与 GET API

- 新增 API DTO、严格 cursor 编解码和统一新业务错误响应。
- 实现 config/store 合并的 host projection；过滤敏感配置字段。
- 实现唯一 health/alerts 派生函数和 schedule 表达式去重探测。
- 注册 hosts、host detail、runs、alerts、operations GET 路由。
- 扩展 `/api/session` 返回 session CSRF token，保持已有字段和未认证响应兼容。

建议文件拆分：

- `internal/hub/api.go`：路由、JSON decode/encode、错误 envelope
- `internal/hub/query.go`：Store 查询与 API DTO 投影
- `internal/hub/health.go`：host health / alerts 单一派生边界
- `internal/hub/cursor.go`：版本化 cursor

验证：

```bash
go test ./internal/hub -run 'TestAPI|TestHealth|TestCursor' -race -count=1
```

必须覆盖空库、NULL、分页并发插入、未知 host、非法 filter、schedule failure、无成功备份、
warn 计入成功、两次 fail 和 verification fail。

## 6. Operation manager 与 POST API

- 实现持久化 operation manager、单活动任务互斥、随机 ID、bounded output 和严格 CLI JSON decode。
- 使用 application context 和 `Pdeathsig=SIGTERM` 管理 Ark；request context 只控制请求接收阶段。
- 实现 backup/verify argv 和结果投影。
- 实现 restore preview operation、内存 confirmation registry、session 绑定、10 分钟 TTL、单次消费。
- 实现 confirm -> 精确 snapshot + expected digest 的真实 restore operation。
- 实现 operation list/detail 响应；只有创建 preview 的同一 session 可获得仍有效的 confirmation token。
- graceful shutdown 取消并等待活动命令，终态持久化为 interrupted；异常退出由下次启动恢复。

建议文件拆分：

- `internal/hub/operation.go`：进程与 Store 生命周期
- `internal/hub/operation_result.go`：各命令 JSON 白名单解码
- `internal/hub/confirmation.go`：恢复确认 token 状态机
- `internal/hub/api_action.go`：POST handler

验证：

```bash
go test ./internal/hub -run 'TestOperation|TestActionAPI|TestConfirmation' -race -count=10
```

测试通过 Go test helper process 模拟 Ark，精确断言 argv、stdout/stderr 上限、非零退出、损坏 JSON、
client disconnect、并发请求、shutdown、Pdeathsig 相关可观察行为和 Store 写入顺序。

回滚点：任何无法确定终态的进程路径都必须先修复，不能用“返回 202 后不管”作为临时实现。

## 7. 文档与规范同步

- 更新 `docs/design.md` 的 Hub API、健康判定、操作持久化和恢复确认。
- 更新 `docs/roadmap.md`，标记 P4-1/P4-2 完成并保留 P4-3 后续边界。
- 更新 `README.md` 的 `ark-hub` 状态和命令示例。
- 更新 `docs/operations.md` 的 serve/install 新参数、schema 升级和 Hub 重启后的 interrupted 语义。
- 更新 backend spec：Hub、Database、External Command、Directory Structure；只记录最终可执行契约。

## 8. 最终质量门禁

```bash
gofmt -w .
go test ./internal/store ./internal/schedule ./internal/doctor ./internal/restore ./internal/cli ./internal/hub ./internal/systemd -race -count=10
make check
make build
go build -trimpath -o bin/ark-hub ./cmd/ark-hub
CGO_ENABLED=0 go test ./internal/store ./internal/hub -count=1
CGO_ENABLED=0 go build -trimpath -o bin/ark-nocgo ./cmd/ark
CGO_ENABLED=0 go build -trimpath -o bin/ark-hub-nocgo ./cmd/ark-hub
go mod verify
go test -v ./internal/systemd -run 'SystemdAnalyzeVerify' -count=1
git diff --check
```

- 对 schema、恢复确认、操作状态机和健康判定执行 full Check-All 反向覆盖扫描。
- 验证工作区没有提交 `bin/`、临时数据库、操作输出或真实清单。
- P4-3 前端、P4-4 embed 和 P4-5 告警发送不得出现在本任务 diff。

