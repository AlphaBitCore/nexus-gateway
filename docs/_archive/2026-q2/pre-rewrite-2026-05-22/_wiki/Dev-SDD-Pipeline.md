# Dev SDD Pipeline

*Audience: active contributors working on new features or significant changes.*

The SDD (Story-Driven Development) pipeline is the mandatory sequence every code change above a trivial one-liner follows. It encodes the hard-won lesson that skipping any step — architecture, requirements, spec, test — produces one of a small set of predictable failure modes: a structural mistake caught at review, a user experience inconsistency, a silent 403 from an IAM mismatch, or a production incident from untested code. The pipeline converts intent into verified, documented, tested output. This page explains each step and what to produce at each one.

---

## The sequence

```
Plan + Todo  →  Architecture  →  Requirements  →  SDD  →  OpenAPI
  →  Code  →  Unit Tests  →  Verify  →  Ask-about-commit
```

"Phase" in any plan means **order of work**, not a compatibility layer. Pre-GA, every refactor is greenfield — delete obsolete code outright rather than running parallel paths.

---

## Step 0 — Plan + Todo

Every change starts with a written plan and a live todo list. This applies to one-line fixes as well as multi-month epics.

**What to write:**
- **Goal**: one-line statement of the user's request.
- **Approach**: how the change will be made.
- **Scope**: what files and packages are touched.
- **Risks**: what could go wrong; which blast-radius surfaces are involved.
- **File touch list**: a concrete enumeration of files that will change.

**Capture the plan as todos.** Each item must be specific, actionable, and verifiable. Never leave a falsely `in_progress` item. New requests during a multi-step session do not interrupt in-flight work — capture them as new todos and finish the current work to a commit point first.

A task is **complex** if any of these apply: more than 2 files changed; cross-cutting (multi-service, IAM, MQ schemas, `packages/shared/`); introduces or modifies an SDD epic or story; touches API contract, data model, or migration; or touches a high-blast-radius surface (macOS NE extension, admin endpoint registration, IAM policies, credential encryption). Complex tasks cannot waive Plan + Todo.

---

## Step 1 — Architecture

Before writing any code, read the relevant architecture docs.

