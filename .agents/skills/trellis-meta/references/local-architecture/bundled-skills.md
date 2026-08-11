# Bundled Skills

"Bundled skills" are multi-file built-in skills shipped inside the Trellis CLI npm package. Unlike marketplace skills (which a user installs separately into their own `.claude/skills/` or other platform skill root), bundled skills are written automatically into every supported platform's skill root by `trellis init` and kept in sync by `trellis update`. They are part of Trellis itself, not third-party content.

A bundled skill is a directory under `packages/cli/src/templates/common/bundled-skills/<skill>/` that already contains its own `SKILL.md` (with YAML frontmatter) plus optional `references/`, assets, or other supporting files. Trellis copies the whole directory tree as-is into each platform's skill root, so references stay lazy-loadable instead of being flattened into one oversized `SKILL.md`.

<!-- BEGIN skill-garden patch trellis-meta-managed-skill-taxonomy v0.6 -->
## What Counts As Bundled (vs. Adjacent Concepts)

Directory shape alone does not determine ownership:

| Ownership class | Evidence | Durable source |
| --- | --- | --- |
| Upstream bundled | Trellis template hashes and bundled-skill distribution | Trellis bundled template source or an explicitly local supplement |
| Skill-Garden managed | `flower/skill-garden` lock/state entries or `skill-garden patch` markers | `vendor/skill-garden/.trellis/0.6/` in the Flower source checkout |
| Flower managed | Flower Plugin state paths/patches and owner IDs | The owning Flower Plugin source, adapter, or `src/patches/` catalog |
| Shared common | Plugin state entry with shared ownership | The common source retained across dependent capabilities |
| Project-local | No upstream template or Plugin ownership claim, plus project intent | The project file itself |

For managed 0.6 changes, Patch leaves own `insert`/`replace`/`remove` transformations; selectors and full baselines fail closed on upstream drift; content files own replacement bytes; explicit targets and `each-existing` prevent accidental platform creation; Bundles own full/selected aliases; required preflight prevents partial writes; markers and first backups support migration/recovery; provenance records the selected operations; compatibility and conflict policies audit the final result; canonical compiled targets prove the pinned upstream output. These responsibilities must stay in the shared Patch Engine instead of being reimplemented by a skill-specific injector.

Discover what is active from the local skill roots, `.trellis/workflow.md`, available `overrides/bundles/`, and `.flower/state.json`. Do not infer ownership from a skill name or maintain a fixed Skill-Garden capability list in this document.
<!-- END skill-garden patch trellis-meta-managed-skill-taxonomy v0.6 -->
## Current Bundled Skills (v0.6.0)

The set is discovered at runtime by listing directories under `templates/common/bundled-skills/`:

| Skill | Purpose |
| --- | --- |
| `trellis-meta` | This skill. Explains the local Trellis architecture and customization entry points to an AI working inside a user project. |
| `trellis-session-insight` | Wraps the `trellis mem` CLI so an AI knows when and how to reach into past Claude Code / Codex / Pi Agent conversation logs. |
| `trellis-spec-bootstrap` | Platform-neutral workflow for creating or refreshing `.trellis/spec/` from the real codebase (with optional GitNexus / ABCoder integration). |
| `trellis-channel` | Capability skill teaching an AI when to reach for `trellis channel` for multi-agent collaboration, forum/thread persistent boards, and dispatcher-wait patterns. |

The list is discovered at runtime, so adding a new directory under `bundled-skills/` is the only step required to register a new skill (see "Adding a New Bundled Skill" below).

<!-- BEGIN skill-garden patch trellis-meta-managed-platform-skill-roots v0.6 -->
## Where Bundled Skills Land Per Platform

Each platform configurator writes the result of `resolveBundledSkills(ctx)` into a specific skill root during `trellis init`, and its collector must return the same paths for `trellis update` hash tracking.

