---
doc: cache-multi-tier-architecture
area: cross-cutting
service: storage
tier: 1
updated: 2026-05-20
---

# Cache Multi-Tier Architecture

> **Tier 1 architecture doc.** Read this before adding a cache layer or debugging cache invalidation. Excludes the prompt cache (separate doc at `prompt-cache-architecture.md`). The Redis "cache only — no pub/sub" rule from CLAUDE.md is binding here.

Nexus uses many caches across services. They differ in tier (in-process vs Redis vs both), eviction (TTL vs LRU vs explicit), and invalidation (signal-driven vs TTL-only). This doc catalogs them and codifies the invalidation patterns.

---

## 1. The Valkey "cache-only" rule (binding)

> Since E61 (2026-05) the Redis-compatible store is **Valkey 8.x + valkey-search** (BSD3) rather than vanilla Redis or Redis Stack (SSPL). The cache-only contract is unchanged; the operational change is the image swap in docker-compose + production. All references below to "Redis" / "Valkey" mean the Valkey-compatible KV+vector store.

Valkey is **not** used for pub/sub at Nexus. Every kind of pub/sub was migrated to the Hub WebSocket change-signal pattern (cross-ref `thing-config-sync-architecture.md`).

Valkey is used for:

- **Sessions** (admin UI) — short-lived.
- **IAM policy cache (L2 tier)** — 60-s TTL Redis backing for the in-process L1 (10-s TTL) — code at `packages/control-plane/internal/identity/iam/cache.go:12-17`; Redis key prefix `nexus:iam:policies:`.
- **Rate-limit counters** — sliding window.
- **Response cache — Extract tier** (AI Gateway L1; exact-match).
- **Response cache — Semantic tier** (AI Gateway L2; HNSW KNN since E61).
- **Desired-state cache** (Thing shadows; per-Thing snapshot for fast lookup).
- **Quota counters** (per-scope Lua-scripted sliding window — cross-ref `quota-architecture.md`).
- **Cert cache** (compliance proxy leaf certs; LRU + Valkey two-tier).

Adding a new "I want Redis pub/sub for X" is **forbidden**. The pattern is: shadow + change-signal + pull (or local invalidation on event).

## 2. Tier catalogue

| Cache | Location | Eviction | Invalidation | Hit-rate target |
|---|---|---|---|---|
| IAM policy cache (per-principal) — L1 | In-process map (`sync.RWMutex`) | TTL 10s | Hub WS change-signal on policy update drops both tiers | 95%+ |
| IAM policy cache (per-principal) — L2 | Redis (key prefix `nexus:iam:policies:`) | TTL 60s | Hub WS change-signal on policy update drops both tiers | 95%+ |
| IAM action catalog | In-process | Refresh on signal | Hub WS change-signal | 100% (full preload) |
| Managed-policy cache | In-process | Refresh on signal | Refresh on admin restart | 100% |
| Session cache | Redis | TTL session-duration | Explicit logout | n/a |
| Rate-limit counters | Redis | Sliding window | Implicit window expiry | n/a |
| Quota counters | Redis | Sliding window | Reserve/reconcile (cross-ref quota doc) | n/a |
| Desired-state cache | Redis | TTL + change-signal | Hub WS change-signal | 99%+ |
| Cert cache (compliance proxy leaves) | In-proc LRU + Redis | LRU + cert validity | Implicit expiry; explicit invalidation on CA rotation | 99%+ |
| Response cache — Extract tier (L1, AI GW) | Valkey | TTL + LRU | Explicit invalidation per (provider, model) on config change | Variable (deterministic prompts) |
| Response cache — Semantic tier (L2, AI GW; E61) | Valkey + valkey-search HNSW | TTL + LRU | Fleet-wide singleton + fingerprint-driven FT.DROPINDEX/FT.CREATE on embedding-model swap | Variable (paraphrased prompts) |
| Embedding infrastructure singleton (E61) | Postgres `semantic_cache_config` + in-process ConfigCache | Hub shadow change-signal | `embedding_fingerprint = sha256(provider:model:dim)` mismatch triggers `semantic_cache.invalidate_all` job — mirrors `ai_guard_config.backend_fingerprint` | 100% |
| Adapter manifest cache | In-process | Refresh on signal | Hub WS change-signal | 100% |
| Provider/model catalog cache | In-process | Refresh on signal | Hub WS change-signal | 100% |
| Credstate (decrypted credentials) | In-process | Dirty-set | `credstate.MarkDirty` on credential update (cross-ref credentials doc) | 99%+ |
| Local agent shadow snapshot | Local file + in-process | Boot reload + change-signal | Hub WS change-signal | 100% |

