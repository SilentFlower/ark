# P1-3 远程 doctor

## Goal

把现有只能完整检查 hub 本机的 `doctor.Run` 拆成 hub 本地检查和单台 host
环境检查。用户可以用 `ark doctor` 检查 hub、用 `--host <name>` 检查指定机器、
用 `--all` 顺序检查全部范围；远程 host 通过 P1-2 的 `sshexec.Runner` 执行，
确保清单里声明的运行前提在备份前可以被真实验证。

来源：`docs/roadmap.md` §P1-3，以及 P1-4 中明确留到 P1-3 一起完成的
`doctor --host` / `--all` CLI 标志。

## Background

当前 `internal/doctor.Run` 同时检查 hub 二进制、仓库凭证、本地 compose 和 target，
但远程 host 只检查 hub 上的 SSH 文件，目标机的 docker、compose、service、volume
和路径仍统一报告 warn。P1-2 已提供本地与系统 OpenSSH 共用的
`sshexec.Runner`，本任务负责让 doctor 真正消费该执行层。

ADR-008 规定三层校验边界：`validate` 只做静态校验，`doctor --local` 检查 hub，
`doctor --host` 检查目标机。现有三态语义继续保持：确定有问题为 fail，
依赖失败导致无法判断为 warn，不能把未检查伪装成通过或失败。

## Requirements

### R1 拆分公共入口

`internal/doctor` 导出：

```go
func RunLocal(ctx context.Context, cfg *config.Config) *Report
func RunHost(ctx context.Context, cfg *config.Config, host *config.Host) *Report
```

- `RunLocal` 只检查 hub 与清单在 hub 上的依赖。
- `RunHost` 检查一台清单 host 的项目与 target 环境；`local: true` 使用
  `sshexec.NewLocal()`，远程 host 使用 `sshexec.NewSSH(*host.SSH)`。
- 上层可以把多个 `Report` 按执行顺序合并，但单台 host 的检查不依赖 CLI。
- 移除旧 `Run` 的过渡行为和“远程检查待就位”说明，不保留两个事实来源。

### R2 RunLocal 检查 hub

`RunLocal` 必须检查：

- `restic`、`ssh`、`systemd-analyze` 存在且可执行；
- `repo.password_file` 存在、不是目录且权限不宽于 `0600`；
- 可选 `repo.env_file` 存在、不是目录且权限不宽于 `0600`；
- 每个远程 host 的 `ssh.identity_file` 权限不宽于 `0600`，
  `ssh.known_hosts_file` 存在且不是目录；
- 每个 host 的生效 `schedule.on_calendar` 由 `systemd-analyze calendar` 校验；
- 使用清单仓库 URL、密码文件和对象存储凭证执行 `restic cat config`，验证对象存储
  可达且仓库可以解锁。

读取 `repo.env_file` 后，凭证只能追加到该次 restic 子进程的环境；不得调用
`os.Setenv`，不得把文件内容、完整环境或密码写入错误和报告。

### R3 RunHost 检查本地或远程 host

`RunHost` 统一通过 `sshexec.Runner` 执行目标环境命令，并检查：

- 远程 host 能建立 SSH 会话；连接失败时该检查为 fail，所有依赖目标机状态的
  后续检查降级为 warn；
- `docker` 与 compose v2 可用；docker 不可用时 compose 和 target 检查为 warn；
- `project.compose_file` 存在且是普通文件；可选 `project.env_file` 存在、
  是普通文件且权限不宽于 `0600`；
- 使用清单的 compose file、project name 和 env file 执行
  `docker compose config --services`；
- postgres / redis target 引用的 service 已定义；
- volume target 引用的 docker volume 存在；
- files target 的每条路径都存在；
- image_digest target 引用的 service 均已定义；
- hub 与目标机时钟的绝对偏移超过 60 秒时产生 warn，不因此阻断其它检查。

本地和远程文件检查必须共用同一套“存在、普通文件、权限位”判定，远程元数据通过
`stat` 获取，不能形成“本地严格、远程宽松”的两套规则。

### R4 依赖失败的三态语义

- SSH 登录失败：登录项 fail，docker、compose、文件、target、时钟检查 warn。
- docker 不可用：docker 项 fail，compose 与依赖 docker 的 target 检查 warn；
  不依赖 docker 的项目文件和 files target 仍应继续检查。
- compose 文件或 `config --services` 失败：对应项目/compose 项 fail，
  依赖服务列表的 target 检查 warn；volume 和 files target 仍独立检查。
