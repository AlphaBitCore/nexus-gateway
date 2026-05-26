# E62 — Cross-Adapter Embeddings + Multimodal Architecture Foundation

> Epic: 62
> Status: In-progress
> Date: 2026-05-19
> Architecture impact: `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md` (NEW, Tier 1 — frames every non-chat endpoint and every multimodal epic); `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` (scope note added — typology framework subsumes the chat-specific specialisation); `docs/developers/architecture/services/ai-gateway/hook-architecture.md` (multimodal-scope note added — endpoint+modality awareness lands here in S1); `docs/developers/architecture/README.md` (one new trigger row)
> SDD: `docs/developers/specs/e62/e62-s1-hook-endpoint-gating.md`, `docs/developers/specs/e62/e62-s2-canonical-embeddings-bridge.md`, `docs/developers/specs/e62/e62-s3-provider-codecs.md`, `docs/developers/specs/e62/e62-s4-traffic-event-embeddings.md`, `docs/developers/specs/e62/e62-s5-smoke-embeddings.md`, `docs/developers/specs/e62/e62-s6-cp-agent-endpoint-awareness.md`
> OpenAPI: `docs/users/api/openapi/ai-gateway/e62-s2-embeddings.yaml`
> Memory anchors: `project_e62_cross_adapter_embeddings` (to be created on epic kickoff), `project_inputstaging_shared_primitive` (E61 cross-ref), `project_local_inference_server_direction` (E61 cross-ref)
> Blocked by: none (parallel to E61; E62 does not depend on E61-S3/S4/S5/S6 being merged)
> Successor epics queued: **E63** Audio (TTS + STT); **E64** Image Generation (sync); **E65** Async Job Orchestrator; **E66** Video Generation; **E67** Modality-Aware Hooks Expansion

---

## 1. Background

The AI Gateway today serves five chat-shaped ingresses (`/v1/chat/completions`, `/v1/messages`, `/v1/responses`, Gemini `:generateContent`, OpenAI Responses streaming) with a mature canonical-bus architecture (`provider-adapter-architecture.md` §3a Rules 1-7). Three real and reproducible gaps surface when "embeddings" is treated as a peer endpoint instead of a chat afterthought:

**Gap 1 — `/smoke-gateway` excludes embeddings.** `tests/scripts/smoke-gateway.py:126-141` carries an explicit regex (`text-embedding|*embedding*|dall-e|whisper|tts-*`) that filters embedding models out of the test catalog. The 29-model × 4-ingress matrix only exercises chat-shape endpoints. A regression in `/v1/embeddings` would not be caught by any automated test today.

**Gap 2 — Embedding implementation is partial and non-conformant.** Three ingress routes exist (`/v1/embeddings`, Azure `/openai/deployments/{d}/embeddings`, GLM `/api/paas/v4/embeddings`), but only the Cohere codec implements a real embedding transform. Canonical embeddings types are absent from `providers/core/types.go`. `canonicalbridge.EndpointRoutable` explicitly rejects cross-format embeddings (`bridge.go:122-146`, citing "Until a real embeddings codec exists, only same-format routing is allowed"). The only embedding test in the entire repo is a cross-format **rejection** test (`embeddings_crossformat_test.go:69-125`), not a happy-path test.

**Gap 3 — The hook framework runs content-scanning hooks unconditionally on embedding responses, where there is no text to scan.** PII Detector, Keyword Filter, Content Safety, AI Guard, and Quality Checker all `Abstain` on float-array responses (extracted text is empty), wasting setup cost and producing misleading audit rows that stamp `decision=APPROVE` when the truth is "hook had nothing to evaluate". This is a chat-framework-applied-to-non-chat assumption and surfaces concretely with embeddings; it will surface more painfully with image / audio / video.

**Plus a strategic gap not yet exposed by code:** Nexus needs to support TTS, STT, image generation, and video generation in upcoming epics. Treating each as a one-off would replay the same architectural gaps repeatedly. **E62 is therefore framed as both (a) the embeddings epic AND (b) the multimodal-architecture-foundation epic**, establishing the shared L3 / L5 / L7 backbone that E63–E67 plug into.

**Business value — vendor-SDK independence (the core customer story).** E62's most concrete commercial benefit is **SDK abstraction**: a customer's application code that uses the OpenAI SDK can have its embedding traffic transparently routed to Cohere, Voyage, Gemini, or a local OpenAI-compatible inference server without a single line of SDK change. Today, switching embedding providers means swapping SDK clients across every service, every codebase, every CI pipeline. With Nexus + E62, the switch is a routing-rule edit in the Control Plane. This is the same value proposition Nexus already delivers for chat completions; E62 extends it to embeddings (and the same pattern then carries to image / audio / video in E63-E67).

Concrete scenarios this unlocks:

1. **Cost optimisation per language**: route Chinese / multilingual embedding traffic to Cohere multilingual-v3 (cheaper for non-English); route English traffic to OpenAI. Customer's SDK code says `text-embedding-3-small` throughout.
2. **Compliance routing**: route EU customer traffic to a Frankfurt-region provider; non-EU stays on OpenAI. No SDK fork required.
3. **Failover during provider outages**: when OpenAI's embedding endpoint degrades, routing rule + capability matrix auto-falls-back to Cohere (per the §FR-6 reject-and-fallback rule). Customer SDKs see continuity.
4. **Cross-provider experimentation**: A/B test Voyage vs OpenAI embedding quality without app-level instrumentation — both run through the same VK, same SDK, just different routing rules.
5. **Local inference adoption**: customer can deploy a local OpenAI-compatible embedding server (Whisper, vLLM, llama.cpp) and route some traffic to it. Cloud SDKs unchanged.

The same hub-and-spoke pattern that delivered this for chat now delivers it for every endpoint typology.

The user (gateway architect) explicitly raised both the embeddings smoke-coverage gap and the multimodal-architecture intent on 2026-05-19. The brainstorm-review pass on the same day locked in:
- **Endpoint typology framework** (sync JSON, stream JSON, sync binary, stream binary, async job).
- **Per-endpoint canonical** chosen from the strongest existing industry spec (OpenAI for chat/embeddings/image/TTS/STT/batch; Google Veo for video; Replicate Predictions for async-job envelope).
- **Hook framework endpoint + modality awareness** (Class A content-scanning vs Class B metadata/control).
- **Cross-format reject + routing pre-filter** as the asymmetry policy (no silent down-projection).
- **`SchemaCodec` interface extension** to support `contentType` + `[]ArtifactRef` (one breaking change now, zero breaking changes later).
- **Model capability matrix migration** (`inputModalities`, `outputModalities`, `lifecycle`, `capabilityJson`) so routing + reject work uniformly.

