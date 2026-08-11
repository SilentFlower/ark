# Local Files Generated After Init

`trellis init` writes the Trellis runtime into the user project. Later, `trellis update` tries to update Trellis-managed template files, but it uses `.trellis/.template-hashes.json` to determine which files have already been modified by the user.

This page only describes files that are visible and editable inside the user project.

## `.trellis/`

```text
.trellis/
├── workflow.md
├── config.yaml
├── .developer
├── .version
├── .template-hashes.json
├── .runtime/
├── scripts/
├── spec/
├── tasks/
└── workspace/
```

| Path | Usually editable? | Notes |
| --- | --- | --- |
| `.trellis/workflow.md` | Yes | Local workflow documentation and AI routing rules. |
| `.trellis/config.yaml` | Yes | Project configuration, hooks, packages, journal line limits, and related settings. |
| `.trellis/spec/` | Yes | Project specs, intended to be updated regularly by users and AI. |
| `.trellis/tasks/` | Yes | Task material and research artifacts, maintained by the task workflow. |
| `.trellis/workspace/` | Yes | Session records, usually written by `add_session.py`. |
| `.trellis/scripts/` | Carefully | Local runtime. It can be customized, but only after understanding the call chain. |
| `.trellis/.runtime/` | No | Runtime state, usually written automatically by hooks/scripts. |
| `.trellis/.developer` | Carefully | Current developer identity. |
| `.trellis/.version` | No | Trellis version record used by update/migration logic. |
| `.trellis/.template-hashes.json` | No | Template hash record. Do not hand-write business rules here. |

## Platform Directories

Different platforms generate different directories. Common categories:

| Category | Example paths | Purpose |
| --- | --- | --- |
| hooks | `.claude/hooks/`, `.codex/hooks/`, `.cursor/hooks/` | Inject session context, workflow-state, and sub-agent context. |
| settings | `.claude/settings.json`, `.codex/hooks.json`, `.qoder/settings.json`, `.trae/hooks.json` | Tell the platform when to run hooks or plugins. |
| agents | `.claude/agents/`, `.codex/agents/`, `.kiro/agents/`, `.zcode/agents/` | Define agents such as `trellis-research`, `trellis-implement`, and `trellis-check`. |
| skills | `.claude/skills/`, `.agents/skills/`, `.qoder/skills/`, `.zcode/skills/` | Skills that auto-trigger or can be read by AI. |
| commands/prompts/workflows | `.cursor/commands/`, `.github/prompts/`, `.devin/workflows/`, `.zcode/commands/` | Explicit user-invoked command or workflow entry points. |

When modifying a platform directory, also confirm whether `.trellis/workflow.md` still describes the same flow.

<!-- BEGIN skill-garden patch trellis-meta-managed-template-hashes v0.6 -->
## Meaning Of Template Hashes

`.trellis/.template-hashes.json` records native Trellis template hashes and still governs ordinary `trellis update` conflicts:

| Case | Native update behavior |
| --- | --- |
| File matches the recorded template hash | Trellis may update it automatically. |
| File differs from the recorded template hash | Trellis may prompt to overwrite, keep, or generate `.new`. |
| File is no longer a current template | Trellis migration rules decide whether to delete, rename, or preserve it. |

This file is not the complete ownership model in a Flower-managed project. When `.flower/plugin-lock.json` and `.flower/state.json` claim a target, Plugin ownership, Patch provenance, transaction checks, and managed result hashes also apply. Do not hand-edit either hash store; inspect both before deciding whether a difference is a user customization, a managed overlay, or drift.
<!-- END skill-garden patch trellis-meta-managed-template-hashes v0.6 -->
<!-- BEGIN skill-garden patch trellis-meta-managed-file-boundaries v0.6 -->
## Local Customization Boundaries

Classify before editing:

| Ownership | Durable edit point |
| --- | --- |
| Project-local | The current project file, after reading its call chain and related workflow semantics. |
| Upstream Trellis template | A local customization or upstream Trellis source, depending on the user's stated goal and template-hash behavior. |
| Skill-Garden managed | `vendor/skill-garden/.trellis/0.6/` in the Flower source checkout, expressed through Patch/Bundle declarations for existing Trellis targets. |
| Flower managed | The owning Flower source, Plugin manifest/adapter, or Patch catalog; never only the deployed result. |
| Shared common | The shared source plus Plugin state ownership rules; preserve content shared by more than one capability. |

Never hand-edit concrete runtime state, `.flower/` lock/state files, template hash contents, global npm caches, or `node_modules` as an implementation shortcut. If the Flower source checkout is not present, use the installed Plugin lifecycle or create separately owned project-local content rather than pretending the managed source exists locally.
<!-- END skill-garden patch trellis-meta-managed-file-boundaries v0.6 -->
