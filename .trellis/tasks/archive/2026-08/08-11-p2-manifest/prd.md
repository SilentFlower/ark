# P2-5 快照清单

## Goal

新增版本化 manifest，把一次 run 产生的多 host、多 target restic 快照和镜像 digest
串成可独立读取、校验并用于恢复规划的单一事实来源。

## Requirements

### R1 数据模型

- 新增 `internal/backup/manifest.go` 和测试，初始 schema version 为 1。
- 记录 run ID、ark 版本、开始/结束时间，以及每个 host/target 的 ID、类型、
  snapshot ID、bytes、duration、status/error 和镜像 digest 映射。
- 时间使用 UTC，duration 和 bytes 非负；稳定字段使用明确 JSON tag。

### R2 校验与兼容

- 序列化前校验 schema、run ID、host、target ID/类型、时间顺序、非负数和重复键。
- 反序列化拒绝未知或高于当前支持的 schema，错误同时报告实际/支持版本。
- v1 字段一旦发布即视为恢复契约；新增字段优先向后兼容，破坏性变更必须升 schema。

### R3 restic 存储

- manifest 使用稳定 stdin filename，不包含日期或 run ID。
- snapshot 标签至少包含 `ark-manifest` 和 `run:<run-id>`。
- 提供按 tag 列出并取回最新 manifest 的入口，使用 restic Snapshots + Dump，
  不解析人类输出。
- 读取时验证 snapshot、文件路径和 manifest 内 run ID 的一致性。

### R4 数据来源

- manifest target 结果复用 P2-4 的最终结果模型，不能重新猜测 snapshot ID 或 bytes。
- image digest 来自 P2-3 的运行容器反查结果，不允许退回 tag。
- manifest 存储失败使整轮 backup 失败，但已产生的 target 快照保留并在状态中可见。

## Non-Goals

- 不执行 target、保留策略、状态库写入或 CLI 编排。
- 不实现 P3 restore Plan，只提供其输入契约。
- 不预埋未知 P3/P4 字段。

## Acceptance Criteria

- [x] 完整 manifest JSON 序列化/反序列化往返不丢字段
- [x] 非法 schema、时间、重复 target、负数和缺失主键均被拒绝
- [x] manifest 以稳定 filename 和两类固定 tag 写入 restic
- [x] 多份 manifest 中能按 snapshot 时间确定性取回最新一份
- [x] dump 内容与 snapshot/run tag 不一致时明确失败
- [x] race、make check、构建与无 CGO 构建通过
