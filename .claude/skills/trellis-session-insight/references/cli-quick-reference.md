# `trellis mem` CLI Reference

Full flag reference for the five subcommands. Pin this as the authoritative source — `trellis mem help` prints the same content at runtime, so anything here that drifts is a bug.

## Subcommands

| Command                | Purpose                                                                                                                |
| ---------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| `list`                 | List sessions. Default subcommand when none is given.                                                                  |
| `search <keyword>`     | Find sessions whose contents match a keyword.                                                                          |
| `context <session-id>` | Drill into one session: top-N hit turns + surrounding context. Pair with `--grep` for keyword anchoring.               |
| `extract <session-id>` | Dump cleaned dialogue. Combine with `--phase` / `--grep` to slice.                                                     |
| `projects`             | List active project `cwd` values with session counts. Use this to discover which `--cwd` to pass to other subcommands. |

<!-- BEGIN skill-garden patch session-insight-grok-cli-flags v0.6 -->
## Flags (apply where meaningful)

| Flag                                                       | Subcommands       | Meaning                                                                                                                                                                      |
| ---------------------------------------------------------- | ----------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--platform claude\|codex\|grok\|opencode\|pi\|zcode\|all` | all               | Default `all`. OpenCode currently reports that its reader is unavailable and continues with the supported local stores.                                                     |
| `--since YYYY-MM-DD`                                       | list / search     | Inclusive lower date bound.                                                                                                                                                  |
| `--until YYYY-MM-DD`                                       | list / search     | Inclusive upper date bound.                                                                                                                                                  |
| `--global`                                                 | list / search     | Include sessions from every project on this machine. Default is the current project `cwd`.                                                                                   |
| `--cwd <path>`                                             | list / search     | Force a specific project cwd instead of inferring from where you are.                                                                                                        |
| `--limit N`                                                | list / search     | Cap output rows. Default `50`.                                                                                                                                               |
| `--grep KW`                                                | extract / context | Filter turns by keyword. Multi-token AND when whitespace-separated.                                                                                                          |
| `--phase brainstorm\|implement\|all`                       | extract           | Slice by Trellis task boundaries. Claude, Codex, Grok, Pi, and ZCode support native boundaries; OpenCode warns and returns all turns. Default `all`.                         |
| `--turns N`                                                | context           | Number of hit turns to return. Default `3`.                                                                                                                                  |
| `--around N`                                               | context           | Surrounding turns to include per hit. Default `1`.                                                                                                                           |
| `--max-chars N`                                            | context           | Total character budget. Default `6000` (~1500 tokens).                                                                                                                       |
| `--include-children`                                       | search / context  | Merge OpenCode sub-agent sessions into their parent session.                                                                                                                 |
| `--json`                                                   | all               | Emit machine-parseable JSON instead of human-readable output.                                                                                                                |
<!-- END skill-garden patch session-insight-grok-cli-flags v0.6 -->
## Common one-liners

```bash
# What past sessions discussed "deadlock" anywhere on this machine?
trellis mem search "deadlock" --global --limit 20

# Inside a specific session, surface the top 5 turns that mention "lock contention"
# plus 2 turns of surrounding context.
trellis mem context 5842592d --grep "lock contention" --turns 5 --around 2

# Recover the brainstorm window for a session — useful when continuing a task
# the user started a week ago.
trellis mem extract 5842592d --phase brainstorm

# List every project this machine has Trellis sessions for, with counts.
trellis mem projects
```

## Output shapes

- **Default human output** (no `--json`): wrapped to a terminal, with session ids highlighted and turn markers visible. Suitable to read inline but messy to paste into a markdown file.
- **`--json`**: stable schema, safe to parse and process. When piping `mem` output into a follow-up step (e.g. summarizing for a Lessons section), prefer `--json`.

<!-- BEGIN skill-garden patch session-insight-grok-cli-caveats v0.6 -->
## Caveats

- **OpenCode conversation reading is still unavailable.** When `--platform` resolves to OpenCode, `mem` prints a reader-unavailable notice and continues with Claude, Codex, Grok, Pi, and ZCode. Do not promise OpenCode coverage until its adapter ships.
- **`--phase` slicing depends on recorded `task.py create` / `task.py start` tool calls.** Sessions where the user ran `task.py` from a different terminal may not have phase boundaries. `--phase all` is the safe fallback.
- **Compaction recovery is platform-store dependent.** Claude, Codex, Pi, and ZCode retain recoverable pre-compaction turns in their local stores. Grok may only retain a rendered transcript for compacted history; `mem` emits an explicit warning instead of claiming that missing dialogue was recovered.
- **`mem` reads platform-local stores only.** If the user clears `~/.claude/projects/`, `~/.codex/sessions/`, `~/.grok/sessions/`, Pi's configured session directory, or `~/.zcode/cli/db/db.sqlite`, `mem` cannot recover deleted history.
- **`mem` is read-only.** It does not upload, synchronize, or edit platform session stores. Any write based on a finding is a separate follow-up action.
<!-- END skill-garden patch session-insight-grok-cli-caveats v0.6 -->
## When you need more than this reference

Run `trellis mem help` in the user's shell. The runtime help is authoritative and will be ahead of this reference during fast-moving beta releases.
