---
doc: cost-estimation-architecture
area: service
service: ai-gateway
tier: 1
updated: 2026-05-21
---

# Cost Estimation Architecture

> **Tier 2 architecture doc — single source of truth for every cost / price computation.**
> Read this before touching `packages/ai-gateway/internal/execution/estimator/**`, `metrics.CalculateCost`, the four price fields on `Model` (`{input,output,cachedInputRead,cachedInputWrite}PricePerMillion`), the `nexus.dry_run` pipeline branch, the `/v1/estimate` endpoint, any cost stamping site in `proxy.go` / `proxy_cache.go`, the internal-ops cost columns (`embedding_cost_usd`, `ai_guard_cost_usd`), the `excludeInternalOpsFromBilledCost` yaml toggle, the reasoning-token cost split, the `traffic_event.reasoning_tokens` / `reasoning_cost_usd` columns, or the admin Cache ROI / Costs breakdown UI. The unified protocol parser that feeds this layer is documented in `normalization-architecture.md` (the canonical `Usage` and `NormalizedPayload` come from `shared/normalize` Tier-1 normalizers; ai-gateway projects via `canonicalbridge.DecodeViaShared`). The bidirectional canonical ↔ wire translation rules for the emission side are documented in `provider-adapter-architecture.md` § 3a.
>
> **Binding rule (2026-05-21).** This doc is the **canonical specification**; if code drifts from this doc, the **code is wrong**. Every cost field, every stamp site, every UI render rule has a single explanation here. Any PR that changes cost math or column semantics must update this doc in the same commit (enforced by the CLAUDE.md "code / doc lockstep" mandatory rule).
>
> **Recent updates** (read these sections if you're scanning for what changed):
> - §2.2 / §2.4 — `provider_pricing` table dropped; `Model` row is the only price source.
> - §3.2 — Anthropic double-count bug fixed in `proxy.go.computeCacheCosts`.
> - §3.3 — Cache HIT path semantic correction: `estimated_cost_usd` = would-have-paid (not 0).
> - §6 — New: Internal-ops costs (embedding, ai-guard) + `excludeInternalOpsFromBilledCost` toggle.
> - §6.5 — The 3 HIT cases (gateway HIT / provider HIT / full MISS) made explicit with worked examples.
> - §10 — Historical recompute scripts + prod-deploy runbook references.

Cost, cache savings, pre-flight estimation, **internal-ops cost** (semantic-cache embeddings + ai-guard classifier), and the **3 HIT cases** are the **same data viewed several ways**. This doc explains the single pricing table (Model row), the single cost function (`metrics.CalculateCost`), the single estimator core (`packages/ai-gateway/internal/execution/estimator/`), the dry-run pipeline branch, and the per-row column semantics that downstream rollups + the Traffic Event drawer's Costs breakdown depend on.

---

## 1. The three numbers and the one calculation

Whatever the customer asks ("what did this cost?", "what did caching save us?", "what would this cost if I sent it?"), the answer is one of three perspectives on a single formula:

```
cost(req) = uncachedInput × inputPrice
         + cacheRead     × cachedInputReadPrice
         + cacheWrite    × cachedInputWritePrice
         + output        × outputPrice
```

| Customer question | What's plugged in for token counts |
|---|---|
| **What did this request cost?** | Counts come from the upstream response's `usage` block, parsed by `shared/normalize/<format>` Tier-1 normalizer and projected via `canonicalbridge.DecodeViaShared` to the canonical `providers.Usage`. |
| **What did the cache save us?** | "Savings" is a counterfactual: it's `cost(req) computed with caching turned off` minus `cost(req) as billed`. Mechanically: gateway response-cache hit ⇒ saved = full upstream cost; provider prompt-cache hit ⇒ saved = `cacheRead × (inputPrice − cachedInputReadPrice)` minus the write cost on the turn that created the cache. |
| **What would this request cost if I sent it?** | Counts come from `estimator.Estimate(req)`: input tokens from the local tokenizer, output tokens from a static per-(model, reasoning_effort) budget table with low/expected/high envelope, cache tokens from a read-only cache lookup against the same response cache and the same prefix-match logic the real path uses. |

The three columns of that table call the same `metrics.CalculateCost` function with different `Usage` inputs. That is the entire architectural commitment of E58: there is exactly one cost function. If the three numbers ever disagree at the database level, it is a bug in cost stamping or in cache savings derivation, not a fork in pricing logic.

## 2. Pricing data model

### 2.1 Where prices live

Prices are properties of the **(Provider, Model) pair**. The `Model` table is already provider-scoped:

```
Model {
    id                                String   @id @default(uuid())
    providerId                        String                          ← FK to Provider
    providerModelId                   String                          ← upstream's id
    code                              String   @unique                ← customer-facing id
    // ...
    inputPricePerMillion              Decimal?  @db.Decimal(12, 8)
    outputPricePerMillion             Decimal?  @db.Decimal(12, 8)
    cachedInputReadPricePerMillion    Decimal?  @db.Decimal(12, 8)    ← NEW (E58-S1)
    cachedInputWritePricePerMillion   Decimal?  @db.Decimal(12, 8)    ← NEW (E58-S1)
    // ...
    @@unique([providerId, providerModelId])
}
```

Same upstream model identifier (`"claude-sonnet-4-6"`) routed through Anthropic direct vs AWS Bedrock vs Vertex AI is **three rows in `Model`** with three different price sets. Anthropic-direct row carries Anthropic's 1.25× cache-write rate; the Bedrock row carries Bedrock's marketplace markup; the Vertex row carries Vertex's region-specific pricing. All four fields are nullable to support partial setup (an admin can register a model and fill in prices later — pricing-less rows produce a `nil` `Cost` and stamp `NULL` on `traffic_event`).

The `Model` row carries authoritative prices; the prior two-table `provider_pricing` schema was consolidated into the `Model` row. Price-change history is captured by `AdminAuditLog`. `cache/layer.Layer.LookupCachePricing(adapterType, providerID, modelCode)` looks up the model in the `modelsByCode` snapshot and assembles a pricing result on the fly. NULL cache-price columns fall back to `Model.inputPricePerMillion` so the cost formula degrades cleanly to "flat input rate, no caching effect" on models the operator hasn't fully configured.

### 2.3 The `nil` semantics

Per-field nullability has a defined meaning. `CalculateCost` (`packages/ai-gateway/internal/platform/metrics/metrics.go:391`) uses `derefPrice(p, fallback)` (`metrics.go:420-425`) — nil prices resolve to a fallback, not to `NaN`:

| Field | `nil` means | Calculation behavior |
|---|---|---|
| `inputPricePerMillion` | not configured | `Cost.UncachedInput = 0` (zero-price fallback). |
| `outputPricePerMillion` | not configured | `Cost.Output = 0`. |
| `cachedInputReadPricePerMillion` | provider has no cache discount, OR rate equals input price | Falls back to `inputPricePerMillion` (`metrics.go:394`). |
| `cachedInputWritePricePerMillion` | provider has no write surcharge, OR rate equals input price | Falls back to `inputPricePerMillion` (`metrics.go:395`). |

The fallback semantics on the two cache fields are deliberate: most OSS-friendly providers (e.g., a self-hosted vLLM, Ollama) have no cache discount; storing `nil` and letting the code fall back to the standard input rate is cleaner than forcing operators to fill in two extra rows of "same as input". A model row with no input/output price configured at all yields a zero-total `Cost` and stamps `0` on `traffic_event.estimated_cost_usd` rather than NULL — operators should configure prices before exposing the model.

## 3. The cost function

### 3.1 Signature

```go
// packages/ai-gateway/internal/platform/metrics/metrics.go
package metrics

import "github.com/.../packages/ai-gateway/internal/providers"
// providers.Usage is the canonical Usage struct. After E58-S0 it is
// populated by canonicalbridge.DecodeViaShared from the shared/normalize
// Tier-1 normalizer's NormalizedPayload.Usage; the struct definition
// may live in providers/types.go or shared/normalize/types.go (final
// location decided in S0-T1.1; whichever is non-canonical becomes a
// type alias).

// ModelPrices is the pricing snapshot for one (Provider, Model) pair.
// Nil fields fall through per the rules in cost-estimation-architecture.md § 2.3.
type ModelPrices struct {
    InputUsdPerM            *float64
    OutputUsdPerM           *float64
    CachedInputReadUsdPerM  *float64
    CachedInputWriteUsdPerM *float64
}

// Cost is the four-component breakdown plus a derived ReasoningSplit
// advisory (already inside Output; surfaced for analytics). Total is the
// sum of the first four. Fields are 0 (not NaN) when the corresponding
// token count is 0 / nil OR when the relevant price was nil with no
// fallback configured (derefPrice falls through to 0).
type Cost struct {
    UncachedInput  float64
    CacheRead      float64
    CacheWrite     float64
    Output         float64
    Total          float64
    ReasoningSplit float64 // = ReasoningTokens × OutputUsdPerM; subset of Output, advisory only
}

// CalculateCost is the single point of cost calculation in the gateway.
// All stamp sites in proxy.go + proxy_cache.go MUST call it (directly or
// via the estimator.Lookup(endpointType) per-endpoint formula registry);
// no inline arithmetic on token counts × prices.
func CalculateCost(u provcore.Usage, p ModelPrices) Cost { ... }
```

### 3.2 Implementation rules

- `CalculateCost` is **pure**: same inputs → same outputs, no global reads, no logger. The five stamp sites are responsible for fetching `ModelPrices` from `cache/layer`.
- The fallback chain for cache prices is **single-level**: `cachedInputReadPricePerMillion ?? inputPricePerMillion`. There is no second-level fallback (no `?? someDefault`); a missing `inputPricePerMillion` yields `NaN` and the caller decides what to do.
- `Output` always uses `CompletionTokens` from the canonical `Usage` — which **already includes reasoning tokens** (all three frontier providers roll reasoning into the output bucket for billing). The separate `ReasoningTokens` field is for reporting transparency, not for re-counting.
- `UncachedInput` is computed as `PromptTokens − CachedTokens − CacheCreationTokens`. Anthropic reports `PromptTokens` as the **uncached** count already (cached subset is on `cache_read_input_tokens` and `cache_creation_input_tokens`); OpenAI reports `PromptTokens` as the **total** input count with `prompt_tokens_details.cached_tokens` as a subset. The `shared/normalize/anthropic_messages.go` extractor normalizes the Anthropic convention to the OpenAI convention (`PromptTokens = uncached + cached_read + cached_write`) before `Usage` reaches `CalculateCost`, so the subtraction is always correct.
- **2026-05-21 bugfix breadcrumb (commit `1cd611943`).** The implementation had drifted from this rule: `internal/ingress/proxy/proxy.go::computeCacheCosts` (the recompute path used after `traffic_event` row is initially stamped) had an Anthropic-only branch that left `regularInput = PromptTokens` (skipping the subtraction), under the false assumption that Anthropic still carried uncached-only semantics. But the normalizer at `packages/shared/transport/normalize/codecs/anthropic_messages.go:340-342` had already summed uncached + cache_read + cache_creation into the canonical `PromptTokens`. Result: Anthropic cached tokens were billed at the full input rate AND again at the cache rate. Production row `09b83222` (claude-opus-4-1) showed actual $0.247846 vs. correct $0.110235 — a 2.25× over-count. The fix removed the AdapterType branch and applies the subtraction unconditionally. Regression test pinned in `internal/ingress/proxy/proxy_cost_test.go::TestComputeCacheCosts/anthropic:_claude-opus-4_regression_(no_double-count)`.

### 3.3 The stamp sites — and the EstimatedCost semantic

The "token-field stamp sweep" rule from CLAUDE.md § 9 applies. The cost-stamp sites are:

1. `internal/ingress/proxy/proxy.go` `handleNonStream` (line 2361) — upstream non-stream response; `rec.EstimatedCostUsd = cost.Total` at line 2430.
2. `internal/ingress/proxy/proxy.go` `handleStream` — SSE upstream stream final-usage stamp.
3. `internal/ingress/proxy/proxy_cache.go` `handleStreamHit` (line 198) — cached SSE replay; `EstimatedCostUsd = wouldHaveCost` AND `GatewayCacheSavingsUsd = wouldHaveCost` (lines 238-243).
4. `internal/ingress/proxy/proxy_cache.go` `handleNonStreamHit` (line 270) — cached non-streaming response; same pair-stamp at lines 310-315.
5. `internal/ingress/proxy/proxy_cache.go` `handleStreamWithSubscription` (line 471) — in-flight stream singleflight joiner.
6. `internal/ingress/proxy/proxy_cache.go` `handleNonStreamWithSubscription` (line 772) — non-stream singleflight joiner.

In addition, `internal/ingress/proxy/proxy.go` `computeCacheCosts` (line 2882) is the **post-stamp recompute helper** that derives the per-row cache breakdown (`CacheReadSavingsUsd`, `CacheWriteCostUsd`, `CacheNetSavingsUsd`) and recomputes `EstimatedCostUsd` for the cache-aware case — invoked from the cache-hit sites after the initial stamp.

#### What `estimated_cost_usd` means

**`estimated_cost_usd` is the PREDICTED upstream-provider cost at the configured `Model` prices.** It is a property of the **request**, not of whether the request happened to hit any cache. Two requests with identical tokens against the same model produce the same `estimated_cost_usd` for sites 3 + 4 (the extract / semantic gateway hits — the cached entry's stored usage is replayed and `wouldHaveCost` is stamped).

**Exception — singleflight joiners (sites 5 + 6).** When the broker joiner detects `rec.GatewayCacheStatus == GatewayCacheHitInflight` (`proxy_cache.go:713` and `:907`), it deliberately zeros `EstimatedCostUsd` AND `GatewayCacheSavingsUsd = fullCost` (with `CacheReadTokens / CacheCreationTokens / CacheWriteCostUsd / CacheReadSavingsUsd / CacheNetSavingsUsd` zeroed) — because the leader request already accounted for the upstream spend on its own row. Stamping the joiner with `wouldHaveCost` would double-count in rollups (the same provider call paid for twice). This carve-out is documented inline at `proxy_cache.go:711-715` and `:904-908`.

Before 2026-05-21 sites 3 + 4 also stamped `EstimatedCostUsd = 0` on cache HITs, conflating "we didn't pay upstream this time" with "this request had no cost". The corrected stamping (`proxy_cache.go:226-243` + `:299-315`) computes `wouldHaveCost = tokens × Model prices via estimator.Lookup(endpointType)` and writes the same value to BOTH `estimated_cost_usd` and `gateway_cache_savings_usd` — making the math:

```
net upstream paid for THIS request = estimated_cost_usd − gateway_cache_savings_usd
                                   = 0 (on full gateway HIT)
                                   = estimated_cost_usd (on full MISS)
                                   = somewhere between (on partial provider-cache discount)
```

This separation lets dashboards show "predicted spend if no cache" alongside "savings" without re-deriving from raw tokens. It also makes rollup math straightforward: `SUM(estimated_cost_usd)` is total catalog-priced volume; `SUM(gateway_cache_savings_usd)` is the gateway-cache contribution to savings; their difference is "what would have been paid net of gateway caching".

#### The sites' stamping contract

| Sites | `estimated_cost_usd` | `gateway_cache_savings_usd` | `cache_read_savings_usd` | `cache_write_cost_usd` |
|---|---|---|---|---|
| 1, 2 — gateway MISS, normal upstream response | `CalculateCost(Usage, Prices).Total` (via `estimator.Lookup(endpointType)`) | `NULL` | recomputed from `cache_read_tokens` × discount | recomputed from `cache_creation_tokens` × surcharge |
| 3, 4 — gateway HIT (extract / semantic) | same formula but using the cached entry's `Usage` (= would-have-paid) | **= `estimated_cost_usd`** | `NULL` (no upstream call → no provider-side discount concept) | `NULL` |
| 5, 6 — singleflight joiner (`hit_inflight`) | `0` (the leader's row already stamped the real upstream cost) | `= fullCost` (recorded as "saved for this joiner") | `0` | `0` |

NOTE on sites 3-4: the cached entry's `Usage` is what was written by the seed call (the upstream MISS that populated the entry); the HIT row's cost columns are computed against that stored `Usage`. If `Usage` was never stored on a given entry, downstream HIT rows surface NULL cost columns honestly rather than fabricating a number. NOTE on sites 5-6: the joiner zero-out is deliberate to prevent rollup double-counting when the leader and joiner rows are summed.

#### The 3 HIT × MISS cases — see §6.5 for worked examples.

### 3.4 The reasoning-token cost split

`traffic_event` carries two derived columns for transparency:

```
reasoning_tokens        Int?       // = providers.Usage.ReasoningTokens (canonical Usage from S0)
reasoning_cost_usd      Decimal?   // = ReasoningTokens × OutputUsdPerM / 1e6
                                   // (already counted inside Cost.Output;
                                   //  this column is for "how much of the
                                   //  output cost was thinking?" reports)
```

The Cost dashboard surfaces the ratio `reasoning_cost_usd / cost_usd` per model. Operators use it to spot models where reasoning effort dominates spend, and to decide whether `reasoning_effort: low` would be a net win.

## 4. Estimator core

### 4.1 Package layout

```
packages/ai-gateway/internal/execution/estimator/
├── estimator.go                // public Estimate() entry point + EstimateInput/Result types
├── tokenize.go                 // Tokenizer interface + family-routed implementations
│                               // (character-ratio heuristic for all adapters in v1)
├── output_budget.go            // (model, reasoning_effort) → expected anchor; expandRange widens
├── reasoning.go                // reasoning_effort / thinking-budget extraction from canonical body
└── cost_formula_registry.go    // per-endpoint billable-units → cost formula (used by proxy stamp
                                // sites via estimator.Lookup(endpointType))
```

Cache-lookup is not wired in v1 (see §4.6). The package is `internal/` — never imported outside ai-gateway. Consumers (the `/v1/estimate` handler at `internal/ingress/proxy/estimate.go`; the `nexus.dry_run` branch at `dry_run.go`; the proxy stamp sites via `cost_formula_registry`) call into this package, not the other way around. Future consumers (cost guardrails, smart-routing cost-aware tiebreaker, budget forecast) join the same direction.

### 4.2 Entry point

```go
// packages/ai-gateway/internal/execution/estimator/estimator.go

type EstimateInput struct {
    CanonicalRequest []byte               // OpenAI-shape canonical JSON; caller already canonicalised
    IngressFormat    provcore.Format      // for reasoning-effort extraction
    Target           ResolvedTarget       // (Provider, Model) after routing dry-run
    Prices           metrics.ModelPrices  // looked up by caller from cache/layer
}

type ResolvedTarget struct {
    ProviderID    string
    ModelID       string
    ModelCode     string
    AdapterType   string
    MaxOutput     int // model.maxOutputTokens; clamps the high envelope
}

type EstimateResult struct {
    Tokens      TokenBreakdown     `json:"tokens"`
    Cost        CostBreakdown      `json:"cost"`
    Cache       CacheBenefit       `json:"cache"`
    Reasoning   ReasoningBreakdown `json:"reasoning"`
    Assumptions []string           `json:"assumptions,omitempty"`
}

type TokenBreakdown struct {
    UncachedInput int   // from tokenizer
    InputCached   int   // reserved for future prompt-cache prefix match (v1: 0)
    Output        Range // from output_budget table
    Reasoning     Range // included in Output but split for transparency
}

type Range struct {
    Low      int
    Expected int
    High     int
}

type CostBreakdown struct {
    Currency string       // "USD"
    Low      metrics.Cost
    Expected metrics.Cost
    High     metrics.Cost
}

type CacheBenefit struct {
    ResponseHitProbability float64 // v1: 0 (cache-lookup integration deferred)
    PromptCacheReadTokens  int     // v1: 0
    SavingsExpected        float64 // v1: 0
}

type ReasoningBreakdown struct {
    EffortRequested  string // "minimal" / "low" / "medium" / "high" / ""
    SupportedByModel bool
    EstimatedTokens  int    // expected anchor
}

// Estimate is the single entry point. Deterministic given inputs; no
// global state, no logger calls in the hot path. v1 does NOT touch the
// response cache or routing engine — the caller resolves Target first
// and Estimate just runs the tokenize → reasoning → output-budget →
// CalculateCost pipeline. Errors are limited to context cancellation
// and malformed canonical body.
func Estimate(ctx context.Context, in EstimateInput) (EstimateResult, error) { ... }
```

`CacheBenefit` fields stay zero in v1 — response-cache lookup integration is documented in §4.6 as a future enhancement, but the current `Estimate` (`estimator.go:130-189`) does not query the cache at all.

### 4.3 Tokenizer selection

In v1, every adapter family uses a character-ratio heuristic (`tokenize.go:40-52`). The `Tokenizer` interface is the seam — `pickTokenizer(adapterType)` routes by `provcore.Format` of `Target.AdapterType`:

| AdapterType (Format) | Tokenizer | Divisor |
|---|---|---|
| `FormatGemini`, `FormatVertex` | `heuristicTokenizer{divisor: 4.0}` | `chars / 4` — Gemini's SentencePiece coalesces slightly more aggressively than tiktoken. |
| All other formats (OpenAI, Azure, Anthropic, DeepSeek, Moonshot, etc.) | `heuristicTokenizer{divisor: 3.5}` | `chars / 3.5` — English-text typical for tiktoken-family tokenizers. |

The `tokenize.go:38-39` comment marks tiktoken-go as a future drop-in: the `Tokenizer` interface is stable so a real per-encoding tokenizer can replace the OpenAI/Azure heuristic without changing call sites. As of 2026-05-22 tiktoken-go is **not** wired and not in `packages/ai-gateway/go.mod`.

`Estimate` always appends a heuristic-accuracy disclaimer to `EstimateResult.Assumptions[]` (`estimator.go:145-149`): `"<adapterType> token count is a character-ratio heuristic (chars/<divisor>); ±10–15% typical error"`.

### 4.4 Output-budget table

`packages/ai-gateway/internal/execution/estimator/output_budget.go` carries the per-`(model code, reasoning_effort)` anchor table. `lookupOutputBudget(modelCode, effort)` (`output_budget.go:44-62`) returns `(anchor, supports)`; the caller calls `expandRange(anchor, maxOutput)` to widen to `low/expected/high`. As of 2026-05-22 the table covers:

- OpenAI reasoning: `gpt-5`, `gpt-5-mini`, `o3`, `o3-mini`, `o4-mini` — keys `minimal/low/medium/high`.
- Anthropic extended-thinking: `claude-opus-4-7`, `claude-opus-4-6`, `claude-sonnet-4-7`, `claude-sonnet-4-6`, `claude-haiku-4-5` — keys `low/medium/high` (no `minimal`).
- Gemini 2.5 thinking: `gemini-2.5-pro`, `gemini-2.5-flash` — keys `low/medium/high`.

Models not in the table return `(0, false)` and the caller falls back to a generic anchor at `max_tokens/4` (`estimator.go:156-162`). The table is editable as data — no code change is needed when a provider ships a new reasoning model or updates its guidance.

### 4.5 Reasoning effort detection

`reasoning.go` reads the canonical body and produces a normalized effort signal:

```go
type ReasoningSignal struct {
    Effort       string // "minimal" / "low" / "medium" / "high" / ""
    BudgetTokens int    // Anthropic thinking.budget_tokens / Gemini thinkingConfig.thinkingBudget
    Source       string // raw JSON path that supplied the signal (for Assumptions)
}

func ReadReasoningSignal(rawBody []byte, ingressFormat provcore.Format) ReasoningSignal { ... }
```

The lookup order (`reasoning.go:26-61`):

1. OpenAI chat-completions `reasoning_effort` (top-level string).
2. OpenAI Responses `reasoning.effort`.
3. Anthropic `thinking.budget_tokens` — raw or under `nexus.ext.anthropic.thinking.budget_tokens` (round-tripped via canonicalext).
4. Gemini `thinking_config.thinking_budget` — raw or under `nexus.ext.gemini.thinking_config.thinking_budget`.

Numeric budgets are bucketed by `bucketBudget` (`reasoning.go:64-75`): `≤0` → `""`; `<2000` → `low`; `2000-7999` → `medium`; `≥8000` → `high`.

This is the **only** place reasoning intent enters the estimator. There is no parallel `mode` parameter on `EstimateInput` — the canonical body is the single source.

### 4.6 Cache lookup (deferred)

Neither the estimator nor the callers (`internal/ingress/proxy/estimate.go`, `internal/ingress/proxy/dry_run.go`) consult the response cache or the prompt-cache prefix tables in v1. `dry_run.go:4-13` enshrines the architectural invariant that dry-run **never** touches the response cache — reading would conflate "what does this cost?" with "what would a prior identical call have cost"; writing from a synthetic body would poison subsequent real requests. `Estimate` therefore returns the request-shape × pricing answer only, and `EstimateResult.Cache` fields stay zero. Adding prompt-cache prefix matching is a future enhancement (§9).

### 4.7 Routing dry-run (performed by the caller)

`Estimate` does not invoke the routing engine — the caller resolves the target first (using the same routing engine the real request would use) and passes the resolved `Target` into `EstimateInput`. The caller's dry-run path short-circuits the smart strategy to its fallback chain's first entry, because the smart strategy would invoke an LLM to pick a model and make estimation more expensive than a real request. When this short-circuit fires, the caller appends `"smart routing dry-run used fallback chain entry; real request may resolve to a different target if the smart LLM picks one"` to `Assumptions[]` before returning the result.

## 5. The `nexus.dry_run` pipeline branch

### 5.1 The flag

`nexus.dry_run: true` lives in the `nexus.*` canonical extension namespace, per provider-adapter Rule 4. It is **a property of the canonical request**, not an HTTP header. Adapters that canonicalize the ingress body (every adapter goes through `canonicalbridge.IngressChatToCanonical`) carry the flag through without needing per-ingress wiring.

Concretely: a client sending `POST /v1/chat/completions` with body:

```json
{
  "model": "gpt-5",
  "messages": [{"role": "user", "content": "..."}],
  "reasoning_effort": "high",
  "nexus": {"dry_run": true}
}
```

— or the equivalent against `/v1/messages` (Anthropic ingress):

```json
{
  "model": "claude-sonnet-4-6",
  "messages": [{"role": "user", "content": "..."}],
  "thinking": {"type": "enabled", "budget_tokens": 8000},
  "nexus": {"dry_run": true}
}
```

— receives a normal-shape response with `usage` populated from the estimator.

### 5.2 Pipeline branching

The pipeline modification is **one branch point**:

```
ingress request
    │
    ▼
[ingress codec → canonicalize]
    │
    ▼
[canonical request, may contain nexus.dry_run]
    │
    ▼
[routing rule resolution]
    │
    ▼
[cache lookup (response-cache for real path; read-only for dry-run)]
    │
    ▼
[dispatch decision]:
    ├── nexus.dry_run == true  →  [estimator.Estimate]  →  [encode as ingress-format response w/ choices: [], usage: {...}]
    └── nexus.dry_run == false →  [executor → upstream] →  [response codec → ingress format]
```

The branch is in `proxy.go`. `isDryRun := canonicalext.IsDryRun(body)` is detected on the raw client body (line 441); dispatch via `h.dryRunDispatch(...)` happens **before** the response cache lookup in the cache-enabled arm (line 968) and is also reached on the cache-DISABLED / SKIP_NO_CACHE / PASSTHROUGH_SKIP branches (line 1056) — see the `dry_run.go:4-13` invariant: dry-run never touches the response cache (neither reads nor writes), because reading would conflate "what does THIS cost?" with "what would a prior identical call have cost" and writing from a synthetic body would poison subsequent real requests. Everything before the dispatch (canonicalization, routing engine) runs identically for real and dry-run.

### 5.3 Response encoding per ingress

The estimator returns an `EstimateResult`. `dry_run.go`'s `encodeDryRunResponse` dispatches by `ingress.BodyFormat` to produce a native success shape with empty content + populated usage block, per the per-ingress response codec:

| Ingress | Response shape produced |
|---|---|
| `/v1/chat/completions` (OpenAI Chat) | `{"id": "estimate-...", "object": "chat.completion", "model": "...", "choices": [], "usage": {...}}` |
| `/v1/responses` (OpenAI Responses) | OpenAI Responses-API success shape with empty `output` array + `usage` |
| `/v1/messages` (Anthropic) | Anthropic message shape with empty `content` + `usage` |
| `:generateContent` (Gemini) | Gemini shape with empty `candidates` + `usageMetadata` |

The full `EstimateResult` breakdown (low/expected/high, assumptions, cache benefit) is emitted in the `x-nexus-estimate` response header as compact JSON (`buildEstimateHeaderJSON`, `dry_run.go:417`). Clients that only want the headline number read `usage.total_tokens`; clients that want the full picture parse the header. On the streaming arm, the header is set before the first `data:` frame so SSE consumers see it on response-headers.

### 5.4 Streaming dry-run

Clients can request a streaming dry-run by setting `stream: true` alongside `nexus.dry_run: true`. The gateway emits exactly one SSE chunk with the usage block and then `[DONE]`. There is no token-by-token simulation — that would be more work than the estimate justifies, and clients that want to see partial token economics during a real run can read SSE chunks from the real path.

### 5.5 Cost guardrails (Phase 4 follow-up — not in E58 v1)

Because dry-run runs the canonical pipeline through routing + cache, a future "reject if estimated cost > VK's max-per-request" guard fits naturally:

```
[cache lookup]
    │
    ▼
[dispatch decision]:
    ├── nexus.dry_run == true                     →  [estimate path]
    ├── VK has max-cost-per-request configured    →  [estimate path] → if EstimateResult.Cost.High > limit → 402 Payment Required
    └── (otherwise)                                →  [executor]
```

This is called out here so the dry-run branch is designed to support it. The actual guard ships in a later story.

## 6. Cache savings UI contract (E59)

The Cache ROI page, the traffic-audit drawer, and the Overview cache savings widget all show cache savings. The UI surfaces exactly two concepts; "L1/L2/L3/L4" vocabulary does not appear in UI strings, i18n keys, API field names, or Prometheus labels.

### 6.1 The two real concepts

| Concept | What it is | How it's computed |
|---|---|---|
| **Gateway Cache savings** | The cost avoided when a request was served entirely from the Nexus response cache (the one in `packages/ai-gateway/internal/cache/`). No upstream call ⇒ full upstream cost avoided. | At cache-hit stamping (`proxy_cache.go` sites 3 / 4), compute what the request *would* have cost using `CalculateCost(usage, prices)` on the originally-stored usage, and stamp it as `cache_read_savings_usd` (the existing column). Sum across rows for the dashboard. |
| **Provider Prompt Cache savings** | The cost reduction when the upstream provider served some input tokens from its own prefix cache (Anthropic ephemeral cache, OpenAI auto prompt caching, Gemini cached content). The request still reached the upstream — just at a discount on input tokens. | `(inputPrice − cachedInputReadPrice) × CachedTokens − cachedInputWritePrice × CacheCreationTokens`. Already stamped as `cache_read_savings_usd` minus `cache_write_cost_usd` in the existing schema. |

### 6.2 What's removed

- All UI strings containing "L1", "L2", "L3", "L4" → renamed to one of the two concepts above.
- The `tooltipGatewayLayers` i18n key (en/zh/es), which described "L1 Exact-match · L2 Semantic · L3 Compressed" — L1 (extract) + L2 (semantic) are live since E61; L3 compressed does not exist in code. The tooltip's L1/L2/L3 framing is renamed to the user-visible "Gateway Cache" / "Provider Prompt Cache" concepts.
- The `colGroupL1L3` / `colGroupL4` column groupings → `colGroupGatewayCache` / `colGroupProviderPromptCache`.
- The `cacheSavingsDesc` "(L1–L4)" suffix in the Overview widget.
- Backend JSON field `totalL4CacheNetSavingsUsd` → `totalProviderPromptCacheNetSavingsUsd`. Same for the per-adapter / per-day breakdowns.
- Prometheus metric `MetricRequestsWithL4CacheHit` → `MetricRequestsWithProviderPromptCacheHit` (with a one-release alias for ops continuity if dashboards reference the old name).
- The `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md` "L1/L2/L3/L4" content is restructured: the doc now describes the two real response cache tiers — extract (exact-match) and semantic (vector-similarity, live since E61) — plus the provider-side observation. Any historical "L3 compressed" framing is removed.

### 6.3 No admin "cache strategies" panel

Per-tier cache configuration lives on `CacheAdapterConfig`. No admin strategies panel exists.

### 6.4 Unified cache_status state model

The `traffic_event.cache_status` column is the **unified, user-visible** cache outcome. Filter dropdowns (Live Traffic, Cache ROI) and the drawer's headline "Cache" line bind to this single field, with only two values: `HIT` and `MISS`. The detail panel exposes the internal breakdown — but the filter UX never asks the user "which layer hit".

#### Five fields on traffic_event

| Field | Values | Set when |
|---|---|---|
| `cache_status` *(unified)* | `HIT` \| `MISS` | Derived at write time from the two internal status columns. Single field every UI filter binds to. |
| `gateway_cache_status` | `hit` \| `hit_inflight` \| `miss` \| `skipped` | Set by the gateway cache decision point (`proxy_cache.go`). `hit_inflight` covers the singleflight-coalescer case. |
| `gateway_cache_skip_reason` | `disabled` \| `no_cache` \| `passthrough` \| `not_cacheable` | Only set when `gateway_cache_status = 'skipped'`. |
| `gateway_cache_kind` | `extract` \| `semantic` | Only set on hits. Both values active since E61 — `extract` for exact-match L1 hits and `hit_inflight` singleflights, `semantic` for L2 vector-similarity hits stamped by `proxy_cache.go` on the semantic-tier read path (`packages/ai-gateway/internal/cache/semantic/`). |
| `gateway_cache_l2_entry_key` | TEXT (`<index>:<hash>`) | Set only when `gateway_cache_kind = 'semantic'`. Carries the L2 entry's Redis HASH key (`<redis_index_name>:<sha256(EmbeddingInput)[:16]>`) so the audit drawer's "Mark as bad cache hit" thumbs-down can post the exact key the gateway's IsPoisoned check will consult on the next FT.SEARCH hit. NULL on extract hits, MISS, SKIPPED, and on rows from compliance-proxy / agent (no L2). |
| `provider_cache_status` | `hit` \| `miss` \| `na` | Derived from provider response usage via `audit.ClassifyProviderCache`. `hit` when `cache_read_tokens > 0`; `miss` when at least one of `cache_read_tokens` / `cache_creation_tokens` is non-NULL but no read hit (covers both "called, no cache use" and "first-turn cache WRITE" where only `cache_creation_tokens > 0`); `na` when both are NULL — meaning the gateway served without calling upstream OR the model doesn't support prompt caching. Drawer disambiguates `na` via `gateway_cache_status`: when gateway served, render "served from gateway cache"; otherwise render "model doesn't support prompt cache". |

#### Derivation rule

```
unified.cache_status = HIT  iff  gateway_cache_status ∈ {hit, hit_inflight}
                                 OR provider_cache_status = 'hit'
                       MISS  otherwise
```

#### Valid combinations (8 total — others rejected at write time)

| Gateway | Provider | Unified | Scenario |
|---|---|---|---|
| `hit` | `na` | **HIT** | Gateway extract cache served; no upstream call |
| `hit_inflight` | `na` | **HIT** | Singleflight coalesce; leader's response replayed |
| `miss` | `hit` | **HIT** | Gateway miss; provider prompt-cache discount applied |
| `miss` | `miss` | **MISS** | Full upstream cost |
| `miss` | `na` | **MISS** | Gateway miss; provider doesn't support prompt cache |
| `skipped` | `hit` | **HIT** | Gateway bypassed; provider still discounted |
| `skipped` | `miss` | **MISS** | Bypassed + no provider discount |
| `skipped` | `na` | **MISS** | Bypassed + provider doesn't support |

Invalid combinations (gateway `hit`/`hit_inflight` paired with provider `hit`/`miss`) are impossible — when gateway serves, no provider call happens — and the audit writer rejects them.

#### Why a stored derived column instead of computed-on-read

`traffic_event` is high-write, low-update. Computing once at write time and storing keeps the filter index single-column. Recomputing on every analytics query (`CASE WHEN ...`) is slower and harder to index.

#### Filter UX contract

- **Filter dropdown** (Live Traffic, Cache ROI): `Any | HIT | MISS`. Three values. No other.
- **Detail drawer "Cache" block**: one of three layouts depending on the (gateway, provider) pair.

#### Drawer rendering (three layouts)

| Layout | Triggered when | Headline | Body |
|---|---|---|---|
| **Gateway-served** | `gateway_cache_status ∈ {hit, hit_inflight}` | `HIT` | `Gateway: HIT (extract)` or `HIT (in-flight)` · `Provider: N/A — gateway served` · "Saved $X" (from `gateway_cache_savings_usd`) |
| **Provider-discount** | `gateway_cache_status ∈ {miss, skipped}` AND `provider_cache_status = 'hit'` | `HIT` | `Gateway: MISS` or `Skipped — <reason>` · `Provider: HIT — N cached tokens` · "Net saved $Y" (from `cache_net_savings_usd`) |
| **No savings** | `gateway_cache_status ∈ {miss, skipped}` AND `provider_cache_status ∈ {miss, na}` | `MISS` | `Gateway: MISS` / `Skipped — <reason>` · `Provider: MISS` (model supports cache, no read hit — may include cache-WRITE surcharge if `cache_creation_tokens > 0`) or `N/A — model doesn't support prompt cache` (both cache token columns NULL). |

The drawer never renders the raw enum value — always the human label per the table above.

### 6.5 The three HIT × MISS cases — worked examples

Two cache layers (Nexus gateway + provider-side) produce **three distinct cost outcomes** per request. Every column on `traffic_event` and every UI panel reflects one of these three. The cases are not just naming — the cost-field semantic differs between them. Memorize this table:

#### Case A — Gateway HIT (extract / semantic)

Nexus gateway served from L1 (extract) or L2 (semantic) cache. **No upstream call was made.**

```
gateway_cache_status = 'hit'  ;  provider_cache_status = 'na'
cache_read_tokens    = NULL/0 (no upstream call → no provider-cache concept)
cache_creation_tokens = NULL/0

estimated_cost_usd        = prompt_tokens × Model.inputPricePerMillion
                          + completion_tokens × Model.outputPricePerMillion
                            (the "would have paid at sticker if we'd called upstream")

gateway_cache_savings_usd = same as estimated_cost_usd   ← we saved 100% of it
cache_read_savings_usd    = NULL                          ← N/A (no upstream call)
cache_write_cost_usd      = NULL                          ← N/A

embedding_cost_usd        = $X if semantic cache ran the lookup (regardless of hit/miss)
ai_guard_cost_usd         = $Y if ai-guard hook fired

NET COST OF THIS REQUEST = (estimated − gateway_cache_savings) + embedding + ai_guard
                         = 0 + embedding + ai_guard
                         = internal-ops only (operator's hosting cost, not upstream provider cost)
```

#### Case B — Gateway MISS + Provider HIT (prompt-cache discount)

Gateway didn't cache it. Upstream WAS called. The provider gave us their own prompt-cache discount (Anthropic ephemeral cache, OpenAI auto prompt caching, Gemini cached content). `cache_read_tokens > 0` is the signal.

```
gateway_cache_status = 'miss'  ;  provider_cache_status = 'hit'
cache_read_tokens    > 0
cache_creation_tokens = optional (0 if reading pre-existing cache; >0 if this request also wrote a new cache prefix)

uncached_tokens = prompt_tokens − cache_read_tokens − cache_creation_tokens

estimated_cost_usd        = uncached × Model.inputPricePerMillion
                          + cache_read × Model.cachedInputReadPricePerMillion       (discounted)
                          + cache_creation × Model.cachedInputWritePricePerMillion  (surcharge)
                          + completion × Model.outputPricePerMillion
                            (= what we actually paid upstream — the discount is BAKED IN)

gateway_cache_savings_usd = NULL    ← gateway didn't save anything this time
cache_read_savings_usd    = cache_read × (input_pm − cachedInputRead_pm)    ← discount we got from provider
cache_write_cost_usd      = cache_creation × cachedInputWrite_pm            ← surcharge we paid (negative-savings)
cache_net_savings_usd     = cache_read_savings_usd − cache_write_cost_usd

embedding_cost_usd        = $X if L2 lookup ran (and presumably missed — that's why we're here)
ai_guard_cost_usd         = $Y if hook fired

NET COST OF THIS REQUEST = estimated_cost_usd + embedding + ai_guard
                         = (real upstream bill, already reflects provider's discount) + internal-ops
```

#### Case D — Gateway MISS + Provider cache WRITE only (first-turn cache populate)

Gateway didn't cache it. Upstream was called. Request carried `cache_control` (Anthropic) or implicitly populated a provider cache prefix, so the provider charged the **write surcharge** but there was no pre-existing entry to read from. Signature: `provider_cache_status = 'miss'` AND `cache_creation_tokens > 0` AND `cache_read_tokens` NULL/0. Cost-wise this turn is a net loss vs. sticker (you paid the surcharge for cache the next turn will benefit from). Distinct from Case B because there's no read discount yet; distinct from Case C because cache machinery did run.

```
gateway_cache_status = 'miss'  ;  provider_cache_status = 'miss'
cache_read_tokens    = NULL/0
cache_creation_tokens > 0

uncached_tokens = prompt_tokens − cache_creation_tokens

estimated_cost_usd        = uncached × Model.inputPricePerMillion
                          + cache_creation × Model.cachedInputWritePricePerMillion  (surcharge)
                          + completion × Model.outputPricePerMillion

gateway_cache_savings_usd = NULL
cache_read_savings_usd    = NULL                                        ← N/A (no read)
cache_write_cost_usd      = cache_creation × cachedInputWrite_pm        ← surcharge paid
cache_net_savings_usd     = -cache_write_cost_usd                       ← net-negative for this turn
```

Bug-fix breadcrumb (2026-05-22). Before `audit.ClassifyProviderCache` was extracted, the three inline switch sites used only `cache_read_tokens` to derive `provider_cache_status`, so this case was misclassified as `na` ("model doesn't support prompt cache") even though `cache_creation_tokens` proved the provider had run cache machinery. The drawer then rendered the misleading "N/A — model doesn't support prompt cache" alongside a populated Cache Creation Tokens block. Helper + table-driven test pinned the truth table; all three call sites now delegate.

#### Case C — Full MISS (no gateway cache, no provider cache)

Default case. Upstream called at full sticker price.

```
gateway_cache_status = 'miss' or 'skipped'  ;  provider_cache_status = 'miss' or 'na'
cache_read_tokens    = NULL/0
cache_creation_tokens = NULL/0

estimated_cost_usd        = prompt × input_pm + completion × output_pm   (full sticker)
gateway_cache_savings_usd = NULL
cache_read_savings_usd    = NULL
cache_write_cost_usd      = NULL

embedding_cost_usd        = $X if L2 lookup ran
ai_guard_cost_usd         = $Y if hook fired

NET COST OF THIS REQUEST = estimated_cost_usd + embedding + ai_guard
```

#### Cross-cutting math identity

```
net_paid_upstream_for_request = estimated_cost_usd − COALESCE(gateway_cache_savings_usd, 0)

total_cost_to_operator        = net_paid_upstream_for_request
                              + COALESCE(embedding_cost_usd, 0)
                              + COALESCE(ai_guard_cost_usd, 0)
```

The UI Costs Breakdown panel in the Traffic Event drawer renders all four case shapes from a single component (`packages/control-plane-ui/src/pages/traffic/audit-drawer/trafficAuditDrawer.tsx`), selecting which rows to display based on which token columns are populated. Case A always shows `uncached + output`; Case B shows all four (`uncached + cache_read + cache_creation + output`); Case D shows `uncached + cache_creation + output` (write surcharge only, no read line); Case C is identical structure to A on the upstream side but with `gateway_cache_savings_usd = NULL`.

#### Subtle pitfall — gateway HIT row's `cache_read_tokens` ≠ "the original T1 was provider-cached"

A Case-A row's `cache_read_tokens = NULL/0` says "for THIS serving, no upstream call → no provider-cache concept". It does NOT mean the original seed call (T1) wasn't provider-cached when it was actually issued. The cached entry doesn't carry that context across replays. If you need to know whether T1 hit provider cache, look up T1's own row by trace.

### 6.6 Internal-ops costs — embedding & ai-guard

Beyond the upstream provider bill, Nexus runs two internal-ops paths that **also cost real money** (we call providers for them):

| Internal op | When | Cost source | Column on `traffic_event` |
|---|---|---|---|
| **Semantic-cache embedding lookup** | Every request where the L2 semantic cache attempts a lookup (regardless of HIT/MISS) — the embedding call itself is unconditional once L2 is enabled. | Embedding-Model row prices × tokens. Default model `text-embedding-3-small` at $0.02/M. | `embedding_cost_usd` |
| **AI-Guard classifier call** | When an ai-guard hook fires and the **internal** ai-guard backend is configured (`configured_provider` mode using our own provider). External-URL ai-guard backends don't bill us — `external_url` mode is explicitly OUT of scope; see `backend_external.go` for the COST SEMANTICS docstring. | The classifier model's Model row × tokens consumed. | `ai_guard_cost_usd` |

#### Stamping path

Both columns are stamped via the standard `audit.Record` → MQ `TrafficEventMessage` → Hub consumer → `traffic_event` INSERT plumbing. The producer sites:

- `packages/ai-gateway/internal/cache/semantic/reader.go` — stamps `rec.EmbeddingCostUsd` on every L2 attempt.
- `packages/ai-gateway/cmd/ai-gateway/wiring/aiguard.go` — `WriterBackedTrafficSink` adapter that wraps each ai-guard classifier call as its own `audit.Record` with `ai_guard_cost_usd` set, then emits via the same audit Writer. The wiring (`uuid.NewString` for ID + `StatusCode: http.StatusOK`) was added 2026-05-21 to fix a silent-drop bug where ai-guard rows had `id=""` and hit `ON CONFLICT (id) DO NOTHING`.

#### YAML toggle: `excludeInternalOpsFromBilledCost`

```yaml
# nexus-hub.yaml
rollup:
  excludeInternalOpsFromBilledCost: false   # default — DO count these against the VK quota
```

The toggle controls how the rollup job aggregates these columns into the **billed** cost metric:

```go
// packages/nexus-hub/internal/jobs/defs/rollup/rollup_5m.go
if isSuccess && !cacheHitVal {
    billed := cost
    if !j.excludeInternalOpsFromBilled {
        billed += derefFloat5m(embeddingCostUsd) + derefFloat5m(aiGuardCostUsd)
    }
    add(metrics.MetricBilledCostUSD, billed)
}
```

Default `false` means **internal-ops costs DO count against the VK's billed quota**, matching the operator-pays-the-bill-regardless-of-who-triggered-it intuition ("都是钱"). An operator who wants to absorb internal-ops costs separately (e.g., charge customers a flat rate that already includes a margin) flips this to `true`.

The CP UI's Costs Breakdown panel mirrors the toggle: the "Internal-ops" section's totalizer is captioned with `internalOpsNoteCounted` (toggle = false) vs `internalOpsNoteExcluded` (toggle = true), and the "vs no-gateway baseline" math adjusts the savings line accordingly.

#### ai-guard external-URL backend is explicitly NOT billed

If an admin configures ai-guard to call an **external URL** (their own deployed scanner, a 3rd-party service), `ai_guard_cost_usd` is NEVER stamped — the external party bills the operator out of band; Nexus has no visibility into their pricing. The `backend_external.go` docstring + regression test `TestExternalBackend_NoCostStamping_EvenWithUsageInResponse` pin this contract.

## 7. Historical recompute — runbook references

When a cost-stamping bug ships, the gateway code can be fixed forward, but **historical rows in `traffic_event` + every rollup tier are still wrong** and need a one-shot recompute. The infrastructure for this is two SQL scripts + the prod-deploy runbook:

| Asset | Path | Purpose |
|---|---|---|
| Recompute traffic_event | `tools/db-migrate/manual-scripts/recompute_traffic_event_costs_2026_05_21.sql` | Chunked PL/pgSQL loop (5000 rows / chunk, 50 ms sleep, per-chunk tx) recomputes the 5 cost columns on every `traffic_event` row from current Model prices. Plus a POST-PASS that handles Case-A HIT rows separately. |
| Reset rollups | `tools/db-migrate/manual-scripts/reset_rollup_after_cost_recompute_2026_05_21.sql` | DELETEs affected `metric_rollup_*` and `thing_metric_rollup_*` buckets, resets the 6 watermarks so the rollup cron re-aggregates from the corrected raw rows. |
| Deploy runbook §3.2 | `docs/operators/ops/runbooks/prod-deploy-data-changes.md` | Full prod-ops procedure: backup commands, run order, mid-run progress monitoring, rollup catch-up time estimates (default ~6 days for a 1-month backlog; documented knob to lower `scheduler.intervals.rollup5m` for faster catch-up), rollback scope. |

**Contract for future cost-stamping changes:** when you change cost math in `proxy.go` / `proxy_cache.go` / `metrics.CalculateCost`, AND the change affects how historical rows would now be calculated differently, write a parallel recompute script (or extend the existing one) in the same PR. Always pair the script with a runbook update (per the code/doc lockstep rule).

## 8. Compatibility with existing instrumentation

E58 preserves existing observability surfaces:

- `cost_usd` column on `traffic_event` continues to be the per-row total. After E58-S1 it equals `Cost.Total` (the sum of the four components). Pre-S1 rows already in production are not retroactively recomputed — historical rows used the old three-field formula, which is correct for rows where cache tokens were always zero (true for non-Anthropic providers up to E58) and incomplete only for the small share of Anthropic traffic that had cache events. The dashboard "since 2026-MM-DD" notation surfaces the change date.
- `cache_creation_tokens`, `cache_read_tokens`, `cache_creation_cost_usd`, `cache_read_savings_usd`, `cache_net_savings_usd` columns continue to be the source of cache analytics. They are populated by the same `CalculateCost` outputs.
- `reasoning_tokens` and `reasoning_cost_usd` are **new** columns added in E58-S1. Pre-S1 rows have NULL; the Cost dashboard shows "no data" rather than zero for periods before the migration.

## 9. Open questions tracked here

- **Tiered Anthropic cache TTLs.** Anthropic ships two cache windows (5 min, 1 hour) with different prices. Today both fold into `cachedInputWritePricePerMillion`. When pricing diverges, split into `cachedInputWrite5mPricePerMillion` + `cachedInputWrite1hPricePerMillion` — that's a `Model` schema addition, a `Cost` struct addition, and a small estimator change.
- **Multi-modal pricing (audio, image, video).** OpenAI gpt-4o-realtime already prices audio separately. When a customer uses it at volume, add `audioInputPricePerMillion` (and corresponding `Usage.AudioInputTokens` per `normalization-architecture.md` § 10).
- **Time-effective pricing history.** Today `AdminAuditLog` records price changes for audit. If billing reconstruction at past prices becomes a requirement, add a `ModelPriceHistory` table with `(modelId, effectiveFrom, four price fields)`. The runtime read path doesn't change — it still reads the current `Model` row — only the audit / reconstruction queries hit the history table.
- **Pre-flight estimation accuracy bounds.** The current low / high envelope is hard-coded as `expected/3` and `expected*3`. Once enough dry-run-vs-real comparisons accumulate (we log both for matched request hashes), we can replace the constants with empirical per-model percentiles.

When any of these become real requirements, file an architecture-doc PR before changing data shapes.

## 10. Where to read next

- `normalization-architecture.md` — the upstream of this pipeline. Where the parser layer lives and where the canonical NormalizedPayload (including Usage) is produced. Read § "Ai-gateway codec delegation (E58-S0)" for the spec_* → shared/normalize delegation contract.
- `provider-adapter-architecture.md` § 3a — the canonical ↔ wire translation rules that constrain how the dry-run flag and the estimate response shape interact with each ingress.
- `cache-multi-tier-architecture.md` — post-E59, the single source of truth for what caches actually exist in the codebase (Redis response cache + Gemini cachedContent + stream coalescer + config caches).
- `prompt-cache-architecture.md` — the upstream prompt-cache machinery that feeds the `CachedTokens` / `CacheCreationTokens` numbers.
- `routing-architecture.md` — the routing engine that the estimator's dry-run uses.
- `docs/operators/ops/runbooks/prod-deploy-data-changes.md` §3.2 — runbook for recomputing historical `traffic_event` cost columns + invalidating affected rollup buckets when a cost-stamping bug ships.
- `tools/db-migrate/manual-scripts/recompute_traffic_event_costs_2026_05_21.sql` — canonical pattern for chunked, prod-safe historical cost recomputes.
- `CLAUDE.md` "Code / doc lockstep" binding rule + `.cursor/rules/code-doc-lockstep.mdc` — the contract that keeps this doc and the cost-stamping code paths from drifting again; enforced via `npm run check:doc-lockstep`.
