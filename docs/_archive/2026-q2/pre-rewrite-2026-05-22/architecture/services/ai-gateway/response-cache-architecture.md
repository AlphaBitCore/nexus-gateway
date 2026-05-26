---
doc: response-cache-architecture
area: service
service: ai-gateway
tier: 1
updated: 2026-05-20
---

# Response Cache Architecture (E35 + E61)

> **Tier 2 architecture doc.** Read when touching `packages/ai-gateway/internal/cache/` response cache code, route-level cache policy, or response-cache analytics. Distinct from **prompt cache** (`prompt-cache-architecture.md`): response cache stores the **entire** response keyed on the canonical request; prompt cache stores prefix KV reuse hints.
>
> Since E61 (2026-05) the gateway response cache is **two cooperating tiers in one Valkey instance**:
>
> - **Extract** (L1) — exact-match: canonical-request SHA-256 → full response. The historical response cache.
> - **Semantic** (L2) — approximate-match: embedding KNN → full response when cosine similarity ≥ threshold. Activated by E61.
>
> A request walks Extract first; on miss + freshness OK + admin opt-in, walks Semantic; on miss falls through to the broker → upstream.

The response cache short-circuits the entire upstream call when the canonical request maps to a previously-served response. Most useful for deterministic prompts (embeddings, structured extractors, RAG retrieval) for the **Extract** tier, and for paraphrased / semantically-similar workloads (summarization, idea generation, creative writing) for the **Semantic** tier.

---

## 1. The three caches at a glance

| | Gateway Extract Cache (L1) | Gateway Semantic Cache (L2) | Provider Prompt Cache |
|---|---|---|---|
| **Stores** | full response bytes (exact-match) | full response bytes (approximate-match) | KV-cache hints / reuse markers |
| **Saves** | full upstream round-trip | full upstream round-trip | prefix re-tokenisation / compute |
| **Keyed on** | canonical request hash | canonical request embedding | prefix hash (first N messages) |
| **Hit reduces** | latency to ~0, cost to 0 | latency to embedding-call + KNN, cost to embedding | TTFT, input-token cost |
| **Default per route** | enabled | **disabled** (admin opt-in) | enabled |
| **Default TTL** | 5m route-configurable | mirrors Extract TTL | tenant-configurable, default 15m |
| **Backed by** | Valkey 8.x | Valkey 8.x + valkey-search (HNSW) | Provider-side |

All three can be active on the same request; the order of evaluation is Extract → Semantic → Provider.

## 2. Extract tier (L1)

The historical exact-match cache. Unchanged shape since E35-S1.

### 2.1 Cache key

```
extract_key = "nexus:cache:" + SHA-256(
  "v3\nprovider=<upstream_provider>\nmodel=<upstream_model_id>\nallowlist=<headerAllowlistHash>\nbody=" + canonicalize_json(prepared_body)
)
```

Fields included via the `prepared_body` (output of `Adapter.PrepareBody`, NOT raw client body):
- `model_id`, `messages`, `tools`, `tool_choice`, `temperature`, `top_p`, `response_format`.

Fields excluded:
- `stream` — stream vs non-stream stored on disjoint hashes via per-entry schema discriminator (`stream/v1` vs `response/v1`).
- `metadata`, `stream_options` — caller-supplied opaque, no response effect.

`vary_by` modifiers on the route policy fold into the canonicalized body via per-policy nonce keys when `vary_by_user=true` or `vary_by_vk=true`.

### 2.2 Read path

1. Compute `extract_key`.
2. `GET extract_key` against Valkey.
3. On hit: deserialise → stamp `GatewayCacheStatus=hit`, `GatewayCacheKind=extract` → replay via `handleStreamHit` / `handleNonStreamHit`.
4. On miss: continue to Semantic (§3) if eligible, else broker.

### 2.3 Write path

After a successful (non-error, non-refusal) upstream completion:

1. Compute `extract_key`.
2. Check route's `response_cache_policy.extract.enabled`. If disabled, skip.
3. Check response shape — error / refusal responses are NOT cached.
4. `SET extract_key <serialised entry> EX ttl`.

## 3. Semantic tier (L2) — new in E61

### 3.1 When the semantic lookup fires

The semantic lookup fires only when ALL of:
- Extract miss.
- `response_cache_policy.semantic.enabled=true` for the resolved route.
- Pre-cache `time-sensitive-prompt` check passed (§4).
- `inputstaging.Plan` produced a fittable embedding input (§3.4).
- Request-stage hooks already approved the incoming request.

The last condition is binding — a semantic hit serves a **different** prompt's response, so compliance must have already accepted the **incoming** prompt. The existing handler order in `proxy.go` enforces this (request hooks at the request stage long before any cache decision); E61 SDD acceptance criteria pins it.

### 3.2 Embedding via the Provider system (fleet-wide singleton, mirror of AI-Guard)

Embedding is one more workload running through the existing `Provider` / `Adapter` infrastructure with a new `endpointType=embedding`. All internal AI calls (routing decision LLM, ai-guard, embedding) inject through the Provider system rather than a parallel framework — cross-reference [[project_local_inference_server_direction]] memory.

**The embedding provider + model are a fleet-wide singleton** stored in the `semantic_cache_config` table (mirror of `ai_guard_config`). Per-route Cache Settings carries policy (`enabled`, `threshold`, `embed_strategy`, `vary_by`, `allow_cross_model`) but **does NOT** carry embedding-model choice. Reasons:

