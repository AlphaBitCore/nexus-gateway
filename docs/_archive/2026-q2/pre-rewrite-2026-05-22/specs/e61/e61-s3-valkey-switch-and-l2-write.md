# E61-S3 — Valkey Switch + L2 Write Path

> Story: e61-s3
> Epic: 61
> Status: Draft
> Requirements: `docs/developers/specs/e61/e61-smart-response-cache.md` §FR-2.1–2.10 (write half), §FR-3, §FR-5 (Valkey infra)
> Architecture: `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` §3 (full semantic-tier picture)
> Blocked by: e61-s1 (freshness detector), e61-s2 (schema + audit constants), e61-s2b (inputstaging), e61-s5 (embedding adapter)
> Blocks: e61-s4 (read path uses the index this story creates)

## User Story

As a Gateway Operator, I want the prod Valkey instance to swap from `redis:7-alpine` to `valkey/valkey:8-alpine` + valkey-search, and the L2 write path wired so that every L1 miss that produces a cacheable response ALSO writes a semantic entry to the per-(embedding_provider, embedding_model) index, so that future requests with semantically-similar prompts can be served from L2.

## Tasks

### T1 — Infrastructure swap

- T1.1 `docker-compose.yml`: change `image: redis:7-alpine` → `image: valkey/valkey:8-alpine` and add `command: valkey-server --loadmodule /opt/valkey/lib/valkey-search.so` (or whatever the module path is — verify at S3 implementation time when we pull the actual image).
- T1.2 Verify valkey-search module exports the RediSearch-compatible API surface (`FT.CREATE`, `FT.SEARCH`, `FT.DROPINDEX`, vector type). If module API differs, S3 implements an internal abstraction layer in `cache/semantic/client.go` so the rest of the gateway is decoupled.
- T1.3 Update `scripts/dev-start.sh` if it pulls Redis directly — should be just docker-compose.
- T1.4 Document the production switch in `docs/operators/ops/runbooks/e61-valkey-migration.md`: backup steps, downtime estimate (~30s, single instance), rollback (revert docker-compose image + restart).

### T1a — L1 ConfigStore + ConfigCache + fingerprint-driven invalidate job

> Added 2026-05-19 per E61 architecture review (§0 of E61-S2). The
> semantic cache embedding model lives in the singleton
> `semantic_cache_config` table (created in S2 T1.2). This story owns
> the in-process cache + the fingerprint-diff invalidate hook.

- T1a.1 `packages/shared/storage/configstore/semantic_cache.go` (new) — `SemanticCacheConfig` struct (`EmbeddingProviderID`, `EmbeddingModelID`, `EmbeddingDimension`, `EmbeddingFingerprint`, `RedisIndexName`, `Enabled`) + `SemanticCacheStore.Load` / `.Save`. Recompute fingerprint inside `Save` so callers can't forget. **Direct copy of `aiguard.go`'s shape** — same SQL patterns, same pgx interfaces. Target 100% coverage via pgxmock.
- T1a.2 `packages/ai-gateway/internal/cache/semantic/config_cache.go` (new) — in-process cache with `atomic.Pointer[SemanticCacheConfig]`, `Get(ctx)` hot-path, `Invalidate()` for shadow-listener refresh. Mirrors `packages/ai-gateway/internal/policy/aiguard/config_cache.go` 1:1 so a reviewer reading both reads the same shape.
- T1a.3 `packages/ai-gateway/internal/cache/semantic/index_lifecycle.go` (new):
    - On every `ConfigCache.Refresh()` (called by the shadow listener after Hub pushes a new `semantic_cache.config` payload), compare stored vs incoming `EmbeddingFingerprint`.
    - If different and the new fingerprint is non-empty (i.e., L1 has a valid (provider, model, dim)): emit a one-shot `semantic_cache.invalidate_all` event into the Hub job queue.
    - Hub-side job consumer (`packages/nexus-hub/internal/jobs/defs/semanticcacheflush/`, new) runs `FT.DROPINDEX <oldIndexName>` then `FT.CREATE <newIndexName>` against the cluster's Valkey. New index uses the new fingerprint's dim. **Idempotent** (DROPINDEX returns OK on missing).
    - Audit row stamped: `traffic_source=AI_GATEWAY`, `action=semantic_cache_flush`, with the old + new fingerprint in details.
