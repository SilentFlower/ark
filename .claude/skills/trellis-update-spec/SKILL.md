---
name: trellis-update-spec
description: "Captures executable contracts and coding conventions into .trellis/spec/ documents. Use when learning something valuable from debugging, implementing, or discussion that should be preserved for future sessions."
---

# Update Code-Spec - Capture Executable Contracts

When you learn something valuable (from debugging, implementing, or discussion), use this to update the relevant code-spec documents.

**Timing**: After completing a task, fixing a bug, or discovering a new pattern

---

## Code-Spec First Rule (CRITICAL)

In this project, "spec" for implementation work means **code-spec**:
- Executable contracts (not principle-only text)
- Concrete signatures, payload fields, env keys, and boundary behavior
- Testable validation/error behavior

If the change touches infra or cross-layer contracts, code-spec depth is mandatory.

### Mandatory Triggers

Apply code-spec depth when the change includes any of:
- New/changed command or API signature
- Cross-layer request/response contract change
- Database schema/migration change
- Infra integration (storage, queue, cache, secrets, env wiring)

### Mandatory Output (7 Sections)

For triggered tasks, include all sections below:
1. Scope / Trigger
2. Signatures (command/API/DB)
3. Contracts (request/response/env)
4. Validation & Error Matrix
5. Good/Base/Bad Cases
6. Tests Required (with assertion points)
7. Wrong vs Correct (at least one pair)

---

## When to Update Code-Specs

| Trigger | Example | Target Spec |
|---------|---------|-------------|
| **Implemented a feature** | Added a new integration or module | Relevant spec file |
| **Made a design decision** | Chose extensibility pattern over simplicity | Relevant spec + "Design Decisions" section |
| **Fixed a bug** | Found a subtle issue with error handling | Relevant spec (e.g., error-handling docs) |
| **Discovered a pattern** | Found a better way to structure code | Relevant spec file |
| **Hit a gotcha** | Learned that X must be done before Y | Relevant spec + "Common Mistakes" section |
| **Established a convention** | Team agreed on naming pattern | Quality guidelines |
| **New thinking trigger** | "Don't forget to check X before doing Y" | `guides/*.md` (as a checklist item) |

**Key Insight**: Code-spec updates are NOT just for problems. Every feature implementation contains design decisions and contracts that future AI/developers need to execute safely.

---

## Spec Structure Overview

```
.trellis/spec/
├── <layer>/           # Per-layer coding standards (e.g., backend/, frontend/, api/)
│   ├── index.md       # Overview and links
│   └── *.md           # Topic-specific guidelines
└── guides/            # Thinking checklists (NOT coding specs!)
    ├── index.md       # Guide index
    └── *.md           # Topic-specific guides
```

### CRITICAL: Code-Spec vs Guide - Know the Difference

| Type | Location | Purpose | Content Style |
|------|----------|---------|---------------|
| **Code-Spec** | `<layer>/*.md` | Tell AI "how to implement safely" | Signatures, contracts, matrices, cases, test points |
| **Guide** | `guides/*.md` | Help AI "what to think about" | Checklists, questions, pointers to specs |

**Decision Rule**: Ask yourself:

- "This is **how to write** the code" → Put in a spec layer directory
- "This is **what to consider** before writing" → Put in `guides/`

**Example**:

| Learning | Wrong Location | Correct Location |
|----------|----------------|------------------|
| "Use API X not API Y for this task" | ❌ `guides/` (too specific for a thinking guide) | ✅ Relevant spec file (concrete convention) |
| "Remember to check X when doing Y" | ❌ Spec file (too abstract for a spec) | ✅ `guides/` (thinking checklist) |

**Guides should be short checklists that point to specs**, not duplicate the detailed rules.

---

## Update Process

### Step 1: Identify What You Learned

Answer these questions:

1. **What did you learn?** (Be specific)
2. **Why is it important?** (What problem does it prevent?)
3. **Where does it belong?** (Which spec file?)

### Step 2: Classify the Update Type

| Type | Description | Action |
|------|-------------|--------|
| **Design Decision** | Why we chose approach X over Y | Add to "Design Decisions" section |
| **Project Convention** | How we do X in this project | Add to relevant section with examples |
| **New Pattern** | A reusable approach discovered | Add to "Patterns" section |
| **Forbidden Pattern** | Something that causes problems | Add to "Anti-patterns" or "Don't" section |
| **Common Mistake** | Easy-to-make error | Add to "Common Mistakes" section |
| **Convention** | Agreed-upon standard | Add to relevant section |
| **Gotcha** | Non-obvious behavior | Add warning callout |

