# Canonical Vs Wire Format

*Audience: contributors adding or modifying provider adapters, ingress endpoints, or anything that touches the canonical request/response shape.*

Nexus Gateway speaks one canonical format internally and many provider wire formats externally. For chat and completions traffic, canonical means the OpenAI chat-completions shape. Every non-OpenAI adapter owns its full bidirectional translation into and out of that shape; the OpenAI adapter is the identity codec. The canonical bus is what the router, cache, hook engine, cost estimator, and audit pipeline all see — none of those components know or care which provider ultimately received the request. This design keeps the N-to-M provider translation problem contained inside adapter packages and keeps the rest of the system simple. This page explains the canonical format, the eight rules governing adapter ownership (§3a), the `canonicalbridge.IngressChatToCanonical` contract, and the `nexus.ext.<provider>.<key>` extension namespace.

---

## Why canonical = OpenAI shape

The OpenAI chat-completions shape is the most widely implemented AI API surface. Most providers either natively support it or have close analogues. Designating it as the canonical bus means:

- Existing OpenAI-compatible clients work against the AI Gateway with no changes.
- New non-OpenAI adapters implement one bidirectional translation (their wire ↔ OpenAI shape) — not N-to-M translations between each provider pair.
- Every internal consumer — router, cache key derivation, hook engine, cost estimator, audit envelope — has one format to parse. Adding a new internal consumer does not require updating any adapter.
- Decoding a new upstream field (e.g., a new token type in the usage block) means updating one `shared/normalize/<format>.go` file; the AI Gateway, Compliance Proxy, Desktop Agent, and Hub audit pipeline all pick it up at once.

**Canonical fields** (OpenAI chat-completions): `model`, `messages[]`, `max_tokens` / `max_completion_tokens`, `temperature`, `top_p`, `top_k`, `stream`, `stop`, `response_format`, `tools[]`, `tool_choice`, `parallel_tool_calls`, `metadata`, `stream_options`. New canonical fields require an architecture-doc PR — adapters do not add canonical fields unilaterally.

---

## The §3a eight rules

These eight rules are binding for every provider adapter PR. They exist because violations have caused production 400 errors (claude-opus-4-7 temperature rejection, gpt-5.x `max_tokens` → `max_completion_tokens` rename when called via the Anthropic ingress).

### Rule 1 — Canonical format is OpenAI chat-completions shape

All internal flow (router input, cache key, hook input, audit envelope, request lineage) sees the canonical form. The canonical fields are exactly OpenAI's. New canonical fields require an architecture-doc PR; adapters do not extend the canonical struct unilaterally.

### Rule 2 — Each non-OpenAI adapter owns its full bidirectional translation

An Anthropic codec's `SchemaCodec.EncodeRequest` does canonical → Anthropic wire. Its `DecodeResponse` does Anthropic wire → canonical. The OpenAI adapter is the identity codec — it never carries case statements for "this came from Anthropic, so do X". The asymmetry is intentional: OpenAI shape is the bus; every other shape adapter wires itself into the bus.

### Rule 3 — Per-model wire quirks belong in the adapter that owns that wire

Concrete examples:
- Anthropic's rejection of `temperature`/`top_p`/`top_k` on claude-opus-4-7: `packages/ai-gateway/internal/providers/specs/anthropic/codec/codec.go`, function `anthropicModelRejectsSamplingParams` at line 308.
- Anthropic's rejection of `temperature + top_p together` on claude-4.x: same file, `anthropicModelRejectsTempTopPTogether`.
- OpenAI's `max_tokens` → `max_completion_tokens` rename for gpt-5.x / o-series: `packages/ai-gateway/internal/providers/specs/openai/rewrites/rewrites.go`, `ApplyReasoningRewrites`.
- Moonshot's `temperature=1` requirement for kimi-k2.5/k2.6: `packages/ai-gateway/internal/providers/specs/compat/moonshot/rewrites.go`, `ApplyRewrites`.

Never add cross-adapter case statements in `spec_adapter.go`. Every prefix-list rule must have a comment citing the observed HTTP 400 (date + error message).

