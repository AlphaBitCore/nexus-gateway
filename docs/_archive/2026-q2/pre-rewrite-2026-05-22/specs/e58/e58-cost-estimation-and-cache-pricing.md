# E58 — Cost Estimation, Cache Pricing & Unified Usage Extraction

> Epic: 58
> Status: Draft
> Date: 2026-05-16
> Architecture impact: `docs/users/product/architecture.md` § 12 "Cost, pricing & estimation"; new doc `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md`; extension of `docs/developers/architecture/services/ai-gateway/normalization-architecture.md` § "Ai-gateway codec delegation (E58-S0)"; new rule in `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` § 3a Rule 8.
> SDD (per-story):
> - `docs/developers/specs/e58/e58-s0-unified-protocol-parser.md`
> - `docs/developers/specs/e58/e58-s1-cache-pricing-and-reasoning.md`
> - `docs/developers/specs/e58/e58-s2-estimator-core.md`
> - `docs/developers/specs/e58/e58-s3-dry-run-flag.md`
> - `docs/developers/specs/e58/e58-s4-compare-endpoint.md`
> OpenAPI:
> - `docs/users/api/openapi/admin/e58-s1-cache-pricing-admin.yaml`
> - `docs/users/api/openapi/ai-gateway/e58-s3-dry-run.yaml`
> - `docs/users/api/openapi/ai-gateway/e58-s4-estimate-compare.yaml`

---

## 1. Background

Three interlocking gaps in the current cost / cache / extraction stack motivate this epic:

### 1.1 Cache pricing is computed wrong

`metrics.CalculateCost(usage Usage, inputPricePerMillion, outputPricePerMillion float64)` takes only two prices. Production traffic that passes through Anthropic models with `cache_control` markers reports `cache_creation_tokens` and `cache_read_tokens` correctly in the codec layer, and the values are stamped on `traffic_event`, but the cost calculation multiplies the full `prompt_tokens` (which includes the cached subset) by the standard input rate. The 1.25× write surcharge and the 0.10× read discount are never applied. The `ProviderPricing` table has had `cache_write_usd_per_m` and `cache_read_usd_per_m` columns since E38-S1 but no runtime path reads them. Net effect: prod cost numbers under-state Anthropic cache savings and slightly over-state per-request cost for cached Anthropic traffic.

### 1.2 Reasoning tokens are invisible

OpenAI gpt-5 / o-series and Claude extended thinking and Gemini 2.5 all bill reasoning tokens at the output rate, with the count rolled into the response's output_tokens total. Because the existing `Cost` calculation is `completion_tokens × output_price`, the dollar amount is **correct** — but the reasoning portion is invisible on every UI surface (traffic audit drawer, normalized payload view, AI gateway simulator, analytics rollup, cost-explorer). Customers using high-effort reasoning don't see that 80 % of their per-request output cost is silent thinking; admin can't quickly identify "low-effort reasoning would save us 60 % here". The data is captured at the codec layer (in `Usage.ReasoningTokens`), but the database has no column for it and the UI types don't carry the field.

### 1.3 Three independent protocol parsers drift (broader than just Usage)

The same upstream OpenAI / Anthropic / Gemini response is parsed in **three independent codebases**:

- `packages/ai-gateway/internal/providers/spec_*/codec.go` + `stream.go` — when Nexus is the API endpoint. Does full request-encoding + response-decoding (all of Usage, content blocks, tool calls, reasoning, SSE accumulation).
- `packages/shared/transport/normalize/<format>.go` — the Tier-1 normalizer used by the Hub audit pipeline, ai-gateway L3 cache normalization, and compliance-proxy capture. Also does full parsing → `NormalizedPayload`. **Already partly shared**: compliance-proxy + agent traffic adapters delegate here via the E46-S12 per-host adapter mechanism.
- `packages/shared/traffic/adapters/*/normalize.go` — the legacy `NormalizedContent` extractors. Mostly delegate to `shared/normalize` for Tier-1 hits but carry per-adapter `ExtractRequest`/`ExtractResponse` for the hook content scanning path.

