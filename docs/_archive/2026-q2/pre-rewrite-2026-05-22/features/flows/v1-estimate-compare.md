# `POST /v1/estimate` — Multi-Target Cost Compare

> **Status**: shipped (E58-S4).
> **Audience**: SDK developers building "pick the cheapest model that meets my quality bar" UX inside their applications.

The `/v1/estimate` endpoint accepts one request body plus a list of up to 10 candidate `(provider, model)` targets and returns per-target token + cost estimates in a single round-trip. No upstream provider is called; the estimator runs locally against the Nexus catalog's pricing data.

## Use cases

- **Pre-flight cost preview** before a long-running prompt: surface the estimated bill to the user before sending.
- **Model picker**: rank Claude / GPT / Gemini / Mistral by expected cost on the same prompt; pick the cheapest that meets the user's stated quality bar.
- **Routing simulation**: dry-run smart-routing decisions in a test harness without burning provider quota.

## Authentication

VK auth — identical surface to `/v1/chat/completions`. The presented VK's `allowedModels` constraint is enforced **per target**: a VK that can reach `gpt-4o` and `claude-sonnet-4-6` but not `gpt-5.5` will receive successful estimates for the first two and a per-target `vk_model_not_allowed` error for `gpt-5.5`, rather than a top-level 403.

## Rate limit

Separate from real-request quota AND from the dry-run flag's rate limit. Default 30 RPM/VK; configurable per VK via the `compareEndpointRateLimitRpm` field on the VK admin form.

## Request shape

```json
{
  "request": {
    "model": "gpt-4o",
    "messages": [
      { "role": "user", "content": "Summarize the attached PDF in 3 bullets." }
    ],
    "max_tokens": 500,
    "reasoning_effort": "medium"
  },
  "compareTargets": [
    { "providerId": "openai-direct",     "modelId": "gpt-4o" },
    { "providerId": "openai-direct",     "modelId": "gpt-5.5",                "reasoningEffort": "high" },
    { "providerId": "anthropic-direct",  "modelId": "claude-sonnet-4-6" }
  ],
  "options": {
    "ingressFormat": "openai"
  }
}
```

### Field reference

| Field | Required | Description |
|---|---|---|
| `request` | yes | The same request body you would POST to `/v1/chat/completions` (or another ingress when `options.ingressFormat` overrides). Tokenized locally to estimate input tokens. |
| `compareTargets` | yes | Array of 1..10 `(providerId, modelId)` targets. Each accepts UUID or human-readable slug. |
| `compareTargets[].reasoningEffort` | no | Per-target override of `reasoning_effort`. Either `minimal` / `low` / `medium` / `high` OR a positive integer (Anthropic / Gemini `budget_tokens`). |
| `options.ingressFormat` | no | Tags the metrics + helps the estimator choose the right tokenizer. Defaults to `openai`. |

## Response shape

```json
{
  "targets": [
    {
      "providerId": "00000000-...-openai-direct",
      "providerName": "openai",
      "modelId": "00000000-...-gpt-4o",
      "modelCode": "gpt-4o",
      "tokens": {
        "uncachedInput": 312,
        "inputCached": 0,
        "output": { "low": 100, "expected": 350, "high": 1500 },
        "reasoning": { "low": 0, "expected": 0, "high": 0 }
      },
      "cost": {
        "currency": "USD",
        "low":      { "uncachedInput": 0.00078, "cacheRead": 0, "cacheWrite": 0, "output": 0.001, "total": 0.00178 },
        "expected": { "uncachedInput": 0.00078, "cacheRead": 0, "cacheWrite": 0, "output": 0.0035, "total": 0.00428 },
        "high":     { "uncachedInput": 0.00078, "cacheRead": 0, "cacheWrite": 0, "output": 0.015, "total": 0.01578 }
      },
      "reasoning": { "effortRequested": "medium", "supportedByModel": false, "estimatedTokens": 0 },
      "assumptions": [
        "openai token count is a character-ratio heuristic (chars/4.0); ±10–15% typical error",
        "smart routing dry-run used fallback chain entry"
      ]
    },
    {
      "providerId": "00000000-...-openai-direct",
      "providerName": "openai",
      "modelId": null,
      "modelCode": "gpt-5.5",
      "error": {
        "code": "vk_model_not_allowed",
        "message": "VK \"prod-vk\" allowedModels does not include \"gpt-5.5\" (providerId=...)"
      }
    },
    {
      "providerId": "...",
      "providerName": "anthropic",
      "modelCode": "claude-sonnet-4-6",
      "tokens": { "uncachedInput": 312, "inputCached": 0, "output": { "low": 100, "expected": 380, "high": 1500 }, "reasoning": { "low": 0, "expected": 0, "high": 0 } },
      "cost": { "currency": "USD", "low": { "...": "..." }, "expected": { "uncachedInput": 0.000936, "output": 0.0057, "total": 0.006636, "...": "..." }, "high": { "...": "..." } },
      "assumptions": [ "anthropic token count is a character-ratio heuristic (chars/3.5); ±10–15% typical error" ]
    }
  ],
  "summary": {
    "cheapestExpectedTarget": "gpt-4o",
    "cheapestExpectedTotalUsd": 0.00428,
    "mostExpensiveExpectedTotalUsd": 0.006636,
    "errorsCount": 1,
    "successCount": 2
  }
}
```

