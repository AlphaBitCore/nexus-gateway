# E58-S3 — `nexus.dry_run` Flag for Pre-Flight Cost Estimation

> Story: e58-s3
> Epic: 58
> Status: Draft
> Requirements: `docs/developers/specs/e58/e58-cost-estimation-and-cache-pricing.md` § FR-5
> Architecture: `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` § 5 "The `nexus.dry_run` pipeline branch"
> Blocked by: E58-S0 (canonical body produced by `canonicalbridge.DecodeViaShared` carries the `nexus.dry_run` flag through every ingress), E58-S2 (estimator core)
> Blocks: E58-S4 (the compare endpoint internally dispatches dry-runs per target — same code path)

## User Story

As an application developer integrating Nexus, I want to set a single boolean (`nexus.dry_run: true`) in my existing chat-completion / messages / responses / generateContent request and get back a normal-shape response whose `usage` block is the gateway's estimate of what the request would have cost. I want to use the same SDK and the same code path I already use for real requests — no separate endpoint, no separate authentication, no parallel response shape — because that guarantees the estimate matches reality.

## Tasks

### T1 — Canonical extension wiring

- T1.1 Reserve `nexus.dry_run` (boolean, default false) in the canonical extension namespace. Define a typed accessor in `packages/ai-gateway/internal/providers/canonicalext/`:
    ```go
    func IsDryRun(canonicalBody []byte) bool
    func SetDryRun(canonicalBody []byte, v bool) ([]byte, error)
    ```
- T1.2 Document the flag in `provider-adapter-architecture.md` § 3a Rule 4's example list of `nexus.ext.<provider>.<key>` extension fields. (`nexus.dry_run` is in the `nexus.*` reserved namespace — not provider-scoped — but documented alongside for discoverability.)
- T1.3 Each ingress's request canonicalization (`canonicalbridge.IngressChatToCanonical` and its siblings for messages / responses / generateContent) carries the flag through unchanged. No per-ingress mapping code needed — the canonical extension namespace is already preserved through ingress codecs.

### T2 — Pipeline branch

- T2.1 In `packages/ai-gateway/internal/handler/proxy.go`, after the routing-resolution + cache-lookup step and before the executor dispatch, add the branch:
    ```go
    if canonicalext.IsDryRun(canonicalBody) {
        return h.handleDryRun(ctx, w, r, canonicalBody, resolved, prices)
    }
    // ... existing executor dispatch ...
    ```
- T2.2 `handleDryRun` is a new function in `proxy.go` (or a new sibling file `proxy_dry_run.go` for clarity):
    1. Build an `estimator.EstimateInput` from the canonical body, the resolved target, and the fetched prices.
    2. Call `estimator.Estimate(ctx, in)`.
    3. Map the result back through the ingress's response codec using `EncodeEstimateAsResponse` (T3).
    4. Write the response headers, including `x-nexus-estimate`.
- T2.3 The dry-run branch runs **after** the cache lookup. If the cache lookup hit (`response cache HIT`), the dry-run returns the cached response's actual cost (cost = 0 since cache HIT means upstream was free) and the estimator is not invoked. This guarantees a real cache HIT is reported as such.
- T2.4 If the cache lookup misses, the dry-run branch invokes the estimator and returns the estimate.

### T3 — Per-ingress response encoders

Each ingress's codec gains an `EncodeEstimateAsResponse` method that wraps the `EstimateResult` in that ingress's response shape:

- T3.1 OpenAI Chat Completions (`spec_openai/codec.go`):
    ```json
    {
      "id": "estimate-<uuid>",
      "object": "chat.completion",
      "created": <unix>,
      "model": "<resolved.modelCode>",
      "choices": [],
      "usage": {"prompt_tokens": ..., "completion_tokens": ..., "total_tokens": ...}
    }
    ```
- T3.2 OpenAI Responses (`spec_openai/codec_responses_response.go`):
    ```json
    {
      "id": "estimate-<uuid>",
      "object": "response",
      "created_at": <unix>,
      "model": "...",
      "status": "completed",
      "output": [],
      "usage": {"input_tokens": ..., "output_tokens": ..., "total_tokens": ...}
    }
    ```
- T3.3 Anthropic Messages (`spec_anthropic/codec.go`):
    ```json
    {
      "id": "estimate-<uuid>",
      "type": "message",
      "role": "assistant",
      "model": "...",
      "content": [],
      "stop_reason": "estimate",
      "usage": {
        "input_tokens": <uncached>,
        "cache_read_input_tokens": <cached>,
        "cache_creation_input_tokens": <write>,
        "output_tokens": <expected output incl. reasoning>
      }
    }
    ```
- T3.4 Gemini generateContent (`spec_gemini/codec.go`):
    ```json
    {
      "candidates": [],
      "usageMetadata": {
        "promptTokenCount": ...,
        "candidatesTokenCount": ...,
        "cachedContentTokenCount": ...,
        "thoughtsTokenCount": ...,
        "totalTokenCount": ...
      }
    }
    ```