- Vector spaces don't compose. text-embedding-3-small (1536D) and bge-small (384D) are mathematically incompatible; cosine similarity across spaces is undefined.
- A single Redis Vector index needs a fixed `DIM` at create time. Per-route embedding choice would force one index per (model, dim) tuple — operational nightmare for a fleet-wide decision.
- The two-layer split (L1 infrastructure singleton + L2 per-route policy) mirrors AI-Guard's settled architecture; reviewers find the same shape.

Two reference deployments ship out of the box:
- **OpenAI cloud** — `provider=openai`, `model=text-embedding-3-small` (1536d, 8191-token context).
- **Local OpenAI-compatible server** — `provider=local-inference`, `baseURL=http://<host>:<port>`, any model exposed by vLLM / Ollama / LiteLLM / custom.

Admin picks the embedding provider+model on the dedicated **Cache Embedding Settings** page (`Settings → Cache Embedding`, mirror of `Settings → AI Guard`). Per-route Cache Settings displays the active model as a read-only chip with a deep-link to this page.

The `semantic_cache_config` singleton row carries:

| Column | Purpose |
|---|---|
| `id` ("singleton") | Sentinel PK; one row only — Nexus is single-tenant on-prem. |
| `embedding_provider_id` | FK to `Provider` (nullable until first config) |
| `embedding_model_id` | FK to `Model` (nullable until first config) |
| `embedding_dimension` | Cached dim for fast index creation |
| `embedding_fingerprint` | `sha256(provider:model:dim)` — drives index lifecycle |
| `redis_index_name` | Default `nexus:semantic-cache:v1`; admin can override on rare migrations |
| `enabled` | Fleet-wide kill switch (incident response — flip false to disable semantic cache everywhere instantly) |
| `updated_at`, `updated_by` | Audit |

**Effective-enabled cascade** the L2 reader evaluates per request:

```
effective_semantic_enabled(route) =
  semantic_cache_config.enabled            AND
  semantic_cache_config.embedding_model_id IS NOT NULL  AND
  route.response_cache_policy.semantic.enabled
```

L1 enabled is the fleet-wide kill switch; L2 enabled is per-route opt-in.

### 3.3 Singleflight on embedding calls

Identical concurrent prompts must not produce N redundant embedding calls. The semantic-cache layer wraps `Adapter.Execute` in a singleflight keyed by `SHA-256(embedding_input)` — same pattern the broker uses for upstream call coalescing.

**Cancellation semantics**:
- The leader's embed call runs on an **independent context** with a hard timeout (default 5s — see §3.11). A single client disconnect does NOT cancel the underlying call. This prevents one impatient client from invalidating work that 99 others are waiting for.
- Joiners share the leader's result (value or error). If the leader fails, every joiner gets the same error and falls through to the broker (no L2 hit, no L2 write).
- If the leader exceeds the hard timeout, every joiner receives a "deadline exceeded" error and falls through to broker. The leader's in-flight HTTP request is aborted at the timeout boundary.
- Joiners that disconnect early stop waiting (their own context observed); the leader continues so the work isn't wasted.

**Circuit breaker on embedding provider failures.** A persistent embedding outage would burn 100ms per L2-eligible request even though every call is doomed to timeout — at 100 RPS that's 10s of useless wait-time per second of wall clock. A circuit breaker protects the latency budget:

- **State machine**: `closed` (normal) → `open` (skip-all) → `half_open` (probe) → `closed`.
- **Trip condition**: 10 consecutive failures (any of: timeout, 5xx, dim-mismatch) within 60s → trip to `open`.
- **Open behaviour**: every L2-eligible request stamps `GatewayCacheSkipReason=embedding_circuit_open` and falls through to broker WITHOUT firing an embedding call. Latency overhead in this state: <1ms.
- **Half-open**: after 30s in `open`, allow 1 probe call. Success → `closed`. Failure → reset to `open` for another 30s.
- **Per-(provider, model) scope**: the breaker is keyed on the L1 embedding provider+model so swapping to a different model in S6c clears the trip state (new endpoint, new chance).
- **Observability**: `nexus_cache_embedding_circuit_state{provider, model}` gauge (0=closed, 1=open, 2=half_open); `nexus_cache_embedding_circuit_trips_total` counter.

Adds one more skip-reason constant: `GatewayCacheSkipReasonEmbeddingCircuitOpen`.

### 3.4 Input staging (oversize handling)

Embedding models have small context windows (text-embedding-3-small: 8191 tokens; bge-small: 512). Large conversation histories cannot be embedded verbatim. The semantic layer plans the embedding input via the shared `inputstaging` primitive (cross-reference [[project_inputstaging_shared_primitive]] memory):

```go
plan := inputstaging.Plan(canonicalMessages, embeddingModel.ContextLimit, policy.EmbedStrategy, reserveOutput)
```

Per-route `embed_strategy` (default `system_plus_last_user`):
- `last_user` — embed only the final user message.
- `system_plus_last_user` — system prompt + final user message (default; balances "what is being asked" with "the agent's persona").
- `recent_turns` — most recent N turns (N derived from contextLimit).
- `head_plus_tail` — first X% + last Y% of the conversation.
- `full_truncated` — head-truncated to contextLimit (legacy mode).

