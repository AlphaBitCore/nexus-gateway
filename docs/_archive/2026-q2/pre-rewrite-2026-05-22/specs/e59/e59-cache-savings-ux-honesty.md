# E59 — Cache UX Honesty (Vocabulary + State Model)

> Epic: 59
> Status: Draft
> Date: 2026-05-19 (state-model expansion)
> Architecture impact: `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` § 6 (Cache savings UI contract + unified state model § 6.4); `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md` § 11 (gateway response cache outcome states); `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md` § 7.5 (provider_cache_status rollup field)
> SDD: `docs/developers/specs/e59/e59-s1-cache-ux-honesty.md`
> OpenAPI: `docs/users/api/openapi/admin/e59-s1-cache-ui-fields.yaml`
> Relationship to E58: **independent epic, runs in parallel**. No blocking dependency in either direction. E58-S1's reasoning-display work touches some of the same UI files but a different region; merge conflicts manageable.

---

## 1. Background

The Cache ROI dashboard, the Overview cache-savings widget, the Traffic Audit Drawer, the admin API field naming, the Prometheus metric naming, and the canonical architecture doc all describe a four-layer cache hierarchy:

- **L1** — Exact-match cache (in-process / Redis response cache).
- **L2** — Semantic cache (embedding-based fuzzy match).
- **L3** — Compressed response cache (some unspecified compression scheme).
- **L4** — Provider prompt cache (Anthropic ephemeral, OpenAI auto, Gemini cachedContent).

A code-level audit performed on 2026-05-16 confirmed:

- **L1 exists** — there is exactly one response cache, in `packages/ai-gateway/internal/cache/` and called via the broker pattern in `packages/ai-gateway/internal/handler/proxy_cache.go`. Exact-match on `normalizeKey(canonical_request)`; Redis-backed.
- **L2 does not exist.** No `semantic`, `embedding`, `vector`, `qdrant`, `pinecone`, `milvus`, `hnswlib`, or `faiss` references anywhere in the codebase that implement a cache lookup. The "semantic cache" was a planned feature that was never implemented.
- **L3 does not exist.** No `compressed` cache implementation. No git history of a commit implementing one. The label appears only in i18n tooltip strings and the (planned) `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md` content.
- **L4 exists** — the upstream-provider prompt cache observed via `cache_read_input_tokens` / `cached_tokens` in the response usage. Nexus does not "own" this cache; we observe and report its effects.

The UI therefore shows users (and ops) a four-layer hierarchy of which **half is fiction**. End-users have repeatedly asked "what is L2 doing for me?" and admin staff have no answer because the cache is not real.

A secondary problem: even the real concepts (L1 + L4) are mixed under a single "L1–L4 combined savings" total in the Overview widget. The two have different units — L1 saves the **entire** upstream cost; L4 saves a **fraction** (the discount on cached input tokens). Adding them together produces a number with no clear meaning. Customers cannot use the combined figure for anything actionable.

This epic strips the UI of the fictional layers and clarifies the two real concepts.

**Update (2026-05-19, state-model expansion).** A separate UX review of the Live Traffic filter dropdown surfaced a second class of dishonesty: the filter exposed 6 internal gateway-cache enum values (HIT / HIT_LIVE / MISS / DISABLED / SKIP_NO_CACHE / PASSTHROUGH_SKIP) directly to end-users, conflating "did we try to cache?" with "what happened?" and ignoring the provider prompt cache outcome entirely. Brainstorm output: users care about one thing — **did caching save me money on this request, yes or no?** — and the gateway-vs-provider breakdown is drill-down detail, not filter UX. This epic expands to redefine `cache_status` as a 2-value unified rollup (`HIT | MISS`), add four detail columns on `traffic_event` for drill-down (gateway / provider status, gateway skip reason, gateway kind — with a schema-only `semantic` slot for future work), and redesign the audit drawer to render one of three human layouts. The vocabulary cleanup and the state-model redesign are bundled because they share the same UI surfaces, the same i18n keys, and the same `traffic_event` schema — splitting them would require two migrations of the same table and two passes through the same files.

