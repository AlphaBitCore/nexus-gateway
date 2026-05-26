# E62-S4 — traffic_event + Cost Stamping for Embeddings

> Story: e62-s4
> Epic: 62
> Status: In-progress
> Date: 2026-05-19
> Requirements: `docs/developers/specs/e62/e62-cross-adapter-embeddings.md` §FR-4
> Architecture: `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md` §7 (traffic_event polymorphism), §8.7.1 (NormalizedPayload Kind extension); `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` (cost formula dispatch); `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` (traffic_event schema authority); `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` §5 (token-field stamp sweep — binding)
> Memory: `project_e62_cross_adapter_embeddings`, `feedback_token_field_handler_sweep`
> Blocked by: S2 (canonical types), S3 (codec produces Usage)
> Blocks: S5 (smoke asserts the audit fields)

---

## User Story

As a **gateway operator monitoring cost + usage**, I want every embedding request to stamp `traffic_event.endpoint_type='embeddings'`, populate modality-specific facts in `metadata` JSONB, and compute `estimated_cost_usd` from the embedding-specific formula — so that Cache ROI / cost dashboards correctly attribute embedding spend and so that no completion-side fields are accidentally populated for embedding rows.

---

## Tasks

### T1 — `endpoint_type` stamping

- T1.1 In `packages/ai-gateway/internal/ingress/proxy/proxy.go` (handler main path), the `endpoint_type` field of the in-progress `traffic_event` row is set from the resolved `Endpoint` constant: `EndpointEmbeddings` → `"embeddings"`. Existing dispatch logic already does this for chat; verify the code path covers embeddings and add if missing.
- T1.2 Cache writes and reads (`proxy_cache.go`) propagate `endpoint_type` correctly. Per `feedback_token_field_handler_sweep`, the cache code is one of the 5 stamp sites — confirm at code-phase that the cache layer treats embeddings the same way it treats chat for the endpoint_type field. (Cache-hit replay should preserve the endpoint_type, since it's per-request, not per-response.)
- T1.3 Streaming responses are out of scope for embeddings (FR-2.5 says no provider streams embeddings). Stream stamp sites are unaffected; no parity work needed.

### T2 — `metadata` JSONB embedding-specific facts

- T2.1 The audit-emit code path in `packages/ai-gateway/internal/platform/audit/` (or wherever the audit writer lives — verify at impl time) stamps the following metadata keys on embedding rows:
  - `metadata.embedding.dimension` (int) — actual response embedding length from `data[0].embedding`. If the client didn't request a specific `dimensions`, this is the model's default dimension.
  - `metadata.embedding.requested_dimension` (int, nullable) — what the client requested via `dimensions` parameter, NULL if omitted.
  - `metadata.embedding.batch_size` (int) — number of input items (1 for single string, N for array).
  - `metadata.embedding.encoding_format` (string) — `"float"` or `"base64"` (from request; default `"float"`).
  - `metadata.embedding.model_default_dimension` (int) — from `Model.capabilityJson.embeddings.default_dimension` — for analytics ("how often do clients override the default").
- T2.2 Source of truth for `metadata.embedding.dimension` is the actual response. If the response carries no vectors (rare error case where Tier-1 normalizer produces empty `data`), stamp the model's default_dimension and add `metadata.embedding.warning="empty_data_array"`.
- T2.3 `metadata.embedding.cross_format_routing` (bool) is set true when the ingress format differs from the target provider's wire format. Computed at handler dispatch time.

### T3 — Cost formula dispatch via `BillableUnits` registry

- T3.1 In `packages/ai-gateway/internal/execution/estimator/`, add `BillableUnits` + `CostFormula` per `endpoint-typology-architecture.md` §6.5:
  ```go
  type BillableUnits struct {
      PromptTokens     int
      CompletionTokens int
      ReasoningTokens  int
      CachedTokens     int
      Images           int       // image-gen output count (E64)
      AudioSeconds     float64   // TTS / STT (E63)
      VideoSeconds     float64   // video-gen (E66)
      Requests         int       // batch row count (E65)
  }
  type CostFormula func(units BillableUnits, model *Model) decimal.Decimal
  ```
- T3.2 A per-endpoint formula registry: `estimator.RegisterFormula(EndpointType, CostFormula)`. E62 registers:
  - `EndpointTypeChat` / `EndpointTypeResponsesAPI` → existing chat formula (extracted to function, not changed in behaviour).
  - `EndpointTypeEmbeddings` → `func(u BillableUnits, m *Model) decimal.Decimal { return decimal.New(int64(u.PromptTokens),0).Mul(m.InputPricePerMillion).Div(decimal.NewFromInt(1_000_000)) }`.
- T3.3 The handler's cost-stamp call (`proxy.go` ~line 2184 — verify at impl time) consults the registry:
  ```go
  formula := estimator.Lookup(resolved.EndpointType)
  cost := formula(units, model)
  ```
  No centralised switch in `proxy.go`. Future epics (E63 audio, E64 image, E66 video) call `RegisterFormula` for their typology — dispatcher unchanged.
- T3.4 `traffic_event.estimated_cost_usd` populated as today. The existing column accepts `decimal(20,8)` per E58 — no schema change.
- T3.5 Embeddings registration populates only `BillableUnits.PromptTokens` (other fields stay zero). Future epics populate their own units fields per typology.

### T4 — Forbid completion-side stamping for embeddings

- T4.1 The audit writer asserts (in a debug-mode panic-on-mistake; in prod, a logged warning + zero-coerce) that for `endpoint_type='embeddings'` rows, all of the following are zero / null:
  - `completion_tokens`
  - `cache_read_tokens`
  - `cache_creation_tokens`
  - `reasoning_tokens`
  - `reasoning_cost_usd`
- T4.2 If any of these is non-zero, log `slog.WarnContext(ctx, "embedding row carries chat-only field", "field", X, "value", Y)`. The row is still written; the field is coerced to zero.
- T4.3 This is a defence-in-depth check, not a behaviour requirement — codec correctness in S3 ensures these fields are never populated in the first place. T4.1's guard exists to catch future regressions.

### T5 — Tier-1 normalizer Usage extraction

- T5.1 Each `shared/normalize/codecs/<wire>_embeddings.go` (created in S3 T6) produces `NormalizedPayload.Usage` with `PromptTokens` + `TotalTokens` populated. `CompletionTokens=0`.
- T5.2 Per-wire aliases:
  - OpenAI / Azure: `usage.prompt_tokens` direct, `usage.total_tokens` direct.
  - Cohere: `meta.billed_units.input_tokens` → `PromptTokens` AND `TotalTokens` (Cohere doesn't distinguish for embeddings).
  - Gemini: `usageMetadata.totalTokenCount` → `PromptTokens` AND `TotalTokens` (Gemini doesn't split).
- T5.3 Each alias is cited in source comment with date observed.

### T6 — Prometheus dimensions

- T6.1 Existing `nexus_traffic_events_total{endpoint, status, provider, model}` metric — `endpoint` label honours `"embeddings"`. No new metric name.
- T6.2 Existing `nexus_request_duration_seconds{endpoint, ...}` — same.
- T6.3 Existing `nexus_request_cost_usd_total{endpoint, ...}` — same.
- T6.4 New label values rolled out automatically via the audit-emit code path; no scrape config change.

### T7 — Tests

- T7.1 Unit test: dispatch cost formula on `endpoint_type='embeddings'` returns `EstimateEmbeddingCost` result.
- T7.2 Unit test: `EstimateEmbeddingCost` calculates correctly for known input (e.g., 1000 tokens × $0.02/M = $0.00002).
- T7.3 Integration test (synthetic upstream): submit an embedding request, verify the resulting traffic_event row carries:
  - `endpoint_type='embeddings'`
  - `prompt_tokens>0`, `completion_tokens=0`, `total_tokens=prompt_tokens`
  - `estimated_cost_usd > 0`
  - `metadata.embedding.dimension` populated
  - `metadata.embedding.batch_size` matches request
  - `cache_read_tokens=0`, `cache_creation_tokens=0`, `reasoning_tokens=0`
- T7.4 Cross-format integration test: submit a Cohere ingress request that routes to OpenAI target; verify `metadata.embedding.cross_format_routing=true`; verify cost is computed against the **target** (OpenAI) model's pricing, not the ingress model.
- T7.5 Defence-in-depth test: synthesise a bogus `Usage{CompletionTokens=5}` for an embedding row; assert the audit writer logs a warning and zeroes the field.
- T7.6 Prometheus delta test: submit N requests; assert `nexus_traffic_events_total{endpoint="embeddings"}` increments by exactly N.
- T7.7 Coverage ≥95% on modified packages.

### T8 — Documentation

- T8.1 Update `cost-estimation-architecture.md` with an "Embedding cost formula" section linking to `endpoint-typology-architecture.md` §7.
- T8.2 Update `audit-pipeline-architecture.md` to note that `metadata.embedding.*` keys land per E62-S4 and the per-endpoint metadata pattern is the future-extension hook for E63/E64/E65/E66.

---

## Acceptance Criteria

- A1: `traffic_event.endpoint_type='embeddings'` is stamped on every embedding request (hit, miss, error, dry-run, cross-format).
- A2: `metadata.embedding.{dimension, requested_dimension, batch_size, encoding_format, model_default_dimension, cross_format_routing}` populated correctly per the rules in T2.
- A3: `estimated_cost_usd > 0` on miss, computed from `(prompt_tokens / 1M) * Model.inputPricePerMillion`.
- A4: `completion_tokens=0`, `cache_read_tokens=0`, `cache_creation_tokens=0`, `reasoning_tokens=0` on embedding rows. Audit writer enforces.
- A5: Cross-format requests stamp cost against the **target** model's pricing.
- A6: Prometheus metric `nexus_traffic_events_total{endpoint="embeddings"}` increments per request.
- A7: Three-source consistency: AI Gateway, Compliance Proxy, Agent (where applicable) produce identical `metadata.embedding.*` fields for the same upstream response.
- A8: Coverage ≥95% on modified packages.

---

## Out of Scope (S4)

- New `traffic_event` columns — explicitly out (FR-4 says JSONB only).
- New Prometheus metric names — reuse existing.
- Cost calibration / pricing updates — handled via existing Model row management.
- Cache ROI dashboard updates to surface embedding cost separately — out of E62 (existing dashboards filter by endpoint_type automatically; specific embedding view is a follow-up if needed).

---

## Implementation Notes

- The "audit writer asserts no completion-side fields" rule (T4) is the safety net for the codec correctness in S3. Don't replace the codec's correctness with the assertion — the assertion catches future regressions but the codec is the source of truth.
- The cross-format cost decision (T7.4) — bill against the target model — mirrors `cost-estimation-architecture.md`'s existing rule for chat. Make explicit so it doesn't surprise admins on the ROI dashboard.
- The `metadata.embedding.dimension` field is the **response** dimension. If a client requests `dimensions=1024` and the model emits 1024, both fields equal 1024. If client omits `dimensions`, `requested_dimension=NULL` and `dimension=model_default_dimension`.
- The Prisma migration for the four `Model` capability columns (S2) doesn't touch `traffic_event`. S4 is purely behavioural code change + audit-writer enhancement.
- The token-field stamp sweep rule (`feedback_token_field_handler_sweep`) applies: 5 stamp sites must agree on embedding fields. Since embeddings have no completion / cache / reasoning tokens, the 5 sites must all stamp zeros consistently. Cache write and cache read sites need the same zero-stamping logic; otherwise cached embedding hits could produce divergent reads.
