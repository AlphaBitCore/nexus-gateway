# Cache multi-tier architecture

The Nexus Gateway response cache short-circuits live upstream calls for AI Gateway requests whose inputs the gateway has seen before. The cache emits a billable `traffic_event` row at the would-have-paid upstream cost and stamps a `gateway_cache_savings_usd` field for analytics, so a HIT is observable end-to-end without losing the spend-attribution that the cache replaced.

The cache is two-tier:

- **L1** — exact-match response cache keyed by a canonicalised request body hash. Backed by Redis (string `SET` / `GET`).
- **L2** — semantic vector cache. Backed by Valkey's `valkey-search` module (`FT.CREATE` HNSW index + `FT.SEARCH`). Looked up only when L1 misses.

A third sibling — the in-flight singleflight broker — coalesces concurrent MISS callers for the same key onto one upstream call and one cache write; joiners are stamped as `hit_inflight` instead of `miss`.

What the cache does **not** serve:

- Requests with the `x-nexus-aigw-no-cache` header (`gateway_cache_status=skipped`, reason `no_cache`).
- Requests whose `ResolvedRequest.Passthrough.BypassCache` flag is set by an emergency passthrough rule (`skipped`, reason `passthrough`).
- Requests classified as time-sensitive by the freshness detector when the runtime `applyFreshnessRules` knob is on (`skipped`, reason `time_sensitive`).
- Requests with no resolvable routing target or with the cache module not wired (`skipped`, reason `disabled`).
- Upstream failures — the broker's leader path returns the error directly; no entry is written.
- Entries exceeding the per-tier size cap (L1: 1 MiB default; L2: 256 KiB default).

A skipped request still runs the provider adapter's `PrepareBody` (request normaliser + provider prompt-cache marker injection, e.g. Anthropic `cache_control`) before going upstream — `stage_cache.go` `prepareUpstreamBody` runs on every skip branch as well as the cache-eligible path. Skipping the gateway's own cache never disables the provider-side prompt cache; the two tiers are independent.

Streaming and non-streaming requests both cache; the L1 schema discriminator (`stream/v1` vs `response/v1`) prevents one being decoded as the other.

## Anchor packages

- L1: `packages/ai-gateway/internal/cache/core/`
- L2: `packages/ai-gateway/internal/cache/semantic/`
- In-flight broker: `packages/ai-gateway/internal/cache/stream/`
- Freshness detector: `packages/ai-gateway/internal/cache/freshness/`
- Cache hit/miss orchestration (proxy): `packages/ai-gateway/internal/ingress/proxy/proxy.go`, `proxy_cache.go`, `proxy_l2.go`
- Cross-shape reshape: `packages/ai-gateway/internal/execution/canonicalbridge/`
- Cache enums + audit record: `packages/ai-gateway/internal/platform/audit/audit.go`
- Wire shape on `traffic_event`: `packages/shared/transport/mq/messages.go`
- Admin negative-feedback (poison) endpoint: `packages/control-plane/internal/ai/cache/handler/semantic_feedback.go`

## L1 — exact-match response cache

`cache/core.Cache` is a Redis-backed key/value store keyed on a SHA-256 of the request inputs. The receiver is safe on `nil` (every method short-circuits to a no-op), so the gateway can run without a cache wired.

**Two entry types**, both carrying an `OriginWireShape typology.WireShape` tag (see § OriginWireShape tagging):

- `StreamEntry` — schema `stream/v1`. Carries the full upstream chunk timeline (`[]ChunkRecord`) including each chunk's raw SSE/NDJSON bytes so HIT replay is byte-equivalent to the original upstream response. Usage totals, upstream HTTP headers, and provider + model are preserved.
- `ResponseEntry` — schema `response/v1`. Carries the canonical (or upstream-shape) response JSON, usage totals, and upstream headers.

