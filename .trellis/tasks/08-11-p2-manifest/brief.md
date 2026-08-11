# Brief — P2-5 快照清单

## Goal

- 用版本化 manifest 把一次 run 的所有 target snapshot 和镜像 digest 串成恢复输入。

## Scope

- 实现 schema v1 模型、严格校验、稳定 JSON、restic 标签存取和最新 manifest 读取。
- 复用 P2-3/P2-4 最终结果，不重新推断 snapshot ID、bytes 或 digest。

## Non-Goals

- 不执行 target、保留策略、store、CLI 或 P3 restore Plan。

## Key Decisions

- manifest 使用稳定 filename、`ark-manifest` 与 `run:<id>` 标签。
- 未知/高版本 schema 拒绝；map 输出排序稳定，不预埋未知恢复字段。

## Key Context

- manifest 是 P3 恢复入口，依赖 p2-restic 与 p2-target-executors。

## Risks / Deferred

- 持久化字段发布后即成为兼容契约，任何破坏性变化必须升 schema。

## Acceptance

- 往返、非法模型、标签写入、最新选择、dump/run 一致性测试通过。

## Next Step

- 启动任务后定义 schema v1 类型与 Validate，再接 restic 存取。