| Platform | Bundled skill root | Notes |
| --- | --- | --- |
| Claude Code | `.claude/skills/<skill>/` | `configureClaude` |
| Cursor | `.cursor/skills/<skill>/` | `configureCursor` |
| OpenCode | `.opencode/skills/<skill>/` | `collectOpenCodeTemplates` and `configureOpenCode` |
| Codex | `.agents/skills/<skill>/` | Shared neutral root |
| Gemini CLI | `.agents/skills/<skill>/` | Shared neutral root |
| Pi Agent | `.agents/skills/<skill>/` | Shared neutral root; `.pi/` holds prompts, agents, extensions, and settings |
| Kimi Code | `.agents/skills/<skill>/` | Shared neutral root; `.kimi-code/skills/` holds commands and agent prompts, not bundled skills |
| Kiro | `.kiro/skills/<skill>/` | `configureKiro` |
| Qoder | `.qoder/skills/<skill>/` | `configureQoder` |
| CodeBuddy | `.codebuddy/skills/<skill>/` | `configureCodebuddy` |
| GitHub Copilot | `.github/skills/<skill>/` | `configureCopilot` |
| Factory Droid | `.factory/skills/<skill>/` | `configureDroid` |
| Antigravity | `.agent/skills/<skill>/` | `configureAntigravity` |
| Devin | `.devin/skills/<skill>/` | `configureDevin` |
| Kilo | `.kilocode/skills/<skill>/` | `configureKilo` |
| ZCode | `.zcode/skills/<skill>/` | `configureZcode` |
| Trae | `.trae/skills/<skill>/` | `configureTrae` |
| Reasonix | `.reasonix/skills/<skill>/` | Workflow, bundled, and sub-agent skills share one root |
| Oh My Pi | `.omp/skills/<skill>/` | `configureOmp` |
| Grok Build | `.grok/skills/<skill>/` | `configureGrok` |
| Snow CLI | `.snow/skills/<skill>/` | `configureSnow` |

The physical bundled-skill roots are therefore the platform-private roots above plus the single shared `.agents/skills/` root. `.pi/skills/` is not a current target, and `.kimi-code/skills/` must not receive a second bundled copy.

Two paths exercise the same data:

1. `configureX(cwd)` writes files during `trellis init`.
2. `collectPlatformTemplates(platformId)` (in `configurators/index.ts`) returns a `Map<filePath, content>` that `trellis update` uses to detect drift and to populate `.trellis/.template-hashes.json`.

Both paths must resolve to byte-identical bundled-skill output for a given root. Shared `.agents/skills/` writers additionally use the neutral resolver so Codex, Gemini CLI, Pi Agent, and Kimi Code do not overwrite each other with platform-specific text.
<!-- END skill-garden patch trellis-meta-managed-platform-skill-roots v0.6 -->
## Dispatch Wiring (Code Path)

The mechanism that auto-dispatches bundled skills to platform skill roots lives in two files:

1. `packages/cli/src/templates/common/index.ts`
   - `listDirectories("bundled-skills")` enumerates the on-disk skills.
   - `listBundledSkillFiles(skillDir)` walks each skill's directory recursively and returns `{relativePath, content}` for every file.
   - `getBundledSkillTemplates()` returns the cached `CommonBundledSkill[]`.

2. `packages/cli/src/configurators/shared.ts`
   - `resolveBundledSkills(ctx)` flattens that list into `ResolvedSkillFile[]` with `<skill>/<relativePath>` paths and resolved placeholders.
   - `writeSkills(skillsRoot, workflowSkills, bundledSkills)` writes both workflow skills and bundled skill files under `skillsRoot`.
   - `collectSkillTemplates(skillsRoot, workflowSkills, bundledSkills)` returns the same shape as a `Map<filePath, content>` for the update / hash pipeline.

Every platform configurator that supports skills imports both helpers (see `claude.ts`, `cursor.ts`, `codex.ts`, `gemini.ts`, `kiro.ts`, `qoder.ts`, `codebuddy.ts`, `copilot.ts`, `droid.ts`, `antigravity.ts`, `devin.ts`, `kilo.ts`). The `index.ts` `PLATFORM_FUNCTIONS` registry also calls `resolveBundledSkills(ctx)` inside each `collectTemplates` closure so `trellis update` tracking stays consistent.

## Adding a New Bundled Skill

The shape and dispatch wiring are already generic, so adding a skill requires only file changes plus distribution verification.

1. **Create the directory tree.**

   ```
   packages/cli/src/templates/common/bundled-skills/<my-skill>/
     SKILL.md                     # YAML frontmatter + body
     references/                  # optional
       <topic>.md
     assets/                      # optional (anything readable as utf-8)
   ```

