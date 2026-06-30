# Provider adapter architecture

The AI Gateway speaks one internal request/response shape — OpenAI chat-completions — and every provider that does not speak it natively gets an **adapter** that translates in both directions. This document defines the adapter contract, the dispatch path, and the eight binding rules (§3a) every adapter must follow.

## 1. The canonical model

The gateway translates every provider into a **canonical** OpenAI shape so the router, the response cache key, hook input, the audit envelope, and request lineage never branch on provider. Canonical is defined **per endpoint kind** (`typology.EndpointKind` — `chat`, `embeddings`, `image_generation`, `tts`, `stt`, `batch`, `models`, …):

- **Chat** is the richest canonical — OpenAI chat-completions. `canonicalbridge.Bridge` converts a non-OpenAI ingress body into it (`IngressChatToCanonical`), into a target wire (`IngressChatToWire`), and back to the caller's shape on the response side (`ResponseCanonicalToIngress`, `ResponseAcrossFormats`). Anthropic, Gemini, Vertex, Bedrock, and Cohere each implement the chat canonical↔wire mapping.
- **Embeddings** has a parallel canonical — the OpenAI embeddings shape (`input`, `model`, `dimensions`, `encoding_format`) — with its own `IngressEmbeddingsToCanonical` / `IngressEmbeddingsToWire`. Gemini, Vertex, Bedrock, Cohere, and Voyage translate to it; each adapter's `embed_canonical.go` carries the mapping.
- The remaining OpenAI-shaped kinds (responses-API, audio speech / transcriptions, image generation, batches, the older `/v1/completions` text endpoint, model listing) flow as OpenAI shape. No non-OpenAI provider translates them, so cross-format routing for those kinds stays OpenAI-only.

Dispatch is keyed by two values:

- **`Format`** (`packages/ai-gateway/internal/providers/core`) — the adapter family (one Format per adapter).
- **`typology.WireShape`** (`packages/shared/transport/typology`) — which native wire a given call targets (`WireShapeOpenAIChat`, `WireShapeAnthropicMessages`, `WireShapeBedrockEmbeddings`, …). The bridge resolves the target wire per endpoint kind via `chatWireShapeForFormat` / `embeddingsWireShapeForFormat`.

The `(Format, WireShape)` projection is described in [endpoint-typology-architecture.md](../../cross-cutting/foundation/endpoint-typology-architecture.md).

## 2. The AdapterSpec contract

Every adapter under `packages/ai-gateway/internal/providers/specs/<name>/` returns an `AdapterSpec` (`packages/ai-gateway/internal/providers/core/spec.go`); the generic `specAdapter` (`packages/ai-gateway/internal/providers/dispatch/spec_adapter.go`) composes it into a runtime `Adapter`. The spec carries:

| Field | Role |
|---|---|
| `Format` | The adapter family this spec implements. |
| `Transport` | `BuildURL` / `ApplyAuth` / `Do` / `Probe` — endpoint, auth, HTTP execution, health probe. |
| `SchemaCodec` | `EncodeRequest` (canonical→wire) / `DecodeResponse` (wire→canonical). |
| `StreamDecoder` | `Open` — wraps the upstream SSE body as a `StreamSession`. |
| `ErrorNormalizer` | `Normalize` — maps an upstream error response to a canonical `ProviderError`. |
| `PassthroughRewrite` | Optional `func(payload map[string]any, modelID string) []string` — per-model rewrites on the passthrough path. |
| `RequestShapes` | The `typology.WireShape` values this adapter accepts. |

The codec-facing interfaces take `shape typology.WireShape` as the per-call dispatch parameter:

```go
type Transport interface {
    BuildURL(target CallTarget, shape typology.WireShape, stream bool) (string, error)
    ApplyAuth(r *http.Request, target CallTarget) error
    Do(ctx context.Context, r *http.Request, target CallTarget) (*http.Response, error)
    Probe(ctx context.Context, target CallTarget) (*ProbeResult, error)
}

type SchemaCodec interface {
    EncodeRequest(shape typology.WireShape, canonicalBody []byte, target CallTarget) (EncodeResult, error)
    DecodeResponse(shape typology.WireShape, nativeBody []byte, contentType string, reqCtx DecodeContext) (DecodeResult, error)
}

// DecodeContext carries the originating request (resolved target + the wire
// request body that was sent upstream) so a response codec can validate the
// response against the request it answered.
type DecodeContext struct {
    Target      CallTarget
    RequestBody []byte
}

type StreamDecoder interface {
    Open(r io.ReadCloser, shape typology.WireShape) (StreamSession, error)
}

type ErrorNormalizer interface {
    Normalize(status int, headers http.Header, body []byte) *ProviderError
}
```

