# P2-2 restic 封装

## Goal

新增 `internal/restic/`，把 restic CLI 封装成稳定、可测试且不会泄漏凭证的 Go 边界，
供后续 target、manifest、backup 和 restore 流程复用。

## Requirements

### R1 API 与职责

- 新增 `repo.go`、`repo_test.go`，实现 roadmap 定义的 `New`、`EnsureInit`、
  `BackupStdin`、`Snapshots`、`Forget`、`ForgetSnapshot`、`Dump`、`Check`。
- `Repo` 只理解 restic 仓库、凭证环境、标签和快照，不依赖 backup、store 或 CLI。
- Snapshot 只映射后续流程实际需要的稳定 JSON 字段，不暴露 restic 人类输出。

### R2 命令与凭证

- 所有机器可读输出使用 `--json` 并通过 `encoding/json` 解析。
- `RESTIC_REPOSITORY`、`RESTIC_PASSWORD_FILE` 和 `repo.env_file` 值只进入当前
  restic 子进程的 `cmd.Env`；禁止 `os.Setenv` 和 `RESTIC_PASSWORD`。
- env 文件沿用 doctor 的受限 `KEY=VALUE` 语法；实现时应复用或最小抽取该解析边界，
  不能形成两套安全语义。
- 错误可包含命令与凭证文件路径，但不得回显 env/password 内容或完整环境。

### R3 流与生命周期

- `BackupStdin` 直接把 reader 连接到 `restic backup --stdin`，filename 必须非空且稳定。
- `Dump` 保持 roadmap 的 `io.ReadCloser` API，同时确保 restic 非零退出能通过最终
  `Read` 或 `Close` 返回，不能只暴露 stdout 而丢失 Wait。
- 实际备份、dump 和 check 使用调用方 context，不套用 doctor 的 15 秒探测超时。
- 任一 reader、pipe 或子进程清理错误都必须可见。

### R4 仓库与保留策略

- `EnsureInit` 幂等；只在确认仓库不存在时 init，鉴权失败、损坏或不可达不能误判为未初始化。
- 标签参数逐项传递，禁止拼 shell 字符串。
- `Forget` 映射 `config.Retention` 并统一 prune；`ForgetSnapshot` 能撤销指定坏快照。
- `Check` 执行 restic 完整性检查并保留错误链。

## Non-Goals

- 不实现 target 命令、流完整性业务、manifest 或 backup CLI。
- 不支持 restic 之外的仓库后端。
- 不在本任务升级 Go 或选择新的凭证格式。

## Acceptance Criteria

- [x] 单元测试覆盖全部 argv、JSON 解析、标签、env 覆盖和敏感值不泄漏
- [x] `EnsureInit` 对已初始化仓库幂等，且不会吞掉鉴权/损坏错误
- [x] stdin 与 dump 全程流式，不整读、不落临时文件，退出状态不会丢失
- [x] 本地临时 repo 集成测试完成 init → backup → snapshots → forget → check
- [x] 集成测试由 `testing.Short()` 和 restic 可执行文件探测保护
- [x] `make check`、`make build`、无 CGO 构建和 `git diff --check` 通过
