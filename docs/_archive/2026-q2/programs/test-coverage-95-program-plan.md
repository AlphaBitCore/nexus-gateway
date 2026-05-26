# Program: Test Migration Sweep — Port unit tests from `nexus-gateway` to `nexus-gateway-refactor` to reach 95% coverage

**Owner session:** nexus (auto memory: parallel-session-aware)
**Created:** 2026-05-19
**Target repo:** `~/workspaces/workspace-nexus/nexus-gateway-refactor/` (current branch `develop`)
**Source repo (read-only):** `~/workspaces/workspace-nexus/nexus-gateway/`
**Binding rules consulted:** CLAUDE.md → Mandatory rules + Pre-edit reading + Mandatory Workflow.

---

## 0. Goal & Success Criteria

**Goal:** Raise unit-test statement coverage to ≥95% on every Go package under `packages/**` of the refactor repo that isn't a legitimate allowlist entry (categories A–F in `scripts/.coverage-allowlist`). Maximize reuse: every test we port from old saves us a from-scratch design pass.

**Success = all of:**
1. `npm run check:coverage` passes (i.e., `scripts/check-go-coverage.sh` exits 0) with the allowlist not relaxed (no new entries unless explicitly approved).
2. The "Open-source readiness backlog" section in the allowlist is reduced to empty or to an explicitly approved residual set.
3. Each test file we add asserts **named, observable business invariants** — not coverage-padding (CLAUDE.md "Unit test coverage ≥95%" → tests must assert observable business behavior and named failure modes).
4. `go test -race -count=1 ./...` is clean.
5. Per-phase ai-gateway smoke is run when the phase touches `packages/ai-gateway/**`, `packages/shared/transport/normalize/**`, `packages/shared/traffic/**`, `packages/shared/canonical*`, `TrafficEvent` schema, or any provider adapter (CLAUDE.md "AI Gateway / traffic_event changes require ai-gateway smoke" binding).

**Out of scope (this program):**
- Frontend / TypeScript / Vitest coverage.
- DB-bound, OS-bound, and network-infra-bound integration tests — these stay allowlisted unless we wire the corresponding CI infra, which is a separate program.
- Production code changes — this program writes tests only. If a test exposes a real bug, surface it; don't fold a fix into this program without explicit user approval (CLAUDE.md "Don't add features… beyond what the task requires").

---

## 1. Reference Materials (consult before any edits)

| Artifact | Path |
|---|---|
| Existing audit report (last sweep, 2026-05-16) | `/tmp/nexus-test-audit/audit-report-2026-05-16.md` |
| Last baseline coverage snapshot | `/tmp/nexus-test-audit/baseline-coverage.md` |
| Old-project test inventory (this session) | `/tmp/nexus-test-migration/old-project-test-inventory.md` |
| Structural delta (this session) | `/tmp/nexus-test-migration/structural-delta.md` |
| Coverage allowlist (authoritative exemptions + readiness backlog) | `packages/../scripts/.coverage-allowlist` |
| Coverage script | `scripts/check-go-coverage.sh` (supports `--staged`, `--strict-allowlist`) |
| Coverage policy doc | `.cursor/rules/unit-test-coverage-95.mdc` |

**Pre-edit reading per phase:** Each phase that touches code lists its required architecture/feature docs from `docs/dev/architecture-doc-triggers.md` and the matching `docs/features/` entries. We do NOT skip the 3-doc rule.

---

## 2. Constraints

- **Main session stays in `nexus-gateway-refactor/develop`.** Per CLAUDE.md "Parallel-session safety", no `git worktree` for the main session; no `git stash`; only explicit-pathspec `git add` / `git commit`.
- **Agent isolation worktrees ARE allowed** for subagents (`Agent(isolation: "worktree", …)`). This is the user-approved interpretation of "你在 worktree 去工作" — confirmed this session. Subagent worktrees are temp git worktrees the harness creates+cleans; they don't touch the main working tree.
- **Subagents default to Sonnet 4.6** (`model: "sonnet"`) — explicit user directive. Opus reserved for synthesis/decisions in main session.
- **English only** for all repository artifacts.
- **No production code edits** without explicit user approval per phase.
- **Per-phase subagent budget gate:** before launching a phase, main session estimates concurrent subagent count (typically 5–10) and asks user before fan-out.

---

## 3. Program Structure

### Phase 0 — Investigation (DONE this session)
Status: ✅ Complete.
Deliverables already on disk:
- Old-project inventory (1030 test files, ~13,416 Test functions, top-30 packages by test density).
- Structural delta (38 Category A / 8 B / 9 C / 6 E / 0 D across 61 refactor packages).
- This program plan document.