- T1a.4 Hub `OnConfigChanged` callback for `semantic_cache.config` shadow key (S2 also adds the configKey to `packages/shared/schemas/configkey/`): on push, reload ConfigCache → step T1a.3 may fire.
- T1a.5 Admin API `GET/PUT /api/admin/semantic-cache/config` lives in `packages/control-plane/internal/ai/cache/handler/semanticcache.go` (new). PUT validates: `embedding_provider_id` must exist + have `endpointType=embedding`; `embedding_model_id` must belong to that provider. IAM-gated on `iam.ResourceSemanticCache.Action(iam.VerbUpdate)`.

### T2 — Semantic cache package

- T2.1 Create `packages/ai-gateway/internal/cache/semantic/` with: `client.go`, `index.go`, `writer.go`, `singleflight.go`, `metrics.go`, `config_cache.go` (see T1a.2), `index_lifecycle.go` (see T1a.3), `*_test.go`, `doc.go`.
- T2.2 `client.go` wraps the Valkey client and exposes:
    ```go
    type Client struct { rdb redis.UniversalClient; log *slog.Logger }
    func New(rdb redis.UniversalClient, log *slog.Logger) *Client
    func (c *Client) EnsureIndex(ctx context.Context, indexName string, dim int) error
    func (c *Client) StoreEntry(ctx context.Context, entry *StoreInput) error
    func (c *Client) Lookup(ctx context.Context, lookup *LookupInput) (*Entry, error)  // implemented in S4
    func (c *Client) DropIndex(ctx context.Context, indexName string) error  // called by T1a.3 flush job
    ```
- T2.3 `index.go` builds the FT.CREATE schema using the **fleet-wide index name from L1** (`SemanticCacheConfig.RedisIndexName`, default `nexus:semantic-cache:v1`):
    ```
    FT.CREATE <indexName> ON HASH PREFIX 1 "<indexName>:"
      SCHEMA
        vector            VECTOR HNSW 6 DIM <L1.dim> TYPE FLOAT32 DISTANCE_METRIC COSINE
        upstream_provider TAG
        upstream_model    TAG
        vk_scope          TAG
        response_body     TEXT NOINDEX
        usage             TEXT NOINDEX
        cached_at         NUMERIC
        response_kind     TAG               # "stream" | "response"
    ```
    The (provider, model) suffixing from the previous draft is dropped — one fleet-wide index keyed off L1's fingerprint. Cross-route hits work because every route shares the same embedding space.
- T2.4 Index creation is idempotent — if the index exists, log debug and continue. The flush job (T1a.3) is the only place that deliberately drops + recreates.
- T2.5 Entry key naming: `keyPrefix + uuid-v4`. The vector field stores the embedding as a `FLOAT32` blob (big-endian).

### T3 — Embedding singleflight

- T3.1 `singleflight.go` wraps `golang.org/x/sync/singleflight` (already in `shared` deps) keyed by `SHA-256(embedding_input)`.
- T3.2 The semantic client exposes `Embed(ctx, input string) ([]float32, embeddingCostUsd float64, err error)`. Inside:
    - Compute key.
    - `singleflight.Do(key, func() { call embedding adapter })`.
    - Return shared result + cost.
