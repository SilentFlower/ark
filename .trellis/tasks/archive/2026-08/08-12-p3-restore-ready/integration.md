# P3 最终集成复核

## 结论

P3 按用户确认的范围完成，结论为“通过·已接受风险”。恢复计划、恢复执行、隔离恢复、
跨主机受限实测和自动演练已形成同一条可审计恢复链；本轮未发现需要回流业务子任务的代码缺陷。

本结论不表示原始严格验收全部通过：P3-3 的干净 VPS 与真实生产业务验收仍未满足，P3-4
hub 自身重建实测已取消。

## 子任务证据

| 子任务 | 业务或验收提交 | 归档结论 |
| --- | --- | --- |
| P3-1 恢复计划 | `39206c8` | Plan、manifest 选择与 dry-run 零副作用已完成 |
| P3-2 恢复执行 | `ed05aa8` | 七阶段顺序、流完整性、幂等、冲突保护与健康检查已完成 |
| P3-2A 隔离恢复 | `226156d` | 资源派生、自动端口、状态续跑与精确 cleanup 已完成 |
| P3-3 跨主机实测 | `c58a952` | 受限隔离验收通过，`CHK-001` 与 `FBK-001` 已接受但未修复 |
| P3-5 自动演练 | `6898e89` | 生产基线、无 published ports、结果持久化与 weekly timer 已完成 |

五个子任务均位于 `.trellis/tasks/archive/2026-08/`，状态为 `completed`，上述提交均为当前
`main` 的祖先。

## 数据链反向审计

| 环节 | 当前实现证据 | 结论 |
| --- | --- | --- |
| Manifest | `backup.Manifest`、`ManifestSelection` 与 schema/target 校验 | 备份事实与精确 manifest snapshot 可追溯 |
| Plan | `restore.BuildPlan` 严格匹配当前清单中的 source、destination、project 与 target | 漂移或歧义在执行前 fail closed |
| Dump | `restic.Repo.Dump` 返回会暴露进程退出状态的流 | restic 截断不能伪装成成功 EOF |
| Feed / Run | `sshexec.Runner` 与 `restore.Execute` 按七阶段串行执行 | 远程退出、数据导入、digest 和健康失败均显式返回 |
| Isolation / Cleanup | `WithIsolationOptions`、label、`state.json` 与 `CleanupIsolation` | 普通恢复不静默隔离，隔离资源可证明归属并精确清理 |
| Verification | `verify.Execute` 对比生产基线并调用 `Store.RecordVerification` | cleanup、基线或持久化任一失败都会得到最终 fail |

## 最终门禁

- `make check`：通过；gofmt、`go vet ./...` 与 `go test ./... -race -count=1` 全绿。
- `make build`：通过。
- `CGO_ENABLED=0 go build -trimpath -o <临时路径>/ark ./cmd/ark`：通过，临时产物已清理。
- `go mod verify`：通过，输出 `all modules verified`。
- `go test -v ./internal/systemd -run '^TestGeneratedUnits_SystemdAnalyzeVerify$' -count=1`：通过，
  当前环境中的 `systemd-analyze verify` 已实际执行。
- `git diff --check`：通过。

真实 Docker 隔离与生产基线测试没有在父任务重复执行；复用 P3-2A 与 P3-5 已归档的实机和
集成测试证据。父任务只运行最终全仓门禁，不把历史未重跑的外部环境测试写成当前通过。

## 已接受风险与取消项

- `CHK-001`：P3-3 destination 不是只预装 Docker、Compose v2 与 sshd 的干净 VPS，且恢复对象
  是受限验证栈，不是严格意义上的真实生产业务验收。
- `FBK-001`：本机回收站中的临时 SSH 私钥、仓库密码和其它测试材料仍可恢复。
- P3-4：缺少独立干净机器且现有宿主机无隔离 VM 空间，经确认取消；不执行、不计为通过。

风险接受不表示问题已修复。若未来要求完成严格 P3-3 或 hub 灾备证明，应分别提供干净 VPS 或
独立故障域并新建任务。
