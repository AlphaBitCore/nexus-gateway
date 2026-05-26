# E58-S5 — Kill `nexus.dry_run` flag, unify on `POST /v1/estimate`

**Status:** Design — pending implementation
**Date:** 2026-05-23
**Author:** brainstorm — nexus + Claude
**Supersedes:** E58-S3 (`nexus.dry_run` flag) — that subsystem is deleted in entirety by this story.
**Related, untouched:** E58-S2 (estimator core), E58-S4 (`/v1/estimate` compare endpoint).

---

## 1. Goal

Make `POST /v1/estimate` the **only** cost-preview surface in the AI Gateway.
Delete the `nexus.dry_run` request-extension flag and every code path it
enabled, so that the real-call proxy (`/v1/chat/completions`,
`/v1/messages`, `/v1/responses`, `/v1/generateContent`) carries **zero**
estimate-related logic.

Architectural principle (binding for this story): **`/v1/estimate` is an
independent endpoint.** It does not read from, write to, or share runtime
state with any real-call subsystem (routing engine, hook pipeline, response
cache, traffic_event audit, quota counters). The only shared primitives are
VK auth and the Model registry — both are gateway-wide infrastructure, not
"real-call capabilities".

## 2. Why

### 2.1 Problem with E58-S3 (`nexus.dry_run` flag)

`nexus.dry_run: true` riding on a normal proxy request created a hybrid mode
that entangled the real-call path with estimation:

1. **Mixed cost semantics in `traffic_event`.** Dry-run rows carry
   `estimated_cost_usd` (heuristic forecast) alongside `embedding_cost_usd`
   and `ai_guard_cost_usd` (actual money spent on hooks). The admin UI
   detail drawer presents both in one Costs breakdown panel without
   distinguishing forecast from actual.
2. **Rollup blind spot.** All rollup SQL filters `WHERE is_dry_run = false`,
   so the real embedding/ai-guard spend incurred during dry-run is excluded
   from every fleet-wide cost aggregate.
3. **API-shape compromise.** Full estimate breakdown stuffed into the
   `x-nexus-estimate` JSON-in-header value, with `assumptions` truncated to
   5 entries to stay under nginx's 8 KB header limit. SDKs that auto-parse
   the body cannot reach this data without opt-in raw-header access.
4. **Mode entanglement in `proxy.go`.** Dry-run dispatch lives in three
   places (cache-HIT / cache-MISS / cache-DISABLED branches); has a
   dedicated rate-limit bucket; bypasses quota; flips
   `pipeline.SetAllowModify(!isDryRun)` to downgrade MODIFY hooks. Every
   future change to the proxy has to reason about both "real" and "dry-run"
   semantics.

### 2.2 Why `/v1/estimate` alone is sufficient

User intent for dry-run (confirmed in brainstorm 2026-05-23):

| Scenario | Frequency | Solved by `/v1/estimate`? |
|---|---|---|
| (B) Client-side cost preview before sending a real request | Primary | **Yes.** Caller posts the same body to `/v1/estimate` with the intended target. |
| (A) Admin tries a new routing rule before flipping it on | Secondary | **Yes, with one extra step.** Admin reads the routing rule via admin API, enumerates candidate models, calls `/v1/estimate` with explicit `compareTargets`. |
| (C) Hook pre-verification — "will my PII scanner block this?" | Secondary | **Not solved, and intentionally punted.** Client tests by calling the real chat API. |

C is the only capability lost. Brainstorm decision: **punt**. If a real
customer asks for a "would my hooks block this without sending upstream"
capability later, we design a clean separate endpoint (e.g.
`/v1/policy/preview`) for it then. We do not pre-engineer.

## 3. Scope

### 3.1 In scope (this story)

