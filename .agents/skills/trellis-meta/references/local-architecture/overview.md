# Local Trellis Architecture Overview

`trellis-meta` is for user projects that have already run `trellis init`. The user's machine usually has only the npm-installed `trellis` command plus the Trellis files generated inside the project; it may not have the Trellis CLI source code.

Therefore, when an AI uses this skill, the default customization target is local files inside the user project:

- `.trellis/`: workflow, tasks, specs, memory, scripts, and runtime state.
- Platform directories: `.claude/`, `.codex/`, `.cursor/`, `.opencode/`, `.kiro/`, `.gemini/`, `.qoder/`, `.codebuddy/`, `.github/`, `.factory/`, `.pi/`, `.kilocode/`, `.agent/`, `.devin/`, `.reasonix/`, `.zcode/`, and similar directories.
- Shared skill layer: `.agents/skills/`.

Do not default to guiding the user to fork the Trellis CLI repository. Treat upstream source code as the operating target only when the user explicitly says they want to change Trellis upstream source, publish an npm package, or contribute a PR.

<!-- BEGIN skill-garden patch trellis-meta-managed-system-model v0.6 -->
## Local System Model

Native Trellis provides three project-local layers:

1. **Workflow layer**: `.trellis/workflow.md` defines current phases, routing, next actions, and prompt blocks.
2. **Persistence layer**: `.trellis/tasks/`, `.trellis/spec/`, and `.trellis/workspace/` store tasks, specs, and deliberate session records.
3. **Platform integration layer**: hooks, settings, agents, skills, commands, prompts, workflows, channel runtime files, and memory entry points connect Trellis to AI tools.

Flower adds a conditional management layer when a Plugin is declared and locked:

4. **Plugin management layer**: `.flower/plugins.json` records intent, `.flower/plugin-lock.json` records the resolved immutable graph and granted capabilities, and `.flower/state.json` records owned paths, Patch provenance, and resulting hashes. Planning, preflight, transaction, lock, state, rollback, and uninstall are one lifecycle rather than unrelated local edits.

Without matching Plugin state or managed markers, use the native three-layer model. With `flower/skill-garden` ownership, read deployed files to understand current behavior but make durable 0.6 changes through the owning Skill-Garden source and Patch catalog.
<!-- END skill-garden patch trellis-meta-managed-system-model v0.6 -->
## Core Paths

| Path | Purpose |
| --- | --- |
| `.trellis/workflow.md` | Workflow phases, skill routing, and workflow-state prompt blocks. |
| `.trellis/config.yaml` | Project configuration, task lifecycle hooks, monorepo package configuration, and journal configuration. |
| `.trellis/spec/` | The user's project-specific coding conventions and thinking guides. |
| `.trellis/tasks/` | Each task's PRD, technical notes, research files, and JSONL context. |
| `.trellis/workspace/` | Per-developer journals and cross-session memory. |
| `.trellis/scripts/` | Local Python runtime used by commands, hooks, and context injection. |
| `.trellis/.runtime/` | Session-level runtime state, such as the current task pointer. |
| `.trellis/.template-hashes.json` | Template hashes for Trellis-managed files, used by update to determine whether local files were modified by the user. |

<!-- BEGIN skill-garden patch trellis-meta-managed-customization-principles v0.6 -->
## AI Customization Principles

1. **Inspect behavior and ownership separately**: Read the current `.trellis/` and platform files for runtime truth, then inspect `.flower/` state, template hashes, and managed markers to locate the durable authoring source.
2. **Choose the owner before the edit**: Project-local content may be edited locally. Trellis templates follow native update rules. Flower/Skill-Garden targets follow Plugin state and Patch ownership.
3. **Use the 0.6 Patch Engine for managed Trellis files**: Declare exact targets, selector/baseline/content, target policy, and Bundle selection. Do not add a special injector or modify only a deployed copy.
4. **Keep the lifecycle ordered**: Change `vendor/skill-garden/.trellis/0.6/`, run `npm run sync` to refresh `enhancements/0.6/`, regenerate/check compiled targets, then update dogfood through the Flower Plugin lifecycle.
5. **Keep shared semantics aligned**: Workflow owner changes may require matching skill, hook, helper, or platform entry updates, but each full procedure stays with its owning capability.
6. **Keep project-specific rules project-local**: Use `.trellis/spec/` or a separately owned local skill; do not turn public `trellis-meta` into a project notebook.
7. **Preserve evidence and user content**: Respect Plugin ownership, first-backup/provenance records, template conflicts, and current user modifications. Never use `node_modules` as an authoring target.
<!-- END skill-garden patch trellis-meta-managed-customization-principles v0.6 -->
## How To Use This Directory

- To understand which files exist after init, read `generated-files.md`.
- To change phases, routing, or next actions, read `workflow.md`.
- To change the task model, JSONL context, or active task behavior, read `task-system.md`.
- To change coding convention injection, read `spec-system.md`.
- To understand journals and cross-session memory, read `workspace-memory.md`.
- To change hooks or sub-agent context loading, read `context-injection.md`.