(Prompt cache is its own doc — `prompt-cache-architecture.md`.)

## 3. Eviction strategies

| Strategy | Used by |
|---|---|
| **TTL** | IAM policy (L1 10s in-process + L2 60s Redis), session |
| **LRU** | Cert cache (in-proc), response cache (Redis) |
| **TTL + LRU combined** | Response cache |
| **Sliding window** (semantic eviction) | Rate-limit, quota |
| **Signal-driven full refresh** | IAM action catalog, manifest catalog, model catalog |
| **Signal-driven dirty-set** | Credstate |

The choice matters: TTL is cheap but produces eventual consistency (up to 60s window for stale IAM at the L2 tier — the in-process L1 narrows the typical staleness to 10s); signal-driven is exact but requires reliable signals (the Hub WS change-signal handles this).

## 4. Invalidation patterns

### Pattern A — TTL (no explicit invalidation)

Used when the cache miss cost is low and freshness within TTL is acceptable. Example: IAM policy cache (L1 10s + L2 60s, two-tier). A 60-s upper-bound window of stale policy is acceptable because policy changes are rare and a stale read still resolves to a real policy.

### Pattern B — Hub WS change-signal (full refresh)

Used when the cache is small and the change is rare. The Hub sends a change-signal on the relevant Thing's shadow; the service's `OnConfigChanged` callback refreshes the entire cache. Example: IAM action catalog, provider catalog.

### Pattern C — Hub WS change-signal (per-key invalidation)

Used when the cache is large and changes are granular. The change-signal includes the affected key id; the service invalidates only that entry. Example: routing-rule cache, hook-config cache.

### Pattern D — Dirty-set (lazy refresh)

The cache marks an entry as dirty; the next access triggers a refresh. Example: `credstate`. Useful when the new value is needed by code that already knows how to fetch fresh.

### Pattern E — Implicit (eviction via expiry)

The cache entries carry their own expiry (e.g., a minted leaf cert valid until X). Eviction is implicit when expiry passes. Example: cert cache.

## 5. Cert cache (two-tier LRU + Redis)

The compliance proxy mints leaf certs on TLS bump. The cert cache prevents re-minting for the same destination on every request:

- **L1 (in-process LRU)** — small, hot. Latency budget: microseconds.
- **L2 (Redis)** — larger, cross-instance. Latency budget: low milliseconds.

On a request:

1. Look up in L1 → hit, return.
2. Look up in L2 → hit, promote to L1, return.
3. Mint, store in L1 + L2.

Cert validity is short (~24h). When the CA rotates (rare), an admin-triggered cache wipe clears both tiers.

## 6. Hit-rate metrics

Each cache exposes Prometheus counters:

- `nexus_cache_hits_total{cache="iam_policy"}`.
- `nexus_cache_misses_total{cache="iam_policy"}`.
- `nexus_cache_size{cache="cert_cache_lru"}`.

Alert thresholds: a cache that drops below 80% hit rate is suspicious — either eviction is too aggressive or invalidation is firing too often (signal storm).

## 7. Local vs distributed considerations

Today's prod is single-EC2. Many caches that would benefit from distribution (in-process IAM action catalog) are fine as in-process because there's one instance. When the architecture moves to multi-instance:

- In-process caches still work but each instance has its own copy.
- Signal-driven refresh fans out via the same Hub WS path — each Thing receives the signal.
- Redis caches share state automatically.

There is **no** plan to add a distributed in-process cache (e.g., Hazelcast). The patterns above scale to multi-instance without that complexity.

## 8. The "no Redis pub/sub" deletion artifact

The pre-Hub architecture used Redis pub/sub for config invalidation:

- `nexus:config:shared` channel.
- Payloads `"hooks" | "routing" | "credentials" | "all"`.

That channel is **dead**. The `shared/heartbeat`, CP `internal/pubsub`, CP `internal_registry` packages are deleted. If a comment or doc still mentions Redis pub/sub for invalidation, it is stale; replace with Hub WS change-signal language.

## 9. Failure modes