2. **Write a valid `SKILL.md` header.** The frontmatter must include at minimum:

   ```yaml
   ---
   name: <my-skill>
   description: "When the AI should reach for this skill. Triggering phrases go here."
   ---
   ```

   The `description` is what each platform's auto-trigger mechanism matches against, so it should describe the user-intent triggers, not the skill's internals.

3. **Use placeholders where appropriate.** Bundled skill content runs through `resolvePlaceholders(file.content, ctx)`. Any `{{platform_name}}`, `{{python_cmd}}`, etc. token supported by `resolvePlaceholders` will be substituted per platform.

4. **No dispatch wiring is required.** `listDirectories("bundled-skills")` discovers the new directory automatically, so all platforms receive it on the next `trellis init` or `trellis update`.

5. **Verify the distribution path** before shipping. Skipping any of these steps has historically caused features to be documented as bundled while the published npm tarball was missing the files:

   - Source files exist on the branch being tagged.
   - `pnpm --filter @mindfoldhq/trellis build` copies the asset into `dist/templates/common/bundled-skills/<skill>/`.
   - `npm pack --dry-run --json` includes the expected `dist/**` paths.
   - In a fresh temp project, `trellis init` writes `.claude/skills/<skill>/SKILL.md`, `.agents/skills/<skill>/SKILL.md`, `.zcode/skills/<skill>/SKILL.md`, etc.
   - `.trellis/.template-hashes.json` lists the generated files.
   - `trellis update --dry-run` in that temp project reports "Already up to date!".

6. **Add a migration manifest entry** if the skill is added in a release that other projects will upgrade into. Without an explicit manifest entry the file will land via the standard "missing file" branch of `trellis update`, but a manifest makes the change visible in the changelog.

<!-- BEGIN skill-garden patch trellis-meta-managed-bundled-overrides v0.6 -->
## Overriding a Bundled Skill Locally

First determine whether the deployed bundled skill is only Trellis-managed or also owned by Flower/Skill-Garden.

- In a native Trellis project, template-hash conflict handling still permits a local divergence, with the normal cross-platform and future-update caveats.
- In a Flower-managed project, do not directly edit a target recorded in `.flower/state.json`. Change the owning Plugin/Patch source and let preflight plus the transaction writer update every existing platform target consistently.
- In the Flower source checkout, Skill-Garden 0.6 modifications belong under `vendor/skill-garden/.trellis/0.6/overrides/`; run `npm run sync`, refresh/check compiled targets, then apply the Plugin to dogfood targets.
- For project-private behavior, prefer `.trellis/spec/` or a differently named project-local skill that has no upstream or Plugin ownership claim.

If Skill-Garden is removed and its state ownership is cleanly released, the remaining bundled skill returns to native Trellis update semantics. Do not leave managed prose behind after uninstall or freeze assumptions into this reference.
<!-- END skill-garden patch trellis-meta-managed-bundled-overrides v0.6 -->
## Removing a Bundled Skill From a Project

There is no per-project opt-out flag for bundled skills. Two options:

1. **Delete the directory in each platform skill root.** `trellis update` will see the file missing, compare against `.template-hashes.json`, and treat the deletion the same as any other user modification — it will not silently re-create the directory unless `--force` is passed.

2. **Pin a Trellis version that did not ship the skill.** The bundled-skill set is determined at build time, so installing an older release of the CLI is the only way to permanently exclude a skill that the current release ships.

A third option — globally disabling all bundled skills — is not supported. The dispatch is unconditional in every configurator. Adding such a flag would require changing `PLATFORM_FUNCTIONS` in `configurators/index.ts` and every `configureX` function.

## Operating Rules

- Treat `templates/common/bundled-skills/` as the single source of truth for what bundled skills exist. Do not hand-maintain platform-by-platform skill lists.
- Do not add platform-specific logic inside a bundled `SKILL.md`. If a behavior is platform-specific, put it in `templates/<platform>/skills/` instead.
- Do not couple bundled skills to a specific CLI binary (e.g. `trellis mem`) without surfacing the dependency in the skill's description and references — users on older releases may not have the command.
- Do not store project-private content in a bundled skill. Bundled skills are public, shipped to every user; project rules belong in `.trellis/spec/` or a local skill.
