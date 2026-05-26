---
name: gap-review
description: >
  Review gaps between SDD documents (source of truth) and all related artifacts:
  architecture, requirements, OpenAPI specs, code implementation, and unit tests.
  Creates a plan and todo list to bring docs and code into alignment.
  Use when docs are out of sync with code, after code-heavy changes, or to audit
  spec-code parity. Trigger keywords: gap review, sync docs, doc drift, code-doc
  mismatch, audit docs, review gaps, spec parity, doc check.
user-invocable: true
---

# Gap Review — SDD-Driven Parity Audit

This skill audits the alignment between **SDD documents** (the source of truth) and all downstream artifacts: architecture, requirements, OpenAPI specs, code implementation, and unit tests. It identifies gaps in both directions — docs that lag behind code **and** code that does not fully implement the SDD — then produces a prioritized plan to fix everything.

**Core principle:** The SDD defines what the system *should* do. Code that diverges from SDD is either a doc gap (SDD needs updating to match intentional code changes) or a code gap (code is incomplete). This skill distinguishes between the two and fixes both.

---

## Invocation Protocol

When this skill is triggered, follow these steps **in order**. Do not skip steps. Do not start fixing anything before the audit is complete.

---

### Phase 1: Scope Selection

Ask the user:

1. **Which epics/stories to audit?**
   - All epics (full audit)
   - Specific epic(s) (e.g., E1, E7)
   - Specific story/stories (e.g., e1-s1, e7-s4)
   - Auto-detect from recent git changes (use `git log --name-only` to find recently changed source files, then map them back to SDD stories)

2. **What is the priority?**
   - **Code gaps** (code does not implement SDD) — fix code first
   - **Doc gaps** (docs do not reflect code) — update docs first
   - **Both equally** (default)

If the user says "just run it" or similar, default to: **auto-detect from recent git changes, both equally**.

---

### Phase 2: SDD Inventory & Mapping

For each story in scope:

1. **Read the SDD file** from `docs/developers/specs/e{epic}-s{story}-{name}.md`
2. **Extract structured data:**
   - Story statement (As a / I want / So that)
   - All tasks (T1, T2, ...)
   - All acceptance criteria (checked and unchecked)
   - Dependencies
3. **Map to related artifacts:**

| Artifact | Location | How to find |
|----------|----------|-------------|
| Architecture | `docs/users/product/architecture.md` (system overview) + per-module docs via `docs/developers/architecture/README.md` (e.g., `hook-architecture.md`, `routing-architecture.md`) | Find the SDD area in the trigger map; open the listed module arch doc(s). |
| Requirements | `docs/developers/specs/e{epic}-*.md` | Match by epic number |
| OpenAPI spec | `docs/users/api/openapi/e{epic}-s{story}-*.yaml` | Match by epic-story |
| Aggregate OpenAPI | `docs/users/api/openapi/gateway-api.yaml` | Check if story endpoints are included |
| Code (gateway) | `packages/gateway/src/` | Match by feature area from SDD tasks |
| Code (dashboard) | `packages/dashboard/src/` | Match by feature area from SDD tasks |
| Unit tests | `packages/*/src/**/*.test.ts`, `packages/*/src/**/*.spec.ts` | Match by module under test |

4. **Build a mapping table** for user visibility:

```
Story: e1-s1-proxy-infrastructure
  SDD:          docs/developers/specs/e1-s1-proxy-infrastructure.md
  Requirements: docs/developers/specs/e1-traffic-interception.md
  OpenAPI:      docs/users/api/openapi/e1-s1-proxy-infrastructure.yaml
  Code:         packages/gateway/src/proxy/, packages/gateway/src/health/
  Tests:        packages/gateway/src/proxy/__tests__/
```

---

### Phase 3: Gap Analysis (the core audit)

For each story in scope, perform **five checks**. Use deep reasoning — read actual file contents, do not guess from filenames alone.

