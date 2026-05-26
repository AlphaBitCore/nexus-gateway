# E18 — Story 10: Agent Response Inspection + SSE

## Context

Per project memory `project_agent_no_response_inspection.md`, the Agent's MITM relay (`packages/agent/core/network/proxy/proxy.go:MITMRelay`) inspects only the first HTTP request after TLS decryption, then falls to a bidirectional raw relay. No response body buffering, no SSE frame parsing, no response-stage hook execution in the relay path. `intercept/handler.ProcessResponse` exists but is not wired into the relay flow.

This story extends the relay to buffer response bodies (10MB cap, matching Compliance Proxy), drive SSE frames through `shared/streaming`, and emit response-side LLM usage fields.

Depends on s4 (shared streaming) and s9 (request-side wiring).

## User Story

**As a** cost analyst,
**I want** Agent-captured AI traffic to include token usage — including streaming chat responses — identical to Compliance Proxy,
**so that** employee direct-to-provider calls are counted in our cost dashboards, not a blind spot.

## Tasks

### 10.1 Response buffering in `MITMRelay`

File: `packages/agent/core/network/proxy/proxy.go`

The existing `Relay()` call (line ~274 per investigation) does bidirectional byte-for-byte copy. Replace with a pipeline that:

1. Reads the upstream response headers.
2. Decides inspection mode based on `Content-Type`:
   - `text/event-stream` → SSE mode (use `shared/streaming.LivePipeline` with passthrough-plus-accumulate config: frames passthrough to client in real time, accumulator runs in parallel for usage extraction).
   - Other AI-shaped JSON (via adapter domain match) → buffer mode (10MB cap). Body is read fully before forwarding.
   - Non-matching content → legacy bidirectional raw relay (no change).
3. On completion, call `intercept/handler.ProcessResponse(resp, body)` with the captured body.

Memory cap: `maxInspectRespBodySize = 10 << 20`. On exceed, bypass inspection (emit `usage_extraction_status = "streaming_unavailable"` for the event) and continue relaying raw.

### 10.2 `ProcessResponse` wiring

File: `packages/agent/core/network/intercept/handler.go`

Ensure `ProcessResponse` populates usage fields on the audit event when invoked by the new relay path:

```go
respMeta := inst.Adapter.DetectResponseUsage(resp, body)
auditEvt.PromptTokens = respMeta.PromptTokens
auditEvt.CompletionTokens = respMeta.CompletionTokens
auditEvt.UsageExtractionStatus = string(respMeta.Status)
```

For streaming: `DetectResponseUsage` delegates to the accumulator populated during the SSE passthrough.

### 10.3 Shared streaming integration

Agent now depends on `packages/shared/transport/streaming`. Register the same adapter-matched `UsageAccumulator` factory (from s4) used by Compliance Proxy. The accumulator's tokenizer tier (Tier 2) is enabled by default for the Agent — unlike AI Gateway in s7 which skips it — because agent traffic rarely has `stream_options.include_usage` set by employees.

### 10.4 Upload payload extension

File: `packages/agent/core/observability/audit/event.go`

Add fields (if not already added in s9):

```go
PromptTokens          *int
CompletionTokens      *int
UsageExtractionStatus string
```

Update the `POST /api/internal/things/agent-audit` payload JSON schema accordingly. Hub handler (s9.4) translates these into `TrafficEventMessage`.

### 10.5 Local audit queue format

The Agent persists audit events to an encrypted SQLCipher queue. The queue's row schema must accommodate the new fields. If the schema is a JSON blob column, no migration needed; if typed columns, add columns with a small SQLCipher migration (see existing migration patterns in `packages/agent/core/observability/audit/store.go`).

### 10.6 Tests

- End-to-end intercept test: MITM a streaming Anthropic chat call → assert audit event has `prompt_tokens`, `completion_tokens`, `usage_extraction_status = "streaming_reported"`.
- End-to-end intercept test: MITM an OpenAI chat call without `stream_options.include_usage` → assert `usage_extraction_status = "streaming_estimated"` (Tier 2 tokenizer path) with token counts within ±5% of reference.
- Regression: non-AI traffic (e.g. a GitHub API call matching no adapter) still relays through the Agent with no inspection added.
- Memory test: craft a response larger than 10MB → assert relay completes without OOM and emits `streaming_unavailable`.

## Acceptance Criteria

- Agent MITM relay handles both buffered and streaming responses for AI-matched traffic.
- `packages/shared/transport/streaming` is imported and used by agent; no code duplicated.
- All acceptance criteria of E18 Requirements F10 and F7 pass under `go test -race -count=1 ./packages/agent/...`.
- Non-AI traffic is unaffected (no extra buffering, latency within noise of pre-epic).
- `project_agent_no_response_inspection.md` memory can be updated to note "response inspection added in E18" — not automated, but flagged in this story's completion note.

## Non-Goals

- NDJSON or other audit transport changes.
- Handling HTTP/2 gRPC streaming (Bedrock's SigV4 + event-stream is the one exception and is already covered via s3 Bedrock accumulator).
- Per-frame auditing (only final usage is recorded).
- Replacing SQLCipher or the local upload queue design.
