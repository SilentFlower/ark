# Directory Structure

> ark 的包划分与文件归属。

---

## Overview

ark 是一个单仓库 Go 项目，产出两个二进制：

- `ark`：装在 hub 上的编排 CLI，由 systemd timer 触发，跑完即退出。
  通过 SSH 驱动所有目标机，目标机上不装它（ADR-002）。
- `ark-hub`：hub 上常驻的界面与 API（P4 阶段才开始写）。它不承载调度，
  需要执行长任务时起一个 `ark` 子进程（ADR-005）。

包划分遵循一条主线：**按「职责边界」分包，而不是按「技术分层」分包**。
判断一个新包是否成立，问的是「它能不能独立解释自己在解决什么问题」，
而不是「它属于 service 层还是 util 层」。项目里没有 `utils/`、`common/`、
`helpers/` 这类按技术角色命名的包，也不要新增。

---

## Directory Layout

关键入口与基础包：

```
cmd/
├── ark/main.go              oneshot CLI 薄入口
└── ark-hub/main.go          常驻 Hub 薄入口
internal/
├── cli/root.go              cobra 命令组装、退出码语义、人类可读输出
├── config/                  备份清单的数据模型 + 静态校验
│   ├── config.go
│   └── config_test.go
├── doctor/doctor.go         运行环境校验（外部命令、文件权限、compose 资源）
├── envfile/                 受限 KEY=VALUE 文件解析，不回显变量值
├── monitoring/              监控秘密加载、钉钉与外部心跳 HTTP 边界
├── schedule/                systemd OnCalendar 的结构化下一次触发与有效周期
├── store/                   SQLite 状态、查询 DTO、迁移和在线一致性导出
└── hub/                     鉴权、HTTP DTO、健康投影和 Ark 子进程生命周期
    └── webui/               go:embed 前端产物 + 静态资源与 SPA fallback
web/                         Vue 3 控制台源码（规约见 .trellis/spec/frontend/）
docs/
├── design.md                架构与 13 条 ADR
└── roadmap.md               分阶段任务，含接口草案与验收标准
examples/
└── ark.yaml                 清单模板（真实清单是 /etc/ark/ark.yaml，不入库）
```

职责归属（扩展现有能力时按此归位，不要另起同义包）：

| 包 | 职责 | 阶段 |
|---|---|---|
| `internal/sshexec/` | SSH / 本地命令执行层，上层执行器不区分远近 | P1-2 |
| `internal/hostkey/` | SSH 主机密钥扫描、指纹生成与 known_hosts 原子刷新 | P2-8 |
| `internal/store/` | SQLite 状态库（`/var/lib/ark/ark.db`） | P2-1 |
| `internal/restic/` | restic CLI 的 Go 封装 | P2-2 |
| `internal/backup/` | 各 target 执行器 + 流完整性 + 快照清单 | P2-3 / P2-4 / P2-5 |
| `internal/systemd/` | systemd unit 模板 | P2-6 |
| `internal/schedule/` | 委托 `systemd-analyze calendar` 解析 next run 与有效周期 | P4-2 |
| `internal/restore/` | 恢复计划与执行 | P3-1 / P3-2 |
| `internal/verify/` | 原 host 隔离恢复演练、生产基线与结果持久化 | P3-5 |
| `internal/hub/` + `cmd/ark-hub/` | hub 后端（界面与 API） | P4-1 / P4-2 |
| `internal/hub/webui/` | go:embed 前端产物、静态资源与 SPA fallback | P4-3 |
| `web/` | Vue 3 前端，构建产物 go:embed 进 ark-hub | P4-3 |
| `internal/monitoring/` | root-only 秘密文件、安全 URL、钉钉 Markdown 与成功/失败双端点心跳 | P4-4 |

早期规划过的 `internal/status/` **已取消**：hub 自己就是执行者，
状态直接落本地 SQLite，不再需要把 `_status/<host>.json` 推到对象存储（design.md §9）。

---

## Module Organization

### 一切实现都在 internal/

