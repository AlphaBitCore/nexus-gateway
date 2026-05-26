# E58-S2 — Estimator Core Package

> Story: e58-s2
> Epic: 58
> Status: Draft
> Requirements: `docs/developers/specs/e58/e58-cost-estimation-and-cache-pricing.md` § FR-4
> Architecture: `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` § 4
> Blocks: E58-S3 (dry_run pipeline branch invokes Estimate); E58-S4 (compare endpoint dispatches Estimate per target)
> Blocked by: E58-S0 (estimator's hypothetical "what if we sent this request" path needs the canonical Usage shape produced by `canonicalbridge.DecodeViaShared`; the static output budget anchor still produces low/expected/high `providers.Usage` values that flow into `CalculateCost`), E58-S1 (uses ModelPrices + CalculateCost)

## User Story

As a platform that needs to compute cost estimates for pre-flight checks (S3 dry_run), model comparisons (S4 /v1/estimate), cost guardrails (Phase 4), and budget forecasts (Phase 4), I want a single internal Go package — `packages/ai-gateway/internal/execution/estimator/` — that turns a canonical request into a `(low, expected, high)` cost envelope with assumptions, given a resolved target and a price snapshot. The package is pure (no HTTP, no DB), and its outputs reuse the exact `metrics.Cost` struct that real requests stamp, so estimates and reality use the same vocabulary at the database level.

## Tasks

### T1 — Package skeleton

- T1.1 Create `packages/ai-gateway/internal/execution/estimator/` with:
    - `doc.go` — package docstring summarizing the contract from `cost-estimation-architecture.md` § 4.
    - `estimator.go` — `Estimate()` entry point + types.
    - `tokenize.go` — Tokenizer interface + family selector.
    - `tokenize_openai.go` — tiktoken-go binding.
    - `tokenize_anthropic.go` — character-ratio heuristic.
    - `tokenize_gemini.go` — character-ratio heuristic.
    - `output_budget.go` — per-(model, reasoning_effort) table lookup.
    - `output_budget_table.go` — the data.
    - `cache_lookup.go` — read-only response-cache + prompt-cache prefix lookup.
    - `routing.go` — routing engine dry-run wrapper.
    - `reasoning.go` — reasoning effort extraction from canonical body.
- T1.2 The package is `internal/` — no consumers outside ai-gateway.

### T2 — Public types

```go
// packages/ai-gateway/internal/execution/estimator/estimator.go

type EstimateInput struct {
    Canonical    canonicalbridge.CanonicalRequest
    Target       ResolvedTarget
    Prices       metrics.ModelPrices
    VKID         string
    LookupCache  bool
}

type ResolvedTarget struct {
    ProviderID    string
    ModelID       string
    ModelCode     string
    AdapterType   string                  // "openai", "anthropic", "gemini", etc.
    MaxOutput     int                     // for clamping high envelope
}

type EstimateResult struct {
    Tokens      TokenBreakdown
    Cost        CostBreakdown
    Cache       CacheBenefit
    Reasoning   ReasoningBreakdown
    Assumptions []string
}

type TokenBreakdown struct {
    UncachedInput int
    InputCached   int
    Output        struct{ Low, Expected, High int }
    Reasoning     struct{ Low, Expected, High int }
}

type CostBreakdown struct {
    Currency string
    Low      metrics.Cost
    Expected metrics.Cost
    High     metrics.Cost
}

type CacheBenefit struct {
    ResponseHitProbability float64
    PromptCacheReadTokens  int
    SavingsExpected        float64
}

type ReasoningBreakdown struct {
    EffortRequested   string
    SupportedByModel  bool
    EstimatedTokens   int
    BudgetTokens      *int
}
```

### T3 — `Estimate()` entry point

- T3.1 Implementation sketch:
    ```go
    func Estimate(ctx context.Context, in EstimateInput) (EstimateResult, error) {
        var assumptions []string

        // 1. Tokenize input
        tk := pickTokenizer(in.Target.AdapterType)
        uncachedInput := tk.CountTokens(in.Canonical.Messages)
        if tk.IsHeuristic() {
            assumptions = append(assumptions, fmt.Sprintf("%s token count is a character-ratio heuristic, ±10%% typical error", in.Target.AdapterType))
        }

        // 2. Read reasoning effort
        rs := ReadReasoningSignal(in.Canonical)

        // 3. Output budget
        outBudget, supported := lookupOutputBudget(in.Target.ModelCode, rs)
        if !supported && rs.Effort != "" {
            assumptions = append(assumptions, fmt.Sprintf("model %s does not support reasoning; reasoning_effort=%s ignored", in.Target.ModelCode, rs.Effort))
        }

        // 4. Cache lookup (read-only)
        var cacheBenefit CacheBenefit
        if in.LookupCache {
            cacheBenefit = lookupCache(ctx, in)
        }

        // 5. Compute low/expected/high
        return buildResult(in, uncachedInput, cacheBenefit, outBudget, rs, assumptions)
    }
    ```
- T3.2 The function is **deterministic** given inputs (modulo cache state at the time of call). No global state, no logger calls in the hot path.
- T3.3 Error returns are limited to context cancellation and tokenizer initialization failure. No "invalid request" errors — those are caught at the canonicalization step before Estimate is called.

### T4 — Tokenizer

- T4.1 `Tokenizer` interface:
    ```go
    type Tokenizer interface {
        CountTokens(messages []canonicalbridge.Message) int
        IsHeuristic() bool
    }
    ```
- T4.2 `tokenize_openai.go` wraps `github.com/pkoukk/tiktoken-go` (or equivalent). Per-model encoding lookup via the tiktoken model→encoding map; falls back to `cl100k_base` for unknown models.
- T4.3 `tokenize_anthropic.go`: `chars/3.5` (documented heuristic; Anthropic's official `count_tokens` API is intentionally NOT used per FR-4 — latency cost).
- T4.4 `tokenize_gemini.go`: `chars/4`.
- T4.5 The `pickTokenizer(adapterType)` selector returns:
    - `openai`, `azure_openai`, `deepseek`, `glm`, `groq`, `moonshot`, `xai`, `mistral`, `perplexity`, `fireworks`, `together`, `huggingface`, `replicate`, `cohere` → OpenAI/tiktoken
    - `anthropic`, `bedrock` → Anthropic heuristic
    - `gemini`, `vertex` → Gemini heuristic
    - unknown → OpenAI/tiktoken as the default, with an assumption note.
- T4.6 tiktoken-go is added to `packages/ai-gateway/go.mod` ONLY — NOT to `packages/shared/go.mod`. Per CLAUDE.md "shared dependencies vetted set" rule.

### T5 — Output budget table

- T5.1 `output_budget_table.go` content per `cost-estimation-architecture.md` § 4.4 (one map literal). Each model entry has a comment citing the vendor docs section that justifies the numbers.
- T5.2 `lookupOutputBudget(modelCode, ReasoningSignal) (budget, supported)`:
    - Try exact match on `modelCode` first.
    - For models not in the table: return `(defaultBudget, false)` where `defaultBudget.Expected = clamp(maxOutput / 4, 100, 2000)` (rough heuristic, intentionally conservative).
    - For models in the table without reasoning support, ignore `reasoning_effort`.
- T5.3 Calibration script `scripts/estimator-calibration/calibrate.go`:
    - Reads a list of prompts from `scripts/estimator-calibration/prompts.json`.
    - Sends each through each reasoning-capable model at each effort level (uses real VK).
    - Records measured `reasoning_tokens` and the average.
    - Outputs a new `output_budget_table.go` for the maintainer to review and commit.
- T5.4 The calibration is offline tooling; not run in CI.

### T6 — Reasoning effort extraction

- T6.1 `reasoning.go`:
    ```go
    type ReasoningSignal struct {
        Effort       string  // "minimal"/"low"/"medium"/"high"/""
        BudgetTokens *int
        Source       string  // for assumptions
    }
    func ReadReasoningSignal(c canonicalbridge.CanonicalRequest) ReasoningSignal
    ```
- T6.2 Reading rules:
    - OpenAI ingress: canonical body has `reasoning_effort` at root → read directly.
    - Anthropic ingress: canonical body has `nexus.ext.anthropic.thinking.budget_tokens` (the canonicalext lift from the original `thinking.budget_tokens`) → map to bucket: `<2000`=low, `2000-7999`=medium, `>=8000`=high.
    - Gemini ingress: same pattern via `nexus.ext.gemini.thinking_config.thinking_budget`.
- T6.3 If both `Effort` and `BudgetTokens` are present (e.g., a power-user explicitly setting both), `BudgetTokens` wins; `Effort` is reported in `Source` as "redundant — budget wins".

### T7 — Cache lookup (read-only)

- T7.1 `cache_lookup.go`:
    - Computes the same cache key the real path would use (`packages/ai-gateway/internal/cache/key.go` `NormalizeKey` function).
    - Calls Redis with the key; if hit, returns `CacheBenefit{ResponseHitProbability: 1.0}`.
    - For prompt-cache: invokes the same prefix-matching logic the real path uses (`packages/ai-gateway/internal/prompt_cache_lookup.go` or equivalent).
- T7.2 Both calls are read-only — Redis `GET` not `SET`; prefix lookup is computation-only.
- T7.3 If `EstimateInput.LookupCache == false` (set by callers that don't want the latency or want to estimate "from scratch"), returns zero `CacheBenefit`.

### T8 — Routing dry-run

- T8.1 `routing.go` wraps the routing engine's `Resolve` function with one alteration: when the matched rule's strategy is `smart`, the dry-run short-circuits to the strategy's fallback chain first entry instead of invoking the smart LLM dispatcher.
- T8.2 Adds an assumption: "smart routing dry-run used fallback chain entry; real request may resolve to a different target if the smart LLM picks one".
- T8.3 The function returns the same `ResolvedTarget` type the executor uses, so the estimator's input is symmetric with the real-path's resolved target.
- T8.4 Tests verify: (a) `single` strategy returns the configured target; (b) `fallback` strategy returns the first chain entry; (c) `loadbalance` strategy returns the first listed target (with assumption note "load-balance dry-run picked first target — real request may pick differently"); (d) `smart` strategy short-circuits with the documented assumption.

### T9 — Building the result

- T9.1 The low/expected/high envelope construction:
    - Output expected = `outBudget.Expected` (clamped to `target.MaxOutput`).
    - Output low = `max(50, outBudget.Expected / 3)`.
    - Output high = `min(target.MaxOutput, outBudget.Expected * 3)`.
    - Reasoning envelope mirrors the budget table's spread.
- T9.2 The three Cost values are computed by three separate `metrics.CalculateCost` calls with three different `Usage` snapshots — same function the real path uses. This is the structural guarantee that estimate units are reality units.
- T9.3 Assumptions are accumulated through the function and included verbatim in the result; tests assert specific assumptions appear for specific inputs.

### T10 — Documentation

- T10.1 `cost-estimation-architecture.md` already covers the architecture; this SDD's "Data Model" section captures the concrete types.
- T10.2 The package's `doc.go` is generously commented; new contributors should be able to understand the package by reading `doc.go` + `estimator.go` alone.

## Acceptance Criteria

| ID | Acceptance |
|---|---|
| AC-1 | `estimator.Estimate(ctx, input)` for a given input is deterministic (same input + same cache state → same output). |
| AC-2 | For an OpenAI-family target, p99 latency < 50 ms including tokenization, routing dry-run, and cache lookup. |
| AC-3 | For an Anthropic/Gemini target, p99 latency < 5 ms (heuristic tokenizer is much faster than tiktoken). |
| AC-4 | The three `metrics.Cost` values inside `CostBreakdown` are computed by `metrics.CalculateCost` — verified by a test that asserts byte-for-byte equality with a hand-rolled call. |
| AC-5 | A smart routing strategy in `EstimateInput.Target`'s rule resolution short-circuits to fallback; the assumption note is present. |
| AC-6 | Reasoning effort is extracted from canonical body correctly for all three ingress shapes (OpenAI/Anthropic/Gemini), tested per ingress. |
| AC-7 | Unit test coverage ≥95 % for `estimator` package. |
| AC-8 | tiktoken-go appears in `packages/ai-gateway/go.mod` only; not in `packages/shared/go.mod`. |
| AC-9 | The output budget table has at least 8 reasoning-capable model entries with vendor-doc references in comments. |
| AC-10 | The calibration script runs end-to-end against a sandbox VK and produces a non-empty diff against the checked-in table — proof that the calibration pipeline works. |

## Data Model

See T2 + T3 + T6. All public types are in `estimator.go`; nothing else is exported from the package besides `Estimate()`.

## Testing strategy

- **Unit (white-box)** per file: tokenize_*, output_budget, reasoning, cache_lookup, routing, estimator.
- **Property-based**: token counts are non-negative; cost envelope is monotonic (low ≤ expected ≤ high); assumptions list does not contain duplicates.
- **Integration**: A full `Estimate` call with a real `cachelayer` + a real routing engine but a fake upstream, asserting the result matches expectations for known prompts.
- **Calibration verification**: Run the calibration script in a dedicated test against pre-recorded responses (no live API hits) and verify the script produces the expected output table format.

## Open questions for review

1. Should the calibration script run against real APIs at story-PR review time, with the maintainer manually verifying the diff before commit? Or only on-demand by the developer? Current draft: on-demand only; calibration is a maintenance task, not a CI gate.
2. The Anthropic / Gemini heuristic uses a single character-ratio constant. Should we add model-specific constants (Claude Sonnet 4.6 may differ from Claude Haiku 4.5 by 5–10%)? Current draft: no — single constant is documented as ±10% error, which is well within the low/high envelope.
3. The `LookupCache: false` mode is for callers that want "fresh request" estimates (e.g., reporting "if you sent this from scratch, how much would it cost"). Is this useful in practice, or should we always look up cache? Current draft: keep the parameter; default to true; the compare endpoint may set false to give a "what-if" view.
