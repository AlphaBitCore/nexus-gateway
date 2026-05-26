# E18 — Story 1: Traffic Adapter Detection Interface

## Context

`packages/shared/traffic.Adapter` today exposes `ExtractRequest` / `ExtractResponse` / `ExtractStreamChunk` for content-shape normalization used by hooks. It has no interface for extracting LLM metadata (provider, model, API key identity, token usage). This story adds that interface. All downstream stories (s2 adapters, s7/s8/s9/s10 data-plane wiring) depend on this interface being locked.

## User Story

**As a** data-plane developer adding a new AI provider,
**I want** a single contract for "given raw HTTP, tell me what LLM call this is and how many tokens it used",
**so that** the three data planes share one detection codebase and I don't re-implement parsing per service.

## Tasks

1. **Extend `packages/shared/traffic/adapter.go`:** add two methods to the `Adapter` interface: `DetectRequestMeta(req *http.Request, body []byte) RequestMeta` and `DetectResponseUsage(resp *http.Response, body []byte) UsageMeta`.

2. **Define result types** in `packages/shared/traffic/types.go`:
   ```go
   type RequestMeta struct {
       Provider          string  // "openai"|"anthropic"|...|"unknown"
       Model             string
       Path              string
       ApiKeyClass       string
       ApiKeyFingerprint string  // SHA256(key)[:8] lowercase hex, "" if no key
   }

   type UsageMeta struct {
       PromptTokens     *int
       CompletionTokens *int
       Status           UsageStatus
   }

   type UsageStatus string

   const (
       UsageStatusOK                  UsageStatus = "ok"
       UsageStatusStreamingReported   UsageStatus = "streaming_reported"
       UsageStatusStreamingEstimated  UsageStatus = "streaming_estimated"
       UsageStatusStreamingUnavailable UsageStatus = "streaming_unavailable"
       UsageStatusParseFailed         UsageStatus = "parse_failed"
       UsageStatusNoBody              UsageStatus = "no_body"
       UsageStatusNonLLM              UsageStatus = "non_llm"
   )
   ```

3. **Add fingerprint helpers** in `packages/shared/traffic/fingerprint.go`:
   - `ApiKeyFingerprint(key string) string` → `SHA256(key)[:8]` lowercase hex. Empty input returns empty string.
   - `ClassifyApiKey(headers http.Header, urlQuery url.Values) (class string, raw string)` → returns the classification label per E18 Requirements F6 and the raw key bytes (caller discards `raw` after computing fingerprint). `class` is empty when no recognized key is present.

4. **Prometheus metric constructor** in `packages/shared/traffic/observability.go`: `NewDetectMetrics(namespace string) *DetectMetrics` exposing `LLMDetectDuration` (histogram, labels: `provider`, `side` = request|response) and `LLMDetectStatus` (counter, labels: `provider`, `status`).

5. **Data Access Declaration block** in this file (see below) — documents exactly which fields the detector reads from request/response bodies, for compliance review.

6. **Unit tests:** `fingerprint_test.go` exercises `ApiKeyFingerprint` (empty, short, long, whitespace-padded) and `ClassifyApiKey` across all nine class labels from F6. No adapter implementations yet — those are s2/s3.

## Acceptance Criteria

- `packages/shared/traffic` compiles with the extended interface; builtins registry (`adapters/builtins.go`) type-checks even though implementations are stubs (returning zero-value) until s2.
- `go test -race -count=1 ./packages/shared/traffic/...` passes for fingerprint and classification helpers.
- `ApiKeyFingerprint` verified stable across runs (deterministic SHA-256 over UTF-8 bytes).
- `ClassifyApiKey` never returns raw key bytes in the `class` return; only the fixed label set from F6.

## Data Access Declaration

The detector interface reads the following from intercepted traffic:

**Request:**
- Auth headers by name: `Authorization`, `x-api-key`, `api-key`, `x-goog-api-key`. Value is hashed (SHA-256); only the first 4 bytes of the raw value are inspected for class classification, never stored or logged.
- Query parameter `key` on Gemini public endpoints; same hash-or-classify treatment.
- Request body, JSON top-level fields only: `$.model`, `$.stream`, `$.stream_options`. No nested content, no prompt text.

**Response:**
- Response body fields: `$.model`, `$.usage.prompt_tokens`, `$.usage.completion_tokens`, `$.usage.input_tokens`, `$.usage.output_tokens`, `$.usage.total_tokens`, `$.choices[0].finish_reason`. No assistant content.
- SSE frames: parsed by the streaming accumulator (s4); access limited to `delta.usage`, `usage`, `usageMetadata` sub-trees.

Prompt and completion **text** is read only when the streaming tokenizer estimator runs (Tier 2, `streaming_estimated`). Text is held in memory inside the accumulator, counted into tokens, then discarded. It is never written to `traffic_event`.

## Non-Goals

- Actual provider detection logic — that is s2 and s3.
- Tokenizer estimation — that is s4.
- Wiring detectors into data planes — that is s7 / s8 / s9 / s10.
