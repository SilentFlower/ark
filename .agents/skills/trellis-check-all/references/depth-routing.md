# Depth Routing

本文件只负责范围、上下文和 light/full 选择。不要在这里展开具体检查 profile。

---

## Step 0：确认范围与适用性

### 0.1 确认变更范围

默认工作区检查：

```bash
git status --short
git diff --name-only HEAD
git ls-files --others --exclude-standard
git log --oneline -10
```

`git diff --name-only HEAD` 覆盖 staged + unstaged，`git ls-files` 补未跟踪文件；不得只用 `git diff --name-only` 判定无变更。

检查已提交的 PR/分支时先确认基线，再用 merge-base diff；`git log -10` 不能替代变更范围。

如果确认范围内确实无变更，提示用户并终止。

### 0.2 读取工作上下文与规范

存在当前 task 时读取 `prd.md`、可选 `design.md` / `implement.md`、`check.jsonl` 所列 spec/research，以及变更包对应规范；无 `prd.md` 时三件套维度为 `N/A`。

不得只依赖 session 摘要推断规划内容，必须读取实际文件。

没有当前 task 时运行 `python3 ./.trellis/scripts/untracked_flow.py status --verbose`：

- `hit`：读取 work id、summary、stage；三件套维度为 `N/A`，其余维度仍覆盖实际 diff、spec 和证据。
- `miss`：仅在用户明确要求检查已知无状态 diff 时继续并报告上下文风险；否则回到 Request Triage。
- `error`：按损坏状态阻塞报告，禁止用聊天摘要恢复或覆盖游标。

untracked 必须为 `stage=check`，只读个人 check 偏好，不创建 task-scoped route decision。

### 0.3 验证运行上下文

默认 `context=interactive`。仅在调用方声称来自 auto-loop 时，通过 runner `status` / `next` 验证：

- run 为 `running`；
- 当前 task 与本次检查任务一致；
- outstanding action 为 `run_check_all` 或 `run_recheck`。

聊天摘要、自然语言或 raw runtime JSON 不能代替 runner。验证失败时报告原因并按 interactive 处理。

### 0.4 解析请求深度

`requested_depth` 只允许 `auto`、`light`、`full`，优先级固定为：

1. 当前用户请求里最新的显式深度意图；
2. validated auto-loop action 的 `requested_check_depth`；
3. 默认 `auto`。

显式意图按语义识别：`简单检查`、`轻量检查`、`light check` 表示 light；`全面检查`、`全量检查`、`最终检查`、`提交前检查`、`full check` 表示 full。同一请求出现多次切换时，以最后一次明确表达为准。单独说 `check` / `check-all` 只是调用统一入口，不自动等同 full。

历史 auto-loop 缺少深度字段时 runner 返回 `full`。文件数、diff 行数或“看起来简单”不能单独决定 light。

### 0.5 选择有效深度

按以下顺序生成检查画像：

```yaml
check_profile:
  context: interactive | auto-loop
  requested_depth: auto | light | full
  effective_depth: light | full
  confidence: high | fallback-full | escalated
  reasons: [string]
```

决策顺序：

1. `requested=full` -> `effective=full`。
2. 命中任一 hard-full -> `effective=full`；若请求为 light，使用 `confidence=escalated` 并记录原因。
3. `requested=light` 且无 hard-full -> `effective=light`。
4. `requested=auto` 且高置信满足全部 light eligibility -> `effective=light`。
5. 其它情况 -> `effective=full`、`confidence=fallback-full`；不询问用户。

**hard-full 信号**：

hard-full 只看行为契约变化和影响面是否闭合。文件载体或主题域本身不构成 hard-full；workflow、skill、command、hook、生成快照或安装材料若只改解释文字或机械投影，仍继续判断 light eligibility。以下任一成立才命中：

- 公共 API、CLI、schema、持久化状态、协议字段、缓存、迁移或历史数据兼容发生行为变化；
- 权限、安全、资金、并发、时序、状态机、回滚、发布或 Git 控制门禁发生行为变化；
- 改动跨越独立行为边界，或直接引用点、状态传播或回归路径无法完整列出；
- 正在重检既有 full `CHK-*` / `FBK-*` 修复结果；
- light 执行中发现未知 dirty path、真实影响面扩大或关键验证缺口。

无法确认是否改变行为契约或影响面是否闭合时，使用 `effective=full`、`confidence=fallback-full`。

**light eligibility 必须全部满足**：

- 变更属于闭合的单一语义范围；同一真实源的多个机械投影仍算一个语义范围；
- 无行为性 hard-full 信号；
- 受影响规划条目、直接引用点、状态传播和回归路径可穷举；
- 局部行为修改时，直接引用点和回归路径可穷举，并有可运行的定向验证；无行为变化时，仅涉及注释、错别字、排版、解释文字、示例或机械投影同步；
- 不在既有 full 修复/重检链中。

light 执行中命中 hard-full 时，立即单向升级 full 并补齐所有适用维度；同一修复/重检循环内 full 不得降级。
