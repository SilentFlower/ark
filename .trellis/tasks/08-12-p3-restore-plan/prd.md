# P3-1 恢复计划与 dry-run

## Goal

从指定备份 manifest 与当前 `ark.yaml` 生成完整、稳定、可打印的纯数据恢复计划，并通过
`ark restore --dry-run` 在不修改目标机、状态库或仓库的前提下让管理员预审全部动作。

## Requirements

### R1 命令入口与选择器

- 新增命令：
  `ark restore --host <source> [--to <destination>] [--snapshot latest|<manifest-id>] --dry-run [--json]`。
- `--host` 必填，表示 manifest 中的备份来源 host；`--to` 省略时恢复到同名 host。
- P3 MVP 的 source 与 destination 都必须存在于当前清单；不支持命令行临时 SSH 参数。
- `--snapshot` 默认 `latest`；显式值必须无歧义匹配一个带 `ark-manifest` tag 的 manifest
  snapshot，不把 target snapshot ID 当作 manifest 选择器。
- `--dry-run` 是本任务唯一允许的 restore 执行模式；未带 `--dry-run` 时由 P3-2 接管。

### R2 Manifest 读取

- 保留现有 `backup.LoadLatestManifest` 契约，并新增可返回 manifest snapshot 元数据的读取入口，
  供恢复计划保留精确 `ManifestSnapshotID`。
- 最新选择继续按时间、ID 稳定排序；显式 ID 不匹配、匹配多个、tag/path/run 不一致、内容损坏
  或 schema 不支持时 fail closed，不回退到其它 manifest。
- manifest 中 source host 不存在、任一 target 为 fail、缺 snapshot ID 或 target 重复时拒绝生成计划。

### R3 清单映射

- manifest 是备份结果事实；当前清单提供 source/destination 的 SSH、Project 和 Target 定义。
- manifest target 必须按 `Target.ID()` 与 type 无歧义匹配 source host target。
- 跨主机恢复要求 destination 的 Project 与全部相关 Target 配置和 source 完全一致，仅连接方式、
  host 名与调度/保留策略可以不同；不支持在恢复时隐式改 service、volume、path 或 compose 定位。
- 任一 target 缺失、类型变化、files paths 漂移、Project 定位变化或 image digest 集合不完整时，
  在任何目标机命令前一次性返回全部可定位错误。

### R4 Plan 契约

- 新建 `internal/restore/plan.go`，定义导出的 `Plan`、阶段/步骤和纯构建函数；导出 API 按项目规范
  提供中文文档注释、参数和返回值说明。
- Plan 至少包含 manifest snapshot ID、backup run ID、source/destination host、目标 Project、
  每个 target 的配置副本、snapshot ID、image digest、稳定阶段顺序和人工确认项。
- 顺序固定为：files → image digest → volume → database prepare → database data → application → health。
- 同一输入必须生成稳定步骤顺序与稳定 JSON；Plan 不持有凭证内容、Runner、Repo、Store 或打开的流。

### R5 只读输出

- dry-run 只允许读取/校验清单和 restic manifest；不得获取全局锁、运行 doctor、创建 Runner、
  SSH 到目标机、运行 Docker、打开状态库、创建容器/volume、写文件或拉镜像。
- 人类输出逐阶段列出来源、目标、target、snapshot、digest、compose 位置、冲突策略和人工确认项。
- `--json` stdout 只包含 snake_case JSON；不得输出密码、对象存储凭证、SSH 私钥或 `.env` 内容。

## Non-Goals

- 不执行任何恢复步骤，不实现 `--force`、破坏前备份、健康检查或状态持久化。
- 不修改 manifest schema，不把 `ark.yaml` 配置复制进历史 manifest。
- 不支持 target 子集恢复、跨项目重命名、临时 SSH 参数或生产环境自动切流。

## Acceptance Criteria

- [ ] 真实 manifest 的 latest 与显式 ID 都能生成完整、稳定、可读的 Plan
- [ ] Plan 保留精确 manifest snapshot ID、run ID、全部 target snapshot 和 image digest
- [ ] source/destination/target/project 漂移被一次性拒绝，错误包含可定位字段
- [ ] 任一失败或缺失 target 都不能进入可执行计划
- [ ] dry-run 依赖替身证明没有锁、doctor、Runner、Docker、Store 或写操作
- [ ] `--json` 为纯 snake_case JSON，人类输出不含敏感内容
- [ ] `go test ./internal/restore ./internal/backup ./internal/cli -race -count=1` 通过
- [ ] `make check`、`make build`、无 CGO 构建与 `git diff --check` 通过