`LookupStream` / `LookupResponse` enforce the schema discriminator on read — a value stored as `response/v1` is invisible to a `LookupStream` call. `StoreStream` / `StoreResponse` enforce a `maxEntryBytes` cap (default 1 MiB) and return `ErrCacheEntryTooLarge` on oversize; oversize entries are silently skipped on the write path.

**Hot-swappable runtime knobs** (`Cache.SetConfig`):

- `Enabled` — atomic bool. When false, both `Lookup*` and `Store*` short-circuit to no-ops.
- `TTL` — nanosecond-precision Redis SET TTL. Default 1 hour.
- `ApplyFreshnessRules` — gates whether the proxy's pre-lookup classifier honours a freshness detector match.

The proxy reads these through `IsEnabled()` / `ApplyFreshnessRules()` on the hot path; the values are pushed via the Hub shadow (`response_cache.extract_config`) and dispatched into `SetConfig` without restarting the service.

**Yaml-only knobs** (set once at boot, since changing them invalidates existing entries):

- `Prefix` — Redis key prefix (default `nexus:cache`).
- `MaxEntryBytes` — per-entry size cap.

## Cache key

`Cache.BuildScopedKey(provider, model, body, allowlistVersion, scopeKey)` (or the legacy `BuildKey` for `vary_by=none`) produces the L1 cache key. `BuildScopedKey` is the current production entrypoint (`proxy.go`). Inputs:

1. A schema version header `v3\n` pinning the key layout — older keys are unreachable.
2. The upstream **provider name** and **provider model id** (the strings the gateway will send to the wire, not the client-facing alias).
3. The **canonicalised JSON body** — object keys sorted recursively at every nesting level, array order preserved. Non-JSON bodies pass through unchanged. This guarantees that semantically identical requests with different SDK key orderings collide on the same key.
4. The **forward-header allowlist version hash** — folded in so a yaml allowlist change invalidates entries whose `UpstreamHeaders` were recorded under a different effective filter.
5. An optional **scope key** (e.g. `vk:<id>`, `user:<id>`, `org:<id>`) — folded as `scope=<scopeKey>\n` when non-empty. When `vary_by=none` the scope key is empty, producing a byte-identical key to the legacy layout (fleet-wide dedup is preserved). Non-empty scope keys enforce per-tenant L1 isolation for the same body + provider + model — matching the isolation that L2's tag filter applies. See `cache/core/cache.go` `BuildScopedKey`.

The body that's hashed is the output of the provider adapter's `PrepareBody`, not the raw client body. That folds out cross-format ingress differences (Anthropic ingress → OpenAI target produces the same key whether the caller hit `/v1/messages` or `/v1/chat/completions`).

The proxy also runs an optional `Normaliser.NormalizeKey` step on the prepared body before hashing. This strips volatile fields (e.g. provider billing nonces) that would otherwise force every request to miss. The mutation is key-only; the upstream call still receives the unmodified prepared body.

**L1 skip reason `GatewayCacheSkipReasonNoEmbeddableText`**: the L2-eligibility check in `proxy_l2.go` uses this reason for all cases where no embeddable text can be extracted from the request. See `platform/audit/enums.go`.

## L2 — semantic vector cache

`cache/semantic` writes entries to a Valkey `valkey-search` HNSW index and looks them up via approximate-nearest-neighbour cosine search.

**`FT.CREATE` schema** (per index):

```
SCHEMA
  vector            VECTOR HNSW 12 DIM <dim> TYPE FLOAT32 DISTANCE_METRIC COSINE M 16 EF_CONSTRUCTION 200 EF_RUNTIME 10
  upstream_provider TAG
  upstream_model    TAG
  vk_scope          TAG
  response_kind     TAG
  fingerprint       TAG
  cached_at         NUMERIC
```

`response_body`, `usage`, and `origin_wire_shape` are payload-only HASH fields — written but NOT indexed. The reader retrieves them via `FT.SEARCH ... RETURN`, which works against unindexed fields.

