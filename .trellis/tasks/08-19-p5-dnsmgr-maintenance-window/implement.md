# P5-3 实施计划

## 1. 清单与恢复计划

- [x] 扩展 `config.HostDNSMgr`，新增有序 `TaskIDs []int64`，重构 host dnsmgr 组合校验，使维护能力与 DNS 能力可独立配置。
- [x] 增加旧清单兼容、只维护、只 DNS、组合配置、空能力、非法/重复 task ID 和顶层依赖测试。
- [x] 在 `internal/dnsmgr` 定义非秘密 `MaintenancePlan` 与结果 DTO，并为公开类型/方法添加中文 GoDoc。
- [x] 扩展 `restore.Plan`、`BuildPlan`、人工项生成与 `WithIsolation`，保证维护计划进入 JSON/preview digest，隔离恢复清除维护动作。
- [x] 更新 plan、inspect、isolation 和 digest 测试，覆盖同机与跨机恢复。

定向验证：

```bash
go test ./internal/config ./internal/restore
```

## 2. dnsmgr 任务客户端与编排

- [x] 在现有 `Client` 增加 `SetTaskActive(ctx, taskID, active)`，复用 AuthApi 请求与安全约束。
- [x] 增加 client 测试，覆盖固定路由、`id/active` 表单、参数、HTTP/JSON/业务错误、缺失业务码、超时、3xx、响应上限和秘密泄漏。
- [x] 新增维护编排：按序暂停、失败短路、结果未知的失败项补偿、逆序恢复、单项失败继续和人工任务收集。
- [x] 为恢复兜底使用 `context.WithoutCancel` 加固定总超时，并用已取消 context 的测试证明仍会发恢复请求。

定向验证：

```bash
go test ./internal/dnsmgr
```

## 3. 恢复 CLI 联动

- [x] 扩展 `restoreDependencies` 注入点和依赖校验，让 DNS 与 maintenance 共享同一安全 client，任一计划存在时运行 dnsmgr doctor。
- [x] 把维护窗口开启放入 `ExecuteOptions.OnPlanReady`：先输出计划，再暂停任务；调整 callback 错误文案为通用语义。
- [x] 在首次暂停成功后注册 defer，根命令把 `SIGINT`/`SIGTERM` 转为 context 取消，覆盖 execute 成功/失败、DNS 成功/失败、context 取消和普通返回路径。
- [x] 让维护窗口持续覆盖 P5-2 DNS 切换，结束时在全局锁释放前用独立有限 context 恢复任务。
- [x] 合并 maintenance 结果、整体状态、错误与人工项；暂停失败明确“目标写入未开始”，恢复不完整不覆盖原始 restore/DNS 错误。
- [x] 更新人类输出与 JSON 测试，保证不泄露 dnsmgr 连接或凭证信息。

定向验证：

```bash
go test ./internal/cli ./internal/restore
```

## 4. 命令隔离与回归

- [x] 证明 dry-run、inspect、isolate、backup、verify 不创建维护 client、不调用任务 API。
- [x] 证明 safety backup 或前置 inspect 失败时尚未暂停任务。
- [x] 证明暂停失败时恢复执行次数为 0，结果未知的失败任务和已暂停任务仍全部尝试恢复。
- [x] 证明没有 `task_ids` 的 P5-2 清单继续保留人工暂停项，DNS 切换与补偿调用顺序不变。
- [x] 证明 task ID 的内容或顺序变化会改变 preview digest。

定向验证：

```bash
go test ./internal/cli ./internal/restore ./internal/backup
```

## 5. 文档、全量检查与部署验证

- [x] 更新 `examples/ark.yaml`、`docs/operations.md`、`docs/design.md` 与 `docs/roadmap.md`，记录配置、时序、正常启用假设、强制终止处理和回滚步骤。
- [x] 更新 `.trellis/spec/backend/dnsmgr-integration-guidelines.md`，固化 P5-3 维护窗口契约与测试矩阵。
- [x] 运行格式化、静态检查、全量测试和示例清单校验。
- [ ] 在测试 dnsmgr 任务上验证暂停与恢复；测试前确认任务应为启用状态，结束后核对 `active=1`。
- [ ] 部署新版 ark 前先运行 `ark validate`、`ark doctor` 和带 expected preview digest 的 dry-run；真实恢复后核对 maintenance 与 DNS 结构化结果。

全量验证：

```bash
gofmt -w <changed-go-files>
go vet ./...
go test ./...
make check
go run ./cmd/ark validate --config examples/ark.yaml
```

## 回滚

1. 从各 host 的 `dnsmgr` 段删除 `task_ids`，执行 `ark validate`。
2. 确认后续 restore dry-run 重新显示人工暂停/恢复项。
3. 人工核对已关联 dmonitor 任务均为 `active=1`。
4. 回滚 ark 二进制；dnsmgr P5-1/P5-2 加法接口无需回滚。
