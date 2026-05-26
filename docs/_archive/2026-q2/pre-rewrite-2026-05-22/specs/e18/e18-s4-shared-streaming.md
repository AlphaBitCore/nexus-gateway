# E18 — Story 4: Promote Streaming Engine to `shared/streaming`

## Context

Compliance Proxy has a mature SSE engine at `packages/compliance-proxy/internal/streaming/` (passthrough / live / buffer modes, per s-investigation). The Agent needs SSE handling for s10. Duplicating the engine is wrong; this story promotes it to `packages/shared/transport/streaming/` and extends it with an LLM usage accumulator.

## User Story

**As a** data-plane developer,
**I want** one SSE handling library used by both Compliance Proxy and Agent,
**so that** streaming-mode bug fixes and usage-extraction logic live in exactly one place.

## Tasks

### 4.1 Promote package

- Move `packages/compliance-proxy/internal/streaming/*.go` (and `*_test.go`) to `packages/shared/transport/streaming/`.
- Update imports in `packages/compliance-proxy/internal/proxy/sse.go` and elsewhere.
- Per CLAUDE.md no-backcompat policy, the old path is deleted in the same PR.

### 4.2 Add `UsageAccumulator`

New file `packages/shared/transport/streaming/usage.go`:

```go
type UsageAccumulator interface {
    // Feed the accumulator a decoded SSE frame (post-parse).
    // Called once per frame by the Live/Buffer pipeline.
    Feed(frame Event)

    // Finalize at end of stream. Returns the extracted usage and the accuracy tier.
    Finalize(ctx context.Context) traffic.UsageMeta
}

type UsageAccumulatorFactory func(providerID string, model string) UsageAccumulator
```

Built-in accumulators:
- **`openaiAccumulator`** — watches for `data: {..., "usage": {...}}` (sent before `data: [DONE]` when `stream_options.include_usage` is set). Also captures `choices[*].delta.content` into a running buffer for tokenizer fallback.
- **`anthropicAccumulator`** — watches for `message_delta` event frames carrying `usage` deltas; accumulates them cumulatively. Captures `content_block_delta.delta.text` for tokenizer fallback.
- **`geminiAccumulator`** — watches for `usageMetadata` in any chunk (Gemini emits it mid-stream).
- **`bedrockAccumulator`** — decodes Smithy envelope frames, dispatches to provider-specific inner accumulator (reuses `anthropic`, `openai`, etc. based on model-id prefix).

### 4.3 Tokenizer fallback (Tier 2)

New file `packages/shared/transport/streaming/tokenizer.go`:

- Interface `Tokenizer { Count(text string) (int, error) }`.
- Built-in implementations: `tiktokenTokenizer` (OpenAI, Azure, DeepSeek, GLM — all GPT-shape tokenization), `anthropicTokenizer` (Anthropic-Claude official tokenizer Go port or wrapped call), `geminiTokenizer` (SentencePiece; use public google.golang.org/genai module if available, else fall back to word-count approximation with a declared ±15% accuracy).
- Worker pool (`sync.Pool` + bounded goroutines, N = `runtime.NumCPU()`) with per-request deadline (200ms). On deadline expiry, status = `streaming_estimated_timeout` (sub-variant of `streaming_unavailable`).

### 4.4 Integration with pipeline

Extend `LivePipeline.Process` and `BufferPipeline.Process` to accept an optional `UsageAccumulator`. When the content-type is recognized as SSE and the adapter declared a streaming AI response, the pipeline feeds frames to both the compliance accumulator (existing) and the usage accumulator (new). Final `UsageMeta` is attached to the pipeline result and consumed by the data plane.

### 4.5 Tests

- Golden SSE fixtures per accumulator: `testdata/openai_with_usage.sse`, `testdata/openai_without_usage.sse`, `testdata/anthropic_message_delta.sse`, `testdata/gemini_usageMetadata.sse`, `testdata/bedrock_smithy.bin`.
- Tokenizer tier-2 tests: feed a known prompt → expect token count within ±2% of the reference.
- Deadline test: spin a tokenizer with artificial 500ms delay, assert `streaming_unavailable` within the 200ms deadline.

## Acceptance Criteria

- `packages/shared/transport/streaming/` exists with the accumulator and tokenizer APIs; `packages/compliance-proxy/internal/streaming/` is deleted (not left as a shim).
- All existing compliance-proxy SSE tests pass after import relocation.
- Tier-1 (`streaming_reported`) extraction succeeds on golden SSE fixtures that contain usage.
- Tier-2 (`streaming_estimated`) extraction runs within 200ms deadline and produces token counts within documented accuracy bands.
- Tier-3 (`streaming_unavailable`) triggers when body is truncated past the accumulator's 8MB cap or when tokenizer deadline expires.

## Non-Goals

- Real-time incremental token reporting (accumulator finalizes at end of stream).
- Custom tokenizers for niche providers (MiniMax, Zhipu beyond GPT-compat) — they land on `streaming_unavailable` for now.
- Replacing the existing passthrough / live / buffer mode selection logic — only the usage sub-feature is new.
