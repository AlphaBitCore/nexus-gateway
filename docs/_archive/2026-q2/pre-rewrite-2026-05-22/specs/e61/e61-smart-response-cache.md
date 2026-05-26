# E61 — Smart Response Cache (Time-Sensitive Skip + Semantic L2)

> Epic: 61
> Status: Draft
> Date: 2026-05-19
> Architecture impact: `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` (full dual-tier rewrite); `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md` (catalogue + skip-reason additions); `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md` (Cache Settings UI consolidation cross-ref); `docs/developers/architecture/README.md` (two new rows)
> SDD: `docs/developers/specs/e61-s1-…s7-…` (7 stories)
> OpenAPI: `docs/users/api/openapi/admin/e61-s2-cache-policy.yaml`, `docs/users/api/openapi/admin/e61-s6-cache-admin.yaml`
> Successor epics queued: E68 (negative-feedback channel), E69 (FAQ pre-warm), E70 (sticky-token guard), E71 (domain thresholds). E62-E67 are reserved by the cross-adapter embeddings / multimodal epic family — do not reuse.

---

## 1. Background

The Gateway response cache today is a single tier — exact-match SHA-256 of the canonicalized request. Two real and reproducible problems break it:

**Problem 1 — Time-sensitive prompts mis-served.** A user asks "当前股价是多少？" (what's the current stock price?) at 10:00; the response is cached. Another user asks the same at 16:00; they receive the 10:00 answer with no indication of staleness. The cache has no notion of freshness. The same class of bug applies to weather, news, scores, exchange rates, latest model versions, "today / now / latest" questions.

**Problem 2 — No semantic matching.** A user asks "Summarize this article in 3 bullets"; another user asks "Give me the 3 main points of this article". Same prompt template, different phrasing. The exact-match cache misses on the second request and pays full upstream cost for a substantially equivalent answer. The cache ROI dashboard shows hit rates of 2-5% on conversational workloads — the floor of what's achievable with byte-exact matching.

Both problems are widely recognized in the response-cache literature; the existing `cost-estimation-architecture.md § 6.4` even reserves the `gateway_cache_kind = 'semantic'` enum value schema-only for "a future semantic-cache implementation". This epic builds that future implementation, plus the freshness layer that protects both tiers from time-sensitive false-hits.

The user (gateway admin) explicitly raised both problems on 2026-05-19. The brainstorm-review pass added five engineering hardenings (A1-A5) and five product hardenings (P1-P5) — captured in the project memory `project_e61_smart_response_cache` and codified in §2.

---

## 2. Functional Requirements

### FR-1: Time-sensitive prompt skip (Phase 1)

| ID | Requirement | Priority |
|---|---|---|
| FR-1.1 | New package `packages/ai-gateway/internal/cache/freshness/` exposes `IsTimeSensitive(canonicalMessages, rules) (matched bool, ruleID string)`. Algorithm: keyword + co-occurrence pattern (keyword + question-mark + entity), NOT bare keyword match. Bare keyword match (e.g., "now" as discourse particle) MUST NOT trigger skip. | Must |
| FR-1.2 | Rule list is Hub-shadow-pushed via configKey `response_cache.time_sensitive_patterns`. Each rule: `{id, keywords[], require_question_mark, require_entity, languages[]}`. ZH + EN seed rules cover stock/exchange-rate/weather/news/score/"current state" patterns. | Must |
| FR-1.3 | When a rule fires: stamp `GatewayCacheStatus=skipped`, `GatewayCacheSkipReason=time_sensitive`. Skip BOTH lookup AND write across BOTH tiers (extract + semantic). | Must |
| FR-1.4 | New audit constant `GatewayCacheSkipReasonTimeSensitive = "time_sensitive"` in `packages/ai-gateway/internal/platform/audit/audit.go`. | Must |
| FR-1.5 | Admin can disable individual rules via configKey override. Per-route `response_cache_policy.skip_time_sensitive: bool` (default `true`) is the master switch. | Must |
| FR-1.6 | Detection runs once per request, before any cache decision. Latency budget ≤2ms p99. | Must |
| FR-1.7 | New Prometheus metric `nexus_cache_freshness_skips_total{rule_id, reason}` for ROI dashboard / debugging. | Should |
| FR-1.8 | **Admin UI for time-sensitive rules** (P5 Round-1 review). The Cache Settings page hosts a new section listing the active rule list (read from `GET /api/admin/cache/time-sensitive-patterns`), with per-rule enable/disable toggles, an "Add rule" modal, and a **Test box** where admin pastes a sample prompt and sees which (if any) rule would fire. Backend exposes `PUT /api/admin/cache/time-sensitive-patterns/:id` for enable/disable + bulk save, and `POST /api/admin/cache/time-sensitive-patterns/test` for the dry-run. Rule writes go through the Hub shadow → ai-gateway `cache/freshness/` reloads atomically (S1 T1.3 swap mechanism). | Must |

### FR-2: Semantic L2 cache (Phase 2)

| ID | Requirement | Priority |
|---|---|---|
| FR-2.1 | New package `packages/ai-gateway/internal/cache/semantic/`. Backed by **Valkey 8.x + valkey-search** module (BSD3 license — preserves OSS readiness). Vector index uses HNSW + cosine similarity. | Must |
| FR-2.2 | **One fleet-wide Redis Vector index per cluster**, named from `semantic_cache_config.redis_index_name` (default `nexus:semantic-cache:v1`). Dimension comes from L1 singleton's `embedding_dimension`. Per-(embProv, embModel) indexing is explicitly rejected — see §FR-8 (L1 fleet-wide singleton mirrors `ai_guard_config`) and `response-cache-architecture.md` §3.5 (fingerprint-driven lifecycle). | Must |
| FR-2.3 | Index schema folds in (upstream_provider, upstream_model, vk_scope) as tag filters. Default lookups filter on all three so a semantic hit comes from the same upstream model AND same VK as the incoming request. | Must |
| FR-2.4 | Per-route policy `semantic.allow_cross_model` (default `false`) lifts the upstream-model filter when admin opts in. Streaming responses ignore this toggle and ALWAYS scope to same upstream provider+model (StreamEntry holds native RawBytes; cross-vendor replay is fragile). | Must |
| FR-2.5 | Read flow: extract miss → freshness OK → input fits → embed (singleflighted by hash of embedding input) → `FT.SEARCH` KNN k=1 → if cosine ≥ threshold → stamp `GatewayCacheStatus=hit, GatewayCacheKind=semantic` → replay via shared hit-replay path. | Must |
| FR-2.6 | Write flow: on extract-write completion → embed (reuse singleflight if read path already embedded) → `HSET` to L2 with TTL mirroring extract TTL. | Must |
| FR-2.7 | New audit constant `GatewayCacheKindSemantic` is finally written (currently `audit.go:173` reserves it schema-only). | Must |
| FR-2.8 | Default per-route `semantic.enabled=false` — admin must opt in per route. Default threshold `0.96`. Default `vary_by="vk"` (stricter than extract default). | Must |
| FR-2.9 | New Prometheus metrics: `nexus_cache_l2_lookups_total{outcome}`, `nexus_cache_l2_similarity_histogram`, `nexus_cache_embedding_latency_seconds`, `nexus_cache_embedding_calls_total{provider,model}`, `nexus_cache_embedding_cost_usd_total`. | Must |
| FR-2.10 | Latency budget on L1 miss path: ≤80ms p95 added by L2 lookup (embedding + KNN). | Must |

### FR-3: Embedding via existing Provider system

| ID | Requirement | Priority |
|---|---|---|
| FR-3.1 | Embedding calls run through the existing `Provider/Adapter/VK/credential` infrastructure. New `endpointType=embedding` on the existing types. NO parallel "internal AI client" framework. | Must |
| FR-3.2 | Two reference deployments ship with seed data: (a) OpenAI cloud (`provider=openai`, `model=text-embedding-3-small`, dim 1536, context 8191); (b) Local OpenAI-compatible server (`provider=local-inference`, configurable baseURL — same server may also host routing-decision LLM and ai-guard endpoints per `project_local_inference_server_direction`). Admin selects which provider+model the cluster uses **once**, on the fleet-wide Cache Embedding Settings page (§FR-8), not per-route. | Must |
| FR-3.3 | Embedding call is singleflighted by `SHA-256(embedding_input)` to deduplicate concurrent identical requests. | Must |
| FR-3.4 | Adapter conformance per `provider-adapter-architecture.md §3a` — embedding endpoint uses standard PrepareBody → Execute → response-codec flow with `endpointType=embedding`. | Must |

### FR-4: Shared input-staging primitive

| ID | Requirement | Priority |
|---|---|---|
| FR-4.1 | New package `packages/shared/transport/inputstaging/` exposes `Plan(messages, modelContextLimit, strategy, reserveOutput) → PlanResult` and `Suggest(contextLimit, profile) → Strategy`. | Must |
| FR-4.2 | Strategy enum (5 values): `last_user`, `system_plus_last_user` (default), `recent_turns`, `head_plus_tail`, `full_truncated`. | Must |
| FR-4.3 | `PlanResult.OverflowKind` ∈ {`none`, `single_message_too_big`, `after_strategy`}. Callers act on overflow per their domain (semantic cache → skip; routing → fallback to bigger-context model; ai-guard → caller-defined). | Must |
| FR-4.4 | E61 is the first consumer (semantic-cache embedding). Smart-routing and ai-guard adopt the same primitive in future epics — captured as tasks #14 / #15, not in scope here. | Must |
| FR-4.5 | `Suggest()` auto-recommends a strategy from the chosen `Model.contextLimit` + a typical-prompt-size profile. The InputStagingSelector UI component highlights the suggestion. | Should |

### FR-5: Per-route policy schema migration

| ID | Requirement | Priority |
|---|---|---|
| FR-5.1 | `response_cache_policy` migrates from flat `{enabled, ttl, vary_by_user, vary_by_vk}` to nested `{extract: {enabled, ttl, vary_by}, semantic: {enabled, threshold, embed_strategy, vary_by, allow_cross_model}, skip_time_sensitive}`. Per-route policy carries **NO** embedding-model fields — those are fleet-wide singleton (§FR-8). | Must |
| FR-5.2 | Migration writes the existing flat fields into `extract.*` and creates `semantic.*` defaults (`enabled=false`). No compat layer; the new shape is the only shape after migration. | Must |
| FR-5.3 | `traffic_event.embedding_cost_usd DECIMAL(20,10)` added. Stamped on every L1 miss that triggered an embedding call (regardless of whether the embedding resulted in an L2 hit or miss). | Must |
| FR-5.4 | New audit constants `GatewayCacheSkipReasonTimeSensitive` and `GatewayCacheSkipReasonOversizeForEmbedding` defined in `audit.go`. | Must |
| FR-5.5 | configKey constants added in `packages/shared/schemas/configkey/` for: `response_cache.time_sensitive_patterns` (cluster-wide rule list), `response_cache.policy.*` (per-route fields). Hub shadow push wired to `OnConfigChanged` in ai-gateway. | Must |
| FR-5.6 | Cross-VK and cross-org defaults: `extract.vary_by` default remains `none` (existing behaviour preserved). `semantic.vary_by` default is `vk` (stricter — VK is the tenant unit; shared-VK admin warning surfaces in UI per FR-7.2). | Must |

### FR-6: Cost + ROI accounting

| ID | Requirement | Priority |
|---|---|---|
| FR-6.1 | `traffic_event.embedding_cost_usd` stamps the cost of every embedding call on the L1 miss path (whether L2 hit or miss). Pricing comes from the embedding Model row. | Must |
| FR-6.2 | Net savings calculation on Cache ROI page: `net_savings = gateway_cache_savings_usd - embedding_cost_usd`. Surfaced as a separate line. | Must |
| FR-6.3 | When `semantic.allow_cross_model=true` and an actual cross-model hit fires, `gateway_cache_savings_usd` is priced at the REQUESTED model's pricing (not the cached model's). UI tooltip explains this so admins understand the accounting choice. | Must |
| FR-6.4 | Cache ROI page adds an "L2 net contribution" chart: cumulative L2 savings minus cumulative embedding spend. Negative regions signal "L2 is costing more than it saves" — actionable for the admin. When the net contribution is negative for ≥3 consecutive days, the page renders an inline alert with concrete recommendations: "(a) raise threshold to 0.98 to require closer matches; (b) disable L2 on routes with low paraphrase rate (sort routes by L2-hit-rate ascending and identify the bottom quartile); (c) switch to a cheaper embedding model on the Cache Embedding Settings page." Each recommendation is a deep-link to the relevant page. | Should |
| FR-6.5 | **Per-route embedding-cost ceiling** (runaway-cost protection). Per-route policy carries `semantic.embedding_cost_ceiling_usd_per_day` (default `null` = no cap). When the day's accumulated `embedding_cost_usd` for the route exceeds the ceiling, the gateway auto-disables L2 for that route until next UTC midnight and emits a tagged audit row. Admin sees a banner on the route's Cache Settings page: "L2 auto-disabled until 2026-05-20 00:00 UTC — daily embedding budget exceeded ($X / $Y). Investigate routes burning embedding cost without proportional savings." | Should |

