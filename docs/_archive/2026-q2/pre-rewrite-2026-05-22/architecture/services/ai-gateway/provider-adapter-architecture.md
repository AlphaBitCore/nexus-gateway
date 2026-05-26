---
doc: provider-adapter-architecture
area: service
service: ai-gateway
tier: 1
updated: 2026-05-20
---

# Provider Adapter Architecture

> **Tier 1 architecture doc.** Read this before touching `packages/ai-gateway/internal/providers/**`, `packages/shared/traffic/adapters/**`, any format translator (`wireformat/`, `canonicalbridge/`), the streaming session implementations, or any token-field stamp site in `proxy.go` / `proxy_cache.go`. The canonical payload that adapters produce is consumed by the router (`routing-architecture.md`). The decisions on streaming compliance live in `hook-architecture.md`.
>
> **Scope note.** This doc is the **chat-specific specialisation** of the per-endpoint adapter pattern. For embeddings, image generation, TTS / STT, video generation, batch / async jobs — and for the generalised pattern that subsumes all of them — read `endpoint-typology-architecture.md` (Tier 1) **first**. Rules 1-7 in §3a below generalise to every endpoint per `endpoint-typology-architecture.md` §4.4; the doc you are reading now stays the authoritative source for chat-completions and responses-api specifics.

Nexus speaks **one canonical format per endpoint internally** and **many provider wire formats externally**. The codec set under `packages/ai-gateway/internal/providers/specs/` ships **20 first-class adapter codecs** — 11 native (OpenAI, Anthropic, Azure, Bedrock, Cohere, Gemini, GLM, MiniMax, Replicate, Vertex, Voyage) plus 9 OpenAI-compatible under `specs/compat/` (DeepSeek, Fireworks, Groq, HuggingFace, Mistral, Moonshot, Perplexity, Together, xAI). Consumer-surface and IDE traffic identification — Cursor, ChatGPT-web, Claude-web, Copilot, etc. — is a **separate concern** handled by 49 traffic adapters under `packages/shared/traffic/adapters/{api,web,ide,generic}/` (20 api + 22 web + 6 ide + 1 generic); those identify and normalise traffic for audit, they do not act as gateway upstreams. For chat / responses, canonical = OpenAI chat-completions shape. Adapters do the translation in both directions. For non-chat endpoints, see `endpoint-typology-architecture.md` §2 for the canonical-selection table.

---

## 1. The Adapter interface

Adapters are composed declaratively. Each provider subpackage under `packages/ai-gateway/internal/providers/specs/<name>/` constructs an `AdapterSpec` from four parts; the generic `specAdapter` wrapper turns the spec into the runtime `Adapter` the rest of the gateway calls.

```go
// internal/providers/core/spec.go
type AdapterSpec struct {
    Format             Format                                       // wire family ("openai", "anthropic", ...)
    Transport          Transport                                    // BuildURL + ApplyAuth + Do + Probe
    SchemaCodec        SchemaCodec                                  // canonical ↔ wire translation
    StreamDecoder      StreamDecoder                                // wraps SSE / chunked frames into a StreamSession
    ErrorNormalizer    ErrorNormalizer                              // upstream 4xx/5xx → canonical ProviderError
    PassthroughRewrite func(payload map[string]any, modelID string) []string // optional per-model wire-quirk hook
    RequestShapes      []string                                     // "chat-completions" (default), "responses-api"
}

// internal/providers/core/spec.go
type SchemaCodec interface {
    EncodeRequest(endpoint Endpoint, canonicalBody []byte, target CallTarget) (EncodeResult, error)
    DecodeResponse(endpoint Endpoint, nativeBody []byte, contentType string) (DecodeResult, error)
}

type StreamDecoder interface {
    Open(r io.ReadCloser, endpoint Endpoint) (StreamSession, error)
}
```

And the runtime contract every adapter exposes (defined by the generic wrapper, not re-implemented per provider):

