# Error taxonomy architecture

Every error surface in the Nexus Gateway resolves to one of four layers: the **canonical `provcore.ProviderError`** that adapters return when an upstream HTTP call fails, the **per-ingress wire envelope** that the gateway encodes for the client (OpenAI / Anthropic / Gemini / Responses-API shape), the **admin-API error helper** that Control Plane uses for its own admin surface, and the **service-internal envelope** that the Hub and compliance-proxy emit on their own HTTP APIs (§9). The first three layers are deliberately separate — adapters reason in canonical codes without caring which client format will be encoded, the wire writers translate one canonical error into the right native shape per ingress, and the admin surface uses its own helper because it never speaks LLM dialect. The fourth layer now uses the same `{"error":{"message","type","code"}}` nested shape as Control Plane: all services call `packages/shared/transport/httperr` so every service surface is parseable with a single decoder.

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
| `CodeProviderQuotaExhausted` | `provider_quota_exhausted` | The provider ACCOUNT's budget is spent. Distinct from `rate_limited`, which clears in seconds: this clears when the billing window resets or the customer raises the limit, and it is account-scoped, so every model behind that provider is equally unusable. Several providers file it under a status that says otherwise — Anthropic returns HTTP 400 `invalid_request_error`, OpenAI a 429 that also carries genuine rate limits — so each normaliser reads the discriminator its own upstream publishes (see the table in `provider-adapter-architecture.md`), falling back to `specutil.IsQuotaExhaustedMessage` where none exists and declining to classify at all where the upstream conflates the two. The executor eliminates the provider for the request rather than deprioritising it (waiting cannot restore a budget), does not charge the credential (the key authenticated; the money ran out), and does record provider health, whose window decays so a raised limit is picked up on the next turn. |
| `CodeTimeout` | `timeout` | 408 / 504 / transport timeout / context deadline. |
| `CodeUpstreamError` | `upstream_error` | 5xx / unrecognised 4xx. |
| `CodeEndpointUnsupported` | `endpoint_unsupported` | Adapter does not serve the requested wire shape on this provider model. |
| `CodeNotImplemented` | `not_implemented` | Feature flagged-off in this adapter. |
| `CodeNoCompatibleProvider` | `no_compatible_provider` | Routing layer found no target adapter that can serve the request. |
| `CodeContextOverflow` | `context_overflow` | Prompt exceeds the target model's context window (OpenAI `context_length_exceeded` / "maximum context length" message; Anthropic "prompt is too long"; Gemini "exceeds the maximum number of tokens"). Target-permanent: the executor never retries the same target but MAY fail over to the next target — the smart strategy arms a larger-window `ContextUpgradeOnly` target used exactly for this class; on the last target the provider's own 400 is surfaced verbatim. |

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

**Every gateway-generated failure names itself on the audit row.** `traffic_event.error_code` is what an operator groups, counts and alerts on, so each writer takes its code as a required argument rather than defaulting it — a new failure path cannot be added without naming itself. The caller is told the same code. `traffic_event.error_code` and the envelope's `error.code` answer the same question — what was refused — so they carry the same UPPER_SNAKE string, and a caller who has to branch on the failure gets the answer the operator will group by. Anything else leaves the caller a status and an English sentence while the gateway had already named the cause.

