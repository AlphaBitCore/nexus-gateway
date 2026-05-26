# Feature Cost Tracking

Nexus stamps the exact cost of every AI request directly on the traffic event row at request completion. The price source is a single table — the `Model` row's four price fields — and a single cost function applied everywhere. The Traffic Event drawer in the Control Plane UI shows a per-request cost breakdown; the Cache ROI page aggregates savings across prompt and response cache tiers; the `/v1/estimate` endpoint provides pre-flight cost estimates before committing to a request.

---

## What Nexus does

### The cost formula

Every traffic event carries a stamped cost derived from:

```
cost = uncachedInput × inputPricePerMillion
     + cacheRead     × cachedInputReadPricePerMillion
     + cacheWrite    × cachedInputWritePricePerMillion
     + output        × outputPricePerMillion
```

All four price fields live on the `Model` row in the database. The same upstream model identifier routed through different providers is three separate `Model` rows with three separate price sets — routing through AWS Bedrock picks up Bedrock's marketplace prices, not Anthropic's direct prices. This means the right price is always used regardless of which credential and routing path the request took.

### Three cost perspectives

Three questions about cost all call the same `metrics.CalculateCost` function with different token inputs:

| Question | Token source | Where it appears |
|---|---|---|
| What did this request cost? | Provider's `usage` block, parsed at response time | `estimated_cost_usd` on the traffic event |
| What did caching save? | Counterfactual: full price minus what was actually billed | `gateway_cache_savings_usd` on the traffic event |
| What would this request cost if sent now? | Local tokenizer (input) + per-model output budget table (output) + read-only cache lookup | `/v1/estimate` response |

If the three numbers ever disagree at the database level, it is a stamping bug — the pricing formula is not forked.

### Cache savings accounting

Cache savings appear on two dimensions:

**Gateway response-cache hit**: the upstream call was skipped entirely. `gateway_cache_savings_usd` equals `estimated_cost_usd` — the request had zero upstream cost. Both fields carry the full would-have-paid amount so the arithmetic `estimated_cost_usd − gateway_cache_savings_usd = 0` holds.

**Provider prompt-cache hit**: the upstream call ran but input tokens were billed at the cheaper cached-read rate. Savings = `cacheRead × (inputPrice − cachedReadPrice)` minus the cache-creation surcharge on the turn that created the cache entry. These savings are smaller than a full gateway cache hit but still reduce the effective input cost, often by 50–90% depending on the provider.

The Cache ROI page aggregates both types of savings per route, broken out by tier.

## Cost fields on the traffic event

The Traffic Event drawer in the Control Plane shows the full cost breakdown for each request:

| Field | Meaning |
|---|---|
| `estimated_cost_usd` | What the upstream would cost at current model prices (independent of cache outcome) |
| `gateway_cache_savings_usd` | Avoided upstream spend due to gateway response-cache hit |
| `cache_read_tokens` | Input tokens served from provider prompt cache (billed at cache-read rate) |
| `cache_creation_tokens` | Input tokens that created a new provider prompt cache entry (billed at cache-write rate, usually higher than input) |
| `reasoning_tokens` | Reasoning/thinking tokens reported by the provider; billed as output tokens; shown separately for transparency |
| `embedding_cost_usd` | Cost of embedding calls made for semantic cache lookups (separate from the billed request cost) |

Costs are `NULL` on a row only when the `Model` row's price fields have not been configured by the administrator.

## Where it sits

- Cost function: `packages/ai-gateway/internal/platform/metrics/metrics.go` (`CalculateCost`).
- Token field stamping: `packages/ai-gateway/internal/ingress/proxy/proxy.go` and `proxy_cache.go` — five stamp sites covering non-stream, stream, cache-hit non-stream, cache-hit stream, and in-flight stream coalescer paths.
- Price data: `tools/db-migrate/schema.prisma` `Model` table — `inputPricePerMillion`, `outputPricePerMillion`, `cachedInputReadPricePerMillion`, `cachedInputWritePricePerMillion`.
- Pre-flight estimator: `packages/ai-gateway/internal/execution/estimator/` — `Estimate()` for `/v1/estimate`.

## How to enable and configure

Cost tracking is always on — no configuration required. Costs are `NULL` on a traffic event row only when the `Model` row's price fields have not been filled in.

To ensure accurate costs:

1. Navigate to **AI Gateway → Providers & Models**.
2. Open each model and fill in all four price fields. Cached input prices are optional — if left empty, cached tokens are billed at the flat input rate (no discount tracked).
3. Provider templates shipped with Nexus (OpenAI, Anthropic, Google Gemini, DeepSeek) pre-populate prices. Verify them against the provider's current published pricing and update when providers announce price changes.

Provider-specific defaults for cache pricing:
- **Anthropic**: cache-read at 0.10× input price; cache-write at 1.25× input price.
- **OpenAI**: cache-read at 0.50× input price; no cache-write surcharge.
- **Gemini**: cache-read at 0.25× input price; no cache-write surcharge.

### Pre-flight cost estimation

Applications can call `POST /v1/estimate` before sending a real request to get an estimated cost envelope. The response includes low / expected / high token count estimates plus the derived cost at current model prices. Useful for gating expensive calls in cost-sensitive workflows.

---

## Canonical docs

- [`cost-estimation-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md) — single source of truth for the cost formula, Model row prices, the five stamp sites, cache savings derivation, reasoning-token split, dry-run pipeline, and UI rendering rules

**Adjacent wiki pages**: [Feature Prompt Cache](Feature-Prompt-Cache) · [Feature Response Cache](Feature-Response-Cache) · [AI Gateway Cost Estimation](AI-Gateway-Cost-Estimation) · [AI Gateway Providers And Models](AI-Gateway-Providers-And-Models) · [Features Index](Features-Index)