### Rule 4 — `nexus.ext.<provider>.<key>` is the canonical extension namespace

Fields with no clean OpenAI mapping ride along inside `nexus.ext.<provider>.<key>` on the canonical body:
- Anthropic extended thinking: `nexus.ext.anthropic.thinking`
- Gemini thinking config: `nexus.ext.gemini.thinkingConfig`
- Anthropic cache creation tokens: `nexus.ext.anthropic.cache_creation_input_tokens`

Use `canonicalext.Get`, `canonicalext.Set`, `canonicalext.ScanUnsupported`, `canonicalext.WarnOnce` from `packages/ai-gateway/internal/providers/canonicalext/`. The `ScanUnsupported` + `WarnOnce` pair emits a one-shot log entry when the canonical surface drifts from what the codec handles — operators see the drift before it becomes a silent data loss.

There is also a reserved sibling namespace `nexus.<flag>` (no `.ext.<provider>` prefix) for cross-provider behavior controls that apply regardless of which target wins routing. Today's only member is `nexus.dry_run` (bool, default false): when `true`, the gateway runs the full pipeline through routing and cache lookup, skips the upstream call, and returns an estimator-based response. Reserved-namespace additions require an SDD update and an OpenAPI schema bump.

### Rule 5 — `SchemaCodec.EncodeRequest` input is canonical; callers canonicalize first

When the ingress format differs from the target provider's wire format, the caller MUST invoke `canonicalbridge.IngressChatToCanonical(ingress, body, target)` before calling `adapter.PrepareBody` or `SchemaCodec.EncodeRequest`. Skipping this step causes the OpenAI identity codec to forward the Anthropic-shape body verbatim to `api.openai.com`, which returns HTTP 400.

Same-shape passthrough is the exception: when the ingress format matches the target's native wire format (e.g., an Anthropic ingress routed to an Anthropic provider), the executor skips canonicalization and forwards the body with only per-model rewrites applied via `AdapterSpec.PassthroughRewrite`.

### Rule 6 — Both streaming and non-streaming are in scope

A codec rule that strips `temperature` from a non-streaming request must also strip it from the streaming variant. The streaming session's pre-dispatch body construction goes through the same `PrepareBody` path, so this typically falls out for free. The gap usually surfaces in error-frame construction: when the gateway synthesizes an SSE error frame mid-stream, it must use the ingress format's error envelope shape, not the upstream's native shape.

### Rule 7 — Every prefix-list rule needs empirical evidence

Every "model X rejects param Y" rule must be backed by an observed HTTP 400 — a logged trace_id or a direct test call. Speculative rules cause silent flattening of caller intent. The comment above each prefix-list switch documents the observation (date + error message). See `anthropicModelRejectsSamplingParams` in `packages/ai-gateway/internal/providers/specs/anthropic/codec/codec.go:308` for the canonical comment style.

### Rule 8 — Decoding goes through `shared/normalize`; only encoding stays in `spec_*/`

Every `SchemaCodec.DecodeResponse` and streaming session's response parser delegates to `canonicalbridge.DecodeViaShared`, which calls the matching `shared/transport/normalize/codecs/<format>` Tier-1 normalizer. The wire-emission side (`EncodeRequest`, `PrepareBody`, the per-model strip/rename/clamp rules) stays in each `spec_*/`.

Adding a new field to an upstream response shape means adding the alias in `shared/normalize/<format>.go` once. Every consumer — AI Gateway, Compliance Proxy, Desktop Agent, Hub audit — picks it up from the single shared codec without per-service edits.

---

## The `canonicalbridge.IngressChatToCanonical` contract

`IngressChatToCanonical` is the single call site that translates an ingress-format request body into the OpenAI canonical shape. It is the entry point for cross-format routing.

Cross-format flow example (Anthropic ingress → OpenAI target):

