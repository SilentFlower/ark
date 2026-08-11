---
name: trellis-create-command
description: "Create a new trellis entry as command or skill; writes agents copy and optionally skill-garden."
---
# Create New Trellis Entry

创建一个新的 trellis 入口。支持两种形态：斜杠命令（command）或 Claude skill。

> **0.6 取舍提示**：skill-garden 0.6 包默认全部 skill 化，不再维护 `.claude/commands/` 目录。形态选 command 时，**只在目标项目落 2 份**，不分发到 skill-garden 0.6 包（强制分发时需手动维护，不建议）。新建入口推荐选 skill 形态。

---

## 前置参考：trellis-meta

> **本 skill 是流程骨架，深层架构 / 命名 / 文件分布以 `trellis-meta` 为准。**

`trellis-meta` 是 Trellis 本地架构的权威参考 skill（类似 `.trellis/spec/guides/`，但范围是 trellis 自身的架构与定制入口）。创建新入口**前**，按以下顺序加载它的 references 作为决策依据：

| 决策点 | trellis-meta 参考 |
|--------|------------------|
| skill / command / prompt / workflow 形态怎么选 | `references/platform-files/skills-and-commands.md` |
| skill / command 的目录、frontmatter、命名、hook 联动 | `references/customize-local/change-skills-or-commands.md` |
| 想加入 hook / 改 settings | `references/customize-local/change-hooks.md` + `references/platform-files/hooks-and-settings.md` |
| 想改 agent（implement / check / research）行为 | `references/customize-local/change-agents.md` + `references/platform-files/agents.md` |
| 想改 workflow phase / breadcrumb / routing | `references/customize-local/change-workflow.md` + `references/local-architecture/workflow.md` |
| 想改 task lifecycle / spec 结构 / context 注入 | `references/customize-local/change-task-lifecycle.md` / `change-spec-structure.md` / `change-context-loading.md` |
| 平台目录映射（.claude / .codex / .cursor / ...） | `references/platform-files/platform-map.md` |

**操作建议**：

1. 在执行 Step 0 之前，先调用 `Skill({skill: "trellis-meta"})` 加载基础架构视图
2. 根据本次要创建的入口类型，按需 `Read` 上表中对应的 references 子文档
3. 本 SKILL.md 下面的 Step 1-8 是**操作流程**；架构 / 命名 / 边界冲突时**以 trellis-meta 为准**

> **不要凭空生成**：trellis-meta 的 references 已经把可定制入口、命名约定、目录结构、hook 注入点全列清楚，create-command 的产物必须落在这套结构内，不得发明新目录或新形态。

---

## 适用场景

- 给项目加一个新 trellis 工具入口
- 现有命令集不覆盖某类需求，需要扩展
- 从零设计新的工作流步骤

---

## 执行步骤

### Step 0: 确定路径

**`<target>`（落地项目）**：默认 = 当前工作目录（`pwd`）。

- 若用户没有明确说"在其他项目"，直接用当前项目
- 若用户明确要在其他项目创建，改用其指定的绝对路径；本 skill 不主动 `cd`，只记住该路径用于后续写入
- 若目标项目就是 skill-garden 本身（在 skill-garden 仓库里 `create-command`），`<target>` 与 `<skill-garden>` 指同一路径，**避免重复写入**（只写一次）

**`<skill-garden>`（分发源，scope 含 skill-garden 时必需）**：默认 `/root/project/skill-garden`。

- 路径不存在或与用户期望不符 → 询问用户确认
- 无需分发时（scope = 只装本项目）整段忽略

### Step 1: 收集基本信息

与用户确认：

| 项 | 说明 | 示例 |
|----|------|------|
| **name** | kebab-case，动词或动词短语开头 | `review-pr`、`sync-data`、`check-deps` |
| **description** | 要完成什么、产出什么 | "检查 PR 代码变更是否符合项目规范" |
| **scope** | 只装本项目 / 也装 skill-garden | 问用户：是否要分发到其他项目？ |

### Step 2: 选形态（command vs skill）

| 形态 | 何时选 | 触发方式 |
|------|--------|---------|
| **command** | 显式动作、高风险、需确认点（如 finish-work、continue） | `/trellis:<name>` |
| **skill** | 自然语可触发、查询 / 分析 / 检查、低破坏性（如 check-all、extract-prd、visualize） | Claude 自动路由 + `/trellis-<name>` |

决定不了时**推荐 skill**：自然语路由更灵活，显式斜杠仍可用。反过来，后悔做成 skill 想改 command 比较费事。

**0.6 偏好**：0.6 主推 skill 化，新建入口默认 skill 形态；选 command 形态时确认是否有不可替代的"显式确认"诉求。

### Step 3: 生成内容骨架

按形态和复杂度生成初稿：

**简单 skill / command**（< 50 行）：

```markdown
<frontmatter 仅 skill 有>
# <标题>

<1-2 行简介>

## 适用场景
- <触发点 1>
- <触发点 2>

## 执行步骤
### Step 1: <动作>
### Step 2: <动作>

## 反模式
- ❌ <误用 1>
- ❌ <误用 2>
```

