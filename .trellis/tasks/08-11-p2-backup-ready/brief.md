# Brief — P2 备份可用

## Goal

- 完成 P2-2 至 P2-8 及实机缺陷修复的最终集成复核，使 hub 的真实备份链具备可追溯、可撤销、可调度和受保护的完整证据。

## Scope

- 核对 8 个代码子任务和 1 个真实环境验收任务均已完成、提交并归档。
- 反向复核 restic、target、流生命周期、manifest、store、CLI、systemd、SSH 主机密钥和 hub 自备份数据链。
- 运行最终全仓门禁，并在结论通过后更新父任务验收项和 `docs/roadmap.md` 的 P2 阶段状态。

## Non-Goals

- 父任务不直接修改业务代码，不进入 auto-loop，不执行 P3 及以后阶段。
- 不重新执行已清理的真实服务器故障注入；复核使用已归档的脱敏实机证据。

## Key Decisions

- 代码任务与外部环境验收拆分；auto-loop 不执行真实服务器或对象存储操作。
- P2-6 采用每 host 一个 timer，全部实例共享全局 flock。
- 实机发现的跨包缺陷由独立 `p2-stream-close-lifecycle` 修复任务承载，父任务只做最终集成审计。

## Key Context

- 依赖链为 restic → target → integrity/manifest → backup CLI → hub protection
  → SSH 主机密钥易用性 → 真实环境验收 → 实机缺陷修复与最终复验。
- auto-loop 只本地提交，不 push、不归档；父任务在全部子任务之后完成。
- 当前九个子任务均已归档，`p2-live-validation` 已提供真实 restic、systemd、截断、
  hub 自备份和对象锁证据。

## Risks / Deferred

- 本地环境若缺少 restic，相关集成测试仍可能跳过；必须与已归档实机证据联合判断，不能把 skip 写成通过。
- 管理密码轮换和对象锁残留 volume 清理属于 release 运维事项，继续保留在上线审计中。

## Acceptance

- 8 个代码任务和真实环境验收的完成链、归档与证据完整。
- P2 依赖方向、稳定标签/filename、Wait/Close/forget、状态库/manifest 和密钥边界无漂移。
- `make check`、`make build`、静态构建和 `git diff --check` 全绿，roadmap P2 状态完成同步。

## Next Step

- 激活父任务后先审计九个已归档子任务的完成链与 release/validation 证据，再执行 P2 数据主链反向复核。
