# Error taxonomy architecture

Every error surface in the Nexus Gateway resolves to one of three layers: the **canonical `provcore.ProviderError`** that adapters return when an upstream HTTP call fails, the **per-ingress wire envelope** that the gateway encodes for the client (OpenAI / Anthropic / Gemini / Responses-API shape), and the **admin-API error helper** that Control Plane uses for its own admin surface. The three layers are deliberately separate — adapters reason in canonical codes without caring which client format will be encoded, the wire writers translate one canonical error into the right native shape per ingress, and the admin surface uses its own helper because it never speaks LLM dialect.

Anchor packages:

- `packages/ai-gateway/internal/providers/core/types.go` — `ProviderError` struct + the 8 canonical `Code*` constants (the single source of truth).
- `packages/ai-gateway/internal/providers/specs/<name>/errors/` — per-provider upstream-response → canonical normalisers (openai, anthropic, gemini all under `errors/`; bedrock, cohere, replicate, voyage have flat `errors.go`).
- `packages/ai-gateway/internal/ingress/envelope/error_envelope.go` — the four wire writers (`encodeOpenAIErrorEnvelope`, `encodeAnthropicErrorEnvelope`, `encodeGeminiErrorEnvelope`, `encodeResponsesAPIErrorEnvelope`) + the SSE-frame variant (`synthesizeSSEErrorFrame`).
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` + `cross_format.go` — gateway-internal error writers (`writeJSONError`, `writeDetailedErr`, `writeNoCompatibleCapability`, `writeResponsesFeatureRejection`, `writeCrossFormatStreamUnsupported`, `writeNoCompatibleProvider`).
- `packages/shared/policy/decision/types.go` — `HookResult.ReasonCode` + 4 standard `Reason*` string constants used by compliance hooks.
- `packages/shared/policy/hooks/core/types.go` — `Decision` vocabulary (`Approve`, `RejectHard`, `BlockSoft`, `Modify`, `Abstain`).
- `packages/control-plane/internal/ai/providers/handler/handler.go: errJSON` + `packages/control-plane/internal/platform/middleware/adminauth.go: errorResp` — the two admin-API envelope helpers (identical shape).

## 1. Canonical `ProviderError`

Every adapter's `Execute` / `Probe` returns `*provcore.ProviderError` on a non-2xx outcome. The fields are:

| Field | Purpose |
|---|---|
| `Status` | Upstream HTTP status code (0 for synthetic errors that never reached the network). |
| `Code` | Canonical category — one of 8 constants, branch on this. |
| `Type` | Provider's own type string (e.g., `"rate_limit_error"`), preserved for observability. |
| `Message` | Human-readable message. |
| `RetryAfter` | Optional `time.Duration` parsed from upstream `Retry-After` (`*time.Duration`). |
| `Raw` | Provider error payload verbatim — what the wire encoder re-emits when passthrough is appropriate. |
| `Headers` | Cloned upstream headers; nil for synthetic errors. |
| `TargetMethod` / `TargetPath` | The URL the adapter actually dispatched to — empty for synthetic errors that never reached the network. |

Canonical `Code` values (`packages/ai-gateway/internal/providers/core/types.go`, the `Code*` constant block):

| Constant | Wire string | Meaning |
|---|---|---|
| `CodeInvalidRequest` | `invalid_request` | 400 / 404 / malformed body. |
| `CodeAuthFailed` | `auth_failed` | 401 / 403 / bad credential. |
| `CodeRateLimited` | `rate_limited` | 429 — `RetryAfter` populated when upstream provided the header. |
| `CodeTimeout` | `timeout` | 408 / 504 / transport timeout / context deadline. |
| `CodeUpstreamError` | `upstream_error` | 5xx / unrecognised 4xx. |
| `CodeEndpointUnsupported` | `endpoint_unsupported` | Adapter does not serve the requested wire shape on this provider model. |
| `CodeNotImplemented` | `not_implemented` | Feature flagged-off in this adapter. |
| `CodeNoCompatibleProvider` | `no_compatible_provider` | Routing layer found no target adapter that can serve the request. |

Adding a new canonical code is a one-line change to the const block; callers branch on the string value, so a misspelling at a producer site silently drops into the upstream-error bucket rather than panicking. Tests under `packages/ai-gateway/internal/providers/core/types_test.go` pin the constant string values.

## 2. Per-provider normalisers

Each provider adapter has its own normaliser that takes the upstream HTTP response + raw body and returns a populated `*ProviderError`. The normaliser owns the provider-specific quirks:

- **OpenAI** (`openai/errors/errors.go`) — parses the OpenAI `{error:{type, message, code}}` shape, maps the HTTP status code to canonical `Code` (`.error.type` is preserved on `ProviderError.Type` for observability but does not drive `Code` selection), extracts `Retry-After` for 429.
- **Anthropic** (`anthropic/errors/errors.go`) — prioritises Anthropic's `.error.type` enum (`authentication_error`, `permission_error`, `invalid_request_error`, `rate_limit_error`, `overloaded_error`, `api_error`) before falling back to HTTP status.
- **Gemini** (`gemini/errors/errors.go`) — maps Google's `status` enum (`INVALID_ARGUMENT`, `UNAUTHENTICATED`, `PERMISSION_DENIED`, `RESOURCE_EXHAUSTED`, …) to canonical codes.
- **Bedrock** (`bedrock/errors.go`), **Cohere** (`cohere/errors.go`), **Replicate** (`replicate/errors.go`), **Voyage** (`voyage/errors.go`) — flat per-provider normalisers for their respective error shapes.

OpenAI-compatible providers (Azure OpenAI, DeepSeek, Mistral, Groq, Perplexity, Together, Fireworks, Moonshot, xAI, GLM, MiniMax, HuggingFace) delegate to the OpenAI normaliser — their adapter spec wires `openai.ErrorNormalizerInstance()` directly into the spec's `ErrorNormalizer` slot.

The normaliser is the only place that knows the provider's error shape; the rest of the gateway reads only `ProviderError.Code` and `ProviderError.Status`. Adding a provider means adding one `errors.go` plus a single Wire spec line; no other touch points.

## 3. Wire envelope per ingress (`internal/ingress/envelope/error_envelope.go`)

The gateway speaks four LLM client formats on its ingress side. Each format gets its own writer; the dispatch is keyed by the resolved ingress wire shape:

| Function | Emits |
|---|---|
| `encodeOpenAIErrorEnvelope` | `{"error":{"message":"…","type":"…","code":"…","param":null}}` — for `/v1/chat/completions`, `/v1/embeddings`, OpenAI-compat providers. |
| `encodeAnthropicErrorEnvelope` | `{"type":"error","error":{"type":"…","message":"…"}}` — for `/v1/messages`. |
| `encodeGeminiErrorEnvelope` | `{"error":{"code":<status>,"message":"…","status":"<gRPC-name>"}}` — for `/v1beta/…:generateContent` and Vertex paths. |
| `encodeResponsesAPIErrorEnvelope` | OpenAI-shaped wrapper with Responses-API-specific `type` values — for `/v1/responses`. |

The writer pulls the `Code`, `Status`, `Type`, and `Message` from the `ProviderError` (or from a synthetic gateway error), so the same canonical error renders consistently per ingress without per-call branching at the producer site.

### 3.1 SSE error frames

`synthesizeSSEErrorFrame` writes the same envelope into the mid-stream SSE framing the ingress expects:

- OpenAI: `data: {…}\n\n` (single frame, no event name).
- Anthropic: `event: error\ndata: {…}\n\n`.
- Responses API: `event: response.failed\ndata: {"type":"response.failed","sequence_number":<n>,"response":{"object":"response","status":"failed","error":{"message":"…","code":"…","type":"…"}}}\n\n` — required by the Responses API stream contract; `sequence_number=0` for pre-stream failures, threaded counter for mid-stream failures.
- Gemini: `data: {…}\n\n`.

A mid-stream error always closes the SSE stream cleanly; clients see a terminal error frame instead of an unterminated body.

## 4. Gateway-internal error writers (proxy + cross-format)

Errors that originate inside the gateway (before upstream dispatch) use dedicated writers so the producer site does not need to know the ingress dialect:

| Writer | Surface | Status | Sets `rec.HookReasonCode` |
|---|---|---|---|
| `writeJSONError` (`proxy.go`) | Generic gateway 4xx (auth, validation). | caller-specified | no |
| `writeDetailedErr` (`proxy.go`) | Same as `writeJSONError` plus a `hint` field — used for VK rejection (401) and quota rejection (429 `QUOTA_EXCEEDED`). | caller-specified | no |
| `writeNoCompatibleCapability` (`cross_format.go`) | Embeddings / tool-use / native-streaming capability missing across every routing target. | 400 | `no_compatible_capability` |
| `writeResponsesFeatureRejection` (`cross_format.go`) | `/v1/responses` request needs Responses-API-native target (`previous_response_id`, `store=true`, built-in tools …). | 400 | `feature_requires_native_responses_target` |
| `writeCrossFormatStreamUnsupported` (`cross_format.go`) | Ingress wire-shape streaming cannot translate to the routed target's streaming. | 400 | `cross_format_stream_unsupported` |
| `writeNoCompatibleProvider` (`cross_format.go`) | Routing layer returned `NoCompatibleProviderError` — no target survives the cross-format compatibility check. | 400 | `no_compatible_provider` |

These writers emit a flat gateway `proxy_error` envelope (`{"error":{"message":"…","type":"proxy_error","code":<int|string>}}`) regardless of ingress format — they do not route through the per-ingress encoders in §3. The per-ingress encoders are reached only by the upstream-error path (`proxy.go` calls `envelope.EncodeErrorEnvelopeForIngress` when a `*ProviderError` came back from the adapter), so gateway-internal errors and upstream errors look different on the wire.

## 5. Hook rejection path

Compliance hooks return a `Decision` (`packages/shared/policy/hooks/core/types.go`):

- `Approve` — pass through.
- `RejectHard` — the gateway emits HTTP 403 with the gateway `proxy_error` envelope (same flat shape as §4 — not the per-ingress encoder), sets `rec.HookReasonCode` from the hook result, terminates the request before upstream dispatch. The blocking rule + actor are captured on the audit row.
- `BlockSoft` — the gateway emits HTTP 246 (Nexus soft-reject convention) with the `proxy_error` envelope and an `X-Nexus-Hook` response header. Wired in the request, response, cache-hit, and streaming paths.
- `Modify` — the gateway pushes the hook's rewritten body back onto the wire via the adapter's `RewriteRequestBody` (request stage) or its response equivalent (non-streaming response + cache-hit response + streaming via the held-back SSE prefix). Adapters that return `ErrRewriteUnsupported` fall through with a warning log and stamp `REDACT_INFLIGHT_UNSUPPORTED` on the audit row.
- `Abstain` — no opinion, equivalent to Approve.

The hook pipeline's `HookResult.ReasonCode` is the per-hook string the audit row carries. Standard values live in `packages/shared/policy/decision/types.go` (the `Reason*` constant block):

- `REDACT_INFLIGHT_UNSUPPORTED` — redaction policy unavailable for the live request shape.
- `REDACT_STORAGE_ONLY_BY_POLICY` — admin policy says redact for storage only, not in-flight.
- `STORAGE_DROPPED_BY_POLICY` — payload-capture policy dropped the body before storage.
- `AIGUARD_SUGGESTED_VS_POLICY` — AI-Guard scanner suggested action overridden by admin policy.

Audit-only reason strings (`no_compatible_capability`, `feature_requires_native_responses_target`, `QUOTA_EXCEEDED`) are written ad-hoc at the rejection site and are not enumerated as constants today — they exist only as the literal string the writer emits and the corresponding `rec.HookReasonCode` assignment. New writers should follow the same pattern: pick a stable snake_case string, set it on the audit record, and grep'able literal at the producer site is the single source of truth.

## 6. Local quota vs upstream rate-limit

The 429 surface is split deliberately. **Local quota** (`proxy.go: writeDetailedErr` at the quota-decision site) emits the gateway's `proxy_error` envelope with `code: "QUOTA_EXCEEDED"` and a `hint` field — the request never reaches upstream. **Upstream provider 429** is normalised to `ProviderError{Code: CodeRateLimited, RetryAfter: <parsed>}` by the per-provider normaliser, then encoded for the client via the per-ingress writer with the provider's native rate-limit shape preserved. Clients distinguish the two by `error.code` — `QUOTA_EXCEEDED` is always Nexus, `rate_limited` is always upstream.

## 7. Admin-API envelope (Control Plane)

Control Plane's admin surface uses the same envelope shape via two helpers — `handler.errJSON` (`packages/control-plane/internal/ai/providers/handler/handler.go`) and `middleware.errorResp` (`packages/control-plane/internal/platform/middleware/adminauth.go`). Both emit `{"error":{"message":"…","type":"…","code":"…"}}`. The admin tier never speaks LLM ingress dialect, so it ignores the per-ingress envelope encoders entirely.

- The admin-auth middleware returns `{401, "authentication_error", "AUTH_REQUIRED"}` through `errorResp`.
- The IAM middleware returns `{403, "authorization_error", "IAM_ACCESS_DENIED"}` with a `details:{action, resource, reason}` block written inline (same envelope shape, extra `details` field).
- Per-handler 4xx paths (validation, not-found, conflict) use the same envelope with caller-supplied `(message, type, code)` triples.

## 8. Error metrics

`packages/ai-gateway/internal/platform/metrics/metrics.go` registers two error-aware counters:

- `requests_total{provider, model, endpoint, status}` — bucketed by HTTP status family. Used for the top-level success-rate panel.
- `errors_total{provider, error_type}` — incremented on every non-2xx path, keyed by the canonical `ProviderError.Code` (or by `proxy_error` for gateway-internal errors). Used for the per-provider error-category panel.

A new canonical `Code` automatically becomes a new label value on `errors_total` — no metrics-side registration needed, but the operator dashboard must add the new bucket explicitly if it's expected to be visible.

## References

- `packages/ai-gateway/internal/providers/core/types.go` — `ProviderError` struct + 8 canonical `Code*` constants.
- `packages/ai-gateway/internal/providers/specs/<name>/errors/` and `<name>/errors.go` — per-provider upstream-to-canonical normalisers.
- `packages/ai-gateway/internal/ingress/envelope/error_envelope.go` — wire envelope encoders + SSE error-frame synth.
- `packages/ai-gateway/internal/ingress/proxy/proxy.go`, `packages/ai-gateway/internal/ingress/proxy/cross_format.go` — gateway-internal `proxy_error` writers + hook decision dispatch.
- `packages/ai-gateway/internal/platform/streaming/live.go` — streaming-side hook decision handling (BlockSoft + Modify).
- `packages/ai-gateway/internal/platform/metrics/metrics.go` — `requests_total` + `errors_total` registration.
- `packages/shared/policy/decision/types.go` — `Reason*` constants.
- `packages/shared/policy/hooks/core/types.go` — `Decision` vocabulary re-exports.
- `packages/control-plane/internal/ai/providers/handler/handler.go` — CP `errJSON` helper.
- `packages/control-plane/internal/platform/middleware/adminauth.go` — `errorResp` helper + 401 surface.
- `packages/control-plane/internal/platform/middleware/iamauth.go` — IAM 403 inline body.