**复杂 skill / command**（50-300 行）：

```markdown
<frontmatter>
# <标题>

<简介>

## 适用场景 / 前置条件

## 执行步骤
### Step 0-N: <动作，每步可执行，带 bash/代码示例>

## 输出模板（必需）
<markdown 模板>

## 核心原则
<5 条左右，每条有 why>

## 反模式
<5-7 条具体误用>
```

> **骨架风格参考**：读一个已有的 skill（如 `trellis-extract-prd` / `trellis-check-all` / `trellis-verify-task`）作风格锚点，保持项目内一致。

### Step 4: Frontmatter 规则（skill 形态必需）

**skill 版 frontmatter**：

```yaml
---
name: trellis-<name>
description: "<what> <when> <exclusion>"
---
```

`description` 写法取决于**触发策略**：

| 策略 | 长度 | 写法 |
|------|------|------|
| **Auto-routing**（常用） | 80–300 字 | 三段式：What（动词开头）+ When（中英触发词）+ Exclusion（"For X, use Y instead"）。Claude 按此匹配用户意图自动加载 |
| **Manual-only**（偶尔用的管理工具） | 15–20 tokens | 单句动作 + 产出物。只靠用户 `/trellis-<X>` 显式触发，description 不作路由依据，节省每次对话的 skill 列表 token |

**选哪种**：
- 选 **Auto-routing** 当 skill 需要在对话中被自然语触发（如 `trellis-extract-prd`、`trellis-verify-task`、`trellis-check-all`）
- 选 **Manual-only** 当 skill 用法明确、频率低、不希望占用每次对话 skill 列表 token（如 `trellis-create-command` 自身、`trellis-plan-version`）

**agents 版 frontmatter**：正文 + frontmatter 与 skill 版完全一致（包括 `name: trellis-<name>`），只是落在 `.agents/skills/trellis-<name>/` 目录。

```yaml
---
name: trellis-<name>
description: "<与 skill 版一致>"
---
```

**command 版**：无 frontmatter，纯 markdown。

### Step 5: 写入副本

**不变量（必须遵守）**：每个 trellis 入口都成对存在 —— `.claude/<commands 或 skills>/...` 主副本 + `.agents/skills/trellis-<name>/SKILL.md` 镜像副本。漏写任一份会破坏 skill-garden install.sh 的对称分发。scope 含 skill-garden 时，skill 形态需要在 `<skill-garden>/.trellis/0.6/` 下各写一份（共 4 份）；command 形态在 0.6 中不再分发到 skill-garden（见下表注）。

按形态决定落盘位置：

**形态 = skill**（2 份 / scope 为 skill-garden 时 4 份）：

| 位置 | frontmatter name |
|------|------------------|
| `<target>/.claude/skills/trellis-<name>/SKILL.md` | `trellis-<name>`（主副本） |
| `<target>/.agents/skills/trellis-<name>/SKILL.md` | `trellis-<name>`（镜像，body + frontmatter 完全同主副本） |
| `<skill-garden>/.trellis/0.6/.claude/skills/trellis-<name>/SKILL.md` | `trellis-<name>` |
| `<skill-garden>/.trellis/0.6/.agents/skills/trellis-<name>/SKILL.md` | `trellis-<name>` |

**形态 = command**（仅 target 2 份；scope = skill-garden 也不分发到 0.6 包）：

| 位置 | 格式 |
|------|------|
| `<target>/.claude/commands/trellis/<name>.md` | 无 frontmatter（主副本） |
| `<target>/.agents/skills/trellis-<name>/SKILL.md` | 带 frontmatter，`name: trellis-<name>`（镜像） |

> **0.6 不分发 command 到 skill-garden**：0.6 包目录树没有 `.claude/commands/`，install.sh 也不会处理。如果你确实希望 command 形态分发给其他项目使用，请：(a) 改用 skill 形态，或 (b) 把该 command 同时放到 0.5 包（向下兼容用户），或 (c) 在 skill-garden 包外用其他机制分发。

**同步技巧**：写完主版（`.claude/skills/trellis-<X>/SKILL.md` 或 `.claude/commands/trellis/<X>.md`）后，用 `cp` 派生其他副本：

```bash
# 主副本写完后，副本内容完全一致，直接 cp 即可
cp <target>/.claude/skills/trellis-<X>/SKILL.md <target>/.agents/skills/trellis-<X>/SKILL.md

# 对 skill-garden 同步（仅 skill 形态、scope 为 skill-garden 时）
cp -r <target>/.claude/skills/trellis-<X> <skill-garden>/.trellis/0.6/.claude/skills/
cp -r <target>/.agents/skills/trellis-<X> <skill-garden>/.trellis/0.6/.agents/skills/
```

### Step 6: 更新 skill-garden README（scope = skill-garden 时）

必改：
- "0.6+ 推荐技能" 表追加一行（名称 / 形态 / 说明 / 使用时机）
- "0.6+ 全部技能" 段同步更新计数

