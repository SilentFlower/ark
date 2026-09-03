# Release Operations

## Conclusion

Needs human review.

代码与本地质量门已完成，但 hub 部署和 `biz` latest verify 尚未闭合。本次生产验收确认 external network 与 `local` named volume 转换已越过原阻断点，同时暴露 Redis readiness 对 `redis-cli PING` 多行输出的误判；生产二进制已回滚。应先在独立任务中修复该阻塞，再重新部署并完成真机验收。

## Evidence Checked

- `task.json`、`prd.md`、`design.md`、`implement.md`、`brief.md`
- `implement.jsonl`、`check.jsonl`
- 业务提交 `a7c7f75` 与任务记录提交 `1f094bf`
- 当前任务目录和 Git 工作区均为 clean，`main` 与 `origin/main` 已同步
- 本次会话的 hub 部署、回滚与隔离资源清理结果

## Drift Check

原任务缺少 `release.md`。实施计划中的生产部署与验收步骤仍未完成，且当前存在新的 Redis readiness 阻塞，需要在后续任务中修复并重新核对。

## SQL Changes

None.

## Configuration Changes

None. 继续使用 hub 现有 `/etc/ark/ark.yaml`，不得在本任务上线过程中变更凭证或生产业务配置。

## Batch / Deployment Scripts / Data Repair

- 在 Redis readiness 修复通过完整质量门后，使用 `CGO_ENABLED=0` 重新构建静态二进制。
- 部署前记录本地 commit、二进制 SHA-256，并为 hub 当前 `/usr/local/bin/ark` 创建新的带时间戳即时备份。
- 原子替换二进制后，执行 `ark validate`、完整 doctor 与 dnsmgr AuthApi 检查。
- 不需要 SQL、数据修复或一次性业务数据处理。

## External Systems / Dependent Platforms

- hub 主机上的 Ark 二进制需要人工部署。
- `biz` 的 Docker Compose 使用生产 external network `api_shared`；隔离演练不得连接、修改或删除该网络。

## Release Order

1. 在独立任务中修复 Redis readiness 多行输出误判，并通过定向测试、重复 race、`make check` 和静态构建。
2. 记录 hub 当前二进制 SHA-256，创建新的即时备份。
3. 原子部署新静态二进制并执行 validate、doctor 与 dnsmgr AuthApi 检查。
4. 记录 `biz` 的生产 project、容器、network、volume 和 files 基线。
5. 执行 `ark verify --host biz --snapshot latest --json`，保存结构化结果。
6. 核对演练容器未连接 `api_shared`，并确认生产基线前后一致。
7. 核对无 isolation 容器、network、volume 或 restore root 残留，确认 verify service/timer 状态正常。

## Rollback Notes

- 验收失败时停止新的手工 verify，恢复本次部署前即时备份的 `/usr/local/bin/ark`，校验 SHA-256 后重新执行 validate 和 doctor。
- 隔离资源只允许通过结构化 cleanup command 或匹配 isolation ID 的 `ark restore cleanup` 清理；归属校验失败时保留现场，禁止按宽泛名称批量删除。
- 不修改生产 `api_shared`、生产容器、volume 或业务文件来绕过验收失败。

## Post-release Verification

- `biz` latest verify 完整通过。
- 隔离容器只连接 Ark 派生 bridge network，未连接生产 `api_shared`。
- 原 external network、生产 project/container/network/volume/files 基线前后一致。
- 结束后无本次 isolation label 对应的容器、network、volume 或 restore root 残留。
- `ark-verify.timer` 保持启用和活跃，`ark-verify.service` 最近一次结果与本次验收一致。