The `shape` parameter tells the codec which of its native wire shapes the call targets — the OpenAI codec dispatches `WireShapeOpenAIChat` to chat-completions encoding, `WireShapeOpenAIResponses` to responses-API encoding, `WireShapeOpenAIEmbeddings` to embeddings encoding. A codec rejects shapes it does not implement.

`DecodeResponse` additionally receives a `DecodeContext` (the resolved target + the wire request body that was sent upstream) so a response codec can validate the response against the request it answered — every batch embedding codec asserts the provider returned exactly one vector per request input (a count mismatch fails the decode → 502 rather than serving position-misaligned vectors), and the Gemini embedding codec estimates prompt tokens from the request text when the wire response carries no usage. A zero `DecodeContext` (post-hoc decodes — cache replay, estimate compare, audit) disables those request-relative checks (fail-open). `Transport.Do` likewise receives the `CallTarget`: transports that must sign after the body is finalized (AWS SigV4 in the Bedrock transport) read their credentials straight from `target.Extras` instead of smuggling them through internal request headers.

## 3. The dispatch path

`specAdapter.Execute` runs `PrepareBody`, which chooses between two paths:

- **Passthrough.** When the caller's body is already in the adapter's `Format` (or both sides are OpenAI-family), `PassthroughRewrite` applies any in-place model rewrites and the body is forwarded. `stripNexusNamespace` deletes the `nexus` key from the body before it reaches the upstream, so extension metadata never leaks to the provider.
- **Codec.** Otherwise `SchemaCodec.EncodeRequest` translates the canonical body into the target wire.

`canonicalbridge.Bridge` (`packages/ai-gateway/internal/execution/canonicalbridge`) holds the per-`Format` codecs and exposes `IngressChatToCanonical`; its `chatWireShapeForFormat` / `embeddingsWireShapeForFormat` helpers resolve the native `WireShape` for a `Format`.

On an upstream error response the adapter's `ErrorNormalizer.Normalize` produces a canonical `ProviderError`, which the ingress layer reshapes to the caller's format (Rule 8).

### Ingress shape preservation (round-trip)

The caller's wire shape is preserved end-to-end: whatever ingress a client calls — `/v1/chat/completions`, `/v1/messages`, gemini `:generateContent`, `/v1/responses`, `/v1/embeddings`, plus the Azure and GLM native ingresses — receives a response in that same shape. The upstream target wire is an internal concern resolved at the call site, not the caller's:

- **Request.** The ingress body is canonicalized once (`IngressChatToCanonical` for chat-kind, `IngressEmbeddingsToCanonical` for embeddings), then `TargetExecutor` sets the call-time `WireShape` from the *target* format — `ChatWireShapeForTarget` for chat-kind, `EmbeddingsWireShapeForTarget` for embeddings — so `Transport.BuildURL` and `SchemaCodec.EncodeRequest` target the correct wire for the primary target and every failover target. The per-request `Ingress.WireShape` is not mutated to the target shape; the `/v1/responses` → chat-completions downgrade is the one exception, and it is **capability-driven** — applied only when the resolved target does *not* serve the Responses API, in which case Responses canonicalizes to chat before dispatch (see *Responses egress: two signals*, below).
  - **Embeddings endpoint selection (single vs batch).** A codec may serve two upstream endpoints from one `WireShape`. Gemini's `WireShapeGeminiEmbedContent` covers both `:embedContent` (single string `input`) and `:batchEmbedContents` (array `input`); the choice is encoded only by the embeddings codec, which inspects the canonical `input` cardinality and returns an `EncodeResult.URLOverride` of `:embedContent` or `:batchEmbedContents`. Because embeddings always skip the gateway cache, the cross-format request is translated by `IngressEmbeddingsToWire`, which now **surfaces that override** alongside the wire body; `TargetExecutor` threads it into `Adapter.ExecuteWithBody`, where `applyURLOverride` swaps the action suffix on the `Transport.BuildURL` result. Dropping the override sends the batch body (`{"requests":[…]}`) to the single-embed URL and Gemini rejects it with `Unknown name "requests": Cannot find field` (regression guard: `TestIngressEmbeddingsToWire_GeminiEndpointSelection`, `TestExecute_EmbeddingsBridgeURLOverride_ReachesAdapter`). No Gemini-native embeddings *ingress* exists, so an embeddings request to a Gemini target is always cross-format and always flows through this path.
- **Response.** The upstream wire body is decoded to canonical, then reshaped back to the caller's format with `ResponseCanonicalToIngress` (chat) / `ResponseCanonicalToIngressEmbeddings` (embeddings), keyed on the ingress read from the request context (not the mutable per-request copy). The reshape fires when the ingress format differs from the target and is an identity no-op for same-format native routes.

