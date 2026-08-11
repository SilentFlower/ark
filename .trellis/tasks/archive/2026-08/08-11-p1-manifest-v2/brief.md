# Brief — P1-1 清单模型升级到多机（v2）

## Goal

- 把 `ark.yaml` 从「一台机器一份清单」改造为「hub 一份清单管所有机器」的 v2 模型，
  并让 `ark validate` 能校验这份多机清单。

## Scope

- `internal/config`：顶层结构改为 `version / repo / defaults / hosts`，
  新增 `Host` 与 `SSH`，`Load` 改为两遍解析（版本探测 → 严格解析），
  新增 `ScheduleFor` / `RetentionFor` 访问器，`Validate` 按 host 下标聚合错误。
- `internal/doctor`：最小适配以保持可编译——遍历 hosts，`local: true` 跑现有全部
  本地检查，远程 host 只跑 hub 侧可判断项，其余记 warn。
- `internal/cli`：`validate` 输出改为逐 host 摘要。
- `examples/ark.yaml`：重写为含一台 `local: true` 的多机清单。
- `internal/config/config_test.go`：整体重写，覆盖全部新增失败路径。
- README 中 `validate` 用法段同步。

## Non-Goals

- 不做 `doctor` 远程化（P1-2 / P1-3 的范围），本轮不加 `--host` / `--all` 标志，
  也不加 `ssh` 二进制检查。
- 不实现 SSH 执行层。
- 不提供 v1 → v2 自动迁移工具。
- 不动 `Target.ID()`（P2-3 才需要在它前面拼 host 段）。

## Key Decisions

- **版本判定必须在严格解析之前**：v1 清单顶层的 `host:` / `project:` 在 v2 结构上会被
  `KnownFields(true)` 报成「未知字段」，正是要避免的含糊错误。改为先宽松解析取 `version`，
  再严格解析完整结构。
- **`version` 缺失也报错**，要求显式写 `version: 2`。不兼容变更已真实发生过一次，
  静默补最新版会让抄旧文档的清单以一串字段错误的形式失败。
- **不把 defaults 归一化拷进各 host**，保留 `nil` 并用 `ScheduleFor` / `RetentionFor`
  取生效值。归一化会丢掉「值是谁写的」，导致错误指向 `hosts[2].schedule`
  而用户其实写在 `defaults` 里。
- **删除「retention 三项同时为 0 才套默认值」的启发式**：指针已精确表达「没写」。
- **`Host.SSH` 用指针**：`local` 与 `ssh` 互斥的判定需要区分「写了空 ssh 段」和「没写」。
- **不校验 `repo.url` 是否带 host 段**：无法可靠判断末尾那段是不是机器名，
  误报代价高于收益；改在示例清单注释里说明。
- **target ID 唯一性仍是 per-host**：不同机器上有同名 volume 完全正常，靠 host tag 区分。

## Key Context

- 改动文件：`internal/config/config.go`、`internal/config/config_test.go`、
  `internal/doctor/doctor.go`、`internal/cli/root.go`、`examples/ark.yaml`、`README.md`。
- 架构依据：`docs/design.md` ADR-002（hub 编排 / 目标机 agentless）、
  ADR-007（未知字段一律报错）、ADR-008（静态与环境校验分层）、
  ADR-009（共用仓库按 tag 区分）。
- 规约约束：`Validate` 内禁止访问文件系统；错误一次性聚合后 `errors.Join`；
  doctor 三态中「无法判断」必须是 warn；注释与错误信息一律中文。

## Risks / Deferred

- `doctor` 的适配面比预期大：`checkTargets` / `composeServices` 依赖 compose 上下文，
  从 `Config` 挪到 `Host` 时容易漏传 `ProjectName` / `EnvFile`。完成后手动跑一次确认输出。
- 测试常量牵一发动全身：`validManifest` 改为多机后，现有用例的 `strings.Replace`
  锚点全部失效，需逐条重写而非微调。
- doctor 的远程检查在本轮停留在 warn 占位，P1-3 必须补齐，否则 `doctor` 对远程机器
  的结论是不完整的。

## Acceptance

- 含 3 台机器（其中一台 `local: true`）的清单能通过 `Validate`。
- 以下每种情况都报可读错误且有专门测试用例：host 重名、`local` 与 `ssh` 并存、
  两者皆空、缺 `known_hosts_file`、`identity_file` 相对路径、`version: 1`、
  `version` 缺失、未知版本号、`hosts` 为空。
- `defaults` 未写时套用常量默认值；host 显式写 `retention: {daily: 3}` 时
  不被补上 weekly / monthly。
- `./bin/ark validate -c examples/ark.yaml` 通过。
- `make check` 全绿。

## Next Step

- 从 `internal/config/config.go` 的结构与加载流程改起（implement.md 步骤 1）。