```
Client sends Anthropic /v1/messages with model=remapped, temperature=0.3, top_p=0.9

Ingress detected: BodyFormat=FormatAnthropic

Routing rule: target = Provider(openai) Model(gpt-5.4)

Executor:
  bridge.IngressChatToCanonical(FormatAnthropic, body)
  → canonical OpenAI chat-completions shape

  spec_openai.identityCodec.EncodeRequest(canonical, target)
  → canonical body verbatim

  ApplyReasoningRewrites for gpt-5.x:
  → max_tokens renamed to max_completion_tokens

Upstream: POST https://api.openai.com/v1/chat/completions
```

Same-shape passthrough flow (Anthropic ingress → Anthropic target):

```
Client sends Anthropic /v1/messages with model=claude-sonnet-4-6

Ingress: BodyFormat=FormatAnthropic
Target: Provider(anthropic) — same family

Executor: passthrough path (no IngressChatToCanonical)
  spec_anthropic.Execute → PrepareBody (passthrough)
  → per-model rewrite check for sampling-param deprecations
  → body forwarded with model field rewritten if needed

Upstream: POST https://api.anthropic.com/v1/messages
```

The location of the canonical bridge: `packages/ai-gateway/internal/execution/canonicalbridge/bridge.go`.

---

## `nexus.ext.<provider>.<key>` extension pattern in practice

Fields that have no clean OpenAI mapping need to survive the canonical round-trip without polluting the canonical struct definition. The extension namespace is how they travel:

**Write side (ingress → canonical).** The adapter's `hub_ingress` handler or `IngressChatToCanonical` calls `canonicalext.Set` to write the extension field onto the canonical body JSON.

**Read side (canonical → wire).** The codec's `EncodeRequest` calls `canonicalext.Get` to recover the extension field and inject it into the wire body.

**Marker preservation.** When round-tripping (same provider → canonical → same provider), the canonical form carries a `_nexus_markers` field that captures provider-specific metadata not represented in canonical. The denormalizer reattaches these markers on egress, so the upstream sees the same fields it would have seen without the gateway.

---

## Where the translation code lives

| Layer | Owns | Package |
|---|---|---|
| Per-adapter codec | canonical ↔ wire for that adapter; per-model wire quirks | `internal/providers/specs/<adapter>/codec/codec.go` |
| Per-adapter passthrough rewrite | same-shape quirks wired via `AdapterSpec.PassthroughRewrite` | `internal/providers/specs/<adapter>/rewrites/rewrites.go` |
| Canonical bridge | ingress → canonical → target wire; response target canonical → ingress wire | `internal/execution/canonicalbridge/bridge.go` |
| Canonical extension namespace | `nexus.ext.<provider>.<key>` and `nexus.<flag>` helpers | `internal/providers/canonicalext/` |
| Tier-1 normalizers (shared) | wire → `NormalizedPayload` used by all three traffic paths | `packages/shared/transport/normalize/codecs/<format>/` |

Any format-translation logic that does not fit one of these five slots is misplaced. The conformance gaps table in `provider-adapter-architecture.md` §11 documents the canonical slot for ambiguous placements.

## Streaming sessions

Streaming responses from providers arrive as Server-Sent Events (SSE) or chunked HTTP/2 frames. The `StreamDecoder` interface wraps the upstream `io.ReadCloser` and yields canonical `CanonicalChunk` deltas. The hook pipeline's streaming stage and the response cache both consume these chunks.

Each provider's `StreamDecoder.Open` implementation handles provider-specific framing:

- OpenAI: standard SSE with `data: {...}` lines and `data: [DONE]` terminator.
- Anthropic: SSE with `event: <type>` / `data: <json>` pairs and `event: message_stop` terminator.
- Gemini: chunked HTTP/2 JSON array frames; `[DONE]` sentinel position differs from OpenAI.
- Cursor: Connect-RPC + protobuf framing (handled by the `ConnectRPCProtobufDetector` Tier-2 non-JSON detector).

All streaming sessions emit canonical `CanonicalChunk` structs regardless of the upstream wire format. The response cache writes a sequence of these chunks and replays them on cache hits; the compliance hook runs per-chunk during the streaming compliance mode.