```go
// internal/providers/core/adapter.go
type Adapter interface {
    Format() Format
    SupportsShape(shape string) bool
    Execute(ctx context.Context, req Request) (*Response, error)
    Probe(ctx context.Context, target CallTarget) (*ProbeResult, error)
    PrepareBody(req Request) ([]byte, []string, error)
    ExecuteWithBody(ctx context.Context, req Request, body []byte, rewrites []string) (*Response, error)
}
```

- `SchemaCodec.EncodeRequest` is the **canonical → wire** path. `canonicalBody == nil` means the caller is already in wire shape (passthrough fast path) and the codec returns a zero `EncodeResult`.
- `SchemaCodec.DecodeResponse` is the **wire → canonical** path. After E58-S0 every codec delegates to `canonicalbridge.DecodeViaShared`, which calls the matching `shared/transport/normalize/codecs/<format>` Tier-1 normalizer; only encoding stays in the `spec_*/` package (see §3a Rule 8).
- `StreamDecoder.Open` wraps the upstream `io.ReadCloser` and yields canonical chunks via `StreamSession`. SSE / chunked HTTP / Gemini's `[DONE]` sentinel are absorbed here.
- `Adapter.PrepareBody` is the pure-function part of `Execute` up to but excluding the network call — produces the wire bytes plus the `x-nexus-coerced` rewrite list. The cache layer calls `ExecuteWithBody` on a miss so `PrepareBody` runs exactly once per request.
- `PassthroughRewrite` is the per-adapter hook for same-shape passthrough (e.g. `spec_openai` strips temperature/top_p and renames max_tokens for gpt-5 / o-series; `spec_moonshot` strips temperature for kimi-k2.5/k2.6). It mutates the OpenAI-shape payload in place and returns the list of `"<from>→<to>"` rewrites. Per §3a Rule 3, per-model wire quirks live next to the adapter that talks to that wire — never in `spec_adapter.go`.
- `RequestShapes` declares which native shapes this adapter serves (`"chat-completions"`, `"responses-api"`). Empty defaults to `["chat-completions"]`. `canonicalbridge.TargetNativelySupportsShape` consults this to choose same-shape passthrough vs cross-format canonical bridge.

Text extraction for hooks does not live on the adapter spec. Hook content evaluation pulls canonical prompt / completion text from `shared/transport/normalize/codecs/<format>` (request side) and from the canonical `DecodeResponse` output (response side); compliance-proxy / agent reuse the same Tier-1 normalizers via `shared/traffic/adapters/`.

## 2. Traffic Adapter Registry

The traffic adapter registry is laid out by surface family under `packages/shared/traffic/adapters/`:

- `api/<vendor>/` — programmatic API surfaces (`openai`, `anthropic`, `gemini`, `azure`, `bedrock`, `vertex`, `cohere`, `deepseek`, `fireworks`, `glm`, `groq`, `huggingface`, `minimax`, `mistral`, `moonshot`, `perplexity`, `replicate`, `together`, `voyage`, `xai`).
- `web/<host>/` — browser / desktop client surfaces (`chatgptweb`, `claudeweb`, `geminiweb`, `googleaistudioweb`, `anthropicconsoleweb`, `openaiplatformweb`, `copilotmsweb`, `m365copilotweb`, `githubcopilotweb`, `huggingchatweb`, `grokweb`, `perplexityweb`, `chatglmweb`, `deepseekweb`, `kimiweb`, `mistralweb`, `poeweb`, `characterweb`, `youweb`, `boltweb`, `devinweb`, `v0web`).
- `ide/<tool>/` — IDE / coding-agent surfaces (`cursor`, `githubcopilot`, `codeium`, `tabnine`, `continuedev`, `replitai`).
- `generic/generic/` — the `generic-jsonpath` fallback adapter.

There is **no per-provider `register.go` convention**. Every adapter is enumerated by the central `packages/shared/traffic/adapters/builtins.go` (which `init()`s the registry) plus the `manifest.json` sibling that ships catalog metadata. The registry exposes:

- `Lookup(adapterID)` — by exact id.
- `LookupByHost(host)` — by domain (for compliance-proxy / agent ingress).
- `LookupByContentType(ct)` — for protocol detection.
- `List()` — for admin UI dropdowns.

Adapter ids are referenced as constants exported by each adapter package (e.g. `openai.AdapterID`) and registered in `builtins.go`; the registry indexes the union for the lookups above.