| Writer | Surface | Status | `rec.ErrorCode` | Also sets |
|---|---|---|---|---|
| `writeError` (`proxy_errors.go`) | Generic gateway 4xx/5xx (hook pipeline, admission, quota, redaction fail-closed, egress reshape, codec fallback). | caller-specified | caller-specified, required, and emitted as the envelope's `error.code` | — |
| `writeDetailedErr` (`proxy_errors.go`) | Same plus a `hint` field and a string `error.code` — used for VK rejection (401), quota rejection (429 `QUOTA_EXCEEDED`), payload-too-large (413), and the routing refusals below. | caller-specified | the same string as the envelope's `code` | — |
| `writeDetailedErr` → `ROUTING_RULES_RESOLVED_NOTHING` (`stage_routing.go`) | Every routing rule that matched the request resolved no target — a deleted target model, a disabled provider, a strategy the gateway cannot dispatch. Refused rather than passed through, because serving the model the caller named is the decision the rule existed to prevent. | 503 | `ROUTING_RULES_RESOLVED_NOTHING` | the routing trace, stamped BEFORE the refusal so the hint's "read the trace" is answerable |
| `writeCodecErr` (`proxy_errors.go`) | Codec / prepare-body failures. A typed `*ProviderError` keeps its own status, code and message; an untyped error falls back to 400 `CODEC_ENCODE_FAILED`. | from the error | from the error, or `CODEC_ENCODE_FAILED` | — |
| `writeNoCompatibleCapability` (`cross_format.go`) | Embeddings / tool-use / native-streaming capability missing across every routing target. | 400 | `NO_COMPATIBLE_CAPABILITY` | `HookReasonCode` `no_compatible_capability` |
| `writeResponsesFeatureRejection` (`cross_format.go`) | `/v1/responses` request needs Responses-API-native target (`previous_response_id`, `store=true`, built-in tools …). | 400 | `FEATURE_REQUIRES_NATIVE_RESPONSES_TARGET` | `HookReasonCode` `feature_requires_native_responses_target` |
| `writeCrossFormatStreamUnsupported` (`cross_format.go`) | Ingress wire-shape streaming cannot translate to the routed target's streaming. | 400 | `CROSS_FORMAT_STREAM_UNSUPPORTED` | `HookReasonCode` `cross_format_stream_unsupported` |
| `writeNoCompatibleProvider` (`cross_format.go`) | Routing layer returned `NoCompatibleProviderError` — no target survives the cross-format compatibility check. | 400 | `NO_COMPATIBLE_PROVIDER` | `HookReasonCode` `no_compatible_provider` |

`ROUTING_RULES_RESOLVED_NOTHING` is attributed to US, not to the upstream, in the traffic
error-governance view (`traffic_errors.go` `classifyTrafficErrorAttribution`) — it is a
configuration mistake and no provider was called. Naming it there is load-bearing: it carries a
5xx, and the default arm books every unlisted 5xx against the upstream. It also feeds the
`proxy.routing_no_match` Hub alert alongside `ROUTING_NO_MATCH`, whose stated population — deleted
rule, model rename, alias drift — is exactly this.

The four `cross_format.go` writers build their bodies through `envelope.GatewayErrorBodyWith` — the same builder as every other gateway error, plus the one field each needs that no other refusal supplies (the capability rejection's `available_capabilities`, the Responses rejection's `param`). They share `rejectWithBespokeEnvelope`, which stamps status, code, reason and the emitted body onto the record. The body is stamped for the same reason `writeIngressError` stamps it: a gateway-generated error envelope carries no user content and is the most useful thing to see when a request fails.

These writers emit the flat gateway envelope (`{"error":{"message":"…","type":"<openai-type>","code":<int|string>,"param":"<field>"?}}`) for OpenAI-family ingress; a non-OpenAI ingress is reshaped to its own envelope by `writeIngressError` (§3).

`type` is derived from the **HTTP status** by `envelope.OpenAIErrorTypeForStatus` (`gateway_error.go`) onto OpenAI's vocabulary: 400 → `invalid_request_error`, 401 → `authentication_error`, 403 → `permission_error`, 404 → `not_found_error`, 429 → `rate_limit_error`, 5xx → `api_error`, other 4xx → `invalid_request_error`. Deriving from the status rather than from a per-code table is deliberate — the gateway emits ~45 distinct UPPER_SNAKE codes across the chat / embeddings / STT / video / realtime / guardrail paths, and a table would silently fall back to a wrong default each time one is added.

`code` is the Nexus UPPER_SNAKE machine code, or absent when the writer named none: the Control Plane UI, the Hub alert aggregators, and the 429 discriminator in §7 all match on it. It is never the numeric status — the SDKs type the field as an optional string, and repeating the status inside the body says nothing the status line has not while looking enough like a machine code to be matched on. `param` names the offending field for the model-resolution codes.

Before AP-3 these writers emitted the constant `type: "proxy_error"` with no `param` — a value in no OpenAI SDK's vocabulary, which told a caller nothing the status had not already said. The per-ingress encoders remain reached only by the upstream-error path (`proxy.go` calls `envelope.EncodeErrorEnvelopeForIngress` when a `*ProviderError` came back from the adapter); the two paths now agree on the `type` vocabulary even though they derive it differently (status here, normalised provider code there).