## Error envelope contract

Errors must reach the client in **the ingress format's envelope shape**, not the upstream's native envelope when the two differ. This is the error-path twin of the canonical bridge:

- OpenAI ingress error: `{"error": {"message": "...", "type": "...", "code": "..."}}`
- Anthropic ingress error: `{"type": "error", "error": {"type": "...", "message": "..."}}`
- Gemini ingress error: `{"error": {"code": N, "message": "...", "status": "..."}}`

The helper `envelope.EncodeErrorEnvelopeForIngress(ingress, upstream, *ProviderError)` in `packages/ai-gateway/internal/ingress/envelope/error_envelope.go` produces the correct shape. Same-family passthrough preserves the raw upstream bytes so native SDKs still see all upstream-specific error fields.

The stream-side variant `encodeErrorEnvelopeForIngressForStream` must be used for SSE error frames (mid-stream or pre-stream rejections). Hand-rolling `data: {"error":...}` inline in `proxy.go` or `proxy_cache.go` is a forbidden pattern.

## Non-chat endpoints and the canonical selection table

The canonical format choice described above applies to chat-completions and responses-API endpoints. Non-chat endpoints have their own canonical shapes documented in `endpoint-typology-architecture.md` §2:

| Endpoint type | Canonical shape | Notes |
|---|---|---|
| Chat completions | OpenAI `chat/completions` | Primary canonical bus |
| Responses API | OpenAI `responses` | New ingress format; same-shape passthrough to OpenAI-native; canonicalized for cross-provider routing |
| Embeddings | OpenAI `embeddings` | Each adapter maps its own embedding request to the canonical shape |
| Image generation | OpenAI `images/generations` | (future; same pattern) |
| TTS / STT | (format TBD) | Not yet in scope |

The Rules 1–8 generalize to every endpoint type per `endpoint-typology-architecture.md` §4.4. The doc you are reading now is the authoritative source for chat-completions and responses-API specifics.

## Adding a new provider — checklist summary

The full checklist is in `provider-adapter-architecture.md` §8. Summary:

1. Decide the wire family: OpenAI-shape, Anthropic-shape, Gemini-shape, or custom.
2. Implement under `packages/ai-gateway/internal/providers/specs/<name>/`:
   - `spec.go` — builds the `AdapterSpec`.
   - `codec/codec.go` — `EncodeRequest` (canonical → wire) + `DecodeResponse` (wire → canonical via `DecodeViaShared`).
   - `stream/` — `StreamDecoder.Open` (SSE or chunked frames → canonical chunks).
   - `transport.go` — `BuildURL` + `ApplyAuth` + `Do` + `Probe`.
   - `errors/` — `ErrorNormalizer` mapping upstream 4xx/5xx to `ProviderError.Code`.
3. Register in `packages/ai-gateway/internal/providers/builtins/`.
4. Add provider + model rows to `tools/db-migrate/seed/seed.ts`.
5. Run `/adapter-conformance-check` to verify §3a compliance before marking the PR ready.

## Response format and structured output

Structured output (JSON mode, function calling, tool use) follows the same canonical format contract as text completions. The canonical `response_format` field and `tools[]` array are part of the canonical bus. Key adapter-level considerations:

- **OpenAI structured output.** `response_format: { type: "json_schema", json_schema: {...} }` is passed through verbatim for OpenAI-family providers. For non-OpenAI providers that have JSON-mode equivalents (e.g., Anthropic `tool_use` + schema), the Anthropic codec's `EncodeRequest` translates the canonical `response_format` to the Anthropic tool-call equivalent.
- **Function/tool calling.** The canonical `tools[]` array uses the OpenAI function-calling schema. Non-OpenAI adapters translate to their wire tool format in `EncodeRequest`. Anthropic uses a different tool-call schema; the Anthropic codec translates canonical tools → Anthropic tool-call format and back.
- **Hook pipeline access.** Hooks receive the canonical body, which contains `tools[]` and `response_format` in their canonical shapes. A custom hook that wants to inspect or modify the tool list works with the canonical form, independent of which provider handles the request.

