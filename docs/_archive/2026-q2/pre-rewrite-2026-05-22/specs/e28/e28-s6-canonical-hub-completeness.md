# E28 — S6 — Canonical hub completeness

## 0. Scope

This story closes the gaps between the **declared** canonical-hub routing
matrix (see requirements doc `docs/developers/specs/e28/e28-hub-canonical-routing.md`)
and the **implemented** translation pipeline in `packages/ai-gateway/internal/{canonicalbridge,wireformat,provbuiltins,providers,executor,handler}`.

The hub already exists and works for plain text-only chat requests. It is
**incomplete** for:

- Multi-turn tool-use (requests and responses, streaming and non-streaming).
- Multimodal parts (images, inline data, file refs).
- Cross-format embeddings (matrix advertises routable; codecs reject).
- Anthropic streaming usage accounting (prompt tokens lost).
- Gemini function-call responses (dropped in both directions).
- Provider-specific metadata (cache tokens, safety ratings, grounding, etc.).
- Bedrock streaming (wired to the wrong decoder).
- Runtime self-check between `ChatRoutable` and `IngressChatToCanonical`.

Each gap is mechanical to close once the contract is explicit. Section 2
fixes the contract; Section 3 enumerates the gaps with code anchors;
Section 4 lists the tasks with file lists, acceptance criteria, and — where
the right answer is non-obvious — code scaffolds the implementer must
follow. Section 5 locks down the canonical subset so no future codec has
to guess what belongs in hub JSON.

Upstream references (do not change without updating `wireformat/doc.go`):

- OpenAI Chat Completions request/response: <https://platform.openai.com/docs/api-reference/chat/create>
- OpenAI streaming chunks: <https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events/>
- Anthropic Messages request/response: <https://docs.anthropic.com/en/api/messages>
- Anthropic Messages streaming events: <https://docs.anthropic.com/en/api/messages-streaming>
- Gemini generateContent / streamGenerateContent: <https://ai.google.dev/api/generate-content>
- Gemini embedContent / batchEmbedContents: <https://ai.google.dev/api/embeddings>
- Bedrock InvokeModel / InvokeModelWithResponseStream: <https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModel.html>, <https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModelWithResponseStream.html>

## 1. Actors and story

- **AI Gateway** owns the full translation pipeline. Every other caller
  (agent, compliance-proxy, control-plane) sees hub-canonical shapes
  through the `/v1` and native ingress routes.
- **Client** sends on a single ingress format. Cross-format translation is
  the gateway's problem, not the client's.
- **Platform operator** wants provider-specific signals (cache hits,
  safety ratings, finish reason variants) retained or at least logged.

User story: *"As a gateway operator, I can point any ingress format at
any registered provider for chat completions (streaming or not), with
tool-use, multimodal, and provider-native metadata preserved end to end,
or, when a downgrade is unavoidable, rejected with a clear 4xx instead of
silently stripped."*

## 2. Target architecture

### 2.1 Canonical hub definition

The canonical hub is **OpenAI `chat.completions` 2024-10 JSON** plus a
small number of OpenAI-documented extensions:

- Request extensions: `tools`, `tool_choice`, `response_format`,
  `parallel_tool_calls`, `stream_options.include_usage`, `seed`,
  `user`, `logit_bias`, `metadata` (OpenAI-shape; see note below).
- Response extensions: `tool_calls`, `logprobs`, `usage.prompt_tokens_details.cached_tokens`,
  `usage.prompt_tokens_details.audio_tokens`, `usage.completion_tokens_details.reasoning_tokens`.

Out-of-subset provider fields that still need a home:

- Anthropic: `cache_control` on content parts, `metadata.user_id`,
  `service_tier`, `stop_sequence` (the actual matched sequence, not just
  "a sequence matched").
- Gemini: `safetySettings`, `safetyRatings`, `groundingMetadata`,
  `cachedContent`, `responseSchema`, `responseMimeType`,
  `generationConfig.thinkingConfig`, `promptFeedback`.
- Bedrock: `anthropic_version`, guardrail trace.

For these, the hub uses two mechanisms:

1. **`CallTarget.Extras`** — provider-specific knobs keyed by dotted
   namespace (`anthropic.version`, `gemini.safety.threshold`, …).
   Already in place for Anthropic and Bedrock; extend to Gemini and
   OpenAI headers. Clients cannot populate Extras; the resolver does.
2. **Canonical extension object `nexus.ext`** on both request and
   response — a dict passthrough that codecs may write to and read from.
   Top-level names match provider: `nexus.ext.anthropic.cache_control`,
   `nexus.ext.gemini.safety_ratings`, etc. Non-subset OpenAI-ingress
   bodies may carry `nexus.ext.*` and round-trip unchanged.

### 2.2 Six translation surfaces

There are exactly six translation functions per provider. This is the
contract — no other translation layer is allowed.

| # | Surface | Input | Output | Where |
|---|---------|-------|--------|-------|
| S1 | Native ingress request → canonical | native JSON | canonical OpenAI request JSON | `spec_X/hub_ingress.go` |
| S2 | Canonical request → provider wire | canonical OpenAI request JSON | provider-native request JSON | `SchemaCodec.EncodeRequest` |
| S3 | Provider wire response → canonical | provider-native response JSON | canonical OpenAI response JSON | `SchemaCodec.DecodeResponse` |
| S4 | Canonical response → native ingress | canonical OpenAI response JSON | native ingress JSON | `spec_X/hub_ingress.go` |
| S5 | Provider SSE → canonical chunk | SSE bytes | `providers.Chunk` (canonical Delta / ToolCallDeltas / Usage / Done, plus RawBytes for same-format passthrough) | `StreamDecoder.Open/Next` |
| S6 | Canonical chunk → native ingress SSE | `providers.Chunk` | ingress-shape SSE frame | new `StreamTranscoder` (does not exist yet — see Task T-STREAM-TRANSCODER) |