Two writers outside the ingress packages answer through the same envelope, because they sit in the middleware chain that wraps the mux and no route escapes them: `Recovery` (500 `INTERNAL_ERROR` on a recovered panic — its previous body made `error` a string rather than an object, so `err.error.message` threw in every SDK that reached for it) and the connection-stage hook rejection (403 `CONNECTION_BLOCKED`). The Realtime error EVENT (`realtime_session.go`) wraps the same inner shape and takes its `type` from the same vocabulary.

### 4.0a Spend refusals name themselves

`image n` and `rerank documents` are bounded because one request must not multiply billable units without limit. Both bounds are enforced on the passthrough leg by `canonicalbridge.Validate{Images,Rerank}IngressGuards`, which return a typed `*ProviderError` carrying `spend_limit_exceeded`.

The type matters. An untyped error falls through `writeCodecErr` to `CODEC_ENCODE_FAILED` with the prefix "canonicalize ingress body:" — on a leg where no canonicalization happens. Nothing failed to encode; a ceiling was enforced. Filed under the codec code, the traffic row says the same, so an operator grouping by `error_code` sees spend refusals as codec faults and a caller cannot tell "I asked for too many images" from "the gateway could not translate my request".

The SHAPE checks in the same guards stay untyped on purpose: a malformed body is the caller's mistake, and putting it in the spend bucket would move their error into the operator's billing view.

`spend_limit_exceeded` is gateway-internal and stays out of `CanonicalCodes` and the Hub's shared vocabulary, for the same reason `client_gone` and `local_processing_failed` do — that slice enumerates the codes an ADAPTER produces from an upstream response, which is what the Hub's upstream-vs-gateway alert split keys on. No adapter can emit this one: the request never reached an upstream.

### 4.1 Routes with no audit record

The catalog, usage and estimate surfaces produce gateway errors without a `*audit.Record`, so they cannot reshape by the record's ingress format. They call `envelope.WriteGatewayError`, which reads the dialect from the request path — the `/v1/messages` family to Anthropic, `/v1beta` to Gemini, `/v1/responses` to the Responses shape, everything else OpenAI — and then builds through the same `GatewayErrorBody`.

The same writer answers any path the gateway does not serve, registered both under `/v1/` and at the root. A catch-all matches everything, which puts `ServeMux`'s own 405/`Allow` branch out of reach — it runs only when no pattern matched — so the fallback asks the mux which methods WOULD have matched (`servedUnderOtherMethods`) and answers 405 `METHOD_NOT_ALLOWED` with `Allow` when any would. Without that step a wrong-method request became a 404 whose body claimed the gateway does not serve a path it does serve. Go's `ServeMux` answers an unmatched pattern with `404 page not found` as `text/plain`, and every SDK the gateway speaks to JSON-parses error bodies, so an unmounted path reached the caller as a status carrying no message. Registering the fallback at the root as well as under `/v1/` is what extends that to the Gemini and Azure-compat prefixes; `ServeMux` prefers the more specific pattern, so no mounted route is displaced.

## 5. Hook rejection path

Compliance hooks return a `Decision` (`packages/shared/policy/hooks/core/types.go`):

- `Approve` — pass through.
- `RejectHard` — the operator-facing `block` action. How the rejection reaches the client depends on which service owns the response: the **AI Gateway** emits HTTP 403 with the gateway envelope (same flat shape as §4 — `type: "permission_error"` from the 403, not the per-ingress encoder) at both the request and response stages; the **Compliance Proxy** (via the shared `tlsbump` forwarder, `richReject=true`) emits a 403 at the request stage and HTTP 451 at the response/SSE stages with an attributed reject body; the **Agent** on-host interceptor (`richReject=false`, fail-open) emits a minimal `Forbidden` with no attribution body. In all cases `rec.HookReasonCode` is set from the hook result and the blocking rule + actor are captured on the audit row, with the redacted copy persisted.
- `BlockSoft` — there is no distinct soft-block response and no HTTP 246. The internal `BlockSoft` decision still exists (e.g. a soft AI-Guard verdict folded into the merge) but `ActionFromDecision` folds it to the `block` action, so it dispatches exactly like `RejectHard` above.
- `Modify` — the `redact` action: the service pushes the hook's rewritten body back onto the wire via the adapter's `RewriteRequestBody` (request stage) or its response equivalent (non-streaming response + cache-hit response + streaming via the held-back SSE prefix), and the same masked body is forwarded, returned, and stored. Adapters that return `ErrRewriteUnsupported` fall through with a warning log and stamp `REDACT_INFLIGHT_UNSUPPORTED` on the audit row.
- `Abstain` — no opinion, equivalent to Approve.

