# E28 — S7 — Cross-format streaming transcoder

## 0. Scope

E28-S6 declared the `StreamTranscoder` interface (Surface S6 in the
canonical-hub matrix) but **deferred the implementation**. The gateway
therefore still rejects every cross-format streaming request that does
not pass `StreamShapeCompatible(ingress, target)` — currently the
`openAILike(ingress) && openAILike(target)` rule plus same-format —
with `cross_format_stream_unsupported` (FR-H5). Same-format streaming
already works end-to-end (E28-S5 wires per-provider `StreamDecoder`
implementations, including Anthropic's typed `event:` frames preserved
through E28-S5 + the May-2026 fixes).

This story implements S6 so:

- **OpenAI ingress + Anthropic provider** stream returns OpenAI
  `chat.completion.chunk` SSE frames to the client.
- **Anthropic ingress + OpenAI-shape provider** stream returns
  Anthropic `message_start / content_block_* / message_delta /
  message_stop` SSE frames to the client.
- **Gemini ingress + any provider** stream returns Gemini
  `streamGenerateContent` NDJSON frames (one JSON object per line, no
  `data: ` prefix).
- The cross-format streaming gate flips from "hard 400 reject" to
  "look up a transcoder; reject only if no transcoder is registered for
  the (ingress, target) pair".

Non-streaming cross-format is already complete and stays unchanged
(`canonicalbridge.IngressChatToCanonical` /
`ResponseCanonicalToIngress`).

## 1. References

- E28 requirements: `docs/developers/specs/e28/e28-hub-canonical-routing.md`
  (FR-H5 governs the streaming gate)
- Predecessor SDD: `docs/developers/specs/e28/e28-s6-canonical-hub-completeness.md`
  (Section 2.3 Surface S6 row, Section 4 Task **T-STREAM-TRANSCODER**)
- Provider streaming decoders (S5):
  `packages/ai-gateway/internal/providers/spec_*/stream.go`
- Current cross-format gate:
  `packages/ai-gateway/internal/handler/cross_format.go`
  (`writeCrossFormatStreamUnsupported` at proxy.go phase 4.1) plus
  `packages/ai-gateway/internal/execution/canonicalbridge/bridge.go::StreamShapeCompatible`
- Live SSE pipeline:
  `packages/ai-gateway/internal/streaming/{sse.go,live.go}`
- Format upstream specs:
  - OpenAI streaming chunks <https://platform.openai.com/docs/api-reference/chat/streaming>
  - Anthropic Messages streaming <https://docs.anthropic.com/en/api/messages-streaming>
  - Gemini streamGenerateContent <https://ai.google.dev/api/generate-content>

## 2. User story

*"As a gateway operator, I can point a streaming `/v1/chat/completions`
or `/v1/messages` request at a provider that speaks a different wire
format and receive SSE frames in the shape my client's SDK expects,
without the gateway rejecting the request and without my client
needing per-provider parser code."*

Acceptance: same parity matrix that today proves cross-format **non-
streaming** parity (E28-S6 round-trip golden) extends to streaming —
the byte stream emitted to the ingress wire validates against the
ingress format's SSE / NDJSON specification, and the assistant-visible
text plus tool-call arguments plus stop reason match the corresponding
direct-provider call.

## 3. Target architecture

### 3.1 Where the transcoder runs

Today's streaming path:

```
StreamSession.Next() → providers.Chunk
    │ (chunkSSEReader)
    ▼
raw provider SSE bytes (chunk.RawBytes)
    │ (streaming.LivePipeline)
    ▼
WriteTypedEvent(client, eventType, data)      // bytes-level passthrough
```

Insert the transcoder **at the source** — replace `chunkSSEReader`'s
"emit RawBytes verbatim" branch with "emit transcoder.Encode(chunk)
when ingress != provider":

```
StreamSession.Next() → providers.Chunk           ← S5 unchanged
    │ (chunkSSEReader)
    ▼
if ingress == providerFormat:                    ← passthrough
    bytes = chunk.RawBytes
else:
    bytes = transcoder(ingress).Encode(chunk)    ← new S6 logic
    │
    ▼
streaming.LivePipeline (parses & relays bytes)
    ▼
WriteTypedEvent(client, ...)
```

