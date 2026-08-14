# 执行计划

## 当前状态

- [x] 五个保留子任务均已完成、提交并归档。
- [x] P3-3 以“通过·已接受风险”收尾；`CHK-001` 与 `FBK-001` 保留为未修复事实。
- [x] P3-4 经用户确认取消，不执行、不计为通过。
- [x] 父任务已完成反向数据链审计、最终全仓门禁、验收状态同步与 roadmap 更新。

## 步骤

1. 完成五个保留子任务的 PRD、设计、实施计划和上下文清单；P3-4 记录为取消且移出父任务。
2. 按已确认的“原 host 隔离项目、每周一次”方案刷新父任务和 `p3-verify` Brief。
3. 以 `commit-only`、`check-depth=auto` 启动第一波 auto-loop：
   `p3-restore-plan`、`p3-restore-execute`，显式声明 Execute 依赖 Plan。
4. 第一波终态后报告本地提交和待归档任务，不自动 push 或归档。
5. 用户提供授权的干净目标机后执行 `p3-live-rebuild-validation`；实机发现的缺陷回到代码
   任务修复并完成复验。
6. 以第二波 auto-loop 实现并检查 `p3-verify`，随后在确认的隔离环境完成验收。
7. 核对五个保留子任务均归档，反向追踪
   `manifest -> Plan -> Dump -> Feed/Run -> compose health -> verification`。
8. 运行最终全仓门禁；发现业务代码缺陷时重开或新建所属子任务，不在父任务直接修复。
9. 全部通过后更新父任务验收项和 `docs/roadmap.md` 的 P3 状态，保留 P3-3 已接受风险与
   P3-4 取消记录。

## Auto-loop Wave A

```text
p3-restore-plan
p3-restore-execute       depends on p3-restore-plan
```

## Auto-loop Wave C

```text
p3-verify                depends on p3-restore-execute, p3-live-rebuild-validation
```

`p3-live-rebuild-validation` 是人工任务，不能作为同一 runner 队列中的自动依赖项；Wave C
只能在该任务完成后单独启动。

## 批次验证

```bash
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

## 完成前检查

- 第一波队列不包含父任务、真实环境任务或 `p3-verify`。
- 所有依赖通过 `--depends-on` 显式传入，不依赖任务列表顺序。
- 不把跳过的 restic/Docker/真实环境测试写成通过。
- auto-loop 不 push、archive、release、deploy 或修改真实外部系统。
- 父任务只更新规划、集成结论和 roadmap；业务代码修复由独立子任务承载。
