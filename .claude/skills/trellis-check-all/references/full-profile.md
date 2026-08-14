# Full Profile

Full 是完整验收映射和全影响面审查。只有 `check_profile.effective_depth=full` 时读取本文件。

---

## Step 1：对照规划三件套检查实现

untracked 上下文没有 task artifacts，本 Step 标记 `N/A`；不得把 summary、stage 或聊天记录当成 PRD。仍须完整执行 Step 2 与 Step 3，并对实际 diff、相关 spec、当前验证证据和多仓分发边界负责。

### 1.1 验收依据

- PRD Requirement / Acceptance Criteria：行为基线。
- Design API、数据模型、数据流、关键决策和 rollback：技术基线。
- Implement 有序步骤、review gate 和 rollback point：落地基线。

full 提取所有适用条目。每条记录来源位置，实际阅读对应代码后再判断。

### 1.2 必查类型

| 来源 | 可验证条目 |
| --- | --- |
| `prd.md` | AC、需求、业务规则、UI 文案、边界和异常场景 |
| `design.md` | API 路径/方法/字段、数据模型、数据流、关键 tradeoff、rollout/rollback |
| `implement.md` | 有序步骤是否落地、review gate 是否满足、rollback point 是否可用 |

`implement.md` 中的 validation command 在本步骤只做静态前提核对；真实运行归 Step 3。

### 1.3 追踪方法

| 条目类型 | 追踪路径 |
| --- | --- |
| API 行为 | Controller/Handler -> Service -> DAO/Storage |
| 前端交互 | 组件 -> 事件 -> 状态管理 -> API 调用 |
| 数据校验 | 前端规则 + 后端 validator/service |
| UI 文案 | 组件、i18n/locale 或其它有效文案来源 |
| 计算转换 | 实际 service/utility 算法及边界值 |
| 状态流转 | 状态定义 + 允许的转换条件 |
| Schema | DTO/类型/迁移中的字段、类型、约束和默认值 |
| Implement 步骤 | 对应代码、配置、迁移或资产是否存在且可用 |

文案要求逐字一致时，对照最终有效文案来源；不要强制要求文案必须直接写在组件字面量中。

### 1.4 记录结果

发现偏差、缺失、部分实现或文案不一致时，写入统一问题集合并继续。不要在此步骤询问“先修还是继续检查”。

---

## Step 2：实现假设验证

根据实际变更选择适用 Dimension。每个适用 Dimension 都要确认源码或真实契约证据，不能凭记忆通过。

### Dimension A：API Contract

**Trigger**：新增或修改已有 API 调用、请求参数或响应解析。

- 读取 Controller/Handler 和 DTO/Schema，确认实际请求、响应结构。
- 找到项目内同 API 或同模式调用作为参考。
- 确认参数名、类型、默认值、分页字段和起始页码。
- 覆盖正常、空值、零值和错误响应。

### Dimension B：Component Context

**Trigger**：在 Modal、Drawer、Tab 或条件渲染容器内修改有状态组件。

- 确认容器关闭或切换时是否销毁子组件。
- 确认受控值、初始化值和外部状态绑定。
- 确认状态保持/重置行为符合规划。
- 对照项目内相同容器的既有用法。

### Dimension C：Data History

**Trigger**：新增、修改或重新解释持久化字段。

- 确认历史记录的新字段值和 null/零值行为。
- 确认过滤、聚合和降级查询能处理历史数据。
- 追踪新字段的写入来源和可靠性。
- 无可用历史数据环境时标记 `部分验证` 或 `阻塞`，不得标记通过。

### Dimension D：Data Flow Trace

**Trigger**：变更跨越 UI、API、Service、Storage 中的两个或更多边界。

- 模拟完整请求路径和返回路径。
- 确认各层参数名、类型、嵌套层级一致。
- 覆盖缺省、空值、零值、特殊字符和错误传播。
- 分层代码分别正确不等于整条链路正确，必须连起来核对。

### Dimension E：Verification Tests

**Trigger**：Dimension A-D 任一适用。

- 检查关键假设是否已有可运行的自动化测试或明确手动验证。
- 优先覆盖最脆弱的参数名、嵌套结构、历史数据和空值路径。
- 测试存在时实际运行；未运行不能报告通过。
- 缺少测试时记录问题，等待用户确认修复范围后再新增测试。

发现假设错误时写入统一问题集合并继续其它可执行检查。只有该错误让后续检查前提失效时，才按“真正阻塞”规则暂停。

---

## Step 3：完整性、规范与项目验证

读取 `.agents/skills/trellis-check/SKILL.md`（Claude-only 项目读取对应 `.claude` 副本），复用以下内容：

- 适用 spec 的读取方法；
- lint、typecheck、测试等项目验证命令；
- 测试覆盖、跨层数据流、复用、依赖和同层一致性检查；
- debug logging、warning suppression 和类型安全绕过检查。

### Audit-Only 覆盖规则

在 Check-All 内执行时，下列 `trellis-check` 指令一律失效：

- “Fix any failures before proceeding”；
- “fix them directly”；
- “Report and Fix”；
- 任何要求检查 agent 直接编辑、补测试或反复修到通过的语句。

验证失败时记录命令、退出状态和关键错误到统一问题集合，继续其它独立验证。可能写业务数据或外部系统的验证不直接运行，按真正阻塞规则处理。

### Maven Evidence 复用

实际变更位于 Maven reactor 时，读取 `maven_verify.py` 的 evidence schema，并只读执行：

```bash
python3 ./.trellis/scripts/maven_verify.py check --latest --require-plan <final-plan.json>
```

- `reusable`：核对 lifecycle、模块、消费者、测试、附属制品和 skip 项后纳入验证证据。
- `partial`：记录未覆盖的 module/consumer/test/artifact 或更高 lifecycle 要求。
- `stale`：记录源码、测试、POM、外部父 POM、Git 或工具链失效原因。
- `failed` / `blocked`：保留命令退出或证据损坏事实，不把未执行验证写成通过。

Check-All 与 dedicated subagent 都是 audit-only：不得调用 `maven_verify.py plan/run`，不得运行任何会写 `target/`、本地仓库或缓存的 Maven goal。缺少可复用 evidence 时，输出由主会话或 implement 路径执行的精确重跑计划需求；不得默认 `clean package/install`、`-amd` 或全 reactor。

所有发现候选按 `references/fallback-findings.md` 先判定 `CHK-*` / `FBK-*`，再分配严重度。严重度不得反向决定通道；不满足三项硬准入的泛化建议不报告，保护收益或验证环境不完整则保留 FBK 并标记报告缺口。

---

## Full 通过条件

Full 通过必须同时满足：

- 所有适用 PRD / design / implement 条目已映射到实现或明确 `N/A`；
- 所有触发的假设 Dimension 已完成源码或真实契约核对；
- 项目规范、复用、依赖、同层一致性和验证命令已覆盖实际变更范围；
- strict pass：无 `CHK-*`、无 `FBK-*`、无阻塞、无部分验证、无实质剩余风险；或
- 已接受风险通过：所有剩余 `CHK-*` / `FBK-*` 都有当前有效的用户风险接受，且无阻塞、无部分验证、无未接受的实质剩余风险。
