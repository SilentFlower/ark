# P3-3 跨主机重建实测

## Goal

在授权的干净 VPS 上仅依赖 hub、对象存储凭证、restic 密码与目标机 SSH 连接，完成一次
可审计的跨主机完整恢复，证明备份材料足以脱离源主机重建真实业务。

## Requirements

### R1 前置条件与隔离

- 目标 VPS 必须是明确授权的非生产新机，只预装 Docker、Compose v2 与 sshd；不得预装
  ark、restic、业务数据或从源主机复制的文件。
- 将目标 VPS 作为新的 destination host 加入临时清单，使用独立 SSH identity/known_hosts；
  凭证与真实 IP 不写入任务文件、Git 或公开输出。
- 恢复前记录目标机容器、volume、文件、端口、DNS/IP 与磁盘基线，确认没有同名生产资源。
- 生产 DNS、TLS、dnsmgr 和源主机保持不变；验收流量只走临时地址或本地端口转发。
- 用户必须在执行前明确授权对该测试 VPS 创建、启动、停止和清理隔离资源。

### R2 执行流程

- 从 hub 运行 latest manifest 的 `ark restore --host <source> --to <destination> --dry-run`，
  人工复核 Plan 的 source、destination、targets、snapshots、digest 与路径。
- 执行真实 restore；记录每阶段开始/结束、结果、snapshot ID、目标资源和脱敏错误。
- 中途至少执行一次可恢复的中断测试，随后重跑同一命令，验证幂等续跑与零重复破坏。
- 如果目标状态需要 `--force`，先证明 safety backup 成功；干净 VPS 正常路径不应需要 force。
- 完成后逐项执行数据、应用、镜像和 agentless 验证；不得以“容器 running”替代业务检查。

### R3 必验业务点

- PostgreSQL：抽查约定的关键表行数、最新记录时间与代表性关联数据。
- 应用：使用测试账号完成登录、读取与一次可回滚写入，再确认写入可读。
- 加密字段：读取至少一项需要 `.env` 密钥解密的数据，证明密钥材料被正确恢复。
- Redis/volume/files：核对代表性 key、上传文件/静态资源、compose、`.env` 与代理配置。
- 镜像：对每个记录 service 核对运行容器实际 RepoDigest 与 Plan/manifest 一致。
- agentless：目标机不存在 ark/restic 二进制、仓库密码和对象存储凭证；所有 dump 由 hub 提供。
- source independence：验收期间可关闭源主机业务访问通道或在网络层证明没有读取源主机；任何
  “还需从旧机复制”的材料都记为备份范围缺陷并判当前验收失败。

### R4 证据、缺陷与清理

- 在任务目录新增 `validation.md`，记录脱敏环境、命令类别、结果、快照/manifest ID、差异和清理。
- 实机发现的代码缺陷回到 `p3-restore-plan`、`p3-restore-execute` 或新建缺陷任务修复；本任务只
  记录现象、复验和最终结论，不在操作记录中临时改业务代码。
- 实机发现的备份范围或恢复顺序缺口必须写回 `docs/design.md` 和对应可执行 spec，再重新备份、
  恢复与复验。
- 测试写入回滚，临时 DNS/端口转发撤销，测试 VPS 的保留或销毁由用户显式决定。

## Non-Goals

- 不验证 hub 自身灾备，留给 `p3-hub-rebuild-validation`。
- 不实现或验收 `ark verify`，不自动修改生产 DNS/TLS/防火墙/dnsmgr。
- 不在未授权生产资源上做故障注入或覆盖恢复。

## Acceptance Criteria

- [ ] 干净 VPS 仅凭 hub 侧材料完成完整恢复，源主机零依赖
- [ ] dry-run Plan 与真实执行目标一致，默认路径无隐式覆盖
- [ ] 中断后重跑能够安全继续，已完成步骤不重复破坏
- [ ] 业务表、最新记录、登录、读写和加密字段验证通过
- [ ] Redis/volume/files 与全部运行镜像 digest 验证通过
- [ ] 目标机没有 ark、restic、仓库密码或对象存储凭证
- [ ] 人工确认清单已逐项处理或明确记录为未上线事项
- [ ] `validation.md` 证据完整、脱敏，发现的缺陷完成修复与复验