Today only S1–S5 exist. S6 is the missing piece that currently blocks
cross-format streaming; until it exists, `StreamShapeCompatible` must
continue to reject such combinations.

### 2.3 Endpoint coverage matrix

| Endpoint | Supported transforms |
|----------|----------------------|
| `chat_completions` | S1–S6 for openai / anthropic / gemini / vertex. Bedrock passthrough only (no ingress). MiniMax / GLM / DeepSeek / Azure-OpenAI share OpenAI identity codec. |
| `embeddings` | Either (a) same-format passthrough only, or (b) codec pair for OpenAI ↔ Gemini / Vertex (Anthropic has no public embeddings API today, Bedrock requires per-model bodies). Choose **option (a)** initially — see Task T-EMBED-1 — and add option (b) only when a customer asks. |
| `models` | Passthrough. Each adapter exposes its own `/models` via Transport. No translation. |
| `completions_legacy` | Passthrough. Deprecated upstream; no cross-format routing. |

### 2.4 Streaming transcoder policy

Until S6 is implemented, `canonicalbridge.StreamShapeCompatible` is the
gate and returns `true` only for:

- `ingress == target` (same format — native passthrough).
- `openAILike(ingress) && openAILike(target)` — OpenAI-compat family
  (openai/deepseek/glm/azure-openai) shares wire frames.

Any other combination MUST return a 400 with `cross_format_stream_unsupported`.
Implementers who add S6 also update `StreamShapeCompatible` to unlock the
corresponding pair and write an end-to-end test.

### 2.5 Non-subset field policy

When a codec encounters a request or response field that is neither in
the canonical subset nor mappable:

1. **Inside an OpenAI-identity passthrough**: leave it untouched. The
   target adapter is responsible for ignoring fields it doesn't
   understand.
2. **Inside a translation codec** (S1/S2/S3/S4): prefer to preserve via
   `nexus.ext.<provider>.*`. When that is not possible (for example,
   `tools` cannot be dropped silently because the client expects a
   tool-use response), the codec MUST return
   `*providers.ProviderError{Code: CodeInvalidRequest, Message: "nexus: field X unsupported on this route"}`.
3. **Always** log once at WARN via `slog` with sampled de-dup on
   `(ingress, target, field)`. Do not panic, do not fall through.

This is a hard rule. Silent drop is the source of most of the
current bugs.

### 2.6 Startup self-check

`canonicalbridge.New` panics on construction if any `(ingress, target)`
pair reachable per `ChatRoutable` lacks a working codec. The check uses
a fixed minimal canonical body per format. See Task T-SELF-CHECK.

## 3. Defect inventory (current gaps)

All file paths are relative to `packages/ai-gateway/internal`.

### 3.1 P0 — wrong behavior customers will notice

#### D-0001 Embeddings matrix promises what codecs cannot deliver

- **Location**: `canonicalbridge/bridge.go:60-72`, `spec_gemini/codec.go:18-20`, `spec_anthropic/codec.go:26-28`, `spec_bedrock/codec.go:30-34`.
- **Observed**: `EndpointRoutable(EndpointEmbeddings, openai, gemini)` returns `true`. Executor does not invoke the bridge for non-chat endpoints (`executor.go:100`), so the canonical OpenAI embedding body is handed to the Gemini adapter. `spec_gemini` codec returns `gemini: unsupported endpoint "embeddings"`, which `specAdapter.prepareBody` surfaces as 400 `CodeInvalidRequest`. The executor then treats `CodeInvalidRequest` as terminal and returns 400 to the client.
- **Expected**: Either the matrix rejects cross-format embeddings (initial behavior) or a real embedding codec exists. OpenAI `{model,input}` ≠ Gemini `{content:{parts:[{text}]}}`.
- **Fix**: Task T-EMBED-1.

#### D-0002 Anthropic streaming never reports prompt tokens

- **Location**: `spec_anthropic/stream.go:61-88`.
- **Observed**: Stream decoder only handles `content_block_delta / message_delta / message_stop`. Anthropic emits prompt tokens on `message_start.message.usage.input_tokens` — that event is never inspected. Resulting `Chunk.Usage.PromptTokens` is always `nil`, so `rec.PromptTokens = 0` in `handler/proxy.go:992`, and quota reconcile charges only output tokens.
- **Expected**: Prompt tokens populated at the start of the stream.
- **Fix**: Task T-ANT-STREAM-1.

#### D-0003 Anthropic streaming drops tool-use events entirely

- **Location**: `spec_anthropic/stream.go:61-88`.
- **Observed**: `content_block_start` (carries `tool_use.id`, `tool_use.name`, `tool_use.input` seed) and `content_block_delta{delta.type:"input_json_delta"}` (carries incremental JSON of tool arguments) are both ignored. `Chunk.ToolCallDeltas` stays empty. Downstream compliance hooks and clients that care about streamed tool calls see nothing.
- **Expected**: Partial tool calls emitted as `ToolCallDeltas{Index, ID, Name, Arguments}`; same shape the OpenAI decoder produces at `spec_openai/stream.go:85-95`.
- **Fix**: Task T-ANT-STREAM-2.

#### D-0004 Gemini function calls lost in both directions

- **Location**: `spec_gemini/codec.go:131-203`, `spec_gemini/hub_ingress.go:112-150`, `spec_gemini/stream.go:62-78`.
- **Observed**: `candidate.content.parts[].functionCall` → canonical `tool_calls` mapping does not exist. Reverse mapping (canonical `tool_calls` → Gemini `functionCall` part) does not exist. Streaming decoder ignores function-call parts.
- **Expected**: Full round-trip tool-use on Gemini, same fidelity as Anthropic codec has for non-streaming.
- **Fix**: Task T-GEM-TOOLS.

### 3.2 P1 — correctness, observability, compliance-visible

#### D-0101 Non-text Anthropic content parts silently dropped at hub ingress

