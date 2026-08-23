# Provider adapter architecture

The AI Gateway speaks one internal request/response shape — OpenAI chat-completions — and every provider that does not speak it natively gets an **adapter** that translates in both directions. This document defines the adapter contract, the dispatch path, and the twelve binding rules (§3a) every adapter must follow.

## 1. The canonical model

The gateway translates every provider into a **canonical** OpenAI shape so the router, the response cache key, hook input, the audit envelope, and request lineage never branch on provider. Canonical is defined **per endpoint kind** (`typology.EndpointKind` — `chat`, `embeddings`, `image_generation`, `tts`, `stt`, `batch`, `models`, …):

- **Chat** is the richest canonical — OpenAI chat-completions. `canonicalbridge.Bridge` converts a non-OpenAI ingress body into it (`IngressChatToCanonical`), into a target wire (`IngressChatToWire`), and back to the caller's shape on the response side (`ResponseCanonicalToIngress`, `ResponseAcrossFormats`). Anthropic, Gemini, Vertex, Bedrock, and Cohere each implement the chat canonical↔wire mapping.
- **Embeddings** has a parallel canonical — the OpenAI embeddings shape (`input`, `model`, `dimensions`, `encoding_format`) — with its own `IngressEmbeddingsToCanonical` / `IngressEmbeddingsToWire`. Gemini, Vertex, Bedrock, Cohere, and Voyage translate to it; each adapter's `embed_canonical.go` carries the mapping.
- The remaining OpenAI-shaped kinds (responses-API, audio speech / transcriptions, batches, the older `/v1/completions` text endpoint, model listing) flow as OpenAI shape with no cross-format translation — the resolved provider must speak the OpenAI shape. **Image generation is the exception (proposal D9 / P2.5, shipped):** it has a self-built OpenAI-images↔provider-wire codec (canonical = OpenAI images; `IngressImagesToCanonical` / `ImagesWireShapeForTarget` / `IngressImagesToWire` on the bridge, image encode/decode branches in the Gemini codec) so it reaches providers whose image model has no dedicated endpoint (Gemini Nano Banana, `:generateContent`). That codec is allow-list-only and strict-§3a like every other adapter, but it is **carved out of the round-trip-identity lossless standard** (§ *Round-trip equivalence standard*) because OpenAI `size` cannot round-trip through Gemini `aspectRatio`; correctness there is by explicit map / caller-visible coerce (`X-Nexus-Coerced`) / 400, not the double-round-trip test. Design contract: `docs/developers/specs/e88-s3-cross-shape-image.md`.

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

- **Native leg.** When the same-spec triage passes (three keys — Rule 3 below), dispatch runs its guards (nexus-strip; the non-object carve-out; the degenerate-target return) and hands the body to the codec's `RewriteNative`, which applies the same-spec differential: the resolved-model stamp (so a client-facing alias or a routing-changed model reaches the upstream as the vendor's real name, not the 404-inducing alias), the adapter's per-model wire rules, and the streaming back-fills. Wires that carry the model outside the body leave it untouched: Gemini (URL path, set by `Transport.BuildURL`), Bedrock (deleted from the body, encoded into the URL by its codec) — their `RewriteNative` is a stated verbatim return. The `nexus` key is deleted before the body reaches the upstream, so extension metadata never leaks to the provider. **Both legs strip it AFTER the codec** — the codec must still see `nexus.ext.<provider>.<key>` to consume it (Rule 4), while nothing internal may leave the box — at the one frame both funnel through (`executeWithBodyAndURL`). That is one dispatcher guarantee rather than twenty codec obligations, and the difference was measured: while it was each codec's job, Cohere's chat codec forwarded the namespace and answered HTTP 422 `unknown field: parameter 'nexus'`, and a registry-wide check found the same latent leak in **18 adapter/shape pairs** — OpenAI answers `400 Unrecognized request argument supplied: nexus`, while DeepSeek and Moonshot answer 200 and quietly keep the metadata. Two gates hold it: a structural one asserting exactly one method reaches `Transport.Do` and that it strips, and a registry-wide one driving all 13 wire shapes on both legs against a capturing server.
- **Codec leg.** Otherwise `SchemaCodec.EncodeRequest` translates the canonical body into the target wire.

