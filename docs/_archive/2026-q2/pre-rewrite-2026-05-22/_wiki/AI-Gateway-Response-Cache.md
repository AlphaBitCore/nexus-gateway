# AI Gateway Response Cache

*Audience: operators configuring cache policy; contributors working on the cache subsystem.*

The AI Gateway response cache short-circuits the upstream LLM call entirely when the canonical request matches a previously-served response. Unlike provider-side prompt caching (which only discounts input tokens), a gateway response cache hit eliminates the full upstream round-trip — reducing both latency and cost to near zero for the requesting call. The cache operates in two tiers within a single Valkey instance: an exact-match Extract tier (L1) and an approximate-match Semantic tier (L2).

---

## Two cooperative tiers

| | Extract (L1) | Semantic (L2) |
|---|---|---|
| **Match type** | Exact — canonical request SHA-256 | Approximate — embedding cosine similarity |
| **Stores** | Full response bytes | Full response bytes |
| **Default state** | Enabled per route | Disabled — admin opt-in per route |
| **Hit reduces** | Latency + full upstream cost | Latency + full upstream cost (minus embedding call cost) |
| **Backed by** | Valkey 8.x | Valkey 8.x + valkey-search (HNSW index) |
| **Best for** | Deterministic prompts (embeddings, structured extractors) | Paraphrased / semantically-similar workloads |

A request walks Extract first. On miss, if the route has Semantic enabled and a freshness check passes, it walks Semantic. On Semantic miss it falls through to the upstream broker.

## Extract tier (L1) — exact-match cache

### Cache key

```
extract_key = "nexus:cache:" + SHA-256(
  "v3\nprovider=<upstream_provider>\nmodel=<upstream_model_id>\nallowlist=<headerAllowlistHash>\nbody=" + canonicalize_json(prepared_body)
)
```

The `prepared_body` is the output of `Adapter.PrepareBody` (not the raw client body), which includes `model_id`, `messages`, `tools`, `tool_choice`, `temperature`, `top_p`, and `response_format`. The `stream` flag is excluded — streaming and non-streaming responses are stored on disjoint hashes. Route-level `vary_by_user` or `vary_by_vk` policies fold a per-user or per-VK nonce into the hash.

### Read and write paths

On a cache miss the gateway calls the upstream, and after a successful non-error, non-refusal completion it writes the entry to Valkey with `SET <key> <serialised entry> EX ttl`. Error and refusal responses are never cached. On a hit the gateway replays via `handleStreamHit` / `handleNonStreamHit` in `proxy_cache.go`, stamping `GatewayCacheStatus=hit` and `GatewayCacheKind=extract` on the traffic event.

## Semantic tier (L2) — approximate-match cache

### When the semantic lookup fires

The Semantic lookup fires only when all of the following are true:

1. Extract miss.
2. `response_cache_policy.semantic.enabled=true` for the resolved route.
3. The freshness check did not skip this request as time-sensitive (see below).
4. The embedding input is within the embedding model's context window.
5. Request-stage hooks have already approved the incoming request — a semantic hit serves a different prompt's response, so compliance checks must run first.

### Embedding provider

The embedding model is a **fleet-wide singleton** stored in the `semantic_cache_config` table. Per-route Cache Settings carry policy (`threshold`, `embed_strategy`, `vary_by`, `allow_cross_model`) but not embedding-model choice. Vector spaces do not compose — a single Valkey Vector index requires a fixed dimension at create time. Admin selects the embedding provider and model on the Cache Embedding Settings page (`Settings → Cache Embedding`).

Two reference deployments ship out of the box:

- **OpenAI cloud** — `text-embedding-3-small` (1536-dimensional, 8191-token context).
- **Local OpenAI-compatible server** — any model served by vLLM, Ollama, LiteLLM, or a custom endpoint.

### Similarity threshold and scoping

Default threshold: `0.96` cosine similarity. Default `vary_by`: `vk` (entries scoped per virtual key) — stricter than L1 because approximate matching opens a cross-user response leakage surface that per-VK isolation closes.

Operators can widen to `org`, `user`, or `none` with explicit choice in the Cache Settings UI. Cross-model matching (`allow_cross_model=true`) is supported but carries cost-accounting drift and fidelity risks; the UI carries an explicit warning.

### Performance budget

| Phase | Budget | Timeout fallback |
|---|---|---|
| Embedding call (singleflight) | 30–50ms (OpenAI cloud) / 10–30ms (local) | 100ms hard timeout → fall through to upstream |
| `FT.SEARCH` (HNSW KNN) | 1–15ms depending on index size | 20ms hard timeout → fall through |
| Total p95 overhead | ≤80ms | — |