The unification of usage tokens (cached_tokens alias chain, reasoning_tokens, cache_creation_input_tokens) is a special case of the broader problem: **the ai-gateway codec layer doesn't share its parser with shared/normalize**. When OpenAI ships a new `prompt_tokens_details.audio_tokens` field, we must edit at least two parser surfaces. Over 30 `normalize.go` files in `shared/traffic/adapters/*` carry a placeholder comment "fields appear (cache_creation_input_tokens, structured_output, ...)" indicating cache/reasoning extraction was deferred for those surfaces and never completed.

Result observed in prod: the same Anthropic request via the AI Gateway and via the compliance proxy produces different `traffic_event` rows — the gateway path has cache token counts and full Usage, the compliance path frequently has NULLs because its adapter went through the legacy `NormalizedContent` path that strips Usage detail.

The fix is to make `shared/normalize/<format>` the **single parser** for every wire format, and have ai-gateway's `spec_*/codec.DecodeResponse` delegate to it via a thin projection layer (`canonicalbridge.DecodeViaShared`). Codec emission (canonical → wire, all the Rules 1-7 per-model strip logic) stays in `spec_*/`. See `normalization-architecture.md` § "Ai-gateway codec delegation (E58-S0)" for the full delegation contract.

### 1.4 There is no way to estimate cost before sending a request

Customers using Nexus for production traffic want to:

- Show "this request will cost approximately $X" in their application UI before submitting.
- Set per-VK or per-project cost budgets that reject requests above a threshold (`cost guardrails`).
- Compare "should I route this to gpt-5 or claude-sonnet-4-6?" by dollar cost — not just by routing rules.
- Forecast monthly spend from rolling-window send rate.

None of these are buildable today. The cost number only exists *after* the request runs. Building a `/v1/estimate` endpoint as a separate code path would create permanent drift between the estimate and the real path — same shape as the extraction drift in § 1.3, applied to estimation. The right architecture is a request-body flag (`nexus.dry_run`) that branches the existing pipeline before the upstream call.

---

## 2. Functional Requirements

### FR-1: Unified protocol parser layer (E58-S0)