### FR-8: L1 — Fleet-wide embedding singleton (mirror of `ai_guard_config`)

| ID | Requirement | Priority |
|---|---|---|
| FR-8.1 | New singleton table `semantic_cache_config` (id='singleton') carries `embedding_provider_id`, `embedding_model_id`, `embedding_dimension`, `embedding_fingerprint`, `redis_index_name`, `enabled`, `updated_at`, `updated_by`. Shape mirrors `ai_guard_config` exactly so reviewers see the same pattern. | Must |
| FR-8.2 | `embedding_fingerprint = sha256(provider_id ':' model_id ':' dimension)` is recomputed inside `SemanticCacheStore.Save` so callers can't forget. ConfigCache observes the change on next reload; mismatch + non-empty new fingerprint emits a one-shot `semantic_cache.invalidate_all` job. The Hub-side consumer runs `FT.DROPINDEX <old>` + `FT.CREATE <new>` against the cluster's Valkey. Idempotent. | Must |
| FR-8.3 | `enabled` is the fleet-wide kill switch — incident response can flip it false to disable semantic cache everywhere instantly without touching any routing rule. | Must |
| FR-8.4 | New admin endpoints `GET /api/admin/semantic-cache/config` + `PUT /api/admin/semantic-cache/config` (E61-S3 T1a.5). IAM-gated on `iam.ResourceSemanticCache.{Read, Update}` — new resource added to `packages/shared/identity/iam/catalog_data.go` per E61-S2 T1a. NexusViewer fixture gains `admin:semantic-cache.read`. | Must |
| FR-8.5 | New admin UI **Settings → Cache Embedding** page (E61-S6c) — mirror of **Settings → AI Guard**. Provider+model picker, enabled toggle, fingerprint display, probe button, last-reindex timestamp. Save-time confirmation modal when (provider, model) change triggers a fingerprint mismatch (and therefore a reindex). | Must |
| FR-8.6 | Effective-enabled cascade evaluated by the L2 reader on every request: `L1.enabled AND L1.embedding_model_id IS NOT NULL AND L2.enabled`. | Must |
| ~~FR-8.7~~ | ~~Forward-compat for SaaS multi-tenant.~~ **Dropped 2026-05-20**: Nexus ships as a single-tenant on-prem product. SaaS direction was abandoned; the `org_id` column + composite PK + per-org Redis index naming were removed from schema, store, handler, and reindex job. Re-evaluate only if the SaaS direction is ever revived. | Won't |