This keeps LivePipeline a bytes-in / bytes-out hook + flush surface;
the transcoder is a pure `(Chunk, state) → []byte` function. No new
goroutines, no new buffering, no change to the cancellation / deadline
plumbing.

### 3.2 Why not "canonical chunk + ingress encoder"

An alternative was to keep LivePipeline canonical (always OpenAI
shape) and transcode at the very last write to the client. That is
cleaner for hook content extraction (`ExtractDeltaText` already speaks
canonical OpenAI) but blows up the change surface: every checkpoint /
compliance hook would need to operate on canonical chunks, the audit
capture path would need to know which bytes are "as transmitted to
client" vs "as inspected by hooks", and the relay would need to carry
two parallel buffers.

The recommended shape is option (a): transcode early, accept that the
LivePipeline's text-extraction sees ingress-shape data. Hooks already
have an existing limitation today on Anthropic-format streams (text is
in `delta.text`, not `choices.0.delta.content`), so this story does
**not** regress hook visibility — it preserves the same lossy
extraction the gateway has today on same-format Anthropic streaming.
Improving hook text extraction across formats is a separate, additive
concern tracked under **T-HOOK-MULTI-INGRESS** below.

### 3.3 Interface

E28-S6 already declared the interface body in `canonicalbridge/stream.go`:

```go
type StreamTranscoder interface {
    // Write consumes one canonical chunk and returns the bytes to
    // emit on the ingress SSE / NDJSON connection. When the chunk is
    // terminal, the transcoder appends the ingress-specific
    // end-of-stream marker (OpenAI "data: [DONE]\n\n", Anthropic
    // "event: message_stop\ndata: {...}\n\n", Gemini final
    // generateContent JSON line).
    Write(ctx context.Context, chunk providers.Chunk) ([]byte, error)
}
```

This story keeps that signature but makes it stateful — Anthropic's
ingress shape requires emitting `message_start` / `content_block_start`
synthesised from the provider's first chunk(s), and `content_block_stop`
ahead of `message_stop`, so the transcoder must remember whether it has
already opened a content block. Stateful transcoders are constructed
once per stream:

```go
// canonicalbridge/stream.go (extension)

// NewStreamTranscoder returns a per-stream transcoder for the given
// (ingress, target) pair, or (nil, false) when no cross-format
// translation is required (same-format passthrough). The boolean
// `crossFormat` lets the proxy decide whether to flip ChunkSSEReader
// into transcode mode without re-running format checks.
func (b *Bridge) NewStreamTranscoder(ingress, target providers.Format) (t StreamTranscoder, crossFormat bool)
```

Per-pair implementations live as separate types under
`canonicalbridge/stream_*.go`:

| ingress    | target group      | implementation                                      |
|------------|-------------------|-----------------------------------------------------|
| openai-shape | anthropic       | `openaiFromAnthropic` — synthesises chat.completion.chunk envelope |
| openai-shape | gemini / vertex | `openaiFromGemini`                                  |
| openai-shape | bedrock         | future (no impl in this story)                      |
| anthropic    | openai-shape    | `anthropicFromOpenAI` — synthesises message_start + content blocks |
| anthropic    | gemini / vertex | `anthropicFromGemini`                               |
| gemini       | openai-shape    | `geminiFromOpenAI`                                  |
| gemini       | anthropic       | `geminiFromAnthropic`                               |

`openai-shape` covers `FormatOpenAI`, `FormatDeepSeek`, `FormatGLM`,
`FormatAzureOpenAI`, `FormatMoonshot`, `FormatMistral`, `FormatGroq`,
`FormatTogether`, `FormatFireworks`, `FormatPerplexity`, `FormatXai`,
`FormatMiniMax`, `FormatHuggingFace` — matched via
`providers.Format.IsOpenAIWireShape()`.

`vertex` shares Gemini transcoder logic by construction.