**Per-entry key**: `<indexName>:<sha256(EmbeddingInput | vk_scope | response_kind [| upstream_provider | upstream_model])[:16]>` — 16 hex chars from a SHA-256 over the embedding text **plus** the entry's scope, response kind, and (unless `AllowCrossModel` is set) its provider + model, NUL-delimited. Folding these in keeps logically-distinct entries that share embedding text on separate HASH keys, so the same prompt written under different tenants, models, or response kinds does not mutually evict via HSET overwrite. Reads never reconstruct the key — `FT.SEARCH` locates the entry by vector + tag filter and returns the stored key — so the composition is purely a write-side uniqueness concern. The returned key drives the L2 entry's poison-list key (see § Poisoning). When `AllowCrossModel` is true the model is interchangeable for retrieval, so provider+model are dropped from the key and the newest response for a given (input, scope, kind) supersedes the prior one.

**Lookup query**:

```
(@vk_scope:{<vk>} @response_kind:{<kind>} @fingerprint:{<fp>}
 [@upstream_provider:{<p>} @upstream_model:{<m>}])
 =>[KNN 1 @vector $vec AS __vector_score]
```

The `upstream_provider` / `upstream_model` clauses are dropped when the caller sets `AllowCrossModel`. Tag values are escaped for `,` and `|` (the search module's TAG separator and OR operator); `-` is intentionally NOT escaped because Valkey-search's TAG dialect treats it as a literal — escaping breaks UUID-shaped scopes.

**Threshold + scoring**: `valkey-search` returns cosine distance in `[0, 2]`; the reader inverts via `similarity = clamp(1 − distance/2, 0, 1)`. A hit with `similarity < Threshold` is treated as a miss. The default threshold floor is 0.96 (rejected as out-of-range otherwise).

**Per-search hard timeout**: 20 ms. On timeout the reader returns nil and the caller stamps `gateway_cache_skip_reason = semantic_search_timeout`.

**Embedding cost stamp**: every L2 lookup that issues an embedding call records the embedding USD cost into `traffic_event.embedding_cost_usd` and the embedding model id into `traffic_event.embedding_model_id`, regardless of whether the lookup hit, missed, or skipped. The L2 read path uses an `EmbeddingSingleflight` so concurrent identical inputs share one embedding call.

**Eligibility — `EffectiveEnabled()`**: the L2 cache is only consulted when the runtime fleet-wide enabled flag is on AND the embedding provider id, embedding model id, and embedding dimension are all populated. A missing embedding model produces `skip_disabled` and falls through to the broker.

## Lookup order + write-back

The proxy's cache phase runs after request hooks and before the broker. Per request:

1. **Pre-lookup classifier** — `classifyCachePreLookup` returns either `(skipped, <reason>)` (cache disabled, no-cache header, passthrough bypass, no routing target, time-sensitive freshness match) or `("", "")` (proceed).
2. **L1 lookup** — `Cache.LookupStream` or `Cache.LookupResponse` against the canonical key. On hit, stamp `gateway_cache_status = hit`, `gateway_cache_kind = extract`, `provider_cache_status = na`, and dispatch into the HIT pipeline.
3. **L2 lookup on L1 miss** — `Handler.tryL2Lookup` runs unconditionally; returns false (and the caller proceeds to the broker) when the L2 reader is not wired or the per-route policy disables semantic. On hit, stamp `gateway_cache_status = hit`, `gateway_cache_kind = semantic`, `gateway_cache_l2_entry_key = <reader entry key>`, `provider_cache_status = na`.
4. **Broker path on full miss** — `streamcache.Registry.Subscribe(key, leaderFn)` returns `(subscription, isFirst, err)`. The first subscriber's `leaderFn` fires the live upstream call and stamps `gateway_cache_status = miss`; joiners share the in-flight stream and stamp `gateway_cache_status = hit_inflight`.
5. **L2 write-back** — on the leader's terminal frame, `Handler.scheduleL2Write` fires a goroutine with a 5-second deadline that embeds the canonical prompt text and HSETs an L2 entry. Both response kinds are written: a non-streaming leader writes a `response_kind=response` entry whose body is the canonical response JSON (fired from `handleNonStreamWithSubscription`, or `ServeProxy` on the direct path); a streaming leader writes a `response_kind=stream` entry whose body is the canonical `[]ChunkRecord` timeline (the broker's `writeCache` invokes `CacheMeta.OnStreamCachePersisted`, which routes the same timeline JSON L1 stored into `scheduleL2Write`). Streaming therefore L2-hits on equivalent subsequent requests rather than paying an embedding charge on a guaranteed miss. The VK scope on every write is resolved from the fleet `vary_by` setting (vk / user / org / none) — the same dimension the reader filters on — so a write and the matching read isolate identically. L1 write-back is internal to the broker's `writeCache` step (single canonical timeline regardless of how many joiners attached).

