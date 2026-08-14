# Change Local Workflow

<!-- BEGIN skill-garden patch trellis-meta-managed-workflow-entry v0.6 -->
When the user wants to change Trellis phases, next actions, task gates, routing, checking, or wrap-up, read `.trellis/workflow.md` first because it is the current runtime contract. Before editing it, determine whether the relevant section is project-local, an upstream template, or a Skill-Garden managed Patch output.
<!-- END skill-garden patch trellis-meta-managed-workflow-entry v0.6 -->

## Read These Files First

1. `.trellis/workflow.md`
2. Entry files for the current platform, such as skills/commands/prompts/workflows
3. The current task's `task.json` and `prd.md`

## Common Needs And Edit Points

| Need | Edit point |
| --- | --- |
| Change phase names or phase order | `Phase Index` and the corresponding Phase sections. |
| Change whether to create a task when there is no task | `[workflow-state:no_task]` state block. |
| Change the next step during planning | Phase 1 and `[workflow-state:planning]`. |
| Change whether an agent is required during in_progress | Phase 2 and `[workflow-state:in_progress]`. |
| Change wrap-up after completion | Phase 3 and `[workflow-state:completed]`. |
| Change which skill a user intent triggers | `Skill Routing` table. |

<!-- BEGIN skill-garden patch trellis-meta-managed-workflow-edit-route v0.6 -->
## Modification Steps

1. Find the runtime section in `.trellis/workflow.md` and identify the owner named by the Workflow Owner Index or owning skill/helper.
2. Inspect Plugin state and nearby markers. If the section is not managed, make the narrow local edit and keep trigger/next-action semantics explicit.
3. If Skill-Garden owns the section, change the matching 0.6 Patch selector/baseline/content and Bundle policy. Do not add a parallel workflow injector or edit only the dogfood workflow.
4. Keep workflow-state tags paired, but do not duplicate a full owner procedure into the state block. State holds a one-hop gate; the owner skill/helper holds the detailed contract.
5. Synchronize affected skills, hooks, helpers, or platform entries through their own managed sources when the shared semantics require it.
6. Run source-to-snapshot sync, conflict checks, compiled target generation/check, final-output review, and idempotent dogfood application before treating the change as complete.
<!-- END skill-garden patch trellis-meta-managed-workflow-edit-route v0.6 -->
## Example: Relax Task Creation Requirements

To change when task creation can be skipped, usually edit `[workflow-state:no_task]`:

```md
[workflow-state:no_task]
Task is not required when the answer is a one-reply explanation, no files are changed, and no research is needed.
[/workflow-state:no_task]
```

If the formal Phase 1 flow also needs to change, synchronize the Phase 1 section.

## Example: One Platform Does Not Use Sub-Agents

If the user wants only one platform to avoid sub-agents, first confirm whether that platform has a separate group in the workflow. Then change Phase 2 routing for that platform group instead of deleting all `trellis-implement` / `trellis-check` instructions across platforms.

<!-- BEGIN skill-garden patch trellis-meta-managed-continue-recovery v0.6 -->
## `/trellis:continue` Recovery Ownership

`trellis-continue` owns resume decisions and uses `task_progress.py status --json` only as advisory recovery evidence. Do not maintain a second fixed route table in this reference; read the installed workflow, the owner skill, and the helper for the current version.

Stable boundaries are:

- With no active task pointer, surface each healthy candidate with its `taskStatus` and only the diagnostics needed to distinguish invalid progress or scan failures. Suggest an explicit rebind when appropriate; never bind a session automatically.
- A `planning` task returns through `trellis-brainstorm` readiness, a refreshed `brief.md`, and the task-start review gate before implementation.
- An `in_progress` task resumes from current artifacts and validated workflow evidence. Implement/check execution goes through `trellis-route`; saved progress must not infer a phase or restore Git behavior.
- A `completed` task enters the `trellis-push` completed-task preflight. That owner either prepares publication recovery, points to explicit `trellis-finish-work`, or blocks on ambiguous evidence. Rework requires an explicit reopen before implementation, and material scope changes require refreshed planning artifacts and Brief approval.

When changing recovery behavior, update `trellis-continue`, `task_progress.py`, the relevant workflow owner/state, and final-output tests together. Keep detailed progress schemas, command arguments, and error matrices in the owner skill/helper rather than duplicating them here.
<!-- END skill-garden patch trellis-meta-managed-continue-recovery v0.6 -->
## Notes

<!-- BEGIN skill-garden patch trellis-meta-managed-workflow-notes v0.6 -->
`.trellis/workflow.md` is the current runtime contract. Before editing, determine whether the target is project-local or Skill-Garden-owned. Direct edits are valid only for unowned local sections; managed sections must be changed through their canonical Patch and owner, synchronized across affected platform entries, and verified in compiled and dogfood outputs.
<!-- END skill-garden patch trellis-meta-managed-workflow-notes v0.6 -->
