---
name: trellis-push
description: "按确认的精确文件范围提交普通变更或完成已就绪的 merge commit；多仓计划可包含已展示的本地生成命令，并在普通推送后同步当前任务记录与进度。"
---

# Trellis Push

`trellis-push` 是 Phase 3.4 唯一的代码提交入口。它只负责生成最小计划、精确提交、普通推送，以及在 task 上触发进度同步或在 untracked 上完成状态清理。

## 职责边界

- 普通模式默认 `commit + push`。
- 普通多仓计划可以包含本地确定性生成命令；生成后没有新增计划外文件时沿用同一次确认。
- 普通模式把当前任务产物与更新后的 `task.json` 纳入同一次确认下的独立任务记录提交。
- 用户明确要求“只提交不推送”时使用 `commit-only`。
- auto-loop 可调用内部 `commit-only`，复用本 skill 的仓库发现、动态多仓计划、确定性本地生成、精确提交和失败保留能力；不再次确认、不 push，也不执行 Step 5 的任务进度写入、进度 commit 或 progress push。Auto-Loop runner 仍按自己的状态契约写入本地 `task.json.progress` 与本地完成态（`status=completed` + `completedAt`）。
- 不发起、终止或解决分支合并；只允许普通模式完成已经开始、冲突已清零且索引完全可归属的 merge commit。
- 不处理上线核对、任务归档、会话日志或自动任务队列状态。
- 不使用 `git add .`、`git add -A`，不要求工作区整体干净，也不提交计划外文件。
- untracked 上下文只接受 `stage=push`；该状态只负责路由，不替代本 skill 的正式计划、确认和 Git 安全检查，也不生成任务进度提交。

## 模式

| 模式 | 确认 | Git 动作 | 进度同步 |
| --- | --- | --- | --- |
| 普通 | 展示最小计划并确认一次 | exact commit；已有 merge 就绪时完成双父提交；然后 push | 有活动任务时立即同步 |
| 用户 `commit-only` | 展示最小计划并确认一次 | exact local commit | 跳过 |
| auto-loop 内部 `commit-only` | 复用 auto-loop 预授权 | exact local commit chain | 由 Auto-Loop runner 写本地 progress 与本地完成态；本 skill 跳过 Step 5 |

内部 `commit-only` 不接受超出当前任务证据、runner owned dirty 和 protected-retained 边界的文件，不执行远端推送或其他附加动作。安全条件不满足时返回失败，由调用方决定后续状态。

## Step 0：记录完成链证据

除 auto-loop 内部 `commit-only` 外，普通 push 或用户 `commit-only` 已经构成明确 Git 意图。本 skill 在读取 Git 提交计划前只记录当前可用的完成链证据，不补跑、不切换阶段，也不新增确认：

- Check-All：根据当前标准报告与实际 diff 标记为 `通过`、`通过（已接受风险）`、`未运行`、`已失效`、`存在未处置 findings`、`blocked` 或 `部分验证`。剩余 `CHK-*` 与 `FBK-*` 均为 0 时标记为 `通过`；所有剩余问题都有当前有效的用户风险接受时标记为 `通过（已接受风险）`，并保留问题 ID 与严重度。没有可验证的当前报告时使用 `未运行`，不得从历史消息、摘要或 dirty 状态猜测通过或风险接受。
- Update-Spec：根据当前 `spec_update_result` 与实际 diff 标记为 `no-op`、`written`、`needs-review`、`未运行` 或 `已失效`。结果缺失或无法证明仍适用于当前 diff 时使用 `未运行` / `已失效`。

上述状态只进入 Step 3 的完成链证据与风险展示，不会阻止读取 Git 状态或生成提交计划。本步骤不得返回 Phase 2.2，不得加载 `trellis-check-all` 或 `trellis-update-spec`，也不得要求用户改写成“跳过检查后 push”。正常 workflow 的 Check-All -> Update-Spec -> Push 顺序仍由 Phase 2.2、Phase 3.3 和各自 owner 推进；`trellis-push` 不反向补做上游阶段。

auto-loop 内部 `commit-only` 已由 runner 的 `run_check_all -> run_spec_update -> commit_only` 状态机和预授权保证顺序，因此不重复记录或判断本交互证据。