#### Check 1: SDD vs Code Implementation

For each task and acceptance criterion in the SDD:

- **Read the relevant source files** in `packages/gateway/src/` and `packages/dashboard/src/`
- Verify the behavior described in the SDD task is actually implemented
- Check that each acceptance criterion has corresponding code logic
- Look for:
  - Missing endpoints or handlers
  - Partially implemented features (e.g., endpoint exists but skips validation)
  - Logic that contradicts the SDD
  - TODO/FIXME/stub code that should be real implementation
  - Features described in SDD that have no code at all

**Output:** List of **Code Gaps** (SDD says X, code does not do X)

#### Check 2: Code vs SDD (reverse direction)

- Scan the code files mapped to this story
- Identify behaviors, endpoints, or logic that exist in code but are **not described** in the SDD
- These represent either:
  - **Intentional improvements** that the SDD should be updated to reflect
  - **Scope creep** that should be reviewed

**Output:** List of **SDD Gaps** (code does X, SDD does not mention X)

#### Check 3: SDD vs OpenAPI Spec

For stories with API endpoints:

- Compare SDD task descriptions against the OpenAPI spec
- Check: paths, methods, request/response schemas, error codes, security requirements
- Verify the aggregate `gateway-api.yaml` includes these endpoints
- Look for:
  - Endpoints in SDD but missing from OpenAPI
  - Schema mismatches (field names, types, required vs optional)
  - Error responses defined in SDD but missing from OpenAPI
  - OpenAPI endpoints not traceable to any SDD task

**Output:** List of **OpenAPI Gaps**

#### Check 4: SDD vs Requirements Document

- Compare SDD story and tasks against the parent requirements document
- Check:
  - Every FR referenced in the requirements has at least one SDD task covering it
  - NFRs have corresponding acceptance criteria or implementation
  - Traceability section in requirements doc is accurate
- Look for:
  - Requirements with no SDD coverage
  - SDD tasks that address requirements not listed in the requirements doc
  - Priority mismatches

**Output:** List of **Requirements Gaps**

#### Check 5: SDD vs Unit Tests

- Find test files corresponding to the code modules for this story
- Check:
  - Each acceptance criterion has at least one test that validates it
  - Test descriptions align with SDD language
  - No major behavioral paths are untested
- Look for:
  - Acceptance criteria with zero test coverage
  - Tests that exist but do not match current SDD (stale tests)
  - Missing test files for implemented modules

**Output:** List of **Test Gaps**

---

### Phase 4: Gap Report

Present findings in a structured report, organized by story, sorted by severity:

```
# Gap Review Report
Date: YYYY-MM-DD
Scope: [what was audited]

## Summary
| Category       | Critical | High | Medium | Low | Total |
|----------------|----------|------|--------|-----|-------|
| Code Gaps      |          |      |        |     |       |
| SDD Gaps       |          |      |        |     |       |
| OpenAPI Gaps   |          |      |        |     |       |
| Requirements   |          |      |        |     |       |
| Test Gaps      |          |      |        |     |       |

## Story: e{N}-s{M}-{name}

### Code Gaps (SDD not implemented)
| # | Severity | SDD Task/AC | Expected | Actual | Fix |
|---|----------|-------------|----------|--------|-----|

### SDD Gaps (code not documented)
| # | Severity | Code Location | Behavior | Recommended SDD Update |
|---|----------|---------------|----------|------------------------|

### OpenAPI Gaps
| # | Severity | Issue | SDD Reference | Fix |
|---|----------|-------|---------------|-----|

### Requirements Gaps
| # | Severity | Issue | FR/NFR ID | Fix |
|---|----------|-------|-----------|-----|

### Test Gaps
| # | Severity | AC Reference | Missing Coverage | Fix |
|---|----------|--------------|------------------|-----|
```

