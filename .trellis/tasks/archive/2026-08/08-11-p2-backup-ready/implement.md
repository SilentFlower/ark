# 执行计划

## 步骤

1. 完成九个子任务的 PRD、设计、实施计划和上下文清单。
2. 收敛所有自动队列任务的 Open Questions，并刷新 Brief。
3. 以 `commit-only`、`check-depth=auto` 启动六个代码任务的 auto-loop，显式传入依赖。
4. 队列终态后报告 blocked 项、本地提交和待归档任务，不自动 push 或归档。
5. 交互完成 `p2-ssh-host-key-usability`，通过 Check-All 并归档后再进入真实环境。
6. 用户提供真实环境后执行 `p2-live-validation`；实机发现的缺陷进入
   `p2-stream-close-lifecycle`，修复后回到验收任务完成复验。
7. 核对九个子任务均已归档，逐项读取其 task/check/release/验证证据，确认完成链完整。
8. 反向追踪 `config.Target -> sshexec.Runner.Stream -> backup executor -> restic.BackupStdin
   -> Wait/Close/forget -> store.RunTarget + manifest -> retention/prune`，核对稳定标签、filename、
   失败快照撤销、脱敏错误和状态一致性。
9. 运行最终全仓验证，核对真实环境证据与本地门禁互补；全部通过后更新父任务验收项和
   `docs/roadmap.md` 的 P2-2 至 P2-8 / P2 阶段状态。
10. 执行 Full Check-All；如发现业务代码缺陷，重开或创建所属子任务，不在父任务直接修复。

## Auto-loop 队列

```text
p2-restic
p2-target-executors        depends on p2-restic
p2-stream-integrity        depends on p2-restic, p2-target-executors
p2-manifest                depends on p2-restic, p2-target-executors
p2-backup-cli              depends on p2-stream-integrity, p2-manifest
p2-hub-protection          depends on p2-backup-cli
```

## 批次验证

```bash
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

真实环境证据额外读取：

```text
.trellis/tasks/archive/2026-08/08-11-p2-live-validation/validation.md
.trellis/tasks/archive/2026-08/08-11-p2-live-validation/release.md
```

## 完成前检查

- 队列不包含父任务和 `p2-live-validation`。
- 所有依赖使用 runner 的 `--depends-on`，不依赖任务列表顺序猜测。
- 不把 skipped 的 restic/真实环境测试写成通过。
- 不由 auto-loop push、archive、release、deploy 或修改真实外部系统。
- 父任务只更新规划、集成结论和 roadmap 状态；任何业务代码修复必须回到独立子任务。