Joiners do NOT issue upstream calls and do NOT reconcile against the quota counter — the leader pays the upstream cost and reconciles its own quota.

## OriginWireShape tagging

Every L1 entry (both `StreamEntry` and `ResponseEntry`) and every L2 entry carries a single `OriginWireShape typology.WireShape` field that records the wire shape the entry was produced under. The cache HIT comparison is one equality test:

```go
// packages/ai-gateway/internal/ingress/proxy/proxy_cache.go
sameShape := entry.OriginWireShape == ingress.WireShape
```

When the shapes differ, the cache layer hands the entry body to `CanonicalBridge.ResponseAcrossFormats(from, to typology.WireShape, body []byte) ([]byte, error)`. The reshape pipeline is:

1. Decode `from`-shape body to canonical chat-completion JSON (via the source codec's `DecodeResponse`, or `openai.DecodeResponsesResponse` when `from == WireShapeOpenAIResponses`).
2. Re-encode canonical to `to`-shape via `ResponseCanonicalToIngress(toFormat, canonical)`.

`from == to` short-circuits and returns the body unchanged.

Entries that carry an empty `OriginWireShape` (any write path that omits tagging) take the reader's untagged branch — a canonical-assuming reshape gate that re-encodes when the requesting ingress is `WireShapeOpenAIChat` or `WireShapeOpenAIResponses` against a non-Responses-native target. The same wire-shape gate exists for the streaming path: when the cached chunks were written under a different ingress shape, the proxy stamps a stream-hit origin on the request context, and `handleStreamWithSubscription` selects an explicit `NewChatCompletionsStreamEncoder` / `NewResponsesStreamEncoder` so the cached canonical chunks are re-encoded into the requesting ingress's SSE grammar instead of forwarding the writer's raw bytes.

`OriginWireShape` is stored at the storage layer as a TEXT HASH field on L2 (`origin_wire_shape`, written by `Client.StoreEntry`, returned by `FT.SEARCH ... RETURN`). The L2 index does NOT index it — every reshape decision is post-retrieval at the reader.

For the underlying `WireShape` typology and the routes table see [`endpoint-typology-architecture.md`](../foundation/endpoint-typology-architecture.md).

## Cache status fields on the traffic event

The audit record (and downstream `TrafficEventMessage`) carries six cache columns. Four are detail / drill-down only; one is the unified rollup; one points the admin UI at the underlying L2 entry.

| Field | Type | Producer |
|---|---|---|
| `CacheStatus` | enum `HIT` \| `MISS` | `DeriveCacheStatus(GatewayCacheStatus, ProviderCacheStatus)` — HIT iff gateway served (`hit` or `hit_inflight`) OR upstream reported a provider-side prompt cache hit. |
| `GatewayCacheStatus` | enum `hit` \| `hit_inflight` \| `miss` \| `skipped` | Stamped by the proxy at the lookup branch (extract HIT, semantic HIT, broker leader, broker joiner, or pre-lookup classifier). |
| `GatewayCacheSkipReason` | enum (16 values) | Set only when `GatewayCacheStatus = skipped`. Vocabulary: `disabled / no_cache / passthrough / not_cacheable / time_sensitive / oversize_for_embedding / valkey_unavailable / embedding_timeout / embedding_provider_error / embedding_dim_mismatch / semantic_search_error / semantic_search_timeout / semantic_reindex_in_progress / semantic_unavailable / embedding_circuit_open / poisoned`. |
| `GatewayCacheKind` | enum `extract` \| `semantic` | Distinguishes L1 hits (`extract`) from L2 hits (`semantic`). |
| `GatewayCacheL2EntryKey` | string | The L2 Redis HASH key (`<index>:<sha256[:16]>` over embedding text + scope + kind [+ provider/model]). Stamped only on rows where `GatewayCacheKind = semantic`. The admin UI uses this as the entry id the gateway's poison check will match against. |
| `ProviderCacheStatus` | enum `hit` \| `miss` \| `na` | Reports the upstream provider's own prompt-cache outcome from the upstream usage envelope. `na` on every gateway-served row (the upstream wasn't called). |