| ID | Requirement | Priority |
|---|---|---|
| FR-1.1 | `packages/shared/transport/normalize/<wire-format>` Tier-1 normalizers (`OpenAIChatNormalizer`, `AnthropicMessagesNormalizer`, `GeminiGenerateNormalizer`) are the **single** source of upstream-response parsing for those wire formats. No third parser implementation exists outside `shared/normalize/` after this story. | Must |
| FR-1.2 | A capability-gap audit is performed first: for each of the four wire formats (OpenAI Chat / OpenAI Responses / Anthropic Messages / Gemini generateContent), compare what `ai-gateway/spec_<format>/codec.go` extracts vs what `shared/normalize/<format>.go` extracts. Document the gap in a table (likely candidates: Anthropic `cache_creation_input_tokens`, Kimi flat `cached_tokens`, DeepSeek `reasoning_content`, OpenAI Responses `output_tokens_details.reasoning_tokens`). | Must |
| FR-1.3 | Gaps from FR-1.2 are filled in `shared/normalize/<format>.go` first — every field the legacy codec extracted is covered by the Tier-1 normalizer before any delegation happens. The Tier-1 normalizer's `NormalizedPayload` plus the projected `providers.Usage` together carry every value the codec used to carry. | Must |
| FR-1.4 | A new bridge package `packages/ai-gateway/internal/execution/canonicalbridge/` (extends the existing canonical-bridge layer) exposes `DecodeViaShared(raw []byte, wireFormat providers.Format, endpoint providers.Endpoint) ([]byte, providers.Usage, error)`. It internally calls the matching Tier-1 normalizer and projects `NormalizedPayload` to the ai-gateway's wire-shape canonical (OpenAI chat-completions JSON form) plus the canonical `Usage`. | Must |
| FR-1.5 | Each of `spec_openai/codec.go`, `spec_anthropic/codec.go`, `spec_gemini/codec.go`, plus `spec_openai/codec_responses_response.go` (Responses API) — their `DecodeResponse` becomes a one-line delegation to `canonicalbridge.DecodeViaShared`. The bespoke parsing code is deleted. | Must |
| FR-1.6 | Each of `spec_openai/stream.go`, `spec_anthropic/stream.go`, `spec_gemini/stream.go`, `spec_openai/stream_responses.go` — their SSE walker / accumulator delegates to `shared/normalize/extract/sse.go` + `shared/normalize/extract/accumulator.go`. The bespoke SSE walking code is deleted. | Must |
| FR-1.7 | Wire-emission code (`EncodeRequest`, `PrepareBody`, per-model parameter-strip helpers, error-envelope synthesis) **stays** in `spec_*/`. It is downstream of routing decisions and encodes per-upstream wire-format requirements (provider-adapter-architecture.md § 3a Rules 1-7); it is not parsing. | Must |
| FR-1.8 | Provider-specific extension fields that ride on the canonical body — Anthropic's `cache_creation_input_tokens`, the Anthropic cache TTL class, Gemini's `thoughts_token_count`, OpenAI Responses' `input_tokens_details.cached_tokens` — are captured by the Tier-1 normalizer and stamped onto the canonical body under `nexus.ext.<provider>.<key>` by the bridge (per provider-adapter Rule 4), preserving today's hub_ingress round-trip behavior. | Must |
| FR-1.9 | A cross-component consistency test in `packages/shared/transport/normalize/` asserts that for every fixture under `testdata/<format>/*.json`, the three call paths produce byte-identical `NormalizedPayload` plus equivalent `providers.Usage`: (a) `shared/normalize.Registry.Normalize(body, meta)` directly, (b) `canonicalbridge.DecodeViaShared(body, format, endpoint)`, (c) `shared/traffic/adapters/<format>.Adapter.Normalize(body, meta)`. CI runs this on every PR. | Must |
| FR-1.10 | Every Tier-1 normalizer that the audit identifies as missing fields gains the necessary alias chains, with the function's godoc citing vendor docs URLs (existing convention in `shared/normalize/`). Fixture coverage includes: vanilla response, cache-hit response (where applicable), reasoning-token response (where applicable), empty-usage response, and any vendor-specific edge case discovered during the audit (Kimi flat `cached_tokens`, DeepSeek-reasoner, Azure OpenAI envelope, Bedrock Anthropic envelope, Vertex Gemini envelope). | Must |
| FR-1.11 | `shared/traffic/adapters/*/normalize.go`'s pre-existing `ExtractRequest` / `ExtractResponse` / `NormalizedContent` legacy surface stays for now (still consumed by some hook paths). A follow-up story (out of E58 scope) may consolidate it onto NormalizedPayload. | Must |
| FR-1.12 | `packages/shared/traffic/detect.go`'s `UsageMeasurement` struct becomes a type alias for the canonical `Usage` shape so the audit pipeline doesn't carry a parallel definition. The canonical `Usage` struct definition lives in one place (today: `packages/ai-gateway/internal/providers/types.go`; may be hoisted to `shared/normalize` as part of S0 if natural). | Must |
| FR-1.13 | The 14 OpenAI-compatible ai-gateway adapters (deepseek, glm, groq, moonshot, xai, mistral, perplexity, fireworks, together, huggingface, replicate, cohere, azure_openai, plus bedrock-anthropic-on-AWS and vertex-gemini-on-GCP wrappers) all delegate through the same `canonicalbridge.DecodeViaShared` path — no per-adapter codec carries its own DecodeResponse after S0. Vendor doc URLs are cited inline in the bridge's per-format projection where alias chains are vendor-specific. | Must |
| FR-1.14 | The canonical `Usage` struct + the alias-chain conventions form part of `shared/`'s API stability contract (CLAUDE.md "shared API stability — additive-only changes once shipped in a released Agent binary"). Removing or renaming a field requires the same forethought as a `shared/audit` schema change. | Must |

### FR-2: Cache pricing data model + cost calculation (E58-S1)

