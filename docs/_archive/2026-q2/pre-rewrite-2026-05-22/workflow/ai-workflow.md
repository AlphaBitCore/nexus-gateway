# AI Vibe-Coding Workflow

This repo doubles as an AI vibe-coding workbench. The `CLAUDE.md` rule set,
the `.cursor/rules/` catalog, the `.claude/skills/` library, and the
`scripts/check-*` lint suite together form a disciplined AI pair-programming
practice: the human supplies intent and judgment, the AI agent supplies
execution, and a system of guardrails keeps the two converging on
production-quality output.

This document explains the workflow itself. If you're forking this repo to
adopt the practice, read this end-to-end before invoking any agent.

---

## Why the artifacts exist

A capable AI agent left unconstrained drifts. It will:

- Refactor adjacent code "for cleanliness" while you asked for a bug fix.
- Add backward-compatibility shims for code that has no users yet.
- Mock the database in a test that's specifically meant to catch DB-prod
  divergence.
- Drift between sibling sessions sharing the same working tree.

Each of these has cost real engineering hours in this repo. The
artifacts below are the **fences** that grew up around the incidents.

## The four artifact layers

```
┌──────────────────────────────────────────────────────────────────┐
│  CLAUDE.md           ← canonical binding rules, English source   │
└──────────────────────────────────────────────────────────────────┘
              │ surfaces into IDE-side rule files
              ▼
┌──────────────────────────────────────────────────────────────────┐
│  .cursor/rules/      ← IDE rule surfacing, glob-scoped triggers  │
│  .claude/skills/     ← repeatable procedures (deploy/test/audit) │
└──────────────────────────────────────────────────────────────────┘
              │ enforced before commits ship
              ▼
┌──────────────────────────────────────────────────────────────────┐
│  scripts/check-*     ← lint suite, pre-commit + CI gates         │
└──────────────────────────────────────────────────────────────────┘
              │ verified at integration time
              ▼
┌──────────────────────────────────────────────────────────────────┐
│  go test -race / Vitest / smoke harnesses                        │
└──────────────────────────────────────────────────────────────────┘
```

Each layer **catches** mistakes the layer above missed. No single layer
is sufficient.

### Layer 1 — `CLAUDE.md`

Single canonical file at repo root, ~250 lines of dense English. Every
sentence is binding unless explicitly marked otherwise; each rule cites
the incident or principle that motivated it. Sections:

- **Mandatory rules.** Hard contracts — English-only, plan-first, complex-
  task Plan + Todo, worktree per session, unit-test coverage ≥ 95%, no
  inline yaml secrets, etc.
- **Pre-edit reading.** The 3-doc rule (architecture doc + feature doc +
  conventions doc) every edit must satisfy before code changes.
- **Mandatory development workflow.** The SDD pipeline (below).
- **Current state.** Service inventory + key facts an agent shouldn't
  rediscover.
- **Project structure / Tech stack / Conventions.** Soft style guidance.

`CLAUDE.md` is *always loaded* into the agent's context. Don't put
ephemeral content here — it's where the load-bearing rules live.

### Layer 2 — `.cursor/rules/` and `.claude/skills/`

Two parallel surfacings of the same workflow, one per IDE.

**`.cursor/rules/`** — `.mdc` files with frontmatter. Two flavors:

- `alwaysApply: true` rules are concatenated into every prompt regardless
  of file context. These are *meta-rules*: SDD workflow, pre-edit reading,
  binding-rules-quick-reference, completion-time self-audit, session
  handoff. They don't duplicate `CLAUDE.md` — they index it.
- `globs:` rules fire when a matching file is open or staged. These
  carry **domain-specific bindings**: agent runtime invariants, IAM impact
  review, provider adapter rules, NE fail-open safety, IoT terminology
  boundary, etc.

Total in this repo: 35 rules, 14 always-apply.

**`.claude/skills/`** — invocable as `/skill-name` in Claude Code, each
skill is a self-contained procedure document. Buckets:

- **Deployment / ops.** `prod-deploy`, `prod-login`, `prod-debug`,
  `build-agent` — battle-tested operational runbooks with hard guards.
- **Testing.** `test-all`, `smoke-gateway`, `test-cursor-adapter`,
  `test-compliance-proxy` — end-to-end smoke harnesses.
- **Architecture / design.** `spec-writing`, `add-provider-adapter`,
  `adapter-conformance-check`, `project-review`.
