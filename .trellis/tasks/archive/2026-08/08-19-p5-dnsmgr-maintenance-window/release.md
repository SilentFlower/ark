# Release Operations

## Conclusion

Release operations exist.

## Evidence Checked

- `task.json`、`prd.md`、`design.md`、`implement.md`
- `implement.jsonl`、`check.jsonl`
- 工作提交 `0d6b7dd feat: 实现 dnsmgr 维护窗口联动`
- 当前 Git 工作区与远端同步状态

## Drift Check

Missing `release.md`; this file records the deployment and post-release verification required by the task evidence.

## SQL Changes

None.

## Configuration Changes

- [08-19-p5-dnsmgr-maintenance-window] 在需要自动维护窗口的目标 host `dnsmgr` 段中配置有序 `task_ids`；这些任务在非维护期必须处于 `active=1`。
- [08-19-p5-dnsmgr-maintenance-window] 部署前运行 `ark validate`、`ark doctor` 和带 `--expected-preview-sha256` 的 restore dry-run，确认清单、凭证和恢复计划均有效。

## Batch / Deployment Scripts / Data Repair

- [08-19-p5-dnsmgr-maintenance-window] 部署包含 P5-3 维护窗口联动的新 Ark 二进制；dnsmgr P5-1 的 `POST /api/dmonitor/task/setactive` 必须已可用。

## External Systems / Dependent Platforms

- [08-19-p5-dnsmgr-maintenance-window] 在测试 dnsmgr dmonitor 任务上验证按序暂停和逆序恢复，测试前确认任务启用，测试结束后确认所有关联任务均为 `active=1`。

## Release Order

1. 确认 dnsmgr P5-1 接口和 AuthApi 凭证可用。
2. 更新 Ark 清单并完成 validate、doctor 和 dry-run。
3. 部署新版 Ark。
4. 执行测试恢复并核对 maintenance、DNS 结构化结果及 dmonitor 最终状态。

## Rollback Notes

1. 从各 host 的 `dnsmgr` 段删除 `task_ids`，运行 `ark validate`。
2. 确认 restore dry-run 重新显示人工暂停/恢复项。
3. 人工确认所有关联 dmonitor 任务均为 `active=1`。
4. 回滚 Ark 二进制；dnsmgr P5-1/P5-2 加法接口无需回滚。

## Post-release Verification

- 验证真实恢复首次写入前所有配置任务已暂停，数据恢复和 DNS 切换结束后任务按逆序恢复。
- 验证成功、恢复失败、DNS 失败和取消路径均输出 maintenance 摘要，且需人工处理的任务 ID 可见。
- 强制终止或恢复兜底失败时，按输出的 task ID 人工调用 dnsmgr 将任务恢复为 `active=1`。
