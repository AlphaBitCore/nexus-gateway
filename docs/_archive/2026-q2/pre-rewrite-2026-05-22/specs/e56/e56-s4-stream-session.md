# E56-S4 — Responses-API streaming session

**Epic:** E56 OpenAI Responses-API ingress
**Type:** New streaming session
**Owner:** nexus
**Depends on:** S2, S3.

## User story

> As an application developer streaming a Responses-API request, I want
> Nexus to emit the OpenAI Responses SSE event grammar (response.created
> → output_item.added → output_text.delta → … → response.completed) with
> monotonic sequence_number and correct output_index / content_index,
> regardless of which upstream provider Nexus routed to.

## Tasks

### T4.1 — Forward stream session (Responses upstream → canonical chunks)

**File:** `packages/ai-gateway/internal/providers/spec_openai/stream_responses.go` (new)

Implement `responsesStreamSession` satisfying `providers.StreamSession`. Parse the following events (per the snapshot from context7 `/openai/openai-python`'s `responses.api.md` event-types list):

| SSE event type | Action |
|---|---|
| `response.created` | record `response.id`, `response.model`; emit canonical role chunk |
| `response.in_progress` | no-op (informational) |
| `response.queued` | no-op (informational; rare) |
| `response.output_item.added` | track item index; if `item.type == "message"` start an assistant content stream; if `item.type == "function_call"` start a tool_call accumulator; if `item.type == "reasoning"` start a reasoning accumulator |
| `response.content_part.added` | track content_index within the current item |
| `response.output_text.delta` | emit `CanonicalChunk{ContentDelta: event.delta}` |
| `response.output_text.done` | finalize current content part |
| `response.function_call_arguments.delta` | emit `CanonicalChunk{ToolCallArgumentsDelta: event.delta, ToolCallIndex: itemIdx}` |
| `response.function_call_arguments.done` | finalize tool call |
| `response.reasoning_summary_text.delta` | emit `CanonicalChunk{ReasoningDelta: event.delta}` |
| `response.reasoning_summary_text.done` | finalize reasoning |
| `response.content_part.done` / `response.output_item.done` | bookkeeping; no canonical emission |
| `response.refusal.delta` / `.done` | emit `CanonicalChunk{RefusalDelta: ...}` if canonical supports it; otherwise canonicalext.WarnOnce + drop |
| `response.completed` | emit final usage chunk (via specutil.ExtractOpenAIUsage + the new alias from S7) + finish_reason chunk |
| `response.failed` | emit `CanonicalChunk{Err: ...}` → drives S8 SSE error frame |
| `response.incomplete` | emit `CanonicalChunk{FinishReason: "length"|"content_filter", ...}` based on `incomplete_details.reason` |
| `response.error` (legacy alias) | treat same as `response.failed` |
| built-in tool events (`response.web_search_call.*`, `response.file_search_call.*`, `response.image_generation_call.*`, `response.mcp_call_arguments.*`, `response.code_interpreter_call_code.*`, `response.computer_call.*`) | preserve in `nexus.ext.openai.responses.builtin_tool_events[]` on the canonical response; not surfaced as canonical chunks (would only matter for same-shape passthrough, which doesn't go through this session) |

Buffer / streaming concerns inherited from `wireformat/` / `shared/streaming/`: chunked SSE reader, never-fully-buffered, backpressure-aware.

### T4.2 — Reverse stream session (canonical chunks → Responses SSE)

**File:** same.

Implement `EncodeResponsesStreamChunk(chunk canonicalbridge.CanonicalChunk, state *responsesStreamState) []byte` invoked by `canonicalbridge.ResponseStreamCanonicalToIngress` when ingress=Responses and target wire is non-Responses.

State machine:

```
INIT
  → on first chunk (any role/content/tool_call/reasoning):
      emit response.created (id=resp_<request_id>, model=routed_model, status=in_progress)
      emit response.in_progress
      transition → ITEM_OPEN_PENDING
ITEM_OPEN_PENDING
  → on ContentDelta:
      emit response.output_item.added (output_index=0, item={type:"message", id:"msg_…", role:"assistant", status:"in_progress"})
      emit response.content_part.added (output_index=0, content_index=0, part={type:"output_text", text:""})
      emit response.output_text.delta (output_index=0, content_index=0, delta=…)
      transition → MESSAGE_OPEN
  → on ReasoningDelta:
      emit response.output_item.added (item={type:"reasoning", id:"rs_…"})
      emit response.reasoning_summary_part.added (output_index=N, summary_index=0)
      emit response.reasoning_summary_text.delta (output_index=N, summary_index=0, delta=…)
      transition → REASONING_OPEN
  → on ToolCallArgumentsDelta:
      emit response.output_item.added (item={type:"function_call", id:"fc_…", call_id:…, name:…, arguments:""})
      emit response.function_call_arguments.delta (output_index=N, delta=…)
      transition → FCALL_OPEN
MESSAGE_OPEN / REASONING_OPEN / FCALL_OPEN
  → on matching delta: emit corresponding delta event
  → on switch (e.g. message → reasoning):
      emit response.content_part.done + response.output_item.done for current,
      then re-enter ITEM_OPEN_PENDING and open the new item
FINAL
  → on FinishReason + Usage chunk:
      emit response.output_item.done for any open item
      emit response.completed (response=full snapshot, status=completed|incomplete, usage=…)
```

`sequence_number` is a monotonic counter on `state` incremented per emitted event.

### T4.3 — Same-shape passthrough non-path

`responsesStreamSession` is NOT used on same-shape OpenAI passthrough (`ingress=Responses, target=OpenAI`). On that path the upstream SSE bytes flow through unchanged via the existing `wireformat/passthrough` stream copier — verified by `handler/proxy_passthrough_fallback_test.go`-style integration test in S10.

### T4.4 — Tests

**File:** `packages/ai-gateway/internal/providers/spec_openai/stream_responses_test.go` (new)

Golden-file driven tests:

1. **Forward — text streaming**: feed a captured Responses SSE transcript ("hello world" in 3 deltas); assert canonical chunks sequence: role → content("hello") → content(" world") → finish_reason("stop") + usage.
2. **Forward — function call streaming**: tool_call arguments delivered in 4 chunks; canonical emits 4 ToolCallArgumentsDelta + final usage.
3. **Forward — reasoning + content interleaved**: `response.reasoning_summary_text.delta` (3 chunks) → `response.output_text.delta` (2 chunks); canonical emits 3 ReasoningDelta then 2 ContentDelta in order.
4. **Forward — response.failed mid-stream**: assert `CanonicalChunk{Err: ProviderError{...}}` emitted.
5. **Reverse — canonical chunks → Responses SSE**: synthetic CanonicalChunk sequence (role + content + content + finish) produces a well-formed event sequence with correct sequence_number ordering and output_index=0.
6. **Reverse — counter discipline**: feed 100 chunks; assert sequence_number starts at 0 and is contiguous; output_index increments only on item switches.
7. **Reverse — incomplete due to length**: canonical finish_reason="length" → emits `response.incomplete` with `incomplete_details.reason:"max_output_tokens"`, NOT `response.completed`.

Golden files in `testdata/responses-stream/*.txt` (captured via `/test-openai-responses` skill against the prod OpenAI endpoint, redacted for sensitive content).

## Acceptance criteria

- AC-4.1: 7 test cases pass.
- AC-4.2: Reverse encoding output validated by feeding it back through the forward decoder (round-trip) — produces same canonical chunk sequence modulo bookkeeping events.
- AC-4.3: Stream session honors backpressure (chunked, never-fully-buffered) — verified by buffer size assertion in golden tests.

## Verification

```
go test ./packages/ai-gateway/internal/providers/spec_openai/ -run TestResponsesStream -race -count=1
```

## Risks

- **R-4.1:** OpenAI's Responses event grammar still gains new event types (refusal events, mcp_call sub-events) as built-in tools evolve. Mitigation: an "unknown event" default branch logs `canonicalext.WarnOnce(event.type)` and skips — never aborts the stream. New event types added by appending to the switch + adding a golden file.
- **R-4.2:** `sequence_number` MUST start at 0 and be contiguous for the OpenAI SDK to accept the stream. The reverse encoder's monotonic counter is the only correct source; never derive it from the canonical chunk index (chunks can be coalesced or split).
- **R-4.3:** `response_index` / `output_index` / `content_index` confusion: each lives in a different scope. Mistakes here cause SDK parse failures that look like 200-OK-with-garbage. Mitigation: explicit fixture tests with annotated indices.
