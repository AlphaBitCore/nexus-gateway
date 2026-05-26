# E61-S5 — Embedding Provider via Existing Provider System

> Story: e61-s5
> Epic: 61
> Status: Draft
> Requirements: `docs/developers/specs/e61/e61-smart-response-cache.md` §FR-3
> Architecture: `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` §3.2; `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` §3a (rules 1-7 apply)
> Memory: `project_local_inference_server_direction`
> Blocked by: e61-s2 (schema migration introduces endpointType=embedding)
> Blocks: e61-s3 (writer needs the adapter), e61-s4 (reader needs the adapter)

## User Story

As a Gateway Admin, I want to register an embedding provider (OpenAI cloud or my own local OpenAI-compatible server) the same way I register an LLM provider, with VK + credential + adapter, so my semantic-cache embedding calls follow the same observability, retry, and policy machinery as any other AI call — and so the local inference server I'm planning to deploy (routing + embedding + ai-guard, one box) drops in without a new framework.

## Tasks

### T1 — endpointType extension

- T1.1 `packages/shared/schemas/...` — add `embedding` to the EndpointType enum (or whatever the current type is — find it via grep at implementation time).
- T1.2 Provider table accepts rows with `endpointType=embedding`. Existing rows (chat, completions) keep working.
- T1.3 Admin API accepts CRUD for embedding providers; UI changes deferred to S6 (where the picker lives).

### T2 — OpenAI embeddings adapter

- T2.1 The OpenAI adapter (`packages/ai-gateway/internal/providers/specs/openai/`) already covers `/v1/chat/completions` and `/v1/responses`. Add `/v1/embeddings` support:
    - Codec for the embeddings request shape: `{model, input, dimensions?, encoding_format?}`.
    - Codec for the embeddings response shape: `{object, data:[{embedding, index, object}], model, usage:{prompt_tokens, total_tokens}}`.
    - Non-streaming only — embeddings have no SSE shape.
- T2.2 Honour adapter conformance Rules 1-7:
    - Rule 1: canonical shape is `EmbeddingsRequest{Model, Input, Dimensions, EncodingFormat}` / `EmbeddingsResponse{Embeddings [][]float32, Model, PromptTokens}`. Add to `packages/ai-gateway/internal/providers/core/types.go`.
    - Rule 2: OpenAI adapter owns full canonical↔wire translation.
    - Rule 3: per-model wire quirks (e.g., text-embedding-3-large optional `dimensions` parameter) live in the adapter.
    - Rule 4: provider extensions ride on `nexus.ext.openai.*` if needed (not expected for embeddings).
    - Rule 5: cross-format callers (semantic cache writer/reader) canonicalize via `canonicalbridge.IngressEmbeddingsToCanonical` (new helper — analogous to `IngressChatToCanonical`).
    - Rule 6: streaming + non-streaming parity — N/A (embeddings are non-stream-only).
    - Rule 7: any model prefix rule needs an observed-400 citation.
- T2.3 Test virtual key for embeddings — add to `tools/db-migrate/seed/seed.ts` or wherever the seed VKs live, so dev/test can call `/v1/embeddings` through the gateway.

### T3 — Local OpenAI-compatible adapter

- T3.1 Reuse the OpenAI adapter for "local OpenAI-compatible". The only difference is `baseURL` — admin sets it per provider row. No new code needed if the local server speaks the OpenAI Embeddings API faithfully.
- T3.2 Seed a `provider=local-inference-embeddings` row with `baseURL=http://localhost:8080` (admin edits at deploy time). Disabled by default. UI in S6 lets admin enable.
- T3.3 The local server may serve embeddings, routing-decision LLM, AND ai-guard from one process — but each is a separate Provider record from the gateway's perspective. This is documented in `project_local_inference_server_direction` memory; no code change beyond the standard Provider CRUD.

### T4 — Embedding-specific Executor path

- T4.1 The standard `executor.Execute` path runs PrepareBody → upstream call → response decode. For embeddings, the response shape differs from chat-completions but `Execute`'s contract is generic; verify the existing path handles `endpointType=embedding` correctly. If not, add a parallel `executor.ExecuteEmbeddings` or extend `Execute` with a polymorphic codec selector. Decision in implementation.
- T4.2 Cost stamping uses `Model.InputPricePerMillion` (already on the Model row). Embeddings have no output tokens; the cost formula is `(input_tokens / 1_000_000) * inputPrice`.

### T5 — Admin endpoint for embedding model probe

- T5.1 `/admin/providers/:id/embedding-probe` — admin clicks "test" in the Cache Settings UI, gateway issues a synthetic embedding call to confirm the provider works. Returns `{ok, dimension, latencyMs, sampleTokens}`. Useful for debugging cloud vs local setups.
- T5.2 IAM action — same as the existing provider-test action (no new IAM resource carve-out).

### T6 — Tests

- T6.1 OpenAI embeddings adapter codec round-trip test (canonical → wire → canonical).
- T6.2 Test with a fixture matching text-embedding-3-small response shape: dimension 1536, expected usage counts.
- T6.3 Local server adapter test using the same OpenAI codec against a different baseURL (httptest server returning identical JSON shape).
- T6.4 Embedding-probe admin endpoint test.
- T6.5 Adapter conformance check passes for the new endpointType (`/adapter-conformance-check` skill should be re-runnable post-S5).
- T6.6 Coverage ≥95%.

## Acceptance Criteria

- A1: Admin can register an OpenAI embedding provider via the existing Provider CRUD with `endpointType=embedding`.
- A2: Admin can register a local OpenAI-compatible embedding provider with a custom baseURL.
- A3: An `executor.Execute` call against an embedding provider returns a canonical `EmbeddingsResponse`.
- A4: The semantic-cache writer + reader (S3 + S4) consume the embedding adapter through the standard Adapter interface — no semantic-cache-specific code path in the adapter layer.
- A5: `/adapter-conformance-check` skill passes for the OpenAI embeddings adapter.
- A6: The test VK can call `/v1/embeddings` end-to-end through the gateway.

## Out of Scope (S5)

- The semantic-cache write/read paths that consume this adapter — S3 + S4.
- UI for selecting an embedding provider per route — S6.
- The local inference server's deployment (vLLM/Ollama/LiteLLM choice) — admin-managed, not a code task.
- A separate `EmbeddingProvider` Go interface that introduces a parallel abstraction — explicitly rejected. The standard `Adapter` interface is sufficient.
