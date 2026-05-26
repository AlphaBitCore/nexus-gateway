# pre-edit-reader

Walk a contributor through the 3-doc pre-edit reading requirement before they start a code change. Shortcut version of the binding rule in CLAUDE.md.

Use this skill when:

- A new contributor is about to make their first code change.
- You're not sure which docs apply to your edit area.
- A teammate's PR is failing review because they skipped reading the architecture doc.

---

## The 3 required docs (every code change)

Before writing any code change, **read all three**:

1. **Architecture doc(s)** — find your edit area in `docs/developers/architecture/README.md` and open the listed doc(s).
2. **Feature doc(s)** — if the change affects a user-visible surface, open the matching doc:
   - CP-UI menu sections → `docs/users/features/cp-ui/<section>.md`.
   - Agent-UI pages → `docs/users/features/agent-ui/<page>.md`.
   - Cross-feature flows (admin → CP → Hub → effect → audit) → `docs/users/features/flows/<flow>.md`.
3. **Code conventions** — `docs/developers/workflow/conventions.md` for the soft style guidance.

## Walkthrough

### Step 1 — Identify your edit area

Run:

```bash
git status
```

Note the files / directories that will change.

### Step 2 — Architecture doc lookup

```bash
# Open the trigger map and search for your file glob:
grep -n "<key-path>" docs/developers/architecture/README.md

# Or just open it and read the table top-to-bottom:
$EDITOR docs/developers/architecture/README.md
```

The match column lists the doc(s) to read. Read them in full or skim the relevant sections.

If your edit area is **not** matched in the table, that's a signal:
- Either the architecture is genuinely undocumented → tell the user, propose adding a new arch doc.
- Or your edit area is small enough to be implicit in an existing doc → ask the user to confirm the right reference.

### Step 3 — Feature doc lookup (if user-facing)

```bash
ls docs/users/features/
ls docs/users/features/cp-ui/
ls docs/users/features/agent-ui/
ls docs/users/features/flows/
```

If your change affects:

- A CP-UI menu section → read the matching `cp-ui/*.md`.
- An Agent-UI page → read the matching `agent-ui/*.md`.
- An end-to-end flow → read the matching `flows/*.md`.

If your change doesn't affect a user-facing surface (pure internal refactor / library change), skip this step but say so in the plan.

### Step 4 — Conventions

If you haven't internalised `docs/developers/workflow/conventions.md` for this language / area, open it. Pay particular attention to:

- The PR review checklist (§11).
- The Go conventions (§2) or TypeScript conventions (§3) per your change.
- The commit style (§8).

### Step 5 — Confirm to the user

Echo back in chat:

```
Pre-edit reading checklist:
- Architecture: <doc-name>.md ✓
- Feature doc: <surface>.md ✓ (or "no user-facing change")
- Conventions: confirmed
```

This makes the 3-doc trip explicit and reviewable.

## When a doc is missing

Don't proceed silently. Surface it:

- "I'm editing `packages/<x>/<y>/...` but I don't see a row in the trigger map. Should we add a new arch doc?"
- "I'm changing the Settings page but `docs/users/features/cp-ui/setup-status-system.md` is light on this section. Should we extend it as part of this change?"

The doc set grows by signal, not by guesswork.

## Skipping the rule

Allowed only with **explicit user approval** in chat. For complex tasks the rule is non-waivable (see CLAUDE.md "Complex tasks: Plan + Todo are NON-WAIVABLE" and `.cursor/rules/complex-task-plan-todo.mdc`).
