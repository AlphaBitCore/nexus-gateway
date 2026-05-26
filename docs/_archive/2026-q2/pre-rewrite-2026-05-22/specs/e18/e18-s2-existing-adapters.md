# E18 — Story 2: Detection for Existing Providers

## Context

`packages/shared/traffic/adapters/` already contains OpenAI, Anthropic, Gemini, Azure, MiniMax, and Generic adapters (each with `ExtractRequest` / `ExtractResponse` / `ExtractStreamChunk` implementations, per s1 investigation). This story adds `DetectRequestMeta` and `DetectResponseUsage` to each. Depends on s1.

## User Story

**As a** platform admin,
**I want** traffic rows to record provider, model, api-key identity, and token usage for the six mainstream AI providers already supported,
**so that** cost attribution and API-key governance work for the common case before expansion to enterprise-specific providers (s3).

## Tasks

### 2.1 OpenAI — `adapters/openai/openai.go`

- `DetectRequestMeta`: `Provider = "openai"`. Read `Authorization: Bearer <key>` → `ClassifyApiKey` yields `sk-proj-` / `sk-` / `nvk_` (for VK traffic passing through Gateway). Extract `$.model`, `$.stream` from body. Path = `req.URL.Path`.
- `DetectResponseUsage` (non-streaming): parse `$.usage.prompt_tokens`, `$.usage.completion_tokens`. Status = `ok` on success, `parse_failed` if JSON broken, `no_body` if body empty.
- `DetectResponseUsage` (streaming): delegate to `shared/streaming.Accumulator` (s4), set Status = `streaming_reported` or `streaming_estimated` based on accumulator output.

### 2.2 Anthropic — `adapters/anthropic/anthropic.go`

- `DetectRequestMeta`: `Provider = "anthropic"`. Read `x-api-key` header → class `sk-ant-`. Extract `$.model`, `$.stream` from body.
- `DetectResponseUsage`: non-streaming parses `$.usage.input_tokens` (prompt), `$.usage.output_tokens` (completion). Streaming delegates to accumulator; usage arrives in `message_delta.usage` frames.

### 2.3 Gemini (Google AI) — `adapters/gemini/gemini.go`

- `DetectRequestMeta`: `Provider = "gemini"`. Read `x-goog-api-key` header or `?key=` query param → class `AIza`. Model is derived from URL path (e.g. `/v1beta/models/gemini-1.5-pro:generateContent` → `gemini-1.5-pro`). `stream = true` if path ends in `:streamGenerateContent`.
- `DetectResponseUsage`: non-streaming reads `$.usageMetadata.promptTokenCount`, `$.usageMetadata.candidatesTokenCount`. Streaming delegates to accumulator.

### 2.4 Azure OpenAI — `adapters/azure/azure.go`

- `DetectRequestMeta`: `Provider = "azure"`. Read `api-key` header → class `azure-api-key`. Model is derived from URL path `/openai/deployments/{deployment}/...` — the deployment name is the model identifier from Azure's perspective (we record it verbatim). Body parsing same as OpenAI for `stream`.
- `DetectResponseUsage`: identical shape to OpenAI (Azure returns OpenAI-compatible responses). Streaming delegates to accumulator.

### 2.5 MiniMax — `adapters/minimax/minimax.go`

- `DetectRequestMeta`: `Provider = "minimax"`. Read `Authorization: Bearer <key>` → class falls back to `"minimax-bearer"` (MiniMax keys have no stable prefix). Extract `$.model`, `$.stream`.
- `DetectResponseUsage`: non-streaming reads `$.usage.total_tokens` (MiniMax returns only total; split 0/total until provider changes). Streaming delegates; MiniMax's streaming emits usage in the final frame.

### 2.6 Generic — `adapters/generic/generic.go`

- `DetectRequestMeta`: `Provider = "unknown"`, `Model = ""`. Still computes `ApiKeyFingerprint` if any recognizable auth header present; `ApiKeyClass = ""`. Host-pattern match attempts before giving up (e.g. `*.openai.azure.com` → defers to Azure adapter, `bedrock*.amazonaws.com` → defers to Bedrock per s3).
- `DetectResponseUsage`: returns `Status = non_llm` and nil tokens. This adapter's job is to be safe for non-LLM traffic that flows through the proxy; it should not emit spurious provider/model labels.

### 2.7 Golden fixtures

For each of the six adapters, add golden fixtures under `adapters/<name>/testdata/`:
- `request_basic.json` — typical request
- `request_streaming.json` — streaming request
- `response_basic.json` — non-streaming response with usage
- `response_streaming.sse` — concatenated SSE frames
- `headers_<class>.txt` — header dumps exercising each class label path

Table-driven tests under `adapters/<name>/<name>_test.go` cover each fixture.

## Acceptance Criteria

- All six adapters implement the new interface methods; `go vet` and `go test -race -count=1 ./packages/shared/traffic/adapters/...` pass.
- For each adapter: at least one test case per `UsageStatus` value reachable by that adapter (non-streaming returns `ok`; streaming returns `streaming_reported` when golden SSE contains a usage frame; `streaming_unavailable` for a golden SSE that omits usage and has no tokenizer wired).
- Fingerprints are stable and deterministic across repeated test runs.
- Generic adapter does not emit `Provider = "openai"` on ambiguous input — only on exact host-pattern match.

## Non-Goals

- No tokenizer integration (covered by s4). `streaming_estimated` is not a reachable status from s2 adapters alone.
- No Bedrock / Vertex / GLM / DeepSeek — those are s3.