| ID | Requirement | Priority |
|---|---|---|
| FR-2.1 | The `Model` table gains two new Decimal(12,8) columns: `cachedInputReadPricePerMillion` and `cachedInputWritePricePerMillion`. Both are nullable; nil falls back to `inputPricePerMillion` at calculation time. | Must |
| FR-2.2 | The `ModelPricing` and `ProviderPricing` tables are dropped (CLAUDE.md no-backward-compat development-phase rule). Price-change history is captured by `AdminAuditLog`. | Must |
| FR-2.3 | `metrics.CalculateCost` is re-signed to `func(usage providers.Usage, prices ModelPrices) Cost` where the input `providers.Usage` is the canonical Usage populated by `canonicalbridge.DecodeViaShared` (S0), `ModelPrices` holds all four price fields, and `Cost` is a four-component struct (`UncachedInput`, `CacheRead`, `CacheWrite`, `Output`, `Total`). All five existing stamp sites in `proxy.go` + `proxy_cache.go` call the new signature. | Must |
| FR-2.4 | The Provider template JSON schema (`packages/control-plane-ui/dist/provider-templates/*.json`) is bumped to include the two new price fields per model entry. Provider templates for all currently-shipped providers (Anthropic, OpenAI, Azure-OpenAI, Bedrock, Vertex, Gemini, DeepSeek, GLM, Groq, Mistral, MiniMax, Moonshot, Perplexity, X.ai, Fireworks, Together, HuggingFace, Replicate, Cohere) are updated with vendor-documented cache prices. | Must |
| FR-2.5 | The CP-UI Provider Wizard (model add/edit form) exposes the two new fields with helper text explaining the typical ratio (Anthropic 1.25× / 0.1×, OpenAI N/A / 0.5×, Gemini N/A / 0.25×). | Must |
| FR-2.6 | Seed data (`tools/db-migrate/seed/*.ts` and `seed-baseline.sql`) populates the two new columns for every model in the catalog using vendor-documented values; doc references appear as inline comments in the seed source. | Must |
| FR-2.7 | The migration is a single atomic Prisma migration (per CLAUDE.md no-phased-rollout rule). Drops `ModelPricing` and `ProviderPricing`, adds the two `Model` columns, backfills cache prices for existing `Model` rows from the per-provider defaults in the seed data. | Must |
| FR-2.8 | Existing `traffic_event` rows are NOT retroactively re-costed. The Cost dashboard adds a "Pricing updated 2026-MM-DD — older rows reflect the pre-update calculation" note for periods spanning the deploy. | Should |
| FR-2.9 | The Admin Models API (`PATCH /api/admin/models/:id`, `POST /api/admin/models`) accepts and validates the two new fields. IAM permission is the existing `admin:models.write`. | Must |

### FR-3: Reasoning token storage + display (E58-S1)

