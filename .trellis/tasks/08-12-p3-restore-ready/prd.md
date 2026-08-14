# P3 恢复可用

## Goal

完成 P3 恢复计划、恢复执行、隔离恢复、跨主机验证与自动演练，使 ark 能从版本化备份
manifest 生成恢复计划、按安全顺序重建服务，并把同一恢复链用于自动隔离演练。

P3-4 hub 自身重建实测因没有独立干净机器、现有宿主机也没有可创建隔离 VM 的空间而取消。
该项不执行、不计为通过，也不再阻塞本轮 P3 收尾；未来若重新提出独立 hub 灾备验收，应新建任务。

P3-3 已在非干净 destination 上完成隔离跨主机恢复、source independence、数据、镜像与清理验证，
但严格干净 VPS 与真实生产业务验收仍未满足。用户已明确接受 `CHK-001` 与 `FBK-001` 风险；
父任务必须保留这一限制，不能把风险接受改写为严格验收通过。

## Requirements

### R1 子任务边界

- `p3-restore-plan`：从最新备份 manifest 与当前 `ark.yaml` 生成纯数据 `Plan`，提供
  `ark restore --dry-run` 的人类可读与 JSON 输出。
- `p3-restore-execute`：执行 `Plan`，实现幂等恢复、冲突保护、破坏前备份、跨主机目标、
  流完整性、健康检查和人工确认清单。
- `p3-isolated-restore`：为 restore 提供显式隔离模式，自动派生资源名、路径和空闲端口，并提供
  可校验的幂等 cleanup，作为实机并行恢复和 verify 的共同基础。
- `p3-live-rebuild-validation`：完成受限隔离跨主机重建，验证数据、应用、加密字段、镜像 digest
  与源主机独立性；严格干净 VPS 缺口以 `CHK-001` 保留为已接受风险。
- `p3-verify`：把已完成受限实机验收的恢复链复用于隔离演练，完成健康检查、销毁与
  `store.Verification` 记录。

### R2 依赖顺序

- `p3-restore-execute` 依赖 `p3-restore-plan`。
- `p3-live-rebuild-validation` 依赖 `p3-restore-execute`；目标机不干净时还依赖
  `p3-isolated-restore` 完成并先验收隔离恢复。
- `p3-verify` 依赖 `p3-restore-execute`、`p3-isolated-restore` 和 `p3-live-rebuild-validation`；实机暴露的恢复缺口必须
  先回写设计并修复，再固化成自动演练。
- 父任务只在五个子任务全部完成后做集成复核，不直接承载业务代码修复。

### R3 Auto-loop 边界

- 第一波 auto-loop 只包含 `p3-restore-plan` 与 `p3-restore-execute`，profile 固定为
  `commit-only`，并显式声明 Execute 依赖 Plan。
- `p3-live-rebuild-validation` 涉及真实服务器、离线凭证和外部系统，不进入 auto-loop。
- 第二波 auto-loop 只在跨主机实机验收通过后运行 `p3-verify`。
- `ark verify` 第一版固定在原 host 的完全隔离 Compose 项目中每周执行一次；有条件时再迁移到
  专用验证 host，迁移不得削弱既有隔离检查。
- auto-loop 不 push、不归档、不部署、不执行真实服务器恢复，也不修改 DNS、TLS、
  防火墙、对象存储控制台或离线密钥。

### R4 集成约束

- 恢复事实以备份 manifest 为准，执行参数以当前 `ark.yaml` 的 source/target host 定义为准；
  两者无法按 target ID/type 无歧义匹配时 fail closed。
- 目标机保持 agentless，只要求 Docker、Compose v2 与 sshd；ark、restic 和仓库凭证只在 hub。
- 全部恢复数据保持 `restic dump -> Runner.Feed` 流式传输，不在 hub 落业务明文临时文件。
- 镜像只按 manifest 记录的 digest 拉取；数据库恢复使用容器内 CLI，不引入业务数据库驱动。
- 任何冲突、截断、健康失败、清理失败或结果未持久化都必须显式失败，不能输出成功假象。

## Non-Goals

- 不实现 P4 Web/API、P5 dnsmgr 自动联动或 P6 长期优化。
- 不支持未加入 `ark.yaml` 的临时 SSH 参数；P3 MVP 的 `--to` 只接受清单内 host。
- 不由父任务直接修改业务代码，也不由 auto-loop执行真实恢复或 hub 灾备操作。
- 不在 P3 自动修改生产 DNS、TLS 证书、防火墙或 `.env` 中的环境专属值。

## Acceptance Criteria

- [x] P3-1 与 P3-2 完成第一波 auto-loop、Check-All、规范更新和提交
- [x] P3-3 完成受限隔离跨主机重建，source independence、数据、镜像与清理验证通过；
  严格干净 VPS 与真实生产业务缺口作为已接受的 `CHK-001` 保留，不标记为严格通过
- [x] P3-4 经用户确认取消；不执行、不计为通过，且不作为本轮 P3 完成条件
- [x] P3-5 在确认的隔离位置完成自动演练，生产资源无变化，失败快照能被识别
- [x] 五个子任务均完成、提交、归档并保留可审计的测试或实机证据
- [x] 最终集成结论显式保留 `CHK-001` 与 `FBK-001`，不把风险接受写成问题已修复
- [x] 最终 `make check`、`make build`、`CGO_ENABLED=0 go build ./cmd/ark` 与
  `git diff --check` 全绿
- [x] `docs/roadmap.md` 准确记录 P3 已完成项、受限验收项与取消项，不把 P3-4 标记为通过
