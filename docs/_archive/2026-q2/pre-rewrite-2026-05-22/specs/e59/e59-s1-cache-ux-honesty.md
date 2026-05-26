# E59-S1 — Cache UX Honesty (Vocabulary + State Model)

> Story: e59-s1
> Epic: 59 (Cache UX Honesty)
> Status: Draft (expanded 2026-05-19 — state model added)
> Requirements: `docs/developers/specs/e59/e59-cache-savings-ux-honesty.md`
> Architecture: `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` § 6 (Cache savings UI contract + § 6.4 unified state model); `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md` § 11 (gateway response cache outcome states); `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md` § 7.5 (provider_cache_status rollup field)
> OpenAPI: `docs/users/api/openapi/admin/e59-s1-cache-ui-fields.yaml`
> Blocked by: none
> Relationship to E58: independent; can run in parallel with E58 implementation. Touches some same UI files but different regions.

## User Story

As a Platform Admin reading the Cache ROI dashboard, I want to see only the cache concepts that actually exist in our code (Gateway response cache + Provider prompt cache discount), with each shown with its real unit (cost-avoided vs token-discount), so I don't have to explain to my team what "L2 semantic cache" is doing — because there is no L2 semantic cache; it was an aspirational label that never shipped.

**Second persona (state-model expansion).** As a Platform Admin filtering Live Traffic by cache outcome, I want a single Yes/No "did caching save me money on this request" filter — not 6 internal gateway-cache enum values that conflate "did we try?" with "what happened?" — so I can answer "what's my cache hit rate?" with one query and drill into the gateway-vs-provider breakdown only when I'm debugging an individual event.

## Tasks

### T1 — Audit current L-vocabulary references (one-time, before edits)

- T1.1 Generate the full grep of L1/L2/L3/L4 references across the codebase:
    - `grep -rn "L1\|L2\|L3\|L4" packages/control-plane-ui/src/ --include="*.tsx" --include="*.ts" --include="*.json"`
    - `grep -rn "L1\|L2\|L3\|L4" packages/control-plane/internal/ --include="*.go"`
    - `grep -rn "L4Cache\|GatewayCacheSavings\|l4_cache_hit\|requests_with_l4" packages/ --include="*.go"`
    - `grep -rn "L1\|L2\|L3\|L4" docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md`
- T1.2 For each reference, categorize:
    - **Rename**: refers to a real concept under a misleading label (e.g., "L1–L3 Gateway Cache" → "Gateway Cache").
    - **Delete**: refers to fictional concept (e.g., "L2 Semantic" tooltip text).
    - **Out of scope**: refers to unrelated tier system (e.g., IAM cache L1/L2 — different domain, keep as-is).
- T1.3 Produce a "rename map" table in this SDD's appendix; reviewer uses it as the diff checklist.

### T2 — Backend API field renames

- T2.1 `packages/control-plane/internal/handler/admin_cost_summary.go`:
    - `TotalL4CacheNetSavingsUSD float64 \`json:"totalL4CacheNetSavingsUsd"\`` → `TotalProviderPromptCacheNetSavingsUSD float64 \`json:"totalProviderPromptCacheNetSavingsUsd"\``
    - Field initialization sites updated accordingly.
- T2.2 `packages/control-plane/internal/handler/admin_cache_roi.go`:
    - Per-day and per-adapter breakdown fields named with `L4` get the analogous rename.
    - The struct comment `// L1/L2/L3 platform response cache savings` becomes `// Gateway response cache savings (full upstream cost avoided)`.
    - Similar comment edits for `requestsWithCacheHit` (`// L4 provider cache read tokens` → `// Provider prompt cache read tokens`).
- T2.3 `packages/control-plane/internal/handler/admin_analytics_rollup.go`:
    - `GatewayCacheSavingsUsd` stays (already correct).
    - Add `ProviderPromptCacheNetSavingsUsd` if not present in the rollup output.
