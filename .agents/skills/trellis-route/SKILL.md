---
name: trellis-route
description: |
  Route trellis-implement / trellis-check execution mode with a gitignored personal preference file.
  Implement can route inline or subagent. Check always routes the unified trellis-check-all entry
  inline or subagent; check-all itself selects light/full depth from intent and risk.
  Invoked from Phase 2.1 target=implement and Phase 2.2 target=check/check-all of the routing-aware workflow.
  Current-task repair/recheck loops reuse the latest valid route decision instead of prompting again.
  Compacted resumes recover same-session route choices from gitignored runtime state before prefs or prompting.
  Final re-checks return to Phase 2.2 before commit.
  Skip in non-trellis projects (no .trellis/). Not for other subagents (trellis-research / trellis-debug).
---

# Trellis 路由器：implement / check 执行模式选择

主 agent 进入 Phase 2.1 实现路由或 Phase 2.2 检查路由时调用本 skill。task 上下文继续使用 task-scoped session route decision；untracked 上下文只读取个人 `.trellis/.route-prefs.tmp`，不读取或写入 `route_decisions`。提交前确实需要最终复查时，回到 Phase 2.2 并按当前 subject 的同一规则解析 route。

个人配置只写入 `.trellis/.route-prefs.tmp`。该文件匹配 `.trellis/.gitignore` 的 `*.tmp` 规则，属于开发者本地偏好，不纳入 git，也不影响其他开发者。auto-loop 可在 `.trellis/.runtime/auto-loop/<run-id>.json` 写入临时 route 授权；它不是个人偏好，优先级低于 `.route-prefs.tmp`，只用于减少 auto 模式下的交互打断。

本轮 route 决策写入当前 session runtime 文件 `.trellis/.runtime/sessions/<context-key>.json` 的 `route_decisions` 字段。该文件匹配 `.trellis/.gitignore` 的 `.runtime/` 规则，只保存当前 AI session/window 的执行状态，用于压缩后恢复；它不是任务属性，不写入 `.trellis/tasks/**`，也不是长期团队配置。

---

## Step 0: 识别目标与用户意图

个人 route 配置只决定“已获准执行后的模式”，不是开工授权。调用 helper 前，必须确认当前 workflow 已允许进入对应 target：task implement 需要任务已完成规划确认并处于 `in_progress`；untracked implement 需要 `untracked_flow.py status` 命中当前 session；check 需要 Phase 2.2 或 untracked `stage=check`。如果仍在 planning、等待用户确认，或用户表达“等一下 / 我再想想”，停止，不读取 runtime/prefs。

先用 `task.py current --source` 与 `untracked_flow.py status` 确认 subject，二者只能命中一个：

- task 命中：`scope=task`，继续使用本 skill 既有 runtime -> prefs -> auto-loop 顺序。
- 无 task 且 untracked 命中：`scope=untracked`，只使用 pref-only CLI；不得伪造 task 路径或 task artifacts。
- 二者都未命中：返回 workflow Request Triage，不展示执行 route。

task 的合法 route 决策必须能追溯到 `trellis-route`、同编号 fallback 选项、有效 `.trellis/.route-prefs.tmp`，或 route helper 校验过的 auto-loop 临时授权，并且 `task` 字段等于当前任务路径。untracked 的合法 route 只来自本次紧邻选择或 `read-pref` 命中；不写 runtime，也不接受 auto-loop 授权。用户自然语言、摘要、SessionStart 提示、`codex-mode`、空偏好或历史裸数字都不能单独作为有效 route 决策。

当前上下文内已有 target 匹配、task 等于当前任务路径、且来源合法的 route 决策时，后续实现、check 发现问题、用户指出刚检查过的实现有问题、修复后重检、提交前复查均默认复用最近 implement/check 路由；除非用户明确要求重选/临时改/清除默认，不再调用本 skill。当前上下文没有 route 决策但 runtime state 命中时，本 skill 恢复该决策并输出同样的结构化 `route_decision`。如果上下文里只有上一个任务的 `route_decision`，必须忽略并重新解析当前任务。

Codex 的 `<codex-mode>` 与 `workflow-state:*-inline` 只描述上游原生上下文/readiness 能力，不是 route 决策或选项过滤器。实际执行位置只读取本 skill 的 runtime/prefs/本轮选择；若紧邻路由决定是 subagent，主 agent 按 catalog dispatch 对应角色。禁止绕过 `trellis-route` 直接从 Codex banner 推导执行模式。

