# Brief — P2-4 流完整性保障

## Goal

- 防止 SSH/远程命令截断被 restic 误判为成功，并用历史字节数发现静默异常。

## Scope

- 统计流量、校验 Wait、撤销坏 snapshot、比较上一成功 bytes，并记录 RunTarget。
- 覆盖 restic/Wait/Forget/store/context 多失败组合和 50% 阈值边界。

## Non-Goals

- 不实现 target/restic 命令、manifest、告警发送或真实 kill pg_dump 验收。

## Key Decisions

- restic 成功后仍必须 Wait；Wait 失败且已有 snapshot 时必须精确撤销。
- 默认阈值固定 50%，恰好 50% 不告警，首次或零基线不比较。

## Key Context

- 依赖 p2-restic、p2-target-executors 和现有 store 历史/RunTarget API。

## Risks / Deferred

- 撤销失败不能被覆盖；真实截断证据留给 `p2-live-validation`。

## Acceptance

- 调用顺序、只调用一次 Wait、坏快照撤销、阈值和状态记录测试通过。

## Next Step

- 启动任务后建立单 target 流消费状态机和 counting reader。
