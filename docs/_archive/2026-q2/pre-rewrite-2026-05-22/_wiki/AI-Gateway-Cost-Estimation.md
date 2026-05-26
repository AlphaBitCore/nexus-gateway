# AI Gateway Cost Estimation

*Audience: operators reading cost analytics; contributors modifying cost stamping or adding token fields.*

Cost attribution in Nexus Gateway flows from a single source of truth — the four price fields on the `Model` database row — through a single cost function (`metrics.CalculateCost`) applied at five stamping sites across the request lifecycle. Every number the analytics UI shows for "what did this cost" and "what did caching save us" comes from this one formula applied to different token inputs. A `nexus.dry_run` flag in the request body lets callers estimate cost before spending tokens.

---

## The single cost formula

Whatever the question — actual cost, cache savings, pre-flight estimate — the answer is one formula applied to different token inputs:

```
cost(req) = uncachedInput   × inputPricePerMillion    / 1_000_000
          + cacheReadTokens × cachedInputReadPricePerMillion  / 1_000_000
          + cacheWriteTokens × cachedInputWritePricePerMillion / 1_000_000
          + outputTokens    × outputPricePerMillion   / 1_000_000
```

`metrics.CalculateCost` is the single function that implements this. No stamp site does inline arithmetic. The result is a `Cost` struct with four component fields plus a `Total`.

### Price data model

Prices are properties of the (provider, model) pair. The `Model` table row already carries the provider scope:

```
Model {
    id                              UUID
    providerId                      FK → Provider
    providerModelId                 String   // upstream's identifier
    code                            String   // customer-facing slug (e.g. "claude-sonnet-4-6")
    inputPricePerMillion            Decimal?
    outputPricePerMillion           Decimal?
    cachedInputReadPricePerMillion  Decimal?
    cachedInputWritePricePerMillion Decimal?
}
```

The same upstream model identifier (`"claude-sonnet-4-6"`) routed through Anthropic direct, AWS Bedrock, and Google Vertex AI is three separate `Model` rows with three different price sets, matching each provider's actual billing markup.

Price fields are nullable. A `nil` cached price falls back to the standard `inputPricePerMillion`, which is the correct behaviour for providers with no caching discount (self-hosted vLLM, Ollama). A `nil` input price produces a `NaN` cost, and the stamping sites record `NULL` on the `traffic_event` row rather than silently writing `$0`.

## The five stamping sites

Every upstream response — streaming or non-streaming, from cache or live — goes through one of five handler functions in `packages/ai-gateway/internal/ingress/proxy/`. All five call `metrics.CalculateCost`; none do inline arithmetic on token counts:

| Site | Handler | When |
|---|---|---|
| 1 | `proxy.go: handleNonStream` | Live upstream call, non-streaming response |
| 2 | `proxy.go: handleStream` | Live upstream call, SSE stream — stamps on final `usage` chunk |
| 3 | `proxy_cache.go: handleStreamHit` | SSE replay from cache hit |
| 4 | `proxy_cache.go: handleNonStreamHit` | Non-streaming replay from cache hit |
| 5 | `proxy_cache.go: handleStreamWithSubscription` | In-flight stream coalescer — the subscriber inherits the leader's stamp |

Adding a new token field requires updating all five sites plus the `Usage` struct and the `traffic_event` schema. Missing the four cache sites leaves all cached-traffic rows with `NULL` on the new column.

## What `estimated_cost_usd` means

`estimated_cost_usd` is the **predicted upstream-provider cost at the configured `Model` prices**. It is a property of the request, not of whether a cache served it. Two requests with identical tokens against the same model produce the same `estimated_cost_usd` regardless of caching.

Cache-hit rows stamp `gateway_cache_savings_usd = estimated_cost_usd` to make the math explicit:

```
net upstream paid = estimated_cost_usd − COALESCE(gateway_cache_savings_usd, 0)
                  = 0          (full gateway cache hit)
                  = estimated  (full miss)
                  = partial    (provider prompt-cache discount only)
```

## Three cost outcomes

Every traffic event falls into one of three cases:

**Gateway hit** — Nexus response cache served the request. No upstream call. `gateway_cache_status ∈ {hit, hit_inflight}`. The row's `estimated_cost_usd` reflects what the upstream would have cost; `gateway_cache_savings_usd` equals that same amount; actual upstream spend is zero.