先判断本次路由目标：

- `target=implement`：决定 `inline` / `subagent`。
- `target=check`：只决定 `check-all inline` / `check-all subagent`。用户明确说 light/full 时，把该意图交给 Check-All 作为 requested depth，不改变 route mode。

再判断用户是否要求覆盖个人配置：

- 临时改：`临时改` / `这次用` / `本次用` / `这次不用默认` / `override once`
- 重新选择：`重新选择` / `重选` / `route 选项` / `show route options`
- 更新默认：`改默认` / `更新默认` / `保存默认`
- 清除默认：`清除默认` / `删除 route 默认` / `reset route`

如果命中上述覆盖意图，即使 `.trellis/.route-prefs.tmp` 存在，也不能直接使用配置；必须进入 Step 2 展示对应选项。

## Step 0.5: 解析已有 route state

task 仅在没有覆盖意图、当前上下文没有 target + 当前 task 匹配的合法 `route_decision` 时调用 `resolve`，解析顺序固定为 runtime -> prefs -> auto-loop。untracked 不调用 `resolve`，每次直接调用 `read-pref`；命中即可执行，miss 才进入 Step 2。

调用随本 skill 分发的 helper；不要在对话中内嵌或改写 helper 逻辑：

```bash
python3 .agents/skills/trellis-route/scripts/route_state.py resolve --target <implement|check>
# Claude 平台若只有 .claude skill 副本，则使用：
python3 .claude/skills/trellis-route/scripts/route_state.py resolve --target <implement|check>
```

untracked 调用：

```bash
python3 .agents/skills/trellis-route/scripts/route_state.py read-pref --target <implement|check>
# Claude 平台若只有 .claude skill 副本，则使用：
python3 .claude/skills/trellis-route/scripts/route_state.py read-pref --target <implement|check>
```

helper 只接受当前 session 或唯一 session fallback 的 `.trellis/.runtime/sessions/<context-key>.json`，并只从 `route_decisions.<target>` 恢复当前任务的决策。命中时输出 `{"status":"hit", ...}`，其中默认输出里的 `task` / `mode` / `source` 已经过 task/target/source/mode/scope 校验，可跳过 Step 2 并进入 Step 3 输出决策。`origin=route-prefs` 表示来自个人 route 配置，并且 helper 已写回 session runtime state；`origin=auto-loop` 表示来自 auto-loop 临时授权且 helper 已写回 session runtime state；`origin=runtime` 表示来自 session runtime state。

helper 默认输出为精简 JSON，只包含 route 执行必需的 `status`、`origin`、`mode`、`source`/`reason` 等字段。需要排查完整 `decision`、session 文件、context key、任务路径、个人配置路径或写回标记时，在同一命令末尾加 `--verbose`；不要为了诊断信息额外读取 runtime 文件。

输出 `status=miss`、文件缺失、JSON 损坏、任务不匹配、source/mode 不合法、prefs 缺失或 prefs 值不合法时，忽略已有状态并继续 Step 2。不要删除不匹配 runtime 文件，避免误伤其他窗口。

---

## Step 2: 展示选项并等待用户选择

优先调用 `AskUserQuestion`。选项 label 前缀编号，方便用户直接打数字快速选。

裸数字回复（如 `1` / `2` / `3` / `4`）只有在当前可见的上一条 assistant 消息刚刚展示同一个 target 的 route 选项、并明确停下等待用户回答时，才可解释为本次 numbered fallback 选择。compact summary、ordinary summary、SessionStart 摘要、replacement history、历史消息、旧 target 的裸数字、非紧邻回复里的裸数字都不能触发 `write --source numbered-fallback`；遇到这些情况必须重新展示当前 target 的选项并等待新的紧邻回复。

如果当前平台或模式没有 `AskUserQuestion` / `request_user_input`，不要自行选择 inline 或 subagent 继续。改用普通聊天消息原样呈现同一组编号选项，并停止等待用户回复；只有用户在这条选项消息之后立即回复裸数字，才可进入 Step 2.5 / 2.6 / 3。

### target = implement，且 resolve 未命中

