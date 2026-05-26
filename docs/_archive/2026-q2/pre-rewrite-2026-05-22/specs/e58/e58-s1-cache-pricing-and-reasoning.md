# E58-S1 — Cache Pricing Fix + Reasoning Token Storage & Display

> Story: e58-s1
> Epic: 58 (Cost Estimation, Cache Pricing & Unified Usage Extraction)
> Status: Draft
> Requirements: `docs/developers/specs/e58/e58-cost-estimation-and-cache-pricing.md` § FR-2 + § FR-3
> Architecture: `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` § 2 (pricing model) + § 3 (cost function) + § 3.4 (reasoning split)
> Blocks: E58-S2 (estimator depends on the new `metrics.CalculateCost` signature)
> Blocked by: E58-S0 (the canonical `Usage` struct populated by the unified `shared/normalize` parser layer must be in place first; S1 stamps cost from the Usage that S0 produces)

## User Story

As a Platform Admin I want the gateway's per-request cost calculation to use the **correct** four-component pricing (uncached input, cached read, cached write, output) so that cache savings on the Cache ROI dashboard reflect reality; and as a customer using gpt-5 / Claude extended thinking / Gemini 2.5 reasoning I want to see how many of my output tokens were thinking tokens so I can decide whether `reasoning_effort: low` would save me money without changing answer quality.

## Tasks

### T1 — Schema: Model table additions

- T1.1 In `tools/db-migrate/schema.prisma`, on the `Model` model, add two new optional Decimal(12,8) fields:
    ```prisma
    cachedInputReadPricePerMillion   Decimal? @db.Decimal(12, 8)
    cachedInputWritePricePerMillion  Decimal? @db.Decimal(12, 8)
    ```
- T1.2 Drop the `ModelPricing` model from the schema entirely (CLAUDE.md no-backward-compat dev-phase rule).
- T1.3 Drop the `ProviderPricing` model from the schema entirely. The `provider_pricing` table's cache fields have never been read by runtime code; the data is migrated into `Model` rows in T1.5.
- T1.4 Run `npx prisma migrate dev --name pricing_unification_cache_fields` to generate the migration SQL. Hand-edit the generated SQL to: (a) order DDL safely (add columns first, then drop tables, to allow the backfill in T1.5 to read both old and new), (b) include the SET statements for the backfill, (c) include a transaction wrapper.
- T1.5 Backfill data. For each existing `Model` row:
    - If a matching `ProviderPricing` row exists (by `(adapter_type, model_pattern)` match against `Model.providerId.adapterType` + `Model.providerModelId`), copy its `cache_write_usd_per_m` to `cachedInputWritePricePerMillion` and `cache_read_usd_per_m` to `cachedInputReadPricePerMillion`.
    - Otherwise apply per-adapter defaults from the seed data (Anthropic: write=1.25×input, read=0.10×input; OpenAI: write=NULL, read=0.50×input; Gemini: write=NULL, read=0.25×input; others: write=NULL, read=NULL).
- T1.6 Generate updated Go types via `npm run db:generate` (this regenerates `tools/db-migrate/generated/*.go` consumed by all services).

### T2 — `metrics.CalculateCost` rewrite

