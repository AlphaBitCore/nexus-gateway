# E58-S0 — T0 Capability Gap Audit

> Companion to `docs/developers/specs/e58/e58-s0-unified-protocol-parser.md`. Read those tasks; this file is the audit deliverable feeding T2 (fill gaps) and T3 (bridge implementation).
> Date: 2026-05-16
> Source files audited:
> - `packages/shared/transport/normalize/{openai_chat,anthropic_messages,gemini_generate,types}.go`
> - `packages/ai-gateway/internal/providers/types.go`
> - `packages/ai-gateway/internal/providers/specutil/usage.go`
> - `packages/ai-gateway/internal/providers/spec_openai/{codec,codec_responses_response,stream,stream_responses}.go`
> - `packages/ai-gateway/internal/providers/spec_anthropic/{codec,stream}.go`
> - `packages/ai-gateway/internal/providers/spec_gemini/{codec,stream}.go`

## 1. Canonical `Usage` struct comparison

| Field | `shared/normalize.Usage` | `ai-gateway/providers.Usage` | Resolution |
|---|---|---|---|
| input tokens | `PromptTokens *int` | `PromptTokens *int` | identical |
| output tokens | `CompletionTokens *int` | `CompletionTokens *int` | identical |
| total tokens | `TotalTokens *int` | `TotalTokens *int` | identical |
| cache-read tokens | `CacheReadTokens *int` | **`CachedTokens *int`** | **NAMING CONFLICT** |
| cache-write tokens | `CacheCreationTokens *int` | `CacheCreationTokens *int` | identical |
| reasoning tokens | `ReasoningTokens *int` | `ReasoningTokens *int` | identical |

**Decision**: keep `shared/normalize.Usage.CacheReadTokens` (it is more semantically precise: read-side vs write-side cache tokens). `providers.Usage.CachedTokens` is renamed to `CacheReadTokens` in S0-T1; this is a mechanical rename across ai-gateway.

After S0-T1, the canonical struct lives ONLY in `shared/normalize/types.go`. `providers.Usage` becomes `type Usage = normalize.Usage`. `shared/traffic.UsageMeasurement` becomes `type UsageMeasurement = normalize.Usage` (it is already a parallel definition in `shared/traffic/detect.go`).

## 2. Per-format capability gap

### 2.1 OpenAI Chat Completions

| Field | shared/normalize/openai_chat.go | ai-gateway/spec_openai/codec.go + specutil | Gap |
|---|---|---|---|
| `usage.prompt_tokens` → PromptTokens | ✓ (line 298) | ✓ via `specutil.ExtractOpenAIUsage` | none |
| `usage.completion_tokens` → CompletionTokens | ✓ (line 299) | ✓ | none |
| `usage.total_tokens` → TotalTokens | ✓ (line 300) | ✓ | none |
| `usage.prompt_tokens_details.cached_tokens` → CacheReadTokens | ✓ (line 302) | ✓ | none |
| `usage.completion_tokens_details.reasoning_tokens` → ReasoningTokens | ✓ (line 311) | ✓ | none |
| `usage.prompt_tokens_details.cache_creation_tokens` (Nexus extension) → CacheCreationTokens | ✓ (line 308) | ✓ | none |
| `usage.cached_tokens` (Kimi K2 flat) → CacheReadTokens | **✗ MISSING** | ✓ via specutil alias chain | **GAP-A1** |
| `usage.prompt_cache_hit_tokens` (DeepSeek) → CacheReadTokens | **✗ MISSING** | ✓ | **GAP-A2** |
| `usage.prompt_cache_tokens` (Moonshot explicit-cache) → CacheReadTokens | **✗ MISSING** | ✓ | **GAP-A3** |
| `usage.input_tokens` → PromptTokens (Responses API path used by some legacy clients via /v1/chat/completions) | **✗ MISSING** | ✓ | **GAP-A4** |
| `usage.output_tokens` → CompletionTokens | **✗ MISSING** | ✓ | **GAP-A5** |

**Impact**: when an upstream that uses Kimi-flat or DeepSeek/Moonshot variant aliases is intercepted by the compliance proxy or agent (both go through shared/normalize Tier 1), cache tokens are LOST. The ai-gateway path via specutil is correct. This is exactly the silent drift the unification is meant to fix.

### 2.2 OpenAI Responses API

| Field | shared/normalize | ai-gateway/spec_openai/codec_responses_response.go | Gap |
|---|---|---|---|
| `usage.input_tokens` → PromptTokens | **(handled by openai_chat.go if registered)** | ✓ | dependency on which normalizer the registry routes to |
| `usage.output_tokens` → CompletionTokens | ✗ if openai_chat handles, otherwise has gap | ✓ | **GAP-B1** |
| `usage.input_tokens_details.cached_tokens` → CacheReadTokens | **✗ MISSING** | ✓ via specutil | **GAP-B2** |
| `usage.output_tokens_details.reasoning_tokens` → ReasoningTokens | **✗ MISSING** | ✓ | **GAP-B3** |