On the native leg the differential is applied one of two ways. The surgical door probes only the paths of rules whose model gate matched (per-rule `gjson` reads — gjson has no genuinely fused multi-path scan, so unprobed rules cost zero body reads) and edits only what is due with `sjson` — a conformant body forwards as the same slice, zero copy. The decode door — one `map[string]any` round-trip, the dominant per-request allocation when taken — serves only the paths that must decode anyway: a streaming body needing `stream`/`stream_options.include_usage` back-fills, or a duplicated key ahead of a surgical edit (`sjson` edits the FIRST occurrence while parsers take last-wins, so dup-keyed bodies take the door whose semantics are exact). A message-level structural rule (deepseek's forced-`tool_choice` strip + `reasoning_content` back-fill) always takes the decode door, since its edits reshape the message array; a no-quirk model (`deepseek-chat`, `moonshot-v1-*`) matches no gate and forwards on the surgical path with zero body reads.

**Multimodal upstream paths.** The OpenAI transport's `PathForEndpoint` table (`specs/openai/transport.go`, reused by OpenAI-compat transports) maps the multimodal wire shapes to their upstream paths: `WireShapeOpenAIImages` → `/v1/images/generations`, `WireShapeOpenAIAudioSpeech` → `/v1/audio/speech`, `WireShapeOpenAIAudioTranscriptions` → `/v1/audio/transcriptions`. The images mapping covers the generations route only — `/v1/images/edits` and `/v1/images/variations` share the wire shape but are multipart routes whose upstream path cannot be derived from the shape alone; they ship together with the multipart handling work. Audio/TTS multimodal routes are native passthrough (no cross-shape translation): the resolved provider must speak the OpenAI shape. **Image generation is the one modality that also has a cross-format codec** (proposal D9 / P2.5, shipped): a caller's OpenAI-images request is translated to a non-OpenAI image wire (Gemini `:generateContent` + `responseModalities:["IMAGE"]`, target-side wire shape `WireShapeGeminiImagesGenerateContent`). The bridge carries the image-kind methods `IngressImagesToCanonical` (validation + identity — the closed-set checks: non-string `prompt` 400, `n` ∈ [1,4], top-level `nexus` 400), `ImagesWireShapeForTarget`, and `IngressImagesToWire` (validates first, then encodes, surfacing `EncodeResult.Rewrites` so coercion markers survive failover legs); the image response reshaper lives in the Gemini codec's decode (ingress == canonical for images, so egress is an identity skip). All three dispatch sites agree: the proxy prepare stage (`prepareUpstreamBody` images arm), the executor kind/translation arms, and the egress skip. The dispatcher propagates a structured `*ProviderError` from `DecodeResponse` verbatim (the image codec's content-policy 400 rides `CodeInvalidRequest`, keeping a provider-safety-blocked prompt out of retry/failover). Each image leg is independently demand-gated: v1 routes the image kind to exactly two targets — the literal `FormatOpenAI` (native passthrough — the body goes upstream byte-unchanged, but `ValidateImagesIngressGuards` still applies the `n` ceiling first, because that bound protects THIS gateway's spend rather than the upstream's wire, and enforcing it only where a body had to be translated left the native leg ungated) and `FormatGemini` (the codec). Wire-adjacent OpenAI-family siblings (Azure, Moonshot, xAI, …) and Vertex are deliberately NOT routable until each leg is opened with the surface it needs (an identity-codec images encode case, that transport's images path, its own verify) — advertising routability that dispatch cannot serve would create dead failover legs. Native passthrough remains the *design default* for any leg whose provider already speaks the OpenAI images shape, applied as each leg opens.

`canonicalbridge.Bridge` (`packages/ai-gateway/internal/execution/canonicalbridge`) holds the per-`Format` codecs and exposes `IngressChatToCanonical`; its `chatWireShapeForFormat` / `embeddingsWireShapeForFormat` helpers resolve the native `WireShape` for a `Format`.

On an upstream error response the adapter's `ErrorNormalizer.Normalize` produces a canonical `ProviderError`, which the ingress layer reshapes to the caller's format (Rule 8). Context-window overflows are classified as `context_overflow` rather than the terminal `invalid_request` bucket — each normalizer detects its provider's observed 400 signature (OpenAI `context_length_exceeded` code / "maximum context length" message, Anthropic "prompt is too long", Gemini "exceeds the maximum number of tokens") so the executor can fail over to a larger-context target. A spent provider ACCOUNT budget takes the same treatment for the same reason, and each normalizer reads the discriminator its own upstream actually publishes rather than applying one rule everywhere — the per-provider evidence discipline of §3a applied to the error path:

| Provider | Discriminator | Why that one |
|---|---|---|
| OpenAI (and Azure / GLM, which wire the same normalizer) | `error.code == "insufficient_quota"` on 429; the message markers on 400 | OpenAI publishes the code, so it is read structurally. The 400 arm covers OpenAI-compat upstreams that reuse the phrasing without the code. |
| Anthropic | message markers on the `invalid_request_error` 400 | Indistinguishable by type or status from a malformed body. |
| Bedrock | `ServiceQuotaExceededException` | AWS names the case itself, and it is not a throttle — backing off does not raise a service quota. |
| Replicate | HTTP 402 | The status is the signal. It previously mapped to `auth_failed` as the closest match the taxonomy then offered, which told an operator to rotate a key that was never the problem. |
| Cohere, Voyage | message markers on the `invalid_request` arm only | No documented signal; the 429 arm is left alone because moving a throttled request off the provider is worse than waiting. |
| Gemini | **none — deliberately not classified** | `RESOURCE_EXHAUSTED` covers both a per-minute rate limit and a spent project quota, and Google words both as "Quota exceeded for quota metric". There is no discriminator to read, and guessing would move a request off a provider a second of backoff would have served. |

The shared marker set (`specutil.IsQuotaExhaustedMessage`) is deliberately narrow and excludes the bare phrase "quota exceeded" for the reason the Gemini row gives. The resulting `provider_quota_exhausted` is eliminated at provider scope by the executor rather than deprioritised. A normalizer with a streaming twin applies the same reclassification there: the parity those functions exist to guarantee is parity of the canonical CODE, not just of the type mapping. See [error-taxonomy-architecture.md](../../cross-cutting/safety/error-taxonomy-architecture.md).

### Ingress shape preservation (round-trip)

The caller's wire shape is preserved end-to-end: whatever ingress a client calls — `/v1/chat/completions`, `/v1/messages`, gemini `:generateContent`, `/v1/responses`, `/v1/embeddings`, plus the Azure and GLM native ingresses — receives a response in that same shape. The upstream target wire is an internal concern resolved at the call site, not the caller's:

- **Request.** The ingress body is canonicalized once (`IngressChatToCanonical` for chat-kind, `IngressEmbeddingsToCanonical` for embeddings), then `TargetExecutor` sets the call-time `WireShape` from the *target* format — `ChatWireShapeForTarget` for chat-kind, `EmbeddingsWireShapeForTarget` for embeddings — so `Transport.BuildURL` and `SchemaCodec.EncodeRequest` target the correct wire for the primary target and every failover target. The per-request `Ingress.WireShape` is not mutated to the target shape; the `/v1/responses` → chat-completions downgrade is the one exception, and it is **capability-driven** — applied only when the resolved target does *not* serve the Responses API, in which case Responses canonicalizes to chat before dispatch (see *Responses egress: two signals*, below).
  - **Embeddings endpoint selection (single vs batch).** A codec may serve two upstream endpoints from one `WireShape`. Gemini's `WireShapeGeminiEmbedContent` covers both `:embedContent` (single string `input`) and `:batchEmbedContents` (array `input`); the choice is encoded only by the embeddings codec, which inspects the canonical `input` cardinality and returns an `EncodeResult.URLOverride` of `:embedContent` or `:batchEmbedContents`. Because embeddings always skip the gateway cache, the cross-format request is translated by `IngressEmbeddingsToWire`, which now **surfaces that override** alongside the wire body; `TargetExecutor` threads it into `Adapter.ExecuteWithBody`, where `applyURLOverride` swaps the action suffix on the `Transport.BuildURL` result. Dropping the override sends the batch body (`{"requests":[…]}`) to the single-embed URL and Gemini rejects it with `Unknown name "requests": Cannot find field` (regression guard: `TestIngressEmbeddingsToWire_GeminiEndpointSelection`, `TestExecute_EmbeddingsBridgeURLOverride_ReachesAdapter`). No Gemini-native embeddings *ingress* exists, so an embeddings request to a Gemini target is always cross-format and always flows through this path.
- **Response.** The upstream wire body is decoded to canonical, then reshaped back to the caller's format with `ResponseCanonicalToIngress` (chat) / `ResponseCanonicalToIngressEmbeddings` (embeddings), keyed on the ingress read from the request context (not the mutable per-request copy). The reshape fires when the ingress format differs from the target and is an identity no-op for same-format native routes.

The cross-format decision is driven by `typology.KindFromWireShape` (chat / embeddings) plus the per-provider Responses capability (see *Responses egress: two signals*, below) rather than a hardcoded ingress list or a Format-level guess, so a new chat or embeddings ingress is covered without changing the dispatch gates.

### Responses egress: two signals (capability + content)

The `/v1/responses` ingress is not a codec on top of chat — it is a co-equal OpenAI standard with strictly greater expressive power (typed `output[]` items, reasoning items, built-in tools, audio streaming, stateful `previous_response_id`). Routing it correctly uses **two independent signals**, never a single Format guess. The **request side** decides what wire to send upstream from a per-provider capability; the **response side** decides how to decode/encode from the actual bytes that came back. The two are deliberately separate: the request-side capability can be wrong (a provider may mis-declare or change behaviour), and the content signal is what makes the egress bulletproof regardless.

**Request side — capability, then content.** Whether a target receives a Responses-shape body or the downgraded chat-completions body starts from the per-provider capability `servesResponsesAPI`, resolved by `canonicalbridge.Bridge.ServesResponses(target, override, body)`:

```
/v1/responses ingress, resolved target T, request body B:
  ServesResponses(T, B)?
    yes → send Responses-shape body to T's /v1/responses   (built-in tools preserved)
    no  → responses → canonical(chat) → T's native chat wire (the downgrade)
```

The capability alone is not sufficient, and the third argument is why. `/v1/responses` accepts a
strict input vocabulary — `input_text`, `input_image`, `input_file` — and a caller sending
anything else (`input_audio` is the one production hits) gets a 400 from the wire, even on a
target that genuinely serves Responses and a model that genuinely accepts the modality. The
content the caller sent is therefore part of the decision: a body carrying an input part this wire
does not serve is downgraded to the chat wire, which does carry it. `ServesResponses` takes the
body as a parameter rather than consulting it elsewhere so the compiler forces every call site to
agree — the four sites that decide this must not disagree about one request.

The check itself (`specs/openai/responses.ExceedsInputVocabulary`) is a byte scan gating an
authoritative walk, because the walk is expensive on exactly the bodies that need it and this
predicate runs up to five times per request. The scan may say YES on a caller's own text and cost
a walk that answers correctly; it must never say NO when an unserved part is present, which means
it also has to see `{"type":"input_audio"}` — JSON's escape set makes `\uXXXX` the only
spelling that can hide an identifier, so a body carrying one goes to the walk.

Resolution order: the per-provider override (`Provider.serves_responses_api`, a nullable Boolean column) is **downgrade-only** — it can turn the capability *off* for a chat-only OpenAI-compatible endpoint, but cannot claim a capability the adapter lacks; with no override (the common case) the default comes from the adapter's `RequestShapes` (`RequestShapes ⊇ WireShapeOpenAIResponses`; today only `FormatOpenAI`). A lockstep test pins the Format default against the OpenAI adapter's declared `RequestShapes`. The capability is resolved per-target from the hydrated routing snapshot inside the failover loop — never a per-request DB read — and rides the already-threaded `CallTarget`. The request-side wire decision consults it at the same value everywhere: the executor's `nativeResponses` decision, `stage_routing.go`, `stage_cache_body.go`, and `bridge.IngressChatToWire`. The executor detects a `/v1/responses` ingress by `base.BodyFormat == FormatOpenAIResponses` — the per-request-stable signal — NOT by `WireShape`: the cache-prep leg downgrades `resolved.WireShape` to chat when a non-responses *primary* leads the target list, and keying on that WireShape would leave a responses-serving *failover* target (a mixed target list) unrecognised — its verbatim Responses body would be posted to the chat URL and 400. On the native-responses branch the executor restores `WireShape = WireShapeOpenAIResponses` per-target so `BuildURL` targets `/v1/responses` regardless of the earlier downgrade. This keeps real OpenAI / Azure working out of the box (default true) while letting a mock or chat-only endpoint opt out, so the gateway never POSTs a Responses body to an endpoint that would 404 it.

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

**The fixture is the load-bearing half.** `TestShapeRoundTripIdentity` runs a MINIMAL body — one user turn of plain text — and compares an (role, text) signature. That shape cannot express most of what a conversion actually gets wrong: a dropped image, rewritten tool-call arguments, a tool definition that does not survive, or a media block replaced by a text placeholder all pass a text-only signature untouched. `TestShapeRoundTripIdentity_RichContent` (`roundtrip_content_test.go`) therefore runs the same chain over a body carrying a system instruction, text plus an image in one turn, a declared tool, an assistant tool call, its result, and a following turn — and a signature naming every one of those. **What the fixture does not carry, the standard does not defend**; extend the fixture, not just the shape list, when adding a content kind.

Enumerated non-round-trips, each with the reason it is allowed to lose fidelity:

| What | Why it cannot round-trip |
|---|---|
| JSON object key ORDER in tool schemas and tool-call arguments | Re-serialized per hop; normalized before comparison. |
| Protocol-default backfill (`max_tokens`, image `detail:"auto"`) and the per-hop model alias | Added by §4, not carried by the caller. |
| The literal `tool_call_id` string | A wire that carries no id (Gemini before 3) forces one to be synthesized. What must hold instead is the LINKAGE — every tool message quotes the id of a call in the same conversation — asserted by `TestCanonicalToolResultsReferenceARealCall`. |
| A non-object tool result through a Gemini hop | Gemini's `functionResponse.response` is documented as an object, so `"18C"` must be wrapped as `{"result":"18C"}`. Unwrapping on return would corrupt a genuinely object-shaped result from a Gemini-native tool, so the comparison normalizes the single-key envelope instead — a result that changes in any other way still fails. |

## 3a. The twelve binding rules

These rules are binding. Any change under `packages/ai-gateway/internal/providers/specs/<name>/` (codec, stream session, error normalizer, hub ingress) must conform before shipping. Run `/adapter-conformance-check` to audit an adapter against them.

### Rule 1 — canonical is the OpenAI shape

All internal flow — router input, cache key, hook input, audit envelope, request lineage — sees the canonical form, which is OpenAI's shape for the endpoint kind (§1). The chat canonical is OpenAI chat-completions:

```
model · messages[] · max_tokens / max_completion_tokens · temperature · top_p · top_k ·
stream · stop · response_format · tools[] · tool_choice · parallel_tool_calls ·
metadata · stream_options
```

The embeddings canonical is the OpenAI embeddings shape (`input` · `model` · `dimensions` · `encoding_format`). New canonical fields require an architecture change — adapters do not add canonical fields unilaterally.

**Content parts, including files.** A canonical message's content array carries OpenAI chat's part types: `text`, `image_url`, `input_audio`, and `file` (`{"type":"file","file":{file_id | filename + file_data | file_url}}`). The `file` part is what a document rides in, and every wire that can carry one maps onto it — Anthropic `document` (`source.type` base64 / url / file), Gemini `inlineData` / `fileData`, OpenAI Responses `input_file`. Two rules follow from Rule 10 and hold for all of them:

- **A part is never replaced by a note about itself.** Emitting `[nexus: input_file report.pdf preserved]` as text discards the caller's bytes, has the model answer about a filename it cannot read, and returns 200 — a wrong answer reported as a right one.
- **A wire that cannot carry the part refuses, naming the field.** A bare `file_id` is an OpenAI-side handle Gemini cannot resolve, so the Gemini codec errors rather than forwarding an empty part. Forwarding nothing is the same lie as forwarding a placeholder.
- **The destination is not always a content part.** Cohere carries documents in a top-level `documents` array — text passages with an optional title, not attachments — so its codec lifts the `file` part out of the message and appends it there, leaving the caller's question in place and extending any `documents` the caller already set rather than renumbering over them. A message left with nothing but the document is refused: the wire rejects both an empty content array and an empty string, and inventing a prompt would change the request. The lift also bounds itself — `documents` carries text, so a media type that is not text is refused with the type named, because extracting a PDF silently would change what the model was given.

  This is the shape of the general obligation: **the mapping is to wherever that wire puts the concept**, which is not always the structural counterpart of where the caller put it. A `file` part that reaches Cohere unmapped answers 422 `unrecognized content type 'file'` — a message that reads like a capability limit and is not one. Establishing which it is takes three probes, not one: through the gateway, direct with our shape, and direct with the vendor's documented shape. Only the third distinguishes "this wire cannot" from "we send the wrong form", and it is the one that decides whether the answer is a codec mapping or a catalog entry.

Ahead of the codec, `inputModalityGuard` (`ingress/proxy/stage_routing_floor.go`) answers the same question one level up — does the request carry a modality the resolved MODEL does not accept — and is the mirror of `floorGuard` beside it, which asks whether the model needs something the request lacks. It refuses only on modalities the catalog **somewhere** describes. That condition is load-bearing rather than defensive: all 200 catalog entries declare `inputModalities` and every one lists `text`, about half list `image` and ten list `audio`, so a model omitting `audio` is genuinely saying it cannot take audio. **No entry declares `file`**, so its absence from a given model says nothing about that model, and refusing on it would reject every document request including the lanes that carry one correctly. When file capability reaches the catalog the guard begins enforcing it with no code change.

Gemini uses one part shape for every attachment, so its declared mime type is the only thing distinguishing a document from an image; the ingress routes on it. Reading every attachment as an image made a PDF arrive at the next wire claiming to be one.

**`encoding_format` is honoured by the gateway, not by the adapters.** A canonical field is normally each adapter's job to translate, but `encoding_format` is the documented exception: only the OpenAI/Azure/DeepSeek codecs can pass it to the wire. Gemini, Vertex, Bedrock and Voyage always emit float and never read the field, and Cohere's wire has no base64 embedding type at all. An adapter therefore translates `encoding_format: "base64"` into whatever its wire *can* accept — for Cohere that is `embedding_types: ["float"]` (`specs/cohere/embed_codec.go`) — and the **ingress response layer** re-encodes the canonical float vector to little-endian float32 base64 for the caller (`honorEmbeddingEncodingFormat`, `internal/ingress/proxy/embedding_encoding.go`).

Two things an adapter must NOT do with this field. It must not **reject** the request (Cohere used to answer 400 "no Cohere wire equivalent"), and it must not **silently downgrade** it to float and return floats. Both break a stock caller, because the official OpenAI SDKs set `encoding_format: "base64"` IMPLICITLY when the caller omits it and then decode the reply as packed float32: openai-node runs `Buffer.from(embedding, "base64")` over whatever arrives, so a float array is truncated one-byte-per-number and reinterpreted into a vector of **one quarter the length**, filled with garbage, at HTTP 200. That is the silent-wrong-answer class Rule 1 exists to prevent — observed on staging 2026-07-27 as a 768-dimension request returning 192 components (AP-3). Pinned by `TestServe_EmbeddingsBase64_IsHonouredEndToEnd` and `TestCohereCodec_EncodeRequest_embeddings_base64AsksWireForFloat`.

**Intentional exception — rerank canonical is Cohere-shaped, not OpenAI.** OpenAI ships no rerank API, so "canonical = OpenAI shape" has no OpenAI anchor for the rerank kind. The de-facto industry standard is Cohere's `/v2/rerank` shape (`{model, query, documents[], top_n?, return_documents?}` → `{id?, model, results:[{index, relevance_score, document?}], meta:{billed_units}}`), which Voyage/Jina mirror and which LiteLLM/TrueFoundry expose as their unified `/rerank`. Nexus adopts the same: the `/v1/rerank` ingress body IS the canonical rerank shape, mounted with `BodyFormat = FormatCohere` (not `FormatOpenAI`), and `WireShapeCohereRerank` is both the canonical ingress shape and the Cohere wire shape (they coincide). The Voyage leg owns its own canonical→Voyage translation (`top_n` → `top_k`; `specs/voyage/rerank_codec.go`). **An adapter audit must not flag `WireShapeCohereRerank`-as-canonical as a Rule 1 violation** — it is this documented, intentional exception. Rerank is currently the only endpoint kind whose canonical is not OpenAI; a future OpenAI rerank API would be the second shape, translated OpenAI→Cohere-canonical. Design contract: `docs/developers/specs/e89-s1-rerank.md` §1-§2.

### Rule 2 — each non-OpenAI adapter owns its full bidirectional translation

`SchemaCodec.EncodeRequest` does canonical→wire; `SchemaCodec.DecodeResponse` does wire→canonical. The OpenAI side stays the identity codec (`packages/ai-gateway/internal/providers/specs/openai`) — it never carries "this came from Anthropic so do X" branches. OpenAI shape is the bus; every other shape adapter (`specs/anthropic/codec`, `specs/gemini/codec`) wires itself onto it.

**Assistant reasoning is an L2 universal field.** Both directions carry chain-of-thought text through canonical `reasoning_content`. Anthropic replay adds ordered `nexus_thinking` entries carrying each signed `{thinking, signature}` or `{redacted_data}` block; the Anthropic codec reconstructs native blocks only from signed entries and never fabricates one from unsigned `reasoning_content`. Gemini routes `{text, thought:true}` to `reasoning_content` and carries each `Part.thoughtSignature` only beside its matching canonical tool call. Both carriers are consumer-gated at every egress leg: Anthropic/Bedrock retain `nexus_thinking`, Gemini/Vertex retain `thought_signature`, and other targets strip both while preserving universal reasoning text. The Responses request converter's replay of `reasoning` input items remains a tracked follow-up.

Provider-owned signature metadata is opaque and exact-bound. Anthropic `thinking` blocks and `redacted_thinking` data round-trip only through the ordered `nexus_thinking` carrier; unsigned `reasoning_content` is never promoted into native thinking, and a `signature_delta` is never treated as reasoning text. Gemini `Part.thoughtSignature` round-trips only beside its matching `tool_calls[].function.thought_signature`; function-call name, arguments, order, and identity remain bound. Cumulative stream snapshots are correlated by candidate/Part position, not by native ID alone: repeated snapshots replace only that position, while a malformed response that reuses one native ID at two positions preserves both calls so downstream ambiguity checks fail closed. Coordinate-bound fallback IDs keep ID-less duplicate calls distinct. The egress carrier gate is consumer-based: only Anthropic/Bedrock retain `nexus_thinking`, only Gemini/Vertex retain `thought_signature`, and all other primary, identity, and failover legs strip both. Compliance redaction treats a signed Gemini call as indivisible and fails closed if its tuple changes.

**A wire that rejects unknown fields is projected onto, never subtracted from.** Cohere v2 answers HTTP 422 `unknown field: parameter '<x>'` for any name it does not know, so a codec that forwards the canonical body minus a list of known offenders ships a 422 for the first canonical field nobody has hit yet. Production showed the loop closing on itself: the deploy that folded `max_completion_tokens` went out around 05:00 UTC 2026-08-05, and by 12:00 the same five Cohere models were failing on `nexus`. The encoder therefore declares the field set it may emit (`specs/cohere/codec_request_fields.go`) and drops everything else, which makes the next canonical field a no-op instead of the next incident.

Establish such a set **both ways**. Probing tells you a name is wrong — it caught the header comment claiming "top_p stays top_p (matches Cohere's `p` field via OpenAI alias)", when `top_p` is in fact a 422, so every request carrying the most common sampling parameter was failing under a comment saying it was handled. But probing only covers names someone thought to probe, and for a projection the untested risk runs the other way: a field the provider accepts and nobody listed is dropped **silently**, which is quieter than a 422 and worse. The first draft of the Cohere set, built from probes alone, was missing `thinking` and `priority`; the API reference had both. Read the reference for the enumeration, call the endpoint for the verdict.

Keep a field whose failure names a MODEL or REQUEST condition rather than an unknown name. Cohere separates them precisely — 422 `unknown field` / `unrecognized content type` means the wire does not know it, while 400 `logprobs is not supported with the specified model`, 400 ``​`thinking` parameter is not supported with the specified model``, and 400 `thinking.token_budget must be less than or equal to max_tokens` all mean it does. Filtering the second class replaces a true per-model capability answer with our own silence.

**Message content is refused, not dropped.** The same subtract-and-forward reasoning does not transfer from top-level fields to content blocks: dropping a sampling parameter degrades a request, while dropping a content block changes what the request means — the model answers about material it never received, and every byte-level assertion stays green. Cohere makes the hazard concrete: an unknown block type is a 422, but `video_url` produced `400 invalid request: must have non-empty content or tool calls` — it discarded the block and then complained the message was empty. The layer that refuses an unrepresentable block is the capability guard that reads the model's declared modalities, on every selection path, not the individual codec.

### Rule 3 — per-model wire quirks live in the adapter that talks to that wire

Parameter renames, mandatory clamping, and HTTP-400 deprecations live in the adapter that owns the wire — in its codec. For the OpenAI family that is the identity codec's `Contract`, constructed per sibling in its `spec.go`: `FieldRule`s for body-root fields (renames, strips, forced values) and `StructuralRule`s for edits inside nested structures (message-array back-fills). They do not live in cross-adapter switches inside `spec_adapter.go`, and there is no dispatch-level rewrite callback.


**Every sampling-parameter rule is an ACCEPTS list over a vendor namespace, and the default inside that namespace is to strip.** Anthropic (`claudeModelsAcceptingSamplingParams`), OpenAI (`gptFamiliesAcceptingSamplingParams`, over generation ≥ 5 and the o-series) and Moonshot (`kimiFamiliesAcceptingTemperature`, over `kimi-`) all read the same way: a family the list does not name is stripped, not forwarded.

The reason is routing. A rule or the smart router sends the request to a model the caller never named, so the parameters that arrive were chosen for a different model. If the unknown direction forwards, routing itself becomes the cause of a 400 — the caller asked for something that worked and got a failure created by our redirect. Stripping costs the caller sampling control on that request, recorded on `X-Nexus-Coerced`; forwarding costs them the request. That asymmetry is what "fails safe" means here, and it is why the namespace matters as much as the list: `gpt-4o` and `moonshot-v1-*` sit OUTSIDE the strip and keep their caller's temperature, because stripping there would forfeit intent without averting anything.

The namespaces are version-aware, not string prefixes. `gptGenerationAtLeast(id, 5)` matches an unreleased `gpt-6`, where a literal `"gpt-5"` prefix would not — and a lineage nobody has probed yet is exactly the one that must land inside the strip. `specutil.MatchesFamily` does the same job on the ACCEPTS side, matching only at a version boundary so `gpt-5.40` cannot inherit `gpt-5.4`'s probed carve-out.

This replaced three denylists on 2026-08-06. The denylist argument was that the safe direction follows each vendor's trend — Anthropic retires these params generation by generation, OpenAI and Moonshot reject on only a few families — but it mistook *today's* rejecting set for the direction an *unknown* family should fall. The kimi-k2.7 incident is the counter-example the codebase already contained: the family shipped in the catalog while the denylist still ended at k2.6, and every temperature-sending client got the vendor's 400 until a smoke run found it. On 2026-08-06 the vendor's `/v1/models` carried `kimi-k3` and neither the catalog nor the registry had heard of it.

Membership on an ACCEPTS list requires an observed 200 with the parameter present. `scripts/check-quirk-coverage.mjs` still fails CI when a catalog chat family has no recorded decision, and every list is anchored from `scripts/quirk-coverage.config.mjs`, so dropping a family from a codec's allowlist without a probe reddens there. The gate remains the safety net; the default is now safe on its own.

*"Move the data to the model catalog next to `inputModalities`"* stays refused. Whether `gpt-5` rejects `temperature` is a fact about OpenAI's API, identical in every deployment — not per-deployment configuration. A catalog row is admin-editable and has nowhere to carry the dated probe each entry cites, and that evidence is load-bearing: the Anthropic list records that the vendor's own deprecation table under-reports the rejecting set (it omits `claude-fable-5`). Moving it would scatter one universal fact across deployments to drift independently, and let an operator tick a capability into a 400 they cannot diagnose.

| Quirk | Lives in |
|---|---|
| Structured outputs are spelled four different ways and every wire we route to serves them — probed per MODEL on 2026-08-19: OpenAI `response_format.json_schema` (16/17; `gpt-4-turbo` answers 400 `'response_format' of type 'json_schema' is not supported`), Anthropic **`output_config.format`** (10/10), Gemini `generationConfig.responseSchema` (7/7), Cohere `response_format` (5/5), Moonshot passthrough (6/7), DeepSeek 400 `This response_format type is unavailable now` (0/2). Anthropic rejects the OpenAI spelling outright (`response_format: Extra inputs are not permitted`) and names the right one in its own 400 (`output_format: This field is deprecated. Use 'output_config.format' instead`); no beta header is needed. The adapter used to refuse `json_schema`, which made Claude the only target in the fleet where a working OpenAI structured-output request failed | `specs/anthropic/codec/codec.go` (the `response_format` switch — unwraps OpenAI's `{name,strict,schema}` envelope to the bare schema; `json_object` keeps its system-instruction path because Anthropic has no json_object mode). **No capability enumeration backs this and none should:** every wire above answers for itself, and a list here would refuse what a provider would serve the moment it lags a model. The one case passthrough cannot catch is recorded below. |
| Structured output arrives on the **ingress** in the same four spellings, and the canonical body is the OpenAI one, so each non-OpenAI ingress owes a translation IN as well as OUT. The Anthropic Messages ingress had only the outbound half: a caller posting `output_config.format` to `/v1/messages` had the schema read for ROUTING and then dropped on the way into the canonical body. Measured live on prod 2026-08-19 — `model: auto` plus a schema routed to `gpt-5.6-terra` (correctly: the routing dimension picks a schema-capable model) and answered `{"should_respond": true, "probe_id": "…"}`. Valid JSON, wrong keys: the model was told to produce JSON and never told the shape. Half a translation is worse than none here — the pool is narrowed for a constraint that is then discarded | `specs/anthropic/ingress/hub_ingress.go` (`output_config.format` with `type: json_schema` becomes canonical `response_format.json_schema`; the schema rides along only when the caller sent one, since an absent schema is the shape Anthropic itself rejects with `output_config.format.schema: Field required`). `output_config.effort` is deliberately NOT read there — it is reasoning depth, not a response format, and reading it would narrow the pool for a constraint the caller never wrote. A request that lands back on an Anthropic target round-trips through the codec into `output_config.format` again. |
| The Gemini ingress had the same gap and it was worse: `generationConfig.responseSchema` was not carried at all. The generationConfig walk moved temperature, topP, topK, maxOutputTokens and stopSequences across and stopped, so a caller posting a schema to `:generateContent` had it dropped before routing ever saw it | `specs/gemini/ingress/hub_ingress.go` (`responseSchema` → canonical `response_format.json_schema`). `responseMimeType: application/json` on its OWN is the json_object case and is deliberately not treated as a constraint: it asks for "some JSON", which every target honours or is instructed into. |
| **Canonical is an OpenAI body, and OpenAI REQUIRES `response_format.json_schema.name`** — 400 `Missing required parameter: 'response_format.json_schema.name'`, measured live 2026-08-19 when a `/v1/messages` request carrying a schema routed to an OpenAI target while the same request on an Anthropic target served fine. Neither non-OpenAI ingress wire has the field: Anthropic's `output_config.format` and Gemini's `responseSchema` are both bare schemas | `providers/specutil/structured_output.go` (`CanonicalJSONSchema` fills `name`, one constant for both ingresses). Same adapter auto-fill as the Anthropic `max_tokens` default: a protocol-required field supplied from what the caller omitted rather than a knob or a refusal. The value is constant on purpose — derived from the schema it would churn on every unrelated property edit, derived from the request it would put caller text in a field OpenAI validates against `^[a-zA-Z0-9_-]+$`. |
| `kimi-k2.5` accepts `response_format: json_schema` and **silently ignores it** — HTTP 200, `finish_reason: stop`, prose in `content` where the caller's schema was promised. Every other Moonshot model probed obeys it. This is the one structured-output answer passthrough cannot make honest, because the wire reports success; DeepSeek's flat 400 is the safer failure | not handled — recorded so the next reader does not mistake the 200 for conformance. A per-model gate is not worth its surface for a single model; revisit if a second one appears. `o3-mini` is a milder cousin: it obeys the schema but delivers it in OpenAI's `refusal` channel with `content: null` |
| `claude-opus-4-7` deprecates `temperature` / `top_p` / `top_k` | `specs/anthropic/codec/codec.go` (`anthropicModelRejectsSamplingParams`) |
| `claude-4.x` rejects `temperature` + `top_p` together | `specs/anthropic/codec/codec.go` (`anthropicModelRejectsTempTopPTogether`) |
| gpt-5.x / o-series rename `max_tokens` → `max_completion_tokens` and (except gpt-5.4) reject the classic sampling params with 400 `Unsupported parameter: 'temperature' is not supported with this model` | `specs/openai/rewrites` — the quirk table as data: `FieldRule`s gated by two predicates, assembled into the identity codec's `Contract` by `OpenAIContract()` (Azure constructs its codec from the same table). The predicates split on probed evidence: `NeedsMaxTokensRename` covers every gpt-5\* family and the o-series (the rename 400 is real for all of them, gpt-5.4 included), while `RejectsSamplingParams` carves out gpt-5.4 (probed: temperature answers 200 on BOTH wires — stripping it was a pure over-strip) and defaults every unprobed gpt-5.x family to the strip, fail-safe. Both request wires draw from the one table — the chat rule list carries rename + strips, the responses list carries the strips only — and both codec entry points apply it through one shared applier, so neither the two wires nor the two doors can drift: the historical incident was exactly a Responses wire with no strip, so `gpt-5.6-*` 400'd there while the identical body answered 200 on chat. The `max_tokens` rename is deliberately chat-only: `/v1/responses` carries the cap as `max_output_tokens`, so **both** chat names are invalid on that wire and renaming one rejected parameter to another would only change which 400 the caller gets. |
| gpt-5.6-\* reject function tools on `/v1/chat/completions` unless `reasoning_effort` is EXPLICITLY `"none"` — 400 `Function tools with reasoning_effort are not supported for gpt-5.6-terra in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.` for any other value AND for the absent field (the family default is itself rejected, so a strip cannot serve; probed 2026-07-17: explicit `"none"` answers 200 with working tool_calls, empty `tools:[]` answers 200 without the field, gpt-5.5/gpt-5.4 accept tools with the field absent) | `specs/openai/rewrites` — a forced-value `FieldRule` (`SetRaw: "none"`, condition-gated on a non-empty `tools` at the body root, chat rule list only; the vendor's own 400 points tool callers at `/v1/responses`, so nothing is forced there). Reported as `reasoning_effort→none (function tools on the chat wire)` via x-nexus-coerced. |
| kimi-k2.5 / k2.6 / k2.7-code / k2.7-code-highspeed require `temperature = 1` and reject any other value with 400 "invalid temperature: only 1 is allowed for this model" | `specs/compat/moonshot/rewrites.go` — `FieldRule` strips gated on `IsFixedTempModel`, assembled into the identity codec's `Contract` by `Contract()`, so BOTH codec entry points apply them — the native chat leg and the leg bridged from `/v1/messages` (a kimi request routed cross-format from `/v1/messages` must get the same strip, or it 400s upstream; observed on the prod-20260717 smoke, P3A arms). The k2.7 families were added after a production smoke caught them 400ing: they shipped in the model catalog while the rule was still a denylist ending at k2.6, and a family a denylist does not name silently inherits "no quirk". That is why the rule is now an ACCEPTS list (`kimiFamiliesAcceptingTemperature`) over the `kimi-` namespace: a family the list does not name is stripped. **Send a new Moonshot family a temperature before adding it to the accepts list** — the catalog is not evidence either way, and the cost of guessing wrong is now a dropped parameter instead of a 400. |
| DeepSeek thinking models (`deepseek-reasoner*` and the whole `deepseek-v4` family — both `deepseek-v4-pro*` and `deepseek-v4-flash*` observed) reject a forced `tool_choice` (`"required"` or a named function) with 400 "Thinking mode does not support this tool_choice", and reject a replayed history whose ASSISTANT TURNS are missing `reasoning_content` with 400 "The `reasoning_content` in the thinking mode must be passed back to the API." The trigger for the second is the REQUEST carrying `tools`, not the individual turn carrying `tool_calls` — probed 2026-08-19 one variable at a time: plain assistant turns with no tools → 200, the same turns with a tools array → 400, with the empty presence marker restored → 200. A tool_calls-scoped fill therefore covered one shape and left staging failing on a 17-tool request whose assistant turns had never called anything. | `specs/compat/deepseek/rewrites.go` — one `StructuralRule` on the identity codec's `ChatStructural` list (value-conditional removal + message-array back-fill are shapes a body-root `FieldRule` cannot express; structural rules run on the decode door, which the same models must decode through anyway). The gate matches the `deepseek-v4` family prefix, not exact suffixes — the quirk is a property of the v4 thinking architecture, and an exact-suffix denylist went stale once: `deepseek-v4-flash` 400'd every tool-loop client until it was added. Both codec entry points apply it, so histories bridged from `/v1/messages` get the same fixes as native chat traffic. |
| Gemini tool `parameters` / `responseSchema` are proto-backed Schemas, not free-form JSON Schema: any unknown key (`additionalProperties`, `$comment`, `const`, …) fails the whole request with 400 `Unknown name "<key>" … Cannot find field`, and a union `type` array fails with `Proto field is not repeating`. The OpenAI `response_format.json_schema` envelope keys (`name` / `strict`) are equally unknown to the proto | `specs/gemini/codec/schema_sanitize.go` (`sanitizeGeminiSchema` — allow-list of the live-probed accepted key set, recursive over `properties`/`items`/`anyOf`/`oneOf`/`allOf`, with semantic conversions `type:["T","null"]`→`type`+`nullable`, `const`→`enum`, `examples`→`example`, numeric `exclusiveMinimum/Maximum`→`minimum/maximum`; applied to both tool `parameters` and the unwrapped `json_schema.schema` before they reach the wire). One value-level rejection is handled alongside the key allow-list: an `enum` array carrying an empty-string entry 400s with "...enum[N]: cannot be empty" (observed on gemini-2.5-pro from OpenAPI-generated tool schemas that emit `""` as an unset sentinel) — the empty entries are dropped and the key removed entirely when none survive. See the reference rule below — `$ref` is resolved, not dropped. |
| Gemini's Schema proto has no reference mechanism, and `$ref` is the mainstream structured-output shape: Pydantic's `model_json_schema()` emits `$defs` + `$ref` for any nested `BaseModel`. Dropping it is not neutral — a `$ref` under `properties` leaves the property as `{}` while `required` still names it, and a `$ref` at the root leaves the schema empty while `responseMimeType: json` still goes out, so the model returns arbitrary JSON with HTTP 200 and the caller cannot detect it | `specs/gemini/codec/schema_inline.go` (`inlineSchemaRefs` — resolves every local pointer against the schema's own `$defs`/`definitions`, merges `$ref` siblings as caller overrides, drops the dictionaries once folded, and runs **before** sanitization, which has no way to keep a reference). **It will not guess a shape:** a pointer that dangles inside a dictionary the schema carries (`#/$defs/Missing`), and a recursive model with no finite expansion, fail the request — restoring the loud, diagnosable failure the vendor itself gave (`400 Unknown name "$ref"`). One reference class gets a per-call-site carve-out: a pointer whose target was **never shipped with the schema** (OpenAPI-style `#/components/...` from a tool extracted out of a larger document, or a remote URL — observed live as a prod `proxy_error` 400 on a tool schema the OpenAI and Anthropic wires accept verbatim). On the **tool-parameters** path that reference degrades to `{"type":"object"}` (keeping the caller's sibling `description`) and is reported through `EncodeResult.Rewrites` as `tools.<name>.parameters.$ref(<ref>)→object`, so the loosened argument contract reaches `x-nexus-coerced` instead of making Gemini the one permanently failing target. The **responseSchema** (structured output) path stays strict — there the schema is the caller's output contract, and degrading it is exactly the silent-wrong-answer class this rule exists to end. The mode is part of the schema cache key (`schema_cache.go`), so the two paths can never serve each other's result. |
| — (how the two rules above are sequenced and paid for) | `specs/gemini/codec/schema_cache.go` (`prepareGeminiSchema`) runs the whole caller-JSON → wire pipeline — decode, inline, sanitize, marshal — and reuses the finished, marshalled schema across turns, keyed on a SHA-256 of the caller's bytes, because a tool declaration is immutable and an agent resends the same declarations on every turn. Inlining sits **inside** the cached pipeline: it is a pure function of the same bytes the key derives from, so caching the inline+sanitize pair is sound and neither walk is paid per request. Its error channel carries two classes and call sites **must** separate them with `isSchemaRefFailure`: a reference failure fails the request, anything else means "not a usable schema" and gets the default declaration. Failures are never cached — memoizing one as the empty schema it sanitizes to would ring the bell on turn one and silently degrade every turn after. The cache is bounded (per-entry and total byte budgets, retiring a generation rather than growing) since the key derives from caller-controlled bytes, and it stores encoded bytes rather than the schema map so the shared value cannot be mutated by a later edit to the encoder. |

When a new family ships a wire deprecation, add the rule to the adapter that owns its wire. Cross-adapter shared helpers create the wrong dependency direction.

**The codec is always in the request path — two entry points.** A same-spec body (the "passthrough") is semantically equivalent to one that completed the full `ingress → codec → canonicalize → codec` round-trip; what it skips is only the trip through the OpenAI canonical spec, never the codec. `SchemaCodec` therefore has two request entry points:

- `EncodeRequest(shape, canonicalBody, target)` — canonical in; full translation plus the per-model quirks (the cross-format leg).
- `RewriteNative(shape, nativeBody, target, stream)` — native in; applies ONLY the differential the second codec pass would have applied (the resolved-model stamp, the per-model quirks, protocol back-fills like streaming `stream_options.include_usage`) and never re-translates, which is what preserves native-only features the canonical subset cannot express. The fast path returns the same slice. **Every codec implements it explicitly — there is deliberately no embeddable default** (a silent verbatim default would let a new model-in-body codec skip the stamp and 404 every aliased model); model-in-URL wires (Gemini, Bedrock) write a one-line verbatim return stating that rationale.

Dispatch owns zero vendor knowledge: it runs the guards (nexus-strip on the native leg; the non-object carve-out — a stated exception where the body is forwarded verbatim and the codec is not called, because a JSON edit on a non-object fabricates a synthetic body and wire must equal audit; and a degenerate-target return when no `ProviderModelID` resolved — nothing to stamp, no quirk can key on an empty model), triages on **three keys** — format equality, both-sides OpenAI-family, or the Responses capability (`RequestShapes` default with a **downgrade-only** per-provider `ServesResponsesAPI` override, matching the canonical-bridge decision so the two legs can never triage the same request differently; naive format equality once turned a native `/v1/responses` passthrough into a canonicalize and shipped a 400) — and delegates. The mechanism is pinned by a property-test family: `RewriteNative ∘ RewriteNative = RewriteNative` (idempotency, incl. no re-reported coercions — the executor re-prepares on retry/failover) for every editing codec, verbatim-fast-path slice identity, and per-wire two-doors parity (`RewriteNative(B) ≡ EncodeRequest` where both doors exist — the `/v1/responses` wire and the Anthropic `/v1/messages` wire, whose native leg applies the same sampling / max_tokens coercions as the cross-format leg per the owner-approved D3 decision; each adapter migration extends the family to its wires). Every adapter's rules ride its codec — the OpenAI family through the identity codec's per-sibling `Contract` (field rules + structural rules), the translation codecs (anthropic, gemini) inside their own `EncodeRequest`/`RewriteNative`. The transitional dispatch callback branch and the `PassthroughRewrite`/`PassthroughRewriteApplies` spec fields are DELETED: dispatch owns zero vendor knowledge, structurally.

### Rule 4 — extension fields ride in `nexus.ext.<provider>.<key>`

Fields with no clean OpenAI mapping (Anthropic's `thinking`, Gemini's `thinkingConfig`, Bedrock's `anthropic_version`) travel inside the `nexus.ext.<provider>.<key>` namespace on the canonical body. The helpers live in `packages/ai-gateway/internal/providers/canonicalext`:

- `Get` / `Set` — read and write a namespaced value.
- `Strip` — remove the whole `nexus` root key.
- `ScanUnsupported` — walk top-level canonical keys against an adapter's supported set.
- `WarnOnce` — emit a one-shot WARN when an adapter observes an unsupported canonical field, so operators see drift between the canonical surface and the codec.

Removal lives in the same package as the reads and writes, so one package owns the key's whole lifetime. `Strip` matches only the ROOT key: a `nexus`-named property inside a caller's tool schema or `json_schema` is the caller's data, and an earlier version that matched the reserved name at any depth deleted such a property while leaving `required` pointing at it — an invalid schema, a model answering about a contract it never received, and HTTP 200 with every byte-level assertion green.

**The namespace must not egress in EITHER direction.** Both halves are enforced, and they fail differently.

*Upstream.* A request body must not carry it. When it did, Cohere answered HTTP 422 `unknown field: parameter 'nexus'`; OpenAI answers 400, while DeepSeek and Moonshot answer 200 and quietly keep the metadata. The strip sits at `executeWithBodyAndURL`, the single frame every dispatch path funnels through, because the two earlier placements were each correct for the legs whoever wrote them had enumerated: in the codecs, until Cohere's forwarded it; then on `PrepareBody`, until the cross-format failover leg — which builds its body in `canonicalbridge` and calls `ExecuteWithBody` directly — turned out never to touch `PrepareBody`.

*Downstream.* A response body must not carry it either, and this half is the one that is easy to miss. The Anthropic, Gemini and Responses egress converters are PROJECTIONS — they rebuild the client body out of named fields — so they drop the namespace as a consequence of how they work. The OpenAI-family egress is the IDENTITY: canonical already *is* the caller's shape, so whatever a codec left in the namespace is what the client receives. Relying on the projections' side effect is not a rule, so the strip is applied to the egress result on every arm, at `egressReshapeNonStream` for a live response and at `handleNonStreamHit` for a cache replay (before the audit capture, so the stored copy equals what the client received). Streaming carries no namespace at all.

**A codec must not write the namespace into a response it does not itself consume.** The strip is the guarantee; this rule is what keeps codecs from leaning on it. A response-side extension key is legitimate ONLY as a carrier between a decode and the specific egress converter that reads it — `nexus.ext.openai.responses.*`, written by the Responses decode and consumed by the Responses egress encoder, is the one such carrier. Anything else belongs in a canonical field the client is meant to receive (Anthropic's cache-creation count rides in `usage.prompt_tokens_details.cache_creation_tokens`, beside the `cached_tokens` OpenAI already defines) or in a response header, which is the gateway's documented channel for what it added — `X-Nexus-Cache`, `X-Nexus-Coerced`, `X-Nexus-Hook`. Using the extension namespace as a client-facing surface would make one key name mean "internal, strip it" going up and "product surface, keep it" coming down, and that ambiguity is what let the upstream leak survive being fixed twice.

### Rule 5 — cross-format callers canonicalize before the codec

A caller holding an ingress-format body (Anthropic `/v1/messages`, Gemini `:generateContent`) MUST canonicalize first:

```go
canonical, err := bridge.IngressChatToCanonical(ingress, body, target)
```

before invoking the codec. Skipping canonicalization makes the OpenAI identity codec forward the ingress body verbatim, and the upstream returns 400 (or parses partially and produces garbage). `EncodeRequest` accepts a canonical body (or a codec-empty passthrough); it does not accept arbitrary shapes.

### Rule 6 — streaming and non-streaming have parity

A codec rule that strips `temperature` from a non-streaming request must strip it from the streaming variant too — the upstream rejects both. Both paths construct their pre-dispatch body through the same `PrepareBody`, so parity normally falls out for free. For OpenAI-family streams the codec's streaming differential (`ensureStreamUsage`, applied by `RewriteNative`'s streaming arm and by the decode door) sets `stream: true` and `stream_options.include_usage` so usage accounting survives the stream; the codec's streaming differential is the only copy — the dispatch-level duplicate was deleted with the transitional callback branch.

Parity also covers the response stream's terminal reason. The canonical `Chunk` carries a `FinishReason` field (`packages/ai-gateway/internal/providers/core/types.go`), so the reason a provider reports mid-stream — typically on the frame that carries the wire's stop token, not the closing `[DONE]`/terminal event — survives the canonical→wire re-encode instead of collapsing to a default `stop`. Each stream decoder stamps it from its wire's stop signal and each stream encoder re-emits it on the terminal frame, keeping the streamed `finish_reason` at parity with the non-streaming response's.

### Rule 7 — every prefix-list rule cites an observed 400

Each "model X rejects param Y" list is backed by an **observed** upstream 400, not speculation. The comment above each prefix-list switch records the upstream error message and the traffic trace it was seen on:

> **Dated evidence is deliberate here, and is the one carve-out from the docs' no-dates rule.** Every other page in `docs/` is written in a timeless present tense, and `doc-review` treats a date as drift. This rule's evidence is the exception: vendor behaviour changes, so *when* a 400 was observed is part of what makes the rule checkable — a rule justified by a probe nobody has repeated in a year is a rule that may already be wrong, and stripping the date removes the only thing that would show it. The carve-out is limited to the observation record for a Rule 7 / §5 quirk (the vendor's message, the model family, and the date it was probed). Prose elsewhere on this page follows the normal rule.

```go
// Observed via trace_id=<id> on claude-opus-4-7:
//   400 "<field> is not allowed for this model"
var anthropicModelRejectsSamplingParams = []string{ /* ... */ }
```

`anthropicModelRejectsSamplingParams` in `specs/anthropic/codec/codec.go` is the canonical example. Without evidence, a speculative rule silently flattens caller intent — it strips a parameter the model actually accepts and degrades behaviour with no surfaced reason.

The rule is machine-checked for the sampling-params class. `npm run check:quirk-evidence` asserts every registered quirk-rule site (`scripts/quirk-coverage.config.mjs` → `goQuirkSites`) carries an observed-400 citation in the comment block above it, and `npm run check:quirk-coverage` forces an explicit recorded decision (strips / accepts / forward-unprobed, each with its evidence) for every chat family in `tools/db-migrate/model-catalog.json`. The forced-decision property is exact-prefix-scoped: on providers with per-family rows (openai, anthropic, google-gemini, deepseek, moonshot) a new family fails CI until someone probes it and records the outcome (azure mirrors openai with deliberately family-wide prefixes, so a new azure sub-family inherits the mirrored decision); the remaining providers carry a recorded catch-all `forward-unprobed` posture, so their new families inherit it without failing — the smoke's send-the-param probes are the truth gate, and only for seeded models. When a quirk rule moves between files (e.g. into a codec), update `goQuirkSites` in the same commit.

### Rule 8 — error envelopes are reshaped to the caller's ingress format

A normalized `ProviderError` is never serialized in one hardcoded shape. `packages/ai-gateway/internal/ingress/envelope/error_envelope.go` exposes `EncodeErrorEnvelopeForIngress(ingress, upstream, pe)`, which selects the encoder for the caller's format — `encodeOpenAIErrorEnvelope`, `encodeAnthropicErrorEnvelope`, `encodeGeminiErrorEnvelope`, or `encodeResponsesAPIErrorEnvelope`. The streaming variant `encodeErrorEnvelopeForIngressForStream` wraps the JSON envelope in the SSE frame.

An Anthropic caller receives an Anthropic-shaped error even when the upstream error and its normalization were OpenAI-internal. Hand-building an OpenAI-shape error frame regardless of caller is the recurring gap this rule closes. The streaming framing details are in [sse-streaming-compliance-architecture.md](../../cross-cutting/safety/sse-streaming-compliance-architecture.md).

### Rule 9 — a codec and its transport address the same set of wire shapes

An adapter is one component with two halves. The codec decides how a canonical request becomes bytes for a given `WireShape`; `Transport.BuildURL` decides where those bytes go. Neither half is complete alone, and nothing in the type system makes them agree — `BuildURL` is a `switch` over `WireShape` with a `default` that returns `unsupported endpoint`, so a shape the codec fully supports can fall through it.

That is not a hypothetical. Cohere and Voyage both shipped a rerank codec that encoded and decoded correctly while `BuildURL` had no rerank case, so every rerank request died before a URL existed and the provider was never called at all — on two adapters, for the entire life of the endpoint. The failure is silent in exactly the wrong way: the codec tests pass, the adapter registers, and the error surfaces as a 500 that reads like an upstream fault.

The rule: for every adapter, `{shapes the codec encodes}` must equal `{shapes BuildURL returns a URL for}`. Adding a shape to one half without the other is an incomplete change, not a partial feature. A `default: return error` arm is a backstop for genuinely unknown shapes, never the place a supported shape lands.

### Rule 10 — a decoded response never reports success the upstream did not

Decoding is a translation, not a verdict. When an upstream body says the answer failed, is unfinished, or was cut short, the canonical response must carry that; a decoder may never resolve the mismatch by picking the nearest success value.

The failure this forbids is the one a caller cannot detect by any means. HTTP 200, plausible content, `finish_reason: "stop"` — nothing distinguishes it from a short but correct answer, so it is not retried, not alerted on, and not noticed until someone compares against the provider's own console.

Two shapes it takes, both of which have shipped here:

- **A non-answer status decoded into a success envelope.** `/v1/responses` reports its outcome inside a 200: `status` is one of `completed`, `incomplete`, `failed`, `errored`, `in_progress`, `queued`. There is no chat-completions `finish_reason` meaning "this failed", and the decoder mapped `failed`/`errored` onto `stop` because the canonical had no better value. `responsesTerminalFailure` in `codec_responses_response.go` now raises those — and the non-terminal `in_progress`/`queued`, which this gateway can only receive in error since it never sends `background: true` — as a `ProviderError` carrying the upstream's own `error.code` and `error.message`, before any canonical body is built. `proxy_upstream.go` surfaces a typed error as the upstream's, not as a decode fault of ours, so error attribution stays honest.
- **A stream that ends without saying why.** A decoder with no `error` arm turns an upstream error frame into a chunk with no content and no error, and the stream then terminates normally — delivering a truncated answer marked complete. Every stream decoder raises a typed `ProviderError` on an upstream error frame, and no downstream emitter terminates with a success finish value after one.

- **A terminal reason rewritten into a different one.** Collapsing a vendor's vocabulary onto the canonical's smaller one is expected — Anthropic's `end_turn` and `stop_sequence` both become `stop`, because OpenAI's `stop` genuinely covers both. Substituting a *different* fact is not. `content_filter` mapped to Anthropic's `stop_sequence`, which asserts the caller's own custom stop string was generated; the correct value is `refusal`, documented as "when streaming classifiers intervene to handle potential policy violations". Gemini's `MALFORMED_FUNCTION_CALL` and `UNEXPECTED_TOOL_CALL` mapped to `tool_calls`, sending an agent loop to read a `tool_calls[]` that is empty — both mean no usable call was produced, so they pass through raw, matching no branch and hiding nothing. Where a vendor value has no canonical counterpart at all (Anthropic's resumable `pause_turn`), it passes through rather than being flattened into a value that claims the answer is finished.

Where a status is genuinely unknown — one the vendor ships after this was written — the decoder stays permissive and decodes it as an answer. Forward compatibility is worth more than a guess, and an unrecognised status is not evidence of failure. The asymmetry is deliberate: raise on the statuses that are *known* to mean "not an answer", decode everything else.

The reverse direction has more than one implementation — the stream encoder in `canonicalbridge`, the hub-ingress translator, and each adapter's own stream decoder kept in lockstep with its codec. Per-value tests prove each does what it was written to do and are silent on whether they agree, which is how `content_filter` came to mean `stop_sequence` on one path and not another. Two properties are asserted instead: a vendor reason that has a canonical counterpart round-trips to itself, and every reverse mapper returns the same value for the same canonical input. The values that legitimately do not round-trip are enumerated with the reason each is allowed to lose fidelity, so widening that set requires saying which case it is.

### Rule 11 — an upstream error may be annotated, never replaced

A provider's own rejection is usually the most accurate description of what went wrong: it names the field, the value, and often the full set the wire accepts. Overwriting it with a gateway-authored message loses that.

What the provider cannot know is the remedy that depends on being a gateway. `/v1/responses` has no audio content part, so an `input_audio` part is rejected with `Invalid value: 'input_audio'. Supported values are: …` — observed 21 times in production on 2026-08-05, every row `gpt-audio-mini`. The message is correct and complete about the line. It is silent on the fact that the *same model* accepts audio on `/v1/chat/completions`, which is live-probed and is the whole remedy. A caller reading only the upstream text concludes the model cannot take audio and abandons a request the gateway could have served.

So the rule is append, not replace: the upstream's text survives verbatim and the gateway's sentence follows it, prefixed so it is attributable.

Two constraints keep this from becoming a place where guesses accumulate:

- **Only evidenced shapes.** §3a Rule 7 already forbids a rule that guesses at behaviour nobody observed, and it applies here identically — a remedy attached to an error class no one has seen is a speculative rule with a friendlier tone. `input_audio` is matched because 21 rows show it; the other content parts absent from the Responses vocabulary get nothing until a real 400 asks for it.
- **Blast radius is asserted, not assumed.** A match that widens silently sends every unrelated 400 to a line that has nothing to do with it, which is worse than no remedy. The test that pins the annotation is paired with one that pins its *absence* on neighbouring errors, and both are mutation-verified — narrowing the match reddens the first, widening it reddens the second.

### Cohere embed — `dimensions` is carried on v4 and withheld on v3

The embed codec dropped `dimensions` for every Cohere model, on the stated
grounds that Cohere models are fixed-dimension. That was true when it was
written and went stale with Embed v4.

Probed from the prod host against `api.cohere.com/v2/embed`, 2026-08-19:

| model | `output_dimension` | result |
|---|---|---|
| `embed-v4.0` | 256 / 512 / 1024 / 1536 | 200, vector exactly that wide |
| `embed-v4.0` | 128 / 384 / 768 / 999 / 2048 / 3072 / 4096 | 422 — "`[256 512 1024 1536] is supported`" |
| `embed-v4.0` | omitted | 200, 1536 |
| `embed-english-v3.0` | present at all | 400 "invalid request: output_dimension…" |
| `embed-english-v3.0` | omitted | 200, 1024 |

So v4 takes a fixed SET of widths and v3 takes no such field. The codec now
emits `output_dimension` for the v4 line and still withholds it from v3/v2, and
`embed-v4.0`'s catalog row enumerates those four widths rather than declaring a
min/max range — a range would be a lie here, because 999 is inside any plausible
bound and the wire refuses it.

This is the counterpart to the range form on the OpenAI and Gemini embedding
models, which DO accept an arbitrary width (777, 999 and 1000 all return vectors
of exactly that size). Both forms exist because the two families are genuinely
different, and describing either in the other's shape manufactures a wrong
answer: an enumeration on a Matryoshka model refuses widths the provider serves,
and a range on Cohere v4 promises widths it does not.

The failure this repaired is the worst-shaped one available. Dropping the field
did not produce an error — a caller asking `embed-v4.0` for 512 received HTTP
200 and a 1536-wide vector, which is not a clamped answer but a different object,
and it corrupts whatever index it is written into without ever surfacing.

### Rule 12 — a field we coerced is a field we own

A codec may forward a caller's block verbatim and let the provider judge it. That is honest only while the numbers in play are still the caller's.

Anthropic's extended thinking is the case that broke it. The `thinking` block was passed through on the stated grounds that the gateway does not validate it, while `max_tokens` two branches earlier is clamped by us to the model's ceiling — or filled by us entirely when the caller omitted it. Probed 2026-08-06 on `claude-haiku-4-5`: `max_tokens` 2048 / budget 1024 answers 200; 1024 / 1024 and 1024 / 2048 both answer `max_tokens must be greater than thinking.budget_tokens`; 1024 / 512 answers `budget_tokens: Input should be greater than or equal to 1024`. So the two fields are coupled, and a caller who sent a consistent pair could have it made inconsistent by our clamp and then be handed a 400 describing a request they never sent.

(The same probe settles a related question: `max_tokens` must exceed the thinking budget, so with thinking enabled it is the **total**, not an output-only allowance. There would be no reason for the constraint otherwise.)

The rule: once a codec coerces one side of a coupled pair, it owns the pair. Reconcile before dispatch rather than forwarding a combination we assembled.

Two constraints on how:

- **Repair toward the caller's intent.** Lowering the thinking budget to fit under the cap preserves what they asked for — thinking, inside the cap they set. Raising `max_tokens` instead would push past the ceiling the clamp exists to respect, trading one 400 for another.
- **Refuse when the coercion cannot be made consistent**, and say why in our own words. When the cap cannot house the 1024 floor, thinking is impossible within it; our error names the cap and the floor together, where the provider's can only report whichever bound it checked first.

Every repair is recorded as a coercion so it reaches `x-nexus-coerced` — a silent adjustment to an answer-affecting field is the failure §3a Rule 10 is about, wearing different clothes.

**The rule does not generalise across wires, and the probes say so.** The obvious next move after fixing Anthropic is to apply the same reconciliation to Gemini and OpenAI. Both were probed on 2026-08-06 and neither wants it:

- **Gemini** (`gemini-2.5-flash`) accepts every pair. `maxOutputTokens` 1024 with `thinkingBudget` 1024 answers 200 where Anthropic 400s; so does a budget of 2048 against a 1024 cap, and so does a budget of 128 — there is no coupling and no floor. Reconciling here would invent a constraint the wire does not have and reject requests Google accepts.
- **OpenAI** has no numeric pair to reconcile: `reasoning_effort` is `low`/`medium`/`high`, not a budget. There is a real coupling — `gpt-5` with `max_completion_tokens: 16` returns a hard 400, not a truncated 200, because reasoning consumes the budget first — but that number is the *caller's*. Our clamp only lowers to the model ceiling and our fill only writes it, so no coercion of ours can manufacture the undersized cap. Rule 12 owns the fields we changed; this one we did not.

Three wires, three different answers. Probe each before writing a rule for it.

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

A codec fills protocol-required fields the caller omitted, so an OpenAI-shaped request reaches a stricter upstream without a 400. The canonical example is Anthropic's `max_tokens`: Anthropic rejects a request that omits it, while OpenAI / Gemini / DeepSeek all accept its absence (verified against the live APIs). Other wires that also host Claude models (Bedrock, Vertex) impose the same requirement, but satisfy it in their own adapters; this section covers the OpenAI-shape path the Anthropic codec owns.

The ceiling comes from the model catalog, on `CallTarget.MaxOutputTokens`, never from a table inside the adapter. Two reasons, both load-bearing:

- **The catalog already owns and publishes the fact.** It is the same number `/v1/models` advertises. A private copy in the codec is a second source that drifts from the advertised one, and a caller that trusts our advertised ceiling and echoes it back must never be rejected by the very cap we published.
- **Model-name prefixes cannot express it.** A single `claude-opus-4-` prefix spans several different ceilings across its members (per the catalog seed — e.g. opus-4-1 far below opus-4-7). Any prefix rule here is wrong for some model in the family — this is the one fact-shape §3a Rule 7's "cite the observed 400" cannot rescue, because the rule is not about a wire quirk but about per-row data the catalog is the source of truth for.

The codec reads the ceiling for both of its branches: it clamps an over-ceiling caller down to it, and synthesizes it when the caller omitted the field. A row with no ceiling (NULL column → 0) disables both: no clamp, and a conservative floor for the fill.

**The ceiling reaches the codec via the routing snapshot** (`routing/core.RoutingTarget.MaxOutputTokens`), not only via the executor's resolved target. The proxy's cache stage prepares the primary target's upstream body from the snapshot alone, and that body serves targets[0]'s first attempt — the only attempt most requests ever make; the executor re-prepares the body from a freshly resolved `CallTarget` only on retry or failover (it resolves the target every attempt for credentials/URL, but reuses the cache-stage body on the first). Both hydration sites populate the field (`routing.lookupTarget` for rule-routed targets, `resolveNoMatchPassthrough` for the passthrough fallback), and the executor's resolver (`provtarget.PgResolver`) reads the same catalog column, so first-attempt and re-prepared bodies agree. Dropping it at any hop is silent: the clamp stops firing (upstream 400) and the fill drops to the floor (truncated completion, no error).

This is the adapter-fill pattern: the adapter that owns the wire supplies the protocol default rather than forcing the caller — or an admin config knob — to know each provider's required fields. Backstops live in the codec (Rule 3) and apply to streaming and non-streaming alike (Rule 6). Both the parameter-removal rewrites (temperature / top_p / top_k) and the `max_tokens` clamp/fill are recorded in the `rewrites` list, so the handler stamps `x-nexus-coerced` and the applied cap is observable in `traffic_event`.

The **OpenAI-family identity codec applies the same ceiling clamp** — `max_tokens` / `max_completion_tokens` over `CallTarget.MaxOutputTokens` are lowered to the ceiling (upstream otherwise 400s "max_tokens is too large: N. This model supports at most M ...") on both chat doors, streaming and non-streaming, recorded as `<field>→<M>_model_max`. It is generic (clamp to the model's own limit, not a per-model quirk, so no prefix list) and — unlike Anthropic — **clamp-only, never fill**: OpenAI treats `max_tokens` as optional, so an absent value is left absent rather than defaulted, and an unknown ceiling (`MaxOutputTokens<=0`) is a no-op. The clamp reaches every OpenAI-compatible sibling (DeepSeek, Moonshot, Azure, …) that constructs from the identity codec.

## 6. Usage parsing & translation

Every codec's `DecodeResponse` returns canonical token accounting in `DecodeResult.Usage`. Extraction is centralized: `core.ExtractUsage(raw, wireFormat)` (`packages/ai-gateway/internal/providers/core/usage_extractor.go`) parses the upstream body through the shared Tier-1 normalizer for that wire format and returns the canonical `Usage`. Codecs delegate here instead of each carrying their own alias-chain logic.

Usage is normalized to the OpenAI convention so downstream cost, analytics, and audit never branch on provider:

- `PromptTokens` = uncached input + cache-read + cache-creation. The Anthropic normalizer folds its raw `input_tokens` (uncached only) and cache tokens into this total, so a caller reading canonical usage must not subtract cache tokens again.

  **The fold is a round trip, and the return leg is the Anthropic egress's job.** Anthropic's wire is additive — `input_tokens` counts only the tokens that were neither read from nor written to the cache, and the two cache counters stand beside it as components a client sums — so `/v1/messages` has to unfold what the normalizer folded. Writing the canonical total straight into `input_tokens` while also emitting both counters made the three overlap and a client summing them over-counted by the entire cached prefix; measured in production, a request whose real uncached input was 12 tokens was reported as `input_tokens=12936` beside `cache_creation_input_tokens=12924`. The conversion has ONE implementation (`ToAnthropicCounters` in `specs/anthropic/ingress`), called by both the non-streaming egress and the streaming transcoder, because it was written twice and only one copy converted — the same request answered differently depending on whether the caller set `stream`. An upstream whose counters exceed the total it also reported is contradicting itself: no additive triple represents that, so the total is preserved as `input_tokens`, the counters that cannot be reconciled are omitted rather than invented, and the contradiction is logged per model.
- `CompletionTokens` follows the OpenAI convention (for Gemini, candidates + thoughts).
- OpenAI-compatible wires share one normalizer that resolves the cached-token alias chain (DeepSeek `prompt_cache_hit_tokens`, Moonshot `prompt_cache_tokens`, Responses-API top-level `input_tokens` / `output_tokens`).

Cache-token detail surfaces as `CacheReadTokens` / `CacheCreationTokens` on the normalized usage, and on the canonical body it lives in `usage.prompt_tokens_details` — `cached_tokens` (the read side, which OpenAI defines) and `cache_creation_tokens` (the write side, which it does not) side by side. That is a canonical field the client is meant to receive, which is why it is not also copied into the extension namespace: a response-side extension key must be a carrier some egress converter consumes (Rule 4), and this one has a canonical home the Anthropic egress converter reads directly. The full normalize contract is in [normalization-architecture.md](normalization-architecture.md).

## 7. Prompt-cache handling

Anthropic `cache_control` is not a separate canonical field. On the passthrough path it rides inside the `messages` content; on the cache-prep path the gateway can inject cache markers before upstream dispatch. On the response side, the cache token counts the upstream reports are parsed by the usage path (§6) and preserved on canonical usage (`CacheReadTokens` / `CacheCreationTokens`) and in `usage.prompt_tokens_details`. The marker mechanism, cache semantics, hit classification, and cost impact are owned by [prompt-cache-architecture.md](prompt-cache-architecture.md); an adapter's obligation is to preserve cache markers and report the cache tokens accurately.

**Preserving the marker means preserving it across the PROJECTION, which is where it was lost.** A caller sets `cache_control` on a content part, and the canonical→Anthropic content translation rebuilds each part from named fields — so a marker that is not explicitly carried is dropped, silently, with a 200. The wire-rewrite injector's contract to respect an explicit caller marker therefore held only on the same-spec passthrough: a caller on the OpenAI wire routed to an Anthropic model had their marker rebuilt away and paid full price for a prefix they had asked to cache. Every emitted block kind carries it now. The general rule this instance illustrates is Rule 1's: **a projection drops what it does not name**, so a field that matters has to be named in every block the projection can emit, not just the one the first caller happened to use.

Because cache classification depends on the usage parse, every ingress (chat, responses, messages, gemini) must exercise prompt-cache in the gateway smoke — a cross-ingress asymmetry, where one ingress reports cache tokens and another silently drops them, is the failure this guards against (§11).

## 8. Reuse across services

The provider adapter (codec) handles the gateway's outbound provider calls. The request/response **parsing** it relies on for usage and normalized text is not gateway-specific: it lives in `packages/shared/transport/normalize`, and the AI Gateway, Compliance Proxy, Agent, and Hub audit pipeline all import the same `normalize/core` + `normalize/codecs`. `core.ExtractUsage` is the gateway's entry into that shared layer.

The consequence: the same upstream response yields byte-identical canonical usage whether the gateway saw it on a forwarded call, the compliance proxy saw it on intercepted HTTPS, or the agent saw it on a client's outbound traffic. Adding a usage or text field for a provider means extending the shared normalizer once, not per service. The interception-side detail (Tier-1 traffic adapters, Tier-2 detectors) lives in the compliance-proxy architecture docs; the shared normalize contract is in [normalization-architecture.md](normalization-architecture.md).

## 9. Per-adapter walkthrough

`specs/anthropic/` is the full example of an own-wire adapter:

- `spec.go` — assembles the `AdapterSpec` (Format + Transport + SchemaCodec + StreamDecoder + ErrorNormalizer).
- `transport.go` — builds the Anthropic URL and applies the API-key + version headers.
- `codec/` — canonical↔Anthropic Messages translation, including the per-model prefix-lists and the catalog-driven `max_tokens` clamp/fill (§5).
- `stream/` — decodes the Anthropic SSE event stream into canonical chunks.
- `errors/` — maps Anthropic's `{"type":"error","error":{...}}` envelope to canonical `ProviderError` codes.
- `ingress/` — the Nexus `/v1/messages` ingress handler that turns an Anthropic-format request into canonical.

Adapters fall into three structural tiers:

| Tier | Shape | Members |
|---|---|---|
| Own wire + Nexus ingress | `codec/` `stream/` `errors/` `ingress/` subpackages | `anthropic`, `gemini` (and `openai`, the canonical/identity codec, with `codec/` `errors/` `responses/` `rewrites/` `stream/`) |
| Own wire, flat codec, no Nexus ingress | flat `codec.go` / `stream.go` / `errors.go` (+ `embed_*.go` for embeddings) | `bedrock`, `cohere`, `replicate`, `voyage` |
| Own wire, codec subpackage | `spec.go` + `transport.go` + a `codec/` subpackage (no stream/errors) | `glm` |
| Family reuse, own transport | `spec.go` + `transport.go`, borrowing the family codec | `azure` (`openai.NewIdentityCodec(openai.OpenAIContract())` — the same wire-rule contract as OpenAI itself), `vertex`, `minimax` |
| Family reuse, borrowed transport | `spec.go` only — reuses `openai.NewTransport()` + `openai.NewIdentityCodec(Contract{})` (the contract argument is REQUIRED: a sibling states its per-model rules, or their absence, on its own wiring line — no default to inherit) | the OpenAI-compatible `specs/compat/*` adapters (`fireworks`, `groq`, `huggingface`, `mistral`, `perplexity`, `together`, `xai`); `moonshot` adds `rewrites.go` carrying its fixed-temperature contract, `deepseek` its thinking-mode structural rules |

A family-reuse adapter exists because the provider speaks an existing wire and only differs in endpoint and auth — it either supplies its own `Transport` (and borrows the family codec) or reuses the family transport outright, rather than writing a codec of its own.

## 10. Adding a new adapter

Use the `add-provider-adapter` skill for the full procedure. The wiring touch points:

1. Define the `AdapterSpec` (Format + Transport + SchemaCodec + StreamDecoder + ErrorNormalizer). An OpenAI-family adapter constructs `openai.NewIdentityCodec(<contract>)` — per-model wire rules are `FieldRule` (body-root) or `StructuralRule` (message-level) entries in that contract, each with its observed-400 evidence. There is no dispatch-level rewrite callback.
2. Map the new `Format` in `chatWireShapeForFormat` / `embeddingsWireShapeForFormat`, or accept the OpenAI-family default for OpenAI-shape-compatible providers.
3. Add a `typology.WireShape` constant if the adapter speaks a non-OpenAI wire.
4. Add the ingress rule to `packages/shared/transport/typology/defaults.go` if a Nexus ingress path delivers requests in that wire shape.
5. Populate `RequestShapes` only with shapes backed by a captured 200 from the real upstream endpoint.

Run `/adapter-conformance-check` before completion to verify the adapter against Rules 1-8.

## 11. Testing an adapter

A new or changed adapter is validated at four levels:

- **Unit tests** — table-driven codec tests for `EncodeRequest` / `DecodeResponse`: canonical↔wire round-trips, each prefix-list rule, the backstop fill (§5), and usage extraction (§6). Each Go package holds ≥95% statement coverage.
- **Round-trip equivalence** — the shape-conversion test of record (§3): the double round-trip `A → canonical → B → canonical → A` must return a semantically-equal `A` for every routable shape pair (`TestShapeRoundTripIdentity`). A new ingress/target adapter adds itself to the standard's shape list in the same PR.
- **Conformance** — `/adapter-conformance-check` audits the codec against §3a Rules 1-8 (per-adapter logic that leaked into the dispatcher, missing canonicalize-before-encode, error envelopes that bypass the helper, prefix-lists without observed-400 evidence, per-model rules that leaked out of the owning codec's `Contract`).
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
