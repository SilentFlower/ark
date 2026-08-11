# Release Operations

## Conclusion

Release operations exist.

## Evidence Checked

- `task.json`
- `prd.md`
- `design.md`
- `implement.md`
- `implement.jsonl`
- `check.jsonl`
- `fd53cab` 与 `82dd81f` 的提交文件
- 当前 Git 状态

## Drift Check

原任务缺少 `release.md`；本次审计已根据任务材料和 Git 提交补齐运行时依赖事项。

## SQL Changes

None.

## Configuration Changes

None. `ARK_SSH_TEST_*` 仅用于 localhost 集成测试，不是生产环境变量。

## Batch / Deployment Scripts / Data Repair

- `[08-11-p1-sshexec]` 部署新版 ark 前，确认 hub 已安装 OpenSSH 客户端，
  且 `ssh` 命令位于服务进程的 `PATH` 中。

## External Systems / Dependent Platforms

None. 目标机继续只要求已有的 `sshd`，本任务不新增目标机部署组件。

## Release Order

1. 在 hub 安装或确认 OpenSSH 客户端可用。
2. 部署包含 `fd53cab` 的 ark 版本。
3. 运行 `ark doctor` 验证新增的 `ssh` 检查项。

## Rollback Notes

回滚 ark 代码即可；已安装的 OpenSSH 客户端可以保留，不影响旧版本。

## Post-release Verification

- 在 ark 运行用户和同一 `PATH` 下执行 `ssh -V`，确认退出码为 0。
- 运行 `ark doctor`，确认全局 `ssh` 检查项为 `ok`。
- P1-3 接入远程 doctor 后，再验证真实目标机连接；该项不属于本任务上线阻断条件。