## 3. Canonical bridge + wire format

Two helper packages sit between adapter implementations and the rest of the gateway:

- **`packages/ai-gateway/internal/execution/canonicalbridge/`** — defines the canonical structs (`Canonical{Request,Response}`), the message format, the tool-call shape. Every codec normalises into / denormalises from these types. Hosts `IngressChatToCanonical` (request side) and `ResponseCanonicalToIngress` (response side); also dispatches `DecodeViaShared` into the matching `shared/transport/normalize/codecs/<format>` Tier-1 normalizer (E58-S0).
- **`packages/ai-gateway/internal/execution/wireformat/`** — provides primitives for OpenAI-shape, Anthropic-shape, Gemini-shape encoding (since most codecs reuse one of these three families).

Marker preservation: when round-tripping (vendor → canonical → vendor for the same provider), the canonical form carries a `_nexus_markers` field that captures fields we don't represent in canonical (provider-specific metadata, vendor-extension headers). The denormalizer reattaches these markers on egress. See `docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md` for the marker contract.

## 3a. Canonical = OpenAI spec; per-adapter owns its own compatibility (binding)

This is a load-bearing architectural rule. Violations have caused real prod incidents (claude-opus-4-7 temperature 400, OpenAI gpt-5.x max_tokens 400 when called via Anthropic ingress, …). Apply it to every adapter PR.

**Rule 1 — Canonical format is OpenAI chat-completions shape.** All internal flow (router input, cache key, hook input, audit envelope, request lineage) sees the canonical form. The canonical fields are exactly OpenAI's: `model`, `messages[]`, `max_tokens` / `max_completion_tokens`, `temperature`, `top_p`, `top_k`, `stream`, `stop`, `response_format`, `tools[]`, `tool_choice`, `parallel_tool_calls`, `metadata`, `stream_options`. New canonical fields require an architecture-doc PR — adapters do not add canonical fields unilaterally.

**Rule 2 — Each non-OpenAI adapter owns its full bidirectional translation.** When you add an Anthropic / Gemini / Bedrock / Cohere / Replicate codec, its `SchemaCodec.EncodeRequest` does canonical → wire and its `DecodeResponse` does wire → canonical. The OpenAI side stays pure (identity codec) — it never carries case-statements for "this came from Anthropic so do X". The asymmetry is intentional: OpenAI shape is the bus, and every other shape adapter wires itself into the bus.

**Rule 3 — Per-model wire quirks (HTTP 400 deprecations, parameter renames, mandatory clamping) belong in the adapter that talks to that wire.** Concretely:

- Anthropic's "claude-opus-4-7 deprecates temperature/top_p/top_k" lives in `packages/ai-gateway/internal/providers/specs/anthropic/codec/codec.go` (`anthropicModelRejectsSamplingParams`, function at line 308).
- Anthropic's "claude-4.x rejects temperature + top_p together" lives in `packages/ai-gateway/internal/providers/specs/anthropic/codec/codec.go` (`anthropicModelRejectsTempTopPTogether`).
- OpenAI's "gpt-5.x / o-series rename max_tokens → max_completion_tokens and strip temperature/top_p" lives in `packages/ai-gateway/internal/providers/specs/openai/rewrites/rewrites.go` (`ApplyReasoningRewrites`, `IsReasoningModel`), wired in via `AdapterSpec.PassthroughRewrite`.
- Moonshot's "kimi-k2.5/k2.6 require temperature=1" lives in `packages/ai-gateway/internal/providers/specs/compat/moonshot/rewrites.go` (`ApplyRewrites`, `IsFixedTempModel`), wired in via `AdapterSpec.PassthroughRewrite`.

When a new family ships an HTTP-400-deprecation, find the adapter that owns its wire and add the prefix-rule there. Do **not** add cross-adapter case-statements in `spec_adapter.go`'s shared helpers — that creates the wrong dependency direction.

