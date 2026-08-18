# Completed Task Recovery

本 reference 只在 `task_progress.py status` 返回 `taskStatus=completed` 时加载。它是普通任务记录发布恢复的唯一详细语义 owner；`trellis-continue`、completed workflow-state 和 `trellis-finish-work` 不复制本分支矩阵。

## Evidence

固定当前任务路径与 `task.json`，读取文件级任务状态、当前分支、upstream、`HEAD`、`@{u}..HEAD` 的提交消息与文件集合，以及 `python3 ./.trellis/scripts/auto_loop.py status --verbose`。所有恢复都必须验证 exact task、最终 progress、`status=completed`、分支和提交归属；缺失 `completedAt` 只记为待归档补写的审计元数据，不单独阻断恢复。progress 文本不能替代 runtime 或 Git 证据。

## Outcomes

按以下优先级只返回一个结果：

1. **显式 finish-work，auto-loop**：健康的终态或 recent auto-loop run 的 `pending_archive.tasks_awaiting_archive` 精确包含当前任务，记录的本地提交仍可验证，且任务 dirty 仅为 runner 在提交后写入的 `<task-dir>/task.json` progress/lifecycle bookkeeping。停止 Push，不得把该本地完成态改成普通远端 push。
2. **任务记录 commit + push 恢复计划**：没有有效 auto-loop handoff；当前任务 exact files 仍 dirty，`task.json` 已包含合法最终 progress 与 `status=completed`，且文件集合可由首次确认或重新确认闭合。缺失 `completedAt` 时不得重复 helper 写入，由后续 archive 补写；不得重复业务提交。
3. **任务记录 push-only 恢复计划**：当前任务目录 clean；upstream 存在；`@{u}..HEAD` 中存在消息、exact file set 和完成态均可归属的任务记录 commit。只推送该已存在提交，不创建新 commit。
4. **显式 finish-work，普通已同步**：当前任务目录 clean；upstream 存在；`@{u}..HEAD` 没有提交修改当前任务。普通任务记录已经同步，不再执行 Push。
5. **阻断**：runtime 与 Git 矛盾、auto-loop marker 无健康 handoff、缺少普通路径 upstream、任务 dirty 超出 exact files、未知 ahead 修改任务，或提交消息/文件集合/分支无法闭合。报告具体证据缺口，不 push、不归档，也不猜测完成来源。

恢复计划仍使用 `trellis-push` 的既有一次确认、执行前漂移检查和结果模板。这里只决定完成态恢复范围，不新增状态、持久化字段或自动确认。