### FR-7: Cache Settings UI consolidation

| ID | Requirement | Priority |
|---|---|---|
| FR-7.1 | The existing "Prompt Cache" admin page is renamed to "Cache Settings". Top callout: "This page controls two distinct cache layers: (1) Provider Prompt Cache reuses the model's KV-cache (saves TTFT + input cost); (2) Gateway Response Cache stores the full response (avoids the entire upstream call)." | Must |
| FR-7.2 | The page hosts three sections: **Provider Prompt Cache** (existing E38 controls), **Gateway Extract Cache** (new — toggle, TTL, vary_by), **Gateway Semantic Cache** (new — toggle, threshold, embed_strategy via `<InputStagingSelector/>`, vary_by, allow_cross_model). The embedding model **is NOT** picked on this page — it's a fleet-wide singleton configured on **Settings → Cache Embedding** (§FR-8). This page shows a read-only chip (`Embedding: <provider> / <model> (<dim>D)`) deep-linking to that page, plus a yellow banner when L1 is unconfigured. | Must |
| FR-7.3 | The Gateway Semantic Cache section carries TWO warning banners: **(P1)** "Semantic cache may return responses to similar-but-different queries. Recommended for non-factual workloads (summarization, idea generation, creative writing); avoid for factual Q&A, translation, calculations." **(P2)** "Semantic cache is isolated per Virtual Key (default). If multiple humans share one VK, their queries can semantically match each other's cached responses — treat shared VKs as a single user." | Must |
| FR-7.4 | When admin enables `allow_cross_model=true`, an additional inline warning appears: "Cross-model matches can return a cheaper model's cached response when this route routes to a premium model. Verify that workload tolerance allows this." | Must |
| FR-7.5 | All visible strings in 3 locales (EN/ZH/ES). i18n namespace `pages` keys `pages.cacheSettings.*`. Existing `pages.promptCache.*` keys migrate (renamed, not aliased — pre-GA). | Must |
| FR-7.6 | New cross-bundle React component `packages/ui-shared/src/components/InputStaging/InputStagingSelector.tsx`. Props `{ modelContextLimit, value, onChange, profile }`. Renders strategy dropdown with the `Suggest()`-recommended option highlighted. Used by Cache Settings; ready for adoption by smart-routing rule editor + ai-guard rule editor in future epics. | Must |
| FR-7.7 | IAM impact review: the rename is the only IAM-relevant change. Same `iamMW(...)` action as the existing prompt-cache page (no policy carve-out). | Must |