The cross-format decision is driven by `typology.KindFromWireShape` (chat / embeddings) plus the per-provider Responses capability (see *Responses egress: two signals*, below) rather than a hardcoded ingress list or a Format-level guess, so a new chat or embeddings ingress is covered without changing the dispatch gates.

### Responses egress: two signals (capability + content)

The `/v1/responses` ingress is not a codec on top of chat — it is a co-equal OpenAI standard with strictly greater expressive power (typed `output[]` items, reasoning items, built-in tools, audio streaming, stateful `previous_response_id`). Routing it correctly uses **two independent signals**, never a single Format guess. The **request side** decides what wire to send upstream from a per-provider capability; the **response side** decides how to decode/encode from the actual bytes that came back. The two are deliberately separate: the request-side capability can be wrong (a provider may mis-declare or change behaviour), and the content signal is what makes the egress bulletproof regardless.

**Request side — capability-driven.** Whether a target receives a Responses-shape body or the downgraded chat-completions body is the per-provider capability `servesResponsesAPI`, resolved by `canonicalbridge.Bridge.ServesResponses(target, override)`:

```
/v1/responses ingress, resolved target T:
  ServesResponses(T)?
    yes → send Responses-shape body to T's /v1/responses   (built-in tools preserved)
    no  → responses → canonical(chat) → T's native chat wire (the downgrade)
```

Resolution order: the per-provider override (`Provider.serves_responses_api`, a nullable Boolean column) is **downgrade-only** — it can turn the capability *off* for a chat-only OpenAI-compatible endpoint, but cannot claim a capability the adapter lacks; with no override (the common case) the default comes from the adapter's `RequestShapes` (`RequestShapes ⊇ WireShapeOpenAIResponses`; today only `FormatOpenAI`). A lockstep test pins the Format default against the OpenAI adapter's declared `RequestShapes`. The capability is resolved per-target from the hydrated routing snapshot inside the failover loop — never a per-request DB read — and rides the already-threaded `CallTarget`. The request-side wire decision consults it at the same value everywhere: the executor's `nativeResponses` decision, `stage_routing.go`, `stage_cache_body.go`, and `bridge.IngressChatToWire`. This keeps real OpenAI / Azure working out of the box (default true) while letting a mock or chat-only endpoint opt out, so the gateway never POSTs a Responses body to an endpoint that would 404 it.

**Response side — content-driven (authoritative).** The decode/encode decision is driven by the **actual upstream bytes**, not by Format and not by the request-side capability. `specs/openai/responses/classify.go` performs exactly one classification:

```
Non-stream (ClassifyNonStreamBody — top-level "object"):
  "response"        → verbatim passthrough              (no re-encode; built-in tools preserved)
  "chat.completion" → canonical → EncodeResponsesResponse
  else              → fail closed (502 — never verbatim)

Stream (ClassifyFirstSSEFrame — first decoded SSE frame):
  event: response.* / data {"type":"response.*"}  → copier mode (verbatim frames)
  data {"object":"chat.completion.chunk"}          → chat mode → responsesStreamEncoder
  else                                             → fail closed (canonical chat lane — never verbatim)
```

The streaming classification happens **exactly once**, lazily on the first decoded frame at the raw-byte boundary (`specs/openai/stream/stream_responses_egress.go`), reusing the shared `SSEScanner` buffer — one per-stream hold, zero per-chunk allocation. The resolved wire shape is carried forward on the stream; the proxy layer does not sniff a second time. Trusting the bytes (not the declared Format) is what keeps a chat-shaped reply from being forwarded to a `/v1/responses` client: even a provider that mis-declares its capability cannot leak `chat.completion.chunk` frames, because the encoder follows the sniffed shape. Content authority is **wire-shape only**, never a compliance signal; the sniffed shape is cross-checked against the resolved capability.

**Raw-byte copier (non-enforced native path).** On the verbatim path the copier forwards each upstream SSE frame byte-for-byte (`Chunk.Verbatim` + `RawBytes`) so built-in-tool / audio events reach the client unparsed, while a usage tee decodes the canonical `Delta` / tool-call / reasoning / usage fields onto the **same** chunk so token and cost accounting survive. "Preserved" here means no re-encode through the canonical waist — not zero-cost.