- **Location**: `spec_anthropic/hub_ingress.go:117-129`, plus the same blind spot at `spec_anthropic/codec.go:109-133` for the reverse direction (`passthroughParts` forwards OpenAI `image_url` shape, which Anthropic rejects at the wire).
- **Observed**: `tool_use` / `tool_result` / `image` / `document` parts are either dropped (to canonical) or forwarded in a shape Anthropic doesn't accept (from canonical). Multimodal and mid-conversation tool-result turns break.
- **Fix**: Task T-ANT-MULTIMODAL, Task T-ANT-TOOLS.

#### D-0102 Anthropic cache usage lost

- **Location**: `spec_anthropic/codec.go:211-223` and `spec_anthropic/stream.go:67-83`.
- **Observed**: `usage.cache_read_input_tokens` and `usage.cache_creation_input_tokens` are not mapped to canonical. Operators cannot see prompt-cache hit rate.
- **Expected**: Map to canonical `usage.prompt_tokens_details.cached_tokens`; retain cache_creation as `nexus.ext.anthropic.cache_creation_input_tokens` so the cost engine can price it.
- **Fix**: Task T-USAGE-EXT.

#### D-0103 Gemini finish-reason enum underspecified

- **Location**: `spec_gemini/codec.go:205-215`, `spec_gemini/hub_ingress.go:152-166`.
- **Observed**: Only `STOP / MAX_TOKENS / SAFETY / RECITATION / OTHER` are mapped. `LANGUAGE / PROHIBITED_CONTENT / SPII / BLOCKLIST / IMAGE_SAFETY / MALFORMED_FUNCTION_CALL / TOOL_USE_ERROR` pass through as raw uppercase strings, breaking OpenAI clients that switch on the enum.
- **Fix**: Task T-GEM-FINISH.

#### D-0104 Gemini stream terminates on first finishReason

- **Location**: `spec_gemini/stream.go:73-78`.
- **Observed**: Any non-empty `finishReason` marks the session `done`, so trailing `usageMetadata`-only chunks that follow in some models are ignored. Usage ends up stale.
- **Expected**: Mark terminal only at stream EOF, or when a `finishReason` chunk has no pending trailer (model-family specific). The safe fix: continue scanning until the underlying reader EOFs, and surface `Done=true` on the last frame.
- **Fix**: Task T-GEM-STREAM.

#### D-0105 Response-side Anthropic envelope contains a synthetic field

- **Location**: `spec_anthropic/hub_ingress.go:196-198`.
- **Observed**: `created_at` is written to the Anthropic response envelope. The real Anthropic API does not return this field. Strict client JSON-schema validation treats the extra field as a warning.
- **Expected**: Drop `created_at`; if round-trip identifier is needed, record it in `nexus.ext.nexus.created_at`.
- **Fix**: Task T-ANT-ENVELOPE.

#### D-0106 Response-side Gemini envelope contains a synthetic field

- **Location**: `spec_gemini/hub_ingress.go:126-128`.
- **Observed**: `finishMessage: ""` is written unconditionally. Gemini API does not return this key.
- **Fix**: Task T-GEM-ENVELOPE.

#### D-0107 `tools` / `tool_choice` / `response_format` not translated from canonical

- **Location**: `spec_anthropic/codec.go:26-96`, `spec_gemini/codec.go:18-73`, `spec_bedrock/codec.go:30-81`.
- **Observed**: Canonical `tools` and `tool_choice` are silently dropped when the target is Anthropic / Gemini / Bedrock. Canonical `response_format: {type: "json_object"}` is dropped.
- **Expected**:
  - Anthropic: canonical `tools` → Anthropic `tools[] {name, description, input_schema}`; `tool_choice: "auto"|{type:"function",function:{name}}` → Anthropic `tool_choice: {type:"auto"|"tool", name?}`; `response_format:{type:"json_object"}` → prefix assistant prefill `{` as documented by Anthropic, or return `CodeInvalidRequest` when the subset does not fit.
  - Gemini: canonical `tools` → Gemini `tools[] {functionDeclarations[]}`; `tool_choice` → `toolConfig.functionCallingConfig.mode`; `response_format:{type:"json_object"}` → `generationConfig.responseMimeType:"application/json"`.
- **Fix**: Task T-ANT-TOOLS and Task T-GEM-TOOLS (bundles with D-0004).

### 3.3 P2 — structural, latent, or developer-only

#### D-0201 `ChatRoutable` is not verified against codec capability

- **Location**: `canonicalbridge/bridge.go:34-56`, `provbuiltins/builtins.go:29-60`.
- **Observed**: `ChatRoutable` is a boolean matrix that assumes every listed pair has a working codec; it can drift from reality.
- **Fix**: Task T-SELF-CHECK.

#### D-0202 Bedrock streaming wired to the wrong decoder

- **Location**: `spec_bedrock/spec.go:22-28`.
- **Observed**: `StreamDecoder: spec_anthropic.NewStreamDecoder(log)` — Bedrock `invoke-with-response-stream` returns AWS Event Stream framing (`application/vnd.amazon.eventstream`), not SSE. The Anthropic SSE parser cannot read event-stream frames.
- **Today**: Unreachable path (Bedrock ingress is not exposed per `ExtractIngressModel`; `StreamShapeCompatible(openai, bedrock)=false`). It is still wrong code.
- **Fix**: Task T-BEDROCK-STREAM.

#### D-0203 `chunkSSEReader` synthesises OpenAI envelope for non-canonical delta

- **Location**: `handler/proxy.go:1078-1094`.
- **Observed**: When `chunk.RawBytes` is empty, a skeleton OpenAI envelope is built around `chunk.Delta`. For non-OpenAI-like adapters, `RawBytes` today always contains the *native* frame, so the fallback never fires. When S6 lands this fallback becomes a liability.
- **Fix**: Task T-STREAM-TRANSCODER replaces `chunkSSEReader` with a format-specific transcoder.

