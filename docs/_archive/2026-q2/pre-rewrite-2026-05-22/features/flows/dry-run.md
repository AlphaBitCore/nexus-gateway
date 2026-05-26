# `nexus.dry_run` — In-Band Cost Estimation

> **Status**: shipped (E58-S3).
> **Audience**: SDK developers who want a cost estimate without changing their request URL.

Set `nexus.dry_run: true` (strict JSON boolean) at the top level of any `/v1/*` request body. Nexus runs the request through routing + request hooks + cache lookup as if it were real, but **skips the upstream provider call** and returns a normal-shape response whose `usage` block carries the estimator's expected numbers. The full cost breakdown is in the `x-nexus-estimate` response header.

## When to use this vs `/v1/estimate`

| Need | Use |
|---|---|
| Estimate using the SAME SDK code path you already have, just toggle a flag | `nexus.dry_run` |
| Compare cost across N candidate models in one round-trip | [`/v1/estimate`](./v1-estimate-compare.md) |
| Force the response shape match a specific provider's SDK | `nexus.dry_run` (same shape as a real call from that ingress) |
| Bulk evaluation of many prompts × one model | either; `nexus.dry_run` lets you reuse your existing prompt pipeline verbatim |

## Per-ingress request examples

### OpenAI Chat Completions (`POST /v1/chat/completions`)

```json
{
  "model": "gpt-4o",
  "messages": [{"role": "user", "content": "Hello"}],
  "max_tokens": 200,
  "nexus": { "dry_run": true }
}
```

### OpenAI Responses API (`POST /v1/responses`)

```json
{
  "model": "gpt-5.5",
  "input": [{"role": "user", "content": "Hello"}],
  "max_output_tokens": 200,
  "nexus": { "dry_run": true }
}
```

### Anthropic Messages (`POST /v1/messages`)

```json
{
  "model": "claude-sonnet-4-6",
  "messages": [{"role": "user", "content": "Hello"}],
  "max_tokens": 200,
  "nexus": { "dry_run": true }
}
```

### Gemini generateContent (`POST /v1beta/models/{model}:generateContent`)

```json
{
  "contents": [{"role": "user", "parts": [{"text": "Hello"}]}],
  "generationConfig": { "maxOutputTokens": 200 },
  "nexus": { "dry_run": true }
}
```

## Response shape (per ingress)

The response uses each ingress's native success shape with content arrays empty and `usage` populated from the estimator.

### OpenAI Chat (also: Anthropic / Gemini SDK-side responses default to OpenAI chat shape unless natively supported — see "Per-ingress encoders" below)

```json
{
  "id": "dryrun-1716908532...",
  "object": "chat.completion",
  "created": 1716908532,
  "model": "gpt-4o",
  "choices": [
    {
      "index": 0,
      "message": { "role": "assistant", "content": "" },
      "finish_reason": "dry_run"
    }
  ],
  "usage": {
    "prompt_tokens": 6,
    "completion_tokens": 100,
    "total_tokens": 106,
    "prompt_tokens_details": { "cached_tokens": 0 },
    "completion_tokens_details": { "reasoning_tokens": 0 }
  }
}
```

### Anthropic Messages

```json
{
  "id": "dryrun-...",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-6",
  "content": [],
  "stop_reason": "estimate",
  "usage": {
    "input_tokens": 6,
    "cache_read_input_tokens": 0,
    "cache_creation_input_tokens": 0,
    "output_tokens": 100
  }
}
```

### Gemini generateContent

```json
{
  "candidates": [],
  "modelVersion": "gemini-2.5-pro",
  "usageMetadata": {
    "promptTokenCount": 6,
    "candidatesTokenCount": 100,
    "cachedContentTokenCount": 0,
    "thoughtsTokenCount": 0,
    "totalTokenCount": 106
  }
}
```

## `x-nexus-estimate` response header

Compact JSON with the full breakdown:

```json
{
  "resolved": { "provider": "openai", "model": "gpt-4o" },
  "tokens":   { "input": { "uncached": 6, "cached": 0 }, "output": { "low": 50, "expected": 100, "high": 400 }, "reasoning": {"low": 0, "expected": 0, "high": 0} },
  "cost":     { "currency": "USD", "low": {"...": "..."}, "expected": {"total": 0.00033, "...": "..."}, "high": {"...": "..."} },
  "cacheBenefit": { "responseHitProbability": 0, "promptCacheReadTokens": 0, "savingsExpected": 0 },
  "cacheHit": false,
  "assumptions": ["openai token count is a character-ratio heuristic (chars/4.0); ±10–15% typical error"]
}
```

Assumptions are capped at 5 entries to stay under the nginx 8 KB default header limit.

## Cache HIT behaviour (SDD T2.3)

When the request body matches an existing response-cache entry, the dry-run path reports **actual zero cost** (upstream wasn't invoked) and skips the estimator. The response carries `cacheHit: true` in the `x-nexus-estimate` header and the assumption `"cache HIT — upstream not invoked; reported cost is zero"`.

## Streaming dry-run (`stream: true`)

Combine `stream: true` with `nexus.dry_run: true` to receive the dry-run as an SSE stream. The gateway emits one terminal frame per ingress format:

- **OpenAI**: a single `data: {"id":"dryrun-...","object":"chat.completion.chunk","choices":[],"usage":{...}}` followed by `data: [DONE]`.
- **Anthropic**: `event: message_start` + `event: message_delta` + `event: message_stop` sequence.
- **Gemini**: a single `data: { candidates: [], usageMetadata: {...} }` line.

## Hooks behaviour (SDD T7.1/7.2)

- **Modification hooks** (redaction, prompt-rewriting) are no-ops — there's no upstream call to reshape.
- **Classification hooks** (PII scan, toxicity detection) STILL run — a PII-bearing dry-run is not a back-door past scanning.

A request rejected by a classification hook returns the same 4xx the real request would.

## Quota + rate limit

- **VK quota**: dry-runs do NOT count against the cost-budget quota.
- **VK rate limit**: dry-runs use a separate bucket. Default 60 RPM/VK; configurable via the `dryRunRateLimitRpm` field on the VK admin form.
- **Estimate-flood DoS protection**: the separate bucket means a dry-run flood cannot exhaust the real-call rate limit.

## traffic_event row

Every dry-run still produces a `traffic_event` row, discriminated by:

- `is_dry_run = true`
- `estimated_cost_usd = 0` (no upstream call; the schema has no bare `cost_usd` column — cost lives on `estimated_cost_usd` per `tools/db-migrate/schema.prisma:1373`)
- `usage` populated from the estimator's expected anchor
- `dry_run_assumptions` JSONB — the estimator's assumption list

The Traffic-list page filters dry-runs out by default; use the "Show dry-runs" toggle to surface them. The drawer renders a "DRY RUN — estimate only" panel with the assumptions list when an operator opens a dry-run row.

## Failure modes

| HTTP | When |
|---|---|
| 429 `DRY_RUN_RATE_LIMITED` | per-VK `dryRunRateLimitRpm` exhausted |
| 4xx | classification hook rejected (PII, etc.) — same as real call |
| 5xx `dry_run_estimator_failed` | estimator hit a malformed canonical body or context cancelled |
| 5xx `dry_run_model_not_found` | routed model not in catalog (pricing rows missing) |

## Telemetry

Same surfaces as `/v1/estimate`:

- `nexus_aigw_estimate_requests_total{ingress, resolved_model, resolved_provider}` — incremented per dispatch.
- `nexus_aigw_estimate_duration_seconds{ingress}` — estimator latency histogram.

The dry-run path does NOT increment `nexus_aigw_requests_total` (that's for real calls only). Dashboards plotting real-traffic RPS stay clean.

## See also

- [Multi-target `/v1/estimate` compare endpoint](./v1-estimate-compare.md)
- `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` — 4-component cost function + pricing model
- `docs/developers/specs/e58/e58-s3-dry-run-flag.md` — SDD with full acceptance criteria
- `docs/users/api/openapi/ai-gateway/e58-s3-dry-run.yaml` — OpenAPI schema
