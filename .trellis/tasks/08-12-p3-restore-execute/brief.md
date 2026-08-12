# Brief — P3-2 恢复执行

## Goal

- 安全执行 P3-1 Plan，在清单内目标 host 上以串行、流式、可重跑且默认拒绝覆盖的方式重建 Compose 服务。

## Scope

- 完成真实 `ark restore`、全局锁、doctor、冲突预检、`--force`、破坏前备份和结构化输出。
- 实现 files、digest、volume、PostgreSQL、Redis、应用启动、健康与最终 digest 校验。
- 复用 backup 指定 host 编排、`restic.Repo`、`sshexec.Runner` 和 Plan，不通过子进程或复制语义实现。

## Non-Goals

- 不自动修改 DNS、TLS、防火墙、`.env` 或 dnsmgr。
- 不实现 verify、hub 灾备、target 子集或未进清单的临时目标机。

## Key Decisions

- 默认发现冲突即零写入退出；`--force` 只授权 Plan 精确资源，并先完成成功的 destination safety backup。
- 顺序固定为 files、digest、volume、数据库启动/导入、应用、health，全程不并行。
- `restic.Dump -> Runner.Feed` 保持流式；任一 Read/Close/Feed/health/digest 失败都使恢复失败。
- PostgreSQL 等待 `pg_isready`；Redis 停机恢复 RDB 并原子切换，不用固定 sleep。

## Key Context

- 与 backup 共用 `/run/ark.lock`，避免备份和恢复同时操作同一环境。
- 目标机不安装 ark/restic，不持有仓库凭证；人工确认清单保留 DNS、TLS、防火墙、`.env` 与 dnsmgr 五项。

## Risks / Deferred

- backup 编排提取可能回归现有 P2 行为，必须由既有 backup 测试锁定。
- `--force`、Redis volume 切换和幂等后置条件属于高风险点，无法证明资源归属时必须 fail closed。

## Acceptance

- 五类 target 的 argv、顺序、流和失败传播有测试。
- 默认冲突零写入，force safety backup 失败时不破坏目标；中断后可安全续跑。
- 健康、digest、人类/JSON 输出和人工清单完整、脱敏。
- restore/cli/backup/restic/sshexec race 测试及全仓、构建、无 CGO、diff 门禁通过。

## Next Step

- 在 P3-1 Plan 契约完成后，先提取可复用 backup 编排，再实现 restore Executor 的 preflight 与阶段状态机。