---

## 3. Non-Functional Requirements

| ID | Requirement | Notes |
|---|---|---|
| NFR-1 | L2 lookup adds ≤80ms p95 on the L1 miss path. | Embedding latency dominates; OpenAI 3-small is typically 30-50ms; KNN ≤3ms. |
| NFR-2 | Freshness detection adds ≤2ms p99. | Regex compilation cached; rule list bounded ≤200 rules. |
| NFR-3 | Per-package Go unit-test coverage ≥95% on every new package (CLAUDE.md binding). | `cache/freshness`, `cache/semantic`, `shared/transport/inputstaging`. |
| NFR-4 | The full-surface `/smoke-gateway --all-ingress` MUST pass before S7 closes. | Per CLAUDE.md AI-Gateway / traffic_event binding. |
| NFR-5 | Valkey switch in dev (docker-compose) must keep the existing seeds + local dev flow runnable without manual intervention. | One image swap; valkey-search module load at startup. |
| NFR-6 | Production Valkey switch is an ops event tracked separately (not part of E61 code). Migration runbook documents the steps. | Single-EC2 single Redis instance — migrate during a planned maintenance window. |
| NFR-7 | Cache HIT correctness across ingress formats. | Existing `proxy_cache.go` hit-replay (egress reshape via `canonicalbridge.ResponseCanonicalToIngress`) is reused — both extract and semantic hits share this path. |
| NFR-8 | Backward compatibility: NONE (pre-GA). | Per CLAUDE.md development-phase policy. |
| NFR-9 | Multi-instance readiness. | L1+L2 in single Valkey → multi-instance ready by default. Pattern lists shadow-pushed → all instances stay in sync. |

