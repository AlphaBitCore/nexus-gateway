# E33 S2 — Shared Streaming Extractors + ChatGPT-Web Adapter + StreamingPolicy

## Story

As a compliance hook running on any of the three data planes I need to evaluate against a canonical `{prompt, completion}` text view of an SSE response so I do not have to understand provider-specific frame shapes, and operators need to control streaming compliance behavior per host or per provider via a single shared policy struct.

## Scope

- `packages/shared/transport/streaming/extract/extract.go` (new) — `ContentExtractor` interface (`ExtractRequest(body []byte) ExtractedContent`, `ExtractResponseChunk(frame []byte) []ExtractedDelta`, `Reduce([]ExtractedDelta) ExtractedContent`). `ExtractedContent` is `{Prompt string, Completion string, Truncated bool}`.
- `packages/shared/transport/streaming/extract/openai_api.go` (new) — extractor for OpenAI `/v1/chat/completions` SSE deltas (`choices[0].delta.content`).
- `packages/shared/transport/streaming/extract/anthropic_messages.go` (new) — extractor for `/v1/messages` SSE events (`content_block_delta.delta.text`).
- `packages/shared/transport/streaming/extract/google_gemini.go` (new) — extractor for Gemini `/v1beta/.../streamGenerateContent` chunked-NDJSON (`candidates[0].content.parts[].text`).
- `packages/shared/transport/streaming/extract/chatgpt_web.go` (new) — extractor for `chatgpt.com/backend-api/f/conversation` JSON-Patch deltas: filters frames by `event:` and `data:` shape, recognizes `{"type":"input_message", ..., "parts":[…]}` for prompt and `{"p":"/message/content/parts/0", "o":"append", "v":…}` (and the patch-array form `{"o":"patch","v":[…]}`) for completion deltas. Skips infrastructure frames (`message_marker`, `server_ste_metadata`, `conversation_detail_metadata`, `resume_conversation_token`, `[DONE]`, `delta_encoding`).
- `packages/shared/transport/streaming/extract/registry.go` (new) — `ContentExtractorRegistry` keyed by `(format string)` where format matches the existing `Provider.adapter_id` / `InterceptionDomain.adapter_id` value. `RegisterBuiltins(*Registry)` wires the four extractors and a `noop` fallback.
- `packages/shared/transport/streaming/policy/policy.go` (new) — `Policy` struct (`Mode`, `ChunkBytes`, `HookTimeoutMs`, `FailBehavior`, `CaptureRequestBody`, `CaptureResponseBody`, `RawSpillEnabled`), `Mode` enum (`PassThrough`, `BufferFullBlock`, `ChunkedAsync`), `FailBehavior` enum (`FailOpen`, `FailClose`).
- `packages/shared/transport/streaming/policy/resolve.go` (new) — `Resolve(globalDefault Policy, override *Policy) Policy` merges per-resource override (each NULL-able field) with the global default.
- `packages/shared/transport/streaming/buffer.go` and `live.go` and `passthrough.go` — replace today's three modes with the new three: rename `live`→`chunked_async`, rename `buffer`→`buffer_full_block`, keep `passthrough`. The new `chunked_async` adapts `chunk_bytes` upward to honor max-64-chunks-per-stream; the old per-event live mode is retired.
- `packages/shared/transport/streaming/accumulator.go` — split `UsageAccumulator` from a new `ContentAccumulator` interface that drives the `ContentExtractor` per chunk. The two accumulators share the same SSE parser.
- `packages/shared/schemas/configtypes/system_metadata_streaming_compliance.go` (new) — typed reader for `system_metadata['streaming_compliance.config']`.
- `packages/shared/traffic/adapters/chatgpt_web.go` (new) — register a new traffic adapter ID `chatgpt-web` for the `chatgpt.com` interception domain. Differs from `openai-compat` by recognizing the JSON-Patch-shaped request and response.
- Seeds: `tools/db-migrate/seed.ts` (or its TS equivalent) — update the seed `chatgpt` domain row to reference `adapter_id=chatgpt-web` instead of `openai-compat`.
- Tests: per-extractor table-driven tests; `policy.Resolve` test (NULL inheritance, override precedence); `chunked_async` 64-cap adaptive chunk test.

## Tasks

1. Implement `ContentExtractor` interface + four extractors. Each extractor has a frozen golden test fixture under `packages/shared/transport/streaming/extract/testdata/`.
2. Build `ContentExtractorRegistry`; wire `RegisterBuiltins`. Add `noop` fallback that returns empty content (so non-LLM domains still flow through).
3. Implement `Policy` + `Resolve`. Tests cover: (a) all NULL → global, (b) full override → override, (c) partial override → merged.
4. Rewrite `live.go` as `chunked_async`. Drop the per-event live mode. Implement adaptive chunk size: `actual_chunk = max(policy.ChunkBytes, total_emitted/64)`.
5. Rewrite `buffer.go` as `buffer_full_block` with the response-stage hook running once at stream end against the full extracted content.
6. Add `chatgpt-web` traffic adapter to `shared/traffic/adapters/`. Update seed for `interception_domain.chatgpt`.
7. Wire the new SystemMetadata reader via `shared/configstore` so all three services receive the same global default snapshot.

## Acceptance criteria

- `go test ./packages/shared/transport/streaming/...` green with race + count=1.
- Each of the four extractors reproduces a known-good `{prompt, completion}` from a frozen SSE fixture (chatgpt-web fixture is the byte-for-byte sample shared by the user during planning).
- `Policy.Resolve` returns the global default when override is `nil`; returns the override values for non-NULL fields and falls back to global for NULL fields when override is partial.
- `chunked_async.Process` invokes the hook executor exactly `min(64, ceil(extracted_size / chunk_bytes))` times for streams up to 1 MiB extracted; runs an additional final invocation regardless.
- `chatgpt-web` adapter `DetectRequestMeta` returns `Provider: "chatgpt-web"` for a real `/backend-api/f/conversation` body; `DetectResponseUsage` returns the streaming-estimated tier.