Pairs not in the table return `(nil, false)` and the gate keeps
returning `cross_format_stream_unsupported` for those combos. The
matrix can grow incrementally; each additional cell is a one-file
addition under `canonicalbridge/`.

### 3.4 Wire-in point

`packages/ai-gateway/internal/handler/proxy.go::handleStream`
constructs `chunkSSEReader` today. Replace with:

```go
transcoder, cross := h.deps.CanonicalBridge.NewStreamTranscoder(
    resolved.BodyFormat,
    providers.Format(target.AdapterType),
)
sseReader := newChunkSSEReader(r.Context(), result.Stream, transcoder, cross)
```

`chunkSSEReader.Read` switches on `cross`:

- `cross == false` (passthrough or pair without a registered
  transcoder): existing behaviour — emit `chunk.RawBytes` (current
  fix to also forward `message_stop` before `[DONE]` stays).
- `cross == true`: call `transcoder.Write(ctx, chunk)` and emit the
  returned bytes. On `chunk.Done`, the transcoder is responsible for
  appending the ingress-correct terminator (OpenAI `data: [DONE]\n\n`,
  Anthropic `message_stop`, Gemini final JSON line with empty
  candidates).

`canonicalbridge.StreamShapeCompatible(ingress, target)` is replaced by
`b.NewStreamTranscoder(ingress, target) != nil OR ingress == target OR
both openAI-shape`. The `writeCrossFormatStreamUnsupported` path stays
for the genuine "no transcoder registered" miss.

### 3.5 Tool-use, reasoning, usage

Each transcoder MUST surface every canonical channel the
`providers.Chunk` carries:

- `Chunk.Delta` → assistant-visible text on the ingress wire.
- `Chunk.ReasoningDelta` → ingress-specific reasoning channel
  (OpenAI `delta.reasoning_content`, Anthropic
  `content_block_delta.thinking_delta`, Gemini `thoughtsTokenCount`
  is not a wire field — drop with debug log; reasoning text is not in
  Gemini's response wire today).
- `Chunk.ToolCallDeltas` → ingress-specific tool-call frames:
  - OpenAI ingress: `delta.tool_calls[i] = { index, id, type:"function", function: { name, arguments } }`.
  - Anthropic ingress: a new `content_block_start` (type `tool_use`,
    id, name) on first delta per index; `content_block_delta` with
    `input_json_delta` for each `Arguments` patch; matching
    `content_block_stop` on next index change or terminal chunk.
  - Gemini ingress: `candidates[0].content.parts[].functionCall =
    { name, args: <merged args object> }` — Gemini does not stream
    partial function calls, so the Gemini transcoder MUST buffer
    `ToolCallDeltas` until it has a complete arguments object before
    emitting.
- `Chunk.Usage` → terminal-chunk `usage` field on each ingress shape;
  Anthropic transcoder also writes `message_delta.usage` (OpenAI
  reports usage only on the final chunk when
  `stream_options.include_usage=true`; the transcoder is permissive
  and emits regardless because the canonical chunk already carries
  the value).

### 3.6 Hook text extraction

`streaming.ExtractDeltaText` only knows the OpenAI
`choices.0.delta.content` path today. Once cross-format streaming
ships, an Anthropic-ingress request whose upstream is OpenAI-shape
will produce Anthropic-shape SSE that LivePipeline cannot mine for
text. The user-visible effect is that response-stage hooks see no
text and never trigger their checkpoint. This is a **regression
relative to "cross-format streaming was just rejected before"** only
if the deployment had response hooks configured for that ingress
path.

The follow-up task **T-HOOK-MULTI-INGRESS** below adds
ingress-aware text extraction to LivePipeline. It is split out so the
streaming-transcoder MVP can ship independently.

## 4. Tasks

Workflow: Architecture → Requirements → SDD (this doc) → OpenAPI →
Code → Unit tests → Verify.

Architecture, requirements, and SDD updates are **complete** in this
document; OpenAPI does not change (the SSE wire format is governed by
provider docs, not by Nexus's admin OpenAPI). Code + tests follow.

### T-S7-INTERFACE-WIDEN — Per-stream constructor on Bridge

- **Files**: `packages/ai-gateway/internal/execution/canonicalbridge/stream.go`
- **Scope**: Keep the `StreamTranscoder` interface declared in E28-S6.
  Add `Bridge.NewStreamTranscoder(ingress, target providers.Format)
  (StreamTranscoder, bool)` that returns the per-pair transcoder
  plus a `crossFormat` boolean.
- **Acceptance**: unit test exhausts every (ingress, target) pair in
  `providers.AllFormats()`; for unregistered pairs returns `(nil,
  false)`; for same-format pairs returns `(nil, false)`; for
  registered cross-format pairs returns a non-nil transcoder with
  `crossFormat=true`.

### T-S7-OAI-FROM-ANTHROPIC — OpenAI ingress, Anthropic provider

- **Files**: `canonicalbridge/stream_openai_from_anthropic.go` (new),
  test fixtures under `canonicalbridge/testdata/stream_oai_from_ant_*`.
- **Scope**: Stateful transcoder. Inputs: provider chunks captured
  from `spec_anthropic.StreamDecoder` (`message_start`,
  `content_block_start`, `content_block_delta` text /
  `input_json_delta` / `thinking_delta`, `content_block_stop`,
  `message_delta`, `message_stop`). Output: OpenAI
  `chat.completion.chunk` JSON SSE frames.

  Mapping rules:

  | Provider chunk           | Emit                                                           |
  |--------------------------|----------------------------------------------------------------|
  | `message_start`          | first chunk with `choices[0].delta.role="assistant"` and `id`  |
  | `content_block_start` (text) | no-op (text deltas merge into a single OpenAI message)     |
  | `content_block_start` (tool_use) | chunk with `delta.tool_calls[i] = {index, id, type:"function", function: {name, arguments:""}}` |
  | `content_block_delta` (text_delta) | chunk with `delta.content = <text>`                  |
  | `content_block_delta` (input_json_delta) | chunk with `delta.tool_calls[i].function.arguments = <partial>` |
  | `content_block_delta` (thinking_delta) | chunk with `delta.reasoning_content = <text>`     |
  | `content_block_delta` (signature_delta) | drop (signatures are Anthropic-only metadata)    |
  | `content_block_stop`     | no-op (OpenAI does not delimit text blocks)                    |
  | `message_delta` (with usage) | terminal-shape chunk with `finish_reason` set + optional usage |
  | `message_stop`           | terminal: emit `data: [DONE]\n\n`                              |
  | `ping`                   | drop                                                           |
  | `error`                  | terminal: `data: {"error":{"message":..., "type":"upstream"}}\n\ndata: [DONE]\n\n` |

- **Acceptance**: golden tests under
  `canonicalbridge/testdata/stream_oai_from_ant_*.{anthropic_in.sse,openai_out.sse}`.
  Five fixtures: text-only short, text-only multi-paragraph, tool-use
  single, tool-use parallel (2 indices), error mid-stream. Each
  fixture is hand-checked against the upstream specs cited in §1.

### T-S7-ANT-FROM-OAI — Anthropic ingress, OpenAI-shape provider

- **Files**: `canonicalbridge/stream_anthropic_from_openai.go` (new),
  test fixtures.
- **Scope**: Stateful transcoder. Synthesises Anthropic event names
  from incoming OpenAI chunk fields.

  Mapping rules:

  | Provider chunk (OpenAI shape)              | Emit on Anthropic wire                                                                                       |
  |--------------------------------------------|--------------------------------------------------------------------------------------------------------------|
  | first chunk (`delta.role="assistant"`)     | `event: message_start`+`data: {"type":"message_start","message":{"id":..,"role":"assistant","model":<modelID>,"content":[],"stop_reason":null,"usage":{"input_tokens":<promptIn or 0>}}}` |
  | first text delta                           | `event: content_block_start` (index 0, type:"text") **then** `event: content_block_delta` with `text_delta`  |
  | subsequent text deltas                     | `event: content_block_delta` with `text_delta`                                                               |
  | first tool-call delta on a new index N     | `event: content_block_stop` (close any open block) **then** `event: content_block_start` (index N, type:"tool_use", id, name) |
  | tool-call argument delta                   | `event: content_block_delta` with `input_json_delta` carrying `partial_json`                                 |
  | reasoning delta                            | `event: content_block_start` (index R, type:"thinking") **then** `event: content_block_delta` with `thinking_delta` |
  | finish_reason                              | `event: content_block_stop` for the open block, then `event: message_delta` with `stop_reason` mapped (`stop`→`end_turn`, `length`→`max_tokens`, `tool_calls`→`tool_use`, `content_filter`→`refusal`) and the usage envelope |
  | terminal `[DONE]`                          | `event: message_stop`+`data: {"type":"message_stop"}\n\n`                                                    |

- **Acceptance**: same five fixture pattern as T-S7-OAI-FROM-ANTHROPIC,
  but with an OpenAI-chunk input file and an Anthropic-event output
  file. The output's `event: ` lines are byte-equal to a real
  Anthropic streaming response for the same conversation.

### T-S7-GEMINI-IN — Gemini ingress, any target

- **Files**: `canonicalbridge/stream_gemini_from_openai.go`,
  `canonicalbridge/stream_gemini_from_anthropic.go`.
- **Scope**: Gemini's `streamGenerateContent` returns NDJSON (one
  JSON per line, `\n` delimiter, no SSE `data:` prefix). The
  transcoder emits one line per upstream chunk with
  `candidates[0].content.parts[].text` filled (text deltas),
  `candidates[0].content.parts[].functionCall` (tool deltas — buffer
  arguments until complete because Gemini does not stream partials),
  and final `candidates[0].finishReason` mapped from upstream stop
  reason.
- **Acceptance**: fixture coverage for text-only and tool-call cases.
  Output is valid `application/x-ndjson` (each line individually
  parsable; no trailing comma).

### T-S7-GEMINI-OUT — Gemini provider, any ingress

- **Files**: `canonicalbridge/stream_openai_from_gemini.go`,
  `canonicalbridge/stream_anthropic_from_gemini.go`.
- **Scope**: Inverse of T-S7-GEMINI-IN. Inputs are Gemini chunks
  (already decoded by `spec_gemini.StreamDecoder`). Output is the
  ingress-shape SSE.
- **Acceptance**: matches the corresponding ingress's golden fixtures.

### T-S7-WIRE-IN — Replace chunkSSEReader's hardcoded passthrough

- **Files**: `packages/ai-gateway/internal/handler/proxy.go`
  (`handleStream`, `chunkSSEReader`, `newChunkSSEReader`).
- **Scope**: Inject the transcoder. Drop the
  `cross_format_stream_unsupported` 400 when a transcoder is
  registered. Update the stream-shape gate to:

  ```go
  transcoder, cross := h.deps.CanonicalBridge.NewStreamTranscoder(
      resolved.BodyFormat, providers.Format(target.AdapterType),
  )
  if isStream && resolved.Endpoint == providers.EndpointChatCompletions &&
      ingressFormat != providers.Format(target.AdapterType) &&
      transcoder == nil {
      h.writeCrossFormatStreamUnsupported(w, rec, ...)
      return
  }
  ```

- **Acceptance**: handler integration test (httptest stub
  StreamSession) confirms the transcoded bytes end up in the response
  writer; existing passthrough cases (same-format, both
  openAI-shape) keep emitting `chunk.RawBytes` byte-for-byte.

### T-S7-MATRIX-EXPAND — Update reference matrix tests

- **Files**: `canonicalbridge/matrix_test.go`.
- **Scope**: `refChatRoutable` already covers `(openAILike, anything)`
  and `(hubIngress, openAILike)`. Add a new test
  `TestStreamRoutable` that runs the cross-product and asserts every
  pair the bridge claims is routable for chat **either** has
  `ingress == target`, **or** is openai-shape ↔ openai-shape, **or**
  has a registered transcoder. No more silently un-streamable pairs.
- **Acceptance**: new test passes; older
  `TestChatRoutable_MatchesReferenceMatrix` keeps green.

### T-S7-PARITY-VERIFY — VK-vs-direct streaming parity

- **Files**: integration script under
  `tests/parity/cross_format_stream.py` (new) +
  `tests/parity/README.md` (extend if exists).
- **Scope**: Mirror the Python `parity_test.py` already used in the
  Apr/May 2026 fix matrix. Decode the `Credential` table, then for
  each ingress in {`openai`, `anthropic`, `gemini`}: send a streaming
  request to the gateway with a model whose provider format differs
  from the ingress, and compare the resulting ingress-shape SSE / NDJSON
  byte-stream against an upstream call done in the ingress's native
  shape (so the comparison is event-name-by-event-name on the ingress
  contract, not byte-for-byte on the upstream wire).
- **Acceptance**: every ingress emits its expected event sequence
  (OpenAI `chat.completion.chunk`s ending with `[DONE]`; Anthropic
  `message_start … message_stop`; Gemini NDJSON terminating with
  `finishReason`) and the assistant text is non-empty for a
  deterministic prompt with `temperature=0`.

### T-HOOK-MULTI-INGRESS — Make LivePipeline text extraction
ingress-aware (follow-up, separate PR)

- **Status**: Out of scope for this story; tracked here so the
  streaming-transcoder MVP can ship without it.
- **Files**: `packages/ai-gateway/internal/streaming/live.go`
  (`ExtractDeltaText`).
- **Scope**: Replace the hard-coded
  `gjson.Get(data, "choices.0.delta.content")` with a per-ingress
  dispatch (OpenAI → existing path, Anthropic → `delta.text` from
  `content_block_delta`, Gemini → `candidates.0.content.parts.0.text`).
  The pipeline already knows the ingress format via
  `StreamHookContext.IngressType`.
- **Acceptance**: `TestLivePipeline_ChunkText_PerIngress` covers the
  three formats; checkpoints fire on Anthropic / Gemini streams when
  the accumulated assistant text crosses `FirstInspectChars`.

## 5. Open questions — please decide before code

### Q1 — Tool-call ordering on Anthropic ingress

Anthropic `content_block_*` events tag each content block with a
stable `index`. OpenAI tool-call deltas tag with a `Index` too, but
Anthropic forbids reusing an index across blocks of different types
(text vs tool_use). When an OpenAI upstream emits text first then a
tool call, the transcoder must close the text block (`content_block_stop`
index 0), open a new tool_use block at a higher index, and never
reuse index 0.

- **A. Track the next free index per type** (text=0, tool_use starts
  at 1+text_blocks_seen) — Anthropic-spec-correct, matches what the
  Anthropic upstream emits.
- **B. Use the upstream OpenAI Index unchanged** — simpler but emits
  invalid `content_block_start` events when text and tool calls
  overlap on the same numeric index.

Recommend **A**.

### Q2 — Reasoning channel on Gemini

Gemini streams thought tokens in
`candidates[].content.parts[].thought` (boolean) and surfaces the
total via `usageMetadata.thoughtsTokenCount`, but does not emit per-
delta thinking text on the wire. The OpenAI canonical chunk's
`reasoning_content` and Anthropic's `thinking_delta` therefore have
no mechanical mapping into Gemini ingress.

- **A. Drop reasoning deltas with a debug log** — simplest; matches
  Gemini's own behaviour.
- **B. Concatenate reasoning text into a synthetic non-streamed
  `thought=true` part emitted alongside the final chunk** — closer
  to Gemini's expectation but synthesises a message Gemini itself
  would not emit.

Recommend **A**.

### Q3 — Error event mid-stream

Anthropic emits a typed `error` event mid-stream that the
`spec_anthropic.StreamDecoder` already maps to a `providers.ProviderError`
returned via `StreamSession.Next()`. Once that error reaches the
gateway, the canonical chunk channel is closed (the session is in
its terminal state). The transcoder must emit an ingress-shape
error frame and terminate.

- **A. OpenAI ingress** emits `data: {"error":{"message":..,"type":"upstream"}}\n\ndata: [DONE]\n\n`.
- **B. Anthropic ingress** emits `event: error\ndata: {"type":"error","error":{...}}\n\nevent: message_stop\ndata: {"type":"message_stop"}\n\n`.
- **C. Gemini ingress** emits a final NDJSON line with
  `error` populated and stops.

Recommend **A/B/C as listed**, all default-on, no flag.

## 6. Non-goals

- **Streaming embeddings** — Embeddings have no streaming wire, no
  ingress to translate into.
- **Bedrock streaming** — Same as E28-S6: bedrock streaming is wired
  to the wrong decoder and currently unreachable. T-BEDROCK-STREAM
  there blocks any bedrock cross-format streaming work; pick that up
  separately.
- **Mid-stream content rewrite for cross-format** — When a
  compliance hook returns Modify, today's pipeline writes the
  rewritten OpenAI delta through `openAIStreamDeltaPayload`. For
  cross-format ingress=Anthropic, the rewrite would need to be
  re-encoded as `content_block_delta`. Out of scope here; tracked
  under future T-HOOK-MODIFY-CROSS-FORMAT.
- **Multimodal stream parts (images, audio)** — Same as E28-S6 §3.

## 7. Migration

Pre-GA, no backward-compatibility concerns. The transcoder ships in
the same release that flips `StreamShapeCompatible` /
`writeCrossFormatStreamUnsupported`'s rejection rule to "no
transcoder registered for this pair". Existing same-format streams
keep their byte-for-byte passthrough behaviour.

## 8. Acceptance summary

- Every (ingress, target) pair listed in §3.3 has a registered
  transcoder with golden test fixtures.
- The `cross_format_stream_unsupported` 400 fires only for pairs not
  in §3.3 (i.e. bedrock and any other future format that lacks a
  transcoder).
- Streaming parity matches non-streaming parity (E28-S6 round-trip
  golden) for ingress shape: every event in the ingress wire is
  byte-equal, modulo LLM nondeterminism and provider-side request_id
  values, to the corresponding native-ingress call.
- Hook text extraction on cross-format streams remains a known gap
  documented under T-HOOK-MULTI-INGRESS; checkpoints continue to
  fire for OpenAI ingress (where text extraction already worked) and
  silently skip mid-stream rewrites for Anthropic / Gemini ingress
  until that follow-up lands.
- `tests/parity/cross_format_stream.py` is green for all entries in
  §3.3.

## 9. Code anchors (cheat-sheet for the implementer)

- `packages/ai-gateway/internal/execution/canonicalbridge/stream.go` — extend with
  `Bridge.NewStreamTranscoder` plus per-pair transcoder structs.
- `packages/ai-gateway/internal/execution/canonicalbridge/stream_*.go` (new) — one
  file per (ingress, target) pair from §3.3.
- `packages/ai-gateway/internal/execution/canonicalbridge/testdata/stream_*` —
  fixture pairs (`<pair>.upstream.sse` + `<pair>.ingress.sse`).
- `packages/ai-gateway/internal/handler/proxy.go::handleStream` —
  inject transcoder via `chunkSSEReader`.
- `packages/ai-gateway/internal/handler/proxy.go::chunkSSEReader` —
  add `transcoder StreamTranscoder` and `cross bool` fields; switch
  inside `Read`.
- `packages/ai-gateway/internal/execution/canonicalbridge/bridge.go::StreamShapeCompatible`
  — keep but downgrade to "is there a transcoder?" via a Bridge
  method; or inline the check in `proxy.go` and remove this helper.
- `packages/ai-gateway/internal/execution/canonicalbridge/matrix_test.go` — add
  `TestStreamRoutable` per T-S7-MATRIX-EXPAND.
- `tests/parity/` — clone the Apr/May 2026 parity script for
  cross-format coverage.

End of E28-S7.
