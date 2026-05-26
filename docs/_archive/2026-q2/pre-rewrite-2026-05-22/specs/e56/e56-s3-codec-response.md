# E56-S3 — Responses-API response-side codec

**Epic:** E56 OpenAI Responses-API ingress
**Type:** New codec
**Owner:** nexus
**Depends on:** S2.

## User story

> As an application developer, I want non-streaming Responses-API
> responses (`output[]` items, `usage`, `status`) returned in the same
> shape OpenAI ships regardless of which upstream provider Nexus routed
> to. As an audit operator, I want a Responses-shape response to
> normalize cleanly into the canonical assistant message so my dashboards
> show prompt/response text without per-ingress branches.

## Tasks

### T3.1 — Decode: Responses wire → canonical (non-stream)

**File:** `packages/ai-gateway/internal/providers/spec_openai/codec_responses.go` (extend from S2)

Implement `DecodeResponsesResponse(raw []byte) (canonicalbridge.CanonicalResponse, error)`. Mapping:

| Responses output item | Canonical |
|---|---|
| `{type:"message", role:"assistant", content:[{type:"output_text", text}]}` | Assistant message; `output_text.text` → `ContentText` block |
| `{type:"message", content:[{type:"output_text", text, annotations:[...]}]}` | Assistant message; annotations preserved in `nexus.ext.openai.responses.annotations[idx]` (Could / future canonical) |
| `{type:"function_call", name, arguments, call_id, id}` | `tool_calls[]` entry: `{id:call_id, type:"function", function:{name,arguments}}` |
| `{type:"reasoning", summary:[{type:"summary_text", text}]}` | accumulate text into `ContentReasoning` block on the assistant message (mirrors closed G5 anthropic gap pattern) |
| `{type:"web_search_call"/"file_search_call"/"image_generation_call"/"mcp_call"/"computer_call"/"code_interpreter_call"}` | preserved in `nexus.ext.openai.responses.builtin_tool_calls[]` so a follow-up re-encode round-trips |

`usage` mapping (via `specutil.ExtractOpenAIUsage` + new alias entry):

| Responses field | Canonical Usage field |
|---|---|
| `usage.input_tokens` | `PromptTokens` |
| `usage.output_tokens` | `CompletionTokens` |
| `usage.total_tokens` | `TotalTokens` |
| `usage.input_tokens_details.cached_tokens` | `PromptCacheTokens` (new alias — see S7 binding) |
| `usage.output_tokens_details.reasoning_tokens` | `ReasoningTokens` (via existing `specutil.ExtractReasoningTokens`) |

Top-level fields: `id`, `created_at`, `model`, `status` (`"completed"` / `"incomplete"` / `"failed"`) preserved in `nexus.ext.openai.responses.{id,created_at,status}` so T3.2 reproduces them.

### T3.2 — Encode: canonical → Responses wire (non-stream)

Implement `EncodeResponsesResponse(c canonicalbridge.CanonicalResponse, opts EncodeOpts) ([]byte, error)`. Used on the cross-format path so a target's canonical response re-encodes into Responses shape.

Top-level synth:

```go
{
  "id":         fallback("resp_"+request_id),
  "object":     "response",
  "created_at": now.Unix(),
  "status":     mapFinishReasonToResponsesStatus(c.FinishReason),
  "model":      c.Model,
  "output":     [],
  "usage":      {...},
}
```

Status mapping:

| canonical `FinishReason` | Responses `status` |
|---|---|
| `"stop"` / `"tool_calls"` | `"completed"` |
| `"length"` / `"max_tokens"` | `"incomplete"` (+ `incomplete_details.reason:"max_output_tokens"`) |
| `"content_filter"` | `"incomplete"` (+ `incomplete_details.reason:"content_filter"`) |
| error path (S8) | `"failed"` |

Output items:

- For each `ContentReasoning` block on the assistant message → emit one `{type:"reasoning", summary:[{type:"summary_text", text}]}` item.
- For each `ContentText` block → append to the current `{type:"message", role:"assistant"}` item's content as `{type:"output_text", text}`.
- For each `tool_calls[]` entry → emit `{type:"function_call", name, arguments, call_id:id, id}` item (preserves OpenAI's id ↔ call_id naming asymmetry).

Usage shape on egress:

```json
{
  "input_tokens":  N,
  "output_tokens": M,
  "total_tokens":  N+M,
  "input_tokens_details":  {"cached_tokens": X},
  "output_tokens_details": {"reasoning_tokens": R}
}
```

### T3.3 — Bridge registration

**File:** `packages/ai-gateway/internal/execution/canonicalbridge/bridge.go`

Register `FormatOpenAIResponses` in the response-side dispatch (`ResponseCanonicalToIngress`).

### T3.4 — Tests

**File:** `packages/ai-gateway/internal/providers/spec_openai/codec_responses_test.go` (extend)

1. Single text message round-trip (decode then re-encode, byte-stable on key set).
2. Function-call echo (mirrors existing `TestExtractRequest_ResponsesAPI_FunctionCallEcho`): `output: [{type:function_call,...}]` → canonical `tool_calls[]` → re-encode preserves call_id ↔ id.
3. Reasoning round-trip: `output: [{type:reasoning, summary:[{type:summary_text,text:"…thought…"}]}, {type:message,...}]` → canonical has both `ContentReasoning` and `ContentText` blocks → re-encode preserves order.
4. Usage extraction: input/output/cached/reasoning all populate canonical Usage; re-encode produces correct Responses-shape usage block.
5. Status mapping: 4 cases.
6. Built-in tool call preservation: web_search_call in input → in nexus.ext → on encode, re-appears in output[].

## Acceptance criteria

- AC-3.1: All 6 table cases pass.
- AC-3.2: For a same-shape passthrough (OpenAI → OpenAI / responses-api), this codec is NOT called — verified by a test that asserts the body bytes received by the client equal the body bytes received from upstream.
- AC-3.3: For a cross-format flow (Anthropic upstream → Responses ingress), the canonical Anthropic-decoded response re-encodes to Responses shape correctly; integration test in handler/integration_test.go (S10) pins this.

## Verification

```
go test ./packages/ai-gateway/internal/providers/spec_openai/ -run TestResponses -race -count=1
```

## Risks

- **R-3.1:** Annotations on `output_text` (citations, file refs) are not in canonical today. They land in `nexus.ext.openai.responses.annotations` so a future canonical-extension PR can lift them up without re-decoding. Test pins they survive round-trip.
- **R-3.2:** Responses `status` values may grow (`"queued"` / `"in_progress"` are observable on stream; on non-stream we should only see `"completed"` / `"incomplete"` / `"failed"`). If a different value appears, return canonical `FinishReason` unmodified and log a `canonicalext.WarnOnce`.