- T3.5 The estimator's expected anchor populates the response `usage` block. The full low/expected/high breakdown plus assumptions goes in the `x-nexus-estimate` header as compact JSON.
- T3.6 Per-ingress encode tests with table-driven fixtures asserting the response shape matches the ingress contract.

### T4 — Streaming dry-run

- T4.1 When the client sends `stream: true` AND `nexus.dry_run: true`, the gateway emits exactly one SSE chunk containing the usage block plus `[DONE]`:
    ```
    data: {"id":"estimate-...","object":"chat.completion.chunk","choices":[],"usage":{...}}\n\n
    data: [DONE]\n\n
    ```
- T4.2 Per-ingress equivalent for non-OpenAI: Anthropic's `message_start` + `message_stop` shape; Gemini's `usageMetadata`-only chunk.
- T4.3 No token-by-token simulation — the per-chunk content is empty.
- T4.4 The `x-nexus-estimate` header is set on the SSE response before the first chunk, same as non-streaming.

### T5 — Auth + scope

- T5.1 Dry-run requests use the same VK authentication as real requests. The existing middleware chain runs unchanged.
- T5.2 The VK's `allowedModels` constraint applies. If the canonical body's resolved target violates the VK's allowlist, the request returns the same 403 a real request would.
- T5.3 Dry-run requests do NOT count against the VK's quota. The quota middleware checks `canonicalext.IsDryRun(canonicalBody)` and short-circuits its enforcement; it still increments a separate dry-run counter (T6.1) for capacity planning.
- T5.4 Per-VK rate limiting applies a separate cap on dry-run requests (default 60/min/VK, configurable per VK via a new `dryRunRateLimit` field — schema addition tracked as a sub-task here). The cap protects against estimate-flood DoS.

### T6 — Metrics

- T6.1 New Prometheus counter `nexus_aigw_estimate_requests_total{ingress, resolved_model, resolved_provider}` — incremented per dry-run request.
- T6.2 New histogram `nexus_aigw_estimate_duration_seconds{ingress}` — observes the estimator latency.
- T6.3 Existing `nexus_aigw_requests_total` counter is NOT incremented for dry-run requests; dry-runs are tracked in their own counter so real-request rate metrics stay clean.
- T6.4 The `x-nexus-estimate` header's payload size is bounded (estimator result is small; assumption count is small); no separate body-size limit needed.

### T7 — Hooks

- T7.1 Dry-run requests skip **modification** hooks (redaction, prompt-rewriting, header-injection) — there's no upstream call to modify.
- T7.2 Dry-run requests DO run **classification** hooks (PII scan, toxicity detection, content-policy) so the estimation surface itself is auditable. A PII-bearing prompt sent as dry-run should still trigger the PII alert; we don't want estimation to be a back-door to bypass scanning.
- T7.3 In the hooks pipeline, the dry-run flag is exposed as `pipeline.Context.IsDryRun()` so individual hooks can opt out where it makes sense.

### T8 — Traffic event

- T8.1 Dry-run requests produce a `traffic_event` row, but with discriminating fields:
    - `is_dry_run = true` (new column — added as part of T8.2).
    - `cost_usd = 0` (no upstream call).
    - `usage` populated from the estimator's expected anchor.
    - `assumptions` JSONB column carrying the assumption list.
- T8.2 Schema addition: `traffic_event` gains `is_dry_run BOOLEAN NOT NULL DEFAULT false` plus `dry_run_assumptions JSONB NULL`. Migration in this story; column adds are non-disruptive.
- T8.3 Stamp sites: the dry-run branch's traffic-event writer paths set the new columns. Real-request paths leave both at default.
- T8.4 The Traffic Audit Drawer (UI) renders dry-run rows with a "DRY RUN" badge and surfaces the assumptions list. (Small UI task; can ship in same PR.)
- T8.5 Default filters on the Traffic list page exclude dry-run rows so operators don't see them mixed with real traffic. A "show dry-runs" toggle reveals them.

### T9 — Documentation

- T9.1 OpenAPI spec for the four ingresses gains a `nexus.dry_run` request-body field example and a "Dry-run behavior" section in each endpoint's description. See `docs/users/api/openapi/ai-gateway/e58-s3-dry-run.yaml`.
- T9.2 `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` § 5 already documents the pipeline branch (written during the architecture phase).
- T9.3 A new section in `docs/users/features/flows/` documents the customer-facing dry-run flow with examples per ingress.

## Acceptance Criteria

| ID | Acceptance |
|---|---|
| AC-1 | A `POST /v1/chat/completions` with `{..., "nexus": {"dry_run": true}}` returns HTTP 200 with `choices: []` and a populated `usage`. The `x-nexus-estimate` header contains valid JSON with low/expected/high cost + assumptions. |
| AC-2 | The same request body sent to `/v1/messages` (Anthropic ingress), `/v1/responses` (OpenAI Responses), and `:generateContent` (Gemini) returns 200 with the respective ingress's response shape and an empty content array. |
| AC-3 | A `stream: true` dry-run returns one SSE chunk with usage + `[DONE]`. The connection closes cleanly. |
| AC-4 | A dry-run for a model the VK can't reach returns 403 — same as a real request would. |
| AC-5 | A dry-run does NOT increment the VK's quota counter. `nexus_aigw_estimate_requests_total` increments by 1. |
| AC-6 | A `traffic_event` row is produced with `is_dry_run = true`, `cost_usd = 0`, and the assumptions list populated. The Traffic Audit Drawer shows a "DRY RUN" badge. |
| AC-7 | A PII-bearing dry-run prompt still triggers the PII classification hook (no estimator-as-back-door). |
| AC-8 | A dry-run with rate-limit exhausted returns 429 with `Retry-After`. |
| AC-9 | `tests/scripts/smoke-gateway.py` includes a dry-run arm per ingress; all pass. |
| AC-10 | Per-package coverage gates from CLAUDE.md hold for the modified `handler/` files and the new `handler/proxy_dry_run.go`. |