没有活动 task 时运行 `python3 ./.trellis/scripts/untracked_flow.py status --verbose`。命中 untracked 后，记录 work id、summary 和 stage，并要求 `stage=push`；完成链证据仍从当前 Check-All / Update-Spec 结果与实际 diff 获取。`miss` 才按既有“无活动任务”普通 Git 路径处理；损坏状态停止，不从摘要猜测。游标命中不表示 Push 已计划、已确认或已执行。

## Step 1：发现仓库与任务

候选仓库包括：

- 含 `.trellis/` 的父仓根目录。
- `.trellis/config.yaml` 中 package 路径对应的独立 Git root。
- 用户明确指定的候选仓库。

同一个 Git root 只保留一次。位于父仓内部但不是独立 Git root 的 package 变更归父仓处理。

为每个候选仓库生成用户可见名称：优先使用 `.trellis/config.yaml` 中匹配的 package 名；没有配置时使用 Git top-level 目录名。`root`、`parent`、`main repo` 只允许作为输入别名，禁止直接显示在计划或结果中。

活动 task 或 untracked work 都是可选上下文：

```bash
python3 ./.trellis/scripts/task.py current --source || true
python3 ./.trellis/scripts/task_progress.py status --json || true
```

存在活动任务时，必须额外获取文件级任务状态：

```bash
git status --short --untracked-files=all -- <task-dir>
```

若 `task_progress.py status` 返回 `taskStatus=completed`，立即按需读取 `references/completed-task-recovery.md`，由该 reference 完成只读 preflight 并返回“恢复计划 / 显式 finish-work / 阻断”之一。在得到结果前不得进入普通业务规划，也不得重复已经成功的业务 Git 动作。reference 缺失、不可读或证据无法闭合时失败关闭；`task.json.progress` 只作诊断，不能单独选择恢复动作。

不得把默认 `git status --short` 可能返回的 `?? <task-dir>/` 折叠目录当成 exact file、展示条目或 pathspec。无活动 task 时仍可提交相关代码，但不生成任务进度。untracked 命中时，结合当前请求、work summary 和实际 diff 判断业务 `planned` 文件归属，计划同时显示 work id；无法明确归属的文件只能保留或作为风险。存在活动 task 时，结合 `brief.md`、`implement.md`、当前 diff 与本轮执行范围生成一行语义进度；同时识别当前任务目录中已存在且可归属的 dirty/untracked 产物，供 Step 5 生成任务记录 exact files。不得从旧进度推断 Git 动作。

## Step 2：预检与文件归属

对每个候选仓库读取：

```bash
git status --short
git branch --show-current
git rev-parse --abbrev-ref --symbolic-full-name @{u} 2>/dev/null || true
git diff --stat
git diff --name-only
git diff --cached --stat
git diff --cached --name-only
git diff --cached --check
git ls-files -u
git rev-parse --verify MERGE_HEAD 2>/dev/null || true
git log --oneline -5
git log @{u}..HEAD --oneline 2>/dev/null || true
```

停止条件：

- detached HEAD、分支不可读、未解决冲突、rebase、cherry-pick、revert 或其它非 merge 的未完成 Git 集成状态。
- `MERGE_HEAD` 存在时仅普通模式可继续，并且必须固定当前 `HEAD` / `MERGE_HEAD`、确认 `git ls-files -u` 为空、全部 staged paths 都属于 planned 且没有 retained staged；否则停止。用户/auto-loop `commit-only` 不适用。
- 普通推送会携带无法归属本次任务的历史 ahead commits。
- 无法确定 planned file 是否属于当前请求或活动任务。
- 内部 `commit-only` 发现 staged 区非空。

业务 Git 文件分为两组；普通模式的当前任务记录 exact files 按下方独立提交规则处理：

- `planned`：本轮明确归属且准备提交的 exact files。
- `retained`：当前存在、但本次明确不提交并保持原状的 dirty paths，包含计划外 untracked、unstaged、staged 文件。clean files 不进入该集合。

