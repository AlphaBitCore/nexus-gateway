# AI Gateway ingress API

This is the caller's guide to the AI Gateway's HTTP API: how to authenticate, which endpoints exist, and how the gateway lets you keep your existing SDK while routing to any provider. Request and response bodies follow the upstream provider shapes (OpenAI, Anthropic, Gemini) — this page documents the gateway-specific surface, not a re-specification of those bodies; for per-provider body detail see [provider-adapter-architecture.md](provider-adapter-architecture.md) and [provider-coverage.md](provider-coverage.md).

## 1. Base URL and authentication

Point your SDK's base URL at the gateway and use a Nexus **virtual key** as the credential. Every route accepts the virtual key in either carrier:

- `Authorization: Bearer <virtual-key>` — the standard bearer convention, honored on all routes.
- `x-nexus-virtual-key: <virtual-key>` — an explicit header alternative.

Gateway-issued virtual keys are prefixed `nvk_`. The caller never sends the upstream provider's own API key; the gateway holds provider credentials and attaches them when it dispatches upstream.

## 2. Supported endpoints

| Endpoint | Shape | Streaming |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions | yes (`stream: true`) |
| `POST /v1/messages` | Anthropic Messages | yes |
| `POST /v1/responses` | OpenAI Responses | yes |
| `POST /v1/embeddings` | OpenAI Embeddings | no |
| `POST /api/paas/v4/chat/completions`, `/api/paas/v4/embeddings` | GLM-native | chat: yes |
| `POST /openai/deployments/{deployment}/chat/completions`, `/embeddings` | Azure OpenAI-native | chat: yes |
| `POST /v1beta/models/{model}:generateContent`, `:streamGenerateContent` | Gemini-native | via the stream variant |
| `GET /v1/models`, `GET /v1/models/{model}` | model catalog | no |
| `POST /v1/estimate` | cost preview (no upstream call) | no |
| `POST /v1/ai-guard/classify`, `/v1/ai-guard/compliance-webhook` | AI-Guard classifier | no |

The canonical `/v1/*` routes are the primary surface. The provider-native shims mirror a provider's own path and body so an SDK already pointed at GLM, Azure, or Gemini works against the gateway with only a base-URL and credential change. `/v1/estimate` returns the projected cost of a request without calling upstream (see [cost-estimation-architecture.md](cost-estimation-architecture.md)); the AI-Guard routes are covered in [aiguard-architecture.md](aiguard-architecture.md).

### Model catalog