- T3.3 Concurrent embed callers with identical input share one adapter call. The result's cost is divided proportionally? — NO. Per the cost-accounting principle: only the leader call's cost is real. Joiners are stamped with `embedding_cost_usd=0` (they didn't pay). Document this in `singleflight.go` and audit the `traffic_event.embedding_cost_usd` write site to use the per-request value.

### T4 — Write path wiring

- T4.1 In `packages/ai-gateway/internal/cache/stream/broker.go` `writeCache` — after the existing `StoreStream` / `StoreResponse` call:
    - If `route.response_cache_policy.semantic.enabled == true`:
    - AND request was NOT marked time-sensitive (the broker has access to the audit Record):
    - AND request was NOT cross-format-dry-run, etc.
    - Then build a `*semantic.StoreInput` with:
        - `Embedding` — produced via `semantic.Client.Embed(ctx, inputstaging.Plan(...).Messages)`. If `OverflowKind != none`, stamp `GatewayCacheSkipReasonOversizeForEmbedding` and skip the write.
        - `UpstreamProvider`, `UpstreamModel` — from `b.meta`.
        - `VkScope` — from the audit record.
        - `ResponseBody` — same canonical bytes the extract entry stored.
        - `Usage` — same usage object.
        - `ResponseKind` — "stream" or "response".
        - `TTL` — same TTL as the extract entry.
    - Call `semantic.Client.StoreEntry(ctx, input)`.
- T4.2 The semantic write is best-effort: a failure logs at WARN, increments a metric, but does NOT fail the extract write or the response delivery.
- T4.3 Wrap the embedding + write in a goroutine bounded by a context-with-timeout (5s) so writeCache's caller path is not blocked.

### T5 — Embedding cost stamping

- T5.1 The leader of the embedding singleflight stamps `traffic_event.embedding_cost_usd` with the per-call cost. Joiners stamp 0.
- T5.2 Cost = `(input_tokens / 1_000_000) * embeddingModel.InputPricePerMillion`. The price comes from the embedding Model row.
- T5.3 The stamp happens in the audit Record before `audit.Write` runs.

### T6 — Metrics

- T6.1 New Prometheus instruments registered in `packages/ai-gateway/internal/cache/semantic/metrics.go`:
    - `nexus_cache_l2_writes_total{outcome="ok|skip_time_sensitive|skip_oversize|error|too_large"}`
    - `nexus_cache_l2_entries_size_bytes` histogram.
    - `nexus_cache_embedding_calls_total{provider,model,outcome="ok|error|coalesced"}`
    - `nexus_cache_embedding_latency_seconds` histogram.
    - `nexus_cache_embedding_cost_usd_total` counter.

### T7 — Tests

- T7.1 miniredis-backed test with vector-extension stub: write path produces an HSET to the expected key with the expected fields.
- T7.2 Singleflight test: 100 concurrent `Embed` calls with the same input → exactly 1 underlying adapter call observed.
- T7.3 Overflow skip test: oversize embedding input → write does not fire, audit Record carries the skip reason.
- T7.4 Time-sensitive skip test: detector returns true → write does not fire.
- T7.5 Embedding adapter failure: write logs warn, broker continues, extract write still happens.
- T7.6 Coverage ≥95%.

## Acceptance Criteria

- A1: After Valkey switch, `docker compose up` starts cleanly; ai-gateway connects to Valkey and creates indexes on startup for each enabled (embedProv, embedModel) pair.
- A2: An L1 miss with semantic enabled produces an L2 HSET to the right index/key with all required fields.
- A3: Concurrent identical prompts result in exactly one embedding call; joiners get 0 stamped on `embedding_cost_usd`.
- A4: Time-sensitive prompts stamp `skip_time_sensitive` and skip L2 write.
- A5: Oversize embedding input stamps `oversize_for_embedding` and skips L2 write; L1 write is unaffected.
- A6: All new Prometheus metrics show up in `/metrics`.
- A7: Unit-test coverage ≥95%.

## Out of Scope (S3)

- The L2 read path — that's S4.
- Embedding adapter implementation — that's S5 (this story consumes a working `embedding.Adapter` interface; if S5 is in flight, a fake adapter is acceptable for S3 tests).
- Cache Settings UI — S6.
