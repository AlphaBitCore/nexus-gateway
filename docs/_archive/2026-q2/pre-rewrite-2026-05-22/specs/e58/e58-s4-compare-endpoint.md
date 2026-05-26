# E58-S4 — `/v1/estimate` Compare Endpoint (Multi-Target Sugar)

> Story: e58-s4
> Epic: 58
> Status: Draft
> Requirements: `docs/developers/specs/e58/e58-cost-estimation-and-cache-pricing.md` § FR-6
> Architecture: `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` § 5.5 (the optional `/v1/estimate` endpoint is sugar over the dry_run pipeline)
> Blocked by: E58-S3 (`nexus.dry_run` branch is what this endpoint dispatches internally)
> Priority: Should-have. Useful for model-selection UX in customer applications; not a v1 blocker for the cost-estimation surface.

## User Story

As an application developer building a "pick the cheapest model that meets my quality bar" feature, I want to send one prompt to a single Nexus endpoint and get back per-target cost estimates for N candidate models in one round-trip — so I can compare options without writing the orchestration code myself, and so the gateway's cache lookups (response cache, prompt cache prefixes) are run once per (prompt, model) rather than recomputed per separate dry-run round trip from my side.

## Tasks

### T1 — Endpoint registration

- T1.1 Add `POST /v1/estimate` to the AI Gateway router (`packages/ai-gateway/internal/handler/router.go` or wherever route registration lives).
- T1.2 The endpoint uses VK authentication (`vkauth` middleware), same as `/v1/chat/completions`.
- T1.3 The endpoint accepts JSON request bodies; no streaming support (compare results are always returned as one JSON response).
- T1.4 OpenAPI spec at `docs/users/api/openapi/ai-gateway/e58-s4-estimate-compare.yaml`.

### T2 — Request shape

```json
{
  "request": {
    // Any valid ingress request body (chat completions, messages, responses, generateContent)
    // The endpoint inspects the body to detect the format.
    "model": "...",        // optional in compare mode; per-target model overrides
    "messages": [...],
    "max_tokens": 500,
    "reasoning_effort": "high",
    ...
  },
  "compareTargets": [
    {
      "providerId": "uuid-or-slug",
      "modelId":    "uuid-or-code",
      "reasoningEffort": "high"  // optional override; defaults to request.reasoning_effort
    },
    {
      "providerId": "another-provider",
      "modelId":    "claude-sonnet-4-6",
      "reasoningEffort": "low"
    },
    // 1 to N targets (default cap 10)
  ],
  "options": {
    "lookupCache": true,       // default true; estimator's cache-lookup parameter
    "ingressFormat": "openai"  // optional explicit override; auto-detected from request body shape if omitted
  }
}
```

- T2.1 The `request` field carries a canonical-shaped or any-ingress-shaped request body. The endpoint detects the ingress format (same logic the canonicalize step uses) and canonicalizes once.
- T2.2 The `compareTargets` array is 1 to N entries. Default cap is 10 targets per request (configurable via gateway YAML; protects against estimate-storm).
- T2.3 An empty `compareTargets` array is a 400 (use `nexus.dry_run` for single-target estimation).
- T2.4 The `options` block holds estimator knobs and ingress overrides.

### T3 — Request handler

- T3.1 New handler `packages/ai-gateway/internal/handler/admin_estimate.go` (or `handler/estimate.go` — pick by convention; this is a VK-auth endpoint, not admin, so `handler/estimate.go` is correct):
    ```go
    func (h *Handler) handleEstimate(c echo.Context) error {
        var req EstimateCompareRequest
        if err := c.Bind(&req); err != nil { return err }
        if err := req.Validate(); err != nil { return err }

        // 1. Canonicalize the inner request once.
        canonical, err := canonicalbridge.IngressChatToCanonical(req.IngressFormat, req.Request, ...)
        if err != nil { return err }

        // 2. For each target, dispatch a dry-run.
        results := make([]EstimateResult, len(req.CompareTargets))
        var wg sync.WaitGroup
        for i, target := range req.CompareTargets {
            i, target := i, target
            wg.Add(1)
            go func() {
                defer wg.Done()
                results[i] = h.runEstimateForTarget(c.Request().Context(), canonical, target, req.Options)
            }()
        }
        wg.Wait()

        // 3. Build summary.
        summary := buildEstimateSummary(results)

        return c.JSON(200, EstimateCompareResponse{
            Targets: results,
            Summary: summary,
        })
    }
    ```
- T3.2 `runEstimateForTarget` for each target:
    1. Resolve the target — verify the target's `(providerId, modelId)` exists, the VK can access this model (per `VK.allowedModels`).
    2. If the VK cannot access the target, return a per-target error in the result (not a top-level 403).
    3. Construct `estimator.EstimateInput` with the canonical body (target-specific reasoning_effort overrides applied), the resolved target, and the prices.
    4. Call `estimator.Estimate(ctx, in)`.
    5. Wrap as `EstimateResult` with target info + estimator output.