**Severity definitions:**
- **Critical**: Core feature missing or broken; system does not behave as specified
- **High**: Important behavior gap; feature works but is incomplete or incorrect
- **Medium**: Minor behavioral mismatch; docs or code slightly out of sync
- **Low**: Cosmetic or documentation-only; no functional impact

---

### Phase 5: Plan & Todo List — REQUIRED

After presenting the report and getting user confirmation:

1. **Create a plan** covering all gaps to fix, ordered by:
   - Critical code gaps first (broken functionality)
   - High code gaps (incomplete features)
   - SDD/doc updates (bring docs in line with code)
   - OpenAPI spec updates
   - Requirements doc updates
   - Architecture doc updates (if affected)
   - Test additions
   - Medium/Low gaps last

2. **Create a todo list** (TaskCreate) with one task per fix item. Each task must specify:
   - What to change (file path and description)
   - Which gap it resolves (reference the gap report)
   - Whether it is a code fix or a doc update

3. **Group tasks by story** for logical execution order.

4. **Get user approval** on the plan before starting any fixes.

---

### Phase 6: Execute Fixes

After user approves the plan:

#### For Code Gaps (SDD not fully implemented):
1. Read the SDD task and acceptance criteria carefully
2. Read the existing code
3. Implement the missing behavior to satisfy the SDD
4. Add or update unit tests for the new/changed behavior
5. Run tests to verify

#### For Doc Gaps (docs behind code):
1. Read the current code behavior
2. Update the relevant doc to accurately describe what the code does:
   - **SDD**: Add/update tasks and acceptance criteria
   - **OpenAPI**: Add/update paths, schemas, responses
   - **Requirements**: Add/update FRs, NFRs, traceability
   - **Architecture**: Update component descriptions, data flows
3. Ensure cross-references between docs remain consistent

#### For Test Gaps:
1. Read the SDD acceptance criterion
2. Read the code under test
3. Write Vitest unit tests that validate the acceptance criterion
4. Run tests to verify they pass

**After each task:** Mark it completed in the todo list.

---

### Phase 7: Verification

After all fixes are applied:

1. Run `npm test` (or workspace-level test command) to confirm all tests pass
2. Verify each gap from the report is resolved
3. Spot-check cross-references between updated docs
4. Present a summary of what was fixed
5. **Ask the user whether they want to commit** (mandatory per project rules; do not auto-commit)

---

## Severity Heuristics

Use these to assign severity consistently:

| Situation | Severity |
|-----------|----------|
| SDD says endpoint exists, code has no handler | Critical |
| SDD acceptance criterion not met by code | High |
| Code has feature not mentioned in SDD | Medium |
| OpenAPI schema field mismatch (type, required) | High |
| OpenAPI missing error response that code returns | Medium |
| Requirement FR has no SDD task | High |
| Acceptance criterion has no test | Medium |
| Test exists but does not match current SDD wording | Low |
| Architecture doc missing a new component | Medium |
| Doc typo or outdated example | Low |

---

## Rules

1. **SDD is the source of truth** — when SDD and code disagree, assume the SDD is correct unless the code change was clearly intentional (e.g., merged PR, commit message explains the divergence). When in doubt, ask the user.
2. **Read before judging** — always read file contents. Do not infer gaps from filenames or directory structure alone.
3. **Do not fix during audit** — complete the full gap report before making any changes. Premature fixes can mask cascading issues.
4. **One concern per gap** — each gap entry should describe exactly one mismatch. Do not bundle multiple issues.
5. **Trace everything** — every gap must reference the specific SDD task/AC, code location, or doc section involved.
6. **English only** — all output and doc updates must be in English per project rules.
7. **No placeholder fixes** — when fixing code gaps, deliver complete implementations per the SDD. No TODO/FIXME/stubs in production code.
8. **Preserve existing behavior** — when updating docs to match code, do not change the code unless it is actually broken. When fixing code to match SDD, do not alter unrelated code.