- target 指向确定不存在的 service、volume 或 path：该 target 为 fail。
- 单个检查失败不中断同一 host 中仍可独立判断的其它检查。

### R5 CLI 范围选择与兼容性

`internal/cli/root.go` 的 `doctor` 命令新增：

- 无范围标志：等价于 `--local`，只调用 `RunLocal`；
- `--host <name>`：只调用指定 host 的 `RunHost`，host 可以是 local 或 SSH；
- `--all`：先调用 `RunLocal`，再按清单顺序调用每个 host 的 `RunHost`；
- `--host` 与 `--all` 互斥；未知 host 返回工具错误，退出码为 1；
- 任一已执行报告存在 fail 时仍返回 `errChecksFailed`，退出码为 2；
- `--json` 对合并后的单份 `Report` 保持现有 JSON 结构，人类可读输出和摘要保持兼容。

README 的 doctor 用法和 `examples/ark.yaml` 顶部操作示例同步新增范围标志；
清单 schema 与示例数据结构不变。`docs/roadmap.md` 同步标记对应 CLI 项完成并删除过渡说明。

### R6 命令生命周期与错误边界

- 每条 doctor 探测命令继续使用 15 秒超时；同一次 `--all` 中单台 host 超时
  不阻止后续 host 检查。
- 目标机命令必须使用 `sshexec.Runner.Run`，不在 doctor 中重新拼 SSH 命令或
  shell 字符串。
- 清单中的 compose 路径、项目名、service、volume 和 files path 始终作为独立 argv
  交给 Runner，由 SSH 执行层负责逐参数转义。
- 错误信息带 host 和检查项上下文，但不得泄露密钥、env 文件内容或完整环境。

### R7 测试覆盖

- `RunLocal` 覆盖二进制、密钥权限、schedule、restic 解锁成功与失败，
  并断言对象存储环境只进入 restic 子进程。
- `RunHost` 使用可控假 Runner 覆盖 local/SSH 选择、登录失败降级、docker/compose
  依赖、文件权限、service、volume、path 和时钟偏移。
- 测试证明 compose/target 中的 shell 元字符仍按独立 argv 传给 Runner。
- CLI 覆盖默认 local、`--host`、`--all`、互斥标志、未知 host、JSON 合并输出
  和退出码语义。
- 提供显式环境配置的真实 host 集成测试或等价手工验证入口；`testing.Short()`
  及缺少专用配置时跳过，`make check` 不依赖真实 SSH 或 docker 环境。

## Non-Goals

- 不实现备份执行、恢复执行、restic 仓库初始化或 P2-2 的完整 `internal/restic` 封装。
- 不并行检查 host；`--all` 本轮按清单顺序串行，确保输出稳定且避免同时压目标机。
- 不新增清单字段或 NTP 阈值配置；60 秒是本轮固定常量。
- 不实现 SSH 重试或自动修复。
- 不检查目标机上的 restic、ark 或 systemd；agentless 目标机只需要 docker 和 sshd。
- 不新增跳过 known_hosts 校验、密码登录或交互式 SSH 能力。

## Acceptance Criteria

- [ ] `RunLocal` 与 `RunHost` 替代旧 `Run`，职责边界符合 ADR-008
- [ ] `RunLocal` 能发现缺失的 restic/ssh/systemd、过宽密钥权限和无法解锁的仓库
- [ ] restic 凭证只注入单个子进程，错误与输出不泄露 env 文件或密钥内容
- [ ] `RunHost` 对 local 与 SSH host 使用同一套检查逻辑和 `sshexec.Runner`
- [ ] SSH 登录失败时登录项 fail、后续依赖项 warn，且 `--all` 继续检查下一台 host
- [ ] docker、compose、项目文件、service、volume、files path 和 image service 均有真实检查
- [ ] 本地与远程文件存在性、文件类型和权限判定一致
- [ ] 时钟绝对偏移超过 60 秒时报告 warn，不阻断其它检查
- [ ] `doctor` 默认 local，支持 `--host <name>` 与 `--all`，互斥和未知 host 错误可读
- [ ] JSON 结构、文本摘要和退出码 0/1/2 语义保持兼容
- [ ] README、示例注释和 roadmap 同步 doctor 新用法，清单 schema 不变
- [ ] 单元测试覆盖依赖降级和高风险参数边界，真实外部依赖测试可安全跳过
- [ ] `make check` 全绿，不新增 Go 模块依赖
