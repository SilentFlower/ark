# Brief — P3-5 ark verify 自动演练

## Goal

- 每周把最新可用备份恢复到原 host 的严格隔离环境，执行健康与一致性检查，记录 Verification 后销毁资源，持续证明恢复链可用。

## Scope

- 新增 `ark verify [--host] [--snapshot] [--keep-on-failure] [--json]`；无 host 的 `latest` 按每台 host
  最近 manifest 串行验证，显式 manifest ID 保持单 manifest 语义。
- 新增 `internal/verify` 编排，复用 restore Plan/Executor、`WithIsolationOptions`、`CleanupIsolation` 与现有 `store.Verification`。
- 采集演练前后生产容器、digest、network、volume 和声明文件基线；默认清理，失败可显式保留已验证归属的资源。
- `ark install` 原子安装单个 weekly all-host verify service/timer，并与 backup/restore 共用 `/run/ark.lock`。

## Non-Goals

- 不实现 P4 页面/API/通知、任意 shell/HTTP 业务探针、生产 DNS/TLS/dnsmgr 修改或公网暴露。
- 第一版不支持专用验证 host、`--to`、自定义 schedule、按 host 不同频率或绕过隔离检查的参数。

## Key Decisions

- 第一版固定在 source 原 host 运行；同一命令串行验证全部 host，systemd 只生成一个 weekly timer，避免多个 timer 争锁。
- verify 扩展现有隔离转换并移除全部 published ports；普通 restore isolation 继续保留 Docker 自动端口行为。
- cleanup、生产基线复核和状态持久化都属于验收结果；任一失败即 verification fail。
- P3-3 仅为受限隔离实机验收通过且风险已接受；P3-4 已取消，二者都不会被改写为严格灾备通过。

## Key Context

- 核心链路为 `BuildPlan -> WithIsolationOptions(verify) -> restore.Execute -> CleanupIsolation -> RecordVerification`。
- `internal/verify` 拥有生命周期与生产基线，`internal/restore` 保持恢复职责，CLI 只组装依赖、全局锁和输出。
- JSON/detail/error 必须脱敏，不得包含 canonical Compose、env、凭证、SSH key 或业务数据。

## Risks / Deferred

- 原 host 演练共享 Docker daemon，只能证明备份可读、恢复链可执行和资源隔离，不能替代独立机器灾备。
- 复杂业务登录/写入探针和专用验证 host 延后，需要新的配置与安全设计。

## Acceptance

- verify 复用现有恢复和隔离实现，不发布宿主机端口；所有资源操作校验 label、project 和路径。
- 成功/失败默认可清理，keep-on-failure 只保留已证明归属的资源；演练前后生产基线完全一致。
- 损坏 snapshot、恢复、health、baseline、cleanup 或 store 失败均记录 fail，任一 host 失败时命令整体非零。
- weekly systemd timer、目标包测试、全仓检查、常规/无 CGO 构建和 diff 门禁全部通过。

## Next Step

- 完成 full Check-All 最终重检；通过后进入 `trellis-update-spec`，核对并记录本任务形成的可执行规范。