| Failure | Behaviour |
|---|---|
| Valkey down | In-process caches keep working. Valkey-backed caches (session, response cache L1 + L2) fail-open: cache lookups return miss, the request continues to the upstream provider. No `gateway_cache_skip_reason` is stamped — the outcome is a regular `miss` (the cache layer simply could not answer). For the L2 semantic tier the embedding call also fails, so neither tier produces a hit; the request proceeds to the broker. |
| Stale entry | TTL bound; eventual consistency. |
| Signal lost | Cache stays stale until next TTL refresh or next signal. Rare in practice; binding rule (cross-ref thing-config-sync) ensures Cat B keys carry `needsPull: true`. |
| Cache hot-path stalled | Background refresh thread per cache; serves stale + refreshes async. |
| Memory pressure | LRU evicts; size metric trips alert. |

## 10. Adding a new cache

Checklist:

1. Choose tier (in-process / Redis / both) based on size + cross-instance need.
2. Choose eviction (TTL / LRU / signal-driven).
3. Choose invalidation pattern (§4 A–E).
4. Add Prometheus counters per §6.
5. Add an alert rule on hit-rate floor.
6. Document in §2 catalogue.
7. If signal-driven, wire `OnConfigChanged` callback per `thing-config-sync-architecture.md`.

## 11. Gateway response cache: outcome states

The response cache in `packages/ai-gateway/internal/cache/` is the only entry in this catalogue whose outcome is surfaced to end-users via the audit drawer and the Cache ROI dashboard. The outcome model is **unified** with the upstream provider prompt cache (see `prompt-cache-architecture.md`) under a single `traffic_event.cache_status` column (`HIT | MISS`).

### Internal states (gateway side)

`gateway_cache_status` (one column on `traffic_event`):

| State | Meaning |
|---|---|
| `hit` | Exact-match key found; response served from cache; upstream not called. |
| `hit_inflight` | Singleflight coalescer attached this request to an identical in-flight one; leader's response replayed. |
| `miss` | Key looked up; not found; upstream called. |
| `skipped` | Cache layer not consulted. Reason in `gateway_cache_skip_reason`: `disabled` (cache module nil) / `no_cache` (client header `x-nexus-aigw-no-cache`) / `passthrough` (E48 emergency passthrough) / `not_cacheable` (response shape ineligible) / `time_sensitive` (E61 — prompt matched a time-sensitive rule; both L1 and L2 skipped) / `oversize_for_embedding` (E61 — semantic tier only; even strategy-truncated input exceeded embedding context — L1 is unaffected). |

The `gateway_cache_kind` column (`extract | semantic`) is set on hits only. Both values are now in active use since E61 (2026-05) — `extract` for the exact-match L1 hits and `hit_inflight` singleflights, `semantic` for the L2 KNN-similarity hits. See `response-cache-architecture.md` §3 for the semantic-tier read/write paths and the per-(embedding_provider, embedding_model) index layout.

On semantic hits, the gateway also stamps `traffic_event.gateway_cache_l2_entry_key` with the served entry's Redis HASH key — `<redis_index_name>:<sha256(EmbeddingInput)[:16]>`. This is the key the gateway's `IsPoisoned` check will consult on its next FT.SEARCH hit, so the audit drawer's "Mark as bad cache hit" thumbs-down posts it verbatim as the poison-list entryKey. The column is NULL on extract hits, MISS rows, SKIPPED rows, and on rows from data-planes that don't run L2 (compliance-proxy, agent).

### Unified derivation

The unified `cache_status` (`HIT | MISS`) rolls up gateway + provider cache outcomes. Filter UIs bind to `cache_status`; the four breakdown columns above are detail-only. Full derivation table (8 valid combinations) and drawer rendering rules: `cost-estimation-architecture.md § 6.4`.

## 12. Sources

- `packages/shared/storage/configcache/` — shadow-key snapshot cache.
- `packages/shared/storage/redisfactory/` — common Valkey/Redis client factory used across services.
- `packages/control-plane/internal/identity/iam/cache.go` — IAM policy cache.
- `packages/ai-gateway/internal/cache/` — response cache + helpers (L1 extract + L2 semantic).
- `packages/compliance-proxy/internal/tls/cache/` — leaf cert cache (in-proc LRU + Valkey two-tier).
- `packages/shared/schemas/credstate/` — credstate dirty-set constants (Redis keys, circuit/health enums, default thresholds).

## 13. Cross-references

- `prompt-cache-architecture.md` — prompt cache is its own tier story.
- `thing-config-sync-architecture.md` — Hub WS change-signal pattern.
- `iam-identity-architecture.md` — IAM policy cache lifecycle.
- `quota-architecture.md` — Redis-backed sliding window counters.
- `credentials-architecture.md` — credstate dirty-set.
- `compliance-pipeline-architecture.md` — cert cache + cert mint.
