# Brief — P3-1 恢复计划与 dry-run

## Goal

- 从选定备份 manifest 与当前清单生成稳定、完整、可打印的纯数据恢复 Plan，并提供严格只读的 `ark restore --dry-run`。

## Scope

- 支持必填 `--host`、可选 `--to`、`--snapshot latest|<manifest-id>`、`--dry-run` 和 `--json`。
- 扩展 manifest 读取以保留精确 manifest snapshot ID，构建 `internal/restore` Plan/Step/Phase。
- 严格匹配 source/destination Project、Target.ID/type 与 manifest target，并输出稳定阶段顺序。

## Non-Goals

- 不执行恢复、运行 doctor/SSH/Docker、打开状态库、获取锁或写目标资源。
- 不支持临时 SSH 参数、target 子集、跨项目改名或 manifest schema 重构。

## Key Decisions

- source 与 destination 都必须存在于当前清单；跨主机只替换连接目标，Project 与 Target 定义必须完全一致。
- 显式 snapshot 只接受唯一 manifest snapshot，不能把 target snapshot 当成 manifest 或回退到其它候选。
- Plan 只保存结构化事实和人工确认项，不保存命令字符串、凭证、Repo、Runner 或打开的流。

## Key Context

- manifest 提供备份结果事实，`ark.yaml` 提供连接、Project 与 Target 参数。
- 顺序固定为 files、image digest、volume、database prepare/data、application、health。

## Risks / Deferred

- 当前 manifest 不含完整 target 配置；任何清单漂移必须在执行前一次性报错。
- 读取 API 必须保留现有 `LoadLatestManifest` 兼容性，同时提供精确 snapshot 元数据。

## Acceptance

- latest/显式 manifest 均生成稳定 Plan，保留 run、manifest snapshot、target snapshot 与 digest。
- target/project 漂移、失败 target 和损坏 manifest 均 fail closed。
- dry-run 依赖替身证明无目标副作用，JSON 纯净且输出脱敏。
- restore/backup/cli race 测试及全仓、构建、无 CGO、diff 门禁通过。

## Next Step

- 先扩展 manifest 选择入口，再实现 Plan 构建和 restore dry-run CLI。