**Rule 4 — `nexus.ext.<provider>.<key>` is the canonical extension namespace.** Fields that have no clean OpenAI mapping (Anthropic's `thinking` block, Gemini's `thinkingConfig`, Anthropic's `cache_creation_input_tokens`, Bedrock's `anthropic_version`) ride along inside `nexus.ext.<provider>.<key>` on the canonical body. The package is `providers/canonicalext/`; use `canonicalext.Get`, `canonicalext.Set`, `canonicalext.ScanUnsupported`, `canonicalext.WarnOnce`. Adapters that observe an unsupported canonical field emit a one-shot WARN so operators see drift between the canonical surface and the codec.

There is also a small reserved sibling namespace `nexus.<flag>` (no `.ext.<provider>` prefix) for **cross-provider behaviour controls** that ride on the canonical body and apply regardless of which target wins routing. Today's members:
  - **`nexus.dry_run`** (`bool`, default `false`, E58-S3) — when `true`, the gateway short-circuits at the cache-lookup boundary, runs `packages/ai-gateway/internal/execution/estimator` against the resolved target, and returns a per-ingress success-shape response with empty content + populated `usage`. Helpers: `canonicalext.IsDryRun`, `canonicalext.SetDryRun`. Strict bool typecheck (`gjson.True`) so a SDK typo like `"true"` (string) doesn't silently trip a dry-run.

Reserved-namespace additions are admin-only — opening a new `nexus.<flag>` key requires an SDD update and a `docs/users/api/openapi/` schema bump because every ingress codec must preserve it through canonicalization unchanged.

**Rule 5 — `SchemaCodec.EncodeRequest` contract: input is canonical (or codec-empty for passthrough); output is target wire.** Callers that have an ingress-format body (Anthropic /v1/messages, Gemini :generateContent) **must canonicalize first** before invoking `adapter.PrepareBody` / `SchemaCodec.EncodeRequest`. The canonicalization helper is `canonicalbridge.IngressChatToCanonical(ingress, body, target)`. Skipping canonicalization causes the OpenAI identity codec to forward the ingress body verbatim and the upstream returns 400 (or worse, parses partially and produces gibberish).

**Rule 6 — Both streaming and non-streaming are in scope.** A codec rule that strips `temperature` from a non-streaming request must also strip it from the streaming variant — the upstream rejects both. The streaming session's pre-dispatch body construction goes through the same `PrepareBody` path, so this typically falls out for free; the gap usually appears on error-frame construction (response side) when the gateway hand-builds an SSE error and forgets the ingress format. The Section 9.5 SSE-error rule covers this.

**Rule 7 — Add empirical evidence to every prefix-list.** Every "model X rejects param Y" rule must be backed by an observed 400 (logged trace_id or direct test call). Speculative rules cause silent flattening of caller intent. The comment above each prefix-list switch documents the observation (date + error message). See `anthropicModelRejectsSamplingParams` in `packages/ai-gateway/internal/providers/specs/anthropic/codec/codec.go:308` for the canonical comment style.

**Rule 8 — Decoding goes through `shared/normalize`; only encoding stays in `spec_*/` (E58-S0).** After E58-S0, every `SchemaCodec.DecodeResponse` and streaming session's response parser delegates to `canonicalbridge.DecodeViaShared`, which calls the matching `shared/normalize/<format>` Tier-1 normalizer. The wire-emission side (`EncodeRequest`, `PrepareBody`, the per-model strip / rename / clamp rules in Rules 1-7) stays in each `spec_*/`. Adding a new field to upstream's response shape = add the alias in `shared/normalize/<format>.go` and every consumer (ai-gateway, compliance-proxy, agent, Hub audit) picks it up at once. See `normalization-architecture.md` § "Ai-gateway codec delegation (E58-S0)" for the full delegation contract.

### How a request flows under Rules 1-7

