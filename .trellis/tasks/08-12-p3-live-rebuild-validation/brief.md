# Brief — P3-3 跨主机重建实测

## Goal

- 在只安装 Docker、Compose v2 与 sshd 的干净 VPS 上完成真实跨主机恢复，证明重建不依赖源主机。

## Scope

- 建立授权测试机基线、运行 dry-run 和真实 restore、执行一次可恢复中断并验证幂等续跑。
- 核对数据库、应用读写、加密字段、Redis、volume、files、镜像 digest 与 agentless 条件。
- 将脱敏证据写入 `validation.md`，缺陷回流代码任务修复后复验。

## Non-Goals

- 不验证 hub 灾备或 `ark verify`，不修改生产 DNS/TLS/dnsmgr。
- 不在未授权生产资源上覆盖、故障注入或清理。

## Key Decisions

- 目标 VPS 使用清单内临时 destination host；恢复数据只来自 hub 与对象存储。
- 源主机只作结果基准，不作为文件或数据来源；需要旧机材料即判备份范围失败。

## Key Context

- 用户已授权在备用 destination 执行隔离恢复、可恢复中断和归属清理；相关实机操作均已完成。
- 凭证、真实 IP 和业务数据不写任务文件、Git 或公开输出。

## Risks / Deferred

- 备用 destination 不是只预装 Docker、Compose v2 与 sshd 的干净 VPS，因此严格 P3-3 验收仍未满足。
- 本轮恢复对象是专用验证栈；隔离恢复、幂等、source independence 和数据链路已验证，但不能替代
  roadmap 要求的真实业务登录与业务数据验收。

## Acceptance

- 干净 VPS 完整恢复且源机零依赖，业务、加密字段和全部 digest 验证通过。
- 目标机无 ark/restic/仓库凭证，中断后重跑安全继续。
- `validation.md` 完整脱敏，发现的缺陷已修复并复验。

## Next Step

- 保留受限隔离验收证据；严格完成 P3-3 仍需在授权干净 VPS 上恢复真实业务栈并完成业务验收。
