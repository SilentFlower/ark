# Directory Structure

> ark 的包划分与文件归属。

---

## Overview

ark 是一个单仓库 Go 项目，产出两个二进制：

- `ark`：装在每台被备份机器上的 agent，由 systemd timer 触发，跑完即退出。
- `ark-hub`：中心机上的只读观测面（P3 阶段才开始写）。

包划分遵循一条主线：**按「职责边界」分包，而不是按「技术分层」分包**。
判断一个新包是否成立，问的是「它能不能独立解释自己在解决什么问题」，
而不是「它属于 service 层还是 util 层」。项目里没有 `utils/`、`common/`、
`helpers/` 这类按技术角色命名的包，也不要新增。

---

## Directory Layout

当前已存在的部分：

```
cmd/
└── ark/main.go              入口，只做一件事：os.Exit(cli.Execute())
internal/
├── cli/root.go              cobra 命令组装、退出码语义、人类可读输出
├── config/                  备份清单的数据模型 + 静态校验
│   ├── config.go
│   └── config_test.go
└── doctor/doctor.go         运行环境校验（外部命令、文件权限、compose 资源）
docs/
├── design.md                架构与 8 条 ADR
└── roadmap.md               分阶段任务，含接口草案与验收标准
examples/
└── ark.yaml                 清单模板（真实清单是 /etc/ark/ark.yaml，不入库）
```

roadmap 已经规划、但尚未创建的包（新建时按此归位，不要另起名字）：

| 包 | 职责 | 阶段 |
|---|---|---|
| `internal/restic/` | restic CLI 的 Go 封装 | P1-1 |
| `internal/backup/` | 各 target 类型的执行器 + 快照清单 | P1-2 / P1-3 |
| `internal/status/` | 状态文件生成与上报 | P1-5 |
| `internal/restore/` | 恢复计划与执行 | P2-1 / P2-2 |
| `internal/hub/` + `cmd/ark-hub/` | 中心机后端 | P3-1 |
| `web/` | Vue 3 前端，构建产物 go:embed 进 ark-hub | P3-2 / P3-3 |

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
 └──────────────────────►
```

`config` 不认识任何其他内部包，`doctor` 只依赖 `config`，`cli` 依赖两者。
新增包时保持这个方向：**越靠近数据模型的包依赖越少**。
如果发现需要反向依赖（比如 `config` 想调用 `doctor`），说明职责切错了，
应该重新划边界而不是加接口绕过去。

### 校验分两层，对应两个包

这是 ADR-008 落在代码结构上的直接结果，也是最容易被新代码破坏的约定：

- `internal/config` 只做静态语义校验，**全程不碰文件系统、不碰 docker、不碰网络**。
  因此中心机可以校验任意一台机器的清单副本。
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
