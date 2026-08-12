# Brief — P2 真实流 Close 生命周期修复

## Goal

- 修复真实 `exec.Cmd.StdoutPipe` 在读取 EOF、`Wait` 完成后再次 `Close` 被误判为备份失败的问题，同时保持失败回收与坏快照撤销语义。

## Scope

- 修正 `internal/sshexec.Stream` 与 `internal/backup.BackupTarget` 之间的真实 pipe 生命周期契约。
- 在统一执行层或流包装层归一化 `Wait` 后由 `exec.Cmd` 已关闭 stdout pipe 导致的预期错误。
- 保留真实 Reader Close 错误的可见性，以及 restic 提前失败时先关闭上游、再等待命令退出的回收顺序。
- 补充基于真实 `exec.Cmd.StdoutPipe` 的 local 与 localhost SSH 集成测试。
- 保留 restic 已输出 summary 后失败时的 snapshot ID，并由 target 完整性层或 manifest
  保存层精确撤销，不能让失败快照进入后续恢复候选。
- 为 systemd oneshot 提供受管的 restic 缓存目录，并安全呈现 local doctor 失败项名称。
- 修复后回到 `p2-live-validation`，重新执行首次信任后的完整备份场景和剩余验收。

## Non-Goals

- 不在 PostgreSQL、Redis、volume、files 等各类 target 中分别增加绕过逻辑。
- 不改变现有 target 类型、备份数据内容或首次 SSH 主机信任策略。
- 不吞掉与 `Wait` 后预期已关闭状态无关的 Reader Close 错误。

## Key Decisions

- 修复放在统一执行层或流包装层，使所有流式 target 共享同一生命周期语义。
- 成功路径继续遵守读取完成后确认上游 `Wait`、再释放 Reader 的 ADR-011 顺序。
- 仅归一化 `Wait` 已接管并关闭 stdout pipe 后产生的预期已关闭错误；其他关闭错误仍作为失败返回。
- `Wait` 非零时仍判定失败，并精确撤销本次产生的坏 snapshot；仅有预期 Close 状态时不得把完整 snapshot 误报为失败。

## Key Context

- 真实环境中 PostgreSQL、Redis、volume、files 均已完整读取并生成 snapshot，但最终统一记录为“关闭数据流失败”；使用内存 Reader 的 `image_digest` 正常。
- 相关边界位于 `internal/sshexec.Stream` 与 `internal/backup.BackupTarget`，现有测试主要使用 `io.NopCloser` 或自定义 Reader，未覆盖 `exec.Cmd.Wait` 自动关闭 `StdoutPipe` 的行为。
- 真实环境已确认仅 Close 阶段失败时 snapshot 仍然存在，因此修复必须同时校正最终状态语义，且不能破坏 Wait 失败时的撤销契约。
- 第二轮实机验证确认 systemd system service 不保证提供 `HOME`，restic 0.19.1 在缺少
  `HOME` 和 `XDG_CACHE_HOME` 时会拒绝启动；另确认上游中断时 restic 可能先提交 snapshot
  并输出 summary，再返回非零退出。
- Full Check-All 进一步确认 `BackupStdin` 的失败返回契约由 target 与 manifest 两个入口消费；
  两者都必须在返回 ID 时执行精确撤销，并保留原始失败与撤销失败的错误链。

## Risks / Deferred

- 若错误归一化范围过宽，可能隐藏真实文件描述符或传输关闭故障；测试必须覆盖预期错误与真实错误的分界。
- 若调整回收顺序，可能重新引入 restic 提前退出时上游进程阻塞；必须保留并验证 Close -> Wait 的失败路径。
- 真实 localhost SSH 与完整隔离环境验收依赖运行环境可用，代码级检查通过后仍需回到父验收任务完成实机验证。

## Acceptance

- 真实 local/SSH `StdoutPipe` 在 Read EOF -> Wait -> Close 顺序下返回成功。
- Wait 非零仍返回失败并精确撤销对应 snapshot。
- restic 提前失败的 Close -> Wait 回收顺序和错误可见性保持不变。
- 四类流式 target 在真实 localhost sshd 上均不再因 Close 误报失败。
- 相关包测试、`make check`、常规构建、无 CGO 构建和 Check-All 通过。
- `p2-live-validation` 重新执行并完成剩余场景。
- systemd service/timer 在无 `HOME` 环境下可完成备份，已提交但失败的 snapshot 会被自动撤销。

## Next Step

- 启动任务后进入实现路由，先读取相关执行器、流结果与备份编排定义，再实现统一生命周期修复和回归测试。