The unified `CacheStatus` is the only column the audit drawer exposes to UI filters. The four detail columns are drill-down only; their § 6.4 layout in [`cost-estimation-architecture.md`](../../services/ai-gateway/cost-estimation-architecture.md) is the source of truth for how the drawer renders them.

## Cost stamp on cache HIT

The cache short-circuits the upstream call but still emits a `traffic_event` row, so dashboards can show "spend if no cache" alongside "savings".

- `EstimatedCostUsd` — the would-have-paid cost at the model's current input/output USD-per-million prices. Computed via `estimator.Lookup(EndpointType)(BillableUnits{prompt, completion}, ModelPrices{...}).Total` on both stream HIT and non-stream HIT paths. Invariant of cache outcome.
- `GatewayCacheSavingsUsd` — set equal to `EstimatedCostUsd` on extract HIT and semantic HIT (this caller did not pay upstream).
- Token counts (`PromptTokens` / `CompletionTokens` / `TotalTokens` / `ReasoningTokens`) on a HIT come from the cached entry's usage envelope so the row carries the same totals the original upstream call produced.

**HIT_LIVE (joiner) cost clearing**. When the broker subscriber is a joiner, `EstimatedCostUsd` is reset to 0 and `GatewayCacheSavingsUsd` is set to the full would-have-cost — the leader (which stamps `miss`) owns the upstream spend; charging the joiner would double-count. Provider-side prompt-cache token / cost fields (`CacheReadTokens`, `CacheCreationTokens`, `CacheWriteCostUsd`, `CacheReadSavingsUsd`, `CacheNetSavingsUsd`) are cleared on the joiner row for the same reason.

**Quota reconcile is skipped** when `gatewayServed := GatewayCacheStatus ∈ {hit, hit_inflight}`. HIT and HIT_INFLIGHT both mean this caller paid no upstream cost; reconciling would inflate the quota counter against zero spend.

## Poisoning — negative-feedback path

Admins can mark a semantic cache HIT as a bad result via the audit drawer's thumbs-down. The mechanism:

- **Backing store**: Redis `SET` keys under namespace `nexus:l2:poison:<vkScope>:<entryKey>`, value `"1"`. TTL = `min(entry_remaining_ttl × 10, 30 days)`, default 24 h × 10 when the entry TTL is unknown.
- **Reader-side check**: `cache/semantic.Reader.Read` calls `PoisonList.IsPoisoned(ctx, entryKey, vkScope)` after every FT.SEARCH hit. A `true` result is treated as a miss and stamped `gateway_cache_skip_reason = poisoned`. A poison-list lookup error is fail-open — the hit still proceeds; a poison-list availability issue must not degrade normal cache ops.
- **Admin endpoint**: `POST /api/admin/cache/semantic-feedback` with body `{entryKey, vkScope, reason, ttlSeconds}`. The Control Plane's `redisPoisonAdder` writes the same `nexus:l2:poison:<vkScope>:<entryKey>` key so the gateway and the Control Plane share one namespace. IAM action: `admin:semantic-cache.update`. The handler also records the feedback in a process-local ring buffer (capacity 1000) so the audit drawer's "recent feedback" panel can render without a DB roundtrip.
- **Why `GatewayCacheL2EntryKey` exists**: the entry id the gateway's `IsPoisoned` check compares against is the L2 Redis HASH key, not the `traffic_event.id`. Stamping it on the audit row gives the admin UI the right token to post back.