- T2.4 Unit tests for the renamed JSON keys; coverage ≥95 %.

### T3 — Prometheus metric renames

- T3.1 `packages/ai-gateway/internal/observability/metrics/metrics.go`:
    - `MetricRequestsWithL4CacheHit` → `MetricRequestsWithProviderPromptCacheHit` (Go identifier).
    - The promauto-registered name `nexus_aigw_requests_with_l4_cache_hit_total` → `nexus_aigw_requests_with_provider_prompt_cache_hit_total`.
- T3.2 `MetricGatewayCacheSavingsUSD` stays.
- T3.3 New metric `MetricGatewayCacheLookupTotal{outcome="hit\|miss"}` (FR-3.4) — for direct hit-rate computation.
- T3.4 Audit any internal Grafana panel or `tools/db-migrate/seed/data/seed-baseline.sql` `AlertRule` row referencing the old metric name. If references exist, ship a one-release alias (T3.5); if none, the rename is clean.
- T3.5 (Conditional on T3.4) One-release alias: register both the old and new metric names with the same underlying counter. Schedule removal of the old name for the release after deploy. Document in `docs/operators/ops/runbooks/cache-metric-rename.md`.

### T4 — CP-UI i18n key renames + deletions

- T4.1 `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json` — apply the rename map from T1.3. Specifically:
    - `cacheSavingsDesc` "(L1–L4)" suffix removed.
    - `combinedSavingsSub` "L1/L2/L3 response cache + L4 provider cache" → "Gateway Cache + Provider Prompt Cache". (Or, since the two are unit-incompatible, delete this string entirely — see T5.5.)
    - `gatewaySavingsSub` "L1/L2/L3 full response cache — no provider call" → "Full response cache — upstream call avoided".
    - `gatewayCacheHitsSub` "requests served from L1/L2/L3 cache" → "requests served from gateway cache".
    - `cacheHits` "L4 Cache Hits" → "Provider Prompt Cache Hits".
    - `tooltipGatewayLayers` — **deleted entirely** (was "L1 Exact-match · L2 Semantic · L3 Compressed response cache"; L2 and L3 don't exist).
    - `tooltipProviderLayer` — content kept but "L4" prefix removed. New text: "Provider prompt cache (e.g. Anthropic cache_control). The request still reaches the provider, but previously-seen prompt tokens are billed at a reduced rate. Savings = (standard rate − cache rate) × cached tokens."
    - `tooltipCombinedSavings` — **deleted entirely** (the combined number was misleading; we no longer display it).
    - `colGroupL1L3` → `colGroupGatewayCache`. Value "L1–L3 Gateway Cache" → "Gateway Cache".
    - `colGroupL4` → `colGroupProviderPromptCache`. Value "L4 Provider Prompt Cache" → "Provider Prompt Cache".
- T4.2 New i18n key `gatewayCacheTooltip` (en/zh/es): "Gateway response cache stores normalized request → response. Identical subsequent requests are served without calling the upstream — full upstream cost avoided."
- T4.3 Verify all 3 locales are kept in sync (CI lint `npm run check:i18n`).
- T4.4 The `normaliserEnabledHint` key references "L3 prompt-cache normaliser pipeline" — this is the **prompt cache normalization** subsystem (E38), a different L3 from the cache tier story. Keep this key unchanged but rename to drop "L3" prefix: "Master switch for the prompt-cache normaliser pipeline. When off, no cache_control markers are injected and no strip rules run upstream."

### T5 — CP-UI Cache ROI Dashboard

- T5.1 `packages/control-plane-ui/src/pages/analytics/CacheROIDashboard.tsx`:
    - Remove the "L1–L4" badge from the three top-level stat cards.
    - The "L1–L3 Gateway Cache" section header → "Gateway Cache".
    - The "L4 Provider Prompt Cache" section header → "Provider Prompt Cache".
    - The column groupings on the per-adapter table use the new i18n keys.
- T5.2 Remove the "combined savings" top stat. Display the two stats side-by-side instead:
    - "Gateway Cache savings — $X — Y requests bypassed upstream entirely"
    - "Provider Prompt Cache savings — $Z — N input tokens served at discount"
- T5.3 The `LayerTag` component (currently rendering "L1–L4" / "L1–L3" / "L4" badges) is either deleted or repurposed to render the two real concepts as distinct labels. Decision in implementation.
- T5.4 Vitest snapshot tests of the dashboard with fixture data confirming the new layout.
- T5.5 Visual review by a non-engineer (T9) confirms the page is understandable without prior context.

### T6 — CP-UI Traffic Audit Drawer

- T6.1 `packages/control-plane-ui/src/pages/traffic/trafficAuditDrawer.tsx`:
    - The "Gateway cache HIT" banner (currently triggered when L1/L2/L3 served the request) text becomes "Served from Gateway Cache".
    - Comment `{/* Gateway cache HIT banner — shown when L1/L2/L3 served this request */}` updated.
    - Add a collapsed "Details" disclosure showing the cache key fingerprint + TTL — useful for debugging, no tier labels.
- T6.2 Vitest test asserting the banner renders for cache-hit fixtures.

### T7 — CP-UI Overview widget

- T7.1 `packages/control-plane-ui/src/pages/<wherever Overview lives>`:
    - The cache savings stat that currently uses `cacheSavingsDesc` "(L1–L4)" — change to either two stats (preferred) or one with a clarified label ("Gateway Cache savings" only — the more impactful number for most users).
    - Decision in implementation; document in PR.

### T8 — Service-layer comment cleanup

- T8.1 `packages/control-plane-ui/src/api/services/analytics.ts`:
    - Comment `// L1/L2/L3 platform response cache` → `// Gateway response cache (full upstream cost avoided)`.
    - Comment `// L4 provider-side prompt cache` → `// Provider prompt cache discount on input tokens`.
- T8.2 `packages/control-plane-ui/src/api/services/iam.ts`:
    - The `L2 TTL` reference (line 91) is in the IAM cache context — different concept. Leave unchanged.

### T9 — Architecture doc cleanup

- T9.1 `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md`:
    - Audit current content for L2 / L3 references.
    - The doc's main body describes only caches that exist in code: gateway response cache (the one Redis-backed exact-match), config caches (`cachelayer/`, `configcache/`), stream coalescer (`streamcache/`), Gemini cachedContent integration (`geminicache/`), and the various small in-process LRUs (cert cache, IAM cache).
    - Move any L2 / L3 description into a clearly-marked "## Roadmap (not implemented)" appendix, or delete entirely.
    - Add a cross-pointer to `cost-estimation-architecture.md` § 6 (UI contract).
- T9.2 The `architecture-doc-triggers.md` row for cache code (`docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md`) stays — the doc still covers all cache code, post-cleanup.

### T10 — Non-engineer review

- T10.1 After T1-T9 land in a draft branch, walk a non-engineer through the Cache ROI page → click into a traffic event → expand cache details → return to the dashboard. Confirm no questions like "what's L2?" arise.
- T10.2 Capture verbatim feedback in the PR description.

### T11 — traffic_event schema migration (Prisma)

- T11.1 Edit `tools/db-migrate/schema.prisma` `model TrafficEvent` (~line 1231):
    - Redefine `cache_status` semantics to `HIT | MISS` (string, nullable). No CHECK constraint at schema level (enforced by audit-writer); a follow-up could harden via Postgres CHECK.
    - Add `gateway_cache_status String? @map("gateway_cache_status")`.
    - Add `gateway_cache_skip_reason String? @map("gateway_cache_skip_reason")`.
    - Add `gateway_cache_kind String? @map("gateway_cache_kind")`.
    - Add `provider_cache_status String? @map("provider_cache_status")`.
    - Add `@@index([cache_status])` for filter performance.
- T11.2 Generate Prisma migration via `cd tools/db-migrate && npx prisma migrate dev --name e59_unified_cache_status`.
- T11.3 Old `cache_status` values (`HIT_LIVE`, `DISABLED`, `SKIP_NO_CACHE`, `PASSTHROUGH_SKIP`) on existing dev rows are dropped per FR-6.4 — the migration includes `UPDATE traffic_event SET cache_status = NULL WHERE cache_status NOT IN ('HIT', 'MISS')` (or wipe table via `TRUNCATE traffic_event` if rows are dev-only). Decision in implementation; document in PR.
- T11.4 Re-run Prisma → Go codegen if the generated bindings consume `traffic_event` rows.

### T12 — audit.go enum refactor + DeriveCacheStatus

- T12.1 Replace `packages/ai-gateway/internal/platform/audit/audit.go:131-136` (current single `CacheStatus` enum) with five Go types:
    ```go
    type GatewayCacheStatus string  // "hit" | "hit_inflight" | "miss" | "skipped"
    type GatewayCacheSkipReason string  // "disabled" | "no_cache" | "passthrough" | "not_cacheable"
    type GatewayCacheKind string  // "extract" | "semantic"
    type ProviderCacheStatus string  // "hit" | "miss" | "na"
    type CacheStatus string  // "HIT" | "MISS" (unified)
    ```
- T12.2 Add `func DeriveCacheStatus(gw GatewayCacheStatus, pv ProviderCacheStatus) (CacheStatus, error)` — returns `("", err)` for invalid combos (gateway `hit`/`hit_inflight` paired with provider `hit`/`miss`).
- T12.3 Table-driven unit tests cover all 8 valid combinations + the 4 invalid combinations + the 2 empty-string boundary cases. Black-box test file.
- T12.4 Per-package Go coverage gate via `scripts/check-go-coverage.sh` on `packages/ai-gateway/internal/platform/audit/`; ≥95% statement coverage with assertions on outputs (not coverage-padding).

### T13 — ai-gateway write-path: stamp all four internal fields + unified

- T13.1 Cache decision points in `packages/ai-gateway/internal/handler/proxy_cache.go` stamp `gateway_cache_status` + `gateway_cache_skip_reason` (when skipped) + `gateway_cache_kind` (`"extract"` on hits — `"semantic"` slot is reserved schema-only and is not written by any code today).
- T13.2 Provider response usage parsing (in `packages/ai-gateway/internal/handler/proxy.go` for non-streaming + the SSE final-usage handler) stamps `provider_cache_status`: `"hit"` when `cache_read_tokens > 0`; `"miss"` when provider was called, model supports prompt-cache (per a small allow-list keyed on adapter_type), and `cache_read_tokens` is 0; `"na"` otherwise.
- T13.3 Audit writer (the site that builds the `TrafficEvent` struct for the audit sink) calls `audit.DeriveCacheStatus(gw, pv)` once both internal statuses are known; stamps the unified `cache_status` on `traffic_event`.
- T13.4 Invalid combo handling: log a structured warning and stamp `cache_status = NULL`. No panic. The 4 internal fields still record what the code observed for debuggability.
- T13.5 Prometheus emission:
    - Existing `MetricGatewayCacheLookupTotal{outcome="hit"|"miss"}` (FR-3.4) continues to count gateway-side lookups. `skipped` is **not** emitted (lookups that didn't happen aren't "lookups").
    - New `MetricProviderPromptCacheLookupTotal{outcome="hit"|"miss"|"na"}` (FR-11.1). Emitted when provider was called (so `na` here means "called, but model doesn't support prompt cache" — the gateway-served `na` is excluded since no provider call happened).
- T13.6 Per-package Go coverage gate on `packages/ai-gateway/internal/handler/`; ≥95% on the changed paths (existing coverage allowlist not touched).

### T14 — Admin API + CP UI: filter + drawer redesign

- T14.1 Admin API query handler for traffic-events (locate via grep: the handler that consumes `cacheStatus` query param). Tighten validation: accept only `HIT | MISS` (empty allowed). Unit test asserts 400 on `HIT_LIVE` / `DISABLED` / other values.
- T14.2 `packages/control-plane-ui/src/pages/traffic/filters/liveTrafficFilters.ts:11-17` — reduce `LiveTrafficCacheStatus` type to `'' | 'HIT' | 'MISS'`. Update `defaultLiveTrafficFilters`, paramSetter, and summary helpers (line 162, 211, 262).
- T14.3 `packages/control-plane-ui/src/pages/traffic/filters/LiveTrafficAdvancedFilters.tsx:65-70` — 6 `<option>` elements reduce to 2 (Any kept as empty value).
- T14.4 `packages/control-plane-ui/src/pages/traffic/audit-drawer/trafficAuditDrawer.tsx`:
    - Remove the raw `cache_status` enum line at L899-903.
    - Add a new Cache block with three layouts per the rendering rules in `cost-estimation-architecture.md § 6.4`. Layout selected by inspecting `gateway_cache_status` + `provider_cache_status`.
    - Update the Gateway cache HIT banner trigger logic at L807: read unified `cacheStatus === 'HIT'` (drop the `|| 'HIT_LIVE'` branch — `HIT_LIVE` no longer exists). Banner amount: `gatewayCacheSavingsUsd` first; fallback to `cacheNetSavingsUsd`. Banner copy uses the existing `cacheSavedBanner` / `cacheHitBanner` keys (no rename) — they're already vocabulary-honest.
- T14.5 i18n key churn in `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json` + `public/locales/` mirror:
    - **Delete**: `pages:traffic.cacheStatus.HIT_LIVE`, `cacheStatus.DISABLED`, `cacheStatus.SKIP_NO_CACHE`, `cacheStatus.PASSTHROUGH_SKIP` in all three locales.
    - **Add**: `pages:traffic.detail.cache.layoutGatewayServed`, `layoutProviderDiscount`, `layoutNoSavings` (drawer headline + sentence templates); `pages:traffic.detail.cache.gatewayStateHit`, `gatewayStateHitInflight`, `gatewayStateMiss`, `gatewayStateSkipped`, `gatewayStateSkippedDisabled`, `gatewayStateSkippedNoCache`, `gatewayStateSkippedPassthrough`, `gatewayStateSkippedNotCacheable`; `pages:traffic.detail.cache.providerStateHit`, `providerStateMiss`, `providerStateNa`, `providerStateNaGatewayServed`, `providerStateNaUnsupported`. (Or pick a tighter key set in implementation — the exact list is in the i18n PR review.)
    - All three locales kept in sync. `npm run check:i18n` passes.
- T14.6 Vitest snapshot updates for `trafficAuditDrawer.tsx` (3 layout fixtures) + `LiveTrafficAdvancedFilters.tsx` (3-option dropdown).
- T14.7 Run `npm run check:design-tokens`, `npm run check:i18n`, `npm run check:workspace-replace` — all pass.

### T15 — Smoke validation

- T15.1 Build ai-gateway from the branch; start local stack via `./scripts/dev-start.sh` + per-service `go run`.
- T15.2 Run `tests/scripts/smoke-gateway.py --all-ingress` with the standing test VK (memory `project_local_test_vk`). Per CLAUDE.md ai-gateway smoke rule, this is non-waivable for changes to `packages/ai-gateway/**`, traffic_event schema, normalize, codecs.
- T15.3 For every observed (gateway_cache_status, provider_cache_status) pair across the 29-model × 4-ingress matrix, assert: the 4 new fields are populated (not NULL where they should be set), the unified `cache_status` matches the `DeriveCacheStatus` rule, and the cache HIT banner fires on rows where unified=`HIT` (manual spot-check in CP UI drawer).
- T15.4 Smoke report's "cache classification" diff section MUST show no regressions vs the pre-change baseline. Cache-hit threshold rule (cache hit rate ≥ 50% on the 2-turn repeats) MUST still pass.
- T15.5 Save the smoke markdown report path (`/tmp/smoke-gateway-<UTC-timestamp>.md`) and link it in the PR description.

## Acceptance Criteria

| ID | Acceptance |
|---|---|
| AC-1 | `grep -rn "L1\|L2\|L3\|L4" packages/control-plane-ui/src/` returns hits only in legitimately-unrelated contexts (IAM cache `L2 TTL`, possibly others identified in T1.3); no false positives in cache / ROI / analytics text. |
| AC-2 | The Cache ROI Dashboard renders two distinct cache savings sections, no "L1–L4" combined badge. |
| AC-3 | The Traffic Audit Drawer shows "Served from Gateway Cache" with collapsed Details disclosure for cache-hit rows. |
| AC-4 | The `tooltipGatewayLayers` i18n key is removed in all 3 locales; `npm run check:i18n` passes. |
| AC-5 | The `cache-multi-tier-architecture.md` doc describes only caches that exist in code (or fenced under a Roadmap appendix). |
| AC-6 | Backend JSON field `totalL4CacheNetSavingsUsd` is renamed to `totalProviderPromptCacheNetSavingsUsd`; CP-UI consumes the new name; no aliasing left in code. |
| AC-7 | Prometheus metric rename is applied; either no aliasing (clean cutover) or a one-release alias documented in the runbook (T3.5). |
| AC-8 | A new metric `MetricGatewayCacheLookupTotal{outcome}` is emitted; hit-rate is computable directly from Prometheus. |
| AC-9 | Non-engineer walkthrough (T10) produces no "what is L1/L2/..." questions; feedback captured in PR description. |
| AC-10 | Per-package coverage gates from CLAUDE.md hold for the touched packages. |
| AC-11 | `traffic_event` schema contains 4 new columns (`gateway_cache_status`, `gateway_cache_skip_reason`, `gateway_cache_kind`, `provider_cache_status`); `cache_status` accepted values restricted to `HIT|MISS`; `@@index([cache_status])` exists; Prisma migration applied; old enum values purged per T11.3. |
| AC-12 | `audit.DeriveCacheStatus` table-driven unit test passes for all 8 valid (gateway, provider) combinations and rejects all 4 invalid combinations. Coverage ≥95% on `packages/ai-gateway/internal/platform/audit/`. |
| AC-13 | Live Traffic filter dropdown shows exactly 3 options (Any/HIT/MISS); admin API `cacheStatus` query param rejects non-`HIT`/`MISS` values with HTTP 400. |
| AC-14 | Traffic Audit Drawer renders the new 3-layout Cache block (Gateway-served / Provider-discount / No savings) for representative fixtures; raw enum values (`HIT_LIVE`, `SKIP_NO_CACHE`, etc.) are never visible. Cache HIT banner fires on unified-HIT rows; never on MISS. |
| AC-15 | `tests/scripts/smoke-gateway.py --all-ingress` passes with: every observed (gateway, provider) combo writing all 4 new fields correctly; unified `cache_status` matching the §6.4 derivation table for every row; no regression in cache-hit threshold rule; smoke markdown report linked in PR. |

## Testing strategy

- **Unit (handler)**: JSON-key rename verified by struct-tag tests and round-trip JSON marshal/unmarshal tests.
- **Unit (Vitest)**: Snapshot tests on Dashboard + Audit Drawer; i18n tests on the new keys + removed keys.
- **Integration**: A real cache HIT walkthrough — send 2 identical requests, second one hits cache; verify dashboard reflects in real time and the Audit Drawer shows the right banner.
- **Manual UX review**: T10 non-engineer walkthrough.
- **Unit (audit)**: `DeriveCacheStatus` table-driven over 8 valid + 4 invalid combos.
- **Unit (ai-gateway write-path)**: per-package coverage gate; integration-test fixtures cover gateway HIT, gateway HIT_INFLIGHT (via singleflight coalescer), gateway MISS + provider HIT, gateway MISS + provider MISS, gateway SKIPPED + provider any.
- **Unit (Vitest, drawer)**: 3 fixtures for the 3 layouts (Gateway-served / Provider-discount / No savings); 1 fixture for skip-reason rendering.
- **Integration (smoke)**: `smoke-gateway.py --all-ingress` MUST pass before "done". Covered by T15. Non-waivable per CLAUDE.md.

## Rollback plan

- Each file rename is a small commit; per-file revert is trivial.
- The backend JSON-key rename is the only piece that has UI-vs-API ordering risk. Mitigate by landing UI and API renames in the same PR; if a deploy hits a UI-loaded-but-API-unmigrated state, the UI gracefully shows the cache stats as `null` (numeric fields fall back to "—"). Verify with a brief deploy-window check that the API + UI are in sync.
- Prometheus metric rename: if `T3.5` alias is in place, the rename is observation-only on the old name for one release. Otherwise, internal dashboards need updating before deploy — verified by T3.4 grep.
- **Schema migration (T11)**: dev-phase rollback is `npx prisma migrate reset` followed by `migrate dev` to re-apply prior state. No prod migration backout path needed (pre-GA).
- **audit.go enum split (T12)**: per-file revert is trivial. All call sites that updated to write the 3 internal statuses + DeriveCacheStatus are within the same PR.
- **UI filter dropdown (T14.2 + T14.3)**: deploy ordering risk — if a CP UI deployment lands while the backend still emits old enum values, the filter never matches (every row's `cache_status` is `HIT_LIVE` not `HIT`). Mitigate by shipping schema migration + ai-gateway write-path FIRST, then UI. Verified in dev by running smoke before deploying UI changes.

## Appendix — L-vocabulary rename map (template for T1.3)

```
| File | Line | Current | New | Category |
|---|---|---|---|---|
| packages/control-plane-ui/src/i18n/locales/en/pages.json | 65 | "cacheSavingsDesc": "...over the past 30 days (L1–L4)." | "cacheSavingsDesc": "...over the past 30 days." | Delete suffix |
| packages/control-plane-ui/src/i18n/locales/en/pages.json | 2682 | "tooltipGatewayLayers": "L1 Exact-match · L2 Semantic · L3 ..." | (key deleted) | Delete fiction |
| packages/control-plane-ui/src/i18n/locales/en/pages.json | 2689 | "colGroupL1L3": "L1–L3 Gateway Cache" | "colGroupGatewayCache": "Gateway Cache" | Rename |
| packages/control-plane-ui/src/pages/analytics/CacheROIDashboard.tsx | 219, 230, 244 | <LayerTag label="L1–L4" ... /> | (component removed or label changed) | Rename/delete |
| packages/control-plane-ui/src/pages/traffic/trafficAuditDrawer.tsx | 666 | {/* Gateway cache HIT banner — shown when L1/L2/L3 served this request */} | {/* Gateway cache HIT banner */} | Comment update |
| packages/control-plane/internal/handler/admin_cost_summary.go | 19 | TotalL4CacheNetSavingsUSD float64 `json:"totalL4CacheNetSavingsUsd"` | TotalProviderPromptCacheNetSavingsUSD float64 `json:"totalProviderPromptCacheNetSavingsUsd"` | Rename API field |
| packages/ai-gateway/internal/observability/metrics/metrics.go | (line) | MetricRequestsWithL4CacheHit | MetricRequestsWithProviderPromptCacheHit | Rename metric |
| ...                                                          | ... | ...                                       | ...                                                      | ...      |
```
