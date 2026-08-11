---
name: trellis-task-brief
description: "从最新 prd.md、design.md、implement.md 生成、刷新、校验并在对话中展示 Trellis 任务的 brief.md。用于 Phase 1.4 task.py start 前、planning review、用户要求任务 brief/交接摘要，或 in_progress 任务缺失/过期 brief.md 时。"
---

# Trellis 任务交接摘要

为当前任务生成或更新 `brief.md`，并把交接摘要展示在对话里。`brief.md` 是从三件套派生的交接视图，不替代 `prd.md`、`design.md`、`implement.md`。

## 核心规则

- 每次运行都重新读取最新 `prd.md`、`design.md if present`、`implement.md if present`。
- 已存在的 `brief.md` 不能作为跳过同步的理由。
- `brief.md` 必须以三件套为准覆盖旧内容；无法从三件套追溯的旧内容不能保留为事实。
- 不要在 `brief.md` 里发明三件套没有表达的新需求。必填字段缺失时写“未明确”，并提示应补充三件套；没有相关内容时直接省略 `Risks / Deferred` 整节。
- 写回 `brief.md` 后，必须在当前对话中展示 brief 正文；不要只给文件路径。
- Phase 1.4 前必须展示完整 brief。默认等待用户确认后再运行 `task.py start`；只有用户明确把当前任务或最终 Brief 与“展示后直接开始 / 不用再次确认 / 视为已确认”绑定时，才可在范围未变化的前提下免除第二次确认。
- “开始做吧”“按你建议来”“可以创建任务”等普通实现或建任务意图不是 Brief 预授权，不能据此跳过确认。
- 预授权只依赖当前对话中仍然明确可见的用户表达，不建立跨会话永久偏好，也不写 session runtime。
- `in_progress` 阶段发现缺失 brief 时，不自动生成未经 review 的 brief；先读取三件套并建议回补。只有用户明确要求当场回补并 review 时，才继续写回 `brief.md`。
- 不要机械限制 brief 或对话展示长度；信息完整优先，不能截掉会影响实现判断的范围、约束、风险或验收条件。

## 执行步骤

1. 确定任务目录：
   - 用户给了任务路径时使用该路径。
   - 否则运行 `python3 ./.trellis/scripts/task.py current --source`，读取 `Current task:`。
2. 读取任务状态：
   - 读取 `<task>/task.json` 的 `status`。
   - 如果 `status` 是 `in_progress` 且 `<task>/brief.md` 不存在，并且用户没有明确要求“回补 / backfill / 重新生成并 review brief”，则只读取三件套、说明 brief 缺失、建议回补，不写 `brief.md`。
3. 读取任务文件：
   - 必读：`prd.md`。
   - 存在则读：`design.md`、`implement.md`。
4. 判断当前对话是否存在有效预授权：
   - 必须由用户明确指向当前任务或最终 Brief，并明确表示展示后直接开始、不用再次确认或视为已确认。
   - 普通实现意图、任务创建授权、旧任务确认或无法确定指向的表达均按“无预授权”处理。
   - 若最终内容扩大范围、仍有未解决 Open Questions，或新增权限、安全、隐私、生产、费用、真实数据、破坏性公开契约、外部系统边界，则预授权失效。
5. 从最新三件套提取：
   - `Goal`：任务目标一句话。
   - `Scope`：本轮实现范围。
   - `Non-Goals`：明确不做的范围。
   - `Key Decisions`：已经收敛、存在实质方案分叉且会改变产品行为、范围、UX、兼容性、风险或核心架构的决定；只提炼最终选择及其影响，不复制完整决策台账。
   - `Key Context`：关键文件、模块、入口和约束。
   - `Risks / Deferred`：仍需在实现或验证中关注的风险，以及明确延后的事项；没有内容时不生成该 section。
   - `Acceptance`：主要验收标准。
   - `Next Step`：只写进入下一阶段后的一个直接动作，不展开完整实施计划。
6. 写回 `<task>/brief.md`。如果文件已存在，仍用最新三件套派生内容覆盖旧正文。
7. 在对话中展示 brief 正文，并说明来源文件：
   - 无有效预授权：展示后结束当前回合，等待用户确认。
   - 有有效预授权：先完整展示，再在同一回合返回主 workflow 执行 `task.py start`，不得省略展示步骤。

## 模板

```markdown
# Brief — <任务标题>

## Goal

- <一句话说明任务目标>

## Scope

- <本轮要做的事情>

## Non-Goals

- <本轮明确不做的事情>

## Key Decisions

- <已经收敛且影响实施批准的关键决定>

## Key Context

- <关键文件、模块、入口和约束>

## Risks / Deferred

- <仅在存在相关风险或明确延后事项时生成；否则省略整个 section>

## Acceptance

- <主要验收标准>

## Next Step

- <进入实现后的下一步>
```

## 展示格式

Phase 1.4 review 前：

```markdown
任务交接摘要已更新：<task>/brief.md

<brief.md 正文>

请确认上述 brief；确认后才运行 `task.py start <task>`。
```

存在有效预授权时：

```markdown
任务交接摘要已更新：<task>/brief.md

<brief.md 正文>

已按你对当前 Brief 的明确预授权完成复核；范围未扩大、无未解决问题，继续启动任务。
```

任务已经是 `in_progress` 时，如果 brief 存在，进入 implement route 前复用同样的完整展示：

```markdown
当前任务 brief：<task>/brief.md

<brief.md 正文>

下一步：进入 `trellis-route(implement)`。
```

三个展示场景都完整展示 `brief.md` 正文，不压缩、不摘录、不改写字段结构。压缩重述会丢掉 Non-Goals、关键决定或验收条件中影响实现判断的内容，因此不再使用。

## 不要做

- 不要在完整展示 brief 之前执行 `task.py start`；启动仍由主 workflow 执行。
- 不要把普通实现意图解释成免确认授权。
- 不要把一次预授权扩展为后续任务或其它高风险操作的长期授权。
- 不要修改三件套来迎合 brief。
- 不要把 `brief.md` 当作第四件套扩写。
- 不要只因为 `brief.md` 已存在就跳过更新。
- 不要只输出“已写入 brief.md”而不展示正文。
