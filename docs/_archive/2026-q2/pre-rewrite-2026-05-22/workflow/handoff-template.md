# Session Handoff — Template

> When a session is approaching context-full, closing out a multi-phase
> program, or has accumulated >~50 tool-call turns of state, produce a
> handoff doc following this template. Bound by `CLAUDE.md` → "Handoff at
> context-full" and surfaced by [`.cursor/rules/session-handoff.mdc`](../../../.cursor/rules/session-handoff.mdc).
>
> Location: `docs/handoffs/program-name.md` for repo-tracked
> programs, or `<program-area>/HANDOFF.md` for area-local plans.

---

## 1. Mission

<!--
Program goal in 1–3 sentences. Why does this work exist? What does "done" look like?
Current phase (e.g. "Phase 3 of 5 — adapter conformance audit").
-->

## 2. Architecture model

<!--
Load-bearing facts the next session needs to design / debug against
without re-reading the whole codebase. Keep DENSE — tables > prose.

Examples of what belongs here:
- Service boundary table (who owns what)
- Data flow ASCII diagram (5 services, who calls whom)
- The 3–5 invariants the program relies on staying true
-->

## 3. API / surface inventory

<!--
What the program operates on. Concrete:
- Routes touched
- Tables touched
- Config keys touched
- Skill triggers touched
- Memory anchors that hold load-bearing facts
-->

## 4. Work units catalog

<!--
Concrete units the next session executes. One row per unit; mark status.

| Unit | Status | File / location | Acceptance |
|---|---|---|---|
| Example: codec audit for spec X | done | packages/.../codec.go | passes adapter-conformance-check |
| Example: SDD for E61-S3 | pending | docs/developers/specs/e61-s03-...md | written + reviewed |
-->

## 5. Existing infrastructure

<!--
What to BUILD ON, what to AVOID rewriting:
- Scripts / skills already available (cite paths)
- Tests / harnesses already wired (cite paths)
- Tables / migrations already in place (cite migration timestamps)

Save the next session from re-discovering these.
-->

## 6. Binding rules in play

<!--
Pointers into CLAUDE.md + memory anchors the next session MUST load
before working. Don't restate the rule text — just the anchor:
- "Adapter format-translation rules (Rules 1–7)"
- "Worktree per session (CLAUDE.md → Worktree per session)"
- "macOS NE fail-open invariants"

Plus relevant memory anchors as `[[memory-name]]` links.
-->

## 7. Recent changes affecting the work

<!--
Renames, deletions, contract changes since the program plan was first
drafted. Quick way for the next session to update its mental model.

Use git: `git log --oneline --since="<plan date>" -- <relevant paths>`.
-->

## 8. Suggested program structure

<!--
Phases + first concrete steps for the next session:

### Phase A — <name>
1.
2.
3.

### Phase B — <name>
1.
2.
-->

## 9. Repo state snapshot

<!--
Run-state info the next session can't reconstruct from git:
- Branch + tip commit hash
- Known dirty WIP from parallel sessions (yaml configs, in-flight edits)
- Open PRs touching the area
- Any `.git/index.lock` weirdness or unmerged paths

Capture with:
- `git status --short`
- `git log -1 --oneline`
- `gh pr list --search "<keyword>"`
-->

---

## Why this template exists

Auto-compact summarises a conversation but loses fidelity on specifics
(file paths, struct fields, contract details). Maintainer-local memory
persists across sessions but is one-line indexed — it can't carry a
300-line architecture map. An on-disk handoff doc is **the single source
of truth** the next session reads; memory + handoff doc together = full
handoff.

## Worked examples

Past handoff docs in this repo (use as reference when filling out a
new one):

- `docs/_archive/2026-q2/programs/test-scenarios-program-plan.md` — 2026-05-16 OSS
  readiness review closeout → API automation test program handoff.
- `docs/_archive/2026-q2/programs/test-coverage-program-handoff.md` — 95% coverage program
  handoff after Phase 4 capstone.
- `docs/_archive/2026-q2/handoffs/e60-attestation-handoff.md` — agent attestation feature
  handoff between sessions.

The worked examples above are program-specific artifacts under
`docs/_archive/2026-q2/{programs,handoffs}/` and may be moved or
renamed over the program's lifetime — **this template** is the stable
anchor for the *structure*.