No further work in Phase 0.

### Phase 1 — Fresh Coverage Baseline + Gap Map
**Why:** The existing baseline is 3 days old; recent commits (refactor PR-0, configkey, reasoning-tokens) shift the picture. We need an authoritative current map before fan-out.

**Tasks (sequential):**
1. Run `npm run check:coverage` and `go test -cover -count=1 ./...` per-module from repo root; capture per-package % to `/tmp/nexus-test-migration/baseline-current.md`.
2. Diff baseline vs allowlist to produce the actionable gap list: `package | current % | gap to 95 | category from structural-delta | old-project test source if any`.
3. Group gap list by:
   - **Tier 1**: Category A/B packages with `current % < 95` AND old equivalent has tests AND not allowlisted. Fast wins.
   - **Tier 2**: Category E (semantic change) packages with old tests — porting needs adaptation but doable.
   - **Tier 3**: Category C (new, no old equivalent) packages — design tests fresh from SDD/OpenAPI.
   - **Tier 4**: Allowlist re-evaluation — entries that might be removable (run with `--strict-allowlist`).
4. Output: `/tmp/nexus-test-migration/gap-map.md` ranked by `gap × old-test-availability × business-criticality`.

**Subagent strategy:** 1 subagent (Sonnet) runs the coverage command per service module in parallel (5 modules: agent, ai-gateway, compliance-proxy, control-plane, nexus-hub; shared run separately) — guards against timeout on full-repo runs. Main session aggregates output.

**Gate before Phase 2:** User reviews gap-map.md and signs off on Tier ordering.

### Phase 2 — Shared Foundation (Tier 1 Category A/B in `packages/shared/**`)
**Why first:** `shared/` is consumed by all 4 services; rising its coverage unblocks any downstream that imports it. Highest portability (per structural delta: 31/45 shared packages are Category A; 2 are B). Old project shows 3,643 Test functions concentrated in shared — biggest test reservoir.

**Subagent fan-out plan:** 6–8 parallel Sonnet subagents, each owning one bucket:
- `shared/traffic/**` (87 old files, 1,728 tests)
- `shared/transport/normalize/**` (255 tests) — flags ai-gateway smoke per CLAUDE.md
- `shared/policy/hooks` + `shared/policy/payloadcapture` + `shared/policy/rulepack` (413 tests)
- `shared/compliance` + `shared/audit` (122 tests)
- `shared/identity/iam` + `shared/identity/pkce` + `shared/identity/rstokenauth` (rename from `security/`)
- `shared/core/**` (rename from `runtime/`)
- `shared/storage/configcache` + `shared/storage/cacheconfig` + `shared/storage/spillstore` + `shared/storage/spillupload`
- `shared/schemas/configtypes` + `shared/schemas/configkey` (new) + `shared/schemas/credstate` + `shared/schemas/domain` + `shared/schemas/thingtype`

**Subagent contract (each):**
- Read CLAUDE.md mandatory rules and the relevant architecture doc(s).
- For each package in the bucket: read current source + current `*_test.go`; read old project's matching file; produce a list of test functions to port + adapt.
- Run in an `Agent(isolation: "worktree")` so concurrent shared edits don't collide.
- Apply ports with import-path fixups (runtime→core, security→identity, httpclient→http, devicepredicate→device).
- Run `go test -race -count=1 -cover ./...` in the bucket; report new %, lingering gap, any production-code surprises.
- Do NOT add allowlist entries; do NOT change production code; flag if either looks needed.
- Output: a per-bucket markdown summary back to main session.

**Main-session merge:** Main session collects each worktree's diff via `git diff` from the worktree, applies them sequentially to develop with explicit pathspec `git add`/`git commit`, runs the full shared test suite, and verifies coverage.

**Gate before Phase 3:** Phase 2 closes when `packages/shared/**` is ≥95% (excluding allowlist entries) AND the per-phase 2-round self-audit is clean.

### Phase 3 — Service Layer Tier 1 (ai-gateway / nexus-hub / compliance-proxy / agent — Category A/B internal packages)
**Why next:** With shared at 95%, service-layer tests stop being blocked by missing shared assertions. Service `internal/` packages have high A/B portability rates per delta (ai-gateway 4 A / 1 B; nexus-hub 9 A; compliance-proxy 9 A; agent 5 A).

