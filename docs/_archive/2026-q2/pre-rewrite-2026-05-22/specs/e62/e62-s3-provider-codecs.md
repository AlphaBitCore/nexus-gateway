# E62-S3 — Provider Codecs (OpenAI, Azure, Cohere, Gemini)

> Story: e62-s3
> Epic: 62
> Status: In-progress
> Date: 2026-05-19
> Requirements: `docs/developers/specs/e62/e62-cross-adapter-embeddings.md` §FR-3
> Architecture: `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md` §2 (per-endpoint canonical), §4 (SchemaCodec contract), §4.4 (conformance Rules 1-7 generalised); `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` §3a (Rules 1-7 binding); `docs/developers/architecture/services/ai-gateway/normalization-architecture.md` (decode-via-shared contract)
> Memory: `project_e62_cross_adapter_embeddings`
> Blocked by: S2 (canonical types + bridge + capability matrix + SchemaCodec interface)
> Blocks: S4 (audit needs codecs to produce usage), S5 (smoke exercises codecs)

---

## User Story

As a **gateway implementer**, I want production-quality embedding codecs for OpenAI, Azure OpenAI, Cohere, and Gemini that conform to `provider-adapter-architecture.md` §3a Rules 1-7 — so that any (ingress, target) pair among the four providers works for both native and cross-format routing, and so that future providers (Voyage, Bedrock-Cohere, etc.) plug in by mimicking these four.

---

## Tasks

### T1 — OpenAI embedding codec

- T1.1 `packages/ai-gateway/internal/providers/specs/openai/codec/embeddings.go` — new file (or extend existing `codec.go` with embedding-specific methods).
- T1.2 `EncodeRequest(EndpointEmbeddings, canonicalBody, target)` is **identity** (canonical IS OpenAI shape). Apply per-model rules per Rule 3:
  - If `target.ModelID` ∈ ada-002 family (regex `^text-embedding-ada-002.*$`), strip `dimensions` and `encoding_format` from the wire body. Source comment cites observed 400 (date + message) per Rule 7. (Verify exact error text from `api.openai.com` at impl time.)
  - If `target.ModelID` ∈ text-embedding-3-* family, honour `dimensions` as-is. Validate against `Model.capabilityJson.embeddings.supported_dimensions` — if user submitted a value not in the list, return `400 invalid_request_error` (this is the codec safety net per S2 T5.5).
- T1.3 `DecodeResponse(EndpointEmbeddings, nativeBody, "application/json")` delegates to `canonicalbridge.DecodeViaShared(raw, FormatOpenAI, EndpointEmbeddings)` per the E58-S0 contract. The Tier-1 normalizer (`shared/normalize/codecs/openai_embeddings.go` — new file in T6) handles usage extraction and field projection.
- T1.4 `EncodeRequest` returns `EncodeResult{Body, ContentType:"application/json", Headers:nil, URLOverride:"", Rewrites}`. `DecodeResponse` returns `DecodeResult{CanonicalBody, Usage, Artifacts:nil}`. Per E62-S2 §T3.1.
- T1.5 URL template: `{base}/v1/embeddings`. Method: POST. Auth: `Authorization: Bearer <api_key>` from credential.
- T1.6 Unit tests cover: (a) canonical → wire round-trip for each model family; (b) ada-002 dimension/encoding_format strip with rewrite-stamp; (c) text-embedding-3-* honouring dimensions; (d) safety-net rejection for unsupported `dimensions`.

### T2 — Azure OpenAI embedding codec

- T2.1 `packages/ai-gateway/internal/providers/specs/azure/codec/embeddings.go` — new file or extension.
- T2.2 Reuses the OpenAI canonical → wire transformation 1:1 (identity codec on the body). Differences:
  - URL template: `{base}/openai/deployments/{deployment}/embeddings?api-version={version}` where `deployment` and `version` come from the Provider record.
  - Auth: `api-key: <api_key>` header (not Bearer).
  - Per-model rules track OpenAI 1:1 — `text-embedding-3-small` deployed on Azure honours the same `supported_dimensions`. Capability data in seed mirrors OpenAI.
