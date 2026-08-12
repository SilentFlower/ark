# Release Operations

## Conclusion

Needs human review. 代码已实现 hub 一致性备份与对象锁提示，但对象存储控制台和离线恢复材料仍需人工确认。

## Evidence Checked

- task.json、prd.md、design.md、implement.md、implement.jsonl、check.jsonl
- commit `e7d17008eecdbf61bae248ff58b8faff976dd86a` 及其文件变更
- `docs/operations.md`、`examples/ark.yaml` 和当前 Git 状态

## Drift Check

Missing release.md. 任务材料明确包含仓库外人工操作，现补充上线审计记录。

## SQL Changes

None

## Configuration Changes

- 按实际 hub 部署调整 `examples/ark.yaml` 中的服务数据路径或卷名，确保不包含 repo 密码、对象存储凭证、SSH 私钥及其它解密材料。
- 在 hub 之外准备并验证至少两份离线恢复材料，范围以 `docs/operations.md` 为准。

## Batch / Deployment Scripts / Data Repair

None

## External Systems / Dependent Platforms

- 在对象存储控制台为 restic bucket 启用 Object Lock、Immutability 或等价的仅追加保留，并让默认保留期覆盖最长 restic 保留窗口。
- 使用 hub 当前写入凭证尝试删除保留期内的测试对象，确认对象存储拒绝删除。该操作不得由自动化归档流程执行。

## Release Order

1. 先验证离线恢复材料可读取。
2. 再启用并核对对象锁保留策略。
3. 最后部署配置并恢复备份定时任务。

## Rollback Notes

- 代码可按常规方式回滚。
- 对象锁通常不能提前解除已锁定对象的保留期；启用前必须确认容量和保留窗口。

## Post-release Verification

- 运行 `ark validate` 和 `ark doctor --all`，确认 `repo.object_lock` 为预期的人工核验警告。
- 执行一次 hub 备份，恢复最新 `ark-state` 快照，并对导出的数据库运行 `PRAGMA integrity_check`。
- 按 `docs/operations.md` 完成对象删除拒绝测试和离线材料可用性检查。