| ID | Requirement | Priority |
|---|---|---|
| FR-3.1 | The `traffic_event` table gains two new columns: `reasoning_tokens Int?` and `reasoning_cost_usd Decimal(12,8)?`. Both are nullable; populated from `providers.Usage.ReasoningTokens` (extracted by `shared/normalize/<format>` Tier-1 normalizer per S0) and `ReasoningTokens × outputPricePerMillion / 1e6` respectively. | Must |
| FR-3.2 | The "5 stamp sites" sweep (proxy.go + proxy_cache.go) for the new columns is part of E58-S1 — same discipline as cache-token stamping. CLAUDE.md cross-cutting rule § 9.1 applies. | Must |
| FR-3.3 | The backend admin API responses that surface usage (`GET /api/admin/traffic/:id`, `GET /api/admin/cost-summary`, analytics rollup endpoints) include the two new fields. Field naming follows the existing camelCase convention: `reasoningTokens`, `reasoningCostUsd`. | Must |
| FR-3.4 | TypeScript types in `packages/control-plane-ui/src/api/types.ts`, `api/services/analytics.ts`, `api/services/quotaAnalytics.ts`, and `hooks/useTrafficStream.ts` add the two new fields. | Must |
| FR-3.5 | The Traffic Audit Drawer (`trafficAuditDrawer.tsx`) displays Reasoning Tokens alongside Prompt / Completion / Total. When reasoning_tokens is null (provider doesn't report) the row is hidden; when it is 0 the row shows "0 (no reasoning)". | Must |
| FR-3.6 | The Normalized Payload View (`NormalizedPayloadView.tsx`) likewise displays reasoning tokens. | Must |
| FR-3.7 | The AI Gateway Simulator page's usage summary line includes reasoning tokens as a separate value. | Must |
| FR-3.8 | The Cost Dashboard (under Analytics) shows a "Reasoning cost ratio" widget — `Σ reasoning_cost_usd / Σ cost_usd` for the selected timerange, segmented per model. | Should |
| FR-3.9 | CSV / JSON exports of traffic_event include the two new columns. | Must |
| FR-3.10 | i18n keys for the new UI strings exist in all three locales (en / zh / es). | Must |

### FR-4: Estimator core (E58-S2)

| ID | Requirement | Priority |
|---|---|---|
| FR-4.1 | A new internal package `packages/ai-gateway/internal/execution/estimator/` provides a pure-function `Estimate(ctx, EstimateInput) (EstimateResult, error)` entry point. The package has no HTTP handler and no DB read code — callers pass pre-resolved targets and pre-fetched prices. | Must |
| FR-4.2 | The estimator computes a low / expected / high envelope. The expected anchor for input tokens comes from a local tokenizer (tiktoken-go for OpenAI/Azure adapter types; character-ratio heuristic for Anthropic / Gemini / other). | Must |
| FR-4.3 | The expected anchor for output tokens comes from a static per-(model, reasoning_effort) token budget table maintained in `packages/ai-gateway/internal/execution/estimator/output_budget_table.go`. The low and high envelope are derived as `expected/3` and `expected*3`, clamped to `model.maxOutputTokens`. | Must |
| FR-4.4 | Reasoning intent is read from the canonical request body — `reasoning_effort` (OpenAI), `thinking.budget_tokens` (Anthropic), `thinking_config.thinking_budget` (Gemini). There is no parallel `reasoningMode` parameter on `EstimateInput`. | Must |
| FR-4.5 | Cache lookup is read-only against the existing response cache (gateway full-hit probability) and the existing prompt-cache prefix matcher (estimated `cachedInputTokens`). Estimator calls do not insert / update / invalidate cache state. | Must |
| FR-4.6 | When the resolved target uses a smart routing strategy, the estimator's routing dry-run short-circuits to the strategy's fallback chain first entry — it does NOT call the smart LLM. An assumption is added to the result: "smart routing dry-run used fallback chain entry; real request may resolve to a different target if the smart LLM picks one". | Must |
| FR-4.7 | When the resolved model does not support reasoning (not in the output budget table), reasoning effort from the request body is ignored. An assumption is added: `model X does not support reasoning, reasoning_effort=high ignored`. | Must |
| FR-4.8 | Tiktoken-go is added as a dependency to `packages/ai-gateway/go.mod` only — NOT to `packages/shared/go.mod`, per the "shared dependencies vetted set" rule. | Must |
| FR-4.9 | `EstimateResult.Cost` uses the same `metrics.Cost` struct as real cost stamping — same four-component breakdown — to keep estimate and reality on identical schema. | Must |
| FR-4.10 | The estimator's static output budget table cites its source (vendor docs URL, manual calibration script readme) inline as comments. A calibration script (`scripts/estimator-calibration/`) runs known prompts through each model at each effort level and produces an updated table; the script is offline tooling, not a runtime dependency. | Should |

### FR-5: nexus.dry_run flag (E58-S3)

| ID | Requirement | Priority |
|---|---|---|
| FR-5.1 | The canonical request body's `nexus.dry_run` boolean field (under the `nexus.*` extension namespace, per provider-adapter Rule 4) instructs the gateway to estimate the request instead of forwarding it upstream. | Must |
| FR-5.2 | All four ingresses inherit dry-run support without per-ingress wiring: `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `:generateContent`. The canonical extension passes through every ingress's canonicalize step. | Must |
| FR-5.3 | A dry-run request runs the full pipeline through canonicalize → route → cache lookup (read-only). The branch into estimator vs executor happens after cache lookup and before the upstream call. | Must |
| FR-5.4 | The response shape matches the ingress's normal success response shape with `choices: []` (or per-ingress equivalent: `output: []`, `content: []`, `candidates: []`) and `usage` populated from the estimator's expected anchor. The `id` field uses a `estimate-<uuid>` prefix to distinguish from real-request IDs. | Must |
| FR-5.5 | An `x-nexus-estimate` response header carries the full `EstimateResult` as compact JSON (low/expected/high cost breakdown, cache benefit, assumptions, resolved target). | Must |
| FR-5.6 | A dry-run streaming request (`stream: true` + `nexus.dry_run: true`) emits exactly one SSE chunk with the usage block and then `[DONE]`. No token-by-token simulation. | Must |
| FR-5.7 | Dry-run requests are subject to VK authentication. The VK's `allowedModels` constraint applies — a dry-run for a model the VK can't reach returns the same 403 the real request would. | Must |
| FR-5.8 | Dry-run requests are NOT counted against quota (the request never went upstream, no cost was incurred). They ARE counted in a separate `estimate_requests_total{model,resolved_model}` metric for capacity planning. | Must |
| FR-5.9 | Dry-run requests skip request-stage hooks that mutate the upstream call (e.g., compliance redaction) — there is no upstream call to mutate. They DO run content-classification hooks (e.g., PII detection) so the estimate path itself is auditable. | Should |
| FR-5.10 | Per-VK rate limiting applies a separate lighter cap on dry-run requests (default 60/min/VK) so a misbehaving client can't estimate-flood the gateway. The cap is configurable per VK. | Should |

### FR-6: /v1/estimate compare endpoint (E58-S4)

| ID | Requirement | Priority |
|---|---|---|
| FR-6.1 | A new endpoint `POST /v1/estimate` accepts a wrapper body containing one original ingress request plus a `compareTargets` array of `{providerId, modelId, reasoningEffort?}` entries. | Must |
| FR-6.2 | The endpoint dispatches a dry-run per target internally — same pipeline as the dry-run flag — and aggregates the results into a single response. | Must |
| FR-6.3 | The endpoint uses VK authentication. Each target in `compareTargets` is checked against `VK.allowedModels`; targets that violate get a per-target error in the response, not a top-level 403. | Must |
| FR-6.4 | The response includes per-target token breakdown + cost breakdown + cache benefit + assumptions, plus a top-level summary ("cheapest target", "expected savings vs original target"). | Must |
| FR-6.5 | The endpoint's IAM is by VK auth only — no admin endpoint variant. Future cost-explorer features in CP-UI use admin endpoints. | Must |

---

## 3. Non-Functional Requirements

| ID | Requirement |
|---|---|
| NFR-1 | `metrics.CalculateCost` p99 latency < 10 µs on the hot path (it is called once per real request and N times per compare-endpoint request). |
| NFR-2 | `estimator.Estimate` p99 latency < 50 ms for OpenAI/Azure (tiktoken), < 5 ms for Anthropic/Gemini (heuristic), inclusive of routing dry-run and cache lookup. |
| NFR-3 | The provider-usage extraction package's per-extractor function p99 latency < 100 µs on a typical response body. Allocations < 5 per call on the hot path. |
| NFR-4 | Cross-component consistency test (FR-1.5) runs in < 5 s total across all fixtures. |
| NFR-5 | The migration in FR-2.7 runs in < 10 s against the current prod row count (`Model` ≈ low hundreds; `ModelPricing` + `ProviderPricing` < 1000 rows combined). |
| NFR-6 | Unit test coverage per package: `shared/normalize` (extended with S0's gap fills), `ai-gateway/internal/canonicalbridge`, `metrics` (cost), `estimator`, and the cost-stamping paths in `handler/` all meet the ≥95 % per-package coverage rule (CLAUDE.md unit-test-coverage rule). The Provider template JSON schema bump in FR-2.4 is validated by a JSON-schema lint that runs in CI. |
| NFR-7 | The pre-deploy `npm run check:design-tokens` and `npm run check:i18n` lints pass for the UI changes. |
| NFR-8 | No new dependency lands in `packages/shared/go.mod` from this epic (tiktoken-go is scoped to ai-gateway only). |

---

## 4. User Roles & Personas

| Role | Touchpoints |
|---|---|
| **Application developer integrating Nexus** | Sends `nexus.dry_run: true` to estimate before submission. Reads `usage` + `x-nexus-estimate` header. Builds cost-preview UI in their own app. |
| **Platform admin** | Edits Provider / Model pricing (CP-UI Provider Wizard). Reads Cache ROI dashboard, Cost dashboard, Reasoning cost ratio widget. Configures VK-level dry-run rate limit. |
| **Auditor / Finance** | Queries `traffic_event` for per-VK / per-org cost rollups. Cross-checks against monthly provider invoices. Reads the cache-savings columns to attribute negotiated-rate value. |
| **OSS adopter** | Reads `normalization-architecture.md` to add a new vendor adapter. Writes one Tier-1 Normalizer (or extends an existing one) + a fixture set; gets gateway + compliance + agent extraction for free via the unified parser layer. |

---

## 5. Constraints & Assumptions

- **C1.** Tiktoken-go is the only viable in-process tokenizer for OpenAI families. No equivalent exists for Anthropic or Gemini (their tokenizers are not publicly distributed). The character-ratio heuristic is acknowledged to have ±10 % error and is documented as such in `Assumptions[]` for every Anthropic / Gemini estimate.
- **C2.** Vendor count-tokens APIs (Anthropic's `/v1/messages/count_tokens`, etc.) are intentionally not used. The per-call latency (100–500 ms) outweighs the accuracy gain for an endpoint that is meant to be cheap and fast.
- **C3.** Reasoning token estimates are inherently uncertain. The static budget table is calibrated against vendor-published guidance + a small set of manual measurements; production traffic patterns may diverge. The low / high envelope (expected/3 .. expected*3) is intentionally wide to communicate uncertainty.
- **C4.** Smart routing strategies are short-circuited in the dry-run path. A request whose real-path routing would invoke the smart LLM may resolve to a different model than the dry-run estimate — the assumption note surfaces this.
- **C5.** The dry-run path runs content-classification hooks so the estimation surface itself is auditable; it does not run modification hooks (redaction, prompt rewriting) because there is no upstream call to mutate.
- **C6.** Cache lookup in the estimator is read-only — it does not warm or invalidate. A real subsequent request may see a different cache state than the estimate suggested.
- **C7.** Pre-E58 `traffic_event` rows are not retroactively re-costed. Historical analytics distinguish pre- and post-cutover periods via a "Pricing updated YYYY-MM-DD" note.

---

## 6. Glossary

| Term | Meaning |
|---|---|
| **Canonical Usage** | The unified `providers.Usage` struct fields: `PromptTokens`, `CompletionTokens`, `TotalTokens`, `CachedTokens` (read-side), `CacheCreationTokens` (write-side surcharge), `ReasoningTokens`. Populated by `canonicalbridge.DecodeViaShared` from `shared/normalize/<format>` Tier-1 normalizer output (S0). |
| **Cached input read** | Input tokens that the upstream provider served from its own prefix cache. Billed at the cached input read rate. |
| **Cached input write** | Input tokens that the upstream provider wrote into its own cache for future reuse. Billed at the cached input write rate (usually a surcharge). Anthropic only as of 2026-05. |
| **Reasoning tokens** | Output tokens the model produced during internal thinking (gpt-5 reasoning, Claude extended thinking, Gemini 2.5 thinking). Always included in `CompletionTokens`; transparency-only when reported separately. |
| **Reasoning effort** | The intensity of internal thinking requested by the client. OpenAI: `reasoning_effort` ∈ {minimal, low, medium, high}. Anthropic: `thinking.budget_tokens` (integer). Gemini: `thinking_config.thinking_budget`. |
| **Dry run** | A request that runs the full Nexus pipeline through routing + cache lookup but returns an estimate instead of calling the upstream. Triggered by `nexus.dry_run: true`. |
| **Estimator core** | The pure-function package `packages/ai-gateway/internal/execution/estimator/` that turns a canonical request into a cost estimate. |
| **Provider template JSON** | The seed data in `packages/control-plane-ui/dist/provider-templates/*.json` that admins import to bootstrap a new Provider with its model catalog and pricing. |
| **Cross-component consistency** | The invariant that the AI Gateway (via `canonicalbridge.DecodeViaShared`), the compliance proxy (via `shared/traffic.Adapter.Normalize`), and the Hub audit consumer (via `normalize.Registry.Normalize`) produce byte-identical canonical `Usage` for the same upstream response. Enforced by a test in `packages/shared/transport/normalize/`. |

---

## 7. MoSCoW Priority

| Story | Priority | Rationale |
|---|---|---|
| S0 — **Unified protocol parser** (ai-gateway/spec_* delegate to shared/normalize) | **Must** | All other stories depend on it. Eliminates the third copy of vendor wire parsers; fixes the cross-component Usage drift bug as a natural side effect; scope is parser unification end-to-end, not just Usage tokens. |
| S1 — Cache pricing + reasoning storage/display | **Must** | Fixes the P0 calculation bug; unlocks meaningful cache ROI reporting. Inherits correct Usage from S0. |
| S2 — Estimator core | **Must** | Foundation for S3 and S4. Consumes `NormalizedPayload` (or projected canonical) produced by S0. |
| S3 — nexus.dry_run flag | **Must** | The primary customer-facing feature. |
| S4 — /v1/estimate compare endpoint | Should | Sugar over S3; high value for model-selection UX but not a v1 blocker. |

---

## 8. Out of Scope (for this epic; tracked in `cost-estimation-architecture.md` § 8)

- Time-effective pricing history (`ModelPriceHistory` table for billing reconstruction).
- Multimodal pricing (audio/image/video input rates).
- Anthropic 5-minute vs 1-hour cache window pricing split.
- Cost guardrails (VK-level `maxCostPerRequestUsd` enforcement) — designed-for in dry-run pipeline branch but not implemented in this epic.
- Smart-routing cost-aware tiebreaker — same.
- Budget forecast widget (monthly spend projection) — same.

---

## 9. Acceptance Criteria

| ID | Acceptance |
|---|---|
| AC-1 | A real Anthropic request via the AI Gateway and the same request via the compliance proxy (using `tests/scripts/test-compliance-proxy`) produce `traffic_event` rows with **identical** `cache_creation_tokens`, `cache_read_tokens`, `reasoning_tokens` values. |
| AC-2 | After applying the migration in FR-2.7, `metrics.CalculateCost` produces a Cost where the sum of components equals `cost_usd` stamped on the corresponding `traffic_event` row, to 8 decimal places. |
| AC-3 | A `traffic_event` row for an Anthropic request with `cache_control` markers shows: `cost_usd` reflects the 1.25× write surcharge on first turn; subsequent turns show the 0.10× read discount. The values match Anthropic's own `usage` block × the seeded prices. |
| AC-4 | The Traffic Audit Drawer shows Reasoning Tokens for a gpt-5 request with `reasoning_effort: high`. The Cost Dashboard's Reasoning cost ratio widget shows a non-zero value for the corresponding period. |
| AC-5 | A `POST /v1/chat/completions` with body `{..., "nexus": {"dry_run": true}}` returns 200 with `choices: []`, `usage` populated, and an `x-nexus-estimate` header containing the JSON breakdown. The same payload sent to `/v1/messages` (Anthropic ingress) returns an Anthropic-shape response with `content: []`. |
| AC-6 | A `POST /v1/estimate` with `compareTargets: [{model: "gpt-5"}, {model: "claude-sonnet-4-6"}]` returns per-target estimates plus a "cheapest" summary. Targets the VK can't reach return per-target errors, not a top-level 403. |
| AC-7 | The cross-component consistency test (FR-1.5) is green on every fixture. |
| AC-8 | All per-package coverage gates from CLAUDE.md hold (≥95 %), except for those listed in the existing `scripts/.coverage-allowlist`. |
| AC-9 | The Provider template JSON schema bump (FR-2.4) is validated by `npm run check:provider-templates` in CI; the lint rejects templates missing the two new fields. |
| AC-10 | The smoke test `tests/scripts/smoke-gateway.py` is updated to include a dry-run arm per ingress (OpenAI chat, OpenAI responses, Anthropic, Gemini) and is green. |