```
┌─ client sends Anthropic /v1/messages with model=claude-sonnet-4-6, temperature=0.3, top_p=0.9
│
├─ ingress detected: BodyFormat=FormatAnthropic, EndpointType="chat/completions"
│
├─ routing rule: target = Provider(anthropic) Model(claude-sonnet-4-6)         ┃  same-family
│                                                                             ┃  → executor's
├─ executor: bridge.IngressChatToWire(Anthropic, Anthropic, body, target)     ┃  fast path
│   = body unchanged (Rule 5 doesn't apply on same-shape passthrough)         ┃  (no canonical)
│                                                                             ┃
├─ specAdapter(anthropic).Execute:
│   PrepareBody sees BodyFormat==Format → passthrough → rewritePassthroughModel
│   (target rewrite hook for Anthropic lives in
│    internal/providers/specs/anthropic/codec/codec.go via codec path
│    rather than the passthrough path)
│
└─ upstream POST https://api.anthropic.com/v1/messages
```

```
┌─ client sends Anthropic /v1/messages with model=remapped, ...
│
├─ ingress: BodyFormat=FormatAnthropic
│
├─ routing rule: target = Provider(openai) Model(gpt-5.4)                     ┃  cross-family
│                                                                             ┃  routing
├─ executor: bridge.IngressChatToWire(Anthropic, OpenAI, body, target)        ┃
│   = IngressChatToCanonical(Anthropic, body)   ← Rule 5: canonicalize
│   = spec_openai.identityCodec.EncodeRequest(canonical, target)              ┃
│   = canonical body verbatim                                                  ┃
│   = req.Body=canonical, req.BodyFormat=FormatOpenAI                          ┃
│                                                                             ┃
├─ specAdapter(openai).Execute:
│   PrepareBody sees BodyFormat==Format (OpenAI==OpenAI) → passthrough
│   → applyOpenAIReasoningRewrites for gpt-5.x: max_tokens→max_completion_tokens
│
└─ upstream POST https://api.openai.com/v1/chat/completions
```

The second flow above is the critical one: every cross-format ingress passes through the OpenAI canonical bus. The non-OpenAI adapter (Anthropic in this example) handled the canonicalization in step 2. The OpenAI adapter (target side) handled its own per-model rewrite in step 3. Neither side carries logic about the other. **This is the architectural rule.**

### Ingress format ≠ canonical: the responses-api case

The canonical bus is OpenAI chat-completions shape (Rule 1). When OpenAI itself ships a second wire shape under the same provider — today that means `/v1/responses` — it is treated as a **new ingress format**, not a second canonical. Mechanism:

1. A new `providers.Format` constant (`FormatOpenAIResponses`) and `providers.Endpoint` constant (`EndpointResponsesAPI`) are registered, exactly like any non-OpenAI ingress.
2. Each adapter declares which shapes it natively serves via `Manifest.RequestShapes` (e.g. `["chat-completions", "responses-api"]` for `spec_openai`, `["chat-completions"]` for everyone else until empirically verified).
3. At executor entry, the canonical bridge checks `target.Manifest.RequestShapes.contains(ingress.BodyFormat-as-shape)`:
   - **Same-shape passthrough fires** → body forwarded verbatim, only model rewrite via `PassthroughRewrite`. Stateful fields (`previous_response_id`, `store`) and provider-native built-in tools ride through untouched.
   - **Cross-format canonicalization fires** → `IngressChatToCanonical(FormatOpenAIResponses, ...)` decodes Responses-shape into canonical chat-completions; executor denormalizes to target wire; `ResponseCanonicalToIngress(canonical, FormatOpenAIResponses)` re-encodes target's response into Responses output shape on the way back to the client.

**This is the architectural pattern for adding any future "OpenAI ships a third wire shape" event** (responses-api in 2026; whatever lands next): one new ingress format constant, one new codec under `spec_openai`, one capability flag on the manifest. Canonical stays put. **Adding a sibling adapter (Moonshot, Groq, …) to the `responses-api` capability list requires a real captured 200 from that sibling's `/v1/responses` endpoint** (Rule 7, binding) — otherwise speculative capability flags route real traffic to a 404/400 surface.

### Where adapter / codec / canonical bridge code lives