- T2.3 `DecodeResponse` delegates to `DecodeViaShared(raw, FormatAzureOpenAI, EndpointEmbeddings)`. The Tier-1 normalizer is the same as OpenAI's (Azure ships byte-identical response shape); registry mapping registers `FormatAzureOpenAI` → `openai_embeddings` normalizer.
- T2.4 Unit tests parallel OpenAI's, plus a `FormatAzureOpenAI` ingress → OpenAI target cross-format test.

### T3 — Cohere embedding codec

- T3.1 `packages/ai-gateway/internal/providers/specs/cohere/codec/embeddings.go` — new file (the existing `codec.go` has placeholder embedding handling per the brainstorm Explore finding; refactor or extend).
- T3.2 `EncodeRequest(EndpointEmbeddings, canonicalBody, target)`:
  - Canonical `input` (string) → Cohere `texts` (always array): `{"texts": ["..."]}`.
  - Canonical `input` ([]string) → Cohere `texts` ([]string): `{"texts": [...]}`.
  - Canonical `input` ([]int tokens) → unsupported by Cohere; safety-net rejects with `400 invalid_request_error` field=`input` reason=`token_array_unsupported_by_cohere`.
  - Canonical `model` → wire `model`.
  - Canonical `dimensions` → **ignored** (Cohere models are fixed-dimension); pre-filter would have already rejected mismatches, codec asserts as safety net.
  - Canonical `encoding_format` → Cohere `embedding_types`: if `float` → `["float"]`; if `base64` → no direct Cohere equivalent → safety-net 400.
  - Canonical `nexus.ext.cohere.input_type` (extracted via `canonicalext.Get`) → wire `input_type` (one of `search_document`, `search_query`, `classification`, `clustering`). Required field for Cohere v3 models — if missing AND target is v3, return `400 missing_required_extension` (reason: Cohere v3 models reject requests without `input_type`).
  - Canonical `nexus.ext.cohere.embedding_types` → wire `embedding_types` ([]string).
  - Canonical `nexus.ext.cohere.truncate` → wire `truncate` (one of `NONE`, `START`, `END`). Default `END` per Cohere docs.
