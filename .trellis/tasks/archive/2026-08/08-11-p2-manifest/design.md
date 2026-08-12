# 技术设计：P2-5 快照清单

## 1. 模型边界

Manifest 类型放在 `internal/backup`，因为它描述一次 backup 的业务结果；restic 包只提供
snapshot 和 dump 原语，store 只持久化可查询状态。模型使用值类型与切片，避免携带 reader、
Runner 或数据库连接。

## 2. JSON 契约

- 顶层固定 `schema_version`、`run_id`、`ark_version`、`started_at`、`finished_at`、`hosts`。
- host 和 target 顺序按清单执行顺序保留，同时在校验中拒绝重复 `(host,target_id)`。
- image digest 映射序列化前按 service 稳定排序，避免无意义的字节变化。
- 解码后必须显式 Validate，不能只依赖 JSON 类型转换。

## 3. 仓库存取

写入流程为 Validate → JSON encode → `BackupStdin`，filename 使用包内常量，tags 使用
`ark-manifest` 与 run tag。读取流程为按 tag 查询 snapshots → 选择最新且唯一可解释项
→ Dump 固定路径 → 解码/校验 → 核对 run tag。

## 4. 失败语义

没有 manifest 是正常“暂无备份”分支；候选 snapshot 存在但 dump/JSON/schema 不合法是故障。
多个时间完全相同的候选使用 snapshot ID 做稳定次序，不能依赖 restic 返回顺序。