- T3.3 Per-target dispatch is concurrent — N targets run in parallel; the response is sized at max(target latencies). Cap concurrency at `min(10, runtime.NumCPU())` via a semaphore.
- T3.4 The endpoint is read-only — no writes to `traffic_event` (compare is meta-analysis, not a real or even simulated request; the per-target estimates aren't worth a row each).

### T4 — Response shape

```json
{
  "targets": [
    {
      "providerId": "uuid",
      "providerName": "Anthropic Direct",
      "modelId": "uuid",
      "modelCode": "claude-sonnet-4-6",
      "tokens": { ... },        // same shape as the x-nexus-estimate header from S3
      "cost": { "currency": "USD", "low": {...}, "expected": {...}, "high": {...} },
      "cacheBenefit": { ... },
      "reasoning": { ... },
      "assumptions": [ ... ],
      "error": null
    },
    {
      "providerId": "uuid",
      "providerName": "OpenAI",
      "modelId": null,           // VK can't access this model
      "modelCode": "gpt-5",
      "tokens": null,
      "cost": null,
      "cacheBenefit": null,
      "reasoning": null,
      "assumptions": [],
      "error": {
        "code": "vk_model_not_allowed",
        "message": "VK 'vk-prod-abc' allowedModels does not include 'gpt-5'"
      }
    }
  ],
  "summary": {
    "cheapestExpectedTarget": "claude-sonnet-4-6",
    "cheapestExpectedTotalUsd": 0.01144,
    "mostExpensiveExpectedTotalUsd": 0.05670,
    "errorsCount": 1,
    "successCount": 1
  }
}
```

- T4.1 Per-target `error` is non-null only when the per-target estimate could not be computed (VK-allowlist violation, target model doesn't exist, target's pricing isn't configured). The summary fields skip errored targets.
- T4.2 The summary identifies cheapest by `cost.expected.total` across non-errored targets. If all targets errored, `summary.cheapestExpectedTarget` is null and the response has `successCount: 0`.

### T5 — Validation

- T5.1 `compareTargets` size: 1 ≤ N ≤ 10 (or configured max).
- T5.2 Each target's `(providerId, modelId)` must exist in the catalog. Both can be supplied as UUIDs or human-friendly slugs (`providerId: "anthropic-direct"` or `modelId: "claude-sonnet-4-6"`); the handler resolves to UUIDs.
- T5.3 The inner `request` body must be a valid ingress request shape — same validation the corresponding `/v1/*` endpoint would apply. If invalid, the endpoint returns 400 with details.
- T5.4 `reasoningEffort` override values are one of `{minimal, low, medium, high}` or an integer (budget_tokens for Anthropic / Gemini). Invalid values return 400.

### T6 — Auth + scope

- T6.1 VK authentication, same as `/v1/*`. The VK middleware runs first.
- T6.2 Per-target `allowedModels` enforcement is per-target (T3.2.2), not top-level. A VK with limited access still gets a useful response for the accessible subset.
- T6.3 Rate limiting: separate from real-request quota, separate from dry-run rate. New per-VK config `compareEndpointRateLimit` (default 30/min/VK) — compare requests are heavier than dry-runs (they dispatch N internally).
- T6.4 No quota deduction; compare requests cost the gateway only CPU.

### T7 — Metrics

- T7.1 `nexus_aigw_estimate_compare_requests_total{ingress}` — incremented per top-level request.
- T7.2 `nexus_aigw_estimate_compare_targets_total{ingress}` — incremented by N per top-level request (so we can see total targets-per-call distribution).
- T7.3 `nexus_aigw_estimate_compare_duration_seconds{ingress}` — observes end-to-end latency.

### T8 — Documentation

- T8.1 OpenAPI spec `docs/users/api/openapi/ai-gateway/e58-s4-estimate-compare.yaml` with full schema + at least 2 examples (single-format compare, cross-format compare).
- T8.2 The Customer Docs page documenting `/v1/estimate` lives under `docs/users/features/flows/v1-estimate-compare.md`.

## Acceptance Criteria

| ID | Acceptance |
|---|---|
| AC-1 | `POST /v1/estimate` with a valid request body + 3 compare targets returns 200 with per-target estimates and a summary identifying the cheapest. |
| AC-2 | Targets the VK can't access return per-target errors, not a top-level 403. |
| AC-3 | An empty `compareTargets` returns 400. |
| AC-4 | A request with 11 compareTargets (default cap 10) returns 400 with a clear error. |
| AC-5 | The summary's `cheapestExpectedTarget` correctly identifies the lowest `cost.expected.total` across non-errored targets. |
| AC-6 | Per-target dispatch is concurrent — measured end-to-end latency for 5 targets ≤ 1.5× the latency for 1 target (proves it's not serial). |
| AC-7 | The endpoint does NOT write to `traffic_event` for the inner estimates (verified by counting rows before and after). |
| AC-8 | The endpoint respects the per-VK `compareEndpointRateLimit`; exhaustion returns 429 with `Retry-After`. |
| AC-9 | Per-package coverage gates from CLAUDE.md hold for the new handler. |
| AC-10 | The smoke test `tests/scripts/smoke-gateway.py` includes a compare-endpoint arm. |

## Data Model

### Request type

```go
// packages/ai-gateway/internal/handler/estimate.go

type EstimateCompareRequest struct {
    Request         json.RawMessage         `json:"request"`
    CompareTargets  []EstimateCompareTarget `json:"compareTargets"`
    Options         EstimateCompareOptions  `json:"options"`
}

type EstimateCompareTarget struct {
    ProviderID      string  `json:"providerId"`      // UUID or slug
    ModelID         string  `json:"modelId"`         // UUID or code
    ReasoningEffort *string `json:"reasoningEffort"` // optional override
}

type EstimateCompareOptions struct {
    LookupCache    *bool   `json:"lookupCache"`    // default true
    IngressFormat  *string `json:"ingressFormat"`  // auto-detected if nil
}
```

### Response type

```go
type EstimateCompareResponse struct {
    Targets []EstimatePerTarget   `json:"targets"`
    Summary EstimateCompareSummary `json:"summary"`
}

type EstimatePerTarget struct {
    ProviderID    string                 `json:"providerId"`
    ProviderName  string                 `json:"providerName"`
    ModelID       *string                `json:"modelId"`
    ModelCode     string                 `json:"modelCode"`
    Tokens        *estimator.TokenBreakdown    `json:"tokens"`
    Cost          *estimator.CostBreakdown     `json:"cost"`
    CacheBenefit  *estimator.CacheBenefit      `json:"cacheBenefit"`
    Reasoning     *estimator.ReasoningBreakdown `json:"reasoning"`
    Assumptions   []string               `json:"assumptions"`
    Error         *EstimateTargetError   `json:"error"`
}

type EstimateTargetError struct {
    Code    string `json:"code"`    // "vk_model_not_allowed" / "target_not_found" / "pricing_unconfigured"
    Message string `json:"message"`
}

type EstimateCompareSummary struct {
    CheapestExpectedTarget       *string `json:"cheapestExpectedTarget"`
    CheapestExpectedTotalUsd     *float64 `json:"cheapestExpectedTotalUsd"`
    MostExpensiveExpectedTotalUsd *float64 `json:"mostExpensiveExpectedTotalUsd"`
    ErrorsCount                  int     `json:"errorsCount"`
    SuccessCount                 int     `json:"successCount"`
}
```

## Testing strategy

- **Unit (handler)**: table-driven tests for request validation (size cap, missing fields, invalid reasoning effort), VK allowlist enforcement, concurrent dispatch correctness.
- **Unit (summary builder)**: cheapest/most-expensive selection with mixed success/error targets, all-error case, single-success case.
- **Integration**: end-to-end test sending 3-target compare to local gateway with seeded VK + prices; assert response structure + summary.
- **Smoke**: extend `smoke-gateway.py` with a compare arm.

## Rollback plan

- The endpoint is a single new route. Disabling = removing the route registration.
- No schema changes.
- No interaction with existing real-request paths.

If post-deploy a critical issue emerges, `git revert` the registration commit; the route returns 404 immediately, customers fall back to either dry-run per-target or no estimation at all (no functionality lost from pre-S4).

## Open questions for review

1. Should the compare endpoint accept a different shape — for example, a list of `(modelCode)` strings rather than `(providerId, modelId)` pairs — for ease of use? Argument for: customers usually think "gpt-5 vs claude-sonnet vs gemini-2.5-pro", not "providerId-uuid". Argument against: model codes can collide across providers (Bedrock `claude-sonnet-4-6` ≠ Anthropic-direct `claude-sonnet-4-6`); ambiguity. Current draft: support both — `target` can be `{providerId, modelId}` OR `{modelCode}` where modelCode resolves to a unique (providerId, modelId) pair via the catalog (error if ambiguous).
2. Should the response include a "recommended" badge on the cheapest target? Marketing-y but useful for direct UI rendering. Current draft: no — the summary names the cheapest; the UI can render whatever badge it wants.
3. Should compare requests count as one or as N for analytics? Current draft: top-level requests counter is 1; targets-dispatched counter is N. Both visible in metrics; analytics queries decide which to use.
