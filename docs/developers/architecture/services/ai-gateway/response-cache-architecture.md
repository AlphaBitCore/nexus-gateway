# Response cache architecture

The AI Gateway's response cache short-circuits live upstream calls for requests it has seen before, across two tiers — an L1 exact-match cache and an L2 semantic vector cache. The tier **mechanism** (cache key construction, the Valkey HNSW index, cross-shape reshape, cache-status stamping, cost-on-HIT, poisoning) is documented once in [cache-multi-tier-architecture.md](../../cross-cutting/storage/cache-multi-tier-architecture.md). This page covers the AI-Gateway-specific concerns that doc does not: how the cache is **configured** (fleet-wide, runtime-hot-swappable), and where it sits in the request lifecycle.

## 1. Configuration is fleet-wide

All response-cache configuration is fleet-wide. There is no per-route or per-rule cache policy — a request's cache behavior is determined entirely by the two singleton config rows below plus the request's own headers and passthrough flags. This keeps the admin surface to two global settings rather than a cache stanza on every routing rule.

Two singleton rows back the two tiers:

- **`extract_cache_config`** (L1) — `enabled`, `ttl_seconds` (range `[60, 604800]`, default `3600`), and `apply_freshness_rules`. The freshness flag lives physically on the L1 row but its effect is fleet-wide across **both** tiers: when on, a time-sensitive request is skipped at L1 and L2 alike.
- **`semantic_cache_config`** (L2) — the embedding provider/model/dimension (with a derived `embedding_fingerprint` and a versioned `redis_index_name`), `enabled`, the cosine `threshold` (default `0.96`), the isolation scope `vary_by` (`none` / `user` / `vk` / `org`, default `vk`), the `embed_strategy` (default `system_plus_last_user`), `allow_cross_model` (default `false`), and admin freshness-rule overrides in `time_sensitive_overrides`.

A couple of L1 knobs are yaml-only because changing them would invalidate existing entries: the Redis key `Prefix` and the per-entry `MaxEntryBytes` cap. Everything else is runtime-swappable.

## 2. Config flow and runtime hot-swap

Admins edit the two rows through the Control Plane; the values reach the gateway as Hub shadow keys, dispatched in `packages/ai-gateway/cmd/ai-gateway/configdispatch`:

| Shadow key | Applied to |
|---|---|
| `response_cache.extract_config` | `Cache.SetConfig` — swaps the L1 `Enabled`, `TTL`, and `ApplyFreshnessRules` atomics. An empty payload disables L1 (`SetConfig{Enabled: false}`). |
| `semantic_cache.config` | `IndexLifecycle.OnConfigSnapshot` — atomically updates the L2 in-process snapshot (embedding provider/model/base-URL/price/dimension, threshold, scope, strategy) and, on an embedding-fingerprint change, ensures/rotates the Valkey index. |
| `response_cache.time_sensitive_patterns` | `FreshnessDetector.Reload` — replaces the compiled time-sensitive rule set. |

Each handler applies its blob atomically, so cache behavior changes without a service restart. The L1 knobs are stored as atomics (`Cache.IsEnabled()` / `Cache.ApplyFreshnessRules()` are read on the hot path); the L2 snapshot is an atomically-swapped pointer guarded by `ConfigCache.EffectiveEnabled()`, which additionally requires the embedding provider, model, and dimension to be populated before L2 is consulted. The config layering and shadow-key contract are described in [configuration-architecture.md](../../cross-cutting/foundation/configuration-architecture.md).

## 3. Where the cache sits in the request lifecycle

The cache phase runs after request hooks and before the in-flight broker. The stage's own gate is the disjunction of the two tiers' enable flags — there is no third, global cache switch:

```go
// packages/ai-gateway/internal/ingress/proxy/stage_cache.go
l1Enabled := /* Cache.IsEnabled() && scope ready */
l2Enabled := /* SemanticConfigCache.EffectiveEnabled() */
cacheEnabled := l1Enabled || l2Enabled
```