**Where to find them:** [`docs/developers/architecture/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/README.md) is the trigger map — every package and subsystem has a row pointing to its architecture doc. If the edit area has no row, that gap is worth raising.

**What to check:** Does the change affect service boundaries, data flow, external integrations, or deployment topology? If yes, update the architecture doc in the same PR. If no, record "no architecture impact" explicitly in the plan — do not skip silently.

Common docs by area:

| Area | Architecture doc |
|---|---|
| AI Gateway providers | `provider-adapter-architecture.md` |
| Config sync / shadow | `thing-config-sync-architecture.md` |
| IAM / roles | `iam-identity-architecture.md` |
| Cost estimation | `cost-estimation-architecture.md` |
| Prompt cache | `prompt-cache-architecture.md` |
| macOS NE proxy | `agent-ne-fail-open-architecture.md` |

---

## Step 2 — Requirements

For new features, write a requirements document at `docs/developers/specs/e{epic}-{name}.md`.

**What to include:**
- **Functional requirements** — what the system must do.
- **Non-functional requirements** — performance, security, observability constraints.
- **User roles and personas** — who is affected.
- **Constraints and assumptions** — what is out of scope, what is taken for granted.
- **Glossary** — new terms introduced.
- **MoSCoW priority** — Must / Should / Could / Won't for each requirement.

Source the requirements from `docs/users/product/overview.md`, `docs/users/product/features.md`, and the conversation with the user. Requirements become the binding contract for the SDD step.

---

## Step 3 — SDD (Story-Driven Design)

Write a story-driven design document at `docs/developers/specs/e{epic}-s{story}-{name}.md`.

**What to include:**
- **User story statement**: "As a [role], I want [feature] so that [value]."
- **Tasks**: discrete, ordered implementation tasks.
- **Acceptance criteria**: testable conditions for "done".

Break the epic into stories; each story maps to one PR or a small set of tightly related PRs. The SDD is the contract the tests are written against — acceptance criteria drive the test assertions.

---

## Step 4 — OpenAPI

For every story with an API endpoint, write an OpenAPI 3.1 spec at `docs/users/api/openapi/e{epic}-s{story}-{name}.yaml`.

**What to include:**
- Path definitions with request and response schemas.
- Error responses (4xx, 5xx) with example bodies.
- At least one success example per path.

The Control Plane UI service layer and the Go route handler must match the OpenAPI spec exactly. Drift between spec and handler is what produces the silent 403s that the IAM impact review rule is designed to prevent.

---

## Step 5 — Code

Implement to match the OpenAPI spec and SDD acceptance criteria.

**Rules:**
- Go route handlers conform to the OpenAPI. Prisma models align with spec schemas. Control Plane UI service layer matches the spec.
- No placeholder production code. No `TODO`/`FIXME`/`XXX`, no empty handlers, no stub returns.
- Keep diffs minimal and scoped. A PR that incidentally refactors adjacent code is harder to review and harder to revert.
- Update todos as tasks complete. Never leave a falsely `completed` item.

**Code / doc lockstep:** When code in a mapped glob changes, the matching docs must also change in the same commit. The mapping lives in `scripts/doc-lockstep.config.mjs`; the check runs at `npm run check:doc-lockstep`. See [Dev Code Doc Lockstep](Dev-Code-Doc-Lockstep) for the full rule.

---

## Step 6 — Unit tests

Write tests before marking any step complete.

**Go:** `go test -race -count=1` per package. Table-driven tests with distinct case identities. No nesting beyond two levels.

**TypeScript / UI:** Vitest, with React Testing Library for component tests.

**What makes a test count:** Tests must assert **observable business behavior and named failure modes**, not just that a function ran without error. A test that calls `handler.Process(input)` and asserts only `err == nil` is coverage padding — it passes without verifying real state.

The 5-step audit methodology for writing tests that count is in [`docs/developers/workflow/coverage-allowlist-methodology.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/coverage-allowlist-methodology.md).

See [Dev Testing Coverage](Dev-Testing-Coverage) for the 95% rule and allowlist policy.

---

## Step 7 — Verify

The verify step is the gate before asking the user about committing.

Run the full check suite:

```bash
npm run check:all         # all lint + coverage checks
go test -race -count=1 ./...   # per module, or npm test for workspace-level
```

Then run the 2-round completion self-audit:

**Round 1 — surface issues:**
- **Q1**: Every todo completed (not deferred, not silently dropped)?
- **Q2**: No `TODO`/`FIXME`/`XXX`/`unimplemented`/`stub` strings in production code?
- **Q3**: Every changed code path exercised by a real test, or explicitly acknowledged as untested with a reason?
- **Q4**: No "we'll fix this later" claims unless the user explicitly said so?

Fix every issue Round 1 surfaces.

**Round 2 — verify fixes:** Re-run all four questions. If Round 2 surfaces new issues, run Round 3. Keep going until two consecutive rounds are clean.

After a clean double round, ask the user whether to commit. Never commit without explicit instruction.

---

## Step 8 — Ask-about-commit

After a clean verify pass, ask the user whether to commit. The suggested commit message format is:

```
<type>(<scope>): <imperative summary under 72 chars>

<body: why the change was needed, referencing the plan or incident>
```

Types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`. Scope is the package or service name.

---

## Canonical docs

- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — the binding rules that frame this pipeline ("Mandatory Development Workflow" section)
- [`docs/developers/workflow/ai-workflow.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/ai-workflow.md) — full AI vibe-coding workflow with detailed rationale for each step
- [`docs/developers/architecture/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/README.md) — the trigger map for Step 1

**Adjacent wiki pages**: [Contributing](Contributing) · [Dev Code Doc Lockstep](Dev-Code-Doc-Lockstep) · [Dev Testing Coverage](Dev-Testing-Coverage) · [Dev Code Review Checklist](Dev-Code-Review-Checklist) · [Workbench Overview](Workbench-Overview)
