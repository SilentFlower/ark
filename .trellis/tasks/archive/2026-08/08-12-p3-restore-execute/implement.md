# 执行计划

## 步骤

1. 基于 P3-1 Plan 定义在 `internal/restore` 增加 Executor、步骤结果与依赖校验。
2. 提取 backup 指定 host 的可复用内部编排，保持现有 `ark backup` 行为和测试不变。
3. 实现 destination preflight、Compose label/volume/files 冲突检测与幂等后置条件检查。
4. 实现 `--force` 精确授权和破坏前 safety backup；失败时零破坏。
5. 按阶段实现 files、digest、volume、PostgreSQL、Redis、application 与 health。
6. 在 CLI 接入全局锁、doctor、skip-doctor、force、JSON、人类输出和失败哨兵。
7. 补 fake Repo/Runner、流生命周期、命令顺序、冲突/重跑/取消/健康失败测试。
8. 更新 external-command、database、backup-orchestration 等相关可执行规范。
9. 运行目标包、race、全仓、构建、无 CGO 与 diff 门禁。

## 验证命令

```bash
go test ./internal/restore ./internal/cli -race -count=1
go test ./internal/backup ./internal/restic ./internal/sshexec -race -count=1
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

## 风险与回滚点

- backup 编排提取是高回归点，必须用既有 `internal/cli/backup_test.go` 锁定行为。
- Redis 原子恢复必须在 fake 与真实 Compose 验收中验证；任何不明确的 volume 归属都 fail closed。
- `--force` 不得扩大为全项目清理开关；每个破坏动作前都以 Plan 与 Compose label 双重确认。

## 完成前检查

- `p3-restore-plan` 已完成并提供稳定 Plan 契约。
- `prd.md` 无 Open Questions，未把 P3-3 实机结果伪装为本任务单测结论。
- 真实服务器恢复仍由 `p3-live-rebuild-validation` 人工任务执行。