- **Debug / audit.** `frontend-bug-trace`, `i18n-gap-check`,
  `iam-impact-review`, `ne-fail-open-audit`.

A full catalog with adoption guidance for forks lives at
[`docs/developers/workflow/ai-skill-catalog.md`](./ai-skill-catalog.md).

### Layer 3 — `scripts/check-*`

The lint suite. ~23 scripts, each enforcing one binding rule. Wired in
both directions:

- **Pre-commit** (`.githooks/pre-commit`) — staged-file-scoped, runs in
  <1s per check. 14 HARD gates (block commit) + 3 SOFT (warn). Bypass
  requires explicit user approval per `CLAUDE.md`.
- **CI** (`.github/workflows/ci.yml`) — full-tree strict mode. The
  `governance-lints` job runs the strict variants on every PR.

Each script aims for:

- **Actionable error messages.** Don't just say "FAILED" — say what to
  fix, where, and link to the binding rule.
- **Sub-second runtime.** Fast feedback or developers route around the gate.
- **Allowlist over deletion.** Where pre-existing violations exist, an
  allowlist with documented categories preserves the gate's intent
  without blocking unrelated commits.

### Layer 4 — Tests

`go test -race -count=1` on every Go package, Vitest on the UI, end-to-end
smoke harnesses on the AI gateway and compliance proxy. The lint layer
verifies *shape*; tests verify *behavior*.

The 95% coverage gate (see [`docs/developers/workflow/coverage-allowlist-methodology.md`](../../../docs/developers/workflow/coverage-allowlist-methodology.md))
is the binding contract here.

---

## The SDD pipeline

Every code change above triviality follows this order:

```
Plan + Todo  →  Architecture  →  Requirements  →  SDD  →  OpenAPI  →
Code  →  Unit Tests  →  Verify  →  Ask-about-commit
```

- **Plan + Todo (Step 0).** Before any edit. The agent writes a plan
  (Cursor Plan Mode or Claude Code TaskCreate). Get user confirmation
  before code touches disk. Complex tasks (cross-cutting, >2 files,
  high-blast-radius surfaces) cannot waive this step.
- **Architecture (Step 1).** Read the relevant `docs/developers/architecture/*-architecture.md`
  doc — the trigger map at [`docs/developers/architecture/README.md`](../architecture/README.md)
  tells you which. If the change has architectural impact, update the
  doc in the same PR. If not, record "no architecture impact" in the plan.
- **Requirements (Step 2).** `docs/developers/specs/e<epic>-<name>.md` —
  Functional/Non-Functional Requirements, User Roles, Constraints,
  Glossary, MoSCoW priority.
- **SDD (Step 3).** `docs/developers/specs/e<epic>-s<story>-<name>.md` — Stories +
  tasks + acceptance criteria.
- **OpenAPI (Step 4).** `docs/users/api/openapi/e<epic>-s<story>-<name>.yaml` —
  paths, schemas, error responses, examples. The Control Plane UI
  service layer must match.
- **Code (Step 5).** Implementations conform to the OpenAPI; Prisma
  models align with spec schemas. No placeholder production code.
- **Unit tests (Step 6).** Go `-race -count=1` + Vitest. Each test
  asserts named behavior, not "function returns nil".
- **Verify (Step 7).** `npm test` + package scripts green. Run the
  4-question completion self-audit (below). Confirm acceptance
  criteria met. Ask user about commit.