**Subagent fan-out plan:** 4 parallel Sonnet subagents (one per service), each operating in its own isolation worktree. Each subagent further fans out within its service if its bucket is large — but the **outer parallelism stays at 4** to keep test runs isolated per service module.

**Per-service buckets (from gap-map.md, indicative):**
- **ai-gateway**: `cache`, `config`, `credentials`, `execution`, `providers/**`, `runtimeapi` (cachelayer + store + handler partially DB-bound — likely allowlist).
- **nexus-hub**: `alerts`, `config`, `identity`, `jobs`, `jwks`, `observability`, `self`, `ws`. DB-bound stores stay allowlisted.
- **compliance-proxy**: `access`, `audit`, `compliance`, `config`, `exemption`, `health`, `metrics`, `proxy`, `siem`. TLS interception still allowlisted if cert-fixtures unavailable.
- **agent**: `compliance`, `host`, `network`, `observability`, `platform`, `sync`. Keystore/intercept OS-bound stay allowlisted.

**Special rule for this phase:** any subagent touching `packages/ai-gateway/**` or `packages/shared/transport/normalize/**` MUST flag the smoke-test requirement; main session schedules a single smoke at phase close, not per-subagent.

**Gate before Phase 4:** All 4 services' Tier-1 packages ≥95% OR explicitly entered into allowlist with user-approved rationale.

### Phase 4 — Category E Semantic-Change Adaptation
**Why fourth:** These need careful porting because signatures shifted (renames + semantic). Per delta the 6 Category E packages are:
1. `shared/core/` (rename of `runtime/` — partial E because old logging structure differs).
2. `shared/identity/` (rename of `security/` with possible IAM catalog drift).
3. `shared/identity/iam` specifically (catalog data might have changed entries).
4. `ai-gateway/internal/ingress` (renamed from old `handler` partial).
5. `ai-gateway/internal/routing` (renamed from `router` with restructure).
6. `control-plane/internal/handler` + `control-plane/internal/identity` (extreme restructure — deferred to Phase 6 actually; only the lighter `store` belongs here).

**Subagent fan-out plan:** 3 subagents in worktrees — one per area (shared, ai-gateway, control-plane-store). Each does deeper read-before-port because old test data structures don't drop in cleanly.

**Gate before Phase 5:** All Category E packages either ≥95% OR flagged as "needs production-code refactor to be testable" (escalate to user before changing prod code).

### Phase 5 — Category C New Packages (no old equivalent)
**Why fifth:** No tests to port; must design fresh from SDD/OpenAPI. Lower throughput per subagent so we defer until the easier work is in.

**Category C packages from delta (high-value targets):**
- `shared/policy/decision` + `shared/policy/pipeline`
- `shared/schemas/configkey` (already partially being added per current git status)
- `shared/storage/redisfactory`
- `ai-gateway/internal/auth` + `ai-gateway/internal/platform` + `ai-gateway/internal/policy`
- `compliance-proxy/internal/runtime`
- `nexus-hub/internal/compliance` + `nexus-hub/internal/quota` + `nexus-hub/internal/traffic` + `nexus-hub/internal/fleet`
- `agent/internal/identity` + `agent/internal/lifecycle` + `agent/internal/policy`

**Subagent contract (each):**
- Read the package's SDD/OpenAPI/Architecture docs (3-doc rule).
- Read source end-to-end (no skimming — per audit-2026-05-16 methodology).
- Identify invariants, write tests targeting **named failure modes**, not happy-path coverage padding.
- Run `go test -race -count=1 -cover`.

**Gate before Phase 6:** All Category C packages ≥95% OR explicitly allowlisted with user-approved rationale.

### Phase 6 — Control Plane Major Restructure
**Why last among code phases:** Control-plane internal is the heaviest lift (per delta: 7 Category C, 2 Category E, only 1 Category A; "porting effort: EXTREME"). Old auth-centric layout (`auth`/`authserver`/`jwtverifier`/`iam`/`audit`/`middleware`) → new domain layout (`identity`/`governance`/`ai`/`fleet`/`infrastructure`/`settings`/`observability`/`platform`/`traffic`). Old tests don't map 1:1; many need wholesale redesign.

**Plan:**
- Sub-phase 6a: trace endpoint migrations. Use `git log --follow -p -- <old handler path>` to find where each handler moved.
- Sub-phase 6b: domain-by-domain port (8 domains). Each as its own subagent in worktree. Heavy adaptation, expect ~90% rewrite rate.
- Sub-phase 6c: Wire IAM impact review per CLAUDE.md "API / menu / route changes require IAM impact review" — applies if porting a handler test exposes that the handler moved without IAM follow-up.

