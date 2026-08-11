# Local Workflow System

<!-- BEGIN skill-garden patch trellis-meta-managed-workflow-source v0.6 -->
`.trellis/workflow.md` is the runtime semantic contract for the current project. It is sufficient to understand what should happen now, but not always sufficient to locate the durable edit point: Skill-Garden-managed sections must be traced to their Patch and workflow owner before modification.
<!-- END skill-garden patch trellis-meta-managed-workflow-source v0.6 -->

## File Responsibilities

`.trellis/workflow.md` has three responsibilities:

1. **Explain workflow phases**: Plan, Execute, Finish.
2. **Define skill routing**: which skill or agent the AI should use when the user expresses a certain intent.
3. **Provide workflow-state prompt blocks**: hooks can inject the prompt block for the current state into the conversation.

## Current Phase Model

```text
Phase 1: Plan    -> clarify what to build, produce prd.md and required research
Phase 2: Execute -> implement against the PRD and specs, then check
Phase 3: Finish  -> final verification, preserve lessons, and wrap up
```

Each phase contains numbered steps, such as `1.3 Configure context`. These numbers are not runtime fields in `task.json`; they are workflow structure for AI and humans to read.

<!-- BEGIN skill-garden patch trellis-meta-managed-owner-routing v0.6 -->
## Skill Routing

Do not choose implementation or checking behavior from a static platform-capability split. Read the current Workflow Owner Index and let `trellis-route` resolve inline versus subagent execution.

Stable owner categories are:

| Gate or behavior | Owner to load |
| --- | --- |
| Request intent and project knowledge discovery | Request Triage, `trellis-start`, and the referenced router helper |
| Active task scope safety | Request Triage and the active-task scope guard |
| Untracked work completion | `workflow-state:untracked*`, Phase 2/3 owners, and `untracked_flow.py` |
| Untracked task adoption | `workflow-state:untracked*`, `trellis-brainstorm`, and `task_intent.py adopt` |
| Planning handoff and activation | `trellis-task-brief` and the task-start Brief guard |
| Implement/check execution mode | `trellis-route` |
| Unified quality verification | `trellis-check-all` |
| Automatic task loop and return gate | `trellis-auto-loop` plus the matching Check-All result |
| Executable knowledge capture | `trellis-update-spec` |
| Commit/push safety and completion activation | `trellis-push` and `task_progress.py` |
| Completed-task archive and session bookkeeping | `trellis-finish-work` and the archive implementation |
| Cross-session task progress discovery and recovery | `trellis-continue` owns the recovery decision, `task_progress.py` owns candidate evidence and completed-task reopen, and `task.py start` with `.trellis/scripts/common/active_task.py` owns explicit session binding |

This reference names owners; it does not copy their command schemas, interaction templates, state formats, or error matrices. Read `.trellis/workflow.md`, the local owner skill/helper, available `overrides/bundles/`, and `.flower/state.json` for the installed version. Do not maintain a fixed Skill-Garden skill count or exhaustive capability list here.
<!-- END skill-garden patch trellis-meta-managed-owner-routing v0.6 -->
<!-- BEGIN skill-garden patch trellis-meta-managed-state-boundary v0.6 -->
## Workflow-State Prompt Blocks

`workflow.md` may contain paired blocks such as `[workflow-state:no_task]...[/workflow-state:no_task]`. Hooks select the current block and inject it as a one-hop next-action guard.

State blocks are not the owner of complete planning, routing, checking, commit, or archive procedures. Keep detailed semantics in the owner skill/helper and keep the state body small enough to identify the immediate gate. If a managed state policy changes, update its Skill-Garden Patch baseline/content and the owning workflow contract together; do not edit only the deployed block.

Treat the status names actually present in the local workflow and task runtime as authoritative. Do not copy a fixed state or error matrix into this reference.
<!-- END skill-garden patch trellis-meta-managed-state-boundary v0.6 -->
<!-- BEGIN skill-garden patch trellis-meta-managed-workflow-change-map v0.6 -->
## Local Modification Patterns

Start from the runtime section, then move to its owner:

| Goal | Durable owner route |
| --- | --- |
| Add or reorder a phase | Workflow Patch/source plus every affected owner handoff |
| Change task creation or scope policy | Request Triage, task-intent helper, and the managed workflow/state Patch |
| Change untracked completion or adoption | `workflow-state:untracked*`, Phase 2/3 owners, `untracked_flow.py`, and `task_intent.py adopt` |
| Change planning handoff or activation | `trellis-task-brief` and the task-start Brief guard |
| Change implement/check execution | `trellis-route`; Check-All remains the unified check entry |
| Change automatic continuation | `trellis-auto-loop` and its runner action contract |
| Change spec capture | `trellis-update-spec` |
| Change commit safety or normal completion activation | `trellis-push` and `task_progress.py` |
| Change recovery decisions or candidate discovery | `trellis-continue` and `task_progress.py` |
| Change explicit candidate rebind | `trellis-continue` owns the decision, and `task.py start` with `.trellis/scripts/common/active_task.py` owns the session pointer write |
| Change completed-task reopen | The explicit `task_progress.py reopen` path |
| Change final archive or session bookkeeping | `trellis-finish-work` and the archive implementation |
| Change one platform adapter | The owning platform file/Patch while preserving the shared workflow contract |

In managed mode, update the source Patch and owner, run the synchronization and compiled-target checks, then reread the final `.trellis/workflow.md`. In native mode, a narrow local edit remains valid when no Plugin ownership claim applies.
<!-- END skill-garden patch trellis-meta-managed-workflow-change-map v0.6 -->
## Relationship To Platform Files

`workflow.md` is the semantic center of the local workflow, but each platform can also have its own entry files:

- skills, such as `trellis-brainstorm` and `trellis-check`.
- commands/prompts/workflows, such as continue and finish-work.
- hooks, such as session-start or workflow-state injection.

If only `workflow.md` changes, platform entry files may still contain old language. When the user wants to change "what the AI actually does," also inspect the relevant platform directory.
