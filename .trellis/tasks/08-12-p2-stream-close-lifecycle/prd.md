# P2 真实流 Close 生命周期修复

## Goal

修复真实 exec.Cmd StdoutPipe 在 Wait 后再次 Close 被误判失败的问题，并补齐真实 pipe 集成测试与坏快照撤销语义。

## Requirements

- 修复 `internal/sshexec.Stream` 与 `internal/backup.BackupTarget` 的真实 pipe 生命周期契约：
  正常读到 EOF、`Wait` 成功后，释放 Reader 不得因 `exec.Cmd.Wait` 已关闭 `StdoutPipe`
  而被误判为 target 失败。
- 保留 ADR-011 的顺序保证：成功路径必须确认上游 Wait；restic 提前失败时仍先关闭上游
  以解除阻塞；真实的 Reader Close 错误仍必须可见，不能全部吞掉。
- 明确成功 Wait 后的重复 Close/已关闭 pipe 错误如何归一化，修复必须位于统一执行层或流包装层，
  不能在每种 target 中分别绕过。
- 修正当前行为中“仅 Close 阶段失败但 snapshot 已完整生成并保留”的状态语义；不得把完整快照
  误报为失败，也不得改变 Wait 失败时精确撤销坏 snapshot 的契约。
- 补充使用真实 `exec.Cmd.StdoutPipe` 的集成测试，覆盖 local 与 localhost SSH 流，不能只使用
  `io.NopCloser` 或自定义无副作用 Reader。
- 实机复验中若 `restic backup` 已输出 summary 和 snapshot ID 后再因上游流失败返回非零，
  必须保留该 ID 给完整性层精确撤销，不能留下状态库不可见的坏快照。
- systemd oneshot 必须提供 restic 可用的受管缓存目录，不能依赖交互 shell 继承 `HOME`；
  local doctor 失败时错误必须列出失败检查项名称，但不得输出可能含敏感信息的 detail。
- 修复后重新执行 `p2-live-validation`，从首次信任后的完整备份场景继续全部验收。

## Acceptance Criteria

- [x] 真实 local/SSH `StdoutPipe` 在 Read EOF -> Wait -> Close 顺序下返回成功
- [x] Wait 非零仍返回失败并精确撤销对应 snapshot
- [x] restic 提前失败的 Close -> Wait 回收顺序和错误可见性保持不变
- [x] 四类流式 target 在真实 localhost sshd 上均不再因 Close 误报 fail
- [x] 相关包测试、`make check`、常规构建、无 CGO 构建和 Check-All 通过
- [x] `p2-live-validation` 重新执行并完成剩余场景

## Notes

- 发现来源：`p2-live-validation` 在 Debian 10 + OpenSSH + restic 0.19.1 的隔离环境中，
  PostgreSQL、Redis、volume、files 都完整读取并获得 snapshot ID，但最终统一记录为
  `关闭数据流失败`；`image_digest` 使用内存 Reader，因此成功。
- 真实仓库中的这些 snapshot 仍可列出，说明当前路径不会在仅 Close 失败时撤销 snapshot。
- 继续实机复验又发现并修复两项同链问题：systemd 环境缺少 `HOME` 导致 restic 无缓存目录，
  以及 restic 在 summary 后非零退出时已提交 snapshot ID 被丢弃，导致上层无法自动撤销。