**Precedence: enforcement > passthrough.** An enforcing response scope (redact / hard-block) forces canonical **buffer** mode, which rewrites the canonical body and therefore cannot also forward verbatim frames. Verbatim passthrough is allowed **only** on the non-enforced `/v1/responses` live lane (`allowVerbatim` in `stream_shape.go` requires `FormatOpenAIResponses` ingress AND no enforcing block/redact AND not the chat-ingress auto-upgrade). When an enforcing scope applies, built-in-tool / audio fidelity is **forfeited** — an accepted, documented blind spot. The enforcement and Model-A re-emit fallback is ingress-shape-aware: for `FormatOpenAIResponses` ingress it builds `NewResponsesStreamEncoder` (never `chat.completion.chunk`), so an enforced `/v1/responses` stream still emits `event: response.*` with a terminal `response.completed`. A second blind spot: built-in-tool content forwarded on the raw-byte path is absent from normalized text and compliance scanning.

**Fail closed.** An unclassifiable / empty / keep-alive-only first frame is **never** forwarded verbatim — the non-stream path returns 502 and the stream path falls back to the canonical chat (or enforced) lane. SSE comments and keep-alives are skipped via the shared `SSEScanner` before classification.

**Cache-HIT scope.** Content-peek runs only on the LIVE / cache-MISS lane. Cache-HIT replay chunks carry no `RawBytes`, so the origin-tag override (`StreamHitOrigin`) stays the authoritative wire-shape selector on a hit.

**Parity holds (§3a Rule 6).** Streaming and non-streaming `/v1/responses` egress stay at parity: both start `response.created`, end `response.completed`, carry a terminal event, and report the same `finish_reason`.

### Round-trip equivalence standard (the shape-conversion test of record)

A shape conversion is correct **iff it is lossless through the canonical hub in both directions**. The standard test — and the bar every shape-conversion change must clear — is the double round-trip:

```
shape A  →  canonical(OpenAI)  →  shape B  →  canonical(OpenAI)  →  shape A′
```

If `A′` is semantically equal to the original `A`, the whole `A ↔ canonical ↔ B` chain is proven: the A→canonical decode, the canonical→B encode, the B→canonical decode, AND the canonical→A encode all agree. Any field the chain drops, renames, or corrupts surfaces as a divergence between `A` and `A′` — you do not need a per-direction golden file for every field, the identity is self-checking.

Equivalence is asserted on the **canonical projection** of both ends — re-canonicalize `A` and `A′` and compare the content-bearing signature (ordered message `role` + `text`, tool calls, etc.), not raw bytes. Field ordering and protocol-default backfill (§4, e.g. `max_tokens`) and the per-hop model-alias rewrite are expected to differ and are not failures.

This is implemented as the table-driven `TestShapeRoundTripIdentity` in `canonicalbridge`, run over every routable `(A, B)` chat pair. It is the request-side companion to the §3 response reshape and the Rule 6 streaming parity. **A new ingress or target adapter is not "done" until it passes the double round-trip against every existing shape** — add the new format to the standard's shape list in the same PR.

## 3a. The eight binding rules

These rules are binding. Any change under `packages/ai-gateway/internal/providers/specs/<name>/` (codec, stream session, error normalizer, hub ingress) must conform before shipping. Run `/adapter-conformance-check` to audit an adapter against them.

### Rule 1 — canonical is the OpenAI shape

All internal flow — router input, cache key, hook input, audit envelope, request lineage — sees the canonical form, which is OpenAI's shape for the endpoint kind (§1). The chat canonical is OpenAI chat-completions:

```
model · messages[] · max_tokens / max_completion_tokens · temperature · top_p · top_k ·
stream · stop · response_format · tools[] · tool_choice · parallel_tool_calls ·
metadata · stream_options
```

The embeddings canonical is the OpenAI embeddings shape (`input` · `model` · `dimensions` · `encoding_format`). New canonical fields require an architecture change — adapters do not add canonical fields unilaterally.

### Rule 2 — each non-OpenAI adapter owns its full bidirectional translation

`SchemaCodec.EncodeRequest` does canonical→wire; `SchemaCodec.DecodeResponse` does wire→canonical. The OpenAI side stays the identity codec (`packages/ai-gateway/internal/providers/specs/openai`) — it never carries "this came from Anthropic so do X" branches. OpenAI shape is the bus; every other shape adapter (`specs/anthropic/codec`, `specs/gemini/codec`) wires itself onto it.

### Rule 3 — per-model wire quirks live in the adapter that talks to that wire

Parameter renames, mandatory clamping, and HTTP-400 deprecations live in the adapter that owns the wire — either in its codec prefix-lists or in its `PassthroughRewrite`. They do not live in cross-adapter switches inside `spec_adapter.go`.