## Data Model

### Canonical extension flag

```go
// packages/ai-gateway/internal/providers/canonicalext/dry_run.go

func IsDryRun(canonicalBody []byte) bool {
    return gjson.GetBytes(canonicalBody, "nexus.dry_run").Bool()
}

func SetDryRun(canonicalBody []byte, v bool) ([]byte, error) {
    return sjson.SetBytes(canonicalBody, "nexus.dry_run", v)
}
```

### `traffic_event` schema additions

```prisma
model traffic_event {
    // ...existing fields...

    is_dry_run            Boolean  @default(false) @map("is_dry_run")
    dry_run_assumptions   Json?    @map("dry_run_assumptions")
}
```

### `x-nexus-estimate` header payload

```json
{
  "resolved": {
    "provider": "anthropic",
    "model": "claude-sonnet-4-6",
    "viaRoutingRule": "smart-default"
  },
  "tokens": {
    "input": {"uncached": 1234, "cached": 800},
    "output": {"low": 100, "expected": 500, "high": 2000},
    "reasoning": {"low": 0, "expected": 300, "high": 1200}
  },
  "cost": {
    "currency": "USD",
    "low":      {"uncachedInput": 0.0037, "cacheRead": 0.00024, "cacheWrite": 0.0, "output": 0.0015, "total": 0.00544},
    "expected": {"uncachedInput": 0.0037, "cacheRead": 0.00024, "cacheWrite": 0.0, "output": 0.0075, "total": 0.01144},
    "high":     {"uncachedInput": 0.0037, "cacheRead": 0.00024, "cacheWrite": 0.0, "output": 0.0300, "total": 0.03394}
  },
  "cacheBenefit": {
    "responseHitProbability": 0.0,
    "promptCacheReadTokens":  800,
    "savingsExpected":        0.00216
  },
  "assumptions": [
    "Anthropic token count is a character-ratio heuristic, ±10% typical error",
    "smart routing dry-run used fallback chain entry; real request may resolve differently",
    "reasoning_effort=high estimated 3000 reasoning tokens for claude-sonnet-4-6"
  ]
}
```

## Testing strategy

- **Unit (per-ingress encode test)**: each `EncodeEstimateAsResponse` test asserts the shape, the `id` prefix, the empty content array, and the populated usage.
- **Unit (canonicalext)**: `IsDryRun` / `SetDryRun` round-trip tests.
- **Unit (handler)**: handler-level test that sends a dry-run request through the proxy with a fake routing engine + estimator, asserts the right branch fires and the response is well-formed.
- **Integration**: live dry-run via `tests/scripts/smoke-gateway.py` dry-run arm; compare-mode arm hits an Anthropic + cache-marker prompt and asserts the `x-nexus-estimate` cache breakdown shows non-zero `promptCacheReadTokens`.
- **Hook integration**: a fake PII hook is registered; a dry-run with PII-shaped prompt triggers it.

## Rollback plan

The pipeline branch is gated entirely on a single canonical-body flag. Rollback options:

- **Per-ingress disable**: remove the `EncodeEstimateAsResponse` for one ingress; dry-runs against that ingress return an error while others continue working.
- **Full disable**: comment out the `handleDryRun` branch in `proxy.go`; all dry-run requests return whatever the legacy path returns (likely 400 from the executor because `choices: []` is not valid input downstream).
- **Schema rollback**: the `is_dry_run` + `dry_run_assumptions` columns are nullable / defaulted; rolling back the migration is a simple ALTER. Historical rows have `is_dry_run = false`, no impact.

## Open questions for review

1. Should dry-runs charge against a separate "estimation quota" so we can limit total estimator load per VK while still preserving the main quota for real requests? Current draft: yes, via the per-VK `dryRunRateLimit` field (T5.4).
2. Should `traffic_event` carry a separate `cost_estimated_usd` column for dry-runs (capturing what the upstream would have cost) so the analytics view can show "potential cost" of estimates? Current draft: yes — the schema addition in T8.2 includes this implicitly via `dry_run_assumptions`, but consider promoting it to its own typed column if reports need it.
3. The `x-nexus-estimate` header may exceed default proxy header-size limits (nginx default 8 KB) if assumptions list grows. Should we cap assumption count at, e.g., 5, or move the payload to a response-body field for non-streaming requests? Current draft: cap to 5 most-relevant assumptions; the rest summarized as "+N more (see logs)".
