---
name: trellis-finish-work
description: "Wrap up the current session: verify quality gate passed, remind user to commit, archive completed tasks, and record session progress to the developer journal. Use when done coding and ready to end the session."
---

<!-- BEGIN skill-garden patch trellis-finish-work-exact-bookkeeping v0.6 -->
# Finish Work

Finish the current session after business code has already been handled through Phase 3.4 `trellis-push`. This skill owns only the current task's release audit, archive bookkeeping, session journal, and eligible bookkeeping push. It must not recommit business code or infer Git actions from task progress.

## Exact Bookkeeping Contract

Before moving files, record:

- The active task source path, task name, and `task.json.children`.
- Current branch, upstream, `HEAD`, upstream HEAD, and `@{u}..HEAD`.
- `baseline_synced=true` only when an upstream exists and the initial `HEAD` equals upstream HEAD.
- Existing staged paths outside this task so they can be verified after scoped commits.

Unrelated planning tasks, old archives, and files from other windows remain untouched. They do not require classification and do not block finish-work. Stop and return to Phase 3.4 only when the current task still has uncommitted business files, or when Git is unsafe because of detached HEAD, conflicts, rebase, or another blocking state.

Follow the steps below in order.

### 1. Completion State Gate

Run:

```bash
python3 ./.trellis/scripts/task_progress.py status --task <task-name> --json
```

Read the current task's `task.json` as the authoritative lifecycle record. Continue only when `taskStatus=completed` and `completedAt` is present. Progress text is recovery evidence and never substitutes for the task status.

- `in_progress`: stop and return to Phase 3.4 `trellis-push`; finish-work must not manufacture completion.
- `completed`: keep the active task pointer until archive succeeds, then continue below.
- Missing, corrupt, or any other status: fail closed and report the exact blocker.

### 2. Decision Audit

Run:

```bash
python3 ./.trellis/scripts/decision_log.py status --task <task-name> --json
```

- No decisions, or the current digest is already `accepted`: continue without another confirmation.
- Unreviewed decisions: show ID, topic, choice, rationale summary, risk, and verification, then wait for one explicit review.
- Accept all: run `decision_log.py review --task <task-name> --verdict accepted` and continue.
- Request changes: run `decision_log.py review --task <task-name> --verdict changes-requested --decision-id <id> [...] --notes <text>`, stop before release audit, and return the task for rework.
- A corrupt decision log fails closed. Do not edit or discard it to bypass review.

`task.py archive` repeats the completion-state and decision guards before any session cleanup or directory move. It preserves the existing `completedAt` and performs no lifecycle status write.

### 3. Current Task Release Audit

Run `trellis-release audit-current`. It reads task artifacts and Git evidence and returns `no-op`, `written`, or `needs-review`.

- Do not add another confirmation.
- Do not duplicate SQL, configuration, batch, external-system, or documentation-drift analysis inside finish-work.
- A written `<task>/release.md` moves with the task archive.
- `needs-review` or `Needs human review` does not block archival, but remains visible in the final result.

### 4. Archive to Disk

Always run:

```bash
python3 ./.trellis/scripts/task.py archive <task-name> --no-commit
```

Use the command result and scoped diff to identify the original task source, actual archive destination, and child `task.json` files changed by parent archival. Never stage the `.trellis/tasks` or `.trellis/tasks/archive` root directory.

### 5. Record the Journal

Always run:

```bash
python3 ./.trellis/scripts/add_session.py --no-commit \
  --title "<session title>" \
  --commit "<business commit hashes>" \
  --summary "<brief summary>"
```

Use the command result and scoped diff to determine the exact journal/index paths changed by this invocation.

### 6. Exact Commits

When `session_auto_commit: false`, keep the archive and journal changes on disk without committing or pushing. Report the exact dirty paths.

When `session_auto_commit: true`, create separate scoped commits:

```bash
git add -- <actual archive destination> <changed child task.json files>
git rm -r --cached --ignore-unmatch -- <original task source>
git commit --only -m "chore(task): archive <task-name>" -- \
  <original task source> <actual archive destination> <changed child task.json files>

git add -- <exact journal/index paths>
git commit --only -m "<configured session commit message>" -- <exact journal/index paths>
```

Skip the second commit when the journal did not change. After each commit, use `git show --name-status -M --format=` to verify exact paths and renames, and confirm unrelated staged paths remain staged. The whole worktree does not need to be clean.

### 7. Eligible Automatic Push

Use only the Git baseline captured at the start:

- If `baseline_synced=true`, verify branch/upstream are unchanged and all newly ahead commits are exactly this run's archive/journal commits, then push the current branch to its upstream.
- If the branch was already ahead, behind, diverged, or lacked upstream at the start, keep the bookkeeping commits local.
- If a concurrent commit, branch/upstream change, or push rejection occurs, stop automatic push, preserve local results, and report the condition.

Never force push. Push eligibility is independent of task progress fields and overall worktree cleanliness.

### 8. Final Result

Report separately:

- Decision audit status and reviewed decision IDs when present.
- Release audit status and `release.md` path when present.
- Archive destination and archive commit when present.
- Journal paths and journal commit when present.
- Push status: `pushed`, `local-only`, `skipped`, or `failed`, including the baseline reason.
- Unrelated dirty or staged paths left untouched.
<!-- END skill-garden patch trellis-finish-work-exact-bookkeeping v0.6 -->
