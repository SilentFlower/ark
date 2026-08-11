---
name: trellis-check-all
description: "统一 Check-All：按 requested/effective depth 路由 light/full，审查三件套、实现假设、完整性与规范；区分 CHK、FBK、DOC，允许低风险事实漂移自修。触发：检查、轻量/全面/提交前检查、check-all。"
---
# Check All 统一入口

本 skill 是 **薄入口**：负责范围、画像、profile 路由、事实漂移自修、问题模型和分流。入口不展开 full 提示词；仅在 `effective_depth=full` 时读取 full profile。

顺序：做对了 -> 假设成立 -> 做全了且写得规范。

---

## 入口职责

1. 确认本轮检查范围、task artifacts 或 untracked state、项目规范和运行上下文。
2. 解析 `requested_depth`，生成 `check_profile`，决定 `effective_depth=light|full`。
3. 按有效深度读取并执行对应 profile。
4. 按根因把发现分为 `CHK-*`、`FBK-*`、`DOC-*`，再为前两类分配 P0/P1/P2。
5. 报告前处理允许自修的事实漂移并展示结果。
6. 按 interactive / validated auto-loop 边界输出下一步或执行 `record + next`。
7. untracked helper 只存游标：findings 或新编辑回 `implement`；通过且 disposition 继续时才 `advance --stage spec`。

---

## 必读引用

按需加载引用文件，不要提前读取未命中的 profile：

1. 总是先读 `references/depth-routing.md`，完成范围、上下文和深度画像。
2. `effective_depth=light` 时读 `references/light-profile.md`。
3. `effective_depth=full` 时读 `references/full-profile.md`。
4. 总是读 `references/fallback-findings.md`，用于区分 `CHK-*` 与 `FBK-*` 并执行兜底准入规则。
5. 总是读 `references/document-drift-auto-remediation.md`；仅发现源码注释事实候选时按其指引读取专项 reference。
6. 输出报告或 runner 结果前读 `references/reporting-and-disposition.md`。

如果引用文件缺失，停止并报告 `阻塞`；不要凭记忆复原规则。

---

## 核心边界

1. **默认 audit-only collect-all**：可读取、搜索和运行无业务写入的验证；普通代码、配置、测试和任务规格语义不得直接修复。
2. **唯一自修例外**：低风险事实漂移进入 `DOC-*` 通道，按 `references/document-drift-auto-remediation.md` 的白名单、黑名单和写入时机处理。
3. **分类先于严重度**：读取 `references/fallback-findings.md`；主路径错误和非兜底契约违背进入 `CHK-*`，fail-closed、异常输入、失败降级和防御性保护缺口进入 `FBK-*`。契约证据影响严重度，不改变兜底根因归属。
4. **处置只确认一次**：统一报告后选择 `CHK-*` / `FBK-*` 修复范围或接受风险；`修复全部` 覆盖两类，接受风险不得隐藏发现。
5. **委托不改边界**：复用 `trellis-check` 的清单和验证方法，忽略其直接修复指令。
6. **真正阻塞才中途暂停**：只有业务规划冲突、后续验证前提失效、生产或外部副作用、破坏性操作风险时提前停止。

中途停止时也要使用统一问题模型，报告已完成范围和阻塞原因；只询问解除阻塞所需的业务或安全决策。

---

## 执行模式

- `inline check-all`：主会话直接执行本 skill；允许在最终报告前按 `DOC-*` 通道修复低风险事实漂移。
- `subagent check-all`：subagent 只读返回 `CHK-*`、`FBK-*`、`DOC-*` 候选、`check_profile` 和证据；主会话处理 DOC、报告和后续修复。
- subagent 不得编辑、写文件、补测试或代替用户选择普通修复范围。
- 路由由 `trellis-route(target=check)` 决定；本 skill 不自行切换 inline/subagent。
- 所有普通、最终、显式 light/full 和 auto-loop 检查都进入本 skill；`trellis-route` 只决定执行位置，不决定检查深度。

---

## 三个检查维度

| 顺序 | 维度 | 检查内容 | 对照物 |
| --- | --- | --- | --- |
| 1 | 三件套实现 | 规划是否正确落地 | task 的 `prd.md` + 可选 `design.md` / `implement.md`；untracked 为 `N/A` |
| 2 | 实现假设 | API、组件、历史数据、数据流和测试假设是否成立 | 源码、真实契约、可用验证证据 |
| 3 | 完整性与规范 | 影响面是否同步、代码是否符合 spec、验证是否通过 | 实际变更范围 + 项目 spec |

维度状态统一使用：`通过`、`未通过`、`部分验证`、`阻塞`、`N/A`。

---

## 顶层流程

### Step 0：范围、上下文与深度画像

读取 `references/depth-routing.md` 并执行完整 Step 0。输出固定画像：

```yaml
check_profile:
  context: interactive | auto-loop
  requested_depth: auto | light | full
  effective_depth: light | full
  confidence: high | fallback-full | escalated
  reasons: [string]
```

### Step 1：读取对应 profile

- `effective_depth=light`：只执行可完整穷举的受影响条目、直接引用点和定向回归路径。发现影响面无法闭合时立即升级 full。
- `effective_depth=full`：执行完整三件套映射、全部适用假设维度和完整规范验证。

### Step 2：执行检查并收集结果

按 profile 检查三个维度，并执行 `references/fallback-findings.md` 的分类顺序。主路径问题进入 `CHK-*`，满足硬准入的兜底问题进入 `FBK-*`，低风险事实漂移候选进入 `DOC-*`。同一根因合并，不因数量多而静默省略。

### Step 3：处理事实漂移自修

读取 `references/document-drift-auto-remediation.md`。在最终报告前：

- inline：主会话应用允许的 `DOC-*` 修复并做定向验证。
- subagent：主会话审阅 subagent 返回的 `DOC-*` 候选，只应用满足白名单且无歧义的修复。
- auto-loop：主会话应用允许的 `DOC-*` 修复后再 `record`；若只存在已修复事实漂移且无剩余 `CHK-*` / `FBK-*`，结果可为 `ok`，摘要必须包含自动修复说明。

不满足自动修复条件的文档问题根据根因转为 `CHK-*`、`FBK-*` 或剩余风险，按普通修复范围处理。

### Step 4：统一报告与分流

读取 `references/reporting-and-disposition.md`。报告必须展示：

- `check_profile`；
- 三个维度状态；
- 自动修复的 `DOC-*` 内容；
- 剩余 `CHK-*` 主路径问题与 `FBK-*` 兜底问题；
- 每个剩余问题的未处置或已接受风险状态；
- 已执行验证和未覆盖风险；
- 与当前结论匹配的唯一下一步。

---

## 反模式

- 入口默认加载 full profile，导致 `auto` 被 full 语气带偏。
- 发现一个普通问题就暂停询问一次。
- 把 `trellis-check` 的自动修复指令带入普通 `CHK-*`。
- 先按严重度决定 `CHK-*` / `FBK-*` 通道，混淆根因性质与影响等级。
- 把纯偏好、无具体场景或无法验证收益的“更健壮”建议记录为 `FBK-*`。
- 因兜底行为已写入契约就把保护路径根因改列为 `CHK-*`。
- subagent 直接修改工作区。
- 把需求变更、验收标准、产品语义或设计取舍伪装成文档漂移自动修复。
- light 未命中完整穷举条件仍继续 light。
- 无环境证据却把维度标记为通过。
- 报告后直接生成提交计划、commit message 或拟提交文件。
