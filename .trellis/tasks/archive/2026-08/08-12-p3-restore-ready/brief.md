# Brief — P3 恢复可用

## Goal

- 完成 P3 恢复主链的最终集成复核，在保留已接受风险与取消项的前提下形成可审计的阶段结论。

## Scope

- 核对恢复 Plan、恢复执行、隔离恢复、跨主机实测和 `ark verify` 五个子任务的归档证据。
- 反向审计 `manifest -> Plan -> Dump -> Feed/Run -> compose health -> verification` 数据链。
- 运行最终全仓门禁，更新父任务验收状态和 `docs/roadmap.md` 的 P3 结论。

## Non-Goals

- 父任务不直接修复业务代码；发现缺陷时重开所属子任务或新建缺陷任务。
- 不执行 P4/P5/P6，不操作真实服务器、DNS、TLS、防火墙、对象存储控制台或离线密钥。
- 不补做 P3-4，也不把 P3-3 的风险接受表述为严格干净 VPS 验收通过。

## Key Decisions

- 五个保留子任务均已完成并归档，父任务只承担最终证据复核和 roadmap 同步。
- P3-3 结论为“通过·已接受风险”：受限隔离恢复、source independence、数据、镜像与清理通过，
  严格干净 VPS 和真实生产业务验收仍未满足。
- `CHK-001` 与 `FBK-001` 是已接受但未修复的风险；证据或范围变化时需要重新确认。
- P3-4 因缺少独立故障域而取消，不执行、不计为通过，也不阻塞本轮 P3 收尾。

## Key Context

- 五个子任务归档于 `.trellis/tasks/archive/2026-08/`，实机证据位于
  `08-12-p3-live-rebuild-validation/validation.md`。
- 数据链为 `backup.Manifest + config.Config -> restore.Plan -> restic.Dump -> Runner.Feed/Run -> Compose health -> Verification`。
- 最终门禁为 `make check`、`make build`、`CGO_ENABLED=0 go build ./cmd/ark` 与 `git diff --check`。

## Risks / Deferred

- `CHK-001`：P3-3 未在只预装 Docker、Compose v2 与 sshd 的干净 VPS 上恢复真实生产业务。
- `FBK-001`：本机回收站中的临时 SSH 私钥、仓库密码和测试材料仍可恢复。
- 独立 hub 灾备能力本轮未验证；未来需要时应提供独立故障域并重新立项。

## Acceptance

- 五个保留子任务均完成、提交、归档并具有测试或实机证据。
- 最终结论准确保留 P3-3 已接受风险和 P3-4 取消状态，不制造严格验收通过的假象。
- 全仓检查、常规构建、无 CGO 构建和 diff 门禁通过，P3 roadmap 状态完成同步。

## Next Step

- 完成统一 Check-All；通过后进入 Update-Spec 与提交阶段。