- **question**: "本次 implement 走哪种模式？"
- **header**: "Impl 模式"
- **options**:
  1. label "1. 本次 Inline", description "主 agent 直接执行，只影响这一次"
  2. label "2. 本次 Subagent", description "dispatch trellis-implement，只影响这一次"
  3. label "3. 保存默认：Inline", description "本次使用 inline，并写入个人配置，后续默认不再问"
  4. label "4. 保存默认：Subagent", description "本次使用 subagent，并写入个人配置，后续默认不再问"

### target = implement，且用户要求临时改 / 重新选择

- **question**: "当前默认：implement=<当前值或无>。本次要怎么处理？"
- **header**: "Impl 覆盖"
- **options**:
  1. label "1. 仅本次 Inline", description "只覆盖这一次，不修改个人配置"
  2. label "2. 仅本次 Subagent", description "只覆盖这一次，不修改个人配置"
  3. label "3. 更新默认为 Inline", description "本次使用 inline，并写入个人配置"
  4. label "4. 更新默认为 Subagent", description "本次使用 subagent，并写入个人配置"
  5. label "5. 清除默认", description "删除 implement 默认，然后重新显示无配置选项"

### target = check，且 resolve 未命中

check 路由始终展示统一 Check-All 入口；深度由 Check-All 决定。

- **question**: "本次 check 走哪种模式？"
- **header**: "Check 模式"
- **options**:
  1. label "1. 本次 Check-all inline", description "主 agent 执行全面检查，只影响这一次"
  2. label "2. 本次 Check-all subagent", description "dispatch 全面检查，只影响这一次"
  3. label "3. 保存默认：Check-all inline", description "本次使用 check-all inline，并写入个人配置"
  4. label "4. 保存默认：Check-all subagent", description "本次使用 check-all subagent，并写入个人配置"

### target = check，且用户要求临时改 / 重新选择

选项仍只展示 `check-all` 路径。

- **question**: "当前默认：check=<当前值或无>。本次要怎么处理？"
- **header**: "Check 覆盖"
- **options**:
  1. label "1. 仅本次 Check-all inline", description "只覆盖这一次，不修改个人配置"
  2. label "2. 仅本次 Check-all subagent", description "只覆盖这一次，不修改个人配置"
  3. label "3. 更新默认为 Check-all inline", description "本次使用 check-all inline，并写入个人配置"
  4. label "4. 更新默认为 Check-all subagent", description "本次使用 check-all subagent，并写入个人配置"
  5. label "5. 清除默认", description "删除 check 默认，然后重新显示无配置选项"

## Step 2.5: 读 subagent_skip_compile

仅 `target=implement` 且最终选择 `subagent` 时读取：

```bash
if [ -f .trellis/config.yaml ]; then
  grep -E "^\s*subagent_skip_compile:\s*true\b" .trellis/config.yaml > /dev/null && echo true || echo false
fi
```

为 `true` 时，Step 3 的 implement subagent 指令会附加“跳过编译”prompt 段。其他路径不读此配置。

---

## Step 2.6: 写入 route state / 默认配置

task 用户选择本次模式后，调用 helper 写入当前 session runtime。选项含“保存默认”或“更新默认”时，加 `--save-pref`。untracked 的“仅本次”只用于当前调用，不写任何 runtime 或偏好；“保存默认/更新默认”只调用 `write-pref`。

只影响本次：

```bash
python3 .agents/skills/trellis-route/scripts/route_state.py write --target <implement|check> --mode <mode> --source <trellis-route|numbered-fallback>
# Claude 平台若只有 .claude skill 副本，则使用：
python3 .claude/skills/trellis-route/scripts/route_state.py write --target <implement|check> --mode <mode> --source <trellis-route|numbered-fallback>
```

保存 / 更新默认：

```bash
python3 .agents/skills/trellis-route/scripts/route_state.py write --target <implement|check> --mode <mode> --source <trellis-route|numbered-fallback> --save-pref
# Claude 平台若只有 .claude skill 副本，则使用：
python3 .claude/skills/trellis-route/scripts/route_state.py write --target <implement|check> --mode <mode> --source <trellis-route|numbered-fallback> --save-pref
```

untracked 保存 / 更新默认：

```bash
python3 .agents/skills/trellis-route/scripts/route_state.py write-pref --target <implement|check> --mode <mode>
# Claude 平台若只有 .claude skill 副本，则使用：
python3 .claude/skills/trellis-route/scripts/route_state.py write-pref --target <implement|check> --mode <mode>
```