| Quirk | Lives in |
|---|---|
| `claude-opus-4-7` deprecates `temperature` / `top_p` / `top_k` | `specs/anthropic/codec/codec.go` (`anthropicModelRejectsSamplingParams`) |
| `claude-4.x` rejects `temperature` + `top_p` together | `specs/anthropic/codec/codec.go` (`anthropicModelRejectsTempTopPTogether`) |
| gpt-5.x / o-series rename `max_tokens` → `max_completion_tokens` and strip sampling params | `specs/openai/rewrites` (`ApplyReasoningRewrites`, wired as the OpenAI `PassthroughRewrite`) |
| kimi-k2.5 / k2.6 require `temperature = 1` | `specs/compat/moonshot/rewrites.go` (`ApplyRewrites`, wired as the Moonshot `PassthroughRewrite`) |
| DeepSeek thinking models (`deepseek-reasoner*`, `deepseek-v4-pro*` — the two evidenced families) reject a forced `tool_choice` (`"required"` or a named function) with 400 "Thinking mode does not support this tool_choice" | `specs/compat/deepseek/rewrites.go` (`ApplyRewrites`, wired as the DeepSeek `PassthroughRewrite`; strips the field, silently downgrading the forcing to auto — default behavior still calls tools) |

When a new family ships a wire deprecation, add the rule to the adapter that owns its wire. Cross-adapter shared helpers create the wrong dependency direction.

### Rule 4 — extension fields ride in `nexus.ext.<provider>.<key>`