Every failure degrades gracefully to the upstream path — the Semantic tier is best-effort, not load-bearing.

## Time-sensitive prompt skip

Before any cache decision, the prompt goes through a freshness check in `packages/ai-gateway/internal/cache/freshness/`. When the prompt is time-sensitive (mentions current prices, today's date, live scores, etc.), both Extract and Semantic skip — lookup AND write — so neither tier serves stale content nor poisons future lookups.

Rules are pushed via Hub config shadow (`response_cache.time_sensitive_patterns`). Each rule combines keyword + question structure + entity reference to avoid false positives. The default rule set covers time references, financial data, news, sports scores, and weather in both English and Chinese.

When a rule fires, `GatewayCacheStatus = skipped` and `GatewayCacheSkipReason = time_sensitive` are stamped on the traffic event.

## Configuring the response cache

### Per-route policy

Each routing rule carries a `response_cache_policy` block that controls both tiers:

```yaml
response_cache_policy:
  extract:
    enabled: true
    ttl: 300            # seconds
    vary_by: none       # none | user | vk | org
  semantic:
    enabled: false      # disabled by default — admin opts in per route
    threshold: 0.96
    embed_strategy: system_plus_last_user
    vary_by: vk
    allow_cross_model: false
  skip_time_sensitive: true
```

The Extract tier is enabled by default; the Semantic tier is off. Enabling Semantic requires the fleet-wide `semantic_cache_config` to have an embedding model configured (`Settings → Cache Embedding`).

### Embedding model selection (Semantic tier)

The embedding model is fleet-wide, not per-route. Admin selects it on the Cache Embedding Settings page. Two reference setups:

1. **OpenAI cloud** — `text-embedding-3-small` (1536-dimensional). Costs ~$0.02 per million tokens.
2. **Local server** — any model served by vLLM, Ollama, or LiteLLM via the `local-inference` provider. Zero cloud cost, low latency on the same host.

Changing the embedding model triggers a blue/green index swap in Valkey. Old index entries become invisible to new lookups (fingerprint guard) and TTL out within the configured cache window. The Cache Embedding Settings page shows a confirmation modal explaining the warm-up period.

### `vary_by` and isolation

The `vary_by` setting controls cache entry scoping:

| `vary_by` | Scope | Use when |
|---|---|---|
| `none` | All traffic shares one cache | Deterministic prompts with no user-specific variation |
| `vk` | Scoped per virtual key | Default for Semantic; prevents cross-VK leakage |
| `user` | Scoped per authenticated user | Per-user personalized responses |
| `org` | Scoped per organization | Per-org deterministic prompts |

Semantic cache defaults to `vary_by=vk` (stricter than Extract's default `none`) because approximate matching creates a cross-user leakage surface that exact-match caching does not.

## Cache ROI surface

The Control Plane Cache ROI page aggregates per route:

- Hit rate by tier (extract / semantic / combined).
- Cost saved (gross) per tier.
- Embedding cost for semantic-tier lookups (regardless of hit or miss).
- Net savings = gross saved minus embedding cost.
- Hit-latency vs miss-latency.
- Time-sensitive skip rate.

## Failure modes

| Failure | Behaviour |
|---|---|
| Valkey unreachable | Both tiers fail-open: log error + cache miss + fall through to upstream. `GatewayCacheSkipReason=valkey_unavailable`. |
| Embedding provider down | Singleflight leader error propagates to all joiners → fall through to upstream. Extract path unaffected. Circuit breaker trips after 10 consecutive failures, skipping L2 for 30s. |
| valkey-search module not loaded | L2 disabled at boot. `GatewayCacheSkipReason=semantic_unavailable`. Extract path unaffected. |
| Reindex in flight (embedding model change) | Fingerprint filter on every `FT.SEARCH` query ensures stale entries are invisible during the blue/green index swap. |

What is never cached: streaming responses with `chunked_async` hook policy; tool-call responses; error / refusal responses; responses with `nexus.ext.cache_bypass=true`; time-sensitive prompts; prompts whose embedding input exceeds the model's context (Semantic only).

---

## Canonical docs

- [`response-cache-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/response-cache-architecture.md) — full two-tier design, singleflight mechanics, index lifecycle, and all skip-reason constants
- [`prompt-cache-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md) — provider-side prompt caching (distinct from response caching)
- [`cost-estimation-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md) — cache savings accounting (§6)

**Adjacent wiki pages**: [AI Gateway Prompt Cache](AI-Gateway-Prompt-Cache) · [AI Gateway Cost Estimation](AI-Gateway-Cost-Estimation) · [AI Gateway Routing Rules](AI-Gateway-Routing-Rules) · [AI Gateway Virtual Keys Quotas](AI-Gateway-Virtual-Keys-Quotas)
