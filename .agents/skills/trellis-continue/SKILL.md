---
name: trellis-continue
description: "Resume work on the current task. Loads the workflow Phase Index, figures out which phase/step to pick up at, then pulls the step-level detail via get_context.py --mode phase. Use when coming back to an in-progress task and you need to know what to do next."
---

# Continue Current Task

Resume work on the current task — pick up at the right phase/step in `.trellis/workflow.md`.

---

## Step 1: Load Current Context

```bash
python3 ./.trellis/scripts/get_context.py
```

Confirms: current task, git state, recent commits.
<!-- BEGIN skill-garden patch trellis-continue-task-progress-recovery v0.6 -->
## Step 1.5: Recover Saved Task Progress

Before loading the Phase Index or deciding a workflow step, run:

```bash
python3 ./.trellis/scripts/task_progress.py status --json
```

Treat the structured result as advisory recovery evidence only:

- For `status=ok` with `taskStatus=in_progress`, relay only `summary.partialStep`, `summary.nextStep`, and notes that are necessary to resume safely.
- For `status=ok` with `taskStatus=completed`, do not resume Phase 2 or Phase 3.3. Enter the `trellis-push` completed-task preflight; it is the one-hop owner that either prepares publication recovery, points to explicit `trellis-finish-work`, or blocks on ambiguous evidence.
- For `status=candidates`, relay each healthy candidate with its `taskStatus` plus necessary `invalidCandidates` or `scanWarnings`, and suggest an explicit rebind when appropriate. After explicit rebind, a completed candidate uses the same Push preflight. Never rebind the session or task automatically.
- For `status=no-progress` or `status=no-current-task`, continue without inventing saved progress. For `status=error`, report the structured blocker instead of guessing.

Progress never overrides the task `status`, planning artifacts, workflow ordering, auto-loop runtime, or Git publication evidence. Do not inspect or classify completed Git recovery here. Do not infer a Phase from progress, restore a previous push mode, or resume Git/commit orchestration from progress text.

To rework a completed task, first obtain an explicit user decision, then run:

```bash
python3 ./.trellis/scripts/task_progress.py reopen --task <task-name> --json
```

Only `completed -> in_progress` is valid. Reopen clears `completedAt` but preserves the auditable progress record. If the rework changes requirements or planning boundaries, refresh the planning artifacts and Brief and obtain approval again before implementation.

### Planning Resume Gate

When the current task is still `status=planning`, enter `trellis-brainstorm` before using artifact presence to choose Phase 1.3 or 1.4. Existing `prd.md`, `design.md`, `implement.md`, JSONL files, or `brief.md` prove only that files exist; they do not prove that acceptance criteria are testable, key decisions have converged, repository-answerable questions were researched, or remaining questions genuinely require the user.

Only after the `trellis-brainstorm` Quality Bar is satisfied may the flow load `trellis-task-brief`, refresh and display the current full brief, and wait for a current explicit user confirmation before `task.py start`. Earlier implementation intent, auto-loop startup, or confirmation for older artifact contents cannot authorize the resumed start.
<!-- END skill-garden patch trellis-continue-task-progress-recovery v0.6 -->

## Step 2: Load the Phase Index

```bash
python3 ./.trellis/scripts/get_context.py --mode phase
```

Shows the Phase Index (Plan / Execute / Finish) with routing + skill mapping.

## Step 3: Decide Where You Are

`get_context.py` shows the active task's `status` field. Route by `status` + artifact presence. This command replaces the user needing to remember the Trellis flow; it does not itself approve implementation.

- `status=planning` + no `prd.md` → **1.1** (load `trellis-brainstorm`)
- `status=planning` + `prd.md` only → decide whether the task is lightweight or complex. Lightweight can move to **1.4** review; complex returns to **1.1** to add `design.md` + `implement.md`.
- `status=planning` + complex artifacts complete + sub-agent jsonl not curated (only the seed `_example` row) → **1.3**
- `status=planning` + required artifacts complete + required jsonl curated or inline mode → **1.4** (ask for start review; only run `task.py start` after user confirms)
- `status=in_progress` + implementation not started → **2.1**
- `status=in_progress` + implementation done, not yet checked → **2.2**
- `status=in_progress` + check passed → **3.3** (spec update) → **3.4** (commit)
<!-- BEGIN skill-garden patch trellis-continue-completed-route v0.6 -->
- `status=completed` -> enter the `trellis-push` completed-task preflight. It either prepares publication recovery, points to explicit `trellis-finish-work`, or blocks on ambiguous evidence. Do not resume Phase 2 or Phase 3.3.
<!-- END skill-garden patch trellis-continue-completed-route v0.6 -->

Phase rules (full detail in `.trellis/workflow.md`):

1. Run steps **in order** within a phase — `[required]` steps must not be skipped
2. `[once]` steps are already done if the required output exists. `prd.md` alone can be enough only for lightweight tasks; complex tasks also need `design.md` and `implement.md`.
3. You may go back to an earlier phase if discoveries require it

## Step 4: Load the Specific Step

Once you know which step to resume at:

```bash
python3 ./.trellis/scripts/get_context.py --mode phase --step <X.X> --platform codex
```

Follow the loaded instructions. After each `[required]` step completes, move to the next.

---

## Reference

Full workflow and detailed phase steps live in `.trellis/workflow.md`. This command is only an entry point — the canonical guidance is there.