**Gate before Phase 7:** Control-plane internal ≥95% OR explicit allowlist entries.

### Phase 7 — Allowlist Sweep + Final Coverage Gate
- Run `scripts/check-go-coverage.sh --strict-allowlist` to find allowlist entries that have since reached threshold; remove those.
- Confirm `npm run check:coverage` is green from a clean checkout (no caches).
- Run full ai-gateway smoke (`tests/scripts/smoke-gateway.py --all-ingress`) once, since the program touched normalize/traffic/adapter areas.
- 2-round self-audit (CLAUDE.md mandatory).
- Promote `/tmp/nexus-test-migration/program-plan.md` to `docs/dev/_internal/test-coverage-95-program-plan.md` for posterity if user agrees (canonical handoff location per CLAUDE.md).

---

## 4. Subagent Strategy

**Default model:** Sonnet 4.6 per user directive.

**Default isolation:** Each phase-2-onward subagent runs in `Agent(isolation: "worktree")` so concurrent writes don't collide on shared files. The worktree is auto-cleaned if no changes; otherwise returns a branch + path that main session merges via explicit-pathspec git ops.

**Prompt skeleton (each subagent gets):**
1. Working-tree paths (refactor: their worktree; old: `~/workspaces/workspace-nexus/nexus-gateway/` read-only).
2. The exact bucket / package list they own.
3. The relevant section of structural-delta.md and old-project-test-inventory.md.
4. CLAUDE.md mandatory rules pertinent to their bucket (English-only, no production-code edits, no `git stash`, no `git add -A`, real-implementation-only).
5. The success criteria: per-package coverage ≥95%, all new tests assert observable behavior, `go test -race -count=1 -cover` clean.
6. Explicit "do NOT" list: no allowlist edits, no production-code changes, no `git commit`, no `git push`.
7. Output contract: a markdown summary + the worktree branch ref.

**Concurrency gates (per user directive):**
- Phase 2: 6–8 parallel.
- Phase 3: 4 parallel (one per service).
- Phase 4: 3 parallel.
- Phase 5: 4–6 parallel.
- Phase 6: 4 parallel (within control-plane domains).

Main session pauses for user OK before each fan-out.

---

## 5. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Subagent worktree drift — old branch as base, doesn't see latest develop changes | Phase entry pulls latest develop into all worktrees via `git fetch && git rebase origin/develop` once per phase start. |
| Two subagents both touch same `shared/` file (e.g., test helper) | Bucket boundaries respect package paths; subagents in worktrees can't see each other's writes; main-session merge applies sequentially with conflict surfacing. |
| Ported test asserts old-project-only behavior that quietly broke in refactor | Subagent contract requires "named failure mode" + reading current source first; tests-not-passing-on-current-code → surface as production bug to user, do NOT delete the assertion. |
| Smoke test required mid-program | Phase 2 + Phase 3 close with a single smoke each, not per-subagent. Phase 7 closes with the full smoke. |
| Coverage-padding sneaking in via volume porting | Spot-check 5 random ported test files per phase; reject ones that only assert `err == nil`. |
| Spec-Driven Workflow drift (Plan→Arch→Req→SDD→OpenAPI→Code→Tests) | Phase 5 (new tests for Category C) MUST consult SDD before writing — captured in subagent prompt. |
| `git worktree` lint trip on main session if I forget | Main session never runs `git worktree add/remove`; only Agent tool's isolation mode does that, and the harness cleans up. |

---

## 6. Decision points needing your input now

1. **Approve this program structure?** (Phase 0 done; Phase 1 ready to start.)
2. **Should I save the program plan under `docs/dev/_internal/` immediately, or keep in `/tmp/` until Phase 7?** (Recommendation: keep in `/tmp/` during execution; promote at Phase 7 close.)
3. **Fresh baseline run scope** — full repo, or split per module (faster, parallelizable)? (Recommendation: split per module via subagents.)
4. **Smoke-test cadence** — accept the plan's "Phase 2 close, Phase 3 close, Phase 7 close" cadence, or run earlier (more confidence, more time)? (Recommendation: as planned.)
5. **Production-code bugs found mid-program** — surface and pause for your decision, or auto-skip (test the current behavior, even if wrong)? (Recommendation: surface and pause.)

---

## 7. TaskList structure (mirrors Phases)

(See TaskCreate calls in main session — one task per phase, with subtasks for the gates.)