#### D-0204 `forwardHeaderAllowList` has no per-provider extension

- **Location**: `providers/spec_adapter.go:37-42`.
- **Observed**: Only `accept / accept-encoding / user-agent / content-type` are forwarded. Client-supplied `anthropic-beta`, `OpenAI-Beta`, `x-goog-user-project` etc. are silently dropped.
- **Fix**: Task T-HEADER-ALLOWLIST.

## 4. Task plan

Work proceeds top-to-bottom. Each task is mergeable on its own but the
order minimises rebase churn (interfaces first, codec internals second,
tests third).

### T-CANON-SUBSET — Lock the canonical subset (doc + types)

- **Files**:
  - `packages/ai-gateway/internal/execution/canonicalbridge/canonical.go` (new).
  - Update `packages/ai-gateway/internal/execution/wireformat/doc.go`.
- **Scope**:
  - Create `canonicalbridge/canonical.go` holding a single `const CanonicalVersion = "openai.chat.completions.2024-10"` and two package-level documents (as Go doc comments): `CanonicalRequestSubset` and `CanonicalResponseSubset`. The subset text is Section 2.1 of this doc, flattened to `// -` bullets so the IDE hover is useful.
  - Add a small helper `func SubsetFields() (request, response []string)` that returns the flat field list, used by the self-check in T-SELF-CHECK and the extension-object scaffolding in T-EXT-OBJECT.
- **Acceptance**:
  - `go vet ./...` clean.
  - The doc string enumerates every field a codec is allowed to emit; the list is the single source of truth.

### T-EXT-OBJECT — `nexus.ext.<provider>.*` passthrough

- **Files**:
  - `packages/ai-gateway/internal/execution/canonicalbridge/ext.go` (new).
  - `packages/ai-gateway/internal/providers/types.go` (add helper if needed).
- **Scope**: A thin codec-side helper that reads / writes `nexus.ext.<provider>.<key>` from canonical JSON using `gjson` + `sjson`.
- **Scaffold** (implementers follow the shape; adapt param names as needed):

  ```go
  package canonicalbridge

  import (
      "github.com/tidwall/gjson"
      "github.com/tidwall/sjson"
  )

  // GetExt returns the JSON string at nexus.ext.<provider>.<key> in body,
  // or "" when absent. Raw value (unparsed) — callers re-parse as needed.
  func GetExt(body []byte, provider, key string) gjson.Result {
      return gjson.GetBytes(body, "nexus.ext."+provider+"."+key)
  }

  // SetExt writes value under nexus.ext.<provider>.<key> in body and
  // returns the new body. value may be any JSON-marshalable type.
  func SetExt(body []byte, provider, key string, value any) ([]byte, error) {
      return sjson.SetBytes(body, "nexus.ext."+provider+"."+key, value)
  }
  ```

- **Acceptance**: Unit test round-trips a canonical body through SetExt /
  GetExt for two providers without clobbering unrelated fields.

### T-EMBED-1 — Lock embeddings to same-format routing

- **Files**:
  - `packages/ai-gateway/internal/execution/canonicalbridge/bridge.go`.
  - `packages/ai-gateway/internal/execution/canonicalbridge/matrix_test.go`.
  - New end-to-end test `packages/ai-gateway/internal/handler/embeddings_crossformat_test.go`.
- **Scope**: Tighten `EndpointRoutable(EndpointEmbeddings, ...)` to
  same-format only. Remove the `ingress == providers.FormatOpenAI`
  branch in the embeddings case. Update tests accordingly.
- **Scaffold**:

  ```go
  func (b *Bridge) EndpointRoutable(ep providers.Endpoint, ingress, target providers.Format) bool {
      switch ep {
      case providers.EndpointChatCompletions:
          return b.ChatRoutable(ingress, target)
      case providers.EndpointEmbeddings:
          // Until a real embeddings codec exists (see T-EMBED-2), only
          // same-format routing is allowed. Cross-format would surface
          // as CodeInvalidRequest from the target codec and confuse the
          // operator about where the failure happened.
          return ingress == target
      case providers.EndpointModels, providers.EndpointCompletionsLegacy:
          return ingress == target || ingress == providers.FormatOpenAI
      default:
          return ingress == target
      }
  }
  ```

- **Acceptance**:
  - `TestEndpointRoutable_EmbeddingsMatrix` updated: every non-self pair is
    now `false`.
  - New handler test `POST /v1/embeddings` with a route that only has a
    Gemini target returns 400 `no_compatible_provider`, not 502.

### T-EMBED-2 — (optional, deferred) OpenAI ↔ Gemini embedding codec

- **Status**: Not scheduled. Pick up only when a customer requests it.
  Must include `embedContent` and `batchEmbedContents`, plus a matching
  update to T-EMBED-1 to re-open the matrix.

### T-ANT-STREAM-1 — Anthropic stream prompt tokens

- **Files**: `packages/ai-gateway/internal/providers/spec_anthropic/stream.go`, and the existing test `spec_anthropic/spec_test.go:119-170`.
- **Scope**: Parse `message_start.message.usage.input_tokens` and emit
  it on the first canonical `Chunk.Usage` so downstream accounting sees
  prompt tokens.
- **Scaffold** (add to `anthropicStreamSession.Next` before the existing
  `switch ev.Event` block):

  ```go
  case "message_start":
      if u := gjson.GetBytes(ev.Data, "message.usage"); u.Exists() {
          usage := &providers.Usage{}
          if v := u.Get("input_tokens"); v.Exists() {
              n := int(v.Int())
              usage.PromptTokens = &n
          }
          if v := u.Get("output_tokens"); v.Exists() {
              n := int(v.Int())
              usage.CompletionTokens = &n
          }
          if usage.PromptTokens != nil && usage.CompletionTokens != nil {
              total := *usage.PromptTokens + *usage.CompletionTokens
              usage.TotalTokens = &total
          }
          chunk.Usage = usage
      }
  ```

  The existing `message_delta` handler must keep writing its own
  `Chunk.Usage` (output tokens refresh). Downstream
  `handler.handleStream` already accumulates the latest non-nil usage.