Extension fields for provider-specific structured output features (e.g., Anthropic `tool_choice.type: "tool"` with a named tool) follow the `nexus.ext.<provider>.<key>` pattern.

## Per-model wire quirks: empirical evidence requirement (Rule 7)

Rule 7 is worth extra emphasis because it is the most commonly violated rule in code review. Every "model X rejects param Y" case in an adapter's codec must have a comment citing the observed HTTP 400. The required comment format:

```go
// Observed: claude-opus-4-7 returns HTTP 400 "temperature not allowed for this model"
// when temperature != nil. Anthropic docs: https://... (retrieved 2026-05-10).
// Strip temperature and top_p for this model family.
func anthropicModelRejectsSamplingParams(model string) bool {
    return strings.HasPrefix(model, "claude-opus-4-7")
}
```

**Why this rule exists.** Speculative rules (added without an observed 400) tend to accumulate and cause false flattening: the adapter strips parameters the provider actually accepts, causing callers to get worse results silently. An operator who sets `temperature=0.1` expects deterministic outputs; an adapter that silently strips `temperature` because someone guessed the model "probably doesn't support it" delivers non-deterministic outputs with no error signal.

**When a 400 is discovered in production.** The correct flow:
1. A user reports that their request to model X with param Y is failing.
2. The gateway's debug log (body-level logs enabled) shows the upstream 400 body.
3. A PR adds the strip rule with a comment citing the trace_id, date, and error message.
4. The PR includes a test that sends a request with the affected param and asserts the adapter strips it.

Do not add the rule without the production evidence; do not add it without the test.

## Cross-format routing: the canonical bridge as a join point

`canonicalbridge.IngressChatToCanonical` is the system's canonical join point. Every cross-format routing decision flows through it. Understanding when it runs (and when it is correctly skipped) is essential for working on the AI Gateway executor.

**When it runs — cross-format routing:**
- Ingress: `FormatAnthropic` (client called `/v1/messages`), target: `openai/gpt-5.4` — bridge converts Anthropic request shape to OpenAI shape before `spec_openai.EncodeRequest`.
- Ingress: `FormatOpenAI` (client called `/v1/chat/completions`), target: `anthropic/claude-sonnet-4-6` — bridge produces a canonical OpenAI body; `spec_anthropic.EncodeRequest` translates canonical → Anthropic wire.
- Ingress: `FormatGemini` (client called `/v1/chat/completions` with a Gemini-family routing rule), target: `openai/...` — bridge converts Gemini shape to canonical first.

**When it is correctly skipped — same-format passthrough:**
- Ingress: `FormatAnthropic`, target: any `anthropic`-family adapter — no canonicalization; `spec_anthropic.PassthroughRewrite` applies per-model rewrites directly to the Anthropic-wire body.
- Ingress: `FormatOpenAI`, target: any `openai`-family or `openai-compat` adapter — no canonicalization; the body is already canonical.

**The forbidden pattern (Rule 5 violation):**
```go
// WRONG — calling EncodeRequest without canonicalizing first when formats differ
body := req.BodyBytes()                          // Anthropic-wire body
canonical, _ := spec_openai.EncodeRequest(body, target) // Identity codec returns the same body
// → api.openai.com receives Anthropic-format JSON → HTTP 400
```

The executor code at `packages/ai-gateway/internal/execution/executor.go` contains the authoritative same-format-detection logic that decides whether to call `IngressChatToCanonical`. Any new executor path that does not go through this detection must be treated as a Rule 5 violation.

## The `nexus.dry_run` flag and the pre-flight estimator

The reserved namespace `nexus.dry_run` (a boolean field in the canonical request body, default `false`) triggers the pre-flight cost estimator path:

1. The AI Gateway detects `nexus.dry_run = true` in the canonical body.
2. The full pipeline runs through routing and cache lookup.
3. If there is a cache hit: returns the cached response with `x-nexus-dry-run: hit`.
4. If there is a cache miss: calls `estimate.Compute(canonical, targetModel)` to produce a token-count estimate without calling the upstream provider.
5. Returns a synthetic response with the estimated token count, estimated cost, and `x-nexus-dry-run: miss`.