| # | Area | Change |
|---|---|---|
| C1 | AI Gateway code | Delete `nexus.dry_run` flag, `dry_run.go`, all `isDryRun` branches in `proxy.go`, dry-run rate limit, `canonicalext.IsDryRun`. |
| C2 | `/v1/estimate` | No functional change. Keep exactly as E58-S4 shipped. Caller must specify `compareTargets` explicitly. |
| C3 | DB schema | Drop `TrafficEvent.is_dry_run`, `TrafficEvent.dry_run_assumptions`. **KEEP `TrafficEvent.estimated_cost_usd`** — the column name is historical/misleading; this is the canonical total-cost column populated for ALL traffic_event rows (real-call + previously-dry-run), and live cost-stamping code writes it on every real call. Drop `VirtualKey.dryRunRateLimitRpm`. Write Prisma migration. |
| C4 | Rollup / analytics SQL | Remove all `WHERE is_dry_run = false` filters (9 sites). |
| C5 | Audit / traffic store | Remove `IsDryRun` / `DryRunAssumptions` fields from `audit.Record` and `trafficstore.TrafficEvent`. **KEEP `EstimatedCostUsd`** everywhere — canonical cost field, written by every real call (see C3). Remove `is_dry_run = true/false` filter option from list queries. |
| C6 | Control Plane UI | Remove dry-run rendering in `trafficAuditDrawer.tsx`. Remove dry-run filter from traffic list (if present). Remove related i18n keys (en/es/zh-CN). |
| C7 | Tests | Sweep all dry-run unit tests, scenario tests, Python smoke arms, UI Vitest cases. |
| C8 | Shared types | Remove `IsDryRun` from `packages/shared/transport/mq/messages.go` (AuditUpload payload). Remove dry-run constants from `packages/shared/traffic/markers.go`. |
| C9 | VK config | Remove `DryRunRateLimitRpm` field from `vkauth.VKMeta` and related admin API surface. |

### 3.2 Explicitly out of scope

- **Hook preview endpoint** (replacement for C scenario). Punted.
- **Estimator accuracy telemetry** (e.g. real-call double-write of estimator
  output for "estimated vs actual" comparison). Rejected: violates
  independence principle by entangling estimator into real-call path.
  Future need can be addressed by an offline sampling job that replays
  historical traffic_event bodies through `/v1/estimate`.
- **`/v1/estimate` enhancements** — including "empty `compareTargets` →
  routing engine resolution". Rejected: routing engine is one of
  ai-gateway's real-call capabilities; entangling it into `/v1/estimate`
  violates the independence principle.
- **Deprecation period for `nexus.dry_run`.** Per CLAUDE.md
  "Development-phase policy: no backward compatibility, no defer" — pre-GA,
  no installed users; hard delete in one PR.

## 4. Detailed design

### 4.1 AI Gateway — code deletions

#### 4.1.1 Delete entire files

- `packages/ai-gateway/internal/ingress/proxy/dry_run.go`

#### 4.1.2 Edits to `packages/ai-gateway/internal/ingress/proxy/proxy.go`

Remove:

- `isDryRun := canonicalext.IsDryRun(body)` (line 445) and the second
  refinement at line 942.
- Dry-run rate-limit branch (lines 498–510): collapse to a single
  `h.checkRateLimit` call.
- Quota-bypass branch (line 695): `quotaInPrice, quotaOutPrice, quotaDecision = h.checkQuota(...)` becomes unconditional.
- Hook pipeline's `SetAllowModify(!isDryRun)` (line 1775): becomes
  `SetAllowModify(true)`.
- Both `dryRunDispatch` invocations (lines 967–969 and 1047–1058).
- Helper `dryRunDispatch`, `writeDryRunResponse`, `writeDryRunBody`,
  `encodeDryRunBody`, `writeDryRunStream`, `dryRunUsage`,
  `buildEstimateHeaderJSON`, `costToMap`, `nowStamp`,
  `dryRunRateLimitDefault`, `checkDryRunRateLimit` — all moved out with
  `dry_run.go`.
- `runRequestHooks` signature: drop the `isDryRun bool` parameter
  (line 1718).

#### 4.1.3 Edits to `packages/shared/canonical*` (canonicalext)

- Remove `nexus.dry_run` from the canonical extension namespace.
- Remove `canonicalext.IsDryRun` function and any related parsing.

#### 4.1.4 VK auth surface

- `packages/ai-gateway/internal/auth/vkauth`: drop `DryRunRateLimitRpm`
  from `VKMeta` struct. Remove its DB read in the VK loader.
- Control Plane admin VK CRUD: remove `dryRunRateLimitRpm` field from
  request/response shapes and SQL.
- DB: drop `VirtualKey.dryRunRateLimitRpm` column (Prisma migration).

### 4.2 `/v1/estimate` — unchanged

The endpoint at `packages/ai-gateway/internal/ingress/proxy/estimate.go`
ships as-is. No new options, no routing-engine integration. Behavior contract:

- Auth: VK auth (shared primitive).
- Rate limit: per-VK `compareEndpointRateLimitRpm` bucket (already
  separate from real-call bucket; rename to `estimateRateLimitRpm` is **not**
  done in this story to keep diff focused — that's a cosmetic follow-up).
- Required input: `compareTargets[]` with at least one explicit
  `(providerId, modelId)` pair, optional `reasoningEffort`.
- Output: per-target `cost{low,expected,high}` + `tokens` + `reasoning` +
  `assumptions[]`; top-level `summary` with cheapest/most-expensive.
- **No** `traffic_event` write. **No** hook pipeline. **No** routing
  engine call. **No** cache touch.

### 4.3 DB schema

`tools/db-migrate/schema.prisma` — `TrafficEvent` model:

```diff
 model TrafficEvent {
   ...
-  is_dry_run            Boolean   @default(false)
-  dry_run_assumptions   Json?
   estimated_cost_usd    Decimal?  @db.Decimal(12, 6)   // KEEP — canonical total-cost
   ...
 }
```

`tools/db-migrate/schema.prisma` — `VirtualKey` model:

```diff
 model VirtualKey {
   ...
-  dryRunRateLimitRpm    Int?
   ...
 }
```

Migration: `tools/db-migrate/migrations/YYYYMMDDHHMMSS_drop_dryrun_columns/migration.sql`:

```sql
-- DROP only dry-run-specific columns. estimated_cost_usd is the canonical
-- total-cost field and stays.
ALTER TABLE "TrafficEvent"
  DROP COLUMN "is_dry_run",
  DROP COLUMN "dry_run_assumptions";

ALTER TABLE "VirtualKey"
  DROP COLUMN "dryRunRateLimitRpm";
```

Note: per CLAUDE.md memory `feedback_migration_timestamp_unique`,
generate the timestamp prefix uniquely (no collision with other in-flight
migrations).

### 4.4 Rollup / analytics SQL

Drop the `WHERE is_dry_run = false` filter from every callsite found:

| File | Line(s) |
|---|---|
| `packages/control-plane/internal/traffic/analytics/handler/analytics_rollup.go` | 262, 371 |
| `packages/control-plane/internal/traffic/analytics/handler/analytics_latency.go` | 141 |
| `packages/control-plane/internal/traffic/analytics/handler/cost_summary.go` | 138, 151, 189, 228 |
| `packages/control-plane/internal/traffic/analytics/handler/cache_roi.go` | 244, 355, 406 |

Plus the explicit `is_dry_run = true/false` filter clause in
`packages/control-plane/internal/traffic/store/trafficstore/traffic_event.go:632–634`.

### 4.5 Audit / traffic store

`packages/ai-gateway/internal/platform/audit/audit.go`:

- Drop fields `IsDryRun` (line 586) and `DryRunAssumptions` (line 590)
  from the `Record` struct.
- Drop the two corresponding writer-side assignments at lines 1303,
  1304 in the record → message mapper.
- **KEEP `EstimatedCostUsd` (line 664) + its mapper at line 1323** —
  this is the canonical cost field, written by every real call (see
  scope row C3). The field name is historical; the data lives on every
  traffic_event row including the post-S5 real-call rows.

`packages/control-plane/internal/traffic/store/trafficstore/traffic_event.go`:

- Drop `IsDryRun` and `DryRunAssumptions` from `TrafficEvent` struct (lines
  ~76, 78).
- Drop `a.is_dry_run, a.dry_run_assumptions` from SELECT columns (line 277).
- Drop `&a.IsDryRun, &a.DryRunAssumptions` from row scanners (lines 373, 428).
- Drop `a.is_dry_run` filter clauses (lines 632–634).

`packages/shared/transport/mq/messages.go`:

- Drop `IsDryRun bool` from the AuditUpload payload (line 32–33).

`packages/shared/traffic/markers.go` + `markers_test.go`:

- Drop `x-nexus-estimate` / `x-nexus-dry-run` marker constants and their
  tests.

### 4.6 Control Plane UI

`packages/control-plane-ui/src/pages/traffic/audit-drawer/trafficAuditDrawer.tsx`:

There are **two** surfaces in this file that reference dry-run state. Both
must go:

**Surface A — the dedicated DRY RUN block** (lines 701–823 approx):
- Guard `{e.isDryRun && (...)}` at line 701.
- "DRY RUN — estimate only" panel title, description, estimate breakdown
  table (Estimated cost / Prompt tokens / Output tokens rows), "How was
  this estimated?" explainer with the input split / output decomp /
  formula sub-sections, and the `dryRunAssumptions` list at lines 805–815.
- Delete the entire `{e.isDryRun && (...)}` JSX block in one cut.

**Surface B — the AiProvider section's Estimated-cost line item** (line 1099):
- `{ label: t('pages:traffic.detail.aiProvider.estimatedCost'), value: e.estimatedCostUsd != null ? fmtCost(e.estimatedCostUsd) : null }`
- Delete this row.

**Surface C — the Costs breakdown panel** (lines 1109–1350):
- The early `const primary = e.estimatedCostUsd ?? 0;` (line 1123) — change
  to read from real cost fields only (the panel already reads `cost_usd` etc
  for real rows; verify nothing else still depends on `estimatedCostUsd`).
- The per-component math at lines 1148–1160 continues to work for real
  rows unchanged.

**i18n key deletion** (all three locales — en / es / zh-CN):
- Every key under `pages:traffic.detail.dryRun.*`
- `pages:traffic.detail.aiProvider.estimatedCost`

`packages/control-plane-ui/src/pages/traffic/filters/LiveTrafficBasicFilters.tsx`:

- Lines 311–313: the `dryRunMode` select with options `real / include /
  only`.
- Comment block at line 274 explaining the filter rationale.
- Delete the entire `dryRunMode` filter UI and any related state /
  query-param wiring.

`packages/control-plane-ui/src/pages/traffic/list/TrafficTab.tsx`:

- Lines 135–137: the `{r.isDryRun && ( <Badge variant="warning">DRY RUN
  </Badge> )}` JSX. Delete.

**i18n keys to delete** — verified via grep against all three locale
bundles (en / es / zh). Scope is **cost-preview only**; do NOT touch the
unrelated `dryRun*` keys for rule-pack testing (those live under hook
authoring pages, not traffic):

| Key path | Used by |
|---|---|
| `pages:traffic.detail.dryRun.*` (whole sub-tree at ~line 1730) | Audit drawer Surface A |
| `pages:traffic.detail.aiProvider.estimatedCost` | Audit drawer Surface B |
| `pages:traffic.dryRunExact` / `dryRunEst` / `dryRunExactTip` / `dryRunEstTip` (~lines 2246–2249) | Traffic list cost column tooltip |
| `pages:traffic.tipDryRun` (~line 2252) | Traffic page filter help text |
| `pages:traffic.dryRunMode.*` (~line 2253) | The filter select being deleted above |

**Keep, unrelated**:
- `pages:hooks.dryRunTitle / dryRunSubtitle / dryRunContent / dryRunPlaceholder
  / dryRunRun / dryRunEmpty` (~lines 796–801) — these belong to the **rule
  pack testing** feature (admin pastes sample text and runs it through a
  rule pack), nothing to do with cost preview. **Do not touch.**

Verification: `npm run check:i18n` after deletion — orphan keys in es/zh
must be cleared too (per `feedback_i18n_defaultvalue_options_mix` memory).

### 4.7 Configuration

Verified by grep: there are **no** `dryRun*` / `DRY_RUN_*` entries in
`.env.example`, `tests/.env.*`, or any YAML under `packages/ai-gateway/config/`.
Section retained as a one-line sanity check during implementation.

### 4.7.1 DO-NOT-TOUCH — unrelated `dryRun` usage

The following uses of "dryRun" in the codebase are **not** related to cost
preview and **must be left alone**:

- `packages/shared/transport/wirerewrite/config.go:103` — `DryRunAlways *bool`
- `packages/shared/transport/wirerewrite/engine.go:121,128,137` —
  `DryRunAlways` plumbing
- `packages/shared/storage/cacheconfig/types.go:72` — `DryRunAlways *bool`
- i18n keys `pages:hooks.dryRun*` (~lines 796–801 of `zh/pages.json`) — rule
  pack testing feature

These are wire-rewrite-rule and rule-pack simulation features under
completely separate epics; the name collision is incidental. The sweep grep
in §4.9 will surface them — implementer must filter them out by hand.

### 4.8 Tests sweep

Per user binding: "记得要修改 相关的 python test 脚本、unit test、e2e test 等".
This is non-waivable for landing.

#### 4.8.1 Go unit tests

| File | Action |
|---|---|
| `packages/ai-gateway/internal/ingress/proxy/coverage_extra_test.go` | Remove tests asserting `x-nexus-estimate` header / dry-run dispatch shape. |
| `packages/ai-gateway/internal/ingress/proxy/coverage_gaps_test.go`, `coverage_boost_test.go`, `proxy_residuals_test.go`, `proxy_cache.go` tests, `proxy_cost_test.go` | Grep for `IsDryRun`, `isDryRun`, `dryRun`, `dry_run` and remove related cases. |
| `packages/ai-gateway/internal/ingress/proxy/estimate_test.go` | Unchanged — keeps coverage on `/v1/estimate`. |
| `packages/shared/traffic/markers_test.go` | Remove dry-run header-name tests. |
| `packages/shared/transport/mq/*_test.go` | Remove `IsDryRun` payload assertions. |
| Any other Go test file matched by `grep -rln "IsDryRun\|isDryRun\|dryRun\|dry_run" packages/ --include="*_test.go"` | Sweep individually. |

Coverage gate: per CLAUDE.md "Unit test coverage ≥95%" binding, the package
coverage gate must still be green after deletion. Removing lines can only
**raise** statement coverage (the deleted lines are no longer in the
denominator), so this is structurally safe — but verify with
`scripts/check-go-coverage.sh` post-delete.

#### 4.8.2 L5 scenario tests

| File | Action |
|---|---|
| `tests/scenarios/dry_run_estimate_test.go` | Delete entirely OR rewrite into `tests/scenarios/estimate_endpoint_test.go` covering: single-target estimate; multi-target compare; VK `allowedModels` per-target enforcement; rate-limit bucket; estimator assumptions surfacing; reasoningEffort variations (low/medium/high). |
| Any other scenario file referencing `nexus.dry_run` | Delete the case. |

Per CLAUDE.md "L5 scenario landing rule" binding: live-run output
(`go test -run ^Test<NNN>$ -count=1 -v`) showing PASS or SKIP **must**
appear in the PR description for any new/edited scenario. Compile-clean is
not sufficient.

#### 4.8.3 Python smoke

`tests/scripts/smoke-gateway.py` — concrete references confirmed by grep:

**Delete:**
- Comment at line 18 mentioning "Cache and dry-run arms explicitly skipped".
- Header-capture comment at lines 859–860 referencing `x-nexus-dry-run` /
  `x-nexus-routed-model` — keep `x-nexus-routed-model` capture if still
  used, drop `x-nexus-dry-run`.
- Cross-check `WHERE te.is_dry_run = false` clause in the SQL at line 1316
  (and the comment at lines 1285–1288 explaining the filter rationale —
  the rationale itself becomes moot).
- Entire dry-run smoke arm: the `_DRY_RUN_EXTRA` constant at line 1795, the
  `call_dry_run_non_stream` / `call_dry_run_stream` callables at lines
  1698–1699, the `_chat_call_dry_run` wrapper at line 1798 + its sibling
  wrappers for the other ingresses, and the comment block at lines
  1695–1793 ("Per-ingress dry-run callable wrappers (E58-S3 smoke arm D)").
- The "Used by the dry-run smoke arm to recompute expected cost" helper at
  line 1197 — verify it's only used by dry-run (likely yes — delete if so).

**Keep, unchanged:**
- The `/v1/estimate` callable at line 1083 (`def estimate(self, body, ...)`)
  — `/v1/estimate` is the post-S5 sole cost-preview surface.

**Per CLAUDE.md "AI Gateway / traffic_event changes require ai-gateway
smoke run before done" binding** — this story touches traffic_event
schema, so a full smoke (`tests/scripts/smoke-gateway.py --all-ingress`)
is required pre-merge. Document the run output in the PR.

#### 4.8.4 UI Vitest

`packages/control-plane-ui/src/pages/traffic/audit-drawer/*.test.tsx`:

- Remove dry-run row test cases.
- Verify Costs breakdown panel still renders correctly for real-call rows
  (the panel logic is unchanged for non-dry-run; just verify nothing was
  inadvertently coupled to `isDryRun` checks).

`packages/control-plane-ui/src/pages/traffic/list/*.test.tsx`:

- Remove dry-run filter test cases.

### 4.9 Sweep checklist (greps to run + expect 0 hits post-merge)

```bash
# Code paths (production) — expect 0 EXCEPT wirerewrite/cacheconfig DryRunAlways (§4.7.1)
git grep -E "IsDryRun|isDryRun|dry_run|dryRun" -- 'packages/**/*.go' ':!packages/**/*_test.go'
git grep -E "x-nexus-estimate|X-Nexus-Estimate" -- packages/

# Tests — expect 0
git grep -E "IsDryRun|isDryRun|dry_run|dryRun" -- 'packages/**/*_test.go' 'tests/'

# UI — expect 0 EXCEPT hooks.dryRun* rule-pack testing keys (§4.7.1)
git grep -E "isDryRun|dryRun|estimatedCost|x-nexus-estimate" -- packages/control-plane-ui/

# i18n — expect 0 EXCEPT hooks.dryRun* (§4.7.1)
git grep -E "dryRun|estimatedCost" -- packages/control-plane-ui/src/i18n/

# Docs (current tree only — _archive/ is frozen and exempt) — expect 0
git grep -E "dry_run|dryRun|x-nexus-estimate" -- docs/developers/ docs/users/ docs/operators/
```

Any remaining hit blocks landing.

## 5. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Existing customer is using `nexus.dry_run` flag against this gateway | Per CLAUDE.md "no installed users" — pre-GA, this risk is accepted. No deprecation period. Communicate via release notes only. |
| Migration drops production data unintentionally | Dev-phase; `is_dry_run` rows in any environment are not load-bearing for billing/SLO. No backup required. |
| Test coverage drops below 95% on `packages/ai-gateway/internal/ingress/proxy/` after deletion | Deletion can only raise coverage. Verify with `scripts/check-go-coverage.sh` post-delete; no new code paths added in this story. |
| L5 scenario rewrite reveals `/v1/estimate` gap not anticipated | If found, **add the case to this spec** (don't silently work around). Re-review with user. |
| ai-gateway smoke regression due to proxy.go cleanup | Mandatory `tests/scripts/smoke-gateway.py --all-ingress` run is part of completion checklist (CLAUDE.md binding). |

## 6. Acceptance criteria

A landing PR satisfies all of:

1. Every `git grep` from §4.9 returns zero hits.
2. `go test -race -count=1 ./packages/...` passes.
3. `scripts/check-go-coverage.sh` reports no regression (allowlist
   unchanged or shrunk).
4. `npm run check:i18n` and `npm run check:terminology` pass.
5. `npm run check:doc-lockstep` passes (this spec lives at
   `docs/developers/specs/`; no other doc trigger paths are touched in
   this PR — verify via the lockstep map).
6. `tests/scripts/smoke-gateway.py --all-ingress` passes against local
   stack with all rolled-up cost columns producing non-zero values for
   real calls and zero for estimate calls (since `/v1/estimate` writes no
   traffic_event).
7. New / rewritten L5 scenarios show live PASS output in the PR description.
8. Manual UI sanity: open Traffic Event detail drawer for a recent real-call
   row, confirm Costs breakdown renders with no dry-run-specific fields
   referenced.

## 7. Non-goals reaffirmed

This story does **not**:

- Add a `/v1/policy/preview` endpoint or any hook-preview capability.
- Add estimator accuracy telemetry.
- Refactor `/v1/estimate` shape, rate-limit naming, or routing
  integration.
- Touch the estimator core (E58-S2) or cache pricing (E58-S1).
- Modify provider adapters or the canonicalbridge.
- Add UI for `/v1/estimate` (it's API-only for now).

## 8. Open questions

None at design time. Implementation may surface follow-ups (e.g. unexpected
test couplings); those get captured as new todos and brought back to the
user before landing.

## 9. Memory anchors

- `[[project_e58_responses_api_done_prod]]` — E58 series context.
- `[[feedback_cache_mandatory_all_ingress]]` — smoke requirement binding.
- `[[feedback_migration_timestamp_unique]]` — migration prefix uniqueness.
- `[[feedback_directory_size_decomp]]` — `proxy.go` is already over the
  threshold; deletion in this story reduces it slightly. Decomp itself is
  out of scope but the trend is healthy.