The full design lives in `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md`. This document captures the requirements that derive from it.

---

## 2. Functional Requirements

### FR-1: Hook framework — endpoint + modality awareness (Story S1)

| ID | Requirement | Priority |
|---|---|---|
| FR-1.1 | `HookInput` gains three new fields: `EndpointType EndpointType`, `InputModality []Modality`, `OutputModality []Modality`. `EndpointType` enum: `chat`, `embeddings`, `image_generation`, `tts`, `stt`, `video_generation`, `batch`, `job`. `Modality` enum: `text`, `image`, `audio`, `video`. Enums defined in `packages/shared/policy/hooks/core/types.go`. | Must |
| FR-1.2 | `Hook` interface gains two methods: `SupportsEndpoint(EndpointType) bool` and `SupportsModality(Modality) bool`. Default helper `ChatOnly()` (exported Go symbol, capitalised) returns true for `EndpointTypeChat` only, to preserve existing semantics for hooks that haven't been audited yet. | Must |
| FR-1.3 | Existing built-in hooks (PII Detector, Keyword Filter, Content Safety, AI Guard, Quality Checker, Webhook Forward, Rule-Pack) declare their `SupportsEndpoint` / `SupportsModality` explicitly per the Class A / Class B split in `endpoint-typology-architecture.md` §6.3. No hook silently inherits "supports everything". | Must |
| FR-1.4 | `Pipeline.BuildPipeline` filters by endpoint **at build time**, not decide time. The filter computes `hooks_for(endpoint, in_mod, out_mod)` per `endpoint-typology-architecture.md` §6.3. Skipped hooks are not constructed; they do not contribute setup cost or audit noise. | Must |
| FR-1.5 | When the pipeline drops all Class-A hooks for a given (endpoint, modality) combination, the audit row stamps `pipeline_skipped_reason="no_applicable_hooks"` instead of misleading `decision=APPROVE`. Class-B hooks continue running regardless. | Must |
| FR-1.6 | Cost stamping, Prometheus emission, audit-row writing, quota counting remain in handler main-path code (not in `Pipeline`). These are Class B by definition and run for every endpoint unconditionally. | Must |
| FR-1.7 | New Prometheus metric `nexus_hook_pipeline_skipped_total{endpoint, reason}` for ROI / debugging. `reason` values include `no_applicable_hooks`, `unsupported_modality`, `passthrough_mode`. | Should |
| FR-1.8 | Per-hook unit tests assert `SupportsEndpoint` / `SupportsModality` truth tables. Coverage ≥95% per CLAUDE.md unit-test binding. | Must |

### FR-2: Canonical embeddings types + bridge (Story S2)

| ID | Requirement | Priority |
|---|---|---|
| FR-2.1 | `packages/ai-gateway/internal/providers/core/types.go` adds `EmbeddingsRequest`, `EmbeddingsResponse`, `EmbeddingsUsage` canonical Go types, modelled on OpenAI `/v1/embeddings`. Fields on the request: `Model string`, `Input EmbeddingsInput` (string \| []string \| []int), `Dimensions *int`, `EncodingFormat *string`, `User *string`. Fields on the response: `Object string`, `Data []EmbeddingDataItem` (each carrying `Embedding []float32`, `Index int`, `Object string`), `Model string`, `Usage EmbeddingsUsage` (`PromptTokens int`, `TotalTokens int`). | Must |
| FR-2.2 | `canonicalbridge` adds `IngressEmbeddingsToCanonical(format Format, body []byte, target CallTarget) ([]byte, error)` and `ResponseCanonicalToIngressEmbeddings(format Format, canonical []byte) ([]byte, error)`. Symmetric with `IngressChatToCanonical` / `ResponseCanonicalToIngress`. | Must |
| FR-2.3 | `EndpointRoutable` opens the cross-format gate for `EndpointEmbeddings`. Gate behaviour: returns `true` iff (a) ingress format and target format are both registered AND (b) the routing pre-filter (FR-6) finds the target compatible. The old hard-coded `return ingress == target` is removed. | Must |
| FR-2.4 | `nexus.ext.<provider>.<key>` extension mechanism applies to embeddings per Rule 4. Cohere `input_type`, `embedding_types`, `truncate` and Gemini `taskType`, `title`, `outputDimensionality` ride on the canonical body via `canonicalext.Set` / `Get`. Helpers reuse existing `canonicalext/` package — no new package. | Must |
| FR-2.5 | Streaming + non-streaming parity (Rule 6) is **N/A** for embeddings — embeddings have no SSE shape, no provider supports streaming embeddings today. This decision is documented in the SDD so it does not need to be re-litigated when a future provider adds streaming embedding. | Must |
| FR-2.6 | Cross-format flow for embeddings mirrors chat flow: `IngressEmbeddingsToCanonical → adapter.PrepareBody → upstream call → adapter.DecodeResponse → ResponseCanonicalToIngressEmbeddings`. Reuses `proxy.go` handler structure; no parallel handler path. | Must |
| FR-2.7 | Per-provider unit tests assert canonical → wire → canonical round-trip is lossless on the in-scope provider×model matrix. Cross-format unit tests assert (a) compatible targets produce identical canonical output, (b) incompatible targets are rejected by the routing pre-filter before codec runs. | Must |

### FR-3: Provider codecs (Story S3)

