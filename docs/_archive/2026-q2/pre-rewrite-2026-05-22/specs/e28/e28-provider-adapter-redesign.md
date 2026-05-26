# E28 — AI Gateway provider adapter redesign

**Status:** Active — 2026-04-24
**Epic:** 28

## 1. Business goal

The AI Gateway must serve three kinds of traffic over a single adapter layer:

1. **OpenAI-compat ingress** — arbitrary SDKs pointed at `/v1/chat/completions` etc.
2. **Provider-native ingress** — SDKs that only speak a specific schema (Anthropic SDK → `/v1/messages`, Google GenAI SDK → Gemini `:generateContent`, Azure OpenAI SDK → `/openai/deployments/...`, MiniMax native SDK → `/v1/text/chatcompletion_pro`).
3. **Internal LLM calls** — smart routing's router-LLM selector and AI Guard's `configured_provider` backend, which need the same credential / URL / schema plumbing the proxy uses.

Today the adapter interface conflates transport and schema, and every internal caller re-implements credential lookup. That has produced three live defects (hard-coded `/v1/chat/completions` in smart routing, path concatenation bug in `BaseAdapter`, AI Guard duplicating credential resolution) and blocks any native ingress beyond OpenAI. E28 rebuilds the adapter layer as a composable Transport × SchemaCodec × StreamDecoder × ErrorNormalizer model, exposes the four native ingress families, and funnels every internal caller through a single `TargetResolver`.

Pre-GA: no backward compatibility; the old `BaseAdapter` hook-based pattern is deleted in the same PR.

## 2. User roles

| Role | Need |
|------|------|
| **Application developer (Nexus tenant)** | Point any major vendor SDK at the gateway without rewriting request bodies. Continue to auth with Nexus VK, including via provider-native header names where the SDK won't let them be overridden. |
| **Platform engineer (Nexus)** | Add a new provider by writing four small components (Transport, SchemaCodec, StreamDecoder, ErrorNormalizer) and a table entry — no base class to subclass, no hidden hooks. |
| **Compliance officer** | Every request, regardless of ingress schema, is inspected by the same hook pipeline and logged with the same signal columns. |
| **Smart routing / AI Guard maintainer** | Make an LLM call by name `(providerID, modelID)` and a canonical `Request` without hand-resolving credentials or URLs. |

## 3. Functional requirements

