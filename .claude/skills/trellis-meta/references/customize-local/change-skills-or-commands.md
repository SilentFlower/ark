# Change Local Skills, Commands, Prompts, And Workflows

<!-- BEGIN skill-garden patch trellis-meta-managed-skill-classification v0.6 -->
When the user wants to change AI entry points, auto-trigger rules, or explicit command behavior, inspect the deployed skill/command/prompt/workflow and then classify its owner.

- **Upstream bundled**: distributed by Trellis and tracked by template hashes.
- **Skill-Garden managed**: modified or projected by `flower/skill-garden`, proven by lock/state entries or managed Patch markers.
- **Flower managed**: owned by another Flower Plugin, adapter, or Flower Patch catalog.
- **Shared common**: projected with shared ownership and retained across dependent capabilities.
- **Project-local**: no upstream template or Plugin ownership claim, and intentionally maintained by the project.

Do not classify every non-bundled name as project-local. Use `.flower/state.json`, `.trellis/.template-hashes.json`, local skill roots, and available Bundle declarations as evidence before selecting an edit route.
<!-- END skill-garden patch trellis-meta-managed-skill-classification v0.6 -->

## Read These Files First

1. `.trellis/workflow.md`
2. Target platform skill/command/prompt/workflow directory
3. Related agent or hook files
4. Whether project rules already exist in `.trellis/spec/`
5. `.trellis/.template-hashes.json` — confirms whether the skill you are about to edit is upstream-owned (entry present) or project-local (entry absent)

## Which Entry Type To Choose

| Goal | Recommendation |
| --- | --- |
| AI should automatically know a capability | Add or modify a skill. |
| User wants to trigger manually with a command | Add or modify a command/prompt/workflow. |
| Team project conventions | Prefer `.trellis/spec/` or a project-local skill — never a bundled skill directory. |
| Tweak a bundled skill (`trellis-meta` et al.) for the user's own project | Create a project-local sibling skill (different name) that overrides intent, or edit `.trellis/spec/`. Edits inside the bundled skill directory survive only until the next `trellis update` and will need a "keep" choice each time. |
| Contribute the change back upstream | Edit `packages/cli/src/templates/common/bundled-skills/<name>/` in the Trellis CLI repo, not the deployed copy. |
| Change Trellis flow semantics | Synchronize `.trellis/workflow.md`. |

## Modify A Skill

A skill is usually:

```text
<skill-name>/
├── SKILL.md
└── references/
```

`SKILL.md` should be short and responsible for triggering/routing. Put long content in `references/` so AI can read it on demand.

The frontmatter description should specify when to use the skill. Example:

```yaml
description: "Use when customizing this project's deployment workflow and release checklist."
```

Do not write vague descriptions such as "helpful project skill"; they can trigger incorrectly.

<!-- BEGIN skill-garden patch trellis-meta-managed-skill-edit-route v0.6 -->
### Bundled vs. Project-Local

Use the ownership evidence, not the directory shape:

| Owner | Update evidence | Correct edit route |
| --- | --- | --- |
| Upstream Trellis | `.trellis/.template-hashes.json` only | Local supplement/divergence or upstream source, according to user intent |
| Skill-Garden | `flower/skill-garden` lock/state entry or managed marker | 0.6 Patch/Bundle source under `vendor/skill-garden` when authoring Flower |
| Flower Plugin | `.flower/state.json` owner/path/patch entry | Owning Plugin source or Flower Patch catalog |
| Shared common | State path with shared ownership | Shared common source; preserve other consumers |
| Project-local | No managed ownership claim | Edit the project file directly |

For a managed target, the durable sequence is source change -> required preflight -> transaction -> state/provenance update. For a project-private behavior, a differently named local skill or `.trellis/spec/` remains preferable to mutating a public bundled skill.
<!-- END skill-garden patch trellis-meta-managed-skill-edit-route v0.6 -->
## Modify A Command/Prompt/Workflow