普通模式存在活动任务时，当前任务目录中已存在且可归属的 dirty/untracked 产物不进入业务 `planned`，也不进入 `retained`；它们与预计由 helper 更新的 `<task-dir>/task.json` 组成 Step 5 的任务记录 exact files。其他任务目录和无法归属当前任务的文件仍属于 `retained` 或风险，不得顺带提交。

普通 `PUSH` 或 auto-loop 内部 `commit-only` 需要在仓库间运行本地生成命令时，计划必须包含命令、工作目录、依赖顺序和后续仓预计 exact files。执行链可包含任意数量的仓库和生成步骤，不硬编码具体仓库、两仓三阶段或命令名称。

动态执行链按以下证据优先级生成：

1. 当前任务 `design.md` / `implement.md` 明确记录的顺序、命令和路径。
2. 项目 SOP/spec 中的 canonical、生成和分发约定。
3. 受版本控制的 `package.json`、Makefile 或仓库脚本入口，以及可确认的输入输出路径。
4. 可验证的 Git/submodule 父子关系。

上述优先级用于发现意图和执行顺序，不允许用文档覆盖当前仓库事实。命令入口、工作目录和输出路径必须由受版本控制内容验证；任务 artifacts、SOP/spec、脚本实际行为或 Git 关系互相冲突时失败关闭。

命令必须是受版本控制的稳定入口，并且本地、确定性、可重复、无外部副作用。工作目录和预期影响路径必须可审计；只有名称相似、mtime、目录邻近或惯例不足以执行。禁止任意 shell 字符串、管道、重定向、命令替换、push、release、deploy、archive、凭证和生产数据操作；证据不足时失败关闭。

`retained` 只是内部集合名。用户可见输出统一写“保留未提交的变更（dirty）”，并逐项标注 `[untracked]`、`[unstaged]`、`[staged]`。unknown ahead、branch/upstream 异常、归属不确定等真正需要处理的事项单独进入“风险”区；普通 retained dirty 不默认视为阻塞。

普通模式允许 `retained` 存在。执行前记录计划外 staged set，提交后确认这些 staged 文件仍保持原状。用户明确要求新增文件时，重新生成计划并确认，不能在执行中静默扩大范围。

auto-loop 内部 `commit-only` 也允许 retained dirty 存在，但每个生成/提交步骤前后都必须验证 retained exact paths 的内容摘要不变，并确认它们与 planned/generated paths 不冲突。内部模式仍要求 staged 区为空；计划外 dirty、retained 漂移、未知 staged、无法由当前计划或已记录提交解释的分支/HEAD 漂移，或归属歧义立即停止后续副作用。

已有 merge 会提交整个索引，因此 planned 必须覆盖全部 staged paths，`retained` 中不得存在 `[staged]`；未跟踪或未暂存 retained 仍可保留。

## Step 3：展示最小计划

确认前禁止 `git add`、`git commit` 或 `git push`。

普通模式或用户 `commit-only` 在所有计划数据已经收敛、即将展示用户可见计划时，必须即时读取 `references/output-templates.md` 的“计划模板”和“共用展示规则”，再按该 reference 渲染。不得在 Skill 入口、仓库发现或预检阶段提前加载该文件；每次实际计划输出都以这次即时读取为准。

reference 缺失、无法读取或缺少对应章节时停止并报告 `阻塞`，不得凭记忆重建、缩写或自制替代模板。

普通多仓只确认一次。计划已展示生成命令和预计 exact files 时，命令成功且没有出现预计列表外的新 dirty path 就沿用原确认；内容、hash 或统计变化不重问。其它计划边界变化仍按 Step 4 重新规划。

auto-loop 内部 `commit-only` 不渲染交互式计划或结果，也不再次询问用户，因此不得为了内部执行读取 `references/output-templates.md`。它仍生成同样的逐仓执行数据用于自检、恢复和调用方结果记录，并且只能在当前任务 artifacts、runner owned dirty 和 protected-retained 边界内形成 exact files/message。

## Step 4：精确提交与推送

每个仓库按计划顺序执行。执行前重新检查 planned files、当前分支、HEAD、upstream、冲突状态、staged、全部 dirty paths 和 retained 摘要；任一关键条件变化都停止当前执行并重新规划。普通模式仅 `retained` 内容变化时可更新说明；auto-loop 内部模式的 retained 内容必须保持不变。