| ID | Requirement | MoSCoW |
|----|-------------|--------|
| FR-1 | Define a single public `Adapter` contract: `Execute(ctx, Request) (*Response, error)` and `Probe(ctx, CallTarget) (*ProbeResult, error)`. No other exported hooks. | Must |
| FR-2 | `Request` carries `Endpoint` (`chat_completions`, `embeddings`, `models`, `completions_legacy`), `BodyFormat` (`openai`, `anthropic`, `gemini`, `azure-openai`, `minimax`, `glm`, `deepseek`, `bedrock`, `vertex`), the raw body bytes, headers, stream flag, and a resolved `CallTarget` (`ProviderID`, `ProviderName`, `BaseURL`, `APIKey`, `ProviderModelID`). | Must |
| FR-3 | Provide `AdapterSpec{Transport, SchemaCodec, StreamDecoder, ErrorNormalizer}` as the declarative unit for registering an adapter. A generic `specAdapter` composes these into the public `Adapter`. | Must |
| FR-4 | `Transport` owns URL construction (no caller concatenates `BaseURL + path`), authentication header rewrite, HTTP client, and `Probe`. | Must |
| FR-5 | `SchemaCodec` converts between provider-native `Format` and canonical OpenAI shape for both request and response bodies. Identity codec is the default when ingress and target formats already match (passthrough fast path). | Must |
| FR-6 | `StreamDecoder` returns a uniform `Chunk{Delta, ToolCallDeltas, Usage, Done, RawBytes}` stream regardless of the native event wrapping (OpenAI SSE, Anthropic SSE, Gemini NDJSON). `RawBytes` is forwarded to the client in native format without re-wrapping. | Must |
| FR-7 | `ErrorNormalizer` translates provider error envelopes into a canonical `ProviderError{Status, Code, Type, Message, RetryAfter}` used by the executor and hook pipeline. | Must |
| FR-8 | Expose a `TargetResolver` that, given `(providerID, modelID)` plus request context, returns a ready-to-use `CallTarget`. It handles credential vault decryption, health-aware key rotation, and provider-model ID mapping. | Must |
| FR-9 | Rewire all internal callers to `TargetResolver` + `Adapter.Execute`: (a) target executor, (b) smart routing router-LLM call, (c) AI Guard `configured_provider` backend. Delete in-caller credential lookups. | Must |
| FR-10 | AI Guard's `external_url` backend is **not** migrated; it stays as a plain HTTP client because it targets customer-owned services outside Nexus' provider catalog. | Must |
| FR-11 | Schema detection is **path-based**: `/v1/chat/completions` → openai; `/v1/messages` → anthropic; `/v1beta/models/{model}:generateContent` → gemini; `/openai/deployments/{deployment}/...` → azure-openai; `/v1/text/chatcompletion_pro` → minimax. `X-Nexus-Body-Format` header overrides only when the path is the generic OpenAI-compat family. No body content sniffing. | Must |
| FR-12 | Expose native ingress endpoints for this round, one path per vendor SDK's default base URL: Anthropic `/v1/messages`, Gemini `/v1beta/models/{model}:generateContent` and `:streamGenerateContent`, Azure OpenAI `/openai/deployments/{deployment}/chat/completions` (+ `/embeddings`), MiniMax `/v1/text/chatcompletion_pro`, GLM `/api/paas/v4/chat/completions` (+ `/api/paas/v4/embeddings`). | Must |
| FR-13 | DeepSeek SDKs ship with the OpenAI-compat path (`/v1/chat/completions`) as their default; no dedicated ingress is mounted for DeepSeek. Clients point the OpenAI SDK's base URL at the gateway and traffic resolves to the DeepSeek provider via the routing rule's normal path. | Must |
| FR-13a | Bedrock and Vertex native ingress are **not** exposed in this round. Their vendor SDKs require AWS SigV4 / GCP service-account JWT, which does not map onto the single-token VK model. They are reachable as **egress** providers only — clients send OpenAI-shaped bodies; the gateway translates and signs to the upstream. Bedrock / Vertex native ingress is a separate epic. | Must |
| FR-14 | On `/v1/messages` accept `x-api-key: <nexus VK>` as an alternative VK carrier to `x-nexus-virtual-key` and `Authorization: Bearer`. The other native ingresses honor their vendor-conventional VK carriers (`x-api-key` for Anthropic; `Authorization: Bearer` / `api-key` header for Azure; `x-goog-api-key` or `?key=` for Gemini; `Authorization: Bearer` for MiniMax; `Authorization: Bearer` for GLM). | Must |
| FR-15 | Cross-format routing is rejected with **HTTP 400 `no_compatible_provider`** when the ingress `BodyFormat` cannot be served by any resolved target (e.g. Anthropic native body routed to an OpenAI-only provider). Automatic translation is deferred. | Must |
| FR-16 | Hook content extraction uses `shared/traffic.AdapterRegistry` looked up by detected ingress `BodyFormat`. AI Gateway registers builtins via `shared/traffic/adapters.RegisterBuiltins` at startup and removes the hardcoded `openai-compat` default. The mapping `Format → traffic adapter ID` is one-to-one (`openai`→`openai-compat`, `deepseek`→`deepseek`, `glm`→`glm`, `anthropic`→`anthropic`, `gemini`→`gemini`, `azure-openai`→`azure-openai`, `minimax`→`minimax`, `bedrock`→`bedrock`, `vertex`→`vertex`); no Format piggybacks on another's traffic adapter. | Must |
| FR-16a | The `generic-jsonpath` traffic adapter has no provider-side counterpart. It exists only as a host-pattern fallback for unknown LLM destinations the data plane sees in the wild and is not exposed as a routable Format. AI Gateway never selects it; compliance-proxy and agent continue to use it as their fallback for unknown hosts. | Must |
| FR-17 | Provider registration at startup is declarative: each built-in adapter contributes one `AdapterSpec`; the registry rejects unknown provider types at startup (no silent "fall back to openai"). Nine specs are registered: openai, deepseek, glm, azure-openai, anthropic, gemini, minimax, bedrock, vertex. | Must |
| FR-18 | `Probe` is wired into the target executor's health tracker, replacing the ad-hoc `TestConnectivity` path. | Should |
| FR-19 | Simulate endpoint (`/internal/routing-simulate`) surfaces the detected ingress `BodyFormat` and the chosen `(providerFormat, schemaCodecMode)` per target (`passthrough` / `translated` / `rejected`). | Should |

