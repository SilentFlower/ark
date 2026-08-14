# P3-5 ark verify 自动演练

## Goal

每周把最新可用备份恢复到与生产资源严格隔离的 Docker Compose 环境，执行健康与一致性检查，
记录 `store.Verification` 后销毁演练资源，让恢复能力从一次性人工证明变成持续可观测能力。

P3-3 已完成受限隔离实机验收并由用户接受非干净 VPS 风险；本任务复用已实测的恢复链与
P3-2A 隔离能力，不把该受限结论改写为严格跨机验收通过。P3-4 已取消，不是本任务前置条件。

## Requirements

### R1 命令、快照与调度

- 新增 `ark verify [--host <source>] [--snapshot latest|<manifest-id>]
  [--keep-on-failure] [--json]`。
- 未指定 `--host` 且 `--snapshot=latest` 时，按当前 `ark.yaml` 顺序为每台 host 选择最近一次包含它的
  manifest 并串行验证；显式 manifest ID 仍只验证该 manifest 中可匹配的 host。指定 `--host` 时只验证
  该 host。未知 host、manifest/config 漂移或 host 缺少可恢复结果均显式失败。
- 第一版 destination 固定为 source 对应的原 host，不提供 `--to`；未来有专用验证机时仍复用
  相同隔离、基线和清理契约，不能增加宽松路径。
- `ark install` 生成一个 `ark-verify.service` 与 `ark-verify.timer`。timer 使用 systemd 的
  `OnCalendar=weekly`、`Persistent=true` 和最长 6 小时随机延迟，service 无 `--host`，一次串行
  验证全部 host，避免多个 per-host timer 同时争用全局锁。
- 手工 verify 与 timer 运行都获取现有 `/run/ark.lock` 非阻塞全局锁；与 backup、restore 或另一轮
  verify 冲突时立即非零退出，不并发访问仓库或目标机。

### R2 隔离约束

- verify 通过 `restore.WithIsolationOptions` 使用 `purpose=verify` 和本次 verification ID 作为
  instance key；必须复用 P3-2A 的命名、路径、Compose 转换、状态和 cleanup，不复制第二套实现。
- Compose project、container、volume、network 和 files root 全部带 verification 身份并与生产
  资源分离；恢复、检查和销毁前校验 `com.docker.compose.project` 与 isolation label。
- verify 专用隔离策略移除全部 published ports，不绑定生产 IP、域名或公网端口；健康检查只使用
  Compose health、容器内数据库 CLI、image digest 和 SSH 内部检查。首版不新增 HTTP/外部业务探针。
- 生成的 Compose 只引用 verification 专用 files root，不得引用或覆盖生产 `.env`、bind mount、
  config、secret、container、network、volume 或文件路径。
- external 资源、host/container namespace、特权设备、无法安全改写的路径或任何无法证明归属的资源
  都必须在创建 Docker 资源前 fail closed。

### R3 执行、生产基线与销毁

- 每个 host 的执行顺序复用 restore：files、image digest、volume、database prepare/data、
  application、health；Executor 不增加 verify 特例。
- 首次目标写入前采集生产 Compose project 的容器 ID/state/digest、network、volume，以及清单声明的
  生产文件元数据摘要；演练清理或保留决策完成后再次采集并比较。
- 正常通过后总是调用 `restore.CleanupIsolation` 销毁验证 container、network、volume、files root
  和生成配置；失败时默认同样清理。
- `--keep-on-failure` 只允许保留状态文件中记录且标签、project、路径全部匹配的 verification 资源，
  并输出精确 cleanup 命令；它不能跳过生产基线复核或放宽归属校验。
- cleanup 失败、生产基线变化或残留未知资源都使 verification 为 fail，即使恢复和健康检查已经通过。
- context 取消后仍使用有界收尾 context 尝试 cleanup、生产基线复核和结果持久化；任何失败事实不能
  因取消被静默丢失。

### R4 健康、损坏与状态库

- 任一 target snapshot 的 `restic.Dump` 失败、Feed 截断、数据库导入失败、digest 不一致、health
  失败、cleanup 失败或生产基线变化都记录 `store.StatusFail`。
- 使用现有 `Store.RecordVerification` 写入 verification ID、host、来源 run ID、manifest snapshot
  ID、开始/结束时间、状态、脱敏错误与合法 detail JSON，不修改现有数据库 schema。历史 manifest 的
  run 已不在本地状态库时，外键关联写 `NULL`，detail 仍保留来源 run ID。
- detail JSON 包含 schema version、source、manifest/run、target snapshots、isolation 资源、各阶段结果、
  health、生产基线差异、cleanup 状态和 keep-on-failure 结果；不包含凭证、环境值或业务数据。
- 人类和 JSON 输出包含每个 host 的 verification ID、manifest、状态、隔离项目、清理结果与失败阶段；
  任一 host 失败时命令整体非零，但仍继续记录后续 host 的验证结果。
- 人为不可读或内容损坏的 target snapshot 必须让对应 verification 明确失败，不能只凭容器已启动而通过。

## Non-Goals

- 不实现 P4 页面/API/通知、跨后端复制、任意用户 shell/HTTP 业务探针或生产切流。
- 不修改生产 DNS、TLS、dnsmgr、防火墙或应用环境变量，不把验证环境暴露给公网。
- 第一版不支持专用验证 host、`--to`、自定义 verify schedule 或按 host 配置不同频率。
- 不允许通过资源名称、`--force`、`--skip-doctor` 或 `--keep-on-failure` 绕过隔离、基线和清理校验。

## Acceptance Criteria

- [ ] verify 复用 restore Plan/Executor、`WithIsolationOptions` 与 `CleanupIsolation`，不存在第二套恢复语义
- [ ] 首版在原 host 使用独立 project、container、volume、network 和 files root，且不发布宿主机端口
- [ ] 所有创建、检查、保留与销毁操作验证 project/isolation label 和路径，无法证明归属时 fail closed
- [ ] 演练前后生产容器、digest、volume、network 与声明文件的基线完全一致
- [ ] 成功和失败默认可清理；清理失败使结果失败，keep-on-failure 只保留已证明归属的验证资源
- [ ] 不可读或损坏 target snapshot、Feed、database、digest、health、baseline 或 cleanup 失败均记录 fail
- [ ] `Store.RecordVerification` 保存完整、合法、脱敏的结果关联，任一 host 失败时命令整体非零
- [ ] `ark install` 原子安装单个每周 all-host verify timer，且与 backup/restore 共用非阻塞全局锁
- [ ] `go test ./internal/verify ./internal/restore ./internal/cli ./internal/store ./internal/systemd -race -count=1` 通过
- [ ] `make check`、`make build`、无 CGO 构建与 `git diff --check` 通过