### Step 3: Read the Target Code-Spec

Before editing, read the current code-spec to:
- Understand existing structure
- Avoid duplicating content
- Find the right section for your update

```bash
cat .trellis/spec/<category>/<file>.md
```

### Step 4: Make the Update

Follow these principles:

1. **Be Specific**: Include concrete examples, not just abstract rules
2. **Explain Why**: State the problem this prevents
3. **Show Contracts**: Add signatures, payload fields, and error behavior
4. **Show Code**: Add code snippets for key patterns
5. **Keep it Short**: One concept per section

### Step 5: Update the Index (if needed)

If you added a new section or the code-spec status changed, update the category's `index.md`.

---

## Update Templates

### Mandatory Template for Infra/Cross-Layer Work

```markdown
## Scenario: <name>

### 1. Scope / Trigger
- Trigger: <why this requires code-spec depth>

### 2. Signatures
- Backend command/API/DB signature(s)

### 3. Contracts
- Request fields (name, type, constraints)
- Response fields (name, type, constraints)
- Environment keys (required/optional)

### 4. Validation & Error Matrix
- <condition> -> <error>

### 5. Good/Base/Bad Cases
- Good: ...
- Base: ...
- Bad: ...

### 6. Tests Required
- Unit/Integration/E2E with assertion points

### 7. Wrong vs Correct
#### Wrong
...
#### Correct
...
```

### Adding a Design Decision

```markdown
### Design Decision: [Decision Name]

**Context**: What problem were we solving?

**Options Considered**:
1. Option A - brief description
2. Option B - brief description

**Decision**: We chose Option X because...

**Example**:
\`\`\`typescript
// How it's implemented
code example
\`\`\`

**Extensibility**: How to extend this in the future...
```

### Adding a Project Convention

```markdown
### Convention: [Convention Name]

**What**: Brief description of the convention.

**Why**: Why we do it this way in this project.

**Example**:
\`\`\`typescript
// How to follow this convention
code example
\`\`\`

**Related**: Links to related conventions or specs.
```

### Adding a New Pattern

```markdown
### Pattern Name

**Problem**: What problem does this solve?

**Solution**: Brief description of the approach.

**Example**:
\`\`\`
// Good
code example

// Bad
code example
\`\`\`

**Why**: Explanation of why this works better.
```

### Adding a Forbidden Pattern

```markdown
### Don't: Pattern Name

**Problem**:
\`\`\`
// Don't do this
bad code example
\`\`\`

**Why it's bad**: Explanation of the issue.

**Instead**:
\`\`\`
// Do this instead
good code example
\`\`\`
```

### Adding a Common Mistake

```markdown
### Common Mistake: Description

**Symptom**: What goes wrong

**Cause**: Why this happens

**Fix**: How to correct it

**Prevention**: How to avoid it in the future
```

### Adding a Gotcha

```markdown
> **Warning**: Brief description of the non-obvious behavior.
>
> Details about when this happens and how to handle it.
```

---

<!-- BEGIN skill-garden patch trellis-update-spec-autonomous-evaluation v0.6 -->
## Autonomous Spec Evaluation

This section replaces the interactive “whether to update” decision. The upstream code-spec depth and seven-section requirements remain authoritative when an update is necessary.

### Result Contract

Every invocation must evaluate the available evidence autonomously and return exactly one result:

```yaml
spec_update_result:
  status: no-op | written | needs-review
  reason: string
  evidence: [string]
  changed_files: [path]
  validation: [string]
```

- `no-op`: no reusable executable contract was learned, the existing specs already cover it, the change is only a one-off implementation/copy/formatting detail, or the user explicitly asked to skip spec updates in the current request.
- `written`: code or test evidence supports a new executable contract, one authoritative target spec is unambiguous, and the minimal write plus focused validation completed successfully.
- `needs-review`: the target spec, business semantics, conflict resolution, or a validation failure cannot be resolved uniquely from repository evidence.

Do not enter the upstream Interactive Mode and do not ask whether a spec should be updated. Only `needs-review` may stop the workflow, and it may ask only one question that is necessary to resolve the current ambiguity.

### Evidence Order

Read real evidence in this order. Do not decide from the chat summary, task title, or intuition alone:

1. For task context, the current task's `implement.jsonl` / `check.jsonl` and every file they reference.
2. For task context, the current task's `prd.md`, `design.md`, and `implement.md`.
3. For untracked context, `untracked_flow.py status --verbose` for work id, summary, and stage; obtain validation evidence from the current Check-All result and current change set rather than the cursor helper.
4. The final Check-All conclusion and its actual validation evidence.
5. The current subject's actual diff, source code, tests, and commit evidence.
6. Existing specs and their indexes returned by `spec_router.py`.

When there is no active task, use a valid current-session untracked state when present; otherwise require an explicit Update-Spec invocation and use the current request, actual diff, source/tests, and existing specs. Do not invent task evidence.

### Minimal Write Boundary

Capture the current dirty baseline before writing. `written` requires all of the following:

- Every change made by this Update-Spec invocation is under `.trellis/spec/**`. Do not modify business code, tests, workflow, skills, task artifacts, or any other file.
- Modify the smallest required section in the fewest files. Do not opportunistically rewrite, expand, reorganize, or format unrelated content.
- Prefer an existing authoritative spec. Create a new file only when no suitable spec exists, and update the corresponding index in the same invocation.
- Do not write a generic principle merely to avoid `no-op`. New content must provide a concrete executable contract such as signatures, fields, boundaries, error matrices, examples, or test assertions, while following the upstream seven-section requirements.

After writing, reread the spec diff and reverse-check it against source code and tests. At minimum run:

```bash
git diff --check -- .trellis/spec
```

When applicable, also validate indexes/links, code signatures, or project-specific spec checks. Fix uniquely resolvable validation failures inside this skill and rerun validation. Return `needs-review` for failures that cannot be resolved uniquely. If this invocation creates changes outside `.trellis/spec/**`, return `needs-review` with `reason=boundary-violation`, stop immediately, and do not proceed to Push. A completed `written` result does not trigger another manual Check-All.

### Workflow Disposition

- Interactive: after a passed Check-All stop, when the user says “下一步”, “继续”, `next`, `continue`, or an equivalent continuation intent, run this skill. A `no-op` or `written` result must load `trellis-push` in the same turn and present its single confirmation plan. A `needs-review` result stops and must not generate a Push plan.
- Interactive direct Git: when the latest user message that triggered the current completion chain explicitly requests an ordinary push or a user-initiated `commit-only`, use that request only as conditional continuation after a strictly passed Check-All. After the existing standard Check-All report is shown, run this skill in the same turn when no currently valid `spec_update_result` exists. Only `no-op` or `written` may proceed to `trellis-push`; `needs-review` stops. Do not infer this intent from history, summaries, dirty state, or an auto-loop internal `commit-only`.
- Validated auto-loop: for `no-op` or `written`, execute `record --action run_spec_update --result ok` and immediately run `next`. For `needs-review`, execute `record --action run_spec_update --result blocked --failure-type spec-needs-review`; never disguise it as `no-op`.
- Untracked: keep the cursor at `spec` for `needs-review`. For `no-op` or `written`, run `untracked_flow.py advance --stage push`. Any later product edit returns the cursor to `implement`; the helper does not validate or preserve owner evidence.

Do not ask again or rerun when a currently valid `no-op` or `written` result already exists. Re-evaluate after the actual diff, Check-All conclusion, or the user's spec intent changes.
<!-- END skill-garden patch trellis-update-spec-autonomous-evaluation v0.6 -->
## Quality Checklist

Before finishing your code-spec update:

- [ ] Is the content specific and actionable?
- [ ] Did you include a code example?
- [ ] Did you explain WHY, not just WHAT?
- [ ] Did you include executable signatures/contracts?
- [ ] Did you include validation and error matrix?
- [ ] Did you include Good/Base/Bad cases?
- [ ] Did you include required tests with assertion points?
- [ ] Is it in the right code-spec file?
- [ ] Does it duplicate existing content?
- [ ] Would a new team member understand it?

---

## Relationship to Other Commands

```
Development Flow:
  Learn something → /trellis:update-spec → Knowledge captured
       ↑                                  ↓
  /trellis:break-loop ←──────────────────── Future sessions benefit
  (deep bug analysis)
```

- `/trellis:break-loop` - Analyzes bugs deeply, often reveals spec updates needed
- `/trellis:update-spec` - Actually makes the updates
- `/trellis:finish-work` - Reminds you to check if specs need updates

---

## Core Philosophy

> **Code-specs are living documents. Every debugging session, every "aha moment" is an opportunity to make the implementation contract clearer.**

The goal is **institutional memory**:
- What one person learns, everyone benefits from
- What AI learns in one session, persists to future sessions
- Mistakes become documented guardrails
