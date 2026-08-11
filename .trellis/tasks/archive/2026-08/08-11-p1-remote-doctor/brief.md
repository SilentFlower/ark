# Brief — P1-3 远程 doctor

## Goal

将现有 `doctor.Run` 拆分为 hub 本地检查与单 host 环境检查，并通过
`sshexec.Runner` 统一执行本地或 SSH 目标机探测。CLI 默认检查 hub，支持
`--host <name>` 检查指定 host，以及 `--all` 按清单顺序完成全范围体检。

## Scope

- 新增 `RunLocal(ctx, cfg)`：检查 hub 上的 restic、ssh、systemd-analyze、仓库文件、
  SSH 文件、schedule，并通过 `restic cat config` 验证仓库可达和可解锁。
- 新增 `RunHost(ctx, cfg, host)`：检查连接、时钟偏移、docker、compose、项目文件、
  compose services，以及 service、volume、files path、image service 等 target 前提。
- 本地 host 使用 `sshexec.NewLocal()`，远程 host 使用 `sshexec.NewSSH()`；目标机命令
  全部以独立 argv 通过 Runner 执行。
- 本地与远程文件检查归一化为共享元数据，并复用存在性、文件类型和权限判定。
- 按依赖关系实现 fail / warn 降级，单项失败不阻断仍可独立判断的检查。
- doctor CLI 新增互斥的 `--host` / `--all`，合并报告并保持 JSON、文本摘要和退出码兼容。
- 补齐 doctor、CLI 与可选真实 host 集成测试，并同步 README、示例注释和 roadmap。

## Non-Goals

- 不实现备份、恢复、仓库初始化或完整的 `internal/restic` 封装。
- 不并行检查 host，不增加 SSH 重试、自动修复或交互式 SSH 能力。
- 不修改清单 schema，不增加可配置的时钟偏差阈值。
- 不检查目标机上的 restic、ark 或 systemd。

## Key Decisions

- `ark doctor` 默认只调用 `RunLocal`；`--all` 先检查 hub，再按 `cfg.Hosts` 顺序逐台检查。
- local host 也走 `RunHost`，仅 Runner 类型不同，避免本地与远程形成两套业务逻辑。
- SSH 登录失败时 connection 为 fail，依赖目标机状态的后续项为 warn；docker 或 compose
  失败时只降级其依赖项，项目文件与 files target 等独立检查继续执行。
- hub 与目标机绝对时钟偏移超过固定 60 秒时报告 warn；使用命令前后本地时间中点降低
  SSH 往返延迟影响。
- `repo.env_file` 使用严格、无 shell 展开的最小解析器；凭证只注入该次 restic 子进程，
  清单中的 repository 与 password file 最终强制覆盖同名环境变量。
- 删除旧 `doctor.Run`，不保留过渡包装；`Report` JSON 结构和退出码 0/1/2 语义不变。

## Key Context

- 主要实现范围：`internal/doctor/doctor.go`、新增 `internal/doctor/doctor_remote.go` 及测试、
  `internal/cli/root.go` 及测试、README、`examples/ark.yaml`、`docs/roadmap.md`。
- 实现必须使用现有 `config.Config`、`config.Host`、`config.SSH`、`config.Repo`、
  `config.Project`、`config.Target` 的真实字段，以及 `sshexec.Runner.Run`、
  `sshexec.NewLocal`、`sshexec.NewSSH` 的现有签名。
- 每条外部探测命令使用 15 秒超时；清单中的路径、项目名、service 和 volume 始终作为
  独立 argv 传入 Runner，不在 doctor 中拼接 SSH 或 shell 命令。
- 导出类、接口和公共方法必须补充中文 Javadoc/Docstring 风格注释；复杂依赖降级逻辑需
  注释说明业务原因。

## Risks / Deferred

- repo env parser 本轮服务于 doctor；P2-2 建立 `internal/restic` 时应复用或迁移，避免
  产生两套凭证解析规则。
- `stat -c` 与 `date +%s` 假设目标机符合 roadmap 规定的 Linux 环境。
- `--all` 串行执行可能较慢；并行化延后到有真实规模数据后评估。
- 默认 doctor 从过渡期的混合检查改为仅检查 hub，这是 ADR-008 与 roadmap 要求的有意变更。

## Acceptance

- `RunLocal` 与 `RunHost` 完全替代旧 `Run`，且职责边界符合 ADR-008。
- hub 二进制、敏感文件权限、schedule 与 `restic cat config` 均有真实检查，凭证不泄露且
  仅进入 restic 子进程。
- local 与 SSH host 共用 Runner 驱动的检查流程，以及一致的文件元数据判定。
- 登录、docker、compose、项目文件、service、volume、path、image service 与时钟检查
  符合 PRD 的 fail / warn 依赖矩阵；`--all` 在单台失败后继续。
- CLI 默认 local，支持互斥的 `--host` / `--all`，未知 host 返回退出码 1，检查失败保持
  退出码 2，JSON 与文本输出兼容。
- 测试覆盖高风险参数、依赖降级、60 秒边界与调用顺序；真实外部依赖测试可安全跳过。
- README、示例注释和 roadmap 同步完成，`make check` 全绿且不新增 Go 模块依赖。

## Next Step

任务已经激活并完成 RunLocal、RunHost、CLI、文档和自动化测试实现。
Full Check-All 已通过；下一步进入规范更新与提交规划阶段。