- **Acceptance**: new table test using the `wireformat/official_wire_contract_test.go:27-41`
  Anthropic SSE fixture asserts
  `*chunk.Usage.PromptTokens == 25` on the first non-empty chunk.

### T-ANT-STREAM-2 — Anthropic tool-use streaming

- **Files**: `packages/ai-gateway/internal/providers/spec_anthropic/stream.go`, new test file `packages/ai-gateway/internal/providers/spec_anthropic/stream_tooluse_test.go`.
- **Scope**: Track active content-block slots per `index`; map
  `content_block_start{type:"tool_use",id,name}` and subsequent
  `content_block_delta{delta.type:"input_json_delta",partial_json}` into
  `Chunk.ToolCallDeltas`.
- **Scaffold**:

  ```go
  type anthropicStreamSession struct {
      scanner *specutil.SSEScanner
      log     *slog.Logger
      done    bool
      // tools maps content-block index → tool-call header so the
      // input_json_delta frames can be attached to the right call.
      tools map[int]struct{ id, name string }
  }

  // inside Next:
  case "content_block_start":
      idx := int(gjson.GetBytes(ev.Data, "index").Int())
      cb := gjson.GetBytes(ev.Data, "content_block")
      if cb.Get("type").String() == "tool_use" {
          if s.tools == nil {
              s.tools = make(map[int]struct{ id, name string })
          }
          s.tools[idx] = struct{ id, name string }{
              id:   cb.Get("id").String(),
              name: cb.Get("name").String(),
          }
          chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, providers.ToolCallDelta{
              Index: idx,
              ID:    cb.Get("id").String(),
              Name:  cb.Get("name").String(),
              // Arguments empty on start frame; accumulates on delta frames.
          })
      }
  case "content_block_delta":
      idx := int(gjson.GetBytes(ev.Data, "index").Int())
      delta := gjson.GetBytes(ev.Data, "delta")
      switch delta.Get("type").String() {
      case "text_delta":
          chunk.Delta = delta.Get("text").String()
      case "input_json_delta":
          if tc, ok := s.tools[idx]; ok {
              chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, providers.ToolCallDelta{
                  Index:     idx,
                  ID:        tc.id,
                  Name:      tc.name,
                  Arguments: delta.Get("partial_json").String(),
              })
          }
      }
  ```

  Note: the `Index` field on `ToolCallDeltas` is the Anthropic content-block
  index (stable through the stream). `ID` and `Name` repeat on every delta
  frame so OpenAI-compat clients can rebuild the tool call without
  remembering state.
- **Acceptance**:
  - New fixture in `stream_tooluse_test.go` derived from Anthropic's
    public streaming doc example (see references). Test asserts one
    `ToolCallDelta` with the start `id/name` and a concatenated
    `Arguments` equal to the final JSON after replay.
  - `handler/proxy.go` chunk forwarding keeps RawBytes verbatim
    (no change needed there).

### T-ANT-TOOLS — Anthropic non-streaming tool round-trip (canonical ↔ wire)

- **Files**: `packages/ai-gateway/internal/providers/spec_anthropic/codec.go`, `packages/ai-gateway/internal/providers/spec_anthropic/codec_test.go` (new).
- **Scope**:
  - `EncodeRequest`: translate canonical `tools[]` to Anthropic `tools[]`
    (`{name, description, input_schema}`); translate canonical
    `tool_choice` per Anthropic shape; preserve `cache_control` when
    present on any content block via `nexus.ext.anthropic.cache_control`.
  - `DecodeResponse`: already handles `tool_use`; add mapping for
    `tool_result` content parts back to canonical `tool` role messages
    when the response is a mid-turn continuation.
- **Acceptance**: golden test for a three-turn tool-use conversation
  (user asks → assistant tool_use → tool_result → assistant final)
  round-tripped through canonical with zero loss.

### T-ANT-MULTIMODAL — Anthropic image / document content parts

- **Files**: `packages/ai-gateway/internal/providers/spec_anthropic/{codec,hub_ingress}.go`.
- **Scope**: Translate OpenAI `{type:"image_url", image_url:{url,detail}}`
  parts to Anthropic `{type:"image", source:{type:"url",url}}` (preferred) or
  `{type:"image", source:{type:"base64",media_type,data}}` when the URL is a
  `data:` URL. Reject `detail:"high"` with `CodeInvalidRequest` if the
  downstream policy requires a specific detail level not expressible on
  Anthropic. Never fall through via `passthroughParts` for images.
- **Acceptance**: round-trip test with a URL image and a base64 image.

### T-GEM-MULTIMODAL — Gemini image (inlineData / fileData) round-trip

- **Files**: `packages/ai-gateway/internal/providers/spec_gemini/{codec,hub_ingress}.go`.
- **Scope**: Symmetric with T-ANT-MULTIMODAL. Canonical OpenAI
  `image_url` with a `data:` URL becomes a Gemini `inlineData` part
  (`{mimeType, data}`); canonical `image_url` with an https URL becomes
  a Gemini `fileData` part (`{mimeType, fileUri}`) where `mimeType` is
  derived from the URL extension (png/jpeg/webp/gif/heic/heif; falls
  back to `image/jpeg`). `image_url.detail:"high"` returns
  `*providers.ProviderError{Code: CodeInvalidRequest,
  Type:"nexus_field_unsupported"}`. Reverse direction: Gemini ingress
  `inlineData` parts canonicalise to a base64 `data:` URL, `fileData`
  parts canonicalise to the `fileUri` URL, both as canonical
  `image_url` shapes; messages with mixed text + image collapse to the
  parts-array form.
