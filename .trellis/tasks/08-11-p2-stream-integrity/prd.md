# P2-4 流完整性保障

## Goal

在 target 数据流与 restic snapshot 之间建立两道完整性防线：显式校验上游 Wait 并撤销
坏快照，以及用历史成功字节数识别静默截断。

## Requirements

### R1 流完成顺序

- 用 counting reader 统计实际交给 restic 的字节数，不整读、不落临时文件。
- `BackupStdin` 返回后必须调用且只调用一次执行器 Wait；不能因 restic 成功就提前判成功。
- restic 失败时仍要关闭 reader、回收上游并保留双方错误，不得遗留进程。

### R2 坏快照撤销

- restic 已创建 snapshot 但 Wait 非零时，target 必须失败并调用 `ForgetSnapshot`。
- Forget 失败与 Wait 失败使用 `errors.Join` 同时可见，不能把撤销失败覆盖掉。
- 未取得 snapshot ID 时不猜测或删除其它快照。

### R3 体积跌幅

- 写入前通过 `Store.LastSuccessfulTargetBytes(host, targetID)` 读取历史基线。
- 没有历史时正常通过；历史值为 0 时不做比例判断。
- 当前成功字节数低于上一成功值的 50% 时标记 warn，保留 snapshot 并记录原因。
- 本任务先使用固定默认 50%，不扩展清单 schema；可配置阈值以后单独规划。

### R4 状态记录

- 成功、warn、失败都写入 `store.RunTarget`，包含 bytes、duration、snapshot ID 和脱敏错误。
- Wait/Forget/写库失败均返回错误；状态库失败不能把 target 伪造成成功。
- 结果模型应可直接被 P2-5 manifest 和 P2-6 整体 run 聚合复用。

## Non-Goals

- 不实现 target 命令、restic CLI 封装、manifest 或完整 backup 编排。
- 不执行真实 `kill -9 pg_dump` 验收；该破坏性场景归 `p2-live-validation`。
- 不实现告警发送，只产生 warn 状态和可消费原因。

## Acceptance Criteria

- [ ] 假上游非零退出时 target 失败，已产生 snapshot 被精确撤销
- [ ] Forget 失败与 Wait 失败均保留，仓库撤销失败不会被静默吞掉
- [ ] 无历史、正常体积、恰好 50%、低于 50% 和零基线边界均有测试
- [ ] bytes、duration、snapshot ID、status 与脱敏 error 正确写入状态库
- [ ] restic 失败、context 取消和状态库失败均清理 reader/Wait 且错误链完整
- [ ] race、make check、构建和 git diff 检查通过
