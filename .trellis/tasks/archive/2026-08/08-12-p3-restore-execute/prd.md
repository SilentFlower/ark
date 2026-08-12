# P3-2 恢复执行

## Goal

执行 P3-1 生成的恢复 Plan，在清单内目标 host 上以严格串行、流式、可重跑且默认拒绝覆盖的
方式重建 Docker Compose 服务，并在任何破坏性动作前保留可回滚的当前状态。

## Requirements

### R1 命令与前置条件

- 扩展 `ark restore --host <source> [--to <destination>] [--snapshot latest|<manifest-id>]
  [--force] [--skip-doctor] [--json]`；不带 `--dry-run` 时执行真实恢复。
- source、destination、snapshot 与 Plan 语义完全复用 P3-1，不在执行路径重新解析一套规则。
- 获取 `/run/ark.lock` 非阻塞全局锁，与 backup 共用同一锁，避免恢复与备份并发。
- 默认运行 local doctor 与 destination host doctor；fail 在任何目标写操作前中止，`--skip-doctor`
  仅作为显式应急选项。
- 所有 preflight 完成后打印 Plan；JSON 模式 stdout 只输出最终结构化结果。

### R2 冲突、幂等与破坏前备份

- 默认检查 destination 的 Compose project 标签、同名 volume、容器与目标文件状态；存在会被覆盖的
  生产资源时 fail closed，不执行恢复。
- `--force` 只授权处理当前 Plan 精确声明的目标资源；不得删除或覆盖标签不属于目标 Project 的资源。
- destination 已存在正在运行或含数据的同项目资源时，`--force` 必须先调用复用后的 backup
  编排对 destination host 生成一份新 manifest；备份未全量成功则中止恢复。
- 重跑同一 Plan 时，已完整完成且可验证的步骤跳过；半完成或校验不一致的步骤明确失败或重做，
  不把“资源存在”直接当成成功。
- 不提供跨 target 并行，步骤与 target 均按 Plan 顺序串行。

### R3 执行顺序与数据流

- 固定顺序：files → image digest → volume → 启动数据库 → 导入数据库 → 启动应用 → health。
- files：`restic.Dump(snapshot, stable path)` 直接流入 destination 的 `tar -xpf -`，保留权限；
  hub 不落明文临时文件。
- image digest：只执行 `docker pull repo@sha256:...`，禁止回退到 compose tag。
- volume：先创建精确 volume，再通过临时 alpine 容器把 tar 流解包；恢复前后都校验 volume 标签/归属。
- PostgreSQL：只启动相关 service，轮询容器内 `pg_isready`，再用 `Runner.Feed` 把 SQL 流送入
  `docker compose exec -T ... psql`。
- Redis：先停止相关 service，把 RDB 流恢复到隔离临时 volume/文件并原子切换，再启动并等待
  `redis-cli PING`；不能向正在运行的 Redis 直接覆盖 `dump.rdb`。
- 应用：数据库 target 完成后执行完整 `docker compose up -d`；不通过固定 sleep 判断成功。
- 所有 `restic.Dump` 都必须读至 EOF或显式 Close，所有 `Runner.Feed` 非零退出、context 取消和
  close/wait 错误均导致当前恢复失败。

### R4 健康检查与结果

- 使用 `docker compose ps --format json` 核对目标 Project 的服务均处于 running；声明 healthcheck
  的服务还必须为 healthy。缺 healthcheck 只记录 warn，并列入人工确认清单。
- 恢复后核对运行容器的实际 image digest 与 Plan 完全一致；不一致为 fail。
- 人工确认清单固定包含 DNS 指向、TLS 证书、防火墙端口、`.env` 环境专属项与
  “请先暂停 dnsmgr 对该主机的检测”。
- 人类输出展示每阶段 ok/skipped/warn/fail；`--json` 只输出 snake_case 结果。
- 错误和持久化摘要只写阶段级脱敏信息，不包含 restic stderr、业务数据、`.env` 内容或凭证。

### R5 复用与测试边界

- 将 backup 的“指定 host 执行一次备份”编排提取为可复用内部边界，CLI 仍负责 cobra 与打印；
  不通过启动 `ark backup` 子进程实现破坏前备份。
- restore 使用现有 `restic.Repo`、`sshexec.Runner`、doctor 与 config 类型，不引入新的 shell、
  SSH 库、数据库驱动或通用 utils 包。
- 外部命令测试用 fake Runner/Repo 依赖注入精确断言 argv、顺序、Feed 流与失败传播。

## Non-Goals

- 不自动修改 DNS、TLS、防火墙、应用 `.env` 或 dnsmgr 状态。
- 不支持 target 子集恢复、跨项目重命名、未进清单的临时目标机或多项目主机。
- 不在本任务实现定期 verify 或 hub 自身灾备流程。

## Acceptance Criteria

- [ ] `ark restore` 严格按 Plan 顺序执行，五类 target 的流与 argv 有单元测试
- [ ] 默认冲突零写入；`--force` 只触碰精确 Project 资源并在破坏前完成成功备份
- [ ] 中途失败后重跑能跳过已验证步骤并继续，不把半完成状态误报成功
- [ ] PostgreSQL 等待 `pg_isready`，Redis 使用停机恢复与原子切换，均无固定 sleep
- [ ] restic/SSH/Feed/Close/health/digest 任一失败都可见且保留错误链
- [ ] 目标机不需要 ark、restic 或仓库凭证，hub 不落业务明文临时文件
- [ ] 人类与 JSON 输出完整、脱敏，人工确认清单包含五项既定内容
- [ ] `go test ./internal/restore ./internal/cli -race -count=1` 通过
- [ ] `make check`、`make build`、无 CGO 构建与 `git diff --check` 通过