- **Acceptance**: round-trip test where a Gemini native request with
  one `inlineData` and one `fileData` part canonicalises and re-encodes
  with byte-identical mime types and data; extension-table table test
  pins the URL → mimeType map.

### T-GEM-TOOLS — Gemini function-call round-trip (all directions)

- **Files**:
  - `packages/ai-gateway/internal/providers/spec_gemini/codec.go`
  - `packages/ai-gateway/internal/providers/spec_gemini/hub_ingress.go`
  - `packages/ai-gateway/internal/providers/spec_gemini/stream.go`
  - tests alongside each.
- **Scope**:
  - S2 (`EncodeRequest`): canonical `tools[]` → Gemini `tools:[{functionDeclarations:[{name,description,parameters}]}]`; canonical `tool_choice` → Gemini `toolConfig.functionCallingConfig.{mode,allowedFunctionNames}`.
  - S3 (`DecodeResponse`) and S5 (stream): map `candidate.content.parts[].functionCall{name,args}` into canonical `choices[].message.tool_calls[] / delta.tool_calls[]` with `arguments` serialized to JSON string.
  - S4 (`OpenAIChatCompletionToGenerateContentResponse`): map canonical `tool_calls[]` into Gemini `functionCall` parts for the reverse direction (so native Gemini clients see native shape).
  - S1 (`GenerateContentRequestToOpenAIChatCompletion`): map Gemini `functionResponse` parts (tool results sent back to the model) into canonical `role:"tool", tool_call_id, content` messages.
- **Scaffold (S3 / S5 function-call extraction)**:

  ```go
  // Inside DecodeResponse, per candidate:
  var toolCalls []any
  parts.ForEach(func(_, p gjson.Result) bool {
      if fc := p.Get("functionCall"); fc.Exists() {
          // Gemini args is an arbitrary JSON object; OpenAI wants it
          // as a JSON-encoded string in arguments.
          args := fc.Get("args").Raw
          if args == "" {
              args = "{}"
          }
          toolCalls = append(toolCalls, map[string]any{
              "id":   fc.Get("id").String(), // Gemini >=1.5 sends id; older models omit — generate "call_"+random when empty
              "type": "function",
              "function": map[string]any{
                  "name":      fc.Get("name").String(),
                  "arguments": args,
              },
          })
      }
      return true
  })
  msg := map[string]any{"role": "assistant", "content": text}
  if len(toolCalls) > 0 {
      msg["tool_calls"] = toolCalls
  }
  ```

  For older Gemini responses without `functionCall.id`, generate a stable
  id via `"call_" + sha1(name+argsJSON)[:10]` so the OpenAI client's
  `tool_call_id` tracking works.
- **Acceptance**: golden test for a full `user → model(functionCall) →
  user(functionResponse) → model(text)` flow round-tripped through
  canonical. Streaming test with a `functionCall` part buffered across
  three frames. Official shape references: <https://ai.google.dev/api/generate-content#FunctionCall>.

### T-GEM-FINISH — Gemini finish-reason enum coverage

- **Files**: `spec_gemini/codec.go` (`mapFinishReason`), `spec_gemini/hub_ingress.go` (`mapOpenAIFinishToGemini`), new table test.
- **Scope**:

  ```go
  func mapFinishReason(r string) string {
      switch r {
      case "STOP":
          return "stop"
      case "MAX_TOKENS":
          return "length"
      case "SAFETY", "RECITATION", "LANGUAGE", "PROHIBITED_CONTENT",
           "SPII", "BLOCKLIST", "IMAGE_SAFETY":
          return "content_filter"
      case "TOOL_USE_ERROR", "MALFORMED_FUNCTION_CALL":
          return "tool_calls" // best-fit — tool call could not complete
      case "OTHER", "":
          return "stop"
      }
      return r
  }
  ```

  Reverse mapping (`mapOpenAIFinishToGemini`) must be symmetric only for
  `stop / length / content_filter / tool_calls`; other canonical values
  are not reachable from OpenAI today.
- **Acceptance**: table test with every Gemini enum listed above.

### T-GEM-STREAM — Gemini stream terminal handling

- **Files**: `spec_gemini/stream.go`.
- **Scope**: Do not set `s.done = true` on first `finishReason`. Instead,
  remember that `Done` should be reported on the *last* emitted chunk,
  which the SSE scanner signals via `io.EOF` from the underlying reader.
  Set `chunk.Done = true` on the frame that actually caused EOF, or on a
  synthesized trailer if none.
- **Acceptance**: test where one chunk carries `finishReason:"STOP"` plus
  text, and a subsequent chunk carries only `usageMetadata`. The
  consumer must see both chunks and only the final one must have
  `Done == true`.

### T-ANT-ENVELOPE / T-GEM-ENVELOPE — remove synthetic response fields

- **Files**: `spec_anthropic/hub_ingress.go`, `spec_gemini/hub_ingress.go`.
- **Scope**: Drop `created_at` from the Anthropic envelope (remove the
  assignment at `spec_anthropic/hub_ingress.go:196-198`). Drop
  `finishMessage: ""` from the Gemini candidate (`spec_gemini/hub_ingress.go:126-128`).
- **Acceptance**: JSON-schema diff against the canonical vendor samples
  in `wireformat/official_wire_contract_test.go` — no synthetic fields
  present.

### T-USAGE-EXT — Preserve Anthropic / Gemini usage detail fields

- **Files**:
  - `spec_anthropic/codec.go` and `spec_anthropic/stream.go`.
  - `spec_gemini/codec.go` and `spec_gemini/stream.go`.
  - New test fixtures.
- **Scope**:
  - Anthropic `usage.cache_read_input_tokens` → canonical
    `usage.prompt_tokens_details.cached_tokens`.
  - Anthropic `usage.cache_creation_input_tokens` → canonical
    `nexus.ext.anthropic.cache_creation_input_tokens` (cost engine reads
    from there).
  - Gemini `usageMetadata.cachedContentTokenCount` → canonical
    `usage.prompt_tokens_details.cached_tokens`.
  - Gemini `usageMetadata.thoughtsTokenCount` (when present) → canonical
    `usage.completion_tokens_details.reasoning_tokens`.
