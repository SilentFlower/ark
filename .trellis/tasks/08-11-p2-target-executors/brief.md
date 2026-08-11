# Brief — P2-3 target 执行器

## Goal

- 用现有 `sshexec.Runner` 为五类 target 生成纯数据流、稳定文件名和确定元数据。

## Scope

- 实现 postgres、redis、volume、files、image_digest 执行器与分发、生命周期测试。
- 保留 reader + Wait，严格执行 compose argv、BGSAVE/LASTSAVE 和运行容器 digest 契约。

## Non-Goals

- 不调用 restic、不写 store、不做坏快照撤销，不执行真实机器验收。

## Key Decisions

- 执行器不判断 local/SSH；所有远程转义继续由 Runner 负责。
- stdin filename 基于 `<host>/<target.ID()>`，不得含日期或 run ID。

## Key Context

- 依赖 `p2-restic` 的稳定接口；P2-4 将消费本任务返回的 Wait。

## Risks / Deferred

- image digest 歧义必须失败；真实 docker/SSH 产流留给 `p2-live-validation`。

## Acceptance

- 五类精确 argv、流、Wait、context、稳定输出和失败矩阵测试通过。

## Next Step

- 启动任务后先定义最小 executor result 与 compose argv 复用边界。