- T2.1 In `packages/ai-gateway/internal/observability/metrics/cost.go` (create if doesn't exist; otherwise edit existing in `metrics.go`):
    ```go
    import "github.com/.../packages/ai-gateway/internal/providers"
    // providers.Usage is the canonical Usage struct produced by
    // canonicalbridge.DecodeViaShared (E58-S0). All five stamp
    // sites pass this exact struct to CalculateCost.

    type ModelPrices struct {
        InputUsdPerM            *float64
        OutputUsdPerM           *float64
        CachedInputReadUsdPerM  *float64
        CachedInputWriteUsdPerM *float64
    }

    type Cost struct {
        UncachedInput float64
        CacheRead     float64
        CacheWrite    float64
        Output        float64
        Total         float64
    }

    func CalculateCost(u providers.Usage, p ModelPrices) Cost { ... }
    ```
- T2.2 Implementation details:
    - `p.CachedInputReadUsdPerM` falls back to `p.InputUsdPerM` if nil; same for `CachedInputWriteUsdPerM`.
    - `UncachedInput` count = `PromptTokens − CachedTokens − CacheCreationTokens`, clamped to ≥ 0 (defensive against integer underflow if a provider reports inconsistent counts).
    - `Output` uses `CompletionTokens` directly (which already includes reasoning tokens per the canonical Usage contract).
    - Each component is `NaN` if its price is nil and no fallback applies; the `Total` is `NaN` if any non-zero component is `NaN`.
- T2.3 Delete the old two-argument `CalculateCost(usage Usage, inputPricePerMillion, outputPricePerMillion float64) float64`. All five stamp sites move to the new signature.
- T2.4 Unit tests for every fallback combination + a "fully populated prices, fully populated usage" golden table-driven test using known Anthropic + OpenAI + Gemini token counts. Coverage ≥95 %.

### T3 — Five stamp-site sweep in handler/

- T3.1 `proxy.go` — `handleNonStream`: after upstream returns, call `metrics.CalculateCost(usage, prices)` with `prices` resolved via `cachelayer.GetModelPrices(modelID)`. Stamp `cost_usd = cost.Total`, plus the breakdown fields (T4).
- T3.2 `proxy.go` — `handleStream`: same after the SSE stream's final usage chunk arrives.
- T3.3 `proxy_cache.go` — `handleStreamHit`: cost = 0 (cache served entirely); compute the "would-have-been" cost using `CalculateCost(originalUsage, prices)` and stamp it as `cache_read_savings_usd`.
- T3.4 `proxy_cache.go` — `handleNonStreamHit`: same as T3.3.
- T3.5 `proxy_cache.go` — `handleStreamWithSubscription`: subscribers don't independently stamp; ensure the leader stamp is replayed correctly.
- T3.6 Add a unit test per stamp site asserting the correct `Cost` value is computed and persisted (use a fake `traffic_event` writer to inspect the stamped row). Coverage ≥95 %.
- T3.7 Add a regression test for the historical "missed 4 cache sites" bug pattern (CLAUDE.md cross-cutting rule § 9.1): assert that all four `cache_*` stamp sites in `proxy_cache.go` call `CalculateCost`, not zero-init.

### T4 — Schema: traffic_event reasoning columns

- T4.1 In `schema.prisma`, on `traffic_event`, add two new optional columns:
    ```prisma
    reasoning_tokens    Int?      @map("reasoning_tokens")
    reasoning_cost_usd  Decimal?  @map("reasoning_cost_usd") @db.Decimal(12, 8)
    ```
- T4.2 Add the columns in the same atomic migration as T1.4 (one migration for all schema changes in S1).
- T4.3 Wire the columns through the Hub `insertTrafficEventSQL` (`packages/nexus-hub/internal/observability/audit/...`) and the MQ TrafficEventMessage schema (`packages/shared/transport/mq/...` and the consumer side).
- T4.4 In every stamp site from T3, compute `reasoning_cost_usd = ReasoningTokens × OutputUsdPerM / 1e6` if both are non-nil; stamp alongside `cost_usd`.

### T5 — Provider template JSON schema bump

- T5.1 Update the JSON schema in `packages/control-plane-ui/dist/provider-templates/` (or wherever the schema lives — likely defined inline in `useProviderWizard.ts` / `helpers.ts`) to require two new fields per model entry:
    ```json
    "cachedInputReadPricePerMillion": 0.30,
    "cachedInputWritePricePerMillion": 3.75
    ```
- T5.2 Update each of the 12 currently-shipped provider templates with vendor-documented values:
    | Provider template | inputPrice | cachedInputReadPrice | cachedInputWritePrice |
    |---|---|---|---|
    | `anthropic.json` (claude-sonnet-4-6) | 3.00 | 0.30 (= 0.1×) | 3.75 (= 1.25×) |
    | `anthropic.json` (claude-opus-4-7) | 5.00 | 0.50 | 6.25 |
    | `anthropic.json` (claude-haiku-4-5) | 1.00 | 0.10 | 1.25 |
    | `openai.json` (gpt-5) | (per-vendor) | 0.5× of input | null (no surcharge) |
    | `openai.json` (o3) | (per-vendor) | 0.5× of input | null |
    | `azure-openai.json` | (per-vendor) | 0.5× of input | null |
    | `gemini.json` (gemini-2.5-pro) | (per-vendor) | 0.25× of input | null |
    | `bedrock.json` (claude-* models) | (per-vendor incl. AWS markup) | per-Anthropic ratio | per-Anthropic ratio |
    | `vertex.json` (gemini-* models) | (per-vendor incl. region) | per-Gemini ratio | null |
    | `deepseek.json` | (per-vendor) | OpenAI-compat: 0.5× | null |
    | `glm.json`, `groq.json`, `moonshot.json`, `xai.json`, `mistral.json`, `perplexity.json`, `fireworks.json`, `together.json`, `huggingface.json`, `replicate.json`, `cohere.json`, `minimax.json` | (per-vendor) | null (most have no published cache discount) | null |
- T5.3 Each price value in the JSON gets an inline JSON5-style comment in a sibling `.md` file (or a JSON `_doc` key by convention) citing the vendor pricing page URL + date.
- T5.4 A new CI lint `npm run check:provider-templates` validates: (a) all 12 templates parse, (b) each model entry has the two new fields (nullable, but the keys must be present), (c) the cited URLs are non-empty strings.

### T6 — CP-UI Provider Wizard update

- T6.1 In `packages/control-plane-ui/src/pages/ai-gateway/providers/wizard/`, add two new input controls to the model add/edit form:
    - "Cached input read price (USD per million)" — number input, placeholder "auto (= input price)", helper text "Anthropic: ~0.1× input · OpenAI: 0.5× input · Gemini: 0.25× input · others: usually empty"
    - "Cached input write price (USD per million)" — number input, placeholder "auto (= input price)", helper text "Anthropic ephemeral cache: ~1.25× input · most providers: empty"
- T6.2 Both inputs are optional; empty submits as null.
- T6.3 Both fields persist via `PATCH /api/admin/models/:id` (T7).
- T6.4 The model detail page (`pages/ai-gateway/providers/detail/`) displays the four prices in a read-mode table.
- T6.5 i18n keys added in en / zh / es with technical-term policy (model, provider, cached stay English; prices etc. translate).
- T6.6 Vitest unit tests for the new form inputs (validation, submit shape, conditional helper text). Coverage targets per CLAUDE.md.

### T7 — Admin Models API update

- T7.1 In `packages/control-plane/internal/handler/admin_models.go`, the request body schema for `POST /api/admin/models` and `PATCH /api/admin/models/:id` adds the two new fields (both `*float64`, optional).
- T7.2 The handler validates values are ≥ 0 (or null).
- T7.3 The `safe_update.go` whitelist of writable columns is extended with the two new column names.
- T7.4 The `admin_providers.go` create-or-update payload likewise accepts the two new fields when creating Models inline.
- T7.5 IAM action: the existing `admin:models.write` covers the new fields. No new IAM resource needed.
- T7.6 Unit tests for the handler. Coverage ≥95 %.

### T8 — Cachelayer wiring

- T8.1 `packages/ai-gateway/internal/cache/layer/loaders.go`: the SELECT that loads Model rows adds the two new columns.
- T8.2 `packages/ai-gateway/internal/store/model.go`: the `Model` struct gains the two new fields; SELECT queries are updated.
- T8.3 A new helper `cachelayer.GetModelPrices(modelID) metrics.ModelPrices` returns the four-field price struct ready for `CalculateCost`. This is the single place callers fetch prices from.
- T8.4 Unit tests verifying the cachelayer returns correct ModelPrices after price update + refresh cycle. Coverage ≥95 %.

### T9 — Backend API: surface reasoning + cache breakdown

- T9.1 `packages/control-plane/internal/handler/admin_traffic.go` (or wherever the traffic detail endpoint lives) — the response shape for `GET /api/admin/traffic/:id` includes:
    ```json
    {
      ...,
      "usage": {
        "promptTokens": 1234,
        "completionTokens": 567,
        "totalTokens": 1801,
        "cachedTokens": 800,
        "cacheCreationTokens": 0,
        "reasoningTokens": 300       // NEW
      },
      "cost": {
        "totalUsd": 0.01234,
        "uncachedInputUsd": 0.00130,    // NEW (was implicit)
        "cacheReadUsd": 0.00024,        // NEW
        "cacheWriteUsd": 0.0,           // NEW
        "outputUsd": 0.01080,           // NEW
        "reasoningCostUsd": 0.00450     // NEW
      },
      "cacheSavings": {                  // NEW grouping
        "gatewaySavingsUsd": 0.0,       // = cache_read_savings_usd when this was a gateway-cache hit
        "providerPromptCacheNetSavingsUsd": 0.00216  // = cache_read_savings_usd − cache_write_cost_usd
      }
    }
    ```
- T9.2 `packages/control-plane/internal/handler/admin_cost_summary.go`: rollup endpoint adds `totalReasoningCostUsd` to the response.
- T9.3 `packages/control-plane/internal/handler/admin_analytics_rollup.go`: time-bucketed rollups include reasoning.
- T9.4 SQL queries are updated to SELECT the new columns; cost component breakdown for the dashboard is computed in Go from the persisted column values (not recomputed from tokens).
- T9.5 Unit tests for each endpoint. Coverage ≥95 %.

### T10 — CP-UI TypeScript types

- T10.1 `packages/control-plane-ui/src/api/types.ts`: add `reasoningTokens`, `reasoningCostUsd`, plus the new cost breakdown fields, to `TrafficEntry` / `TrafficDetail` / `UsageSummary` / etc. interfaces.
- T10.2 `packages/control-plane-ui/src/api/services/analytics.ts` and `quotaAnalytics.ts`: add the fields to the response type signatures.
- T10.3 `packages/control-plane-ui/src/hooks/useTrafficStream.ts`: same.
- T10.4 No backwards-compat aliasing — the cleanup is wholesale.
- T10.5 Vitest tests for any service that derives values from the new fields.

### T11 — CP-UI: Traffic Audit Drawer reasoning display

- T11.1 In `packages/control-plane-ui/src/pages/traffic/trafficAuditDrawer.tsx`, in the AI Provider usage panel:
    - Add a "Reasoning Tokens" row, shown only when `reasoningTokens != null`.
    - Format: `300 reasoning tokens (≈ 53% of completion)` — the percentage is computed from `reasoningTokens / completionTokens`.
- T11.2 Add a "Reasoning cost" row in the cost panel, shown only when `reasoningCostUsd != null`.
- T11.3 i18n keys added in en/zh/es:
    - `pages:traffic.detail.aiProvider.reasoningTokens`: "Reasoning tokens"
    - `pages:traffic.detail.aiProvider.reasoningTokensTooltip`: "Output tokens used by the model's internal thinking. Already included in completion tokens; shown here for transparency."
    - `pages:traffic.detail.aiProvider.reasoningCost`: "Reasoning cost"
- T11.4 Vitest test asserting the rows render only when data is present.

### T12 — CP-UI: Normalized Payload View reasoning display

- T12.1 In `packages/control-plane-ui/src/pages/traffic/NormalizedPayloadView.tsx`, in the usage block, append the same Reasoning Tokens display rules.
- T12.2 Same i18n keys.
- T12.3 Vitest test.

### T13 — CP-UI: AI Gateway Simulator usage summary

- T13.1 In `packages/control-plane-ui/src/pages/tools/ai-gateway-simulator/AIGatewaySimulatorPage.tsx`, in the usage-summary string (currently `prompt / completion / total (n req, $cost)`), add `(reasoning: X)` segment when applicable.
- T13.2 i18n key added.

### T14 — CP-UI: Cost Dashboard reasoning ratio widget (Should-have)

- T14.1 In `packages/control-plane-ui/src/pages/analytics/` add a new widget — "Reasoning cost ratio" — showing `Σ reasoning_cost_usd / Σ cost_usd` for the selected timerange, with per-model segmentation.
- T14.2 New SQL aggregation in the cost-summary endpoint (T9.2).
- T14.3 i18n keys for the widget title, description, empty-state message.

### T15 — CSV export updates

- T15.1 Any traffic export script / endpoint (admin traffic export, audit export) adds the two new columns. Audit `packages/control-plane/internal/handler/admin_export*.go` for affected endpoints.
- T15.2 Column headers in CSV: `reasoningTokens`, `reasoningCostUsd`.
- T15.3 Unit tests.

### T16 — Seed data update

- T16.1 `tools/db-migrate/seed/*.ts` updates `Model` row creation to populate the two new cache price fields per the table in T5.2.
- T16.2 `tools/db-migrate/seed/data/seed-baseline.sql` is regenerated from a fresh seed run; the existing seed-then-export workflow handles this. If the workflow doesn't auto-include the new fields, update the export script.
- T16.3 The 2026-05-13 collapsed prod-baseline approach means the seed file IS the canonical prod state — getting this right is the prod migration.

### T17 — Documentation

- T17.1 `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` is already written as part of this epic (the architecture doc landed before the SDD). Verify it accurately reflects what S1 implements.
- T17.2 `docs/users/api/openapi/admin/e58-s1-cache-pricing-admin.yaml` documents the new Admin Models API fields, the new traffic detail response shape, and the cost summary additions.
- T17.3 The `add-provider-adapter` skill is updated to reference the new pricing fields when scaffolding a new provider.

## Acceptance Criteria

| ID | Acceptance |
|---|---|
| AC-1 | Schema migration runs in < 10 s against current prod data; `Model` table has the two new columns; `ModelPricing` and `ProviderPricing` tables are dropped. `npm run check:db-schema` passes. |
| AC-2 | `metrics.CalculateCost` returns a `Cost` struct whose components sum to `Total` (to 8 decimal places) for every test row. |
| AC-3 | All five stamp sites in `proxy.go` + `proxy_cache.go` call the new `CalculateCost(usage, prices)` signature. `grep -n "CalculateCost(" packages/ai-gateway/` returns exactly the expected sites with the new signature; no callers of the old two-price signature remain. |
| AC-4 | A real Anthropic request with `cache_control` markers produces a `traffic_event` row whose `cost_usd` matches `(uncachedInputTokens × inputPrice + cacheReadTokens × cachedInputReadPrice + cacheCreationTokens × cachedInputWritePrice + completionTokens × outputPrice) / 1e6`. |
| AC-5 | A `traffic_event` row from a gpt-5 request with `reasoning_effort: high` has `reasoning_tokens > 0` and `reasoning_cost_usd > 0`. The Traffic Audit Drawer renders both with the correct values. |
| AC-6 | `GET /api/admin/traffic/:id` returns the new `reasoningTokens` / `reasoningCostUsd` fields and the new cost breakdown grouping. |
| AC-7 | All 12 provider template JSON files have the two new fields. `npm run check:provider-templates` is green. |
| AC-8 | CP-UI Provider Wizard accepts and submits the two new fields. Model detail page renders all four prices. |
| AC-9 | Unit test coverage ≥95 % for `metrics`, `cachelayer`, `handler/admin_models`, `handler/admin_traffic`, `handler/admin_cost_summary` packages. |
| AC-10 | The `smoke-gateway` skill is updated to assert `cost_usd` reflects cache pricing correctly for an Anthropic + cache_control test arm. |
| AC-11 | i18n lint passes for the new keys in en / zh / es. |
| AC-12 | Pre-deploy `npm run check:design-tokens` passes for the UI changes. |

## Data Model

### Migration SQL (sketch)

```sql
BEGIN;

-- 1. Add new columns on Model
ALTER TABLE "Model"
  ADD COLUMN "cachedInputReadPricePerMillion"  DECIMAL(12, 8),
  ADD COLUMN "cachedInputWritePricePerMillion" DECIMAL(12, 8);

-- 2. Add new columns on traffic_event
ALTER TABLE "traffic_event"
  ADD COLUMN "reasoning_tokens"   INTEGER,
  ADD COLUMN "reasoning_cost_usd" DECIMAL(12, 8);

-- 3. Backfill cache prices from ProviderPricing where matches exist
UPDATE "Model" m
SET
  "cachedInputReadPricePerMillion"  = pp."cache_read_usd_per_m",
  "cachedInputWritePricePerMillion" = pp."cache_write_usd_per_m"
FROM "provider_pricing" pp, "Provider" p
WHERE m."providerId" = p.id
  AND pp."adapter_type" = p."adapterType"
  AND m."providerModelId" ~ pp."model_pattern";

-- 4. Apply per-adapter defaults where no ProviderPricing matched
-- Anthropic
UPDATE "Model" m
SET
  "cachedInputReadPricePerMillion"  = COALESCE(m."cachedInputReadPricePerMillion",  m."inputPricePerMillion" * 0.10),
  "cachedInputWritePricePerMillion" = COALESCE(m."cachedInputWritePricePerMillion", m."inputPricePerMillion" * 1.25)
FROM "Provider" p
WHERE m."providerId" = p.id AND p."adapterType" = 'anthropic';

-- OpenAI / Azure
UPDATE "Model" m
SET "cachedInputReadPricePerMillion" = COALESCE(m."cachedInputReadPricePerMillion", m."inputPricePerMillion" * 0.50)
FROM "Provider" p
WHERE m."providerId" = p.id AND p."adapterType" IN ('openai', 'azure_openai');

-- Gemini / Vertex
UPDATE "Model" m
SET "cachedInputReadPricePerMillion" = COALESCE(m."cachedInputReadPricePerMillion", m."inputPricePerMillion" * 0.25)
FROM "Provider" p
WHERE m."providerId" = p.id AND p."adapterType" IN ('gemini', 'vertex');

-- (others: leave null → fallback to input price at cost-calc time)

-- 5. Drop legacy tables
DROP TABLE "provider_pricing";
DROP TABLE "ModelPricing";

COMMIT;
```

### Go types

```go
// packages/ai-gateway/internal/observability/metrics/cost.go

type ModelPrices struct {
    InputUsdPerM            *float64
    OutputUsdPerM           *float64
    CachedInputReadUsdPerM  *float64
    CachedInputWriteUsdPerM *float64
}

type Cost struct {
    UncachedInput float64
    CacheRead     float64
    CacheWrite    float64
    Output        float64
    Total         float64
}

func CalculateCost(u providers.Usage, p ModelPrices) Cost {
    inP := f64(p.InputUsdPerM)
    cReadP := f64Or(p.CachedInputReadUsdPerM, inP)
    cWriteP := f64Or(p.CachedInputWriteUsdPerM, inP)
    outP := f64(p.OutputUsdPerM)

    cachedRead := int64(deref(u.CachedTokens))
    cacheWrite := int64(deref(u.CacheCreationTokens))
    promptTotal := int64(deref(u.PromptTokens))
    uncached := promptTotal - cachedRead - cacheWrite
    if uncached < 0 { uncached = 0 }
    completion := int64(deref(u.CompletionTokens))

    cost := Cost{
        UncachedInput: float64(uncached) * inP / 1e6,
        CacheRead:     float64(cachedRead) * cReadP / 1e6,
        CacheWrite:    float64(cacheWrite) * cWriteP / 1e6,
        Output:        float64(completion) * outP / 1e6,
    }
    cost.Total = cost.UncachedInput + cost.CacheRead + cost.CacheWrite + cost.Output
    return cost
}
```

## Testing strategy

- **Unit (white-box)**: Per-component tests for `CalculateCost` covering nil-price fallback, zero-token paths, large-number arithmetic, and the underflow defense in `uncached < 0`.
- **Unit (integration with cachelayer)**: Fake cachelayer returning known prices; assert the stamp sites stamp the expected cost.
- **Integration (golden DB writes)**: A handler-level test sends a fake upstream response with known tokens; assert the `traffic_event` row has the expected `cost_usd`, `cache_read_savings_usd`, `reasoning_tokens`, `reasoning_cost_usd`.
- **End-to-end smoke**: `smoke-gateway --routing` test arm that hits an Anthropic model with cache markers and asserts the four-component cost matches manual calculation.
- **UI snapshot**: Vitest snapshot of the Traffic Audit Drawer rendering a fixture traffic event with reasoning data.

## Rollback plan

This migration is **destructive** (drops `ModelPricing` + `ProviderPricing`). A clean rollback requires `git revert` of the migration + restoring the dropped tables from backup. Pre-deploy steps:

- Take a backup: `pg_dump --table=ModelPricing --table=provider_pricing nexus_gateway > /tmp/pricing-tables-pre-e58s1.sql`.
- Tag the commit `prod-pre-e58s1-baseline` on prod before applying.

If post-deploy a critical issue emerges:

- Revert the application code (so callers move back to the old two-price `CalculateCost`).
- `git revert` the migration commit, then run a new migration that re-adds the dropped tables and copies back the data from the backup.
- This is more involved than the typical CLAUDE.md "rollback = git revert" — call out to user during prod deploy that this story is irreversible without manual DB restore.

The cost calculation difference for in-flight traffic is small (Anthropic cached traffic, currently mis-billed) and the dollar-level impact is on the order of $X/day (estimate to be added after a 24h prod measurement). No customer billing is affected.

## Open questions for review

1. The Anthropic 1.25× write surcharge assumes the 5-minute cache window. The 1-hour window is 2× input. Should we add a third price field `cachedInputWrite1hPricePerMillion` now, or defer until pricing actually diverges (currently both windows are billed identically post-2026-05 — but Anthropic has signaled they may diverge)? Current draft: defer; document the assumption in T5.3.
2. Should the migration also backfill historical `traffic_event.reasoning_tokens` from the existing `usage` JSONB column where the data is present? Current draft: no — leave historical NULL; the "since YYYY-MM-DD" note on the Cost Dashboard surfaces the change. Backfilling is an extra script with its own correctness risk and the value is low.
3. The `nil`-fallback semantics in `CalculateCost` (cache prices fall back to input price) is silent. Should we emit a one-shot `slog.Warn` per (model, missing-field) when a stamp site discovers a nil price that would have benefited from explicit configuration? Current draft: yes — wire a tiny `metrics.WarnOnMissingPrice` call gated on missing OutputUsdPerM only (since that's the bug case; missing cache prices are legitimately "no discount available").