Either tier being on makes the request cache-eligible; both off takes the lean cache-off path (no key build, no lookup, no freshness projection). Fleet-wide emergency cache-off is therefore done by flipping those same per-tier flags (the status strip's "Disable all gateway cache" on `/ai-gateway/cache` does exactly that, fleet-wide, in one click) — a durable off that persists until re-enabled.

The other way off is **Emergency Passthrough**: a `bypassCache` tier short-circuits both the key build and the lookup/write, and the pre-lookup classifier stamps `(skipped, "passthrough")` rather than `(skipped, "no_cache")` so an operator-forced bypass is SQL-distinguishable from a client's `X-Nexus-No-Cache` header (which it also overrides). That path is auditable (mandatory reason + `enabledBy`) and time-boxed (≤ 8 h, auto-reverted). The two surfaces are complementary: the status-strip disable-all is the fast, durable incident switch; passthrough is the governed, self-reverting, optionally provider-scoped bypass. See [emergency-passthrough-architecture.md](../../cross-cutting/safety/emergency-passthrough-architecture.md) and [cache-multi-tier-architecture.md](../../cross-cutting/storage/cache-multi-tier-architecture.md).

For each request the handler:

1. Runs the pre-lookup classifier, which short-circuits to `skipped` for the embeddings endpoint (`embeddings_endpoint` — embeddings are never cached because each input is unique per workflow step and not session-bound, so caching wastes Redis and dilutes the chat cache-hit dashboards; checked first, regardless of cache config), a disabled cache, a no-cache header, a passthrough bypass, no routing target, or a freshness match (when `apply_freshness_rules` is on).
2. Looks up L1 against the canonical key.
3. On an L1 miss, attempts L2 via `tryL2Lookup`.
4. On a full miss, dispatches through the broker (which coalesces concurrent identical misses onto one upstream call).

The L2 path reads its settings from the fleet `semantic_cache_config` snapshot — `fleetSemanticPolicy` assembles the threshold, embed strategy, scope, and cross-model flag from the singleton; there is no per-route decode. The isolation scope resolves the Redis tag the entry is filtered by: `vk` → virtual-key id, `user` → user id, `org` → org id, `none` → cross-tenant. The embedding input is built from the canonical messages via the configured `embed_strategy`. The canonical the cache consumers read is post-redaction: when a request hook rewrote the wire body, the rewritten bytes are renormalized once (`proxyState.cacheNormalized`, memoized) so the embedding provider and the vector store see the redacted content the upstream sees — never the pre-hook original; a renormalize failure skips the cache semantics for that request rather than falling back to the stale canonical. Requests without a rewrite (the vast majority) keep the admission-time canonical with no added work. The lookup/write-back sequence and the cache-status stamping are detailed in [cache-multi-tier-architecture.md](../../cross-cutting/storage/cache-multi-tier-architecture.md).

## 4. L2 supporting machinery

The L2 subsystem is assembled in `packages/ai-gateway/cmd/ai-gateway/wiring/semantic.go`. Beyond the reader and writer it wires:

- **Index lifecycle** — watches the embedding fingerprint (provider + model + dimension) and ensures the Valkey index exists. A fingerprint change triggers a blue/green index rotation so stale-dimension entries fall out of reach behind the new fingerprint tag.
- **Circuit breaker** — one breaker per `(provider, model)` embedding endpoint. After a run of consecutive embedding failures within a window it opens, and L2 lookups for that pair are stamped `embedding_circuit_open` and fall through to the broker without firing an embedding call until the cooldown elapses.
- **Embedding singleflight** — deduplicates concurrent identical embedding calls so a burst of the same prompt issues one embedding request.

When the Redis client is not a plain `*redis.Client` (for example a Sentinel or Cluster client), the reader and writer are left nil and L2 is silently skipped — the gateway still serves L1 and live traffic.

## References

- `tools/db-migrate/schema/cache.prisma` — `ExtractCacheConfig`, `SemanticCacheConfig` models
- `packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go` — shadow-key dispatch into cache config
- `packages/ai-gateway/cmd/ai-gateway/wiring/semantic.go` — L2 subsystem assembly (reader, writer, index lifecycle, circuit breaker, singleflight)
- `packages/ai-gateway/internal/cache/core/cache.go` — L1 cache, runtime config atomics
- `packages/ai-gateway/internal/cache/semantic/config_cache.go` — L2 in-process config snapshot
- `packages/ai-gateway/internal/cache/freshness/detector.go` — time-sensitive rule detector
- `packages/ai-gateway/internal/ingress/proxy/proxy_l2.go` — fleet L2 policy assembly, scope resolution, lookup/write-back integration
- `packages/shared/schemas/configkey/configkey.go` — `response_cache.extract_config`, `semantic_cache.config`, `response_cache.time_sensitive_patterns` keys
- `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md` — the two-tier cache mechanism