---

## 4. User Roles & Personas

- **Gateway Admin** — configures routing rules, picks embedding providers, sets thresholds, monitors Cache ROI. Primary persona for the UI in §FR-7. Needs to understand the false-positive tradeoff (P1) and the shared-VK trap (P2) without reading the architecture doc.
- **End user (LLM client)** — issues requests through a Virtual Key. Experiences faster responses on cache hits. Cannot directly affect cache policy. May (in future E68) signal "bad cache" feedback to the gateway.
- **Operations / On-call** — operates Valkey, monitors hit rates and embedding costs, responds to Cache ROI anomalies. Needs the new Prometheus metrics (FR-1.7, FR-2.9) and the failure-mode runbook.
- **Compliance reviewer** — verifies that hooks fire correctly on cache hits, including semantic hits where the response was produced for a different prompt. Needs FR-2.5's guarantee that request-stage hooks have ALREADY approved the incoming prompt before semantic lookup.

---

## 5. Constraints & Assumptions

### Constraints

- C-1 (binding): Production stays single-EC2 single-Valkey for this epic. Multi-instance is not a hard requirement but the design must not preclude it.
- C-2 (binding): No backward compatibility. Pre-GA.
- C-3 (binding): No `git stash` / `git add -A` (CLAUDE.md parallel-session safety).
- C-4 (binding): All new yaml secret/credential fields require explicit user approval (CLAUDE.md secrets rule). Embedding provider credentials use the existing `Credential` table — no new yaml secret fields.
- C-5 (binding): adapter conformance per `provider-adapter-architecture.md §3a` for the embedding endpoint.
- C-6 (binding): English-only repository text.
- C-7 (binding): Valkey + valkey-search license MUST stay BSD-compatible (chosen over redis-stack SSPL for this reason).

### Assumptions

