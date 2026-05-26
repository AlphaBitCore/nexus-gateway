# API AI Gateway

*Audience: integrators building applications against the Nexus AI Gateway.*

The AI Gateway exposes an OpenAI-compatible HTTP surface on `:3050`. Applications that already use the OpenAI SDK can point at Nexus by changing the base URL and replacing the OpenAI API key with a virtual key. Nexus then routes the request through the configured routing rules, runs compliance hooks, enforces quotas, and proxies to the resolved upstream provider. Every request produces a `traffic_event` row for cost, token, and cache analytics. See the capability matrix below for which features apply to which endpoint.

---

## Endpoints

### Chat completions — `POST /v1/chat/completions`

The OpenAI-format chat endpoint. Accepts `messages`, `model`, `temperature`, `max_tokens`, `stream`, `tools`, `tool_calls`, and all standard OpenAI chat parameters. Set `model: "auto"` to activate smart routing, which selects a provider based on message content, cost, and health — no other endpoint supports `"auto"`.

Non-streaming responses return `Content-Type: application/json` with the standard `ChatCompletion` shape. Streaming requests (`stream: true`) receive a `text/event-stream` SSE stream of `ChatCompletionChunk` objects, terminated by `data: [DONE]`.

### Anthropic messages — `POST /v1/messages`

The Anthropic-format chat endpoint. Accepts the Anthropic `messages` API request shape (with `role: user / assistant` and the `messages` array, not a `content` string). The Anthropic adapter canonicalizes the wire format to the OpenAI canonical shape internally; responses return in Anthropic wire format. Streaming follows the Anthropic SSE protocol.

Use this endpoint when migrating Anthropic SDK clients to Nexus without changing request serialization code.

### Responses-API — `POST /v1/responses`

The OpenAI Responses-API ingress. Accepts the `input`, `instructions`, `tools`, and `stream` fields of the OpenAI Responses-API shape. When the routing rule resolves to a target whose adapter supports the Responses-API wire format natively, the body is forwarded verbatim and stateful fields ride through. When the target uses a different provider format, Nexus canonicalizes the request and re-encodes the response in Responses-API shape on egress; stateful built-in tools are rejected pre-flight with a structured `400`.

The full request and response schema is defined in [`docs/users/api/openapi/ai-gateway/e56-s1-responses.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/ai-gateway/e56-s1-responses.yaml).

### Embeddings — `POST /v1/embeddings`

The OpenAI-format embeddings endpoint. Accepts `model`, `input` (string or array of strings), `encoding_format` (`float` or `base64`), and `dimensions` (model-dependent). Returns a list of floating-point embedding vectors. The model `"auto"` is not supported for embeddings; specify the embedding model explicitly.

Additional ingress shapes for Azure OpenAI, Cohere, and Gemini embeddings are defined in [`docs/users/api/openapi/ai-gateway/e62-s2-embeddings.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/ai-gateway/e62-s2-embeddings.yaml). Those alternate-format paths canonicalize through the same pipeline as `/v1/embeddings`.

### Model catalog — `GET /v1/models` and `GET /v1/models/{model}`

Returns the list of enabled models in an OpenAI-compatible format. No authentication is required. Each model object includes `id` (database primary key), `owned_by` (provider ID), and `owner_display_name`.

### Usage — `GET /v1/usage` and `GET /v1/usage/daily`

Returns usage counters and quota status for the authenticated virtual key. `/v1/usage` returns the current billing period totals (tokens, cost, quota remaining). `/v1/usage/daily` returns a daily time-series for the given date range (max 90 days), broken down by model and provider.

---

## Capability matrix

| Feature | `/v1/chat/completions` | `/v1/messages` | `/v1/responses` | `/v1/embeddings` |
|---|---|---|---|---|
| Streaming (SSE) | ✅ | ✅ | ✅ | ❌ |
| Function/tool calls | ✅ | ✅ | ✅ | ❌ |
| `model: "auto"` smart routing | ✅ | ❌ | ❌ | ❌ |
| Response cache | ✅ | ✅ | ✅ | ✅ |
| Prompt cache (Anthropic explicit) | ✅ | ✅ | ❌ | ❌ |
| Prompt cache (OpenAI auto) | ✅ | ❌ | ✅ | ❌ |
| Prompt cache (Google contextCache) | ✅ | ❌ | ❌ | ❌ |
| Compliance hooks | ✅ | ✅ | ✅ | ✅ |
| Quota enforcement | ✅ | ✅ | ✅ | ✅ |
| Cost tracking | ✅ | ✅ | ✅ | ✅ |
| Auth required | ✅ | ✅ | ✅ | ✅ |

---

## Authentication

All traffic-bearing endpoints authenticate via a virtual key. Two methods are accepted; the gateway checks the header first:

1. `x-nexus-virtual-key: <vk>` header — preferred, checked before the `Authorization` header.
2. `Authorization: Bearer <vk>` header — standard bearer form.

A virtual key is a bearer secret prefixed `nvk_` created via the admin UI. It encodes org/project scope, an optional model allowlist, an optional provider allowlist, and an optional quota policy. A missing, revoked, expired, or disabled virtual key returns `401`. A request blocked by a compliance hook returns `403`. A request exceeding the rate limit or budget quota returns `429` with a `Retry-After` header (rate limit only).

---

## Response headers

Every proxied response includes diagnostic headers:

| Header | Value |
|---|---|
| `x-nexus-aigw-request-id` | UUID for this request (use in support tickets) |
| `x-nexus-aigw-provider` | Upstream provider that served the request |
| `x-nexus-aigw-model` | Model ID used for the upstream call |
| `x-nexus-aigw-latency-ms` | Total gateway latency in milliseconds |
| `x-nexus-aigw-routing-rule` | Name of the routing rule applied (if any) |
| `x-nexus-cache` | `HIT` or `MISS` for the response cache |
| `x-nexus-attempts` | Number of upstream attempts (1 = first succeeded; 2+ = at least one retry or failover) |
| `x-nexus-quota-warning` | Present only when usage approaches the budget limit |

---

## Error shapes

All error responses follow the same envelope:

```json
{
  "error": {
    "message": "VIRTUAL_KEY_MISSING: vkauth: virtual key missing",
    "type": "proxy_error",
    "code": 401
  }
}
```

Common error codes:

| Code | Meaning |
|---|---|
| `401` | Virtual key missing, invalid, disabled, or expired |
| `403` | Request blocked by a compliance hook |
| `429` | Rate limit or quota exceeded |
| `400` | Invalid request (missing `model`, malformed JSON, unsupported capability) |
| `502` | All upstream providers failed |

---

## Canonical docs

- [`ai-gateway-v1.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/ai-gateway/ai-gateway-v1.yaml) — primary OpenAPI 3.1 spec (chat, embeddings, models, usage)
- [`e56-s1-responses.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/ai-gateway/e56-s1-responses.yaml) — Responses-API ingress spec
- [`e62-s2-embeddings.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/ai-gateway/e62-s2-embeddings.yaml) — multi-format embeddings ingress spec
- [`credentials-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/credentials-architecture.md) — virtual key auth model and resolution logic

**Adjacent wiki pages**: [API-Overview](API-Overview) · [API-Authentication](API-Authentication) · [AI-Gateway-Overview](AI-Gateway-Overview) · [AI-Gateway-Virtual-Keys-Quotas](AI-Gateway-Virtual-Keys-Quotas) · [AI-Gateway-Routing-Rules](AI-Gateway-Routing-Rules) · [AI-Gateway-Hooks](AI-Gateway-Hooks)