清除默认：

```bash
python3 .agents/skills/trellis-route/scripts/route_state.py clear-pref --target <implement|check>
# Claude 平台若只有 .claude skill 副本，则使用：
python3 .claude/skills/trellis-route/scripts/route_state.py clear-pref --target <implement|check>
```

helper 写入规则：保留另一个 target 的 runtime 决策和偏好；覆盖当前 target；`source` 保持原始合法来源，正常 skill 交互用 `trellis-route`，普通聊天编号 fallback 用 `numbered-fallback`，prefs 命中由 helper 写 `route-prefs`，auto-loop 临时授权命中由 helper 写 `auto-loop`；保留 session runtime 文件中的 `platform`、`current_task`、`current_run`、`current_auto_run` 等既有字段；内部 `task` 必须写当前 `task.py current --source` 的任务路径。写入失败不阻塞本次 route 输出，但要在回复中简短说明未持久化，压缩后可能再次询问。

当同一 session 从一个任务切换到另一个任务时，helper 写入当前任务任一 target 的决策前会清理 runtime 中属于其他任务的 `route_decisions`。个人默认仍保留在 `.trellis/.route-prefs.tmp`；清理只影响当前 session 的临时恢复状态，目的是避免新任务的 check 阶段误复用上一个任务的 check 路由。

---

## Step 3: 输出执行指令

本 skill 不调用 Skill / Agent 工具，而是输出指令让主 agent 在下一轮执行。

### 路由表

| 路由决定 | 主 agent 应执行 |
|---------|----------------|
| `inline implement` | `Skill({skill: "trellis-before-dev"})` 加载 spec；task 再读任务文档，untracked 读取 helper 状态与实际 scope → 主线程实施 → 跑必要验证 → 回到 Phase 2.1 completion contract |
| `subagent implement` | 按下方当前平台执行配方调用 `trellis-implement`；task 使用 `Active task:`，untracked 使用下方自包含 `Untracked work:` 契约；主 agent 收到结果后回到 Phase 2.1 completion contract |
| `inline check-all` | `Skill({skill: "trellis-check-all"})` |
| `subagent check-all` | 读取 catalog 当前平台条目，只调用 `checkAll.target` 声明的专用 audit-only `trellis-check-all` 角色，并按 `checkAll.launch` 启动；subagent 只返回 `CHK-*` / `FBK-*` / `DOC-*` 候选，不写文件。目标缺失、host 未发现或资格不成立时停止并请用户改选 inline；禁止通用 agent 与 `trellis-check` fallback |

implement 路由只决定执行位置，不拥有实现后的停止策略。无论 inline 或 subagent，focused validation 完成后都必须返回 workflow Phase 2.1 的 completion contract；由该 owner 处理 auto-loop、用户显式继续/暂缓、已有 hold 和默认立即 Check-All 的优先级。

### 平台 Dispatch Catalog

`references/platform-dispatch.json` 是平台启动契约的唯一事实源。当前条目提供稳定 `id`、`implement.launch/target`、`checkAll.launch/target/format/verification` 和 `inlineOnlyReason`。仅当 route 已选中 subagent 时，主 agent 才从当前运行平台上下文确定稳定平台 ID，读取 catalog 对应条目，并把 `launch` 字段翻译成当前 host 的工具调用。inline 路径不读取 catalog，也不预先过滤 route 选项。

dispatch prompt 第一行始终遵守当前 task/untracked 契约。agent 文件存在只证明项目产物已投影；实际 dispatch 时还必须确认当前 host 暴露 catalog 声明的 launch 能力。目标、launcher 或资格不可用时停止并让用户改选 inline，不得静默降级、发明替代 agent 或使用上游 workspace-write `trellis-check`。`verification` 只说明该角色如何证明只读资格，不放宽 Check-All 边界；`eligible=false` 时同时展示 `inlineOnlyReason`。

### Untracked Subagent Dispatch 契约

untracked 的 implement/check subagent prompt 第一行固定为 `Untracked work: <work-id>`，并包含事项摘要、当前 stage、实际 diff、相关 spec 路径、已有验证上下文和本轮明确职责。helper 只提供流程游标，不提供 scope、baseline、fingerprint 或 owner evidence。不得写 `Active task:`，不得要求 `prd.md`、`implement.jsonl` 或 `check.jsonl`。agent 必须直接执行，不得递归 dispatch implement/check agent。