**Gateway miss + provider hit** — The gateway did not cache it, but the upstream provider applied its own prompt-cache discount (Anthropic ephemeral cache, OpenAI auto prompt caching, Gemini cached content). `cache_read_tokens > 0`. The `estimated_cost_usd` already incorporates the discount from the cached-input price — it is what was actually paid, not the sticker price. `cache_read_savings_usd` captures the saving from the provider's cache alone.

**Full miss** — No caching at any layer. `estimated_cost_usd = prompt_tokens × input_price + completion_tokens × output_price`. Both cache savings columns are `NULL`.

## Internal-ops costs (embedding + ai-guard)

Two internal operations also incur real provider charges and are tracked separately:

| Operation | When | Column |
|---|---|---|
| **Semantic-cache embedding lookup** | Every request where L2 Semantic cache attempts a lookup | `embedding_cost_usd` |
| **AI-Guard classifier call** | When an ai-guard hook fires using the configured internal backend | `ai_guard_cost_usd` |

By default both of these count against the virtual key's billed quota (the `excludeInternalOpsFromBilledCost` flag in `nexus-hub.yaml` controls this). The Traffic Event drawer's Costs Breakdown panel shows all three cost components — upstream provider cost, embedding cost, and ai-guard cost — so operators can evaluate the true overhead of semantic caching and content classification.

## Pre-flight estimation (`nexus.dry_run`)

Any request body can carry `"nexus": {"dry_run": true}`. When the gateway sees this flag after routing resolution and cache lookup, it short-circuits to the estimator (`packages/ai-gateway/internal/execution/estimator/`) instead of calling the upstream:

```json
{
  "model": "gpt-5",
  "messages": [{"role": "user", "content": "..."}],
  "nexus": {"dry_run": true}
}
```

The estimator returns a normal-shape response with `choices: []` (or the ingress equivalent) and a populated `usage` block. The full breakdown — low / expected / high token envelope, cache benefit probability, reasoning-token split, and human-readable assumptions — appears in the `x-nexus-estimate` response header as compact JSON.

Tokenization is exact for OpenAI-family models (tiktoken-go) and a character-ratio heuristic for Anthropic (`chars / 3.5`) and Gemini (`chars / 4`). Output-token budgets come from a per-model table in `output_budget.go` keyed by `(model, reasoning_effort)`:

| Model | `medium` effort expected output |
|---|---|
| `claude-sonnet-4-6` | 3000 tokens |
| `gpt-5` | 1500 tokens |
| `gemini-2.5-pro` | 3000 tokens |

The dry-run path runs the same routing engine and cache key lookup as a live request, so the estimate matches reality as closely as possible. A cache key hit on the response cache results in `CacheBenefit.ResponseHitProbability = 1.0` and the estimated cost is effectively zero (the upstream would not be called).

### Streaming dry-run

Setting `stream: true` alongside `nexus.dry_run: true` returns exactly one SSE chunk with the usage block followed by `[DONE]`. There is no token-by-token simulation.

## Reasoning tokens

For models that support extended thinking (OpenAI o-series, Anthropic claude-opus/sonnet with `thinking`, Gemini 2.5 with `thinking_config`), reasoning tokens are billed as output tokens by all three providers. The `traffic_event.reasoning_tokens` and `reasoning_cost_usd` columns preserve the split for reporting ("how much of the output cost was thinking?") without re-counting in the total.

---

## Canonical docs

- [`cost-estimation-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md) — full price-field model, the five stamp sites, dry-run pipeline, and three HIT × MISS worked examples
- [`prompt-cache-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md) — provider-side cache discounts that feed `cache_read_tokens`
- [`response-cache-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/response-cache-architecture.md) — gateway response cache savings accounting

**Adjacent wiki pages**: [AI Gateway Prompt Cache](AI-Gateway-Prompt-Cache) · [AI Gateway Response Cache](AI-Gateway-Response-Cache) · [AI Gateway Virtual Keys Quotas](AI-Gateway-Virtual-Keys-Quotas) · [AI Gateway Overview](AI-Gateway-Overview)