| ID | Requirement | Priority |
|---|---|---|
| FR-3.1 | **OpenAI embeddings codec** (`packages/ai-gateway/internal/providers/specs/openai/codec/`). Identity codec for canonical → wire (canonical IS OpenAI shape). `dimensions` field is stripped on models that don't support it (ada-002); `text-embedding-3-*` honour it. Per-model rules live in this package per Rule 3. | Must |
| FR-3.2 | **Azure OpenAI embeddings codec** (`packages/ai-gateway/internal/providers/specs/azure/codec/`). Reuses OpenAI codec; only difference is URL path template (`/openai/deployments/{deployment}/embeddings?api-version=...`) and auth header (`api-key`). Capabilities track OpenAI 1:1 — same supported_dimensions per model. | Must |
| FR-3.3 | **Cohere embeddings codec** (`packages/ai-gateway/internal/providers/specs/cohere/codec/`). Canonical → wire: `input` array → `texts` array (always array); `dimensions` → ignored (Cohere models are fixed-dimension; routing pre-filter rejects mismatched dimensions before codec runs); `nexus.ext.cohere.input_type` → `input_type`; `nexus.ext.cohere.embedding_types` → `embedding_types`; `nexus.ext.cohere.truncate` → `truncate`. Wire → canonical: Cohere `embeddings` array → OpenAI `data[].embedding`; `meta.billed_units.input_tokens` → `usage.prompt_tokens`. Cohere batch limit (96) declared in `capabilityJson.embeddings.max_batch_size`. | Must |
| FR-3.4 | **Gemini embeddings codec** (`packages/ai-gateway/internal/providers/specs/gemini/codec/`). Distinguishes single-input (`:embedContent`) vs batch (`:batchEmbedContents`). Canonical `input` (string) → Gemini `:embedContent` request `{content: {parts: [{text: ...}]}}`. Canonical `input` ([]string) → `:batchEmbedContents` request with `requests` array. `dimensions` → `outputDimensionality`. `nexus.ext.gemini.taskType` → `taskType`. `nexus.ext.gemini.title` → `title`. Wire → canonical: Gemini `embedding.values` → OpenAI `data[].embedding`; `usageMetadata.totalTokenCount` → `usage.prompt_tokens` (Gemini does not split prompt/total separately for embeddings). | Must |
| FR-3.5 | Each codec declares its `capabilityJson` contribution to seed data (`tools/db-migrate/seed/seed.ts`): `max_input_tokens`, `supported_dimensions`, `default_dimension`, `max_batch_size`, supported `nexus.ext.<provider>.*` extension keys. New seed rows for at minimum: `text-embedding-3-small`, `text-embedding-3-large`, `text-embedding-ada-002`, Azure equivalents, `embed-multilingual-v3`, `embed-english-v3`, `text-embedding-004` (Gemini). | Must |
| FR-3.6 | GLM embedding codec (`/api/paas/v4/embeddings`) is **out of scope for E62** — the ingress route is preserved (no regression) but the codec lands when GLM is needed by a customer. This is recorded as a deferred follow-up. | Won't |
| FR-3.7 | Per-codec unit tests cover: (a) canonical → wire round-trip with each `nexus.ext.<provider>.*` extension; (b) per-model dimension/quirk rules; (c) wire → canonical with usage extraction; (d) error envelope shape on upstream 4xx. Coverage ≥95%. Empirical 400 citations (Rule 7) in source comments for any per-model rule. | Must |

### FR-4: traffic_event + cost stamping (Story S4)