The estimator uses the `Model.context_window` and simple tokenizer heuristics. It is accurate to within ~10% for most prompts but is not a substitute for actual cost tracking. The `/v1/estimate` endpoint is a dedicated dry-run path.

`nexus.dry_run` is the only member of the non-provider reserved `nexus.*` namespace. Any new cross-provider behavior flag must use this namespace (not `nexus.ext.<provider>.*`) and must include an SDD update and OpenAPI bump.

## Tier-1 normalizers and the shared decode contract

Rule 8 mandates that all response decoding goes through `packages/shared/transport/normalize/codecs/<format>/`. The four Tier-1 normalizers currently implemented:

| Format | Package path | Handles |
|---|---|---|
| OpenAI chat-completions | `normalize/codecs/openai/` | `/v1/chat/completions` response, streaming SSE |
| Anthropic Messages | `normalize/codecs/anthropic/` | `/v1/messages` response, SSE |
| Google Gemini | `normalize/codecs/google/` | `generateContent` response, chunked HTTP/2 |
| Cursor IDE | `normalize/codecs/cursor/` | Connect-RPC + protobuf frames (text-first, Tier-2 overlay) |

Each normalizer implements the `NormalizedPayload` interface:
- `ExtractText() string` — the human-readable text for compliance inspection.
- `ExtractUsage() *Usage` — token counts: prompt, completion, cached input read, cache creation.
- `ExtractModel() string` — the model name as returned by the provider (may differ from the requested model for auto-routed models).

The `shared/normalize` package is the single location for wire-format parsing. A bug in OpenAI token extraction needs one fix in `normalize/codecs/openai/` to propagate to all three traffic paths (AI Gateway, Compliance Proxy, Desktop Agent) and the Hub audit pipeline simultaneously.

## Conformance audit and the `adapter-conformance-check` skill

The `/adapter-conformance-check` skill runs a structural audit of all adapter packages against the §3a rules. It checks:

1. Each non-identity adapter has both `EncodeRequest` and `DecodeResponse` wired.
2. `DecodeResponse` calls `canonicalbridge.DecodeViaShared` (Rule 8 — not a custom decode path).
3. No cross-adapter case statements in `spec_adapter.go` (Rule 3 — no global switch on format).
4. Every prefix-list rule in `codec.go` has a comment with the observed HTTP 400 (Rule 7).
5. The streaming session's response parser yields `CanonicalChunk` structs (Rule 6 — streaming parity).

Running `/adapter-conformance-check` before marking a PR ready is a binding requirement (CLAUDE.md "Adapter format-translation follows `provider-adapter-architecture.md` §3a"). The conformance gaps table in `provider-adapter-architecture.md` §11 documents the canonical slot for ambiguous placements.

## Cost extraction and the canonical `Usage` struct

Cost computation is downstream of canonical format. The `Usage` struct extracted by the Tier-1 normalizer has four fields:

| Field | Source |
|---|---|
| `PromptTokens` | Canonical `usage.prompt_tokens` (already subtracted cached tokens on OpenAI wire) |
| `CompletionTokens` | `usage.completion_tokens` (includes reasoning tokens for o-series, sonnet-4-5 thinking) |
| `CachedInputReadTokens` | `usage.prompt_tokens_details.cached_tokens` (OpenAI) or `nexus.ext.anthropic.cache_read_input_tokens` |
| `CacheCreationTokens` | `nexus.ext.anthropic.cache_creation_input_tokens` (Anthropic only; OpenAI caching is automatic, no creation token field) |

`metrics.CalculateCost(usage, prices)` takes this struct and a `ModelPrices` row (four price fields from the `Model` DB table) and returns a four-component USD breakdown. Any provider that introduces a new token type (e.g., a new "context compression" token) must: (1) add it to the Tier-1 normalizer output, (2) add a `nexus.ext.<provider>.<key>` carrier if it has no canonical field, (3) add a price field to `ModelPrices`, and (4) update `CalculateCost` to include it.