Fields with no clean OpenAI mapping (Anthropic's `thinking`, Gemini's `thinkingConfig`, Bedrock's `anthropic_version`) travel inside the `nexus.ext.<provider>.<key>` namespace on the canonical body. The helpers live in `packages/ai-gateway/internal/providers/canonicalext`:

- `Get` / `Set` — read and write a namespaced value.
- `ScanUnsupported` — walk top-level canonical keys against an adapter's supported set.
- `WarnOnce` — emit a one-shot WARN when an adapter observes an unsupported canonical field, so operators see drift between the canonical surface and the codec.

`stripNexusNamespace` removes the whole `nexus` key on the passthrough path before the body reaches the upstream.

### Rule 5 — cross-format callers canonicalize before the codec

A caller holding an ingress-format body (Anthropic `/v1/messages`, Gemini `:generateContent`) MUST canonicalize first:

```go
canonical, err := bridge.IngressChatToCanonical(ingress, body, target)
```

before invoking the codec. Skipping canonicalization makes the OpenAI identity codec forward the ingress body verbatim, and the upstream returns 400 (or parses partially and produces garbage). `EncodeRequest` accepts a canonical body (or a codec-empty passthrough); it does not accept arbitrary shapes.

### Rule 6 — streaming and non-streaming have parity

A codec rule that strips `temperature` from a non-streaming request must strip it from the streaming variant too — the upstream rejects both. Both paths construct their pre-dispatch body through the same `PrepareBody`, so parity normally falls out for free. For OpenAI-family streams, `applyStreamUsageOption` sets `stream_options.include_usage` so usage accounting survives the stream.

Parity also covers the response stream's terminal reason. The canonical `Chunk` carries a `FinishReason` field (`packages/ai-gateway/internal/providers/core/types.go`), so the reason a provider reports mid-stream — typically on the frame that carries the wire's stop token, not the closing `[DONE]`/terminal event — survives the canonical→wire re-encode instead of collapsing to a default `stop`. Each stream decoder stamps it from its wire's stop signal and each stream encoder re-emits it on the terminal frame, keeping the streamed `finish_reason` at parity with the non-streaming response's.

### Rule 7 — every prefix-list rule cites an observed 400

Each "model X rejects param Y" list is backed by an **observed** upstream 400, not speculation. The comment above each prefix-list switch records the upstream error message and the traffic trace it was seen on:

```go
// Observed via trace_id=<id> on claude-opus-4-7:
//   400 "<field> is not allowed for this model"
var anthropicModelRejectsSamplingParams = []string{ /* ... */ }
```

`anthropicModelRejectsSamplingParams` in `specs/anthropic/codec/codec.go` is the canonical example. Without evidence, a speculative rule silently flattens caller intent — it strips a parameter the model actually accepts and degrades behaviour with no surfaced reason.

### Rule 8 — error envelopes are reshaped to the caller's ingress format

A normalized `ProviderError` is never serialized in one hardcoded shape. `packages/ai-gateway/internal/ingress/envelope/error_envelope.go` exposes `EncodeErrorEnvelopeForIngress(ingress, upstream, pe)`, which selects the encoder for the caller's format — `encodeOpenAIErrorEnvelope`, `encodeAnthropicErrorEnvelope`, `encodeGeminiErrorEnvelope`, or `encodeResponsesAPIErrorEnvelope`. The streaming variant `encodeErrorEnvelopeForIngressForStream` wraps the JSON envelope in the SSE frame.

An Anthropic caller receives an Anthropic-shaped error even when the upstream error and its normalization were OpenAI-internal. Hand-building an OpenAI-shape error frame regardless of caller is the recurring gap this rule closes. The streaming framing details are in [sse-streaming-compliance-architecture.md](../../cross-cutting/safety/sse-streaming-compliance-architecture.md).

## 4. Provider model discovery

The AI Gateway exposes an internal endpoint for listing the upstream models a provider supports. This is a read-only admin-path probe used by the create-provider wizard, not part of the traffic dispatch path.

### Transport capability: `ListModels`

The OpenAI transport (`packages/ai-gateway/internal/providers/specs/openai/transport.go`) implements the optional `transportModelLister` interface:

```go
ListModels(ctx context.Context, target CallTarget) ([]string, error)
```

The method issues a `GET {BaseURL}/v1/models`, parses the standard OpenAI list envelope (`{"data":[{"id":"..."},...]}`), and returns the id strings. It shares the probe HTTP client (same timeout as `Probe`) so the call is bounded and does not block traffic.

The `specAdapter` wraps this capability in `ListModels(ctx, target) ([]string, bool, error)`. The boolean `supported` return is `true` when the underlying transport implements `transportModelLister` (OpenAI and all OpenAI-compatible adapters that reuse `openai.NewTransport()`), and `false` otherwise. Callers use the boolean to distinguish "adapter does not expose a model list" from an upstream error without branching on format names.

**OpenAI-family scope.** Only adapters that reuse the OpenAI transport implement `ListModels`. Adapters with their own transport (Anthropic, Gemini, Vertex, Bedrock, Cohere, etc.) do not — they speak a wire that either has no standard model-listing endpoint or uses a provider-specific shape that is not covered by this heuristic. Adding `ListModels` to a non-OpenAI transport requires a separate adapter PR.

### Internal endpoint: `POST /internal/provider-discover-models`

`packages/ai-gateway/internal/ingress/debug/provider_discover_models_endpoint.go` exposes this route alongside the existing `ProviderTestHandler`. It is `INTERNAL_SERVICE_TOKEN`-gated (same trust boundary as all `/internal/*` routes) and does not appear in the public or admin API.

**Request.** JSON body: `adapterType`, `baseUrl`, `apiKey` (all strings; `baseUrl` required).

**Response.** On success: `{"success": true, "models": [{"id": "<upstream-id>", "suggestedType": "<type>"}]}`. On upstream error: `{"success": false, "error": "..."}` at HTTP 200 so the caller (Control Plane BFF) can surface a readable message. On unsupported adapter: `{"success": false, "error": "...", "code": "discovery_unsupported"}` at HTTP 400.

### `SuggestModelType` heuristic

`debug.SuggestModelType(id string) string` maps a model id to one of four Nexus model types using lowercase substring matching:

| Substring match | Suggested type |
|---|---|
| `"embed"` | `embedding` |
| `"whisper"`, `"tts"`, `"audio"`, `"transcribe"` | `audio` |
| `"dall-e"`, `"image"` | `image` |
| (none of the above) | `chat` |

The heuristic is best-effort; the OpenAI `/v1/models` response carries no type field. Admins can override the suggested type per row in the create-provider wizard before saving.

### No traffic-path impact

Discovery is a pre-flight probe. It does not affect routing, caching, cost stamping, or the `traffic_event` pipeline. It does not change any persisted row. The smoke test therefore does not cover discovery (it covers the traffic path only). Unit tests for `SuggestModelType` and the `ProviderDiscoverModelsHandler` live in `packages/ai-gateway/internal/ingress/debug/`.

## 5. Request backstops & protocol defaults

A codec fills protocol-required fields the caller omitted, so an OpenAI-shaped request reaches a stricter upstream without a 400. The canonical example is Anthropic's `max_tokens`: Anthropic rejects a request that omits it, while OpenAI treats it as optional. When a caller forwards an OpenAI-shape body with neither `max_tokens` nor `max_completion_tokens`, the Anthropic codec synthesizes one from `AnthropicModelMaxOutput(model)` — the published per-model output ceiling, matched by model-name prefix, with a conservative floor for unrecognized models (`specs/anthropic/codec/codec.go`).

This is the adapter-fill pattern: the adapter that owns the wire supplies the protocol default rather than forcing the caller — or an admin config knob — to know each provider's required fields. Backstops live in the codec (Rule 3) and apply to streaming and non-streaming alike (Rule 6). Both the parameter-removal rewrites (temperature / top_p / top_k) and the synthesized `max_tokens` fill are recorded in the `rewrites` list, so the handler stamps `x-nexus-coerced` and the applied cap is observable in `traffic_event`.

## 6. Usage parsing & translation

Every codec's `DecodeResponse` returns canonical token accounting in `DecodeResult.Usage`. Extraction is centralized: `core.ExtractUsage(raw, wireFormat)` (`packages/ai-gateway/internal/providers/core/usage_extractor.go`) parses the upstream body through the shared Tier-1 normalizer for that wire format and returns the canonical `Usage`. Codecs delegate here instead of each carrying their own alias-chain logic.

Usage is normalized to the OpenAI convention so downstream cost, analytics, and audit never branch on provider:

- `PromptTokens` = uncached input + cache-read + cache-creation. The Anthropic normalizer folds its raw `input_tokens` (uncached only) and cache tokens into this total; callers must not subtract cache tokens again.
- `CompletionTokens` follows the OpenAI convention (for Gemini, candidates + thoughts).
- OpenAI-compatible wires share one normalizer that resolves the cached-token alias chain (DeepSeek `prompt_cache_hit_tokens`, Moonshot `prompt_cache_tokens`, Responses-API top-level `input_tokens` / `output_tokens`).

Cache-token detail also rides in `nexus.ext.<provider>.<key>` (Rule 4) — the Anthropic codec stores `cache_creation_input_tokens` there — and surfaces as `CacheReadTokens` / `CacheCreationTokens` on the normalized usage. The full normalize contract is in [normalization-architecture.md](normalization-architecture.md).

## 7. Prompt-cache handling

Anthropic `cache_control` is not a separate canonical field. On the passthrough path it rides inside the `messages` content; on the cache-prep path the gateway can inject cache markers before upstream dispatch. On the response side, the cache token counts the upstream reports are parsed by the usage path (§6) and preserved both on canonical usage (`CacheReadTokens` / `CacheCreationTokens`) and in `nexus.ext`. The marker mechanism, cache semantics, hit classification, and cost impact are owned by [prompt-cache-architecture.md](prompt-cache-architecture.md); an adapter's obligation is to preserve cache markers and report the cache tokens accurately.

Because cache classification depends on the usage parse, every ingress (chat, responses, messages, gemini) must exercise prompt-cache in the gateway smoke — a cross-ingress asymmetry, where one ingress reports cache tokens and another silently drops them, is the failure this guards against (§11).

## 8. Reuse across services

The provider adapter (codec) handles the gateway's outbound provider calls. The request/response **parsing** it relies on for usage and normalized text is not gateway-specific: it lives in `packages/shared/transport/normalize`, and the AI Gateway, Compliance Proxy, Agent, and Hub audit pipeline all import the same `normalize/core` + `normalize/codecs`. `core.ExtractUsage` is the gateway's entry into that shared layer.

The consequence: the same upstream response yields byte-identical canonical usage whether the gateway saw it on a forwarded call, the compliance proxy saw it on intercepted HTTPS, or the agent saw it on a client's outbound traffic. Adding a usage or text field for a provider means extending the shared normalizer once, not per service. The interception-side detail (Tier-1 traffic adapters, Tier-2 detectors) lives in the compliance-proxy architecture docs; the shared normalize contract is in [normalization-architecture.md](normalization-architecture.md).

## 9. Per-adapter walkthrough

`specs/anthropic/` is the full example of an own-wire adapter:

- `spec.go` — assembles the `AdapterSpec` (Format + Transport + SchemaCodec + StreamDecoder + ErrorNormalizer).
- `transport.go` — builds the Anthropic URL and applies the API-key + version headers.
- `codec/` — canonical↔Anthropic Messages translation, including the per-model prefix-lists and the `max_tokens` default fill.
- `stream/` — decodes the Anthropic SSE event stream into canonical chunks.
- `errors/` — maps Anthropic's `{"type":"error","error":{...}}` envelope to canonical `ProviderError` codes.
- `ingress/` — the Nexus `/v1/messages` ingress handler that turns an Anthropic-format request into canonical.

Adapters fall into three structural tiers:

| Tier | Shape | Members |
|---|---|---|
| Own wire + Nexus ingress | `codec/` `stream/` `errors/` `ingress/` subpackages | `anthropic`, `gemini` (and `openai`, the canonical/identity codec, with `codec/` `errors/` `responses/` `rewrites/` `stream/`) |
| Own wire, flat codec, no Nexus ingress | flat `codec.go` / `stream.go` / `errors.go` (+ `embed_*.go` for embeddings) | `bedrock`, `cohere`, `replicate`, `voyage` |
| Own wire, codec subpackage | `spec.go` + `transport.go` + a `codec/` subpackage (no stream/errors) | `glm` |
| Family reuse, own transport | `spec.go` + `transport.go`, borrowing the family codec | `azure` (OpenAI codec + `ApplyReasoningRewrites`), `vertex`, `minimax` |
| Family reuse, borrowed transport | `spec.go` only — reuses `openai.NewTransport()` + `openai.IdentityCodec()` | the OpenAI-compatible `specs/compat/*` adapters (`fireworks`, `groq`, `huggingface`, `mistral`, `perplexity`, `together`, `xai`); `moonshot` adds `rewrites.go` for its fixed-temperature quirk, `deepseek` for its thinking-mode `tool_choice` quirk |

A family-reuse adapter exists because the provider speaks an existing wire and only differs in endpoint and auth — it either supplies its own `Transport` (and borrows the family codec) or reuses the family transport outright, rather than writing a codec of its own.

## 10. Adding a new adapter

Use the `add-provider-adapter` skill for the full procedure. The wiring touch points:

1. Define the `AdapterSpec` (Format + Transport + SchemaCodec + StreamDecoder + ErrorNormalizer; add `PassthroughRewrite` only if the adapter is OpenAI-family and needs per-model rewrites).
2. Map the new `Format` in `chatWireShapeForFormat` / `embeddingsWireShapeForFormat`, or accept the OpenAI-family default for OpenAI-shape-compatible providers.
3. Add a `typology.WireShape` constant if the adapter speaks a non-OpenAI wire.
4. Add the ingress rule to `packages/shared/transport/typology/defaults.go` if a Nexus ingress path delivers requests in that wire shape.
5. Populate `RequestShapes` only with shapes backed by a captured 200 from the real upstream endpoint.

Run `/adapter-conformance-check` before completion to verify the adapter against Rules 1-8.

## 11. Testing an adapter

A new or changed adapter is validated at four levels:

- **Unit tests** — table-driven codec tests for `EncodeRequest` / `DecodeResponse`: canonical↔wire round-trips, each prefix-list rule, the backstop fill (§5), and usage extraction (§6). Each Go package holds ≥95% statement coverage.
- **Round-trip equivalence** — the shape-conversion test of record (§3): the double round-trip `A → canonical → B → canonical → A` must return a semantically-equal `A` for every routable shape pair (`TestShapeRoundTripIdentity`). A new ingress/target adapter adds itself to the standard's shape list in the same PR.
- **Conformance** — `/adapter-conformance-check` audits the codec against §3a Rules 1-8 (per-adapter logic that leaked into the dispatcher, missing canonicalize-before-encode, error envelopes that bypass the helper, prefix-lists without observed-400 evidence, missing `PassthroughRewrite` wiring).
- **Full-surface smoke** — `tests/scripts/smoke-gateway.py --all-ingress` exercises every model across all ingresses (chat / responses / messages / gemini), non-stream + SSE + a two-turn cache arm. It cross-checks each `traffic_event` row (cost, tokens, cache classification, normalized text) and diffs Prometheus counters. The prompt-cache arm is mandatory on every ingress (§7).
- **Usage / cost cross-check** — the smoke compares the parsed canonical usage against the persisted `traffic_event` row, catching a codec that parses usage but fails to stamp it.

Any change under `packages/ai-gateway/internal/providers/specs/<name>/` requires a gateway smoke run before the work is considered done.

## References

- `packages/ai-gateway/internal/providers/core/spec.go` — AdapterSpec + Transport / SchemaCodec / StreamDecoder / ErrorNormalizer interfaces
- `packages/ai-gateway/internal/providers/dispatch/spec_adapter.go` — generic specAdapter, PrepareBody, passthrough vs codec path; ListModels capability delegation
- `packages/ai-gateway/internal/execution/canonicalbridge/` — Bridge, IngressChatToCanonical, WireShape-for-Format helpers
- `packages/ai-gateway/internal/providers/canonicalext/` — `nexus.ext.<provider>.<key>` Get / Set / ScanUnsupported / WarnOnce
- `packages/ai-gateway/internal/providers/core/usage_extractor.go` — centralized canonical usage extraction
- `packages/ai-gateway/internal/ingress/envelope/error_envelope.go` — ingress-format error envelope encoders (unary + SSE)
- `packages/ai-gateway/internal/ingress/debug/provider_discover_models_endpoint.go` — ProviderDiscoverModelsHandler + SuggestModelType heuristic
- `packages/ai-gateway/internal/providers/specs/openai/transport.go` — ListModels transport capability (OpenAI and OpenAI-compatible transports)
- `packages/ai-gateway/internal/providers/specs/` — per-adapter implementations
- `packages/shared/transport/normalize/` — shared usage / text normalizer reused by gateway, compliance proxy, agent, and Hub
- `packages/shared/transport/typology/` — WireShape constants + ingress default rules
- `tests/scripts/smoke-gateway.py` — full-surface adapter smoke