- **Acceptance**: golden test where a response body with cache hit
  returns canonical `usage.prompt_tokens_details.cached_tokens == N`.

### T-SELF-CHECK — Bridge self-verification at construction

- **Files**: `canonicalbridge/bridge.go`, `canonicalbridge/selfcheck_test.go` (new).
- **Scope**: Add a method `func (b *Bridge) SelfCheck() error` that
  enumerates every pair returned by `ChatRoutable`, takes a minimal
  canonical body per ingress from `canonicalbridge/canonical_fixtures.go`
  (new, mirrors `canonicalbridge/matrix_test.go:46-77`), runs
  `IngressChatToWire`, and reports the first failure. Call it from
  `cmd/ai-gateway/main.go` after `provbuiltins.Register` → `bridge.New`.
  On failure, log+panic — startup must abort, not degrade.
- **Scaffold**:

  ```go
  // SelfCheck walks every ChatRoutable pair and asserts the bridge can
  // actually produce wire bytes for that pair. Returns the first error
  // encountered; nil when the matrix is fully backed by codecs.
  func (b *Bridge) SelfCheck() error {
      formats := providers.AllFormats()
      for _, ingress := range formats {
          body, ok := canonicalFixtureFor(ingress)
          if !ok {
              continue // ingress has no declared hub mapper (e.g. bedrock)
          }
          for _, target := range formats {
              if !b.ChatRoutable(ingress, target) {
                  continue
              }
              ct := providers.CallTarget{Format: target, ProviderModelID: fixtureModelFor(target)}
              if _, err := b.IngressChatToWire(ingress, target, body, ct); err != nil {
                  return fmt.Errorf("canonicalbridge: %s→%s unusable: %w", ingress, target, err)
              }
          }
      }
      return nil
  }
  ```

- **Acceptance**: deleting any codec body (simulated in test) causes
  SelfCheck to return a non-nil error and the unit test to fail.

### T-HEADER-ALLOWLIST — Per-provider forward-header extension

- **Files**: `providers/spec_adapter.go`, each `spec_X/spec.go`.
- **Scope**: Add a `PerFormatForwardHeaders map[string]struct{}` field
  on `AdapterSpec`. `specAdapter.forwardHeaders` unions the base
  allowlist with this set before forwarding. Populate:
  - OpenAI: `openai-beta`, `openai-organization`, `openai-project`.
  - Anthropic: `anthropic-beta`, `anthropic-version` (server may still
    override).
  - Gemini: `x-goog-user-project`.
  - Bedrock / Vertex: none (headers are signed).
- **Acceptance**: handler integration test that supplies `anthropic-beta:
  prompt-caching-2024-07-31` header on `/v1/messages` and verifies the
  upstream `httptest.Server` receives it.

### T-STREAM-TRANSCODER — (design only in this story) S6 transcoder interface

- **Status**: Not implemented in this story. Add the interface so future
  work has a stable shape.
- **Files**: `canonicalbridge/stream.go` (new).
- **Scope**: Declare:

  ```go
  // StreamTranscoder converts a canonical provider chunk stream into the
  // wire SSE frames an ingress client expects. Implementations live per
  // (ingress, target) pair and are registered in the bridge.
  type StreamTranscoder interface {
      // Write consumes one canonical chunk and returns the bytes to emit
      // on the ingress SSE connection. When the chunk is terminal, the
      // transcoder appends the ingress-specific end-of-stream marker
      // (OpenAI "data: [DONE]\n\n", Anthropic "event: message_stop...").
      Write(ctx context.Context, chunk providers.Chunk) ([]byte, error)
  }
  ```

- **Acceptance**: interface declared; doc comment explains the
  invariants. No implementations required yet — `StreamShapeCompatible`
  stays conservative.

### T-BEDROCK-STREAM — Strip the broken Bedrock stream decoder

- **Files**: `spec_bedrock/spec.go`, new `spec_bedrock/stream.go`.
- **Scope**: Replace `StreamDecoder: spec_anthropic.NewStreamDecoder(log)`
  with a decoder that returns `CodeEndpointUnsupported` on `Open`. A
  correct event-stream decoder is deferred (Bedrock streaming is
  currently unreachable; no customer impact).
- **Acceptance**: unit test verifies `Open` returns an error with
  `providers.CodeEndpointUnsupported`.

### T-ROUND-TRIP-GOLDEN — Golden round-trip test matrix

- **Files**: `canonicalbridge/roundtrip_golden_test.go` (new), fixtures
  under `canonicalbridge/testdata/`.
- **Scope**: For each `(ingress, target)` pair where both `S1` and `S2`
  exist (and are not `bedrock`), run:
  1. native ingress body → canonical (S1)
  2. canonical → provider wire (S2)
  3. assert that semantic fields survive: model id, messages (role +
     text per turn), system prompt, temperature, top_p, top_k, max
     tokens, stop sequences, tool list (names only), response_format
     presence.
  The test is data-driven — each case loads a JSON fixture pair
  (`anthropic_chat_tool.native.json` + `anthropic_chat_tool.expected_openai.json`)
  from `testdata/`. Fixtures are derived from vendor docs; no
  synthetic payloads.
- **Acceptance**: the matrix fails loudly if any codec loses a field in
  the canonical subset; passes when the subset is preserved.

### T-DOC-CROSS-REFS — Update architecture + requirements

- **Files**:
  - `docs/users/product/architecture.md` — extend the section on "provider
    translation" with Section 2.1 / 2.2 of this story.
  - `docs/developers/specs/e28/e28-hub-canonical-routing.md` — add a table
    referencing S1–S6 and linking to the tasks above.