| ID | Requirement | Priority |
|---|---|---|
| FR-4.1 | `traffic_event.endpoint_type='embeddings'` is stamped on every embedding request that reaches the gateway (hit, miss, error, dry-run). Reuses the existing `endpoint_type` column; **no schema migration**. | Must |
| FR-4.2 | Modality-specific facts (dimension actually requested, batch size, encoding_format, target model's default_dimension when request omitted it) ride in the existing `metadata` JSONB column under key `embedding`: `metadata.embedding.{dimension, batch_size, encoding_format, model_default_dimension}`. Set by the audit writer at response-stamp time. | Must |
| FR-4.3 | Cost formula registry: a new `BillableUnits` Go struct captures unit-bearing fields (`PromptTokens`, `CompletionTokens`, `ReasoningTokens`, `CachedTokens`, `Images`, `AudioSeconds`, `VideoSeconds`, `Requests`). Per-endpoint `CostFormula func(BillableUnits, *Model) decimal.Decimal` registered with the estimator. Embeddings register `embeddingCostFormula(u, m) → u.PromptTokens / 1_000_000 * m.InputPricePerMillion`. Chat keeps its existing formula (registered alongside). `proxy.go` dispatch consults the registry by `endpoint_type` — no centralised switch. The abstraction unblocks E63-E66 cost formulas (TTS per-second, image per-count, video per-second) without growing the dispatcher. Per `endpoint-typology-architecture.md` §6.5. | Must |
| FR-4.4 | `normalize/codecs/<wire>` Tier-1 extractors handle embedding response usage extraction (`prompt_tokens` / `total_tokens` aliases). OpenAI exposes both `prompt_tokens` and `total_tokens`; Cohere exposes `meta.billed_units.input_tokens` and `meta.billed_units.output_tokens` (treat output as 0 for embeddings); Gemini exposes only `usageMetadata.totalTokenCount`. Each extractor documents the alias mapping in source comments. | Must |
| FR-4.5 | Prometheus dimension `endpoint="embeddings"` is honoured on `nexus_traffic_events_total`, `nexus_request_duration_seconds`, `nexus_request_cost_usd_total`. No new metric names. | Must |
| FR-4.6 | No `prompt_cache_*` token columns are populated for embeddings (embeddings have no prompt-cache semantic). Audit writer asserts these are zero / null for `endpoint_type='embeddings'` rows; integration test enforces. | Must |
| FR-4.7 | `traffic_event.metadata.embedding.cross_format_routing` is stamped `true` when the ingress format != target format. Used by the smoke harness cross-ingress-consistency arm and by the Cache ROI dashboard's per-route routing-fanout report. | Should |

### FR-5: Smoke harness — P3E phase (Story S5)

| ID | Requirement | Priority |
|---|---|---|
| FR-5.1 | `tests/scripts/smoke-gateway.py` adds a new phase **P3E** (embeddings) parallel to the existing P3 / P3R / P3A / P3G phases. P3E runs by default (no flag required to opt in); a `--no-embeddings` flag is available for tests that genuinely don't need it. | Must |
| FR-5.2 | The `_NON_CHAT_RE` filter in `smoke-gateway.py:126-141` is replaced by classification on `Model.outputModalities`. Models with `outputModalities=["embedding"]` are routed to P3E; models with `outputModalities=["text"]` continue to P3 / P3R / P3A / P3G. Image / audio / video models continue to be skipped until E63 / E64 / E66 add their phases. | Must |
| FR-5.3 | P3E test arms (per (ingress, model) tuple): **(A) non-stream basic** — POST, verify 200, verify vector length matches expected dimension; **(B) dimensions round-trip** — request `dimensions=1024`, verify response embedding length == 1024 (skipped if model doesn't support custom dims); **(C) batch input** — submit N inputs (per model's max_batch_size), verify N vectors returned in order; **(D) traffic_event cross-check** — DB read confirms `endpoint_type='embeddings'`, `prompt_tokens>0`, `completion_tokens=0`, cost > 0 on real upstream call; **(E) Prometheus delta** — `nexus_traffic_events_total{endpoint="embeddings"}` incremented by exactly N (number of submitted requests); **(F) cross-ingress consistency matrix** — every (ingress, target) pair of distinct formats verifies (i) response is shaped per ingress format, (ii) embedding values are within float-tolerance of native ingress call. | Must |
| FR-5.4 | Per-phase reject-asymmetry test (per `endpoint-typology-architecture.md` §9 binding template arm #6): a request whose `dimensions` exceeds target's `supported_dimensions` returns HTTP 400 with `error.code="no_compatible_provider"` and `error.message` naming the offending field; no upstream call is made; no traffic_event row is created with `provider_id=<target>`. | Must |
| FR-5.5 | The cache-related arm (2-turn prompt cache) is **explicitly skipped** for P3E with a logged reason `embeddings_no_prompt_cache_semantic` — the smoke report must distinguish "skipped because not applicable" from "passed". | Must |
| FR-5.6 | Smoke run produces a markdown report at `/tmp/nexus-test/smoke-gateway-<UTC-ts>.md` with a dedicated P3E section listing per-(ingress, model) results. The cache cross-ingress matrix table extends to include embedding ingresses. | Must |
| FR-5.7 | P3E is also wired into `tests/run-all.sh` so the `/test-all` skill exercises it. | Should |

### FR-6: Cross-format reject + routing pre-filter (cross-cutting, lands in S2/S3)

| ID | Requirement | Priority |
|---|---|---|
| FR-6.1 | Routing engine's candidate-target evaluation gains a pre-filter step that consults `Model.capabilityJson.embeddings` for embedding requests. Filter checks: ingress `dimensions ∈ target.supported_dimensions` (or `dimensions is nil`); ingress `len(input) ≤ target.max_batch_size`; ingress `encoding_format ∈ target.supported_encoding_formats`; ingress `input_modalities ⊆ target.input_modalities`. Targets failing any check are dropped from the candidate pool before scoring. **A single target failing the check is NOT a client error — it is silently dropped from the candidate pool, and routing proceeds with surviving candidates.** Per `endpoint-typology-architecture.md` §5.3 fallback-chain rule. | Must |
| FR-6.2 | HTTP 400 `no_compatible_provider` fires **only when every candidate target fails the pre-filter** (the candidate pool was emptied by the filter). Error body MUST include an `available_capabilities` array listing each considered target's capability so admin can self-debug: `{"error":{"code":"no_compatible_provider","message":"No routing target supports dimensions=2048","param":"dimensions","type":"invalid_request_error","available_capabilities":[{"provider":"openai","model":"text-embedding-3-small","supported_dimensions":[512,1024,1536]},...]}}`. Per ingress format, the envelope shape adapts (OpenAI / Cohere / Gemini error shapes) but the `available_capabilities` payload is preserved. | Must |
| FR-6.3 | Codec acts as a safety net: if the routing pre-filter is bypassed (configuration drift, admin pushed an inconsistent `capabilityJson`), the codec returns a structured `400 invalid_request` rather than silently mutating user input. Codec MUST NOT down-project vectors, split batches without policy, or truncate input behind the user's back. | Must |
| FR-6.4 | The pre-filter is endpoint-agnostic in design — the same engine handles chat-specific filters in future epics (e.g. image-gen's `size` field, video-gen's `duration_s`). E62 implements the engine + embedding rules; subsequent epics add their own rule clauses. | Must |
| FR-6.5 | `capabilityJson` is hot-path read on every request. The routing engine MUST pre-parse `capabilityJson` into a typed `ModelCapabilitySnapshot` Go struct at startup + on shadow-pushed config changes (`atomic.Pointer` swap, mirroring how existing `Model` fields are cached). Pre-filter consults the snapshot, not raw JSONB — raw JSONB parse per request is unacceptable. | Must |
| FR-6.6 | Per-route policy `on_capability_mismatch: reject | warn-and-continue` (default `reject`) — admins MAY opt into best-effort routing where a missing optional parameter (e.g. `nexus.ext.cohere.input_type` for Cohere v3) is filled with a routing-rule-level default. When `warn-and-continue` fires, the response carries `x-nexus-coerced` header listing the auto-filled fields. Default-strict behaviour preserved (no surprise). | Should |
| FR-6.7 | Routing rule editor UI is **out of scope for E62** — admin sees the rejection in audit logs / smoke output. UI-level capability filter editor lands in a future UI epic. | Won't |

### FR-7: SchemaCodec interface extension (cross-cutting, S1/S2)

| ID | Requirement | Priority |
|---|---|---|
| FR-7.1 | `SchemaCodec.EncodeRequest` returns a structured `EncodeResult` (not positional values): `{Body []byte, ContentType string, Headers http.Header, URLOverride string, Rewrites []string}`. Per `endpoint-typology-architecture.md` §4.1. Rationale: per-request headers (multipart boundary, `x-goog-api-key`, custom auth), URL-template overrides (Gemini `:embedContent` vs `:batchEmbedContents`), and future controls (signed headers, trailers) extend the struct without breaking the interface again. Chat / embedding adapters set `ContentType="application/json"`, `Headers=nil`, `URLOverride=""`. | Must |
| FR-7.2 | `SchemaCodec.DecodeResponse` returns a structured `DecodeResult`: `{CanonicalBody []byte, Usage Usage, Artifacts []ArtifactRef}`. Chat / embedding adapters set `Artifacts=nil`. Future adapters populate per `endpoint-typology-architecture.md` §4.1. | Must |
| FR-7.3 | New `ArtifactRef` Go type defined in `packages/ai-gateway/internal/providers/core/types.go` with fields per `endpoint-typology-architecture.md` §4.1 (`Kind`, `MIMEType`, `URL`, `Bytes`, `Base64`, `JobID`, `Width`, `Height`, `DurationS`, `SizeBytes`). `ArtifactKind` enum: `image`, `audio`, `video`, `job`. | Must |
| FR-7.4 | All existing chat / embedding adapters are migrated to the new signature in a single PR (pre-GA, no compatibility shim). Build succeeds; chat smoke green; no behaviour change. | Must |
| FR-7.5 | `AsyncAdapter` interface shape is **NOT declared in E62** — only `JobRef`, `JobStatus`, and `ArtifactKind=job` (which appear in `ArtifactRef`) ship. The concrete `SubmitJob` / `PollJob` / `CancelJob` signature lands in **E65's SDD**, validated against the orchestrator implementation. Pre-declaring an interface we haven't validated against working code violates the "interface widens only once" commitment. | Must |
| FR-7.6 | Adapter conformance test (`/adapter-conformance-check` skill) is extended to assert the new signature on every adapter package. | Should |

### FR-9: Compliance Proxy + Agent endpoint awareness (Story S6)

The same multimodal framework must apply across all three Nexus traffic paths (AI Gateway, Compliance Proxy, Agent), because the same `shared/policy/hooks/` + `shared/transport/normalize/` + `shared/traffic/adapters/` Go code runs in each. E62-S6 establishes the **endpoint typology + canonical mapping in the CP/Agent ingress path**, mirroring how E62-S1 through S5 establish it in the AI Gateway ingress path. Per `endpoint-typology-architecture.md` §8.7.

| ID | Requirement | Priority |
|---|---|---|
| FR-9.1 | New shared package `packages/shared/traffic/classify/` exposes `EndpointClassifier.Classify(host, method, path, contentType) (EndpointType, AdapterID, ok)`. Registry-style: each provider's adapter contributes rules at `init()` time. The CP and Agent forwarder pipelines invoke `Classify` after TLS bump (CP) or after intercept-extract (Agent) and BEFORE the content-extract step. Rules registered in E62 cover OpenAI / Azure / Cohere / Gemini embedding URL patterns per `endpoint-typology-architecture.md` §8.7.3. Future endpoint epics (E63 audio, E64 image, E65 batch, E66 video) append rules; the framework does not change. | Must |
| FR-9.2 | `NormalizedPayload.Kind` enum (defined in `packages/shared/transport/normalize/core/`) gains a new value **`ai-embeddings`**. The struct gains a new typed slot `Inputs []string` (request-side text inputs). Response-side: no new fields — embedding response carries `Model` + `Usage`; **the vector array is not stored** in `traffic_event_normalized` (high volume, low forensic value, re-derivable). Per `endpoint-typology-architecture.md` §8.7.1 vector storage rule. | Must |
| FR-9.3 | Each Tier-1 adapter under `packages/shared/traffic/adapters/api/<provider>/` that the classifier maps to an embedding host (`openai`, `azure-openai`, `cohere`, `gemini`) gets a per-endpoint `Normalize` enhancement. Request side: extract the input text(s) into `NormalizedPayload.Inputs []string`. Response side: parse `usage.prompt_tokens` + `model`; do NOT decode or store the float vector. Adapter-level coverage per `endpoint-typology-architecture.md` §8.7.2 matrix. | Must |
| FR-9.4 | The same Tier-1 normalizers under `packages/shared/transport/normalize/codecs/<wire>` extend their `Normalize` to recognise `Kind=ai-embeddings`. After E58-S0's three-source consistency invariant, this guarantees AI Gateway, CP, and Agent produce **byte-identical `NormalizedPayload` values for the same upstream embedding response**. Existing fixture-based consistency test extends to embeddings fixtures. | Must |
| FR-9.5 | Hook pipeline endpoint-aware filtering (E62-S1) automatically applies to CP + Agent — they share the same `Pipeline.BuildPipeline` code path. The CP/Agent shadow-pushed `HookConfig` rows MAY declare `applicableEndpoints: [embeddings]` to scope per-endpoint; absent declaration defaults to "applies to all endpoints" for backward compatibility, with each hook's `SupportsEndpoint()` doing the per-endpoint cull. | Must |
| FR-9.6 | `interception_domain` rules (CP shadow config) gain a new optional field `applicableEndpoints: EndpointType[]` so admin can scope a rule to one endpoint without blanket-matching every endpoint on the same host (e.g. "log embedding inputs but not chat" or vice versa). Default empty list = all endpoints. | Should |
| FR-9.7 | `/test-compliance-proxy` skill adds an **embedding through-MITM** smoke arm: sends `POST /v1/embeddings` to api.openai.com through the prod Compliance Proxy host on port `:3128` (host resolved from `tests/.env.prod` per the `/test-compliance-proxy` skill contract), verifies (a) MITM bump succeeds, (b) `traffic_event` row with `source='compliance-proxy'` + `endpoint_type='embeddings'`, (c) `traffic_event_normalized.request_normalized.Kind='ai-embeddings'` with populated `Inputs`, (d) `response_normalized` has empty `data[]` field (no vectors stored), populated `usage.prompt_tokens`. Cross-check against `nexus_traffic_events_total{source="compliance-proxy",endpoint="embeddings"}` Prometheus delta. | Must |
| FR-9.8 | macOS Agent — content-aware embedding hooks **remain unavailable on macOS** in E62, identical to today's chat situation. The macOS NETransparentProxyProvider does not TLS-bump (`agent-ne-fail-open-architecture.md` + CLAUDE.md "macOS NE proxy must fail-open"); embedding traffic on macOS captures only metadata (host, IP, byte counts, process attribution). The `EndpointClassifier` is still invoked on metadata, so `traffic_event.endpoint_type='embeddings'` is correctly stamped for routing analytics; only the content-extract + hook stages are skipped on macOS. Promoting macOS to TLS-bumped intercept is tracked separately (pf-based replacement, not part of E62). | Must |
| FR-9.9 | Linux + Windows Agent — full content-aware embedding pipeline via pf / WinDivert TLS bump. Same adapter `Normalize` path as CP. Smoke coverage in E62 is **AI Gateway + CP only**; Linux/Windows agent embedding smoke is a follow-up that rides on the existing pf-intercept rollout (no agent platform code changes required for E62 — adapter changes happen in shared code, agent picks them up). | Should |
| FR-9.10 | Audit emission: CP and Agent emit `traffic_event` rows with `endpoint_type='embeddings'` identical to AI Gateway (FR-4.1). `metadata.embedding.{dimension, batch_size, encoding_format}` is populated when extractable from the wire (some providers omit batch_size from the response; CP/Agent omit the field rather than guess). | Must |
| FR-9.12 | Privacy: `NormalizedPayload.Inputs` (the captured embedding input text) is subject to the **same retention + redaction policy as chat `Messages[]`**. CP/Agent honour the existing `hook_config` `storageAction.redactSpans` rule + spillstore-overflow path documented in `data-retention-purge-architecture.md` and `pii-redaction-policy-architecture.md`. Embedding inputs are NOT a new privacy surface — they ride the same compliance pipeline as chat prompts. Admin who already configured retention for chat does NOT need to reconfigure for embeddings; the rules apply by Kind tag. | Must |
| FR-9.11 | Backward compatibility: the existing `NormalizedPayload.Messages[]` / `Tools[]` chat-shaped fields are unchanged. `Inputs []string` is purely additive. `Kind=ai-embeddings` is a new enum value; existing consumers that switch on `Kind` and have no `ai-embeddings` case will fall through to their default branch (no panic). UI renderer gets an `ai-embeddings` view in this epic (simple: list of input strings + token count + model name; no vector visualisation). | Must |

### FR-8: Model capability matrix migration (S2)

| ID | Requirement | Priority |
|---|---|---|
| FR-8.1 | Prisma migration adds four columns to `Model`: `inputModalities String[] @default(["text"])`, `outputModalities String[] @default(["text"])`, `lifecycle String @default("sync")`, `capabilityJson Json?`. Existing chat models inherit safe defaults (matching today's behaviour); no backfill of `capabilityJson` is required for chat models. | Must |
| FR-8.2 | Seed file (`tools/db-migrate/seed/seed.ts`) is extended to populate `outputModalities=["embedding"]` and `capabilityJson.embeddings = {max_input_tokens, supported_dimensions, default_dimension, max_batch_size}` for each in-scope embedding model. Per-(provider, model) values cite the upstream API doc as the source of truth in a SQL comment. | Must |
| FR-8.3 | Routing engine reads `capabilityJson.embeddings` at request time (cached snapshot, mirrors how routing reads other Model fields today). No new caching layer. | Must |
| FR-8.4 | `Model` admin API responses include the new fields. Admin UI display / edit is **out of scope** — admins manage capabilityJson via seed migrations or direct SQL in dev; a UI editor lands in a future UI epic. | Won't |
| FR-8.5 | Migration is forward-only (pre-GA, no down migration). Existing `Model` rows that pre-date the migration get defaulted via Prisma; no data loss. | Must |

---

## 3. Non-Functional Requirements

| ID | Requirement | Notes |
|---|---|---|
| NFR-1 | Embedding request latency: end-to-end p95 adds ≤ 5ms over a direct upstream call. | Same path as chat today; only adds the hook-filter step (sub-ms) + capability pre-filter (sub-ms). |
| NFR-2 | Hook pipeline savings: on an embedding response, no Class-A hook is constructed or invoked. Verified by Prometheus `nexus_hook_pipeline_skipped_total{endpoint="embeddings"}`. | This is the architectural cleanup, not a perf target — accept any p95 win as bonus. |
| NFR-3 | Per-package Go unit-test coverage ≥95% on every new / modified package (CLAUDE.md binding). | `providers/specs/openai/codec`, `providers/specs/azure/codec`, `providers/specs/cohere/codec`, `providers/specs/gemini/codec`, `shared/policy/hooks/core`, `shared/policy/pipeline`, `canonicalbridge`. |
| NFR-4 | Full-surface `/smoke-gateway --all-ingress` MUST pass before E62 closes. This is the single most important verification gate. Per CLAUDE.md AI-Gateway / traffic_event binding. | P3E + the chat phases all green. Cross-ingress matrix table populated for embeddings. |
| NFR-5 | Adapter conformance per `provider-adapter-architecture.md §3a` Rules 1-7 (generalised per `endpoint-typology-architecture.md` §4.4) for every embedding adapter. `/adapter-conformance-check` skill passes. | Codec audit is part of the verification gate. |
| NFR-6 | Backward compatibility: NONE (pre-GA per CLAUDE.md development-phase policy). | Migration is forward-only; no legacy compatibility path retained. |
| NFR-7 | No degradation of existing chat smoke pass rates. Every existing P3 / P3R / P3A / P3G arm continues to pass at the rate it passed before E62. | Verified by running smoke against `main` baseline and the E62 branch and diffing. |
| NFR-8 | The `SchemaCodec` interface widening is a one-time breaking change. After E62 ships, the interface is stable for E63 / E64 / E65 / E66 / E67. | Verified by code review: subsequent epics MUST not modify the interface — they add new typology-specific extension interfaces (e.g. `AsyncAdapter`) instead. |
| NFR-9 | Pre-edit reading per CLAUDE.md "3-doc rule" honoured for every code-phase PR: `endpoint-typology-architecture.md` + `provider-adapter-architecture.md` + relevant feature doc + `workflow/conventions.md`. | Self-policed during code-phase sessions. |
| NFR-10 | Coverage gate (`scripts/check-go-coverage.sh`) for any modified Go package must pass. Pre-existing under-95% packages do not block E62 if E62 didn't modify them; new packages MUST be ≥95%. | Per CLAUDE.md unit-test binding + the recent coverage-gate-sweep memory anchor. |
| NFR-11 | Smoke upstream cost policy (per `endpoint-typology-architecture.md` §9.1) — P3E runs against real upstream by default (cost negligible at embedding rates). The cost-policy JSON framework is created in E62-S5 with the P3E entry, so E63 (audio), E64 (image), E66 (video) phases can register their cost modes (default fixture vs `--all-upstream`) without further harness rework. | Per the multimodal cost-control rule. |

---

## 4. User Roles & Personas

- **Gateway Architect** — drives the multimodal architecture decisions. Primary persona for the architecture doc (`endpoint-typology-architecture.md`) and the canonical-selection table. Needs to be confident that E62 establishes patterns reusable in E63 / E64 / E65 / E66 / E67 without rework.
- **Gateway Operator (Admin)** — registers embedding providers, manages VKs, monitors smoke results. Needs P3E to be a routine green check, not a special-case ritual. Needs traffic_event endpoint_type discriminator to filter cost dashboards by endpoint.
- **End user (Embedding-API client)** — issues `POST /v1/embeddings` calls (OpenAI shape) and expects them to work across whichever upstream provider routing picks (assuming capability matches). Receives an OpenAI-shaped response on success regardless of upstream wire format. On capability mismatch, receives a clear 400 with field name; does NOT receive silently-altered embeddings.
- **Provider-adapter implementer (future)** — when E63 / E64 / E65 / E66 land, picks up the E62-defined `SchemaCodec` + `AsyncAdapter` interfaces and plugs in. Needs the conformance contract (`endpoint-typology-architecture.md` §4.4) and the capability matrix (§5) to be unambiguous. NFR-8 is for this persona.
- **Smoke harness maintainer** — extends `smoke-gateway.py` with each new phase. P3E in E62 sets the template (arms A–F + reject-asymmetry); subsequent phases (P3I, P3T, P3S, P3V) copy the structure. Also extends `/test-compliance-proxy` skill with an embedding through-MITM arm (E62-S6).
- **Compliance Proxy / Agent maintainer** — relies on FR-9 to ensure embedding traffic transiting CP or pf-intercept-agent surfaces with the same `Kind=ai-embeddings` + endpoint-aware hook filtering as AI Gateway. The 3-path consistency invariant (architecture §8.7) is the load-bearing guarantee for this persona.

---

## 5. Constraints & Assumptions

### Constraints

- C-1 (binding): No backward compatibility (pre-GA per CLAUDE.md development-phase policy).
- C-2 (binding): No `git stash` / `git add -A` (CLAUDE.md parallel-session safety) during the code-phase sessions that this epic spawns.
- C-3 (binding): Adapter conformance per `provider-adapter-architecture.md §3a` Rules 1-7 (generalised per `endpoint-typology-architecture.md` §4.4). No per-model logic in `spec_adapter.go`.
- C-4 (binding): Secrets are env-only (CLAUDE.md secrets rule). Embedding provider credentials use existing `Credential` table.
- C-5 (binding): English-only repository text.
- C-6 (binding): Pre-GA "no defer" policy — no `@deprecated` markers, no parallel "legacy" embedding path. Existing `embeddings_crossformat_test.go` reject-test is replaced by the new happy-path + reject suite, not augmented.
- C-7 (binding): IAM impact review for any new admin route (none introduced in E62 — provider/model CRUD reuse existing endpoints; new resource records in `Model` flow through existing settings.read/update actions).
- C-8 (binding): AI Gateway smoke run mandatory before "done" per CLAUDE.md binding.
- C-9 (binding): Configuration changes (new `Model` columns) go through `configuration-architecture.md`. The four new columns are bounded-context to the catalog table; no new configKey / system_metadata / yaml field needed in E62.

### Assumptions

- A-1: The four in-scope providers (OpenAI, Azure, Cohere, Gemini) maintain their `/v1/embeddings`-equivalent endpoints + documented capability tables for the duration of E62 implementation. Capability data is taken from upstream docs as of 2026-05-19.
- A-2: Cohere `embed-multilingual-v3` and `embed-english-v3` are the in-scope Cohere models; newer Cohere variants land in a follow-up.
- A-3: Gemini `text-embedding-004` is the in-scope Gemini model; `embedding-001` is legacy and not added to seed.
- A-4: Existing `traffic_event.endpoint_type` column is the right discriminator. (Verified before migration; if a different column name is used in production schema, migration uses that name.)
- A-5: Routing engine's per-target candidate evaluation has a hook point suitable for inserting the capability pre-filter without invasive rewrite. (Verified at S2 implementation start.)
- A-6: The `SchemaCodec` interface change is a tractable PR (≤ 50 adapter packages, mostly mechanical return-signature update).
- A-7: `/smoke-gateway` has bandwidth to add a 5th major phase (P3E) without exceeding sane runtime — embeddings calls are fast (~50ms), so per-model arm cost is < 5s. Total runtime budget ≤ 15 min full-surface; P3E budget ≤ 2 min.

---

## 6. Glossary

- **Endpoint typology** — the wire-protocol classification (A/B/C/D/E) defined in `endpoint-typology-architecture.md` §1. Drives codec interface, lifecycle service, hook applicability.
- **Per-endpoint canonical** — the chosen industry spec used as the internal hub for one endpoint. For embeddings: OpenAI `/v1/embeddings` shape.
- **Hook Class A** — content-scanning hooks that need extractable text / image / audio / video content (PII, Keyword, Safety, AI Guard, NSFW classifier, …). Filtered out at pipeline-build for endpoints / modalities that don't carry their content type.
- **Hook Class B** — metadata / control hooks that operate on transaction metadata (rate limit, audit emit, cost stamp, quota counter). Run for every endpoint unconditionally; not subject to applicability filtering.
- **Capability matrix** — the per-model `(inputModalities, outputModalities, lifecycle, capabilityJson)` tuple stored on `Model` and consulted by the routing pre-filter.
- **Routing pre-filter** — the candidate-target filter step that drops incompatible targets BEFORE scoring. Embeds the `ingress.capability ⊆ target.capability` rule.
- **Reject-asymmetry rule** — the binding policy: when ingress requirements don't fit target capabilities, return HTTP 400 with `no_compatible_provider`; do NOT silently down-project, truncate, or coerce. Established in E62 for embeddings; carries forward to every future typology (image, audio, video).
- **ArtifactRef** — the new Go struct on `SchemaCodec.DecodeResponse` return tuple, carrying binary / URL artefact references for image, audio, video, job typologies. Empty for chat / embedding.
- **AsyncAdapter** — the additional Go interface (declared in E62, implemented in E65) for Typology E endpoints (`SubmitJob` / `PollJob` / `CancelJob`).
- **Cross-format embedding routing** — when the ingress format (e.g. Cohere `/v1/embed`) differs from the target provider (e.g. OpenAI). Goes through canonical (OpenAI shape) per the same hub-and-spoke pattern as chat.

---

## 7. MoSCoW Priority Summary

**Must (in scope for E62):**

- Hook framework endpoint + modality awareness with Class A / Class B split at build time (FR-1.1–1.6, FR-1.8). Carries through to Compliance Proxy + Agent automatically because of shared hook framework code.
- Compliance Proxy + Agent endpoint awareness: classifier package, `Kind=ai-embeddings` in `NormalizedPayload`, per-adapter request-text extraction, three-source consistency invariant, CP smoke arm (FR-9.1–9.5, FR-9.7, FR-9.8, FR-9.10–9.11).
- Canonical `EmbeddingsRequest` / `EmbeddingsResponse` types in `providers/core/types.go` (FR-2.1).
- `canonicalbridge.IngressEmbeddingsToCanonical` + `ResponseCanonicalToIngressEmbeddings` (FR-2.2).
- Open the `EndpointRoutable` cross-format gate for embeddings (FR-2.3).
- `nexus.ext.<provider>.<key>` extension for embedding-specific fields (FR-2.4).
- OpenAI, Azure, Cohere, Gemini embedding codecs (FR-3.1–3.5) with conformance tests (FR-3.7).
- `traffic_event.endpoint_type='embeddings'` stamping + metadata JSONB embedding fields (FR-4.1–4.6).
- Embedding cost formula dispatch in `proxy.go` (FR-4.3).
- `/smoke-gateway` P3E phase with arms A–F + reject-asymmetry test (FR-5.1–5.6).
- Routing pre-filter engine + embedding capability rules (FR-6.1–6.4).
- `SchemaCodec` interface widening (`contentType` + `artifacts`) (FR-7.1–7.4).
- `AsyncAdapter` interface declared (no impl) (FR-7.5).
- `ArtifactRef` Go type (FR-7.3).
- `Model` capability matrix Prisma migration + seed (FR-8.1–8.2, FR-8.5).
- Routing engine reading `capabilityJson.embeddings` (FR-8.3).

**Should (nice to have, in scope if time permits):**

- New `nexus_hook_pipeline_skipped_total` Prometheus metric (FR-1.7).
- `metadata.embedding.cross_format_routing` audit field (FR-4.7).
- `tests/run-all.sh` includes P3E (FR-5.7).
- Adapter conformance check skill extended (FR-7.6).
- `interception_domain` per-endpoint scoping field (FR-9.6).
- Linux/Windows Agent embedding smoke coverage (FR-9.9).

**Could (deferred to future epics):**

- GLM `/api/paas/v4/embeddings` codec — preserve route, add codec when customer demand (FR-3.6).
- Routing rule editor UI capability-filter view (FR-6.5).
- `Model` admin UI display / edit of `capabilityJson` (FR-8.4).
- Voyage, Bedrock embedding adapters — reserve for E62+ follow-up; ingress is open to OpenAI shape so they can be added without architectural change.

**Won't (explicitly out of scope):**

- Code implementation in this session — **this document and the SDDs / OpenAPI it references are doc-only deliverables.** Code-phase work is driven in subsequent sessions, per the user's explicit instruction on 2026-05-19.
- Cross-format down-projection / dimension truncation. Reject-asymmetry rule wins; we will not silently mutate user input.
- Streaming embeddings (no provider supports them; design accommodates if one ever does — see FR-2.5).
- Image / audio / video adapter implementation — those are E63 / E64 / E66 scope.
- Async job orchestrator implementation — that is E65 scope.
- Modality-aware hook implementations (image NSFW, voice clone safety, etc.) — those are E67 scope; E62 only ships the framework hook points.

---

## 8. Open questions

None at requirements-freeze time. The brainstorm-review pass on 2026-05-19 closed:

- (Q) E62 routing — new epic vs upgrade E61-S5? → **New E62; E61-S5 stays option-a (same-format only) for unblock; E62 introduces full cross-adapter.** Settled.
- (Q) First-batch provider coverage? → **OpenAI + Azure + Cohere + Gemini.** Voyage / Bedrock deferred.
- (Q) traffic_event schema — new columns or endpoint_type only? → **endpoint_type only**; modality facts ride in `metadata` JSONB. No migration to traffic_event.
- (Q) Cross-format asymmetry handling? → **Reject + routing pre-filter.** No silent down-projection.
- (Q) Hook framework approach? → **Option 2: endpoint-aware at pipeline-build time** + `Modality` axis added simultaneously.
- (Q) `SchemaCodec` interface — extend now or post-extend later? → **Extend now** (one breaking change, zero subsequent).
- (Q) `Model` capability migration — E62 or defer? → **E62**, four columns, lockstep with the canonical-bridge work.
- (Q) Video canonical body — invent or pick a spec? → **Google Veo + `nexus.ext.<provider>.*`** (out of E62 scope but locked for E66 to reuse without re-design).
- (Q) Async-job envelope — invent or pick a spec? → **Replicate Predictions for single jobs; OpenAI Batches for batch jobs; shared Go `JobRef`.** Locked for E65 to reuse.
- (Q) This session's deliverable scope? → **Doc-only.** Code is driven in subsequent sessions.
- (Q) Should CP and Agent also handle the new endpoint typologies (raised 2026-05-19 during requirements drafting)? → **Yes.** New FR-9 group + new story E62-S6 ensure the typology framework applies uniformly across all three traffic paths. macOS Agent remains metadata-only until pf-intercept replaces NE — known limitation, tracked separately.

---

## 9. Deferred Items

> AI-queryable index of E62-scoped work that did NOT make it into Must / Should buckets. Each entry has an owner-epic or `backlog` tag. Future sessions: query this section to know what's pending without re-deriving from MoSCoW.

| ID | Item | Reason for deferral | Owner | Status |
|---|---|---|---|---|
| D-1 | GLM `/api/paas/v4/embeddings` codec implementation | Customer demand not yet validated. Ingress route is reserved (FR-3.6) so adding the codec later is a one-PR change. | `backlog` (E62 follow-up) | Open |
| D-2 | Voyage AI + Bedrock embedding adapters | Capability matrix + canonical bridge support them, but no production seed Provider rows yet. Customer demand will drive prioritisation. | `backlog` (E62 follow-up) | Open |
| D-3 | Routing rule per-rule "default extensions" UI editor | FR-6.6 establishes the schema (`on_capability_mismatch: warn-and-continue` + per-route `default_extensions` map), but admin UI surface deferred. Today admin sets via direct DB / shadow push. | UI follow-up | Open |
| D-4 | Routing-fallback admin visibility (which requests cross-format-routed silently, which fell back through candidate list) | Not yet decided whether this surfaces in admin UI vs only audit logs. FR-4.7 stamps `metadata.embedding.cross_format_routing=true` on traffic_event; UI surface deferred. | UI follow-up | Open |
| D-5 | Streaming embeddings | No provider supports streaming embeddings as of 2026-05-19; if a provider ever ships one, add `StreamEmbeddingsSession` mirroring chat. | `backlog` (future) | Open |
| D-6 | `Model.capabilityJson` admin UI display / edit | Today admins manage via seed migrations or direct SQL in dev. UI editor lands when admin-driven capability tuning becomes a customer ask. | UI follow-up | Open |
| D-7 | macOS Agent pf-intercept replacement (closes content-aware-hook gap for all endpoints, not just embeddings) | Architecturally separate effort; surfaced by E62 §8.7.2 review. Numbered as **E74** in `docs/developers/roadmap.md` (was proposed as E68 in early E62 drafts; E68 was claimed by negative-feedback before E74 was assigned). | E74 | Surfaced |
| D-8 | `AsyncAdapter` interface signature finalisation | Deferred to E65 (orchestrator) — interface validated against concrete impl before locking. FR-7.5. | E65 | Surfaced |
| D-9 | Cross-path drift detector (production sampling job) | Requires E65 orchestrator infrastructure. Documented in E62-S6 §T12 so E65 picks up. | E65 | Surfaced |
| D-10 | Per-modality content hooks (image NSFW, voice-clone safety, video frame scan) | E62 ships framework hook-points (Class-A modality applicability). Concrete hook implementations are E67 scope. | E67 | Surfaced |
| D-11 | Image / audio / video classifier rules in `packages/shared/traffic/classify/` | E62-S6 ships the framework + embedding rules. Image / audio / video rules added by E64 / E63 / E66. | E63 / E64 / E66 | Surfaced |

**Discoverability convention.** Each item has:
- A short ID (`D-N`) so commit messages / future SDDs can reference it.
- A clear owner: a future epic (e.g. `E65`, `E67`, `E74`), `UI follow-up`, or `backlog`.
- `Status: Open` means not yet picked up; `Status: Surfaced` means the owning epic's plan has been informed (e.g. drift detector recorded in E62-S6 SDD §T12).

When an owner-epic ships, its requirements PR closes the corresponding `D-N` items here (status → `Closed`, with PR ref). When `backlog` items are picked up, the owning epic's requirements file lists them in its own FR table.