## Failure modes

| HTTP | `error.code` | Trigger |
|---|---|---|
| 400 | `estimate_no_targets` | `compareTargets` empty |
| 400 | `estimate_too_many_targets` | `compareTargets` > 10 entries |
| 400 | `estimate_invalid_reasoning_effort` | `compareTargets[i].reasoningEffort` not in {minimal,low,medium,high} and not a positive integer |
| 400 | `estimate_invalid_json` | request body not parseable |
| 400 | `estimate_no_request` | `request` field missing or empty |
| 401 | `estimate_unauthorized` | VK auth failed |
| 405 | `estimate_method_not_allowed` | non-POST |
| 429 | `estimate_compare_rate_limited` | per-VK compare-endpoint cap exhausted |
| per-target `error.code = vk_model_not_allowed` | — | the VK's allowedModels disallows this target |
| per-target `error.code = estimate_target_not_found` | — | the (provider, model) tuple is not in the catalog |

## Curl example

```bash
curl -X POST https://nexus.example.com/v1/estimate \
  -H "Authorization: Bearer $NEXUS_VK" \
  -H "Content-Type: application/json" \
  -d '{
    "request": { "model": "gpt-4o", "messages": [{"role":"user","content":"hi"}], "max_tokens": 50 },
    "compareTargets": [
      { "providerId": "openai-direct",    "modelId": "gpt-4o" },
      { "providerId": "anthropic-direct", "modelId": "claude-sonnet-4-6" }
    ]
  }'
```

## Telemetry

Two Prometheus surfaces:

- `nexus_aigw_estimate_compare_requests_total{ingress}` — 1 per top-level request.
- `nexus_aigw_estimate_compare_targets_total{ingress}` — N per top-level request (fan-out).
- `nexus_aigw_estimate_compare_duration_seconds{ingress}` — end-to-end latency histogram.

Plus the per-target counters shared with the dry-run pipeline:
- `nexus_aigw_estimate_requests_total{ingress, resolved_model, resolved_provider}`
- `nexus_aigw_estimate_duration_seconds{ingress}` — per-target estimator latency.

## Architectural notes

- **No traffic_event row** is written for compare requests — they're meta-analysis, not real or simulated traffic. The per-target dispatches also do NOT write traffic_event rows (unlike the in-band `nexus.dry_run` flag which does).
- **Parallel dispatch** — N targets run concurrently bounded by `estimateConcurrency = 8`.
- **No quota deduction** — compare requests cost the gateway only CPU.

## See also

- [`nexus.dry_run` flag flow](./dry-run.md) — single-target version that goes through the regular `/v1/*` endpoints
- `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` — pricing model + 4-component cost function
- `docs/developers/specs/e58/e58-s4-compare-endpoint.md` — SDD with full acceptance criteria
- `docs/users/api/openapi/ai-gateway/e58-s4-estimate-compare.yaml` — OpenAPI schema