## 4. Non-functional requirements

| ID | Requirement |
|----|-------------|
| NFR-1 | Go 1.25+, idiomatic Go: interface-small; error wrapping with `%w`; no panics in library code; `log/slog` structured logging. |
| NFR-2 | Adding a tenth adapter must require only a new `AdapterSpec` and a registry entry; no changes to any caller, the executor, or the handler. |
| NFR-3 | Passthrough fast path: when ingress and target format match, zero JSON marshal/unmarshal of the body on the hot path. Memory allocation budget ≤ one copy of the body for hook extraction. |
| NFR-4 | Streaming response latency (first chunk to client) must not regress by more than 2 ms p95 vs. current proxy under the same provider. |
| NFR-5 | All new code covered by `go test -race -count=1 ./packages/ai-gateway/...` and Vitest where control-plane-ui touches the flow (routing simulate panel). |
| NFR-6 | Pre-GA: old `BaseAdapter`, `PrepareRequestFn`/`ParseResponseFn`/…, `GetProviderBaseURL`, `defaultTestConnectivity`, and the hard-coded `openai-compat` default in the handler are **deleted** in this epic — no `@deprecated` shims, no phased rollout. |
| NFR-7 | English-only for all code comments, doc strings, error messages, and spec prose per `CLAUDE.md`. |

## 5. Glossary

| Term | Meaning |
|------|---------|
| **Format** | Wire-level body shape: `openai`, `anthropic`, `gemini`, `azure-openai`, `minimax`, `glm`, `deepseek`, `bedrock`, `vertex`. Matches `shared/traffic/adapters` IDs. |
| **Endpoint** | Semantic operation: `chat_completions`, `embeddings`, `models`, `completions_legacy`. |
| **CallTarget** | Fully resolved upstream locator: `{ProviderID, ProviderName, BaseURL, APIKey, ProviderModelID}`. |
| **AdapterSpec** | Declarative tuple `{Transport, SchemaCodec, StreamDecoder, ErrorNormalizer}` that composes into a public `Adapter`. |
| **Passthrough** | Upstream call that forwards the client's raw body bytes without re-marshaling (possible only when ingress Format == target Format). |
| **Cross-format routing** | Ingress Format ≠ target Format. Deferred — returns 400 in this round. |

## 6. Constraints

- **Pre-GA, greenfield**: delete obsolete paths; no parallel legacy routes; no feature flags for rollback (rollback is `git revert`).
- Credential vault decryption continues to happen inside AI Gateway; `TargetResolver` reads via the existing `credential` store layer.
- `shared/traffic.AdapterRegistry` is the single source of truth for content extraction per format; provider adapters must not grow a parallel content-extractor API.
- `/internal/routing-simulate` stays mounted on the admin BFF surface and continues to work without a VK (already warns about missing VK context per E20).

## 7. Out of scope (this epic)

- **Update (hub routing):** cross-format **chat** translation for Anthropic/Gemini ingress is specified in [e28-hub-canonical-routing.md](./e28-hub-canonical-routing.md). Remaining gaps (streaming transcoders, MiniMax native hub ingress, etc.) stay out of scope until specified there.
- AI Guard `external_url` backend rewrite — left untouched.
- Bedrock and Vertex **native ingress** (their VK ↔ AWS / GCP credential model needs its own epic). Egress to Bedrock and Vertex from OpenAI-shape ingress IS in scope.
- MiniMax / GLM dedicated content-extraction adapter refactor inside `shared/traffic/adapters` — those adapters already exist and are reused as-is.
- A separate `generic` provider type on the AI Gateway side. `generic-jsonpath` stays a traffic-side fallback only.
- New streaming modes beyond the existing passthrough / live / buffer set in `shared/streaming`.