可选：
- 如引入新形态约定或命名前缀：更新"新增 Trellis 技能"指引

### Step 7: 验证

| 检查项 | 方法 |
|-------|------|
| 新 skill 出现在 Claude skill list | 读 `<available-skills>` 区，确认 `trellis-<X>` 存在且 description 完整 |
| 新 command 出现在 slash 列表 | 下拉 `/trellis:` 能看到 `<name>` |
| scope=skill-garden：install 端到端 | `rm -rf /tmp/sg-test && mkdir -p /tmp/sg-test/.trellis && echo "0.6.0-beta.8" > /tmp/sg-test/.trellis/.version && bash <skill-garden>/scripts/install.sh /tmp/sg-test <X>` |
| 副本内容一致 | `wc -l` 行数一致；关键段落 `diff` 确认 |

### Step 8: 输出确认

```markdown
✓ 已创建 trellis 入口：<name>

- **形态**：<command | skill>
- **范围**：<本项目 | 本项目 + skill-garden 0.6>

### 副本

- `<target>/.claude/<path>`
- `<target>/.agents/skills/trellis-<X>/SKILL.md`
- `<skill-garden>/.trellis/0.6/` 同步位置 × 2（仅 skill 形态时显示）

### 触发方式

- **自然语**：<触发词例子>
- **显式**：<`/trellis:<X>` 或 `/trellis-<X>`>

### 下一步建议

- 在当前对话试一次触发，观察 Claude 是否正确路由
- 触发失败时调整 description（精准化 when/exclusion）
- 内容有遗漏时补充 Step 或 checklist
```

字段行与分组条目必须使用 Markdown 列表项。`•` 是普通字符，Markdown 不识别，缩进两格也不构成代码块，整块会折叠成一段；不得用 `•` 或裸段落行代替列表。

---

## 命名约定

| 类型 | 前缀 | 示例 |
|------|------|------|
| 会话生命周期 | — | `continue` / `finish-work` |
| Pre-development | `before-` | `before-dev` |
| Check | `check-` | `check-all` |
| Verify | `verify-` | `verify-task` |
| Extract（有源严格提取） | `extract-` | `extract-prd` |
| Create / generate（从零创建） | `create-` | `create-command` |
| Analyze | `analyze-` | `analyze-task` |
| Sync / update | `sync-` / `update-` | `sync-prd` / `update-spec` |
| 动作类 | 动词开头 | `push` / `visualize` |

> **0.6 命名取舍**：「严格提取」类用 `extract-` 前缀（强调原文不动、禁止发挥）；「从零创建」类才用 `create-` 前缀（如 create-command）。区分这两者能让 AI 在触发时不混淆生成 vs 提取的语义。

### 命名反模式

- ❌ 不用 kebab-case（不要 `reviewPr` / `review_pr`）
- ❌ 名字过于笼统（`tool` / `helper` / `util`）
- ❌ 与现有命令冲突（先 `ls .claude/commands/trellis` 和 `.claude/skills/` 确认）
- ❌ skill 写文件时漏 `trellis-` 前缀（影响自动路由分组）
- ❌ "严格提取"语义的入口用 `create-` 前缀（应该用 `extract-`，避免 AI 误判为生成型）

---

## 内容写作约束

- skill body 默认 < 300 行；超过说明该拆分或融合
- 每个 Step 要有**具体可执行动作**（读文件、grep、写文件），避免 "Analyze the requirements" 这种模糊词
- 输出格式必须用 markdown 模板明示
- 反模式清单必写（帮 Claude 避免常见误用）
- 中文注释 + 英文 description（description 中的触发词可中英混合）
- 引用文件路径用反引号 `.claude/...`，不写裸路径

---

## 反模式（避免）

- ❌ 写 `.cursor/commands/`（已废弃，统一 `.claude/commands/`）
- ❌ 同时创建 command 和 skill 同名入口（触发歧义，不知选哪个）
- ❌ 只写 `.claude/...`，漏了 `.agents/skills/trellis-<X>/`（skill-garden install 会不对称）
- ❌ 选 Auto-routing 策略但 description 过短（< 80 字），Claude 路由不稳；Manual-only 策略无此要求
- ❌ description 用名词开头（"A skill for..."）而不是动词开头（"Analyzes..." / "Extract..." / "Create..."）
- ❌ 不询问 scope 直接写 skill-garden（需要用户显式确认 skill-garden 路径）
- ❌ 不参考已有 skill 就凭空生成，导致风格不一致
- ❌ 写完不验证（description 语法错或 frontmatter 缩进错，Claude 静默忽略）
- ❌ command 形态硬要分发到 skill-garden 0.6 包（0.6 不维护 commands 目录，会被忽略）
- ❌ **跳过 trellis-meta 参考、凭 SKILL.md 简版描述就开写**（trellis-meta 是本地架构权威；冲突时以它为准）
- ❌ **发明 trellis-meta 未列出的新目录 / 新形态**（如自创 `.trellis/extensions/...` 这种结构；新需求应先去 trellis-meta 找现有入口）
