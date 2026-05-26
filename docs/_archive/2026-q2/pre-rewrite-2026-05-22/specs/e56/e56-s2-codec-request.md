# E56-S2 — Responses-API request-side codec

**Epic:** E56 OpenAI Responses-API ingress
**Type:** New codec
**Owner:** nexus
**Depends on:** S1.

## User story

> As an application developer using OpenAI Responses API, I want my
> request fields (`input`, `instructions`, `tools`, `reasoning.effort`,
> `text.format`, `max_output_tokens`, `previous_response_id`, …) to
> translate losslessly into Nexus's canonical chat-completions form so
> the router, hooks, quota, and audit see consistent payloads regardless
> of which ingress format I used.

## Tasks

### T2.1 — Decode: Responses wire → canonical

**File:** `packages/ai-gateway/internal/providers/spec_openai/codec_responses.go` (new)

Implement `DecodeResponsesRequest(raw []byte) (canonicalbridge.CanonicalRequest, error)`. Field mapping:

| Responses-API field | Canonical chat-completions field | Notes |
|---|---|---|
| `model` | `model` | direct |
| `input: string` | `messages = [{role:"user", content:string}]` | shorthand expansion |
| `input: array of input messages` | `messages = [...]` | each item's `role` + `content[]` blocks (input_text / input_image) map to canonical content blocks |
| `instructions: string` | prepend `{role:"system", content:instructions}` to messages | only if non-empty; do not duplicate if first message is already `system` |
| `max_output_tokens` | `max_completion_tokens` | direct rename |
| `reasoning.effort` | `nexus.ext.openai.reasoning_effort` | via `canonicalext.Set` |
| `temperature` / `top_p` | `temperature` / `top_p` | direct |
| `stream` | `stream` | direct |
| `parallel_tool_calls` | `parallel_tool_calls` | direct |
| `metadata` | `metadata` | direct |
| `tools[]` (function shape) | `tools[]` (chat-completions function shape) | structural translation |
| `tools[]` (built-in: web_search / file_search / computer_use_preview / image_generation / mcp / code_interpreter) | accumulate in `nexus.ext.openai.builtin_tools` (array of original tool entries) | S6 rejects on cross-format route |
| `tool_choice` | `tool_choice` | direct (mostly identical) |
| `text.format: { type: "json_schema" / "json_object" }` | `response_format` | direct re-shape |
| `previous_response_id` / `store` / `truncation` / `include` | preserved in `nexus.ext.openai.responses.{previous_response_id,store,truncation,include}` | S6 rejects on cross-format route; same-shape passthrough never invokes this codec |

Implementation rule: use `gjson` to read, build canonical via `sjson` over a base JSON template. Do NOT introduce reflective struct marshaling.

### T2.2 — Encode: canonical → Responses wire

**File:** same file.

Implement `EncodeResponsesRequest(c canonicalbridge.CanonicalRequest) ([]byte, error)`. Used by S11 auto-upgrade and by any future caller that needs to send canonical out as Responses wire (e.g. a hypothetical Nexus-as-client downstream). Inverse of T2.1 for the fields above. Special handling:

- If canonical has a leading `system` message AND the canonical request was created from a Responses ingress (look up `nexus.ext.openai.responses.original_instructions` if S2 stored it), encode it back to `instructions` rather than as a user-input-message system block.
- If canonical has `nexus.ext.openai.builtin_tools`, splice those entries back into `tools[]`.
- If `nexus.ext.openai.responses.{previous_response_id,store,truncation,include}` are present, restore them as top-level Responses fields.
- `max_completion_tokens` → `max_output_tokens`.

### T2.3 — Bridge registration

**File:** `packages/ai-gateway/internal/execution/canonicalbridge/bridge.go`

Register `FormatOpenAIResponses` in the ingress-format → codec dispatch table:

```go
case providers.FormatOpenAIResponses:
    return spec_openai.DecodeResponsesRequest(body)
```

### T2.4 — Tests

**File:** `packages/ai-gateway/internal/providers/spec_openai/codec_responses_test.go` (new)

Table-driven tests:

1. Text-only `input: "hello"` → 1 user message, text content.
2. Multi-turn `input: [{role:user,content:[{type:input_text,text:"q"}]}, {role:assistant,content:[{type:output_text,text:"a"}]}, {role:user,content:[{type:input_text,text:"b"}]}]` → 3 canonical messages with correct roles + content.
3. `instructions: "be terse"` + 1 user message → canonical messages = `[{system}, {user}]`.
4. Function tools — round-trip (decode then re-encode) byte-equal modulo key order.
5. Reasoning effort `"high"` → `nexus.ext.openai.reasoning_effort: "high"`.
6. Structured outputs `text.format: {type:json_schema,...}` → canonical `response_format: {type:json_schema,...}`.
7. Built-in tool `{type:"web_search"}` → empty `canonical.tools`, populated `nexus.ext.openai.builtin_tools`.
8. Stateful fields → stored under `nexus.ext.openai.responses.*` for S6 to inspect.

## Acceptance criteria

- AC-2.1: All 8 table cases pass.
- AC-2.2: Round-trip (decode → encode) for non-stateful fields produces semantically equivalent JSON.
- AC-2.3: `nexus.ext.openai.*` namespacing follows `canonicalext.Get/Set` per §3a Rule 4 (no ad-hoc string concat).

## Verification

```
go test ./packages/ai-gateway/internal/providers/spec_openai/ -run TestResponses -race -count=1
go test ./packages/ai-gateway/internal/execution/canonicalbridge/ -race -count=1
```

## Risks

- **R-2.1:** OpenAI's Responses input-message content-block types are still evolving (input_text, input_image, input_audio, input_file, input_text_with_annotations). Pin against the snapshot captured via context7 at S2 start; explicitly handle unknown content-block types as a pass-through into canonical with a `canonicalext.WarnOnce` so we can see drift without breaking traffic.