**Decision**: Add a dedicated `shared/normalize/openai_responses.go` Tier-1 normalizer that handles the `/v1/responses` shape. OR extend `openai_chat.go` to accept both shapes. **Recommend dedicated normalizer** — Responses shape has structurally different `output[]` (vs chat's `choices[].message`); shared logic only works for the Usage block. Cleaner to have one normalizer per surface.

### 2.3 Anthropic Messages

| Field | shared/normalize/anthropic_messages.go | ai-gateway/spec_anthropic/codec.go | Gap |
|---|---|---|---|
| `usage.input_tokens` → PromptTokens (raw Anthropic convention = uncached only) | ✓ direct copy (line 303) | ✓ same | **GAP-C1 (semantic)** |
| `usage.output_tokens` → CompletionTokens | ✓ (line 304) | ✓ | none |
| `usage.cache_read_input_tokens` → CacheReadTokens | ✓ (line 309-312) | ✓ | none |
| `usage.cache_creation_input_tokens` → CacheCreationTokens | ✓ (line 305-308) | ✓ + `canonicalext.Set` mirror (codec.go:714) | **GAP-C2** (canonicalext stamping not in shared/normalize) |
| TotalTokens computation | input + output (line 314) | (does not compute) | **GAP-C3** — should be uncached + cache_read + cache_write + output |
| `usage.cache_creation` Anthropic 2026 sub-object (5m/1h windows) | ✗ MISSING | ✗ MISSING | future gap, out of S0 scope |

**GAP-C1 (CRITICAL)**: Anthropic's `input_tokens` is the **uncached** count. After S0 the canonical convention is OpenAI-style "PromptTokens = total input (uncached + cached_read + cached_write)". The shared/normalize Anthropic normalizer must compute `PromptTokens = input_tokens + cache_read_input_tokens + cache_creation_input_tokens` to honor the canonical contract. This is a NORMALIZATION rule, not a missing field.

**GAP-C2**: The `canonicalext.Set(canonical, "anthropic", "cache_creation_input_tokens", ...)` round-trip mirror currently happens in `spec_anthropic/codec.go:714`. After bridging, this stamping moves into `canonicalbridge.DecodeViaShared`'s Anthropic-specific projection (provider extension stamping per Rule 4).

**GAP-C3**: TotalTokens should be `uncached + cache_read + cache_write + output`. Currently `input + output` (which is `uncached + output`). Underestimates total when cache is in play.

### 2.4 Gemini generateContent

| Field | shared/normalize/gemini_generate.go | ai-gateway/spec_gemini/codec.go | Gap |
|---|---|---|---|
| `usageMetadata.promptTokenCount` → PromptTokens | ✓ (line 273) | ✓ | none |
| `usageMetadata.candidatesTokenCount` → CompletionTokens | ✓ (line 274) | ✓ | none |
| `usageMetadata.totalTokenCount` → TotalTokens | ✓ (line 275) | ✓ | none |
| `usageMetadata.cachedContentTokenCount` → CacheReadTokens | ✓ (line 276) | ✓ | none |
| `usageMetadata.thoughtsTokenCount` → ReasoningTokens (Gemini 2.x) | ✓ (line 280) | ✓ | none |

**No gaps**. Gemini is the cleanest of the three.

## 3. Bedrock + Vertex wrapper envelopes

**Bedrock (Anthropic-on-AWS)**: The Bedrock response envelope wraps Anthropic shape with AWS metadata. `spec_bedrock` strips the envelope before delegating to spec_anthropic's parser. After S0, `canonicalbridge.DecodeViaShared` for `providers.FormatBedrock` (or a Bedrock-specific path) unwraps first, then calls shared/normalize's Anthropic normalizer.

**Vertex (Gemini-on-GCP)**: Vertex's `:generateContent` response shape is identical to Gemini API's; same parser handles both. `spec_vertex` for Anthropic-on-Vertex unwraps similarly.

No new normalizers needed; these are wrappers handled at the bridge layer.

## 4. Streaming gap

All three shared/normalize normalizers already implement streaming (SSE) parsers (`normalizeStreamResponse`). The ai-gateway spec_*/stream.go files duplicate this logic. After S0, ai-gateway's streaming sessions delegate the per-chunk parsing to shared/normalize's SSE walker; only the streaming-session wrapping (chunk-emission loop, transcoding for ingress format) stays in ai-gateway.

Streaming gap-fill items mirror the non-stream gaps above (Kimi flat, DeepSeek, Moonshot for OpenAI Chat streams; PromptTokens normalization for Anthropic stream's `message_delta` usage).

## 5. Summary: T2 work items derived from T0

| ID | Gap | File to edit | Action |
|---|---|---|---|
| GAP-A1 | Kimi flat `cached_tokens` | `shared/normalize/openai_chat.go` non-stream + stream | Add fallback alias after `prompt_tokens_details.cached_tokens` |
| GAP-A2 | DeepSeek `prompt_cache_hit_tokens` | same | Add to alias chain |
| GAP-A3 | Moonshot `prompt_cache_tokens` | same | Add to alias chain |
| GAP-A4 | `input_tokens` for chat (legacy / cross-shape) | same | Add as fallback to `prompt_tokens` |
| GAP-A5 | `output_tokens` for chat | same | Add as fallback to `completion_tokens` |
| GAP-B1-B3 | OpenAI Responses API | `shared/normalize/openai_responses.go` (NEW) | Create dedicated Tier-1 normalizer |
| GAP-C1 | Anthropic PromptTokens normalization | `shared/normalize/anthropic_messages.go` | Compute `PromptTokens = input + cache_read + cache_write` |
| GAP-C2 | Anthropic extension stamping | `canonicalbridge` | Move from spec_anthropic to bridge's projection |
| GAP-C3 | Anthropic TotalTokens fix | `shared/normalize/anthropic_messages.go` | Recompute to include cache tokens |

## 6. T3 implementation plan (post-gap-fill)

The bridge package `packages/ai-gateway/internal/execution/canonicalbridge/decoder.go`:

```go
package canonicalbridge

import (
    "context"
    "github.com/.../packages/shared/transport/normalize"
    "github.com/.../packages/ai-gateway/internal/providers"
)

// DecodeViaShared is the single decode entry for ai-gateway codecs.
//
// Flow:
//   1. Call shared/normalize.Registry.Normalize(raw, meta) to get NormalizedPayload.
//   2. Project NormalizedPayload → canonical wire-shape JSON (OpenAI chat-completions form).
//   3. Project NormalizedPayload.Usage → providers.Usage (currently identical struct after S0-T1; trivial copy).
//   4. Stamp provider-specific extensions onto canonical via canonicalext (Anthropic cache_creation, etc.).
//
// Per-format projection helpers:
//   - projectOpenAIChat(NormalizedPayload) []byte
//   - projectAnthropic(NormalizedPayload) ([]byte, anthropicExtFields)
//   - projectGemini(NormalizedPayload) ([]byte, geminiExtFields)
//   - projectOpenAIResponses(NormalizedPayload) []byte
func DecodeViaShared(
    ctx context.Context,
    raw []byte,
    wireFormat providers.Format,
    endpoint providers.Endpoint,
) (canonicalBody []byte, usage providers.Usage, err error) {
    // Meta construction per wire format.
    meta := normalize.Meta{
        Direction:   normalize.DirectionResponse,
        AdapterType: adapterTypeFor(wireFormat),
        // ... other meta fields ...
    }

    // Look up the registry. The registry is per-binary; ai-gateway's main
    // function registers Tier-1 normalizers at startup. The bridge
    // receives a reference (or uses a package-level cached reference).
    np, err := normalize.Registry.Normalize(ctx, raw, meta)
    if err != nil {
        return nil, providers.Usage{}, fmt.Errorf("canonicalbridge: shared/normalize parse: %w", err)
    }

    // Per-format projection.
    canonicalBody, usage = projectForFormat(np, wireFormat, endpoint)
    return canonicalBody, usage, nil
}
```

The bridge's projection step is straightforward because `NormalizedPayload` already carries everything the canonical wire shape needs: messages (with role + content blocks → OpenAI choices[].message), Usage (1:1 copy of fields after S0-T1).

## 7. Risks identified

| Risk | Mitigation |
|---|---|
| Renaming `providers.Usage.CachedTokens` → `CacheReadTokens` touches every spec_* codec and many handler/test files | Use `replace_all` via Edit tool; verify with `go build ./...` |
| Anthropic's `PromptTokens` normalization rule change breaks downstream cost math that expected the un-normalized value | The OLD `metrics.CalculateCost(usage, inputPrice, outputPrice)` used `usage.PromptTokens * inputPrice`. This was already wrong for Anthropic + cache (the bug that motivated S1). The NEW post-S1 `CalculateCost` uses `UncachedInput = PromptTokens - CachedTokens - CacheCreationTokens` — which yields the SAME billable input under the new convention. So changing the convention IS the fix. |
| shared/normalize TotalTokens fix may collide with downstream consumers that read `usage.totalTokens` | Audit traffic_event analytics queries for any consumer comparing TotalTokens vs Prompt+Completion. Document in S1 release notes. |
| Streaming parser changes risk SSE replay desync | Comprehensive streaming tests in shared/normalize already exist; we extend, don't replace. |

## 8. Decision log

- **D1**: Canonical `Usage` struct lives in `shared/normalize/types.go`. User-confirmed 2026-05-16.
- **D2**: `providers.Usage.CachedTokens` field renamed to `CacheReadTokens` for clarity (read-side vs write-side).
- **D3**: OpenAI Responses API gets its own dedicated normalizer `shared/normalize/openai_responses.go` rather than extending openai_chat.go.
- **D4**: Anthropic `PromptTokens` is normalized to the OpenAI convention (uncached + cached_read + cached_write) inside `shared/normalize/anthropic_messages.go`. The non-normalized count remains available implicitly as `PromptTokens - CacheReadTokens - CacheCreationTokens`.
- **D5**: Bedrock + Vertex wrapper envelopes are unwrapped at the bridge layer; no new normalizers needed.