| Layer | What it owns | Files |
|---|---|---|
| Per-adapter codec | canonical ↔ wire for THIS adapter only; per-model wire quirks for THIS adapter only | `internal/providers/specs/<adapter>/codec/codec.go` |
| Per-adapter passthrough rewrite | per-adapter PassthroughRewrite hooks wired into `AdapterSpec` for same-shape passthrough quirks | `internal/providers/specs/<adapter>/rewrites/rewrites.go` (OpenAI), `internal/providers/specs/compat/<adapter>/rewrites.go` (Moonshot et al.) |
| canonicalbridge | request: ingress → canonical → target wire (composes codecs); response: target canonical → ingress wire | `internal/execution/canonicalbridge/bridge.go` |
| canonicalext | nexus.ext.<provider>.<key> helpers | `internal/providers/canonicalext/` |
| spec_adapter (generic) | the generic specAdapter wrapper, NOT per-adapter knowledge | `internal/providers/dispatch/spec_adapter.go` |

Anything that doesn't fit one of these slots is misplaced — the audit in §11 calls out current violations.

### Gemini batchEmbedContents per-item model field

Gemini's `:batchEmbedContents` endpoint differs from `:embedContent` (singular): each sub-request inside the `requests` array MUST carry a `model: "models/<provider_model_id>"` field, in addition to the URL-level model path. Omitting the per-item field surfaces as HTTP 400 `BatchEmbedContentsRequest.requests[i].model: model is not specified`. The codec (`packages/ai-gateway/internal/providers/specs/gemini/codec/embeddings.go`) passes `target.ProviderModelID` into `buildGeminiBatchEmbedRequest` to satisfy this. This is a §3a Rule 4 case (per-model wire quirk stays in the adapter that talks to that wire).

## 4. Streaming sessions

Most providers stream as SSE; some use chunked HTTP/2 frames (Gemini, Cursor). Streaming sessions handle:

- Incremental JSON parsing (chunked, never-fully-buffered).
- Buffer mgmt (max size, backpressure).
- Heartbeat / keep-alive frame handling.
- Graceful close (handle upstream FIN, half-open, mid-stream error).
- Per-provider quirks (Gemini's `[DONE]` sentinel, Anthropic's `event:` / `data:` split, OpenAI's role-only first chunk).

Sessions emit canonical chunks (`CanonicalChunk` — a delta diff over the canonical response). The hook pipeline (streaming stage) and the response cache both consume these.

## 5. Token-field stamp sweep (binding)

Memory `feedback_token_field_handler_sweep` is binding: adding a new usage / token field requires touching **every** stamp site, not just the obvious one. The sites (real function names in `packages/ai-gateway/internal/ingress/proxy/`):

1. `proxy.go:handleNonStream` (line 2361) — live upstream call (both non-stream and the SSE final-usage stamp share this stamper).
2. `proxy_cache.go:handleStreamHit` (line 198) — streaming response path replayed from an extract / semantic cache hit.
3. `proxy_cache.go:handleNonStreamHit` (line 270) — non-streaming response path replayed from a cache hit.
4. `proxy_cache.go:handleStreamWithSubscription` (line 471) — streaming singleflight joiner on a cache miss (writes the new entry while serving).
5. `proxy_cache.go:handleNonStreamWithSubscription` (line 772) — non-streaming singleflight joiner on a cache miss.

In addition, the post-stamp recompute helper `proxy.go:computeCacheCosts` (line 2882) re-derives the cache-token cost columns after a hit; touch it if the new field interacts with cache cost math.

Missing **the 4 cache sites** = all prod cache traffic NULL on the new column. Checklist when adding a token field:

- [ ] Add field to `Usage` struct in `internal/providers/core/types.go`.
- [ ] Add column to `traffic_event` schema (Prisma + migration).
- [ ] Stamp in `handleNonStream`.
- [ ] Stamp in `handleStreamHit` and `handleStreamWithSubscription`.
- [ ] Stamp in `handleNonStreamHit` and `handleNonStreamWithSubscription`.
- [ ] Update `computeCacheCosts` if the new field is used in cache cost derivation.
- [ ] Unit test that exercises both non-stream and stream cache paths.

## 6. Debug log fields (pre-wired)

Debug-level logs are pre-wired at four canonical points in `spec_adapter.go` and `shared/httpclient/logging.go`:

| Log message | Fields | When |
|---|---|---|
| `"upstream request body"` | `format`, `url`, `body` (first 8 KB) | Before `Transport.Do()` |
| `"upstream response headers"` | `format`, `status`, `stream`, `content_type`, `content_length`, `body_nil` | After `Transport.Do()` |
| `"upstream stream body"` | `format`, `bytes_captured`, `body` (first 8 KB) | On stream body close |
| `"outbound http"` | `url`, `status`, `req_bytes`, `resp_bytes`, `duration_ms` | After response body closed |

To activate: ensure `log.level: "debug"` in the service's `*.dev.yaml`. No code changes needed for standard body inspection. For mid-stream chunk values, add a temporary `slog.LevelDebug` log inside the relevant stream session and **remove before committing**.

## 7. Non-JSON Detector framework (Tier-2)

For non-JSON wire formats (binary protocols, Google batchexecute, Connect-RPC + protobuf), the pattern is:

- Add a `NonJSONDetector` in `packages/shared/transport/normalize/extract/detector.go` (and append it to the `NonJSONDetectors` slice in that file).
- Tier-1 adapters delegate to the same detector framework.

**Do not** write a fresh per-host adapter from scratch for a new non-JSON format. The detector framework is the canonical reusable path; bypassing it creates parallel maintenance burdens. Memory `feedback_tier2_nonjson_detector_framework` is binding.

Today's `NonJSONDetectors` (in iteration order):

- `ConnectRPCProtobufDetector` — Cursor IDE chat wire (Connect-RPC + protobuf framing).
- `BatchExecuteDetector` — `gemini.google.com` batchexecute (`f.req=` form-urlencoded JSON envelope).

Adding a new detector is preferred over duplicating a Tier-1 adapter.

## 8. Adding a new provider

Checklist:

1. Decide the wire family — OpenAI-shape, Anthropic-shape, Gemini-shape, or custom.
2. Implement the per-adapter components under `packages/ai-gateway/internal/providers/specs/<name>/`:
   - A `spec.go` that builds an `AdapterSpec` (Format, Transport, SchemaCodec, StreamDecoder, ErrorNormalizer; optional PassthroughRewrite; RequestShapes if non-default).
   - A `codec/codec.go` implementing `SchemaCodec.EncodeRequest` (canonical → wire) and `SchemaCodec.DecodeResponse` (wire → canonical via `canonicalbridge.DecodeViaShared`; see §3a Rule 8).
   - A `stream/` package implementing `StreamDecoder.Open` if streaming is supported (delegates to `shared/transport/streaming` SSE / chunked helpers).
   - A `transport.go` implementing `Transport` (BuildURL + ApplyAuth + Do + Probe).
   - An `errors/` package implementing `ErrorNormalizer`.
3. Register the adapter in `packages/ai-gateway/internal/providers/builtins/`.
4. Add the provider entry to `tools/db-migrate/seed/seed.ts` (and to prod seed if relevant).
5. Add the provider's endpoint config to the AI Gateway dev yaml.
6. Add a `test-<provider>-adapter` skill or extend `test-all` to exercise it.
7. Add the provider's models to the catalog via admin API or seed.
8. Map provider-specific error shapes to `ErrorClass` (cross-ref `error-taxonomy-architecture.md`).
9. If the provider supports prompt caching natively, wire the integration (cross-ref `prompt-cache-architecture.md`).

## 9. Response Marker Headers

Nexus emits a small set of response headers per traffic event (`x-nexus-request-id`, `X-Nexus-Routed-To`, `X-Nexus-Cache`, ...). These survive both the canonical bridge and the denormalize path; the contract is in `docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md`.

## 9.5 Error envelope contract (request errors + SSE error frames)

Errors must reach the client in **the ingress format's envelope shape**, never the upstream's native envelope when the two differ. This is the error-path twin of `canonicalbridge.ResponseCanonicalToIngress` and is binding for both non-stream and stream.

**Non-stream 4xx**: `envelope.EncodeErrorEnvelopeForIngress(ingress, upstream, *ProviderError)` (exported from `packages/ai-gateway/internal/ingress/envelope/error_envelope.go`) produces the right envelope per ingress format (OpenAI `{"error":{...}}`, Anthropic `{"type":"error","error":{...}}`, Gemini `{"error":{"code":...,"status":...}}`). Same-family passthrough preserves the raw upstream bytes so native SDKs still see all upstream-specific fields.

**Stream 4xx mid-stream or pre-stream**: the SSE error frame must also be ingress-shaped. OpenAI ingress emits `data: {"error":{...}}\n\n`. Anthropic ingress emits `event: error\ndata: {"type":"error","error":{...}}\n\n`. Gemini ingress emits the Google streaming-error JSON. The stream-side helper (`encodeErrorEnvelopeForIngressForStream`) lives in the same file and must be reused by every SSE error path (handle subscription error, broker error, mid-stream provider error).

Forbidden patterns:

- Hand-rolling `data: {"error":...}` in `proxy_cache.go` or `proxy.go` outside the helper.
- Forwarding `pe.Raw` verbatim on a cross-format path (the d914275a incident).
- Returning HTTP 200 + an empty body when the upstream 4xx'd (callers cannot distinguish from a real empty response).

## 10. Sources

- `packages/ai-gateway/internal/providers/core/` — `Adapter`, `AdapterSpec`, `SchemaCodec`, `StreamDecoder`, `ErrorNormalizer`, `Usage`, `Format`, `Endpoint`.
- `packages/ai-gateway/internal/providers/specs/` — per-adapter codec / stream / transport / error normalizer (`anthropic`, `openai`, `gemini`, `azure`, `bedrock`, `vertex`, `cohere`, `voyage`, `replicate`, `glm`, `minimax`, plus `compat/{deepseek,fireworks,groq,huggingface,mistral,moonshot,perplexity,together,xai}`).
- `packages/ai-gateway/internal/providers/canonicalext/` — `nexus.ext.<provider>.<key>` and reserved `nexus.<flag>` helpers.
- `packages/ai-gateway/internal/providers/dispatch/` — the generic `specAdapter` wrapper that composes an `AdapterSpec` into an `Adapter`.
- `packages/ai-gateway/internal/providers/target/` — `CallTarget` resolver (credential decrypt + base URL + extras).
- `packages/ai-gateway/internal/providers/builtins/` — adapter registration set baked into the binary.
- `packages/ai-gateway/internal/providers/specutil/` — shared codec utilities (`ExtractOpenAIUsage`, `ExtractCachedTokens`, `ExtractReasoningTokens`).
- `packages/ai-gateway/internal/execution/wireformat/` — OpenAI / Anthropic / Gemini wire helpers.
- `packages/ai-gateway/internal/execution/canonicalbridge/` — canonical types, `IngressChatToCanonical`, `ResponseCanonicalToIngress`, `DecodeViaShared`.
- `packages/ai-gateway/internal/ingress/envelope/error_envelope.go` — `EncodeErrorEnvelopeForIngress` + stream-shape helper.
- `packages/shared/traffic/adapters/` — per-vendor traffic adapter implementations (`api/`, `web/`, `ide/`, `generic/`) registered by `builtins.go`.
- `packages/shared/transport/normalize/extract/detector.go` — `NonJSONDetector` framework + `NonJSONDetectors` slice.
- `packages/shared/transport/normalize/codecs/` — Tier-1 normalizers consumed by `canonicalbridge.DecodeViaShared` (E58-S0).

## 11. Conformance gaps (audit target)

No open conformance gaps; append new gaps here as they arise.

| # | Gap | Rule violated | Priority | Status |
|---|---|---|---|---|

When a new gap is found: open a row with `# | description | rule | priority | open — <description>`. Each gap fix is its own PR; update the row from "open" to "closed — <one-line fix summary>" when the fix ships.

## 12. Cross-references

- `routing-architecture.md` — canonical payload feeds routing; `ResolvedRequest` triggers denormalize.
- `hook-architecture.md` — text extraction feeds hooks; streaming session feeds streaming stage.
- `error-taxonomy-architecture.md` — provider error mapping → `ErrorClass`.
- `prompt-cache-architecture.md` — provider-side cache integration.
- `compliance-pipeline-architecture.md` — compliance-proxy ingress uses `ExtractText`.
- `nexus-response-markers.md` — header contract.
