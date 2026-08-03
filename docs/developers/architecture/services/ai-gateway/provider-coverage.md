# Provider coverage

Provider coverage is the catalog of upstream providers and models the AI Gateway can route to — across endpoint kinds (chat, embeddings, image, TTS, STT, video, realtime, rerank) — plus how a model named in a request resolves to a concrete provider, credential, and wire adapter. The catalog is database-backed and operator-defined. How an adapter translates a request once resolved is in [provider-adapter-architecture.md](provider-adapter-architecture.md).

## 1. Provider formats

Each provider family is identified by a `Format` (`packages/ai-gateway/internal/providers/core/types.go`), and every Format has a registered adapter — startup registration panics if a Format in `AllFormats()` has no spec, so the Format set and the adapter set never drift.

Coverage is not chat-only. A family declares which endpoint kinds it serves through its adapter's `RequestShapes` (the `typology.WireShape` set the adapter accepts; see [provider-adapter-architecture.md](provider-adapter-architecture.md) §1–§2 for the per-endpoint-kind canonical model). Voyage serves embeddings (`WireShapeVoyageEmbeddings`) and reranking (`WireShapeVoyageRerank`); OpenAI, Cohere, Gemini, Bedrock, Azure, and GLM serve embeddings alongside chat. `FormatOpenAIResponses` is endpoint-scoped (it serves `/v1/responses`) and is valid without belonging to `AllFormats()`. A model's own endpoint kind is recorded in `Model.type` (§2).

**Rerank coverage (`/v1/rerank`).** Two families serve the rerank endpoint kind: **Cohere** declares `WireShapeCohereRerank` (native passthrough — the canonical rerank shape IS Cohere's `/v2/rerank`, so encode/decode are near-identity) and **Voyage** declares `WireShapeVoyageRerank` (the adapter owns the canonical→Voyage translation, `top_n` → `top_k`). A rerank request routes only to a target in `{FormatCohere, FormatVoyage}`; any other target is rejected as `no_compatible_provider` (the routability gate is `canonicalbridge.Bridge.RerankRoutable`). Rerank is the one endpoint kind whose canonical shape is not OpenAI — see [provider-adapter-architecture.md](provider-adapter-architecture.md) §3a Rule 1.

The OpenAI-compatible families (Mistral, xAI, Groq, Perplexity, Together, Fireworks, Moonshot, DeepSeek, HuggingFace) each carry a distinct Format for per-vendor metrics and policy while reusing the OpenAI codec (see [provider-adapter-architecture.md](provider-adapter-architecture.md) §8).

## 2. The database-backed catalog

Coverage is defined by three tables (`tools/db-migrate/schema/providers.prisma`), mirrored as Go structs in `packages/ai-gateway/internal/platform/store`:

- **Provider** — `adapter_type` (the `Format`, and the sole adapter-dispatch key), `baseUrl`, `pathPrefix`, `apiVersion`, `region`, `enabled`, custom `headers`, and per-provider streaming and traffic-capture policy overrides (null inherits the global policy).
- **Model** — `code` (the customer-facing identifier clients send) versus `providerModelId` (the string sent upstream); `providerId`; `type` (chat / embedding / image / audio / tts / stt / rerank / video / realtime); `features`; `inputModalities` / `outputModalities`; pricing (`inputPricePerMillion`, `outputPricePerMillion`, and the cached-input read and write rates); `maxContextTokens` / `maxOutputTokens`; `status` and `lifecycle`; `aliases` (alternate codes that resolve to the same row); and `capabilityJson` for non-chat capability descriptors.
- **Credential** — the encrypted upstream key (`encryptedKey` + `encryptionIv` + `encryptionTag`) linked to a Provider, with pool selection weight, status, and health and circuit state.

There is no global builtin provider catalog in code: providers and models are operator-defined through the Control Plane, so coverage is per-deployment rather than a fixed list. The local development seed ships a sample provider and model set as demo data, not a shipped catalog.

## 3. Where capabilities live

A model's capabilities are read from several places:

- **Model columns** — `features`, `inputModalities` / `outputModalities`, `type`, `status`, `lifecycle`, the pricing fields, and the token limits.
- **`capabilityJson`** — a per-model descriptor for non-chat endpoints (such as embedding input limits and dimensions), unmarshalled by `packages/ai-gateway/internal/routing/capability`. Chat models leave it null; their capability is implied by `features`.
- **Adapter backstops** — a protocol-required field a provider demands is filled by the adapter that owns that wire, reading the value from the catalog rather than restating it. For example `specs/anthropic/codec` synthesizes and clamps `max_tokens` — which only Anthropic requires — from the `maxOutputTokens` column carried on the resolved target, the same ceiling `/v1/models` advertises (see [provider-adapter-architecture.md](provider-adapter-architecture.md) §5).

## 4. The /v1/models surface

`packages/ai-gateway/internal/ingress/models/models.go` serves the model list directly from the enabled-model catalog, filtered to the calling virtual key's allowed-model set. The response shape is negotiated: an Anthropic-shaped list when the request carries an `anthropic-version` header, otherwise the OpenAI `{ "object": "list", "data": [...] }` shape. Both carry a Nexus model-classification field (`type` in the OpenAI shape, `modelType` in the Anthropic shape) plus `inputModalities` / `outputModalities`, so a consumer can distinguish chat / embedding / image / tts / stt / video / realtime / rerank models without a second lookup; the Anthropic-shaped list additionally carries `max_input_tokens` / `max_tokens` (from `maxContextTokens` / `maxOutputTokens`).

## 5. Model-to-provider resolution

`packages/ai-gateway/internal/providers/target/resolver.go` is the single entry point that turns a request's model into a concrete call. `Resolve` looks up the Provider and Model, validates both are enabled and linked, selects a credential from the provider's pool, and assembles a `CallTarget`: `Format` (cast from `Provider.adapter_type`), `BaseURL`, the decrypted `APIKey`, `CredentialID`, `ProviderModelID` (the upstream wire value), and an `Extras` map for provider-specific config such as an Azure deployment name. The executor reads `Format` from the `CallTarget` to select the adapter — it never re-derives the adapter from the provider name, so the dispatch key has a single source.

## References

- `packages/ai-gateway/internal/providers/core/types.go` — Format constants, AllFormats, CallTarget
- `packages/ai-gateway/internal/platform/store/` — Provider / Model store structs
- `tools/db-migrate/schema/providers.prisma` — Provider, Model, Credential models
- `packages/ai-gateway/internal/routing/capability/` — capabilityJson parsing
- `packages/ai-gateway/internal/ingress/models/models.go` — /v1/models surface
- `packages/ai-gateway/internal/providers/target/resolver.go` — model-to-provider resolution into CallTarget
