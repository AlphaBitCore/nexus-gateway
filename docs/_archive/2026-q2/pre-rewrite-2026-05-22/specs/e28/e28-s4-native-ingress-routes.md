# E28 — Story 4: Native ingress routes and schema detection

## Context

With the adapter stack and caller migration done, add the **five** native ingress families so vendor SDKs can point at the gateway unchanged using their default base URL:

- **Anthropic** — `/v1/messages`
- **Gemini** — `/v1beta/models/{model}:generateContent` and `:streamGenerateContent`
- **Azure OpenAI** — `/openai/deployments/{deployment}/chat/completions` (+ embeddings)
- **MiniMax** — `/v1/text/chatcompletion_pro`
- **GLM (ZhipuAI)** — `/api/paas/v4/chat/completions` (+ `/api/paas/v4/embeddings`)

DeepSeek SDKs default to the OpenAI-compat path verbatim (`/v1/chat/completions`), so no dedicated ingress is mounted; clients keep using the OpenAI-compat surface and the routing rule lands on the DeepSeek provider.

**Bedrock and Vertex native ingress are out of scope** for this round (their AWS SigV4 / GCP service-account JWT flows do not map onto the single-token VK model — see requirements FR-13a). They are reachable as **egress** providers only via the OpenAI-compat ingress.

Schema detection is **path-based** (authoritative) with `X-Nexus-Body-Format` as an explicit override only on the generic OpenAI-compat family. No body content sniffing.

## User Story

**As a** tenant developer,
**I want** to point the Anthropic SDK (or Gemini, Azure, MiniMax SDK) directly at the Nexus AI Gateway with my VK and use it unchanged,
**so that** existing application code works without migrating to the OpenAI interface, and my content still goes through Nexus compliance and routing.

## Tasks

### 1. HTTP route table — `packages/ai-gateway/cmd/ai-gateway/main.go`

Register these handlers (all point at the same generic `ProxyHandler` with a decorator that injects the detected `BodyFormat` and `Endpoint` into the request context):

| Method & path | Detected `(Endpoint, BodyFormat)` | Notes |
|---|---|---|
| `POST /v1/chat/completions` | `(chat_completions, openai)` | Honors `X-Nexus-Body-Format` to override (e.g. for dev testing). DeepSeek SDK clients land here unchanged. |
| `POST /v1/embeddings` | `(embeddings, openai)` | Same override rule. |
| `GET  /v1/models` | `(models, openai)` | |
| `POST /v1/messages` | `(chat_completions, anthropic)` | Accepts `x-api-key` as a VK carrier (see §3). |
| `POST /v1beta/models/{model}:generateContent` | `(chat_completions, gemini)`, `Stream=false` | `{model}` populates request-scoped `RequestedModel`. |
| `POST /v1beta/models/{model}:streamGenerateContent` | `(chat_completions, gemini)`, `Stream=true` | `?alt=sse` is preserved and forwarded. |
| `POST /openai/deployments/{deployment}/chat/completions` | `(chat_completions, azure-openai)` | `{deployment}` populates `RequestedModel`; `?api-version=` is preserved and fed into `CallTarget.Extras["azure.apiVersion"]`. |
| `POST /openai/deployments/{deployment}/embeddings` | `(embeddings, azure-openai)` | |
| `POST /v1/text/chatcompletion_pro` | `(chat_completions, minimax)` | MiniMax native. |
| `POST /api/paas/v4/chat/completions` | `(chat_completions, glm)` | GLM (ZhipuAI) native — body is OpenAI-compat plus GLM extensions (`do_sample`, `tools[].type=web_search`, `request_id`). |
| `POST /api/paas/v4/embeddings` | `(embeddings, glm)` | GLM native embeddings. |
| `GET  /api/paas/v4/models` | `(models, glm)` | GLM native model listing. |

The URL path map is the single source of detection. The `X-Nexus-Body-Format` header is only consulted on `/v1/*` OpenAI-compat routes and must evaluate to a registered `Format`; otherwise the gateway returns 400.

### 2. Ingress model resolution

For native routes where the model name lives in the URL (Gemini `{model}`, Azure `{deployment}`) the handler extracts it and treats it as the client-requested model for routing. Routing rules match on this model name as they do today.

For body-carrying native routes that still embed the model in the body (Anthropic's `"model": "claude-3-..."`, MiniMax's `"model": "abab..."`), the handler reads the model from the body's top-level `model` JSON field. Reading is a single-pass `gjson` lookup — no full parse — to keep passthrough cheap.

Extraction utility lives in `packages/ai-gateway/internal/handler/ingress_model.go` with format-specific implementations. One function per `Format` enum value; a registry-keyed lookup in the handler picks the right extractor.

### 3. VK authentication on native routes — `packages/ai-gateway/internal/handler/auth.go`

Extend the VK extractor to accept all of these carriers, in this precedence order:

1. `x-nexus-virtual-key: <vk>` (always honored, all routes).
2. `Authorization: Bearer <vk>` (always honored, all routes).
3. Provider-conventional native carriers, accepted **only on their matching native route**:
   - `/v1/messages` (Anthropic): `x-api-key: <vk>`.
   - Gemini native routes: `x-goog-api-key: <vk>` header, or `?key=<vk>` query parameter.
   - Azure native routes: `api-key: <vk>` header.
   - MiniMax: the standard `Authorization: Bearer <vk>` carrier already covers MiniMax's SDK convention; no extra rule.
   - GLM native routes: GLM SDK uses `Authorization: Bearer <jwt>` natively, which collides with our standard bearer carrier — the gateway accepts `Authorization: Bearer <vk>` directly (the VK is a Nexus-issued bearer; GLM's per-request JWT is a vendor concern resolved server-side via `provtarget.Resolver`).
4. Fallback 401 as today.

Once extracted, the VK is resolved by the existing VK store (no change to the VK model).

### 4. Cross-format routing (OpenAI chat hub)

After routing resolves the target `(providerID, providerFormat)` for **`chat_completions`**:

- If `ingressFormat == providerFormat`: pass through on the wire (executor still runs `DecodeResponse` → canonical OpenAI internally; the handler reshapes back to ingress for non-streaming native clients).
- If `ingressFormat` is **OpenAI-shaped** (`openai`, `deepseek`, `glm`, `azure-openai`): any registered provider target is allowed; the existing `SchemaCodec.EncodeRequest` path maps canonical → provider wire.
- If `ingressFormat` is **Anthropic**, **Gemini**, or **Vertex** (Vertex uses the same `generateContent` JSON as Gemini): targets are allowed when the hub can convert **ingress → canonical OpenAI chat JSON → provider wire** (`canonicalbridge` + per-format codecs). Unsupported pairs still return HTTP 400 `no_compatible_provider`.
- **Embeddings** and **models** endpoints: unchanged legacy matrix — same wire format, or OpenAI ingress only.

Observability: rejected pairs still emit `nexus_ai_gateway_schema_mismatch_total{ingress,provider}`.

### 5. Response shaping on native ingress

For **non-streaming** `chat_completions`, the adapter stack produces **canonical OpenAI** `chat.completion` JSON from `SchemaCodec.DecodeResponse`. The proxy then calls the hub **response** mapper so the bytes written to the client match the **ingress** wire format (Anthropic Messages response envelope, Gemini `generateContent` response envelope, or OpenAI-shaped bodies for OpenAI-compat ingresses).

**Streaming:** when ingress and upstream SSE framing are not both OpenAI-shaped bodies on the client path, the gateway returns HTTP 400 `cross_format_stream_unsupported` until streaming transcoders exist.

### 6. `/internal/routing-simulate` update — `handler/routing_simulate_endpoint.go`

Extend the response:

```json
{
  "request": {
    "modelId": "...",
    "endpointType": "chat/completions",
    "ingressBodyFormat": "anthropic"
  },
  "targets": [
    {
      "providerId": "...",
      "providerFormat": "anthropic",
      "schemaMode": "passthrough"   // "passthrough" | "translated" | "rejected"
    }
  ]
}
```

`schemaMode` per target: `passthrough` when `ingressBodyFormat == providerFormat`, `translated` when the hub or legacy codec path applies (including native ingress → foreign provider on chat), `rejected` when no route exists. When `routing-simulate` is wired with a nil bridge (tests only), behavior matches the legacy matrix. Existing "missing VK context" warning (E20) is unchanged.

### 7. Unit + integration tests

Package `packages/ai-gateway/internal/handler`:

1. `ingress_detect_test.go` — table: every path in §1 → expected `(Endpoint, BodyFormat, Stream)`; `X-Nexus-Body-Format` on `/v1/chat/completions` overrides; on other paths it is ignored.
2. `ingress_model_test.go` — Anthropic body → `model` extracted; Gemini `{model}` → extracted; Azure `{deployment}` → extracted; malformed body → 400.
3. `auth_native_test.go` — `x-api-key` accepted on `/v1/messages`, ignored on `/v1/chat/completions`; `?key=` accepted on Gemini route; etc.
4. `cross_format_reject_test.go` — Anthropic-ingress + OpenAI-only routing rule → 400 with `no_compatible_provider`.
5. `proxy_passthrough_test.go` — Anthropic-ingress + Anthropic provider (fake upstream): request bytes match what the fake saw (no JSON round-trip); response bytes match what the fake sent.

Integration (`cmd/ai-gateway/integration_test.go`): spin up a fake upstream server per provider format; issue real HTTP requests to each native ingress path with a seeded VK; assert status 200 and content-type correctness.

## Acceptance Criteria

- All five native ingress route families (§1) respond 200 for a well-formed request when a matching provider is routed.
- `X-Nexus-Body-Format` respected only on the OpenAI-compat `/v1/*` paths.
- `x-api-key` works as a VK carrier on `/v1/messages` and is ignored on `/v1/chat/completions`.
- `?key=<vk>` works on Gemini native routes and is ignored on every other path.
- `go test -race -count=1 ./packages/ai-gateway/internal/handler/... ./packages/ai-gateway/cmd/ai-gateway/...` passes.
- Simulate endpoint returns `ingressBodyFormat` and per-target `schemaMode` for the five new ingress paths.
- Cross-format routing yields HTTP 400 `no_compatible_provider` for an Anthropic-ingress → OpenAI-only routing fixture.
- A Bedrock-ingress request (none of the routes in §1 match; falls through to `/`) is rejected with HTTP 404 — no native Bedrock path exists this round.

## Out of scope

- Bedrock and Vertex **native ingress** (separate epic; egress to them is in scope through the OpenAI-compat surface).
- Gemini function-calling tool-shape mapping beyond what was already covered by the s2 adapter tests. (Smoke-level tests here; deep tool-calling conformance is an adapter-quality story for later.)
- New embeddings providers on native routes beyond what the matching adapter exposes.
- UI changes in control-plane-ui are out of scope (simulate panel already renders unknown fields — a minor label for `ingressBodyFormat` can be added in a follow-up UI ticket).