### Subagent Check-All Dispatch 契约

使用 catalog 声明的专用 subagent 时，dispatch prompt 第一行必须是当前任务路径，并包含以下完整边界：

```text
Active task: <task path from task.py current>

执行本项目 0.6 `trellis-check-all` 的 audit-only collect-all 全流程，区分主路径 `CHK-*`、兜底 `FBK-*`，并识别可由主会话处理的 `DOC-*` 低风险事实漂移候选。

必须：
1. 读取 <task>/check.jsonl 及其列出的文件，再读取 prd.md、design.md（若存在）、implement.md（若存在）。
2. 读取并遵循本地 trellis-check-all/SKILL.md；完成三件套实现、实现假设、完整性与规范三个维度。
3. 先按本地 fallback findings 规则判定 `CHK-*` / `FBK-*`，再为两类问题分配 P0/P1/P2；已声明的兜底契约只影响证据和严重度，不改变 `FBK-*` 归属。具备具体位置、可达场景和问题证据时返回 `FBK-*`，不要求异常已实际发生；保护收益和验证方式属于报告完整度，缺少验证环境时保留 ID 并标记部分验证。泛化建议不报告。低风险事实漂移使用 `DOC-*` 候选单独返回。
4. 只读审查；禁止编辑、写文件、补测试或自修复。Step 3 只复用 trellis-check 的检查清单，忽略其自动修复指令；`DOC-*` 也只能返回候选，由主会话按 Check-All 规则决定是否写入。
5. 真正阻塞条件返回主会话，不替用户选择业务行为或修复范围。

返回：统一 Check-All 结果、全部 `CHK-*`、`FBK-*`、`DOC-*` 候选、已执行验证和剩余风险。不要输出 commit/push 计划。
```

必须确认 catalog 目标内容明确 audit-only：允许识别 `DOC-*` 候选，但不得写文件。名称相同但会直接自修复代码、测试、配置或普通 `CHK-*` 的 agent 不可使用。平台没有合格专用角色时，不得静默改成 inline，也不得使用通用 agent 或 `trellis-check` 代替；停止并让用户重新选择 check route。

### 输出模板

````markdown
- **路由决定**：<inline/subagent> <implement | check-all>
- **来源**：<个人 route 配置 `.trellis/.route-prefs.tmp` (<key>=<value>) | auto-loop 临时授权 `.trellis/.runtime/auto-loop/<run-id>.json` | session runtime state `.trellis/.runtime/sessions/<context-key>.json` 的 `route_decisions`>
- **已写入**：`.trellis/.runtime/sessions/<context-key>.json` 的 `route_decisions`
- **裸数字有效性**：用户下一条紧邻裸数字回复才可按本 target 解释；摘要、历史消息或旧 target 的裸数字无效

```yaml
route_decision:
  target: <implement | check>
  mode: <inline | subagent | check-all-inline | check-all-subagent>
  source: <trellis-route | route-prefs | auto-loop | numbered-fallback>
  scope: <task | untracked>
  task: <current task path; task only>
  work_id: <current untracked work id; untracked only>
```

接下来主 agent 应当：
- <路由表里对应的工具调用形式>
- [若 implement subagent 且 subagent_skip_compile=true：附加“跳过编译”prompt 段]

不要：
- <要避免的工具调用>
````

