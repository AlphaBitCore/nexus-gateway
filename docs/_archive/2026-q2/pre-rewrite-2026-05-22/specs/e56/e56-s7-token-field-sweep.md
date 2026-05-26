# E56-S7 — Token-field stamp sweep + cached_tokens alias

**Epic:** E56 OpenAI Responses-API ingress
**Type:** Token extraction + audit / cache stamping
**Owner:** nexus
**Depends on:** S3.

## User story

> As a billing operator, I want every Responses-API call's `input_tokens`,
> `output_tokens`, `total_tokens`, `cached_tokens`, and `reasoning_tokens`
> populated in `traffic_event` — on both live traffic AND cache replay —
> so cost reports + cache-hit-rate dashboards work without a per-ingress
> branch.

The §5 stamp-site sweep is binding per CLAUDE.md `feedback_token_field_handler_sweep`: missing the 4 cache sites caused all prod cache traffic to NULL on the new column in the E53-S4 incident. This story explicitly pins the equivalent fix for Responses ingress.

## Tasks

### T7.1 — Cached-token alias addition (BINDING)

**File:** `packages/ai-gateway/internal/providers/specutil/usage.go`

Extend `cachedTokenAliases`:

```go
var cachedTokenAliases = []string{
    "usage.prompt_tokens_details.cached_tokens",   // chat-completions
    "usage.input_tokens_details.cached_tokens",    // responses-api (NEW — E56-S7)
    "usage.prompt_cache_hit_tokens",               // Moonshot
    "usage.prompt_cache_tokens",                   // legacy alias
    "usage.cached_tokens",                         // misc OpenAI-compat
}
```

`ExtractCachedTokens` already walks this slice; no other change. Pin a regression unit test in `specutil/usage_test.go`:

```go
{
    name:  "responses_api_cached_tokens",
    body:  `{"usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":80}}}`,
    want:  80,
},
```

### T7.2 — Reasoning-token alias addition

**File:** same.

Confirm `reasoningTokenAliases` already contains `usage.output_tokens_details.reasoning_tokens` (it does today via the closed G6 gap — Responses uses the same shape). Add a regression test pinning the Responses usage block.

### T7.3 — Stamp site verification — 5 sites

For each of the 5 sites mandated by `provider-adapter-architecture.md` §5, verify the Responses-API egress path stamps usage correctly:

| Site | File / function | Verification |
|---|---|---|
| 1 | `handler/proxy.go:handleNonStream` | When ingress=Responses, the canonical Usage struct is built from upstream JSON via specutil → re-serialised to Responses-shape via `EncodeResponsesUsage` (helper in S3) for the response body. Audit Record gets canonical numbers. |
| 2 | `handler/proxy.go:handleStream` | At stream end (S4's `response.completed` event on upstream Responses, or canonical Usage chunk on cross-format), same canonical Usage built; on egress the S4 reverse encoder emits `response.completed` with Responses-shape usage. |
| 3 | `handler/proxy_cache.go:cacheStoreNonStream` | Canonical Usage serialised into the cache row (existing logic). |
| 4 | `handler/proxy_cache.go:cacheStoreStream` | Same. |
| 5 | `handler/proxy_cache.go:cacheRead*` | On cache HIT, canonical Usage deserialised; if ingress=Responses, S3 encoder re-shapes it; if ingress=chat-completions, existing OpenAI identity codec re-shapes. |

No new field added to canonical `Usage` struct. No `traffic_event` migration. Per F-16 / CLAUDE.md "Real implementation only" — the work is in the alias table + verification.

### T7.4 — Tests

**File:** `packages/ai-gateway/internal/providers/specutil/usage_test.go`

- Responses-shape cached_tokens extraction (T7.1).
- Responses-shape reasoning_tokens extraction (T7.2).

**File:** `packages/ai-gateway/internal/handler/proxy_cache_capture_test.go` (extend)

- Cache-store + cache-read round trip with Responses ingress + Responses canonical-bridge target — assert the cached response body's `usage` block contains all of `input_tokens` / `output_tokens` / `total_tokens` / `input_tokens_details.cached_tokens` / `output_tokens_details.reasoning_tokens`.

## Acceptance criteria

- AC-7.1: `cachedTokenAliases` includes `usage.input_tokens_details.cached_tokens`.
- AC-7.2: Regression unit tests pass (T7.1 + T7.2 + cache round-trip).
- AC-7.3: Manual inspection of a real `/test-openai-responses` run on prod after deploy: `SELECT prompt_cache_tokens, reasoning_tokens FROM traffic_event WHERE endpoint_type='responses'` returns non-NULL for cache-hit rows.
- AC-7.4: §5 sweep checklist (6 boxes) in the checklist file `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` §5 is satisfied for Responses ingress — recorded in S7 commit message.

## Verification

```
go test ./packages/ai-gateway/internal/providers/specutil/ -race -count=1
go test ./packages/ai-gateway/internal/handler/ -run TestCacheCapture -race -count=1
```

## Risks

- **R-7.1:** Forgetting the alias = E53-S4 incident class (all prod cache rows NULL). Pinning a unit test that asserts the alias exists makes regression detection automatic. CI failure on alias removal is the safety net.
- **R-7.2:** Future Responses-API usage extensions (e.g. `usage.audio_tokens`, `usage.tool_use_tokens`) require both an alias add AND a canonical-Usage struct field add. Out of scope for E56; would be its own story.
