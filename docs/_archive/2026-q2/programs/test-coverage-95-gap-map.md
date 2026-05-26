# Gap Map — Actionable Coverage Gaps by Tier

**Date:** 2026-05-19
**Branch:** feature/unit-test
**Source:** baseline-current.md + structural-delta.md + old-project-test-inventory.md + .coverage-allowlist

---

## Scoring Key

- **gap_to_95** = max(0, 95 − current%). Packages with 0% and no test files get gap=95.
- **old_test_availability**: high = package in old-project top-30 OR same-path match in old inventory; medium = renamed (B) or core/identity rename (E); low = Category C (new, no old equivalent).
- **business_criticality**: high = shared/, normalize/, traffic/, providers/, routing/, identity/, IAM, killswitch, audit; medium = handlers, jobs, store; low = cmd/wiring, infra.

---

## Tier 1 — Category A/B, non-allowlisted, <95%, has old tests (Fast Wins)

Sorted by `gap × criticality × old_test_availability` descending.

| Package | Module | Current % | Gap | Cat | Old-Test Source | Business Criticality | Phase 2/3 Bucket |
|---|---|---|---|---|---|---|---|
| `internal/ingress/models` | ai-gateway | 22.6% | 72.4 | E | ai-gateway/handler/proxy (268 tests, rank#2) | high | ai-gateway/ingress |
| `internal/routing/core` | ai-gateway | 29.6% | 65.4 | E | ai-gateway/router (159 tests, rank#10) | high | ai-gateway/routing |
| `internal/identity/store/authstore` | nexus-hub | 31.8% | 63.2 | A | nexus-hub (162 test files, stores) | high | nexus-hub/identity |
| `internal/ingress/envelope` | ai-gateway | 60.2% | 34.8 | E | ai-gateway/handler/proxy (268 tests, rank#2) | high | ai-gateway/ingress |
| `internal/ingress/debug` | ai-gateway | 74.1% | 20.9 | E | ai-gateway/handler/proxy (268 tests, rank#2) | medium | ai-gateway/ingress |
| `internal/providers/specs/anthropic/codec` | ai-gateway | 78.4% | 16.6 | A | ai-gateway/providers/spec_anthropic (111 tests, rank#21) | high | ai-gateway/providers |
| `internal/providers/core` | ai-gateway | 80.6% | 14.4 | A | ai-gateway/providers (general) | high | ai-gateway/providers |
| `internal/ingress/proxy/classify` | ai-gateway | 85.2% | 9.8 | E | ai-gateway/handler/proxy (268 tests, rank#2) | high | ai-gateway/ingress |
| `internal/routing/matcher` | ai-gateway | 93.1% | 1.9 | E | ai-gateway/router (159 tests, rank#10) | high | ai-gateway/routing |
| `internal/routing/strategies` | ai-gateway | 94.7% | 0.3 | E | ai-gateway/router (159 tests, rank#10) | high | ai-gateway/routing |
| `internal/ingress/proxy` | ai-gateway | 94.3% | 0.7 | E | ai-gateway/handler/proxy (268 tests, rank#2) | high | ai-gateway/ingress |
| `internal/fleet/shadow` | nexus-hub | 16.6% | 78.4 | C | nexus-hub/thingmgr (148 tests, rank#12) | high | nexus-hub/fleet |
| `internal/fleet/overrides` | nexus-hub | 34.3% | 60.7 | C | nexus-hub/thingmgr (148 tests, rank#12) | medium | nexus-hub/fleet |
| `cmd/ai-gateway/configdispatch` | ai-gateway | 22.7% | 72.3 | A | ai-gateway (general config) | low | ai-gateway/cmd |
| `cmd/compliance-proxy/configdispatch` | compliance-proxy | 20.4% | 74.6 | A | compliance-proxy (general) | low | compliance-proxy/cmd |
| `cmd/compliance-proxy/replay` | compliance-proxy | 16.0% | 79.0 | A | compliance-proxy (general) | low | compliance-proxy/cmd |
| `internal/proxy/connect` | compliance-proxy | 0.0% | 95.0 | A | compliance-proxy/proxy (old tests) | high | compliance-proxy/proxy |
| `internal/proxy/forward` | compliance-proxy | 0.0% | 95.0 | A | compliance-proxy/proxy (old tests) | high | compliance-proxy/proxy |
| `transport/normalize/codecs` | shared | 94.6% | 0.4 | A | shared/transport/normalize (255 tests, rank#3) | high | shared/transport |
| `internal/identity/handler/bootstrap` | nexus-hub | 0.0% | 95.0 | A | control-plane/handler/agent (179 tests, rank#8) | medium | nexus-hub/identity |
| `internal/identity/handler/enroll` | nexus-hub | 0.0% | 95.0 | A | nexus-hub/identity (old tests) | medium | nexus-hub/identity |
| `internal/observability/handler/diag` | nexus-hub | 0.0% | 95.0 | A | nexus-hub/observability (old tests) | medium | nexus-hub/observability |
| `internal/config/shadow` | compliance-proxy | 0.0% | 95.0 | A | compliance-proxy/config (old tests) | medium | compliance-proxy/config |
| `internal/tls/pinning` | compliance-proxy | 0.0% | 95.0 | C | compliance-proxy/cert (old cert tests) | medium | compliance-proxy/tls |
| `internal/tls/issuer` | compliance-proxy | 92.2% | 2.8 | C | compliance-proxy/cert (old cert tests) | high | compliance-proxy/tls |
| `internal/tls/kms` | compliance-proxy | 91.2% | 3.8 | C | compliance-proxy/cert (old cert tests) | high | compliance-proxy/tls |
| `internal/runtime/server` | compliance-proxy | 94.4% | 0.6 | C | compliance-proxy/runtimeapi (old tests) | medium | compliance-proxy/runtime |
| `internal/runtime/handler` | compliance-proxy | 38.2% | 56.8 | C | compliance-proxy/runtimeapi (old tests) | medium | compliance-proxy/runtime |
| `internal/execution/estimator` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/execution (old tests) | medium | ai-gateway/execution |
| `cmd/control-plane/configdispatch` | control-plane | 25.0% | 70.0 | A | control-plane (general config) | low | control-plane/cmd |
| `internal/handler` | control-plane | 25.9% | 69.1 | E | control-plane/handler (126 tests, rank#15) — allowlisted backlog | medium | control-plane/handler |
| `internal/infrastructure/infra` | control-plane | 90.6% | 4.4 | C | control-plane/handler/infra (173 tests, rank#9) | medium | control-plane/infrastructure |
| `internal/traffic/analytics/handler` | control-plane | 94.7% | 0.3 | C | control-plane/handler/analytics (125 tests, rank#17) — FAIL | high | control-plane/traffic |
| `internal/traffic/handler/traffic` | control-plane | 6.8% | 88.2 | C | control-plane/handler/traffic (109 tests, rank#25) | high | control-plane/traffic |
| `internal/fleet/handler/agent` | control-plane | 92.1% | 2.9 | C | control-plane/handler/agent (179 tests, rank#8) | high | control-plane/fleet |
| `internal/governance/hooks/handler` | control-plane | 72.8% | 22.2 | C | control-plane/handler/exemption (122 tests, rank#18) | high | control-plane/governance |
| `internal/ai/cache/handler` | control-plane | 65.9% | 29.1 | C | control-plane (old handler) | medium | control-plane/ai |
| `internal/ai/providers/handler` | control-plane | 87.2% | 7.8 | C | control-plane/handler/providers (154 tests, rank#11) | high | control-plane/ai |
| `internal/settings/handler/settings` | control-plane | 60.6% | 34.4 | C | control-plane/handler (general) | medium | control-plane/settings |
| `internal/providers/specs/anthropic/errors` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers/spec_anthropic (111 tests) | high | ai-gateway/providers |
| `internal/providers/specs/anthropic/ingress` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers/spec_anthropic (111 tests) | high | ai-gateway/providers |
| `internal/providers/specs/anthropic/stream` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers/spec_anthropic (111 tests) | high | ai-gateway/providers |
| `internal/providers/specs/gemini/codec` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers (gemini) | high | ai-gateway/providers |
| `internal/providers/specs/gemini/errors` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers (gemini) | high | ai-gateway/providers |
| `internal/providers/specs/gemini/ingress` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers (gemini) | high | ai-gateway/providers |
| `internal/providers/specs/gemini/stream` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers (gemini) | high | ai-gateway/providers |
| `internal/providers/specs/openai/codec` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers/spec_openai (99 tests, rank#27) | high | ai-gateway/providers |
| `internal/providers/specs/openai/errors` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers/spec_openai (99 tests) | high | ai-gateway/providers |
| `internal/providers/specs/openai/responses` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers/spec_openai (99 tests) | high | ai-gateway/providers |
| `internal/providers/specs/openai/rewrites` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers/spec_openai (99 tests) | high | ai-gateway/providers |
| `internal/providers/specs/openai/stream` | ai-gateway | 0.0% | 95.0 | A | ai-gateway/providers/spec_openai (99 tests) | high | ai-gateway/providers |
| `cmd/nexus-hub/wiring` | nexus-hub | 0.0% | 95.0 | A | N/A (wiring) | low | nexus-hub/cmd |
| `cmd/agent/platformshim` | agent | 0.0% | 95.0 | A | agent (platform shims) | low | agent/cmd |
| `cmd/agent/wiring` | agent | 0.0% | 95.0 | A | N/A (wiring) | low | agent/cmd |
| `cmd/compliance-proxy/breakglass` | compliance-proxy | 0.0% | 95.0 | A | compliance-proxy (breakglass) | medium | compliance-proxy/cmd |
| `cmd/compliance-proxy/wiring` | compliance-proxy | 0.0% | 95.0 | A | N/A (wiring) | low | compliance-proxy/cmd |
| `cmd/control-plane/wiring` | control-plane | 0.0% | 95.0 | A | N/A (wiring) | low | control-plane/cmd |

**Tier 1 total: 56 packages**

---

## Tier 2 — Category E (semantic change), has old tests, needs adaptation

Packages whose path survived but internal semantics changed significantly. Old tests exist but need adaptation beyond import fixup.

| Package | Module | Current % | Gap | Old-Test Source | Business Criticality | Phase 2/3 Bucket |
|---|---|---|---|---|---|---|
| `internal/ingress/models` | ai-gateway | 22.6% | 72.4 | ai-gateway/handler/proxy (268 tests) | high | ai-gateway/ingress |
| `internal/ingress/envelope` | ai-gateway | 60.2% | 34.8 | ai-gateway/handler/proxy (268 tests) | high | ai-gateway/ingress |
| `internal/ingress/debug` | ai-gateway | 74.1% | 20.9 | ai-gateway/handler/proxy (268 tests) | medium | ai-gateway/ingress |
| `internal/ingress/proxy/classify` | ai-gateway | 85.2% | 9.8 | ai-gateway/handler/proxy (268 tests) | high | ai-gateway/ingress |
| `internal/ingress/proxy` | ai-gateway | 94.3% | 0.7 | ai-gateway/handler/proxy (268 tests) | high | ai-gateway/ingress |
| `internal/routing/core` | ai-gateway | 29.6% | 65.4 | ai-gateway/router (159 tests) | high | ai-gateway/routing |
| `internal/routing/matcher` | ai-gateway | 93.1% | 1.9 | ai-gateway/router (159 tests) | high | ai-gateway/routing |
| `internal/routing/strategies` | ai-gateway | 94.7% | 0.3 | ai-gateway/router (159 tests) | high | ai-gateway/routing |
| `internal/routing` | ai-gateway | 98.4% | 0.0 | ai-gateway/router (159 tests) | high | ai-gateway/routing |
| `internal/handler` | control-plane | 25.9% | 69.1 | control-plane/handler (126 tests) — allowlisted | medium | control-plane/handler |
| `internal/handler` | nexus-hub | 0.0% | 95.0 | nexus-hub/handler (old tests) | medium | nexus-hub/handler |
| `identity/iam` | shared | 100.0% | 0.0 | shared/security/iam (IAM catalog) | high | shared/identity |

> Note: Several Tier 2 packages also appear in Tier 1 because category E shares characteristics with both. Packages already at ≥95% in Tier 2 (e.g., shared/identity/iam at 100%) are listed for completeness — they are DONE and require no work.

**Tier 2 actionable (gap > 0): 11 packages**

---

## Tier 3 — Category C (new, no old equivalent — design from SDD)

No direct old-project source. Tests must be designed from the SDD/OpenAPI spec or PR comments.

| Package | Module | Current % | Gap | Business Criticality | Phase Bucket |
|---|---|---|---|---|---|
| `internal/fleet/shadow` | nexus-hub | 16.6% | 78.4 | high | nexus-hub/fleet |
| `internal/fleet/overrides` | nexus-hub | 34.3% | 60.7 | medium | nexus-hub/fleet |
| `internal/fleet/smartgroup` | nexus-hub | 0.0% | 95.0 | medium | nexus-hub/fleet |
| `internal/fleet/handler/hubapi` | nexus-hub | 0.0% | 95.0 | medium | nexus-hub/fleet |
| `internal/traffic/ingest/audit` | nexus-hub | 0.0% | 95.0 | high | nexus-hub/traffic |
| `internal/traffic/ingest/spill` | nexus-hub | 0.0% | 95.0 | medium | nexus-hub/traffic |
| `internal/traffic/store` | nexus-hub | 0.0% | 95.0 | medium | nexus-hub/traffic (DB-bound) |
| `internal/identity/store/enrollstore` | nexus-hub | 0.0% | 95.0 | medium | nexus-hub/identity (DB-bound) |
| `internal/identity/store/userstore` | nexus-hub | 0.0% | 95.0 | medium | nexus-hub/identity (DB-bound) |
| `internal/runtime/handler` | compliance-proxy | 38.2% | 56.8 | medium | compliance-proxy/runtime |
| `internal/runtime/server` | compliance-proxy | 94.4% | 0.6 | medium | compliance-proxy/runtime |
| `internal/tls/pinning` | compliance-proxy | 0.0% | 95.0 | medium | compliance-proxy/tls |
| `internal/tls/issuer` | compliance-proxy | 92.2% | 2.8 | high | compliance-proxy/tls |
| `internal/tls/kms` | compliance-proxy | 91.2% | 3.8 | high | compliance-proxy/tls |
| `internal/ai/cache/handler` | control-plane | 65.9% | 29.1 | medium | control-plane/ai |
| `internal/ai/providers/handler` | control-plane | 87.2% | 7.8 | high | control-plane/ai |
| `internal/ai/quota/handler` | control-plane | 0.0% | 95.0 | high | control-plane/ai |
| `internal/ai/simulator/handler` | control-plane | 0.0% | 95.0 | medium | control-plane/ai |
| `internal/fleet/handler/agent` | control-plane | 92.1% | 2.9 | high | control-plane/fleet |
| `internal/governance/dsar/handler` | control-plane | 0.0% | 95.0 | medium | control-plane/governance |
| `internal/governance/hooks/handler` | control-plane | 72.8% | 22.2 | high | control-plane/governance |
| `internal/governance/interception/handler` | control-plane | 0.0% | 95.0 | medium | control-plane/governance |
| `internal/governance/rulepacks/handler` | control-plane | 0.0% | 95.0 | medium | control-plane/governance |
| `internal/infrastructure/infra` | control-plane | 90.6% | 4.4 | medium | control-plane/infrastructure |
| `internal/observability/opsmetrics/handler` | control-plane | 0.0% | 95.0 | medium | control-plane/observability |
| `internal/observability/retention/handler` | control-plane | 0.0% | 95.0 | medium | control-plane/observability |
| `internal/observability/siem/handler` | control-plane | 0.0% | 95.0 | medium | control-plane/observability |
| `internal/observability/thingstats/handler` | control-plane | 0.0% | 95.0 | medium | control-plane/observability |
| `internal/settings/handler/settings` | control-plane | 60.6% | 34.4 | medium | control-plane/settings |
| `internal/traffic/analytics/handler` | control-plane | 94.7% | 0.3 | high | control-plane/traffic (FAIL — fix first) |
| `internal/traffic/handler/traffic` | control-plane | 6.8% | 88.2 | high | control-plane/traffic |
| `internal/identity/scim/handler` | control-plane | 0.0% | 95.0 | medium | control-plane/identity |
| `internal/identity/sessions/handler` | control-plane | 0.0% | 95.0 | medium | control-plane/identity |
| `internal/identity/sso/handler` | control-plane | 0.0% | 95.0 | high | control-plane/identity |
| `internal/identity/users/handler` | control-plane | 0.0% | 95.0 | high | control-plane/identity |
| `internal/platform/audit` | control-plane | 100.0% | 0.0 | high | DONE |
| `policy/decision` | shared | 0.0% | 95.0 | high | shared/policy |
| `policy/pipeline` | shared | 99.2% | 0.0 | high | DONE |
| `schemas/configkey` | shared | 100.0% | 0.0 | medium | DONE |
| `storage/redisfactory` | shared | 99.3% | 0.0 | medium | DONE |

**Tier 3 actionable (gap > 0, excluding DONE): 35 packages**

---

## Tier 4 — Allowlist Re-evaluation Candidates

Packages currently in the allowlist where the baseline shows coverage ≥95%. These MAY be removable from the allowlist if user approves — must verify the allowlist category still applies.

> **Authority reminder:** Removing an allowlist entry requires explicit user approval per CLAUDE.md.

| Package | Module | Current % | Allowlist Category | Removable? | Notes |
|---|---|---|---|---|---|
| `internal/observability/audit/backfill` | agent | 91.7% | backlog | no | Still below 95%; remove only after gap closed |
| `internal/identity/clockoffset` | agent | 89.5% | backlog | no | Still below 95% |
| `internal/jobs/defs/audit` | nexus-hub | 91.7% | backlog | no | Still below 95% |
| `internal/jobs/defs/expiry` | nexus-hub | 94.6% | backlog | no | Still below 95% (0.4pp gap) |
| `internal/jobs/defs/health` | nexus-hub | 79.9% | backlog | no | Still below 95% |
| `internal/jobs/defs/metrics` | nexus-hub | 43.1% | backlog | no | Still below 95% |
| `internal/jobs/defs/quota` | nexus-hub | 57.2% | backlog | no | Still below 95% |
| `internal/jobs/defs/retention` | nexus-hub | 74.9% | backlog | no | Still below 95% |
| `internal/jobs/defs/rollup` | nexus-hub | 45.2% | backlog | no | Still below 95% |
| `internal/self/reg` | nexus-hub | 93.0% | backlog | no | Still below 95% |
| `internal/storage/store` | nexus-hub | 51.9% | backlog | no | Still below 95% |
| `internal/compliance/catbagent` | nexus-hub | 88.4% | backlog | no | Still below 95% |
| `core/metrics/platform` | shared | 94.4% | backlog | no | 0.6pp gap remains |
| `transport/normalize/codecs` | shared | 94.6% | not allowlisted | n/a | Only 0.4pp gap — quick fix in Phase 2 |
| `internal/handler` | control-plane | 25.9% | backlog (glob match) | no | Far below 95% |

> No Tier 4 packages are currently at ≥95% AND still in the allowlist, so no allowlist entries can be mechanically removed today. All backlog entries still need work. Once the corresponding gap is closed in Phase 2/3, the entry can be nominated for removal with user approval.

**Tier 4 entries reviewed: 15 — removable today: 0**

---

## Pre-existing FAIL List

All three must be fixed before their respective tiers can close. Fix authority is user-approved-only (this program writes tests only; test-logic bugs in existing tests count as test edits and are in scope, but production-code bugs require explicit user approval).

| Package | Module | Error Summary | Tier | Fix Notes |
|---|---|---|---|---|
| `internal/proxy/server` | compliance-proxy | 7x `ServeHTTP did not return` (15s timeout); 1x `status=403 want 407`. Coverage 94.5% — already below threshold. | Tier 1 (Category A) | Likely mock ResponseWriter not closed, causing goroutine hang. The 403/407 mismatch is a test assertion bug. Both are test-only fixes. Must fix before compliance-proxy Tier 1 can close. |
| `internal/traffic/analytics/handler` | control-plane | `status=500 want 200; query failed` (preceded by `conn lost` warning). Coverage 94.7%. | Tier 3 (Category C) | Mock analytics store returning error on dimension-label lookup path. Test-only fix (update stub to return correct result). Must fix before control-plane/traffic bucket closes. |
| `internal/policy/quota` | ai-gateway | `weekly TTL too large: 306h19m36.630168s`. Coverage 99.7% (no coverage gap). | Tier 1 (Category E sub-pkg) | Time-sensitive test: TTL calculation bug at week-boundary. Test-only fix (mock `time.Now` or compute expected correctly). Must fix before ai-gateway Tier 1 is fully clean. |

---

## Notes for Phase 2 Fan-out

The 8 shared buckets from program-plan §3 are confirmed by this baseline:

| Bucket | Packages | Gap Packages | Baseline State |
|---|---|---|---|
| 1. `shared/traffic/**` | ~25 packages | 0 actionable gaps | All at ≥95% or types-only. No work needed. |
| 2. `shared/transport/normalize/**` | 4 packages | 1 (`codecs` at 94.6%) | 0.4pp gap in codecs — tiny fix. Triggers ai-gateway smoke per CLAUDE.md. |
| 3. `shared/policy/hooks` + `payloadcapture` + `rulepack` | ~9 packages | 0 actionable gaps | All at ≥95%. No work needed. |
| 4. `shared/compliance` + `shared/audit` | 2 packages | 0 actionable gaps | Both at ≥95%. No work needed. |
| 5. `shared/identity/iam` + `pkce` + `rstokenauth` | 3 packages | 0 actionable gaps | All at 100%. No work needed. |
| 6. `shared/core/**` | 7 packages | 1 (`core/metrics/platform` at 94.4% — allowlisted backlog) | 0.6pp gap; allowlisted. Nominate for fix + allowlist removal with user approval. |
| 7. `shared/storage/**` | 7 packages | 0 actionable non-allowlisted gaps | `spillstore/s3` allowlisted (E-network). All others ≥95%. |
| 8. `shared/schemas/**` + `shared/policy/decision` | ~9 packages | 1 (`policy/decision` — no tests, allowlisted C) | Need fresh test design for `policy/decision`; all schemas OK. |

**Phase 2 is largely DONE for shared.** The only shared work remaining: (a) close the 0.4pp gap in `transport/normalize/codecs`, (b) close 0.6pp in `core/metrics/platform` + nominate allowlist removal, (c) design tests for `policy/decision`. All other shared buckets are green.

**Phase 3 bulk is in service-layer:** ai-gateway ingress + routing + provider sub-packages are the highest-value Tier 1 bucket. nexus-hub fleet + jobs/defs are the highest-value allowlist-backlog bucket. Control-plane has the most packages but they are largely Category C (new) — deferred to Phase 6 (program-plan §3 Phase 3/High-Risk).