---

## 2. Functional Requirements

### FR-1: Remove all L1/L2/L3/L4 vocabulary from user-visible surfaces

| ID | Requirement | Priority |
|---|---|---|
| FR-1.1 | All UI strings containing "L1", "L2", "L3", "L4", "L1–L4", "L1/L2/L3" are replaced with one of two real concepts: **Gateway Cache** (the existing exact-match response cache) or **Provider Prompt Cache** (the upstream's cache discount). | Must |
| FR-1.2 | All i18n keys in `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json` that reference L-layers are renamed and re-translated to use the new vocabulary. Stale keys are removed (no `@deprecated`). | Must |
| FR-1.3 | The `tooltipGatewayLayers` i18n key (which describes "L1 Exact-match · L2 Semantic · L3 Compressed response cache") is deleted entirely — its content was wrong. The replacement tooltip describes only what exists: "Gateway response cache stores normalized request → response. Identical subsequent requests are served without calling the upstream — full upstream cost avoided." | Must |
| FR-1.4 | The Cache ROI Dashboard column groupings `colGroupL1L3` and `colGroupL4` are renamed to `colGroupGatewayCache` and `colGroupProviderPromptCache`. The "L1–L4 combined savings" badge is removed — combined totals were misleading because the two units differ. | Must |
| FR-1.5 | The Overview widget's `cacheSavingsDesc` no longer shows "(L1–L4)" suffix. It shows two separate stat rows: "Gateway Cache savings — $X — Y requests bypassed upstream entirely" and "Provider Prompt Cache savings — $Z — N tokens served at discount". | Must |
| FR-1.6 | The Traffic Audit Drawer's "Gateway cache HIT" banner (which used to say "L1/L2/L3 served this request") simply says "Served from Gateway Cache". A "Details" disclosure shows the cache key fingerprint and TTL — useful for debugging without exposing fictional tier labels. | Must |

### FR-2: Backend API field naming aligned with the new vocabulary

| ID | Requirement | Priority |
|---|---|---|
| FR-2.1 | `admin_cost_summary.go` field `TotalL4CacheNetSavingsUSD` renamed to `TotalProviderPromptCacheNetSavingsUsd`. `TotalGatewayCacheSavingsUSD` stays (already correctly named). | Must |
| FR-2.2 | `admin_cache_roi.go` per-adapter and per-day breakdown fields are renamed identically. | Must |
| FR-2.3 | The CP-UI service layer (`api/services/analytics.ts`, `api/services/quotaAnalytics.ts`) is updated to consume the new field names. Old field names are not aliased — clean cutover per CLAUDE.md no-backward-compat rule. | Must |
| FR-2.4 | A comment audit in `api/services/analytics.ts` removes the now-stale "L1/L2/L3 platform response cache" / "L4 provider-side prompt cache" comments. | Must |

### FR-3: Prometheus metric naming

| ID | Requirement | Priority |
|---|---|---|
| FR-3.1 | Metric `MetricRequestsWithL4CacheHit` is renamed to `MetricRequestsWithProviderPromptCacheHit`. The Prometheus exposed name (`nexus_aigw_requests_with_provider_prompt_cache_hit_total` or equivalent) follows. | Must |
| FR-3.2 | A one-release alias keeps the old metric name visible **only** if there are operator dashboards or alert rules referencing it. Audit `tools/db-migrate/seed/data/seed-baseline.sql` `AlertRule` rows and any internal Grafana panels first; if no references, drop the old name immediately. | Should |
| FR-3.3 | `MetricGatewayCacheSavingsUSD` stays — already correctly named. | Must |
| FR-3.4 | A new metric `MetricGatewayCacheLookupTotal{outcome="hit\|miss"}` is added (if not already present) to make hit-rate computable from Prometheus alone — the existing schema requires summing two derived rates. This is a Should-have improvement bundled here while we're in the cache-metrics neighborhood. | Should |

### FR-4: Architecture doc cleanup

| ID | Requirement | Priority |
|---|---|---|
| FR-4.1 | `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md` (currently the tier-1 doc for all cache-related code per `architecture-doc-triggers.md`) is audited and restructured. All references to L2 / L3 caches are either deleted or moved into a clearly-marked "## Roadmap (not implemented)" appendix. | Must |
| FR-4.2 | The doc's main body describes only the caches that exist in code: gateway response cache (the one Redis-backed exact-match), config caches (`cachelayer/`, `configcache/`), stream coalescer (`streamcache/`), Gemini cachedContent integration (`geminicache/`), and the various small in-process LRUs (cert cache, IAM cache). | Must |
| FR-4.3 | A pointer is added from the cache architecture doc to `cost-estimation-architecture.md` § 6 (the UI contract) so contributors editing cache UI find the field-naming rules. | Must |
| FR-4.4 | The CLAUDE.md auto-memory note about cache UX (if one exists) is updated; if no memory exists yet, one is added stating "Gateway Cache + Provider Prompt Cache are the only two user-visible cache concepts; no L1/L2/L3/L4 labels". | Should |

### FR-5: No admin "Cache Strategies" panel

| ID | Requirement | Priority |
|---|---|---|
| FR-5.1 | An earlier brainstorm proposed a new admin page exposing L1 / L2 / L3 as three configurable strategies. This proposal is **explicitly out of scope** because L2 and L3 do not exist. The decision is documented in the SDD and in `cost-estimation-architecture.md` § 6.3. | Must |
| FR-5.2 | If semantic or compressed caches ship in a future epic, the admin UI for them ships with the implementation — not as advance scaffolding. **Schema-level enum slots reserved for future cache kinds (e.g., `gateway_cache_kind = 'semantic'`) are exempt — they are unsurfaced until populated and exist only to avoid a future schema migration.** | Must |
| FR-5.3 | The existing Hub config edit surface (where the single response cache's TTL and size limits live) is sufficient for current configuration needs. No new admin page is created. | Must |

### FR-6: Unified cache_status state model

| ID | Requirement | Priority |
|---|---|---|
| FR-6.1 | `traffic_event.cache_status` is redefined to a **two-value** enum: `HIT | MISS`. All UI filter dropdowns binding to cache state present exactly three options: `Any`, `HIT`, `MISS`. | Must |
| FR-6.2 | `cache_status` is derived at write time per the rule in `cost-estimation-architecture.md § 6.4`: `HIT iff gateway_cache_status ∈ {hit, hit_inflight} OR provider_cache_status = 'hit'`, `MISS` otherwise. The derivation is performed inside `audit.go` (`DeriveCacheStatus()` helper) and run by the gateway audit-writer once both gateway and provider statuses are known. | Must |
| FR-6.3 | Invalid (gateway, provider) combinations are rejected at write time. The 8 valid combinations are documented in `cost-estimation-architecture.md § 6.4`. | Must |
| FR-6.4 | Dev-phase migration: existing pre-migration `traffic_event` rows carrying the old 6-value `cache_status` enum are dropped or overwritten per the no-backward-compat development-phase rule (CLAUDE.md). No data-migration code for runtime-only rows. | Must |

### FR-7: traffic_event schema additions

| ID | Requirement | Priority |
|---|---|---|
| FR-7.1 | Add column `gateway_cache_status` to `traffic_event` (`tools/db-migrate/schema.prisma`). Type: nullable string. Allowed values: `hit | hit_inflight | miss | skipped`. | Must |
| FR-7.2 | Add column `gateway_cache_skip_reason`. Type: nullable string. Allowed values: `disabled | no_cache | passthrough | not_cacheable`. Only populated when `gateway_cache_status = 'skipped'`. | Must |
| FR-7.3 | Add column `gateway_cache_kind`. Type: nullable string. Allowed values: `extract | semantic`. Only populated when `gateway_cache_status ∈ {hit, hit_inflight}`. Today always `extract`; `semantic` is reserved schema-only. | Must |
| FR-7.4 | Add column `provider_cache_status`. Type: nullable string. Allowed values: `hit | miss | na`. | Must |
| FR-7.5 | Single-column index on `cache_status` for filter performance: `@@index([cache_status])`. | Must |

### FR-8: Filter dropdown reduction (Live Traffic + Cache ROI)

| ID | Requirement | Priority |
|---|---|---|
| FR-8.1 | The "Cache" filter dropdown in `LiveTrafficAdvancedFilters.tsx` reduces from 6 options (HIT/HIT_LIVE/MISS/DISABLED/SKIP_NO_CACHE/PASSTHROUGH_SKIP) to 3 options (Any/HIT/MISS). The TypeScript type `LiveTrafficCacheStatus` reduces accordingly. | Must |
| FR-8.2 | No advanced/secondary filter for gateway-specific or provider-specific cache states. Drill-down detail lives in the traffic-event drawer, not in the filter UX. | Must |
| FR-8.3 | The admin API query param `cacheStatus` accepts only `HIT` or `MISS` (or empty). | Must |

### FR-9: Drawer cache block redesign

| ID | Requirement | Priority |
|---|---|---|
| FR-9.1 | `trafficAuditDrawer.tsx` renders a "Cache" block with one of three layouts (Gateway-served / Provider-discount / No savings) per the rendering rules in `cost-estimation-architecture.md § 6.4`. The block headline is the unified `cache_status` (HIT or MISS). | Must |
| FR-9.2 | The raw `cache_status` enum value (e.g., `HIT_LIVE`, `SKIP_NO_CACHE`) is never shown to the user. All enum values map to human labels. | Must |
| FR-9.3 | The gateway cache HIT banner trigger logic reads unified `cache_status === 'HIT'` (not the old `cacheStatus === 'HIT' || 'HIT_LIVE'`). Banner savings amount preference: `gatewayCacheSavingsUsd` first, fallback to `cacheNetSavingsUsd`. | Must |

### FR-10: audit.go enum refactor

| ID | Requirement | Priority |
|---|---|---|
| FR-10.1 | `packages/ai-gateway/internal/platform/audit/audit.go:131-136` splits the existing single `CacheStatus` enum (6 values) into three Go types: `GatewayCacheStatus`, `ProviderCacheStatus`, `CacheStatus` (unified, 2 values). | Must |
| FR-10.2 | A new `GatewayCacheSkipReason` enum captures the four skip reasons. | Must |
| FR-10.3 | A `DeriveCacheStatus(gw GatewayCacheStatus, pv ProviderCacheStatus) CacheStatus` pure function implements the rule in `cost-estimation-architecture.md § 6.4`. Table-driven unit tests cover all 8 valid combinations + invalid-combination rejection. | Must |
| FR-10.4 | All call sites in the cache decision path and provider-response audit-stamping path write the three internal statuses + run `DeriveCacheStatus()` once both are known. | Must |

### FR-11: Provider prompt cache lookup metric (parallel to gateway)

| ID | Requirement | Priority |
|---|---|---|
| FR-11.1 | Add Prometheus metric `MetricProviderPromptCacheLookupTotal{outcome="hit|miss|na"}` parallel to the gateway `MetricGatewayCacheLookupTotal` introduced in FR-3.4. Provider hit rate computable as `outcome="hit" / (sum over outcomes except "na")`. | Must |
| FR-11.2 | Emission point: provider response usage parser stamps the outcome based on `cache_read_tokens` and supported-model detection. | Must |

---

## 3. Non-Functional Requirements

| ID | Requirement |
|---|---|
| NFR-1 | All UI changes are i18n-complete in all three locales (en / zh / es) before merge. `npm run check:i18n` lint passes. |
| NFR-2 | All UI changes pass `npm run check:design-tokens` (CSS / inline-style hygiene). No new hex colors, no new raw numeric paddings. |
| NFR-3 | `npm test` for `packages/control-plane-ui` passes — Vitest tests covering the renamed pages don't break. |
| NFR-4 | `go test ./packages/control-plane/internal/handler/...` passes for the renamed API field names. |
| NFR-5 | Per-package coverage gates from CLAUDE.md hold (≥95 %), with no new entries to the coverage allowlist. |
| NFR-6 | The user-visible string changes are reviewed for product-voice consistency — sentence case in en, no exclamation points, no marketing fluff. |

---

## 4. User Roles & Personas

| Role | Touchpoints |
|---|---|
| **Application developer reading the Cost dashboard** | Sees clear two-concept breakdown. Can answer "what did the cache do for me" in one sentence. |
| **Platform admin** | Reads the Cache ROI dashboard for capacity planning. Can compute hit rate from Prometheus directly (`MetricGatewayCacheLookupTotal`). Doesn't have to explain "what is L2" to teammates. |
| **Auditor / Finance** | Reconciles cache savings against provider invoices. Provider Prompt Cache savings line is directly comparable to the cached-discount line on Anthropic / OpenAI bills. |
| **OSS adopter** | Reads `cache-multi-tier-architecture.md` and gets an accurate picture of what actually runs. No more "what does L2 do?" confusion when reading the code. |

---

## 5. Constraints & Assumptions

- **C1.** The renames are pre-GA (no installed user base reading the field names externally). Cleaner immediate cutover beats compatibility aliasing.
- **C2.** The Prometheus metric rename may break internal Grafana panels referencing the old name. A pre-merge audit lists every dashboard referencing `requests_with_l4_cache_hit*` and updates each. If any external alert subscribers exist, the one-release alias from FR-3.2 covers them.
- **C3.** The fiction in i18n tooltips has been there since 2026-Q1; no user-flow accommodation is made for documentation referring to L2/L3 (none was ever real).
- **C4.** If a customer asks "where's my L2 semantic cache?", the answer is "it was a planned feature, not implemented. We removed the misleading label." No commitment is made to ship semantic cache as a result of this epic.

---

## 6. Glossary

| Term | Meaning (post-E59) |
|---|---|
| **Gateway Cache** | The exact-match response cache in `packages/ai-gateway/internal/cache/`. Redis-backed. Stores `normalize(canonical_request) → response_envelope`. Hit means the upstream is not called and the full upstream cost is avoided. There is **one** of these — no "L1 Gateway Cache" vs "L2 Gateway Cache". |
| **Provider Prompt Cache** | The upstream provider's own cache (Anthropic ephemeral cache_control, OpenAI auto prompt caching, Gemini cachedContent). Nexus observes its effect via `cache_read_input_tokens` / `cached_tokens` in the response usage. The request still reaches the upstream — only some input tokens are discounted. |
| **L1 / L2 / L3 / L4** | Deprecated labels. Removed from all user-visible surfaces. If you see them in code or docs after E59 ships, file a bug. |
| **Cache hit rate** | `MetricGatewayCacheLookupTotal{outcome="hit"} / MetricGatewayCacheLookupTotal` (new metric introduced by FR-3.4). |
| **cache_status (unified)** | The two-value rollup column on `traffic_event` (`HIT | MISS`) every filter binds to. Derived at write time per `cost-estimation-architecture.md § 6.4`. |
| **gateway_cache_status** | Internal column on `traffic_event` recording the gateway-side cache decision (`hit | hit_inflight | miss | skipped`). Detail-only; not exposed in filter UIs. |
| **gateway_cache_skip_reason** | Internal column populated only when `gateway_cache_status = 'skipped'`. Values: `disabled | no_cache | passthrough | not_cacheable`. |
| **gateway_cache_kind** | Internal column recording the kind of gateway cache that fired. Today always `extract`; `semantic` reserved schema-only for a future implementation. |
| **provider_cache_status** | Internal column recording the provider prompt-cache outcome (`hit | miss | na`). `na` covers two sub-cases disambiguated by `cache_read_tokens` NULL (no provider call) vs zero (call but no cache support). |

---

## 7. MoSCoW Priority

| Story | Priority | Rationale |
|---|---|---|
| S1 — UI / API / metric rename + cache architecture doc cleanup + unified cache_status state model + traffic_event schema migration | **Must** | Single coherent change. The vocabulary cleanup (L-fiction removal) and the state-model redesign (unified HIT/MISS + four detail columns) share the same UI surfaces, the same i18n keys, and the same `traffic_event` schema; splitting them risks half-renamed surfaces and two migrations of the same table. |

The epic has one story — the work is unified rename + delete-fiction. The single SDD lays out the full file-by-file change list.

---

## 8. Out of Scope

- Semantic cache implementation (not in this epic; a future epic ships it WITH UI).
- Compressed cache implementation (likewise).
- New cache strategy admin page (explicitly out per FR-5).
- Multi-tier cache architecture redesign (the current single-tier reality is sufficient; if a second tier is added later, it gets its own design doc and UI).
- A second "advanced" filter dropdown for gateway-specific or provider-specific cache states. The drill-down is the audit drawer, not a filter (per FR-8.2).
- Backfill of historical `cache_status` values on existing `traffic_event` rows (per FR-6.4, dev-phase wipe is acceptable).
- Cost-saved-derivation refactor. The cost numbers on the drawer continue to use the existing fields (`gateway_cache_savings_usd`, `cache_net_savings_usd`, etc.); no new derived savings columns this epic.

---

## 9. Acceptance Criteria

| ID | Acceptance |
|---|---|
| AC-1 | `grep -r "L1\|L2\|L3\|L4" packages/control-plane-ui/src/` finds no occurrence in `.tsx`, `.ts`, or `.json` files within the cache/ROI/analytics context (false positives in unrelated code — e.g., `L2 TTL` in IAM cache — remain). |
| AC-2 | The Cache ROI Dashboard shows exactly two cache concepts. Visual review by a non-engineer confirms the page is understandable without prior knowledge of the codebase. |
| AC-3 | The Traffic Audit Drawer for a cache-hit request shows "Served from Gateway Cache" with a Details disclosure. No "L1" / "L2" / "L3" string visible. |
| AC-4 | `cache-multi-tier-architecture.md` describes only caches that exist in code. The L2 / L3 sections are either deleted or under a marked "Roadmap (not implemented)" appendix. |
| AC-5 | `MetricRequestsWithProviderPromptCacheHit` is the active metric name. Either the old `L4` name is fully removed (preferred) or a one-release alias is in place (if dashboards reference it). |
| AC-6 | All i18n locales (en / zh / es) have consistent, accurate cache vocabulary. `npm run check:i18n` passes. |
| AC-7 | A user-flow walkthrough — admin clicks Cache ROI → reads numbers → clicks into a traffic event → expands cache details → returns to dashboard — completes without encountering a fictional or unexplained term. |
| AC-8 | `traffic_event` schema contains 4 new columns (`gateway_cache_status`, `gateway_cache_skip_reason`, `gateway_cache_kind`, `provider_cache_status`); `cache_status` enum is restricted to `HIT|MISS`; single-column index on `cache_status` exists; migration applied; old enum values purged per FR-6.4. |
| AC-9 | `DeriveCacheStatus()` table-driven unit test passes for all 8 valid (gateway, provider) combinations and rejects all invalid combinations. Per-package Go coverage ≥95% on touched packages. |
| AC-10 | Live Traffic filter dropdown shows exactly 3 options (Any/HIT/MISS); admin API `cacheStatus` query param accepts only HIT/MISS or empty. |
| AC-11 | Traffic Audit Drawer renders the new 3-layout Cache block (Gateway-served / Provider-discount / No savings) for representative fixtures; raw enum values are never visible. |
| AC-12 | `smoke-gateway --all-ingress` validates: all 4 new `traffic_event` fields populate correctly across the 29-model × 4-ingress matrix; unified derivation matches `cost-estimation-architecture.md § 6.4` for every observed (gateway, provider) combo. |