计划包含本地生成命令时，前置仓成功后按计划执行命令，再复用本节现有预检。命令成功、后续仓全部 dirty paths 都在预计 exact files 内且 retained 摘要未漂移时直接继续；否则停止并重新生成计划。预计文件最终 clean 时不强行提交。

auto-loop retry/resume 时，读取调用方提供的已完成仓库提交，逐个验证 repository、commit object、message 和文件集合仍符合当前任务证据，并确认当前分支/HEAD 变化可由这些提交解释。验证通过的提交直接跳过；验证失败立即 blocked，不重复提交。确定性生成入口可以安全重跑，以当前 Git 状态重新规划后续步骤。

普通精确提交：

```bash
git add -- <exact planned files>
git commit --only -m "<confirmed message>" -- <exact planned files>
```

提交后验证：

```bash
git show --name-only --format= HEAD
git diff --cached --name-only
```

commit 只能包含 planned files，执行前的计划外 staged set 必须仍保留。

已有 merge：

```bash
pre_merge_head="$(git rev-parse HEAD)"
merge_head="$(git rev-parse MERGE_HEAD)"
git add -- <exact existing planned files>
git add -u -- <exact deleted planned files not already staged>
git diff --cached --check
git commit -m "<confirmed message>"
```

merge 中的 `git commit` 不能携带 pathspec。现存 planned 使用 `git add --`；已删除 planned 仅在尚未进入 cached 集合时使用 `git add -u --`。提交前确认 cached path set 与 confirmed planned files 完全相等、`git ls-files -u` 为空且 `git diff --cached --check` 通过。提交后验证：

```bash
git rev-list --parents -n 1 HEAD
git diff-tree --no-commit-id --name-only -r HEAD^1 HEAD
```

结果必须恰好有两个父提交，顺序为记录的 `pre_merge_head`、`merge_head`；first-parent 文件集合必须等于 confirmed planned files。任一验证失败都停止 push，不自动重写提交。

普通模式继续推送当前分支：

```bash
git push origin <current-branch>
```

已有 upstream 且远端名称不是 `origin` 时，使用实际 upstream remote。无 upstream 时只能在计划中明确将当前分支设置到选定 remote；不能猜测目标分支。

`commit-only` 到本地提交成功即结束，不推送，也不写远端任务进度。

多仓执行失败时停止后续未开始仓库，保留已经成功的提交/推送，不做回滚。

auto-loop 内部链失败时向调用方返回全部已完成仓库提交和失败位置。只有确定性生成未收敛或仍可安全重新规划的本地预检使用 `commit-repairable`；计划外 dirty、retained 漂移、未知 staged、无法由当前计划或已记录提交解释的分支/HEAD 漂移、归属歧义和外部副作用风险必须立即 blocked。不得 reset、rebase、revert、amend 或撤销成功提交。

## Step 5：同步任务进度

仅普通模式且存在活动 task 时由本 skill 执行。untracked、用户 `commit-only` 与 auto-loop 内部 `commit-only` 都跳过本 Step；Auto-Loop runner 在 action record/next 后按自身契约写入本地 `task.json.progress`，不属于这里的任务记录提交或推送。全部业务仓库成功后一次原子写入最终 progress 与完成态，再提交并推送任务记录；已有仓库成功而后续仓库失败时只写 partial 进度，明确 completed、失败位置、next 和 notes，状态保持 `in_progress`。尚未发生成功 Git 动作就失败时，不记录虚假的 completed steps；只有父仓仍可安全提交并推送时才允许记录 failure notes。

新进度固定为：

```json
{
  "updatedAt": "<ISO 8601>",
  "completedSteps": ["<已完成步骤>"],
  "partialStep": "<部分完成步骤或 null>",
  "nextStep": "<下一步>",
  "notes": "<可选说明；无说明时为空字符串>"
}
```

进度不得保存本轮模式、业务 commit hash 或提交计划。

写入前确认：

- 当前任务 exact files 与首次确认的路径集合一致，没有新增当前任务路径或无法归属的 dirty 内容。
- 父仓分支、upstream 和冲突状态安全。
- 推送不会携带无法归属的历史 ahead commits。