- **Acceptance**: reviewer can navigate architecture.md → this SDD →
  task list without gaps.

## 5. Canonical subset specification (locked contract)

### 5.1 Request required fields

- `model` (string, non-empty).
- `messages` (array, non-empty) — each element has `role` and `content`.
  `role ∈ {"system","user","assistant","tool"}`.
  `content` is either a string or an array of content parts.

### 5.2 Request allowed optional fields

- `temperature`, `top_p`, `top_k`, `max_tokens`.
- `stop` (string | string[]).
- `stream` (bool), `stream_options.include_usage` (bool).
- `tools[]` (OpenAI shape — `{type:"function", function:{name, description, parameters}}`).
- `tool_choice` (`"none" | "auto" | "required" | {type:"function", function:{name}}`).
- `response_format` (`{type:"json_object"} | {type:"json_schema", json_schema:...}`).
- `parallel_tool_calls` (bool).
- `seed`, `user`, `logit_bias`.
- `metadata` (OpenAI shape: flat `map[string]string`).
- `nexus.ext.<provider>.*` — passthrough namespace.

### 5.3 Request content part shapes

- `{"type":"text","text":"..."}`.
- `{"type":"image_url","image_url":{"url":"...","detail":"auto|low|high"}}`.
- `{"type":"tool_result","tool_call_id":"...","content":"..."}` (only on `role:"tool"` messages).

Any other part type in a translation codec MUST either produce a valid
native part or fail with `CodeInvalidRequest`.

### 5.4 Response required fields

- `id`, `object:"chat.completion"`, `created`, `model`.
- `choices[].index`, `choices[].message`, `choices[].finish_reason`.
- `message.role:"assistant"` and `message.content` (string or null when
  only `tool_calls`).
- `finish_reason ∈ {"stop","length","tool_calls","content_filter","function_call"}`.

### 5.5 Response allowed optional fields

- `message.tool_calls[]` (`{id,type:"function",function:{name,arguments}}`).
- `usage.prompt_tokens`, `usage.completion_tokens`, `usage.total_tokens`.
- `usage.prompt_tokens_details.cached_tokens`.
- `usage.prompt_tokens_details.audio_tokens`.
- `usage.completion_tokens_details.reasoning_tokens`.
- `logprobs`.
- `system_fingerprint`, `service_tier`.
- `nexus.ext.<provider>.*` — passthrough namespace.

### 5.6 Stream chunk shape

- `id`, `object:"chat.completion.chunk"`, `created`, `model`.
- `choices[].index`, `choices[].delta`, `choices[].finish_reason | null`.
- `delta` carries incremental `role` (first chunk only), `content`, or
  `tool_calls[]` where each element has `index`, `id` (on first frame
  of that call), `function.name` (on first frame), `function.arguments`
  (incremental JSON fragment).
- Trailing `usage` chunk when `stream_options.include_usage == true`,
  followed by `data: [DONE]\n\n`.

## 6. Test fixtures

Fixtures derived from vendor documentation — not synthesized. Every
fixture file in `testdata/` MUST include at the top a comment identifying
the vendor doc page and revision date.

### 6.1 Already present

- `wireformat/official_wire_contract_test.go:21-47` — OpenAI chat SSE,
  Anthropic messages SSE, Gemini streamGenerateContent SSE. Keep.

### 6.2 New fixtures to add

| File | Derived from | Used by |
|------|--------------|---------|
| `canonicalbridge/testdata/openai_chat_tools.request.json` | OpenAI Chat API reference example | T-ANT-TOOLS, T-GEM-TOOLS, T-ROUND-TRIP-GOLDEN |
| `canonicalbridge/testdata/anthropic_chat_tooluse.native.json` | Anthropic tool-use docs § "Tool use with Claude" | T-ANT-TOOLS |
| `canonicalbridge/testdata/gemini_chat_functioncall.native.json` | Gemini function-calling docs § "Multi-turn function calls" | T-GEM-TOOLS |
| `spec_anthropic/testdata/tooluse_stream.sse` | Anthropic streaming docs § "Tool use streaming" | T-ANT-STREAM-2 |
| `spec_gemini/testdata/functioncall_stream.sse` | Gemini docs § "Function calling (stream)" | T-GEM-TOOLS (stream) |
| `spec_anthropic/testdata/prompt_cache_response.json` | Anthropic prompt caching docs § "Checking cache usage" | T-USAGE-EXT |
| `spec_gemini/testdata/context_cache_response.json` | Gemini context caching docs § "Usage metadata" | T-USAGE-EXT |

## 7. Verification checklist

Mandatory before marking any task in Section 4 complete:

1. `cd packages/ai-gateway && go test -race -count=1 ./...` — green.
2. `cd packages/ai-gateway && go vet ./...` — clean.
3. Golden round-trip matrix (T-ROUND-TRIP-GOLDEN) covers every changed
   codec.
4. No synthetic fields (see D-0105, D-0106) introduced.
5. Any new field read / write uses `canonicalbridge.GetExt` /
   `canonicalbridge.SetExt` when non-subset; no ad-hoc `nexus.ext.*`
   key building elsewhere.
6. `canonicalbridge.Bridge.SelfCheck()` returns nil at startup (CI
   smoke test calls it).
7. Logs include at most one WARN per `(ingress, target, field)` tuple
   per process (sampled de-dup via `sync.Map`).

## 8. Out of scope

- True cross-format streaming (S6 transcoder implementations).
- Bedrock ingress exposure.
- Bedrock event-stream decoder.
- Cross-format embeddings codec (T-EMBED-2 deferred).
- MiniMax `chatcompletion_pro` native ingress.
- Non-text modalities beyond images (audio input, video).

## 9. Rollout

Dev-phase project; no user install base (per CLAUDE.md). Each task lands
directly on `main` once its acceptance criteria pass. No feature flags,
no phased compatibility layers. `SelfCheck` panicking at startup on a
broken codec is the intended guardrail.