- A-1: `valkey-search` module provides `FT.CREATE` / `FT.SEARCH` / vector type compatible with the RediSearch wire syntax. Confirmed at integration time during S3; if surprises emerge, S3's design allows swap to a plain Valkey vector path.
- A-2: HNSW ghost-document cleanup on TTL expiry works as in RediSearch. Verified at S3 integration time; if not, S3 adds a background sweeper.
- A-3: OpenAI text-embedding-3-small remains available and its pricing remains $0.02/M tokens for the duration of E61 ROI calculations. Pricing change is admin-managed via the Model record.
- A-4: The local OpenAI-compatible server (when admin chooses it over cloud) speaks the standard `/v1/embeddings` schema.
- A-5: Existing audit pipeline writes `traffic_event` rows with full gateway+provider cache status pair — no MQ envelope changes needed.

---

## 6. Glossary

- **Extract cache (L1)** — exact-match cache, the historical response cache.
- **Semantic cache (L2)** — new in E61; KNN-similarity cache backed by valkey-search.
- **Time-sensitive prompt** — prompt asking about current/changing state (stock, weather, news, "now / today / latest"). Both tiers skip on detection.
- **Freshness pattern** — co-occurrence rule (keyword + question-mark + entity) Hub-pushed to ai-gateway.
- **Embedding provider** — a Provider record with `endpointType=embedding`. Can be OpenAI cloud or a local OpenAI-compatible server.
- **Input staging** — the truncation step that fits a large conversation into an embedding model's context window. Shared primitive at `packages/shared/transport/inputstaging/`.
- **Cross-model semantic match** — when `semantic.allow_cross_model=true`, a request to model X can hit a cached response originally produced by model Y. Off by default.
- **vary_by** — the cache-scoping dimension. `semantic.vary_by` defaults to `vk` (per-VK isolation).
- **Valkey** — the BSD-licensed Redis-compatible KV store + vector module that replaces vanilla Redis in E61.

---

## 7. MoSCoW Priority Summary

**Must (in scope for E61):**

- Time-sensitive skip with co-occurrence patterns + admin UI rule editor (FR-1.1–1.6, FR-1.8).
- Semantic L2 with valkey-search, **fleet-wide singleton index** + fingerprint-driven lifecycle, default same-upstream-model scoping (FR-2.1–2.10).
- Embedding via existing Provider system + singleflight (FR-3.1–3.4).
- Shared `inputstaging` primitive (FR-4.1–4.4).
- Policy schema migration + new audit constants + configKey wiring (FR-5.1–5.6).
- `embedding_cost_usd` column + ROI net-savings line (FR-6.1–6.3).
- Cache Settings page consolidation + warning banners + InputStagingSelector component (FR-7.1–7.7).
- **L1 fleet-wide embedding singleton** + Cache Embedding Settings page mirror of AIGuardPage + new IAM resource (FR-8.1–8.6).

**Should (nice to have, in scope if time permits):**

- Cache freshness metric breakdown by rule_id (FR-1.7).
- L2 net-contribution chart in Cache ROI (FR-6.4).
- `Suggest()`-driven highlight in InputStagingSelector (FR-4.5).

**Could (deferred to future epics):**

- Negative-feedback channel for cache poisoning → **E68**.
- Pre-warm L2 from FAQ corpus → **E69**.
- Sticky-token exact-match guard → **E70**.
- Domain-specific thresholds → **E71**.
- Routing / ai-guard adopt `inputstaging` primitive → captured as tasks #14 / #15.
- ~~Multi-tenant `org_id` PK on `semantic_cache_config`~~ — dropped 2026-05-20; Nexus is single-tenant on-prem.

**Won't (explicitly out of scope):**

- LLM-classifier secondary judgment for time-sensitive detection (user vetoed — keyword + co-occurrence is sufficient for v1).
- Cross-provider semantic match without `allow_cross_model` explicitly enabled by admin.
- pgvector / external vector DB (Qdrant, Weaviate, etc.) — Valkey aligns cache lifecycle and avoids new infrastructure.
- A separate "Cache Strategies" admin page (covered in §FR-7 by consolidating into Cache Settings).

---

## 8. Open questions

None at requirements-freeze time. The brainstorm-review pass on 2026-05-19 closed:

- (Q) Should L2 cross-provider match? → **No by default; admin opt-in toggle.** Settled in §FR-2.4.
- (Q) `vary_by` default for semantic? → **`vk` (stricter than extract).** Settled in §FR-5.6.
- (Q) Vector storage? → **Valkey + valkey-search (BSD3).** Settled in §FR-2.1.
- (Q) Embedding provider default? → **OpenAI cloud; local server is drop-in.** Settled in §FR-3.2.
- (Q) Time-sensitive detection method? → **Keyword + co-occurrence pattern, no LLM judge.** Settled in §FR-1.1.