## Extension field lifecycle: from ingress to egress

A worked example of how the `nexus.ext.anthropic.thinking` extension field flows through a request:

1. **Ingress (Anthropic format, `hub_ingress` handler).** The Anthropic ingress reads `request.thinking` from the Anthropic wire body. It calls `canonicalext.Set(body, "nexus.ext.anthropic.thinking", thinkingConfig)` to store it in the canonical body.

2. **Routing.** The router inspects the canonical body's top-level fields only. It does not see `nexus.ext.*` fields — they are opaque to the router.

3. **Hook pipeline.** Hooks see the canonical body. The PII hook might scan `messages[].content` but ignores `nexus.ext.*`. A custom hook that wants to inspect thinking config can call `canonicalext.Get`.

4. **Cross-format routing (Anthropic ingress → OpenAI target).** `IngressChatToCanonical` runs. `thinking` has no OpenAI equivalent, so it is dropped from the canonical output (or bridged if there is a `nexus.ext.anthropic.thinking` → `nexus.ext.openai.reasoning_effort` mapping configured).

5. **Same-format passthrough (Anthropic ingress → Anthropic target).** The codec's `EncodeRequest` calls `canonicalext.Get(body, "nexus.ext.anthropic.thinking")` and reinjects it into the wire body. The upstream receives the thinking config as intended.

6. **Response.** The Anthropic response normalizer reads `thinking` block(s) from the response and stores them via `canonicalext.Set`. The response encoder reattaches them on egress so the client receives the full thinking content.

This lifecycle is why `ScanUnsupported` + `WarnOnce` matters: during cross-format routing step 4 above, if the codec does not handle `nexus.ext.anthropic.thinking`, `ScanUnsupported` emits a one-shot log entry warning that a field was silently dropped. Operators see the warning before a user files a bug.

## Adapter family membership and identity codecs

Provider adapters belong to a wire family. The family determines whether same-format passthrough applies:

| Wire family | Members | Same-shape passthrough condition |
|---|---|---|
| `openai` | OpenAI, Azure OpenAI, many compat providers | Ingress = `FormatOpenAI`, target = any `openai`-family adapter |
| `anthropic` | Anthropic, AWS Bedrock Anthropic, Vertex Anthropic | Ingress = `FormatAnthropic`, target = any `anthropic`-family adapter |
| `gemini` | Google Gemini, Vertex Gemini | Ingress = `FormatGemini`, target = any `gemini`-family adapter |
| `openai-compat` | Perplexity, Together AI, Fireworks, Moonshot, … | Ingress = `FormatOpenAI`, target = `openai-compat` adapter (with adapter-specific rewrites) |

Identity codec behavior: when the ingress and target are in the same family, `EncodeRequest` is the identity function for the canonical top-level fields. Per-model rewrites (strip `temperature`, rename `max_tokens`, etc.) still run via `AdapterSpec.PassthroughRewrite`.

Cross-family routing always goes through `IngressChatToCanonical`. The canonical form is the bus; the codec's `EncodeRequest` translates from bus to target wire.

---

## Canonical docs

- [`provider-adapter-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) — Rules 1–8 in full, adapter interface definitions (`AdapterSpec`, `SchemaCodec`, `StreamDecoder`), flow diagrams for same-shape passthrough vs cross-format routing, conformance gaps audit table
- [`normalization-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/normalization-architecture.md) — Tier-1 normalizer pipeline, E58-S0 codec delegation contract

**Adjacent wiki pages**: [The Five Services](The-Five-Services) · [Three Traffic Paths](Three-Traffic-Paths) · [AI Gateway Provider Adapters](AI-Gateway-Provider-Adapters) · [AI Gateway Ingress Endpoints](AI-Gateway-Ingress-Endpoints) · [Recipe Adding A Provider Adapter](Recipe-Adding-A-Provider-Adapter)
