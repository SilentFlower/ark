# Change Local Task Lifecycle

Task lifecycle includes creation, start, context configuration, finish, archive, parent/child tasks, and lifecycle hooks. The default customization targets are `.trellis/tasks/`, `.trellis/config.yaml`, and `.trellis/scripts/`.

<!-- BEGIN skill-garden patch trellis-meta-managed-lifecycle-entry-points v0.6 -->
## Read These Files First

1. `.trellis/workflow.md`
2. `.trellis/config.yaml`
3. `.trellis/scripts/task.py`
4. `.trellis/scripts/common/task_store.py`
5. `.trellis/scripts/common/task_utils.py`
6. `.trellis/scripts/common/active_task.py`
7. `.trellis/scripts/task_progress.py` when saved progress, completion, or reopen behavior is involved
8. The owning `trellis-task-brief`, `trellis-push`, `trellis-continue`, or `trellis-finish-work` skill for the boundary being changed
9. The current task's `.trellis/tasks/<task>/task.json` and planning artifacts

## Common Needs And Edit Points

| Need | Edit point |
| --- | --- |
| Change planning handoff or activation approval | `trellis-task-brief`, the task-start Brief guard, and planning workflow ownership. |
| Automatically sync an external system after a lifecycle command | The matching `hooks.after_*` entry in `.trellis/config.yaml`. |
| Change default task fields or archive movement | `.trellis/scripts/common/task_store.py` and `.trellis/scripts/common/task_utils.py`. |
| Change active task behavior | `.trellis/scripts/common/active_task.py` plus the relevant platform session bridge. |
| Change saved progress validation or lifecycle writes | `.trellis/scripts/task_progress.py` and its owning caller. |
| Change normal completion activation | `trellis-push`, `task_progress.py`, and `[workflow-state:completed]`. |
| Change interruption recovery or candidate rebinding | `trellis-continue` owns the user decision, `task_progress.py` owns candidate evidence, and `task.py start` with `.trellis/scripts/common/active_task.py` owns the explicit session bind; never bind a candidate automatically. |
| Change completed-task rework | The explicit reopen path, then refresh planning artifacts and Brief when scope changed. |
| Change final archive and session bookkeeping | `trellis-finish-work` plus the archive implementation; archive must not create completion implicitly. |
<!-- END skill-garden patch trellis-meta-managed-lifecycle-entry-points v0.6 -->

## lifecycle hooks

`.trellis/config.yaml` supports:

```yaml
hooks:
  after_create:
    - "python3 .trellis/scripts/hooks/my_sync.py create"
  after_start:
    - "python3 .trellis/scripts/hooks/my_sync.py start"
  after_finish:
    - "python3 .trellis/scripts/hooks/my_sync.py finish"
  after_archive:
    - "python3 .trellis/scripts/hooks/my_sync.py archive"
```

Hook commands receive the `TASK_JSON_PATH` environment variable, pointing to the current task's `task.json`. Hook failures should usually warn, but not block the main task operation.

## Change Task Fields

If the user wants to add project-local fields, prefer putting them under `meta` in `task.json` to avoid breaking existing scripts' assumptions about standard fields.

Example:

```json
"meta": {
  "linearIssue": "ENG-123",
  "risk": "high"
}
```

If standard fields really need to change, inspect every local script that reads `task.json`.

## Change Active Task

Active task is session-level state stored in `.trellis/.runtime/sessions/`. Do not fall back to a global `.current-task` model. If the user wants to change active task behavior, edit:

- `.trellis/scripts/common/active_task.py`
- platform hooks or shell session bridges
- active task descriptions in `.trellis/workflow.md`

### `task.py create` Sets the Active Pointer

`cmd_create` in `.trellis/scripts/common/task_store.py` calls `set_active_task` best-effort right after writing the new task directory. The behavior:

- When the calling shell carries session identity (`TRELLIS_CONTEXT_ID` env var, or any platform-specific session env that `resolve_context_key` recognizes — see `active_task.py:_ENV_SESSION_KEYS`), the per-session pointer at `.trellis/.runtime/sessions/<context_key>.json` is rewritten to point at the new task. The task's `status=planning` and `[workflow-state:planning]` fires on the very next `UserPromptSubmit`.
- When session identity is unavailable (raw CLI invocation outside an AI session, or a platform that doesn't propagate identity to shell), the task directory is still created and `status=planning` is still written, but the active pointer is left untouched. The user can attach the task later with `task.py start <dir>` once they're back in an AI session.

This makes `[workflow-state:planning]` the live breadcrumb during the brainstorm and JSONL curation work that follows `task.py create`. The pre-R7 behavior left the breadcrumb stuck on `no_task` until `task.py start`, so the planning block was effectively dead text.

If you fork `task.py` to add a new creation path (e.g. an external import that bypasses `cmd_create`), audit whether your path also calls `set_active_task`. Without that call, your created tasks will not surface as active. The full status writer table is in `.trellis/spec/cli/backend/workflow-state-contract.md`.

<!-- BEGIN skill-garden patch trellis-meta-managed-lifecycle-modification-steps v0.6 -->
## Modification Steps

1. Confirm the current task and inspect `task.json`, planning artifacts, saved progress, and the current workflow state.
2. Identify the exact lifecycle writer and owner. Do not treat `task.py`, progress text, a state block, and an owner skill as interchangeable sources of truth.
3. Preserve the stable sequence: Brief review before planning activation; final progress synchronization before local completion; explicit finish-work after completion; explicit reopen before rework.
4. For project-local behavior, edit the narrow local owner. For Flower/Skill-Garden-managed behavior, change the canonical Patch/skill/helper source and then synchronize snapshot, compiled targets, and dogfood.
5. Update every affected caller, guard, recovery path, conflict assertion, and final-output test. A status transition is incomplete if another entry can bypass or contradict it.
6. Re-run the relevant task lifecycle, Patch conflict, compiled-target, and idempotency checks before relying on the new behavior.
<!-- END skill-garden patch trellis-meta-managed-lifecycle-modification-steps v0.6 -->
## Do Not

- Do not directly edit `.trellis/.runtime/sessions/` to "fix" business state.
- Do not hard-code project-private fields into scripts; prefer `meta`.
- Do not default to asking the user to fork Trellis CLI.