This is the *order* — not phases-of-rollout. The repo has no shipped
backward-compatibility shims; pre-GA refactors delete obsolete code
outright (see `CLAUDE.md` → "Development-phase policy: no backward
compatibility").

---

## The 2-round completion self-audit

Before any "I'm done" claim:

**Round 1 — surface issues:**

- **Q1.** Every todo completed (not deferred, not silently dropped)?
- **Q2.** No `TODO` / `FIXME` / `XXX` / `unimplemented` / `not implemented` /
  `stub` strings in production code? (`scripts/check-no-prod-todos.mjs`
  catches this mechanically.)
- **Q3.** Every changed code path exercised by a real test OR explicitly
  acknowledged as untested with a reason?
- **Q4.** No "we'll fix this later" claims unless the user explicitly said so?

Fix every issue surfaced; record explicit follow-ups as todos if scope
must defer.

**Round 2 — verify fixes:** Re-run the same 4 questions. If Round 2
surfaces new issues, run Round 3. Keep going until **two consecutive
rounds are clean.**

Then ask the user about committing — never auto-commit.

---

## Worktree per session

Multiple agent sessions can run concurrently, each in its own
`git worktree`. The working tree, index, and `.git/index.lock` are
private to that session, so `git stash`, `git add -A`, and `git restore`
are all safe inside your own worktree. `CLAUDE.md` → "Worktree per
session" is the binding source; the conventions:

1. **Spawn**: `git worktree add ./worktrees/<topic> [-b feature/<name>] [<base>]`.
   `./worktrees/` is gitignored.
2. **Clean up after merge**: `git worktree remove ./worktrees/<topic>`;
   delete the branch if fully merged.
3. **One branch per worktree** — git enforces this. Create a sibling
   branch for a second session on the same logical work.
4. **Shared module coordination** — worktrees isolate the working tree,
   not the module graph. Two worktrees editing the same
   `packages/shared/<subpkg>/` file at once will conflict at merge.
   Coordinate at the subpackage level; pick disjoint shared sub-packages.
5. **Commit hygiene**: `git status --short` before commit;
   `git log -1 --stat` after.
6. **`.git/index.lock` in your worktree** = your own prior git invocation
   crashed mid-flight. Investigate freely.

Worktrees make all standard git operations (stash, add -A, restore) safe inside the session's own tree.

---

## Memory and handoff

For multi-session work, the agent keeps a maintainer-local memory at
`.claude/projects/<project>/memory/` with typed entries (user / feedback /
project / reference) — the canonical anchor for "facts that survive
across sessions".

When a session approaches context-full or closes a major program, the
agent proactively writes a handoff document. Template at
[`docs/developers/workflow/handoff-template.md`](./handoff-template.md); rule at
[`.cursor/rules/session-handoff.mdc`](../../../.cursor/rules/session-handoff.mdc).

---

## What this isn't

- **It isn't a "let the AI cook" workflow.** Every layer requires human
  judgment at decision points: plan approval, complex-task waivers,
  commit-time review.
- **It isn't dogma.** Rules cite incidents, not preferences. A rule
  whose incident no longer applies can be retired — see the periodic
  rule-drift audit pattern in
  [`docs/_archive/2026-q2/programs/audit-skills-rules-lint-docs-2026-05-19.md`](../../_archive/2026-q2/programs/audit-skills-rules-lint-docs-2026-05-19.md).
- **It isn't proprietary.** This whole layer set is in the repo. Forks
  are encouraged to adopt the pattern, adapt the rules to their domain
  incidents, and contribute back improvements.

---

## Adopting this workflow in a fork

If you're cloning this practice into your own repo:

1. **Start with `CLAUDE.md`.** Keep the meta-rules (Plan + Todo,
   worktree per session, completion self-audit, English-only,
   commit reminder). Replace the project-specific bindings with your
   own incidents as they accumulate.
2. **Adapt the cursor rules.** Pull the meta-rules verbatim (sdd-workflow,
   pre-edit-reading, completion-time-self-audit, session-handoff,
   complex-task-plan-todo, binding-rules-quick-reference,
   english-only). Drop the domain rules and grow your own from
   incidents.
3. **Cherry-pick lints.** Most generic: `check-no-prod-todos.mjs`,
   `check-no-yaml-secrets.mjs`, `check-arch-doc-triggers.mjs`,
   `check-go-coverage.sh`. Skip the UI-specific ones if you don't
   have a UI surface.
4. **Wire pre-commit + CI in parallel.** Pre-commit catches the 95%
   case fast; CI catches the rest with no escape hatch.
5. **Grow skills from procedures you repeat 3+ times.** Don't make a
   skill for a one-off; do make one for "deploy to prod" the third
   time you do it.

Adopt slowly. The discipline compounds.

---

## Related reading

- [`CLAUDE.md`](../../../CLAUDE.md) — canonical binding rules.
- [`CONTRIBUTING.md`](../../../CONTRIBUTING.md) — how to contribute, mirrors
  the SDD pipeline for human contributors.
- [`docs/developers/architecture/README.md`](../architecture/README.md) —
  the trigger map for the 3-doc pre-edit rule.
- [`docs/developers/workflow/handoff-template.md`](./handoff-template.md) — session-handoff
  document template.
- [`docs/developers/workflow/coverage-allowlist-methodology.md`](../../../docs/developers/workflow/coverage-allowlist-methodology.md) —
  the 95% gate's allowlist policy and audit methodology.
- [`docs/developers/workflow/ai-skill-catalog.md`](./ai-skill-catalog.md) — the
  `.claude/skills/` catalog with adoption guidance for forks.