`GET /v1/models` and `GET /v1/models/{model}` return the catalog of models the gateway serves. Both require a valid virtual key (parity with every upstream provider's `/v1/models`); an unauthenticated call is rejected with `401`. The result is scoped to the key: a virtual key restricted to specific models sees only those in the list, and requesting the detail of a model outside that scope returns `404` (the model is hidden rather than revealed).

The response shape follows the caller's SDK. Send an `anthropic-version` header to get Anthropic's native `/v1/models` shape (`data[].{type:"model", id, display_name, created_at, max_input_tokens, max_tokens}` plus top-level `first_id`/`last_id`/`has_more`); otherwise the OpenAI-style `{object:"list", data:[…]}`. `GET /v1/models/{model}` returns a single entry in the same shape.

Each entry carries Nexus extension fields so a client can choose a model locally without a second round-trip: `aliases` (alternate request strings that resolve to the model), `features` (capability flags such as `vision`, `function_calling`, `json_mode`, `thinking`), the context window (`maxContextTokens`/`maxOutputTokens`, carried as the native `max_input_tokens`/`max_tokens` in the Anthropic shape), `type` plus `inputModalities`/`outputModalities`, `lifecycle` (`ga`/`preview`/`deprecated`), `capabilityJson` (embedding dimensions, batch limits), and `pricing` (configured USD-per-million-token input/output rates plus cached-input read/write rates). SDKs ignore the extension keys; any field is omitted when unset.

## 3. Cross-format translation

You do not have to match your API shape to the target provider. The gateway accepts your request in whichever supported shape your SDK speaks, translates it to whatever provider and model the routing rule selects, and translates the response back into your shape. A request to `/v1/chat/completions` can be served by an Anthropic or Gemini model, and the client still receives an OpenAI Chat Completions response.

This works through a canonical pivot. The ingress codec decodes your body into a canonical (OpenAI-shaped) representation via `CanonicalBridge.IngressChatToCanonical` (or the embeddings counterpart); routing selects the target; the target provider's adapter translates the canonical request into that provider's wire format; and on the way back the upstream response is normalized to canonical and re-encoded to your ingress shape via `ResponseCanonicalToIngress`. The translation contract — canonical equals the OpenAI shape, and each non-OpenAI adapter owns its own canonical↔wire mapping — is detailed in [provider-adapter-architecture.md](provider-adapter-architecture.md) §3a, and the normalization layer in [normalization-architecture.md](normalization-architecture.md).

Two guardrails bound this:

- **Compatibility filter** — the gateway only routes to targets the ingress shape can be translated to; an incompatible target is filtered out of the candidate set rather than producing a broken call.
- **Responses-API guard** — a `/v1/responses` request whose resolved target is not natively a Responses provider is rejected with a Responses-shaped `400`, because stateful Responses fields and OpenAI built-in tools cannot be honored over a non-Responses wire.

## 4. Choosing the model

The request's `model` field drives routing (see [routing-architecture.md](routing-architecture.md)). Send a concrete model and the gateway resolves it to a provider+model target through the active routing rules; send the `auto` sentinel to hand model selection to the LLM-dispatch smart router (see [smart-routing-architecture.md](smart-routing-architecture.md)).

Provider-specific parameters that have no clean OpenAI equivalent (for example Anthropic's `thinking` or Gemini's `thinkingConfig`) travel in the `nexus.ext.<provider>.<key>` namespace on the request body, so a single canonical request can still carry vendor extensions — see [provider-adapter-architecture.md](provider-adapter-architecture.md) §Rule 4.

## 5. Streaming

Set `stream: true` (or use a provider-native streaming path such as Gemini's `:streamGenerateContent`) to receive a Server-Sent Events stream. The stream is emitted in the event grammar of the API shape you called, regardless of which provider served it — a streamed `/v1/chat/completions` always yields Chat Completions SSE frames even when an Anthropic or Gemini model produced them.

## 6. Response and control headers

On every response the gateway stamps headers that report what happened:

- `X-Nexus-Routed-Model` / `X-Nexus-Routed-Provider` — the model code and provider the request was actually served by (useful when the requested model was `auto` or substituted by routing).
- `X-Nexus-Attempts` — how many upstream attempts were made (retry/fallback).
- `X-Nexus-Cache` — the cache outcome for the request.
- `X-Nexus-Quota-Used` / `X-Nexus-Quota-Limit` / `X-Nexus-Quota-Warning` / `X-Nexus-Quota-Downgrade` / `X-Nexus-Quota-Original-Model` — quota accounting and any quota-driven model downgrade.
- `X-Nexus-Hook` / `X-Nexus-Coerced` / `X-Nexus-Mode` — compliance-hook and request-handling annotations.

On the request side, send `x-nexus-aigw-no-cache: 1` to bypass the response cache for that call (the request still executes upstream; see [response-cache-architecture.md](response-cache-architecture.md)).

## 7. Errors

Errors are returned in the envelope of the API shape you called, with the HTTP status preserved: an OpenAI-shape route returns `{"error": {"message", "type", "code", "param"}}` and an Anthropic-shape route returns `{"type": "error", "error": {"type", "message"}}`. So an SDK's native error handling continues to work against the gateway.

## References

- `packages/ai-gateway/internal/auth/vkauth/vkauth.go` — virtual-key carriers and `nvk_` prefix
- `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go` — ingress route registration
- `packages/ai-gateway/internal/ingress/models/models.go` — model-catalog endpoints (`/v1/models`, `/v1/models/{model}`): vk enforcement, per-key filtering, response shapes
- `packages/ai-gateway/internal/execution/canonicalbridge/` — ingress↔canonical translation
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — cross-format filter, Responses guard, response headers, no-cache handling
- `packages/ai-gateway/internal/ingress/envelope/error_envelope.go` — per-ingress error envelopes
- `packages/ai-gateway/internal/providers/canonicalext/` — `nexus.ext.<provider>.<key>` extension helpers