## Spillstore

The cache layer does NOT delegate to the spillstore. The two systems are independent: spillstore archives request and response payload bytes for the `traffic_event` row when payload capture is enabled and the body exceeds an inline-byte cap; the cache stores upstream responses keyed for replay. See [`spillstore-architecture.md`](spillstore-architecture.md) for the spill path.

## Eviction + freshness

**L1 eviction** is pure Redis TTL — `SET key value EX <ttl>` at write time, default 1 h, runtime-tunable via Hub shadow. There is no in-process LRU on the gateway side; the eviction policy is whatever the Redis instance is configured with (`maxmemory-policy`).

**L2 eviction** is per-entry `PEXPIRE` set at write time, plus index-level lifecycle. The L2 config snapshot carries a `Fingerprint = sha256(provider:model:dim)` and an explicit `RedisIndexName` (e.g. `nexus:semantic-cache:v1`). When the fingerprint changes — embedding provider, model, or dimension swap — a blue/green index rotation drops the stale index and recreates with the new dimension. Stale entries are unreachable behind the new fingerprint TAG filter even before the index drop.

**Freshness — time-sensitive skip**. `cache/freshness.Detector` is a hot-swappable rule set evaluated over the canonical message stream. When `Cache.ApplyFreshnessRules()` is true AND `Detector.IsTimeSensitive(messages)` matches, `classifyCachePreLookup` returns `(skipped, time_sensitive)` so both L1 and L2 are bypassed. The detector's rule list is loaded from the Hub shadow and replaced atomically via `Detector.Reload` — no restart required. The default rule set seeds from `semantic_cache_config.time_sensitive_overrides` (the `tools/db-migrate/seed/fixtures/semantic_cache_config.json` fixture); there is no Go-side default, so if the seed never ran the rule list is empty and nothing is classified time-sensitive.

**Poisoning** (above) is the third eviction-adjacent path: a poisoned L2 entry remains in Valkey but is invisible to FT.SEARCH-driven reads until the poison key TTL elapses or the entry's own TTL evicts it.

## References

- `packages/ai-gateway/internal/cache/core/cache.go`
- `packages/ai-gateway/internal/cache/semantic/client.go`
- `packages/ai-gateway/internal/cache/semantic/lookup.go`
- `packages/ai-gateway/internal/cache/semantic/writer.go`
- `packages/ai-gateway/internal/cache/semantic/poison.go`
- `packages/ai-gateway/internal/cache/semantic/config_cache.go`
- `packages/ai-gateway/internal/cache/stream/broker.go`
- `packages/ai-gateway/internal/cache/freshness/detector.go`
- `packages/ai-gateway/internal/ingress/proxy/proxy.go`
- `packages/ai-gateway/internal/ingress/proxy/proxy_cache.go`
- `packages/ai-gateway/internal/ingress/proxy/proxy_l2.go`
- `packages/ai-gateway/internal/execution/canonicalbridge/bridge.go`
- `packages/ai-gateway/internal/execution/canonicalbridge/api.go`
- `packages/ai-gateway/internal/platform/audit/audit.go`
- `packages/shared/transport/mq/messages.go`
- `packages/control-plane/internal/ai/cache/handler/semantic_feedback.go`
- `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md`
- `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md`
- `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md`
- `docs/developers/architecture/cross-cutting/storage/spillstore-architecture.md`