**Block response attribution (proxies that own the client response).** When the AI Gateway or Compliance Proxy blocks, the reject body tells the caller what tripped the policy, with a safety asymmetry by stage: a **request-stage** block may carry matched rule/category IDs and the blocked values (the caller already holds its own request data), while a **response-stage** block carries only rule-ID / category labels and **never** echoes the upstream's original sensitive value (echoing it would leak the very content the block exists to contain). The agent never synthesizes an attributed body.

The hook pipeline's `HookResult.ReasonCode` is the per-hook string the audit row carries. Standard values live in `packages/shared/policy/decision/types.go` (the `Reason*` constant block):

- `REDACT_INFLIGHT_UNSUPPORTED` — a `redact` match could not be applied on the live wire shape (adapter returned `ErrRewriteUnsupported`); the redacted copy is absent so the raw body is dropped rather than persisted.
- `AIGUARD_SUGGESTED_VS_POLICY` — AI-Guard scanner suggested action overridden by admin policy.
- `GENERATIVE_PROMPT_BLOCKED` — AI-Gateway-local (constant in `packages/ai-gateway/internal/ingress/proxy/generative_block.go`, not in the shared vocabulary): a content match on a generative binary-output prompt (image / video generation) escalated to a hard 403 even though the matching hook was configured observe-only — the output artifact is uninspectable, so the prompt is the only enforcement point. Unlike the `REDACT_INFLIGHT_UNSUPPORTED` arm (which leaves the pipeline's MODIFY decision on the row), the escalation stamps the aggregate `request_hook_decision` as `REJECT_HARD` so blocked-traffic queries and the drawer's decision badge count it as blocked; the per-hook trace still carries each hook's real approve/observe vote.

`REDACT_STORAGE_ONLY_BY_POLICY` and `STORAGE_DROPPED_BY_POLICY` stay defined as constants so historical rows that carry them still render, but no live path stamps them: a single `action` axis admits no store-only-redact divergence, and `drop-content` is not an operator choice.

Audit-only reason strings (`no_compatible_capability`, `feature_requires_native_responses_target`, `QUOTA_EXCEEDED`) are written ad-hoc at the rejection site and are not enumerated as constants today — they exist only as the literal string the writer emits and the corresponding `rec.HookReasonCode` assignment. New writers should follow the same pattern: pick a stable snake_case string, set it on the audit record, and grep'able literal at the producer site is the single source of truth.

## 6. Local quota vs upstream rate-limit

The 429 surface is split deliberately. **Local quota** (`proxy.go: writeDetailedErr` at the quota-decision site) emits the gateway envelope with `type: "rate_limit_error"` (from the 429), `code: "QUOTA_EXCEEDED"` and a `hint` field — the request never reaches upstream. **Upstream provider 429** is normalised to `ProviderError{Code: CodeRateLimited, RetryAfter: <parsed>}` by the per-provider normaliser, then encoded for the client via the per-ingress writer with the provider's native rate-limit shape preserved. Clients distinguish the two by `error.code` — `QUOTA_EXCEEDED` is always Nexus, `rate_limited` is always upstream.

Two more 429s are ours and answer in the same envelope: `RATE_LIMITED` when a virtual key's RPM bucket is exhausted, and `GATEWAY_OVERLOADED` when the admission gate sheds load. Every one of them carries `Retry-After`, and the RPM rejections carry `X-RateLimit-Limit` — stamped before the limiter's verdict, so the response that most needs to say what the limit is is not the one response without it. The RPM cap applies on the read routes (`/v1/models`, `/v1/usage*`, the public catalog) through `vkReadRateLimit` as well as on the data plane through `checkRateLimit`, and both answer with the same code and the same envelope.

### 6b. Three scopes, one word

"Quota" names two unrelated things on this surface, and they reach the caller as different codes on purpose:

- `QUOTA_EXCEEDED` (429, gateway-generated) is the VIRTUAL KEY's own spend quota, enforced by us. §6 covers it.
- `provider_quota_exhausted` is the PROVIDER ACCOUNT's budget, spent at the vendor. Ours to react to, not to enforce.

The second lands on three scopes and each is decided separately, which is why the three predicates in `classify.go` disagree with each other:

| Scope | Treatment | Why |
|---|---|---|
| Credential | NOT charged; the circuit breaker stays closed | The key authenticated. Opening the breaker would show an operator a credential problem and send them to rotate a key that was never wrong. |
| Provider | Eliminated for the rest of THIS request, and counted toward provider health | A budget is account-wide, so every model behind that provider is equally unusable; and the resolver hands out one credential per provider, so there is no second key for the executor to try. The health window decays, so a raised limit is picked up on the next turn rather than shunning the provider indefinitely. |
| Virtual key | Untouched | The caller's own quota is a separate ledger and has not moved. |

## 6a. Client cancellation vs provider failure (499 `CLIENT_CLOSED`)

When the inbound request context is canceled (the client closed the connection or hit its own deadline) while an upstream attempt is in flight, the cancellation propagates into the upstream call and the executor returns an exhausted-targets error. The upstream-fetch path (`proxy_upstream.go: fetchUpstreamWithPreparedBody`) checks `r.Context().Err()` **before** emitting `502 PROVIDER_UNAVAILABLE`: if it is `context.Canceled` / `context.DeadlineExceeded`, the failure is attributed to the client as **`499 CLIENT_CLOSED`** (`statusClientClosedRequest`, mirroring nginx's 499 — Go's `net/http` defines no such constant), not to the provider. This keeps a client disconnect out of provider-availability accounting; without it, every client that walks away mid-stream would inflate the upstream's apparent 502 rate. The credential circuit breaker is unaffected either way — a canceled attempt surfaces to `RecordAttempt` as status `0` (`network`), which the breaker ignores (only 401/403/429/2xx drive transitions). The response-body write is a no-op on an already-closed connection; the value is the correct `rec.StatusCode = 499` / `rec.ErrorCode = "CLIENT_CLOSED"` attribution on the audit row.

## 7. Admin-API envelope (Control Plane)

Control Plane's admin surface uses the same envelope shape via two helpers — `handler.errJSON` (`packages/control-plane/internal/ai/providers/handler/handler.go`) and `middleware.errorResp` (`packages/control-plane/internal/platform/middleware/adminauth.go`). Both emit `{"error":{"message":"…","type":"…","code":"…"}}`. The admin tier never speaks LLM ingress dialect, so it ignores the per-ingress envelope encoders entirely.

- The admin-auth middleware returns `{401, "authentication_error", "AUTH_REQUIRED"}` through `errorResp`.
- The IAM middleware returns `{403, "authorization_error", "IAM_ACCESS_DENIED"}` with a `details:{action, resource, reason}` block written inline (same envelope shape, extra `details` field).
- Per-handler 4xx paths (validation, not-found, conflict) use the same envelope with caller-supplied `(message, type, code)` triples.

## 8. Error metrics

`packages/ai-gateway/internal/platform/metrics/metrics.go` registers two error-aware counters:

- `requests_total{provider, model, endpoint, status}` — bucketed by HTTP status family. Used for the top-level success-rate panel.
- `errors_total{provider, error_type}` — incremented once per request a provider failed, by `proxy_upstream.go: recordUpstreamFailure`. Both provider-failure paths feed it: the terminal 4xx and the all-targets-exhausted path. Used for the per-provider error-category panel.

`error_type` is the canonical `ProviderError.Code` of the **terminal attempt** — the last one that actually reached a provider. One label value is not a canonical code: `unclassified`, for a dispatched attempt whose transport failure produced no provider envelope (a dial refusal, a connection reset). Label cardinality is therefore bounded by the canonical code set plus that one, which is why the counter takes the code and never free-form error text.

`provider` is the terminal attempt's provider. When no attempt reached a provider at all there is nothing to attribute, and the counter is not incremented — so **do not add a `not_dispatched` bucket to a dashboard**: the log path uses that word to describe an attempt that never left the process, but it can never appear as a metric label, because the absence of a terminal attempt is exactly the case that skips the counter.

Two deliberate exclusions:

- **Client disconnects.** A `499 CLIENT_CLOSED` (§ above) is a client-side outcome, so counting it would inflate the provider's apparent error rate with failures the provider never had.
- **Gateway-internal rejections.** Quota, routing and admission failures never reach a provider and carry no provider label. They are visible on `requests_total` by status and on their own counters; `errors_total` is scoped to provider failures.

A new canonical `Code` automatically becomes a new label value on `errors_total` — no metrics-side registration needed, but the operator dashboard must add the new bucket explicitly if it's expected to be visible.

## 9. Service-internal envelope (Hub + compliance-proxy)

Hub and compliance-proxy HTTP APIs use the same `{"error":{"message","type","code"}}` nested shape as the Control Plane admin surface (§7), via `packages/shared/transport/httperr`.

```json
{"error": {"message": "<human-readable>", "type": "<snake_case_category>", "code": "<SCREAMING_SNAKE_MACHINE_CODE>"}}
```

**Hub** Echo handlers call `c.JSON(status, httperr.ErrJSON(msg, errType, code))` via the helper functions in each subsystem's `helpers.go` (fleet, identity, alerts, traffic ingest, observability diag). Raw-writer paths use `httperr.WriteError`. Type strings: `validation_error`, `auth_error`, `not_found`, `internal_error`, `service_unavailable`. Examples: `alerts/engine/handlers_admin.go`, `fleet/handler/hubapi/helpers.go`, `identity/handler/enroll/helpers.go`, `traffic/ingest/spill/helpers.go`. Exception: a small set of diagnostic and RPC-bridge responses (`observability/handler/diag/runtime_bridge.go` 501/503 paths, `fleet/handler/hubapi/hub_api_dlq.go`) carry richer payloads (extra `meta`, `target`, or `dispatchId` fields) that do not conform to the standard envelope and are not parsed as errors by callers.

**compliance-proxy (runtime API)** raw-writer handlers call `httperr.WriteError(w, status, msg, errType, code)` which sets `Content-Type: application/json`, writes the status code, and encodes the same envelope. Covered files: `runtime/auth/auth.go`, `runtime/breakglass/break_glass.go`, `runtime/config/runtime_config.go`, `runtime/handler/handler.go`, `runtime/server/server.go`.

The standard API error path across all four services uses a single `{"error":{"message","type","code"}}` shape via `packages/shared/transport/httperr`. Clients that branch only on status code and read `error.message` / `error.code` work identically across services.

## References

- `packages/ai-gateway/internal/providers/core/types.go` — `ProviderError` struct + 8 canonical `Code*` constants.
- `packages/ai-gateway/internal/providers/specs/<name>/errors/` and `<name>/errors.go` — per-provider upstream-to-canonical normalisers.
- `packages/ai-gateway/internal/ingress/envelope/error_envelope.go` — wire envelope encoders + SSE error-frame synth.
- `packages/ai-gateway/internal/ingress/proxy/proxy.go`, `packages/ai-gateway/internal/ingress/proxy/cross_format.go` — gateway-internal error writers + hook decision dispatch.
- `packages/ai-gateway/internal/platform/streaming/live.go` — streaming-side hook decision handling (`RejectHard` in-band termination + `Modify` rewrite of the held-back SSE prefix).
- `packages/ai-gateway/internal/platform/metrics/metrics.go` — `requests_total` + `errors_total` registration.
- `packages/shared/policy/decision/types.go` — `Reason*` constants.
- `packages/shared/policy/hooks/core/types.go` — `Decision` vocabulary re-exports.
- `packages/shared/transport/httperr/httperr.go` — canonical `ErrJSON()` + `WriteError()` shared by all services.
- `packages/control-plane/internal/platform/httperr/httperr.go` — CP re-export of the same envelope shape.
- `packages/control-plane/internal/platform/middleware/adminauth.go` — `errorResp` helper + 401 surface.
- `packages/control-plane/internal/platform/middleware/iamauth.go` — IAM 403 inline body.
- `packages/nexus-hub/internal/handler/errors.go` — Hub error helpers (`badRequest`, `unauthorized`, `forbidden`, `notFound`, `internalError`, `serviceUnavailable`) all call `httperr.ErrJSON`.
- `packages/compliance-proxy/internal/runtime/` — compliance-proxy runtime API handlers all call `httperr.WriteError`.