全部业务 commit/push 成功时，通过 helper 用同一份最终 progress 原子写入 `progress`、`status=completed` 与 `completedAt`：

```bash
python3 ./.trellis/scripts/task_progress.py write \
  --task <task-dir> \
  --progress-json '<progress-json>' \
  --complete \
  --json
```

部分成功时调用同一 helper，但不得携带 `--complete`，并写入精确恢复位置。用户 `commit-only`、auto-loop 内部 `commit-only` 和尚未发生任何成功业务 Git 动作的失败都不得由本 skill 请求 complete；auto-loop 的本地完成态由 Auto-Loop runner 自己写入，不经过本步骤。helper 写入失败时任务保持原状态，不得继续任务记录提交或报告完成。

helper 成功后，只提交并推送首次确认的当前任务 exact files；该集合包含完成态 `task.json`，以及首次计划时已存在且可归属的当前任务 dirty/untracked 产物：

```bash
git add -- <current-task-exact-files>
git commit --only -m "chore(task): update <task-name> progress" -- <current-task-exact-files>
git push origin <current-branch>
```

该动作属于用户已确认的普通 push 计划，不增加第二次确认。提交后必须验证 commit 只包含首次确认的当前任务 exact files，且 `task.json` 已包含同一份最终 progress、`status=completed` 与 `completedAt`；其他任务和无关 dirty/staged 文件保持原状。

失败时保留真实现场，不 reset、amend、revert 或制造 dirty 回滚：

- helper 失败：任务保持 `in_progress`，不得创建任务记录 commit。
- helper 成功但任务记录 commit 失败：保留本地 `completed` 与当前任务 exact dirty，后续按 Step 1 的任务记录 commit 恢复路径重新验证和确认；不得重复业务提交或 helper 写入。
- 任务记录 commit 成功但 push 失败：任务目录应为 clean，并保留可归属的 ahead commit；后续只重试该 commit 的 push，不重复业务提交、helper 写入或任务记录 commit。
- 任务记录 push 成功：本任务产生的当前任务目录变更必须 clean；不得再写入第二份预归档完成态。

任何恢复都必须验证当前分支、upstream、HEAD、`@{u}..HEAD`、任务记录 commit message 与 exact file set，以及 `task.json` 的最终完成态。无法证明归属时停止，不把未知 ahead 或 dirty 当作可恢复任务记录。

## Step 6：结果

untracked 的全部已确认 Git 动作成功后，最后运行 `python3 ./.trellis/scripts/untracked_flow.py clear --reason completed --work-id <work-id>`。清理成功才报告完成链已结束；任一仓库、push 或清理失败都保留状态并报告恢复位置，禁止因部分成功伪造完成。用户 `commit-only` 的已确认动作全部成功时同样可以完成并清理。

普通模式、用户 `commit-only` 或 untracked 路径在即将展示用户可见结果时，必须再次即时读取 `references/output-templates.md` 的“共用展示规则”、“结果模板”和“结果补充规则”，再按该 reference 渲染。不得依赖 Step 3 曾经读取的模板仍在上下文中。

reference 缺失、无法读取或缺少对应章节时停止并报告 `阻塞`，不得凭记忆重建、缩写或自制替代结果。auto-loop 内部 `commit-only` 不读取或渲染该交互式结果模板，只按 Step 4 的失败保留契约向调用方返回逐仓 commits、files、retained、message 和失败位置，由 `trellis-auto-loop` 完成 `record + next`。

## 禁止事项

- 扩大到计划外文件或要求清理无关工作区。
- 把普通 push 中可归属当前活动任务的规划产物列为 retained，并以“finish-work 归档时再入库”为由延后首次记录。
- 执行首次计划未展示的生成命令，或生成计划外文件后仍沿用旧确认。
- 用任务进度决定是否推送代码。
- 在本 skill 内发起、终止、解决冲突或改变分支合并目标；只允许完成已就绪的 merge commit。
- 自动解决 push rejection、冲突、凭证或远端保护规则问题。
- 在业务失败后伪造已完成进度，或因进度同步失败回滚业务提交。
