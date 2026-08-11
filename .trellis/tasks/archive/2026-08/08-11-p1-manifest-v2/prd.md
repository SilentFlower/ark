# P1-1 清单模型升级到多机（v2）

## Goal

把 `ark.yaml` 从「一台机器一份清单」改造为「hub 一份清单管所有机器」的 v2 模型，
并让 `ark validate` 能校验这份多机清单。

来源：`docs/roadmap.md` §P1-1，以及 §P1-4 中与 `validate` / `examples` 相关的部分。
架构依据：`docs/design.md` ADR-002（hub 集中编排、目标机 agentless）、
ADR-009（所有机器共用一个 restic 仓库，按 tag 区分）。

## Background

P0 的清单模型假设「每台机器上装一个 agent，各自读本机的 ark.yaml」，
因此顶层直接是 `host` / `project` / `targets`，`repo.url` 末尾带 `/<host>` 做隔离。

架构改为 hub 集中编排后，这三条假设全部失效：

- 清单只存在于 hub 上，一份要描述 N 台机器；
- hub 需要知道怎么连到每台机器（SSH 参数），而这在旧模型里无处安放；
- 仓库全局唯一，按 restic tag 区分 host，URL 不再按机器分路径。

## Requirements

### R1 顶层结构改为多机

```
version: 2
repo:      全局唯一仓库（沿用现有 Repo 定义，URL 不再带 /<host>）
defaults:  schedule + retention 的全局默认值
hosts:     []Host
```

`Host` 包含：`host`、`local`、`ssh`、`project`、`targets`、`schedule`、`retention`。
`Project` / `Target` / `Schedule` / `Retention` 的定义原样沿用，
它们描述「备份什么」，与「谁来执行」无关。

### R2 新增 SSH 连接信息

`SSH{ Address, User, IdentityFile, KnownHostsFile }`。

- `known_hosts_file` **必填**，且不提供任何「跳过主机密钥校验」的开关。
  hub 会把生产数据流经这条连接，中间人劫持同时意味着数据泄露和恢复投毒。
- `identity_file` / `known_hosts_file` 必须是绝对路径（清单由 systemd 拉起的进程读取，
  相对路径的解析基准不可控）。
- 文件是否存在、权限是否 0600 属于 `doctor` 的职责，`Validate` 不碰文件系统。

### R3 `local` 与 `ssh` 互斥

`local: true` 表示这台就是 hub 自己，命令直接本地执行。
此时 `ssh` 段必须为空；`local: false`（或未写）时 `ssh` 必填。
两者并存或两者皆空都要报错。

### R4 host 名称全局唯一

`host` 是 restic tag、也是恢复时的检索键。重名会让两台机器的快照混在一起，
且恢复时无法区分。重复必须报错，并指出与哪一条冲突。

`hostPattern` 沿用现有定义（小写字母 / 数字 / 中划线，不以中划线开头结尾）。

### R5 默认值继承用指针区分「没写」和「写了 0」

`Host.Schedule` / `Host.Retention` 为指针，`nil` 表示套用 `defaults`。

现有 `applyDefaults` 里「retention 三项同时为 0 才套默认值」的启发式必须删掉：
在指针模型下，「没写」由 `nil` 精确表达，不需要再猜。
用户显式写了 `retention: {daily: 3}` 时，`weekly` / `monthly` 就是 0，不补默认值。

生效值通过 `Config.ScheduleFor(h)` / `Config.RetentionFor(h)` 获取，
不在加载时把 defaults 拷进各 host——保留 `nil` 才能让校验错误指向用户真正写错的位置。

### R6 v1 清单给出迁移提示

检测到 `version: 1` 时，报「检测到 v1 单机清单，请参考 examples/ark.yaml 迁移到 v2」，
而不是让用户面对一串「hosts 不能为空 / host 字段未知」这类含糊的字段错误。

### R7 `validate` 输出与示例清单适配

- `validate` 改为逐 host 摘要（当前打印的 `cfg.Host` / `cfg.Project.Name` 已不存在）。
- `examples/ark.yaml` 重写为多机清单，含一台 `local: true` 的 hub 条目。
- README 中 `validate` 的用法段随之同步。

## Non-Goals

- 不动 `doctor` 的检查内容：远程化是 P1-2 / P1-3 的范围。本轮只保证它能编译、
  能对多机清单跑**现有的本地检查**，不新增远程检查。
- 不实现 SSH 执行层（P1-2）。
- 不提供 v1 → v2 的自动迁移工具：清单是人手写的，机器改写反而容易悄悄改错语义。
- 不动 `Target.ID()`（P2-3 才需要在它前面拼 host 段）。

## Acceptance Criteria

- [ ] 一份含 3 台机器（其中一台 `local: true`）的清单能通过 `Validate`
- [ ] host 重名 → 报错并指出冲突的下标
- [ ] `local: true` 且 `ssh` 非空 → 报错
- [ ] 既非 `local` 又缺 `ssh` → 报错
- [ ] 缺 `known_hosts_file` → 报错
- [ ] `version: 1` → 报可读的迁移提示，而不是字段缺失错误
- [ ] `defaults` 未写时套用常量默认值；host 显式写 `retention: {daily: 3}` 时
      不被补上 weekly / monthly
- [ ] 上述每一条都有专门的测试用例
- [ ] `./bin/ark validate -c examples/ark.yaml` 通过
- [ ] `make check` 全绿