If `plan.OverflowKind != none` (even the strategy-truncated input exceeds the embedding model's context), the semantic layer stamps `GatewayCacheSkipReasonOversizeForEmbedding`; neither lookup nor write fires. Extract path is unaffected.

### 3.5 Index layout — fleet-wide singleton + fingerprint-driven lifecycle

**One Redis Vector index per cluster**, named from `semantic_cache_config.redis_index_name` (default `nexus:semantic-cache:v1`):

```
FT.CREATE <indexName> ON HASH PREFIX 1 "<indexName>:"
  SCHEMA
    vector            VECTOR HNSW 12 DIM <L1.embedding_dimension> TYPE FLOAT32 DISTANCE_METRIC COSINE
                          M 16 EF_CONSTRUCTION 200 EF_RUNTIME 10
    upstream_provider TAG
    upstream_model    TAG
    vk_scope          TAG
    response_kind     TAG               # "stream" | "response"  — filter on lookup
    fingerprint       TAG               # mirror of L1.embedding_fingerprint at write time
                                        # — read path filters reindex-in-flight stragglers
    cached_at         NUMERIC
```

`response_body`, `usage`, and `upstream_headers` are stored on the HASH (HSET) alongside the indexed fields but **are NOT declared in the FT.CREATE schema** — Valkey 8.x's open-source search module rejects `TEXT NOINDEX` ("Invalid field type … Unknown argument `TEXT`"); see `packages/ai-gateway/internal/cache/semantic/client.go:100-105`. The reader retrieves them by reading the hash directly after the KNN match, not via FT.SEARCH RETURN clauses. On lookup the caller adds `@response_kind:{<stream|response>}` to the filter so a streaming client never gets a `response_kind=response` entry replayed and vice versa.

The `fingerprint` tag protects against reindex races (§3.5.2). Writes stamp the L1 fingerprint observed at HSET time; reads filter on the current L1 fingerprint. An entry whose fingerprint no longer matches the active L1 (because admin swapped models between write and read) is invisible to the lookup — same effect as if it were already evicted, but with no race-window where stale vectors leak.

A single fleet-wide index is sound because the embedding model is a fleet-wide singleton (§3.2): every entry shares one vector space + one dimension. Cross-route hits work because every route shares the same embedding.

**Fingerprint-driven lifecycle (blue/green via versioned index name).** Same-name DROPINDEX + CREATE is **not** atomic — between the two commands, FT.SEARCH against the index name returns "no such index" and every in-flight L2 lookup fails. The fix is to never reuse an index name.

When admin changes the L1 (provider, model) selection through the Cache Embedding Settings page (E61-S6c), `embedding_fingerprint = sha256(provider:model:dim)` is recomputed inside the ConfigStore `Save`, AND the index name is bumped to the next version (e.g., `nexus:semantic-cache:v1` → `nexus:semantic-cache:v2`). The flush job is a 3-step blue/green:

1. **CREATE** the new index with the new name + new dimension.
2. **SWAP** — `semantic_cache_config.redis_index_name` flips to the new value (ConfigCache observes the change; reads + writes immediately route to the new index, which is empty).
3. **DROPINDEX** the old index (+ optionally `UNLINK` old hash entries; default behaviour leaves them to TTL out to avoid blocking on millions of DEL ops).

Between step 1 and step 2 the old index is fully alive. Between step 2 and step 3 the new index is empty but live; readers see 0 results (treated as miss → broker fallthrough, no error). At no point is FT.SEARCH issued against a non-existent index name.

If admin reverts before step 3 completes (rare), step 3 is skipped and the next save just bumps to a fresh `v3` (we never reuse a name). The version counter is monotonic.

This mirrors `ai_guard_config.backend_fingerprint` flush semantics 1:1 but with versioned-name atomicity. Admin sees a Save-time confirmation modal warning that the existing semantic-cache entries become unmatchable until the next eligible request seeds the fresh index — a few minutes of warm-up, not a permanent loss.

#### 3.5.2 Reindex-race semantics (writes/reads during a blue/green swap)

With the blue/green flow (§3.5), at no point does an index name disappear and reappear — FT.SEARCH is always against either the old (alive, full) or the new (alive, empty) index. So the failure mode "FT.SEARCH says no-such-index" is eliminated. The remaining race is narrower:

**Window: after CREATE-new, before SWAP** — the ConfigCache in every ai-gateway pod still points at the old index name. Writes go to the old index; reads go to the old index. Behaviour is identical to "no swap in progress". No correctness impact.

**Window: after SWAP, before all ConfigCaches refresh** — a single ai-gateway pod's ConfigCache may still hold the old index name (ConfigCache refresh interval ≈ 1s). That pod's writes during the lag window land in the **old index**, which is still alive (DROPINDEX runs only at step 3). Those entries become unreachable as soon as DROPINDEX fires. Bounded loss, no correctness violation; the worst case is "the first ~1s of post-swap writes are wasted."

**Belt-and-suspenders: `fingerprint` tag** — every HSET stamps `fingerprint = <fingerprint observed at write time>`. Every FT.SEARCH filters on `@fingerprint:{<current L1 fingerprint>}`. So even if the index-name SWAP propagates faster than the ConfigCache refresh (rare ordering), a read that hits the new (empty) index can't accidentally surface a stale entry from the old fingerprint. Defence-in-depth against ConfigCache lag.

- **Flush job retry** — the Hub-side consumer wraps CREATE-new + SWAP + DROP-old in a single job; on retry the CREATE step is idempotent (returns "index already exists" → success), SWAP is idempotent (compare-and-swap on `redis_index_name`), DROP is idempotent (DROPINDEX returns OK on missing). Audit row written once per successful completion.
- **Flush job failure** — if Valkey is unreachable, the job is requeued with exponential backoff up to 5 retries (10s, 30s, 90s, 5m, 15m). After max retries: dead-letter + alert (`nexus_semantic_cache_flush_failed_total`). Meanwhile the gateway treats fingerprint mismatch as "L2 is in transition" and stamps `GatewayCacheSkipReason=semantic_reindex_in_progress` on lookups + skips writes. Operators recover by manually triggering `POST /api/admin/semantic-cache/reindex` (S6c provides the button).

Why not per-(embedProv, embedModel) indexes (the alternative considered and rejected):
- Forced multiple indexes alive simultaneously during transitions → memory cost.
- Each route would need to know which index to read/write → policy surface grows.
- TTL-out of "old" indexes during the transition window is unbounded — entries written under model A may live for hours after admin switched to model B, served via cosine similarity that's already meaningless against new vectors. Fingerprint-flush is decisive.

### 3.6 Upstream-model scoping (cross-model toggle)

Default: semantic match is scoped to **same upstream (provider, model)** via the `upstream_provider` + `upstream_model` tag filter. Streaming responses are ALWAYS scoped this way (StreamEntry stores native RawBytes; cross-vendor replay would require re-transcoding).

Per-route `semantic.allow_cross_model=true` removes the upstream-model filter (still keeps upstream-provider, or drops both — admin choice). Cross-model is technically supported by the canonical bridge (`canonicalbridge.ResponseCanonicalToIngress` handles ingress reshape), but carries real risks the admin must accept:
- Cost accounting drift — `gateway_cache_savings_usd` reports avoided spend on the *requested* model's pricing, not the *cached* model's.
- Reasoning / tool-call fidelity loss — provider-extension fields (`nexus.ext.*`) may not survive cross-vendor canonicalization.
- Routed-model mismatch — a user routed to "the best reasoning model" can receive "the cheapest chat model's" cached response.

The Cache Settings UI carries an explicit warning when admin enables `allow_cross_model`.

### 3.7 vary_by scoping

Default `semantic.vary_by="vk"` — semantic match limited to entries cached under the same Virtual Key. This is **stricter** than the L1 default (which allows cross-VK by default for deterministic-prompt reuse) because semantic similarity opens a new attack surface: a malicious user can craft prompts that semantically match a victim's cached responses. Per-VK isolation closes this.

If multiple humans share one VK, that isolation collapses — they semantically match each other's cached responses. The UI carries an explicit warning: shared VKs are a single user from the cache's point of view.

Other accepted values: `org`, `user`, `none`. `vk` is the recommended default; admin can widen with explicit choice.

### 3.8 Read path

1. (Extract miss already established; freshness check passed; hooks approved.)
2. `embedding_input := inputstaging.Plan(...)` → if overflow, skip.
3. `embedding := embed(embedding_input)` via Provider/Adapter, singleflighted.
4. FT.SEARCH query:
    ```
    FT.SEARCH <indexName>
      "(@upstream_provider:{<p>}
        @upstream_model:{<m>}
        @vk_scope:{<v>}
        @response_kind:{<stream|response>}
        @fingerprint:{<current L1 fingerprint>})
       =>[KNN 1 @vector $vec AS __vector_score]"
      PARAMS 2 vec <embedding>
      SORTBY __vector_score
      DIALECT 2
    ```
5. If best result `cosine_similarity ≥ threshold` → stamp `GatewayCacheStatus=hit`, `GatewayCacheKind=semantic` → replay via `handleStreamHit` / `handleNonStreamHit` with the cached entry (chunks for stream, body for non-stream).
6. Else → fall through to broker.

Per §3.5, the lookup filter MUST include both `response_kind` (so streaming clients don't replay non-stream entries) and `fingerprint` (so stale entries written under an old embedding model are invisible during reindex transitions).

Latency budget: see §3.11 for the full performance subsection.

### 3.9 Write path

1. (Extract write already happened on the leader's terminal frame.)
2. If `semantic.enabled` for the route AND freshness OK AND embedding-input fittable:
3. `embedding := embed(embedding_input)` (reuses singleflight handle from §3.8 when present).
3a. **Size cap check**: serialise candidate entry (response_body + ChunkRecord array if streaming + headers); if size > `semantic.maxEntryBytes` (default **256 KiB** — stricter than L1's 1 MiB because L2 stores per-entry vector + tag fields with overhead; oversize entries are usually low-reuse "long structured output" anyway): stamp `nexus_cache_l2_writes_total{outcome="too_large"}` and skip the write. L2 over-write protection mirrors L1 `cache.ErrCacheEntryTooLarge`.
4. HSET to `<L1.RedisIndexName>:<uuid-v4>` with fields:
    ```
    vector            <FLOAT32 BE blob>
    upstream_provider <p>
    upstream_model    <m>
    vk_scope          <v>
    response_kind     <stream|response>
    fingerprint       <current L1 fingerprint observed at write time>
    response_body     <canonical bytes for non-stream;
                       JSON-serialized []ChunkRecord for stream>
    usage             <JSON-serialized provcore.Usage>
    upstream_headers  <JSON-serialized map[string][]string, allowlisted>
    cached_at         <unix epoch>
    ```
5. `EXPIRE <key> ttl` — L2 TTL mirrors Extract TTL for lifecycle alignment.

The `<uuid-v4>` suffix prevents key collisions when admin temporarily overrides `redis_index_name` to migrate; old entries under the old name TTL out independently. The `fingerprint` field is the reindex-race guard (§3.5.2).

### 3.10 Hit replay path is shared with Extract

A semantic hit and an extract hit funnel into the same `handleStreamHit` / `handleNonStreamHit` routines. The semantic-tier code reads the L2 hash and constructs an in-memory shape compatible with the L1 entry types:

- `response_kind=response` → `*core.ResponseEntry{CanonicalResponse: <response_body bytes>, Usage, UpstreamHeaders, …}`.
- `response_kind=stream` → `*core.StreamEntry{Chunks: <JSON-decoded ChunkRecord array>, Usage, UpstreamHeaders, …}`.

From `handleNonStreamHit` / `handleStreamHit`'s perspective the entry could have come from either tier; the only differences:
- `GatewayCacheKind` stamp (`extract` vs `semantic`).
- The entry was selected by an approximate match — the response originally answered a *different* (but similar) prompt. Compliance has approved the *incoming* prompt before lookup (§3.1).

The egress reshape via `canonicalbridge.ResponseCanonicalToIngress` runs the same way for both.

### 3.11 Performance budget

Total L2-lookup latency overhead on the L1-miss path, with hard timeouts and fallback behaviour:

| Phase | Budget | Hard timeout | Timeout fallback |
|---|---|---|---|
| `inputstaging.Plan` | <1ms | n/a (in-process) | n/a |
| `embedding singleflight + HTTP` | 30–50ms (OpenAI cloud) / 10–30ms (local) | 5s (`singleflight.go:26 defaultEmbedTimeout`) | Stamp `GatewayCacheSkipReason=embedding_timeout` → fallthrough to broker → no L2 write |
| `FT.SEARCH` | 1–3ms at 10k entries; 5–15ms at 1M entries | 20ms | Log warning + fallthrough to broker → no L2 hit attempt; L2 write still fires after upstream |
| Entry decode + replay setup | <2ms | n/a | n/a |
| **p95 budget overall on the happy path** | **~30–60ms** (mostly the embedding round-trip) | — | — |

HNSW parameters (defaults emitted by the `FT.CREATE` call site at `packages/ai-gateway/internal/cache/semantic/client.go:106-124`; tunable on the L1 singleton config when valkey-search exposes them):

| Param | Default | Rationale |
|---|---|---|
| `M` | 16 | Standard for ≤1M entries; trade-off between memory and recall. |
| `EF_CONSTRUCTION` | 200 | Build-time recall — higher = better quality, slower writes. |
| `EF_RUNTIME` | 10 | Query-time recall — k=1 only, so EF_RUNTIME=10 is sufficient. |

The exact `FT.CREATE` invocation (HNSW 12 attributes, dim from the embedder, `DISTANCE_METRIC COSINE`, `TYPE FLOAT32`) is constructed in `client.go:106-124`; the canonical schema string is in the comment block at `client.go:68-71` so the doc + code stay in lockstep.

The L2 path never blocks the L1 miss → broker path. If anything along the embed-or-search chain stalls, the gateway proceeds to upstream — semantic cache is best-effort, not load-bearing.

## 4. Time-sensitive prompt skip — new in E61

Before any cache decision, the prompt goes through a freshness check (`packages/ai-gateway/internal/cache/freshness/`). When the prompt is time-sensitive (asks about current/changing state), **both** Extract and Semantic skip — lookup AND write — so neither tier serves stale content nor poisons future lookups.

### 4.1 Detection

Hub-shadow-pushed rule list (configKey `response_cache.time_sensitive_patterns`). Each rule:

```yaml
- id: stock-current
  keywords: [当前股价, current stock price, latest price]
  require_question_mark: true
  require_entity: true       # at least one named entity (ticker / company / number)
  languages: [zh, en]
```

Pure keyword match is NOT sufficient — pattern requires keyword + question structure + entity reference to avoid false positives like "How does DI work now?" where "now" is a discourse particle.

### 4.2 Skip semantics

When a rule fires:
- `GatewayCacheStatus = skipped`.
- `GatewayCacheSkipReason = time_sensitive`.
- Neither L1 lookup nor L1 write fires.
- Neither L2 lookup nor L2 write fires.
- Request proceeds straight to broker → upstream.

The skip applies to both write AND lookup so a time-sensitive prompt can't be poisoned into the cache by a prior non-time-sensitive variant of itself.

### 4.3 Default rule set

Ships with Hub seed:
- Time references (ZH+EN): 现在/当前/今天/最新/now/today/latest/current.
- Financial: 股价/汇率/利率/stock price/exchange rate.
- News/sports: 比分/新闻/news/score.
- Weather: 天气/温度/weather/temperature.

Admin can extend / disable rules via the configKey shadow without code change.

## 5. Two-layer config: L1 singleton (infrastructure) + L2 per-route (policy)

### 5.1 L1 — `semantic_cache_config` singleton (infrastructure)

```sql
-- One row, id='singleton'. Mirror of ai_guard_config.
embedding_provider_id  TEXT REFERENCES "Provider"(id)
embedding_model_id     TEXT REFERENCES "Model"(id)
embedding_dimension    INT
embedding_fingerprint  TEXT   -- sha256(provider:model:dim); drives blue/green index swap (§3.5)
redis_index_name       TEXT   -- versioned, auto-bumped on (provider, model) change
                              -- ('nexus:semantic-cache:v1' → 'v2' → 'v3'); admin override
                              -- is allowed but rare (advanced migration scenarios only)
enabled                BOOLEAN -- fleet-wide kill switch
updated_at             TIMESTAMPTZ
updated_by             TEXT
```

L1 admin endpoint: `GET/PUT /api/admin/semantic-cache/config`. UI: **Settings → Cache Embedding** page (mirror of **Settings → AI Guard**). IAM: `iam.ResourceSemanticCache.{Read, Update}`.

### 5.2 L2 — per-route policy (behaviour)

```yaml
response_cache_policy:
  extract:
    enabled: true
    ttl: 300            # seconds
    vary_by: none       # none | user | vk | org
  semantic:
    enabled: false      # default OFF — admin opts in per route
    threshold: 0.96
    embed_strategy: system_plus_last_user
    vary_by: vk         # default stricter than extract
    allow_cross_model: false
  skip_time_sensitive: true
```

L2 carries **policy / risk / scope** only — no embedding-model choice. The embedding model is L1 infrastructure; L2 just consumes it. Per-route admin endpoint: existing `PUT /api/admin/routing-rules/:id` accepts the new shape (no embedding fields). UI: per-route **Cache Settings** page (E61-S6) shows a read-only chip listing the active L1 embedding model with a deep-link to the L1 page.

### 5.3 Effective-enabled cascade

```
effective_semantic_enabled(route) =
    semantic_cache_config.enabled            (L1 kill switch)
  AND semantic_cache_config.embedding_model_id IS NOT NULL  (L1 configured)
  AND route.response_cache_policy.semantic.enabled (L2 opt-in)
```

The current three-tier config model uses `extract.*` for L1 keys and `semantic.*` for L2 keys on `response_cache_policy`. The "embedding provider per route" configuration is explicitly rejected — the embedding model is L1 infrastructure; per-route admin surfaces only the L2 opt-in and TTL/vary settings (see §3.5).

## 6. Failure modes

The semantic tier is **best-effort** — every failure must degrade gracefully back to the broker path. The extract tier is **load-bearing** for its own scope (an extract-cache failure must NOT skip extract write/read silently — it must surface via metrics + logs). The contract per failure class:

| Failure | Behaviour | Audit stamp | Operator alert |
|---|---|---|---|
| **Valkey unreachable** | Extract: log error + miss + broker fallthrough; no extract write attempted. Semantic: log error + skip lookup + broker fallthrough; no semantic write. Both tiers fail-open. | `GatewayCacheSkipReason=valkey_unavailable` | `nexus_valkey_unavailable_total` rate alarm |
| **Embedding provider 429 / 5xx** | Singleflight leader receives error → all joiners see same error → fallthrough to broker. No L2 write. Extract path unaffected. | `GatewayCacheSkipReason=embedding_provider_error` (`error_detail` field carries the HTTP status) | `nexus_cache_embedding_calls_total{outcome="error"}` rate alarm |
| **Embedding hard-timeout** (5s; `singleflight.go:26 defaultEmbedTimeout`) | Singleflight aborts → fallthrough to broker. No L2 write. Counts toward circuit-breaker trip threshold (§3.3). | `GatewayCacheSkipReason=embedding_timeout` | `nexus_cache_embedding_latency_seconds` p99 alarm |
| **Embedding circuit breaker `open`** | All L2-eligible requests skip the embedding call entirely → broker fallthrough. <1ms overhead during outage. Auto-recovers via half-open probe after 30s. | `GatewayCacheSkipReason=embedding_circuit_open` | `nexus_cache_embedding_circuit_trips_total` rate alarm |
| **Per-route embedding budget exhausted** | Route auto-disables L2 until next UTC midnight. L1 unaffected. Banner surfaces on the route's Cache Settings page (S6 T5d.2). Resets at 00:00 UTC. | `GatewayCacheSkipReason=embedding_budget_exceeded` | `nexus_cache_embedding_budget_exceeded_total{route}` per-route counter; if same route trips repeatedly across days, investigate threshold / paraphrase rate |
| **FT.SEARCH error / malformed result** | Log warning + skip L2 hit attempt → fallthrough to broker. L2 write still fires after upstream returns. | `GatewayCacheSkipReason=semantic_search_error` (read-side); write-side error stamps `nexus_cache_l2_writes_total{outcome="error"}` | `nexus_cache_l2_search_errors_total` rate alarm |
| **FT.SEARCH timeout (20ms)** | Same as malformed result — log + skip + broker. | `GatewayCacheSkipReason=semantic_search_timeout` | Same alarm |
| **Reindex job in flight (fingerprint mismatch)** | Read: `@fingerprint:{current}` filter naturally excludes old entries → KNN returns 0 results → miss → broker. Write: HSET stamps current fingerprint; if admin saved new fingerprint mid-write the entry becomes orphan + TTLs out. | `GatewayCacheSkipReason=semantic_reindex_in_progress` (when explicit detection — config_cache observes mismatch in flight) | `nexus_semantic_cache_reindex_inflight_total` |
| **Reindex job failed after retries** | Job dead-lettered; gateway treats fingerprint mismatch as "in-progress" indefinitely until operator triggers manual reindex via S6c button. L2 effectively disabled until then. Extract unaffected. | `nexus_semantic_cache_flush_failed_total` | Page on-call |
| **L1 config disabled mid-request** | Request that started with `effective_semantic_enabled=true` and already hit L2 successfully → serve the hit (it's a cached real response). Request that hasn't yet checked L2 → skip L2 (config_cache observes the flip on next refresh; up to 1s window). | n/a (correct behaviour) | n/a |
| **valkey-search module fails to load on startup** | ai-gateway boots with semantic tier disabled. All L2 paths short-circuit to "module unavailable" → broker. Extract tier and L1 admin endpoints unaffected. | `GatewayCacheSkipReason=semantic_unavailable` | `nexus_cache_l2_module_unavailable` gauge |
| **Embedding adapter returns wrong dimension** | Compare returned vector dim against L1.embedding_dimension; on mismatch → log error + skip L2 write (would corrupt index) + broker fallthrough. Triggers alert because it indicates provider drift. | `GatewayCacheSkipReason=embedding_dim_mismatch` | `nexus_cache_embedding_dim_mismatch_total` |

Bullet rules every failure handler must obey:

- **Never block the broker path.** Any L1 or L2 phase that takes >its budget MUST surface a fallback; the upstream call wins by default.
- **Never poison L2 on uncertainty.** If anything along the write path is uncertain (config in transition, dim mismatch, adapter timeout), skip the write — empty L2 is better than wrong L2.
- **Stamp every skip with a reason.** Operators read `gateway_cache_skip_reason` to debug missing hits; an unstamped skip is a debug black hole.
- **Audit-trail the reindex job.** Both happy-path completion and failure-path dead-letter write to the existing audit pipeline so the operator can reconstruct what happened.

Skip reasons added in E61 — recap in one place (full list defined in `packages/ai-gateway/internal/platform/audit/audit.go`):

| Constant | Triggers |
|---|---|
| `GatewayCacheSkipReasonTimeSensitive` | freshness rule matched |
| `GatewayCacheSkipReasonOversizeForEmbedding` | inputstaging.Plan returned OverflowKind != none |
| `GatewayCacheSkipReasonValkeyUnavailable` | Valkey connection error |
| `GatewayCacheSkipReasonEmbeddingTimeout` | embedding singleflight hard-timeout |
| `GatewayCacheSkipReasonEmbeddingProviderError` | embedding upstream non-2xx |
| `GatewayCacheSkipReasonEmbeddingDimMismatch` | returned dim != L1.embedding_dimension |
| `GatewayCacheSkipReasonSemanticSearchError` | FT.SEARCH RESP error or malformed result |
| `GatewayCacheSkipReasonSemanticSearchTimeout` | FT.SEARCH exceeded 20ms |
| `GatewayCacheSkipReasonSemanticReindexInProgress` | active L1 fingerprint differs from cache_config snapshot at decision time |
| `GatewayCacheSkipReasonSemanticUnavailable` | valkey-search module not loaded at gateway boot |
| `GatewayCacheSkipReasonEmbeddingCircuitOpen` | embedding provider circuit breaker tripped (§3.3) |
| `GatewayCacheSkipReasonEmbeddingBudgetExceeded` | per-route daily embedding cost ceiling exceeded (§7.3) |

S2-T2.1 wires these into `audit.go`. S4 acceptance criteria asserts the right reason fires for each failure simulation.

## 7. Cost + quota accounting

### 7.1 Extract hit

- Provider cost: 0 (upstream not called).
- `gateway_cache_savings_usd`: what the upstream would have cost.
- `embedding_cost_usd`: 0 (no embedding call).
- Quota `requests`: decremented. Quota `tokens`: not decremented (no upstream tokens burnt).

### 7.2 Semantic hit

- Provider cost: 0.
- `gateway_cache_savings_usd`: what the upstream would have cost (priced at the *requested* model, not the cached model).
- `embedding_cost_usd`: cost of the embedding call that produced the hit (new column in E61-S2).
- Net savings = `gateway_cache_savings_usd` - `embedding_cost_usd`. Cache ROI surfaces this net figure (E61-S6 UI).
- Quota: same as Extract hit.

### 7.3 Semantic miss / threshold miss

- Provider cost: full upstream cost.
- `embedding_cost_usd`: cost of the embedding call that produced the miss/threshold-miss.
- Net cost = full upstream cost + embedding cost. Worst-case overhead is bounded by `embedding_cost_usd`, which sits **below the per-call cost of the cheapest text-embedding-3-small invocation at the time of writing**. We deliberately do not pin a dollar figure here because provider embedding pricing changes; consult OpenAI's current pricing page (https://openai.com/api/pricing/) and apply it against the actual `usage.prompt_tokens` reported on the embedding call — the gateway stamps that into `traffic_event.embedding_cost_usd` per request, which is the authoritative number for a given deployment.

### 7.4 Per-route embedding-cost budget (Round-1 review addition)

Per-route `semantic.embedding_cost_ceiling_usd_per_day` (default `null`, no cap) bounds runaway embedding spend on misconfigured routes (e.g., low-similarity workload where every request triggers an embedding but almost none hit). Mechanism:

- The audit-pipeline writer accumulates a per-route daily counter in Valkey: `INCRBYFLOAT nexus:semantic:budget:<route_id>:<utc_date> <embedding_cost_usd>`. TTL set to 26h to allow reset at the next UTC midnight.
- The L2 lookup path (§3.8 step 3) reads `GET nexus:semantic:budget:<route_id>:<utc_date>` BEFORE issuing the embedding call. If the value ≥ ceiling, stamp `GatewayCacheSkipReason=embedding_budget_exceeded` and skip both lookup and write. <1ms overhead per request.
- Counter resets at 00:00 UTC (TTL expiry); no explicit reset cron needed.
- Admin can manually reset via `DELETE /api/admin/routing-rules/:id/cache-budget-state` (new endpoint in S6 T5d.2's referenced API).
- UI: S6 T5d.2 renders the auto-disabled banner with the next-reset timestamp.

## 8. Cache ROI surface

CP "Cache ROI" page aggregates per-adapter (the breakdown table in `packages/control-plane-ui/src/pages/analytics/CacheROIDashboard.tsx:404,408,454` is keyed on `adapter`):
- Hit rate by tier (extract / semantic / combined).
- Cost saved (gross) per tier.
- Embedding cost (semantic only).
- Net savings (gross saved - embedding cost).
- Hit-latency vs miss-latency.
- Top-cached requests (by hit count).
- Time-sensitive skip rate (an indicator that the rule list is reaching the right traffic).

## 9. Eviction + invalidation

- **TTL** — both tiers respect per-route TTL.
- **LRU** — Valkey configured with `maxmemory-policy: allkeys-lru`.
- **HNSW ghost cleanup** — valkey-search drops indexed documents when the underlying hash key expires (verified at integration time; if behaviour drifts, S7 adds a background sweeper).
- **Embedding model upgrade** — switching `semantic.embedding_model_id` routes new writes to a fresh index. Old index entries TTL out.
- **Policy change** — when `vary_by` or `threshold` changes, existing entries remain valid (they were written under the previous policy but are still semantically correct results for matching queries). The next write under the new policy lays new entries.

## 10. What's NOT cached

- Streaming responses with `chunked_async` hook policy — partial bytes may have reached client.
- Tool-call responses — function-call decision triggers client-side state.
- Error / refusal responses — would amplify provider issues.
- Responses with `nexus.ext.cache_bypass=true` marker.
- Time-sensitive prompts (§4).
- Prompts whose embedding input exceeds the embedding model's context (semantic only — extract still caches).

## 11. Observability

Per-tier metrics:
- `nexus_cache_l1_lookups_total{outcome="hit|miss|skip"}` (existing, renamed from generic).
- `nexus_cache_l1_threshold_misses_total` — N/A for L1.
- `nexus_cache_l2_lookups_total{outcome="hit|threshold_miss|miss|skip"}`.
- `nexus_cache_l2_similarity_histogram` — distribution of best-neighbour cosine similarity values.
- `nexus_cache_embedding_latency_seconds`.
- `nexus_cache_embedding_calls_total{provider,model}`.
- `nexus_cache_embedding_cost_usd_total`.
- `nexus_cache_freshness_skips_total{reason}` — per time-sensitive rule.

## 12. Sources

- `packages/ai-gateway/internal/cache/core/` — Extract cache primitives.
- `packages/ai-gateway/internal/cache/semantic/` — Semantic cache (E61-S3+S4).
- `packages/ai-gateway/internal/cache/freshness/` — time-sensitive detector (E61-S1).
- `packages/ai-gateway/internal/cache/stream/` — broker + singleflight (E35-S1).
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — `classifyCachePreLookup`, cache decision orchestration.
- `packages/ai-gateway/internal/ingress/proxy/proxy_cache.go` — hit replay (shared by extract + semantic).
- `packages/ai-gateway/internal/execution/canonicalbridge/` — ingress-format reshape.
- `packages/shared/transport/inputstaging/` — shared truncation primitive (E61-S2b).
- `packages/control-plane/internal/handler/routing.go` — admin policy CRUD.
- `tools/db-migrate/schema.prisma` — `SemanticCacheConfig` (semantic cache, E61-S3+S4) and `ExtractCacheConfig` (Extract cache) Prisma models. There is no `response_cache_policy` model; per-route cache settings live on the routing rule row.

## 13. Cross-references

- `prompt-cache-architecture.md` — distinct tier (provider-side KV reuse). UI consolidated with this doc's settings on the same "Cache Settings" page since E61.
- `cache-multi-tier-architecture.md` — parent multi-tier catalogue.
- `provider-adapter-architecture.md` — canonical payload feeds the key; `ResponseCanonicalToIngress` powers hit-replay reshape.
- `routing-architecture.md` — route policy carries `response_cache_policy`.
- `quota-architecture.md` — hit accounting.
- `cost-estimation-architecture.md` § 6.4 — derivation of unified `cache_status` and audit-drawer rendering. The `embedding_cost_usd` column added by E61 surfaces in the same drawer.

<!-- 💡 harvest: the embedding-singleflight pattern (§3.3) mirrors the broker singleflight pattern — when a third internal-AI workload arrives (routing decision LLM, ai-guard classify), this might be worth extracting into a generic shared helper. Track as a code-health note, not a Cursor rule. -->