Explicit entry points should state:

- How the user triggers it.
- Which `.trellis/` files to read.
- Which scripts to run.
- How to report after completion.

If a command only repeats workflow rules, prefer making it reference/read `.trellis/workflow.md` instead of maintaining a second copy of the flow.

<!-- BEGIN skill-garden patch trellis-meta-managed-common-paths v0.6 -->
## Common Paths

| Platform | Entry directories |
| --- | --- |
| Claude Code | `.claude/skills/`, `.claude/commands/` |
| Cursor | `.cursor/skills/`, `.cursor/commands/` |
| OpenCode | `.opencode/skills/`, `.opencode/commands/` |
| Codex | `.agents/skills/`, `.codex/skills/` |
| Gemini CLI | `.agents/skills/`, `.gemini/commands/` |
| Kiro | `.kiro/skills/` |
| Qoder | `.qoder/skills/`, `.qoder/commands/` |
| CodeBuddy | `.codebuddy/skills/`, `.codebuddy/commands/` |
| GitHub Copilot | `.github/skills/`, `.github/prompts/` |
| Factory Droid | `.factory/skills/`, `.factory/commands/` |
| Pi Agent | `.agents/skills/`, `.pi/prompts/` |
| Oh My Pi | `.omp/skills/`, `.omp/commands/` |
| Grok Build | `.grok/skills/`, `.grok/commands/` |
| Kimi Code | `.agents/skills/`, `.kimi-code/skills/` |
| Snow CLI | `.snow/skills/`, `.snow/commands/` |
| Reasonix | `.reasonix/skills/` (no separate commands dir; slash commands built into the platform) |
| ZCode | `.zcode/skills/`, `.zcode/commands/` |
| Kilo / Antigravity / Devin / Trae | workflows or commands + skills |

Only each platform's skill root receives the bundled skills. `.agents/skills/` is the shared root used by Codex, Gemini CLI, Pi Agent, and Kimi Code; `.pi/prompts/` and `.kimi-code/skills/` contain their platform-private entry points rather than another bundled-skill copy.
<!-- END skill-garden patch trellis-meta-managed-common-paths v0.6 -->
## Add A Project-Local Skill

If the user wants to document team-private customizations, create a project-local skill — never put project-private content into a bundled skill directory, since `trellis update` will overwrite it.

```text
.claude/skills/project-trellis-local/
└── SKILL.md
```

<!-- BEGIN skill-garden patch trellis-meta-managed-shared-skill-consumers v0.6 -->
For multi-platform projects, add equivalent versions in each platform skill directory, or use `.agents/skills/` on platforms that support the shared layer (Codex, Gemini CLI, Pi Agent, Kimi Code).
<!-- END skill-garden patch trellis-meta-managed-shared-skill-consumers v0.6 -->

Pick a name that does **not** collide with the bundled set:

- `trellis-meta`
- `trellis-spec-bootstrap`
- `trellis-session-insight`
- `trellis-channel`

A reused name causes `getBundledSkillTemplates()` to overwrite the project-local copy on the next update. A common convention is to prefix the project name: `acme-trellis-deploy`, `acme-trellis-onboarding`.

## Notes

- Do not mix every platform's syntax into one file.
- Do not change only one platform entry point while claiming all platforms are supported.
- Do not hide long-term engineering conventions inside a command; write them to `.trellis/spec/`.
- Do not hand-edit files inside `trellis-meta/`, `trellis-spec-bootstrap/`, `trellis-session-insight/`, or `trellis-channel/` under any `.{platform}/skills/` directory expecting the change to persist — they are bundled and refreshed by `trellis update`. Either contribute upstream or add a project-local skill that complements them.
- After `trellis update` reports a "modified by you" conflict on a bundled skill file, choose **keep** only if you accept maintaining the divergence by hand; otherwise accept the overwrite and re-apply the intent as a project-local skill.