除 `cmd/` 外所有代码都放在 `internal/` 下。ark 是应用不是库，
没有对外 API 承诺；放进 `internal/` 意味着任何包都可以随时重构，
不必担心仓库外的引用。目前没有理由新增顶层公开包。

### cmd/ 保持极薄

`cmd/ark/main.go` 只有 15 行，全部内容是调用 `cli.Execute()` 并把返回的
int 交给 `os.Exit`。命令定义、flag 绑定、输出格式一律在 `internal/cli` 里。
新增二进制时照此办理：`cmd/` 下不写业务逻辑，不写 flag 解析。

### 依赖方向是单向的

```
cli  ──►  doctor  ──►  config
 ├────►  hostkey
 ├────►  backup / restore / verify
 ├────►  monitoring ──► envfile
 └────►  sshexec ──► config

doctor ──► monitoring

hub  ──►  config / schedule / store / monitoring
 ├────►  hub/webui
 └─exec─► ark --json
```

`config` 不认识任何其他内部包；`hostkey` 只依赖标准库和 OpenSSH 工具；
`doctor` 依赖 `config` 与 `monitoring`，`sshexec` 依赖 `config`，`cli` 负责编排业务边界。`schedule` 只负责
systemd calendar 结构化解析，可被 doctor、CLI 与 Hub 复用；`monitoring` 只依赖 `envfile`
和标准库，不理解 Hub、Store 或 backup 终态；`store` 不反向依赖业务包。
`hub` 可以依赖 config、schedule、store 和 monitoring，但不得导入 backup、restore、verify、restic 或 sshexec；
长任务跨进程调用 `ark --json`，避免 Hub 成为第二套业务编排器。
`hub/webui` 是依赖链的末端：它只依赖标准库，不认识 config、store 或任何业务概念，
因此可以独立验证「静态资源服务是否正确」而不必造一份清单或状态库。
新增包时保持这个方向：**越靠近数据模型的包依赖越少**。
如果发现需要反向依赖（比如 `config` 想调用 `doctor`），说明职责切错了，
应该重新划边界而不是加接口绕过去。

### 校验分两层，对应两个包

这是 ADR-008 落在代码结构上的直接结果，也是最容易被新代码破坏的约定：

- `internal/config` 只做静态语义校验，**全程不碰文件系统、不碰 docker、不碰网络**。
  因此 hub 可以校验任意一台机器的清单段落，不需要连上那台机器。
- `internal/doctor` 做运行环境校验——文件是否存在、权限是否安全、
  compose 里是否真有这个 service、volume 是否真的存在。

往 `config.Validate` 里加一个 `os.Stat` 就会破坏这条边界。
需要检查真实环境时，加到 `doctor` 里。

---

## Naming Conventions

- 包名用单数小写名词，与目录名一致：`config`、`doctor`、`restic`。
  不用复数，不用下划线，不用驼峰。
- 文件名用小写加下划线：`config.go`、`config_test.go`、`repo.go`。
- 单文件包直接用包名做文件名（`doctor/doctor.go`）；
  包内容超过一个明确子职责时才拆文件（如 roadmap 规划的
  `backup/manifest.go` 从 `backup/` 中独立出来）。
- 构造函数用 `New`（`restic.New`），返回具体类型而非接口。
  ark 目前没有需要 mock 的接口边界，不要提前抽象。
- cobra 子命令的构造函数统一叫 `newXxxCmd`，见 `internal/cli/root.go:73`
  起的 `newVersionCmd` / `newValidateCmd` / `newDoctorCmd`。

---

## Examples

想了解本项目的组织习惯，按顺序读这三个文件：

- `internal/config/config.go` — 数据模型 + 分层校验的范例。
  注意 package doc 注释（第 1-12 行）如何交代包的职责边界，
  以及 `Validate` 如何用 `errors.Join` 一次性收集全部问题。
- `internal/doctor/doctor.go` — 与外部世界交互的范例。
  注意检查之间的依赖处理（第 91-128 行）：docker 不可用时，
  依赖它的检查降级为 warn 而不是伪造成 fail。
- `internal/cli/root.go` — 命令组装的范例。
  注意退出码语义（第 35-47 行）和输出一律走 `cmd.OutOrStdout()`。