- T3.3 `DecodeResponse(EndpointEmbeddings, nativeBody, "application/json")` delegates to `DecodeViaShared(raw, FormatCohere, EndpointEmbeddings)`. Mapping (in the Tier-1 normalizer):
  - Cohere `embeddings` (array of arrays, or object with `float`/`int8` keys when multiple embedding_types) → OpenAI canonical `data[].embedding` (the float array; if multiple types requested, prefer `float`).
  - Cohere `meta.billed_units.input_tokens` → canonical `usage.prompt_tokens`. `usage.total_tokens` = same value (Cohere doesn't distinguish).
  - Cohere `meta.api_version.version` → discarded (not surfaced to client).
  - Cohere `response_type` / `id` → discarded.
- T3.4 URL template: `{base}/v1/embed`. Method: POST. Auth: `Authorization: Bearer <api_key>`.
- T3.5 Unit tests: (a) input as string vs []string round-trip; (b) input_type extension passthrough; (c) v3 model missing input_type → 400; (d) []int token unsupported → 400; (e) cross-format from OpenAI ingress (string input + no input_type) to Cohere target — assert canonical adds default `nexus.ext.cohere.input_type="search_document"` per the routing pre-filter's compatibility check (S2 T5.2 enforces required extensions exist; routing pre-filter rejects upstream if not provided by ingress). Decision in implementation: if pre-filter rejects, codec never sees the request, so the unit test is a pre-filter test, not a codec test.

### T4 — Gemini embedding codec

- T4.1 `packages/ai-gateway/internal/providers/specs/gemini/codec/embeddings.go` — new file.
- T4.2 `EncodeRequest(EndpointEmbeddings, canonicalBody, target)`:
  - Distinguish single vs batch on canonical `input`:
    - `input` (string) → URL path `:embedContent`; body `{"content":{"parts":[{"text":"..."}]}}`.
    - `input` ([]string) → URL path `:batchEmbedContents`; body `{"requests":[{"content":{"parts":[{"text":"..."}]}},...]}`.
    - `input` ([]int tokens) → unsupported by Gemini; safety-net 400.
  - Canonical `dimensions` → wire `outputDimensionality` (request key). Honoured per `Model.capabilityJson.embeddings.supported_dimensions`.
  - Canonical `nexus.ext.gemini.taskType` → wire `taskType` (one of `RETRIEVAL_QUERY`, `RETRIEVAL_DOCUMENT`, `SEMANTIC_SIMILARITY`, ...). If missing, default `RETRIEVAL_QUERY`.
  - Canonical `nexus.ext.gemini.title` → wire `title` (used by `RETRIEVAL_DOCUMENT` task type).
  - Canonical `model` → URL path model id (`models/text-embedding-004`).
- T4.3 `DecodeResponse(EndpointEmbeddings, nativeBody, "application/json")` delegates to `DecodeViaShared(raw, FormatGemini, EndpointEmbeddings)`. Mapping:
  - Single `:embedContent` response: `embedding.values` (float array) → canonical `data[0].embedding`.
  - Batch `:batchEmbedContents` response: `embeddings[].values` → canonical `data[].embedding` preserving index order.
  - Gemini `usageMetadata.totalTokenCount` → canonical `usage.prompt_tokens` AND `usage.total_tokens` (Gemini doesn't split).
- T4.4 URL templates: `{base}/v1/models/{model}:embedContent` and `{base}/v1/models/{model}:batchEmbedContents`. The codec returns `EncodeResult.URLOverride` set to the chosen suffix (`:embedContent` for single input, `:batchEmbedContents` for batch); transport layer composes against `{base}/v1/models/{model}{URLOverride}`. Auth: codec sets `EncodeResult.Headers["x-goog-api-key"]` (when credential supplies it) or transport falls back to `?key={api_key}` query (when admin configures that auth scheme); both modes carry forward existing chat behaviour.
- T4.5 Unit tests: (a) single → `:embedContent`; (b) batch → `:batchEmbedContents`; (c) taskType extension; (d) outputDimensionality; (e) batch order preservation; (f) cross-format (OpenAI ingress []string → Gemini target → batchEmbedContents).

### T5 — Adapter registration + Manifest

- T5.1 Each adapter's `init()` registers the embeddings endpoint in `Manifest.RequestShapes`:
  ```go
  Manifest: Manifest{
      ProviderID:    "openai",
      RequestShapes: []string{"chat-completions", "responses-api", "embeddings"},
      ...
  }
  ```
- T5.2 The capability bridge (`bridge.codecs` map) registers each adapter's codec for `EndpointEmbeddings`.
- T5.3 Existing adapter tests assert `RequestShapes` contains `"embeddings"` for the four in-scope providers.

### T6 — `shared/normalize/codecs/<wire>_embeddings.go` Tier-1 normalizers

Per E58-S0 contract: response-decode goes through `shared/normalize`. Embeddings need new normalizers.

- T6.1 `packages/shared/transport/normalize/codecs/openai_embeddings.go`:
  - Parses `{"object":"list","data":[{"embedding":[...],"index":0,"object":"embedding"}],"model":"...","usage":{"prompt_tokens":N,"total_tokens":N}}`.
  - Produces `NormalizedPayload` with `Kind="ai-embeddings"`, `Inputs=nil` (response side), `Model=string`, `Usage.PromptTokens`, `Usage.TotalTokens`. Vector data not stored (per E62-S6 FR-9.2).
  - For decode-via-shared from `spec_openai/codec`, the codec post-processes the `NormalizedPayload` back into canonical OpenAI bytes (or, more directly, projects via `canonicalbridge.ProjectToCanonical` analog for embeddings).
- T6.2 `packages/shared/transport/normalize/codecs/openai_embeddings.go` is reused for Azure (registered under `FormatAzureOpenAI`).
- T6.3 `packages/shared/transport/normalize/codecs/cohere_embeddings.go` — parses Cohere response shape, projects to canonical OpenAI shape.
- T6.4 `packages/shared/transport/normalize/codecs/gemini_embeddings.go` — parses both `embedContent` and `batchEmbedContents` response shapes.
- T6.5 Each normalizer implements the `normalize.Normalizer` interface and registers via `RegisterDefaultAIBuiltins(reg)` extension.
- T6.6 Three-source consistency test (per `normalization-architecture.md` cross-component invariant): for every fixture under `testdata/embeddings/<wire>/*.json`, the result of `shared/normalize.Registry.Normalize` AND `canonicalbridge.DecodeViaShared` AND `shared/traffic/adapters/<wire>.Adapter.Normalize` produce byte-identical NormalizedPayload + Usage.

### T7 — Error envelope

- T7.1 Embedding errors flow through the existing `handler.encodeErrorEnvelopeForIngress` helper. Each ingress format's envelope is produced per Rule 9.5 in `provider-adapter-architecture.md`. New responsibility: codec safety-net 400 errors (T1.2, T3.2, T4.2) must reach this helper, not bypass it.
- T7.2 No new error class needed. Existing `ProviderError` types cover `invalid_request_error`, `no_compatible_provider`, etc.

### T8 — Tests + coverage

- T8.1 Per-codec unit tests as listed in T1-T4.
- T8.2 Cross-format integration tests: for every (ingress, target) ∈ {OpenAI, Azure, Cohere, Gemini}² where ingress != target AND target supports the request, assert the full canonical bridge → codec → upstream-fixture → decode → response-reshape pipeline produces the expected ingress-shaped response.
- T8.3 Coverage ≥95% per codec package. Per CLAUDE.md unit-test binding.
- T8.4 Adapter-conformance check skill (`/adapter-conformance-check`) — extend to verify Rules 1-7 for each embedding codec. Add to test plan.

### T9 — Documentation

- T9.1 Each codec's source file has a top-of-file comment block linking to `endpoint-typology-architecture.md` §2 (canonical), `provider-adapter-architecture.md` §3a Rule references, and the upstream provider doc URL + observed-date for each per-model rule.
- T9.2 Update `provider-adapter-architecture.md` §11 (Conformance gaps) — no new gaps introduced; if any are surfaced, append.

---

## Acceptance Criteria

- A1: Each of OpenAI / Azure / Cohere / Gemini has a working embedding codec implementing the extended `SchemaCodec` interface.
- A2: Per-model rules (ada-002 dimension strip; Cohere v3 input_type required; Gemini taskType default) carry source comments with empirical 400 citations (Rule 7).
- A3: Cross-format routing for every pair of in-scope providers works: ingress format ↔ target format = N+M pattern, not N×M.
- A4: New `shared/normalize/codecs/<wire>_embeddings.go` normalizers produce byte-identical NormalizedPayload across `shared/normalize.Registry`, `canonicalbridge.DecodeViaShared`, and `shared/traffic/adapters/<wire>.Adapter.Normalize`. Three-source consistency test passes.
- A5: Adapter Manifest `RequestShapes` includes `"embeddings"` for all four providers.
- A6: Codec safety-net rejection produces correctly-shaped error envelope per ingress format. No hand-rolled JSON.
- A7: Coverage ≥95% per codec package.
- A8: `/adapter-conformance-check` skill passes for each embedding codec.

---

## Out of Scope (S3)

- GLM embedding codec — deferred (FR-3.6).
- Voyage / Bedrock embedding codecs — deferred to E62 follow-up.
- Per-model price seeding for new embedding models — handled by the existing price-seeding mechanism; pricing values referenced per provider doc at impl time, NOT in this SDD body.
- Streaming embedding — N/A (FR-2.5).
- Image / audio / video codecs — E64 / E63 / E66.
- Admin UI for codec selection — N/A (codec is auto-selected by adapter registry).

---

## Implementation Notes

- The "decode goes through shared/normalize" rule (Rule 8) is the architectural commitment that makes the N+M translation work. Don't be tempted to duplicate parsing logic in `specs/<provider>/codec/embeddings.go` — delegate.
- The Cohere `input_type` mandatory-on-v3 rule has been verified by Cohere docs as of 2026-05-19. If a future Cohere v4 changes this, update the rule's source comment + the empirical citation.
- The Gemini `:embedContent` vs `:batchEmbedContents` dispatch is on canonical `input` shape: single string → single endpoint; array → batch. Gemini does NOT support a single-element batch request gracefully (returns the same content shape but with batchEmbedContents URL). Use single endpoint for single-input requests for clarity.
- Azure URL template needs `{deployment}` and `{api-version}` parameters that come from the Provider record, not the Model record. Verify the Provider schema carries these (or add them in S2 if missing).
- Cohere returns `embeddings` as the float array (not `embedding` like OpenAI). The normalizer handles the rename.
- Gemini `embedContent` response uses `embedding.values` (note the singular `embedding` object containing `values`). The normalizer handles this.
- Three-source consistency test is the critical verification — it's the gate that prevents AI Gateway, Compliance Proxy, and Agent from drifting on the same upstream response. Don't skip it.
