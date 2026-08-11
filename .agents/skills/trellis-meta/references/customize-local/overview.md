# Local Customization Overview

<!-- BEGIN skill-garden patch trellis-meta-managed-customization-entry v0.6 -->
This directory covers both native local customization and managed overlays. Read generated `.trellis/` and platform files to understand current behavior, but determine ownership before editing: project-local targets may be changed in place, while Flower/Skill-Garden targets must be changed through their owning Plugin or Patch source. Never use the npm cache or `node_modules` as an authoring source.
<!-- END skill-garden patch trellis-meta-managed-customization-entry v0.6 -->

## First Determine What The User Actually Wants To Change

| User wording | Read first |
| --- | --- |
| "Change the Trellis flow / phases / next prompt" | `change-workflow.md` |
| "Change task creation, status, archive, or hooks" | `change-task-lifecycle.md` |
| "AI did not read context / change injected content" | `change-context-loading.md` |
| "A platform hook is not behaving as expected" | `change-hooks.md` |
| "Change implement/check/research agent behavior" | `change-agents.md` |
| "Add a skill/command/workflow/prompt" | `change-skills-or-commands.md` |
| "Adjust the project spec structure" | `change-spec-structure.md` |
| "Add team conventions and local notes" | `add-project-local-conventions.md` |

<!-- BEGIN skill-garden patch trellis-meta-managed-customization-order v0.6 -->
## General Operation Order

1. **Confirm scope**: inspect enabled platform roots and the current active task.
2. **Read runtime truth**: read `.trellis/workflow.md`, `.trellis/config.yaml`, and the relevant platform files.
3. **Resolve ownership**: inspect `.flower/plugins.json`, `.flower/plugin-lock.json`, `.flower/state.json`, `.trellis/.template-hashes.json`, and managed markers.
4. **Choose one route**:
   - project-local or native local customization: edit the narrowly scoped local source;
   - Flower/Skill-Garden managed target: edit the owning Plugin/Patch source, not the deployed result.
5. **For Skill-Garden 0.6 authoring**: change `vendor/skill-garden/.trellis/0.6/`, run `npm run sync`, regenerate/check compiled targets, then apply the Flower lifecycle to dogfood targets.
6. **Verify final semantics**: check every existing platform target, conflict assertions, provenance, and idempotency. The final files must agree with `.trellis/workflow.md` and their workflow owner.
<!-- END skill-garden patch trellis-meta-managed-customization-order v0.6 -->
## Local File Priority

| Layer | Files |
| --- | --- |
| Workflow | `.trellis/workflow.md` |
| Project configuration | `.trellis/config.yaml` |
| Task material | `.trellis/tasks/<task>/` |
| Project specs | `.trellis/spec/` |
| Runtime scripts | `.trellis/scripts/` |
| Platform integration | `.claude/`, `.codex/`, `.cursor/`, `.opencode/`, `.zcode/`, and similar directories |
| Shared skill | `.agents/skills/` |

## Things Not To Do By Default

- Do not edit the global npm install directory.
- Do not edit `node_modules/@mindfoldhq/trellis`.
- Do not assume the user has the Trellis GitHub repository.
- Do not overwrite local files already modified by the user with default templates.
- Do not put team project rules into public `trellis-meta`; project rules belong in `.trellis/spec/` or a local skill.

## When To Inspect Upstream Source

Switch to an upstream source-code perspective only when the user explicitly expresses one of these goals:

- "I want to open a PR to Trellis"
- "I want to change npm package publish contents"
- "I want to fork Trellis"
- "I want to modify the generation logic for `trellis init/update`"

Otherwise, default to modifying local Trellis files inside the user project.