字段行必须使用 `- **字段**：值` 列表项；裸段落行会在渲染时折叠成一段，不得改回。`route_decision` 必须放在 ```yaml 代码块内，靠代码块保留缩进结构，不得裸写。

条件字段按命中情况出现：`来源` 只在命中个人配置、auto-loop 临时授权或 session runtime state 时显示，三者互斥取实际命中项；`已写入` 只在写入 runtime state 成功时显示；`裸数字有效性` 只在本消息刚展示 route 选项并等待用户回答时显示；“跳过编译”段只在 implement subagent + skip_compile=true 时附加。`route_decision` 必须保留在回复中，并至少保留 target/mode/source/scope/task；需要 path/decided_at 等诊断字段时重新调用 helper 并加 `--verbose`。compact summary 若只有自然语言描述，后续 agent 仍应优先读取 runtime state，而不是把 summary 当证据。

---

## 核心原则

1. **个人配置私有**：`.trellis/.route-prefs.tmp` 是本地偏好，gitignored，不能进入提交计划。
2. **runtime state 私有**：`.trellis/.runtime/sessions/` 是 session/window 状态，gitignored，不能进入提交计划。
3. **压缩恢复少打断**：当前上下文没有 route 决策时，先读 runtime state；命中当前任务/target 的合法决策就复用，不查 `trellis mem`，也不重问。
4. **个人默认次于本轮决策**：runtime state 表示“本轮已选”，优先于 `.route-prefs.tmp`。
5. **auto 授权低于个人默认**：auto-loop 临时授权只在 runtime 和个人偏好都 miss 时生效。
6. **显式覆盖优先于一切**：用户要求临时改、重新选择或清除默认时，必须重新展示选项，不能让 runtime state 或配置优先。
7. **当前任务复用路由**：当前任务内已有合法来源的最近 implement/check 路由时，后续实现、修复、重检和复查默认沿用，不再次询问模式。
8. **check 入口统一**：check 路由只展示 `check-all` inline/subagent；light/full 是 Check-All 的 requested depth，不是 route mode。
9. **check-all subagent 保持只读**：不得 fallback 到强制自修复的 `trellis-check` agent；不兼容时显式阻塞并让用户重选。
10. **决策与执行分离**：本 skill 只输出指令，下一轮由主 agent 调工具。
11. **严格执行用户选择**：路由结论一旦输出，主 agent 必须按指令执行，不可“出于谨慎”再换路径。
12. **Codex capability 不裁剪选项**：Codex mode banner 只描述原生上下文/readiness 能力；route 明确选中 subagent 时，按 catalog 路径执行。

---

## 反模式

- 在本 skill 内部直接调用 `Agent` / `Skill` 工具。
- 用户要求临时改 / 重新选择时，仍直接使用 `.trellis/.route-prefs.tmp`。
- 把 `.trellis/.route-prefs.tmp` 加入 git 暂存或提交计划。
- 把 `.trellis/.runtime/sessions/` 加入 git 暂存或提交计划。
- runtime state 的 task/target/source/mode 不匹配时仍复用。
- 把显式 light/full 当成新的 route mode，或绕过 Check-All 直达 `trellis-check`。
- check-all subagent 不存在专用 agent 时，fallback 到带自修复语义的 `trellis-check` agent。
- 平台没有兼容的 audit-only subagent 时，静默改成 inline 或继续执行。
- `AskUserQuestion` / `request_user_input` 不可用时，记录为 inline 或 subagent 路径并继续。
- 没有有效 check 配置、用户选择或最近本轮 check 路由决定时，自动执行 Check-All inline。
- 没有 `source` 合法的 `route_decision`，就把“用户说过 inline/subagent”或 compact summary 当成已路由。
- 把 compact summary、ordinary summary、SessionStart 摘要、replacement history 或历史消息里的裸数字 `1` 当成当前 target 的 numbered fallback 选择。
- 用户裸数字回复不是紧邻当前 route 选项消息时，仍调用 `write --source numbered-fallback`。
- check 发现问题后，把当前任务内已有合法 route 的修复/重检当成新的 route 边界再次询问模式。
- 给 check 任何模式附加“跳过编译”指令。
- 询问后忽视用户答案默认 subagent。
- 因 `<codex-mode>` 或 `in_progress-inline` 提到 inline，就自行把无配置 route 结果改成 inline 或隐藏 subagent 选项。

---

## 边界

- **非 trellis 项目**（无 `.trellis/`）：输出“非 trellis 项目，跳过路由”，不阻断流程。
- **config.yaml 缺失或字段缺失**：视为 false，不附加跳过编译指令。
- **无法解析 session context key**：跳过 runtime state 读写，继续 `.route-prefs.tmp` 或展示选项。
- **runtime state 损坏或不匹配**：忽略该文件，继续 `.route-prefs.tmp` 或展示选项；不要删除。
- **.route-prefs.tmp 内容损坏**：忽略偏好；必要时删除该文件并重新展示选项。
- **旧单值偏好**：文件内容只有 `inline` / `subagent` 时视为无效配置，按无配置处理并重新展示选项。
