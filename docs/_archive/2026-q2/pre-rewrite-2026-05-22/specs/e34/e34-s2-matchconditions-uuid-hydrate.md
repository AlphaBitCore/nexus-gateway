# E34 Story 2 — `matchConditions` UUID storage + runtime code-to-UUID hydrate

**Epic:** 34 — Routing engine ID hygiene
**Story:** 2
**Status:** Draft — 2026-04-29
**Requirements:** N/A (architectural cleanup; no new functional requirement)
**OpenAPI:** `docs/users/api/openapi/admin/e34-s2-routing-rule-matchconditions.yaml`

## User Story

> **As a** platform admin editing routing rules in the UI,
> **I want** Match Conditions → Models to store and display the same model rows the rest of the form already uses (the `Model.id` UUIDs in primary/fallback targets and VK `allowedModels`),
> **so that** the value I save is the same value that appears in the dropdown the next time I open the rule, and the routing engine actually evaluates the rule against incoming requests.

## Context

- Today the dropdown's `<option value>` is `Model.id` (UUID), but the routing engine compares `matchConditions.models` strings against the **request's `model` field**, which is the customer-facing `Model.code` string (`ai-gateway/internal/router/matcher.go:314`). The two ends speak different alphabets, so a saved selection never matches at runtime, and DB rows accumulate a mix of UUIDs (from UI saves) and codes (from seed).
- `RoutingRule.config`, `VirtualKey.allowedModels`, and `RoutingRule.fallbackChain` already use `Model.id` UUIDs. Only `matchConditions.models` is divergent — fixing it brings every persisted reference under one identifier scheme.
- `Resolver.hydrateRequestedModel` (`router/resolver.go:168`) calls `r.db.GetModel(ctx, rctx.RequestedModel.ID)` against the request string, but `GetModel` looks up by UUID PK. The hydration is silently a no-op for every real request, leaving `RequestedModel.ProviderID` / `Type` empty, which in turn makes `matchConditions.providers` and `matchConditions.modelTypes` permanently dead. This story fixes both bugs at the same time.
- One special routing rule (`smart-auto-routing`) currently encodes the literal keyword `"auto"` inside `matchConditions.models`. `"auto"` is not a `Model` row — it is a request-side sentinel meaning "let the smart router pick". Storing it next to UUIDs would re-introduce a mixed-alphabet column.

## Decision (locked, see brainstorm in chat 2026-04-29)

1. **`matchConditions.models` becomes a `Model.id` (UUID) array.** UI, seed, DB rows, and the engine all agree.
2. **A new sibling field `matchConditions.requestedModelLiterals: string[]`** carries non-`Model` request keywords (today only `"auto"`). It is matched against the raw request string and is independent of UUID hydration.
3. **`Resolver.hydrateRequestedModel` is rewritten** to resolve the request string to a candidate set of `Model.id` values via `Model.code` (exact) plus `Model.aliases` (member). The set is stored on `RoutingContext.RequestedModel.CandidateIDs`. `Provider`/`Type` are filled from the first candidate so `matchConditions.providers` / `modelTypes` start working.
4. **`matcher.RuleMatchesContext`** changes the `Models` check from `stringSliceContains(conds.Models, modelID)` to `intersect(conds.Models, ctx.RequestedModel.CandidateIDs)`; adds an `AND` literals check `stringSliceContains(conds.RequestedModelLiterals, modelID)`.
5. **VK allowedModels stays UUID-keyed** and continues to be enforced by the existing `ModelMatchesAllowedRefs`. (Cross-cutting "VK narrows candidate set then routing rule narrows further" is **explicitly out of scope** here — tracked as a follow-up so this story stays small enough to land in one PR.)

## Out of scope

- VK-driven candidate narrowing (multiple `Model.id`s for the same `code` resolved against `vk.allowedModels` to pick the operator-permitted variant). Tracked as `e34-sX` follow-up.
- Renaming `RoutingContext.RequestedModel.ID` (currently misnamed — holds a string, not a UUID). Touches 16 call-sites; deferred to a pure-rename change.

## Tasks

### T1. Schema — `MatchConditions` Go struct

Files:

- `packages/ai-gateway/internal/router/types.go`

Changes:

1. Add `RequestedModelLiterals []string \`json:"requestedModelLiterals,omitempty"\`` to `MatchConditions`. Comment that it carries non-`Model.code` request keywords (e.g. `"auto"`) and is OR'd nowhere — it AND's with `Models` like every other dimension.
2. On `RequestedModel`, add `CandidateIDs []string` (no JSON tag — runtime-only). Comment that `Resolver.hydrateRequestedModel` populates it from the request `model` field by looking up `Model.code` + `Model.aliases`.

### T2. Store — code+alias resolver

Files:

- `packages/ai-gateway/internal/store/model.go`

Changes:

1. Add `ResolveModelCandidates(ctx context.Context, code string) ([]Model, error)`. SQL:
   ```sql
   SELECT id, code, name, "providerId", "providerModelId", type, enabled,
          "inputPricePerMillion", "outputPricePerMillion",
          COALESCE(features, '{}'), "maxContextTokens", "maxOutputTokens"
   FROM "Model"
   WHERE enabled = true
     AND (code = $1 OR $1 = ANY(aliases))
   ```
   Returns zero, one, or many rows. Empty slice + `nil` error when nothing matches (the routing engine treats "no candidates" as "request model unknown — `matchConditions.models` cannot match"). Reuses the existing `Model` scan helpers; do not duplicate `ParseDecimal` etc.

### T3. Cache layer — pass-through

Files:

- `packages/ai-gateway/internal/cache/layer/lookups.go`

Changes:

1. Add `ResolveModelCandidates` to the `Layer` (delegate to `store.DB.ResolveModelCandidates`). No caching for this slice in v1 — model catalog is small and Layer's broader snapshot already amortizes lookups; revisit only if profiles show hot-path cost.

### T4. Router — hydrate + matcher

Files:

- `packages/ai-gateway/internal/router/resolver.go`
- `packages/ai-gateway/internal/router/matcher.go`
- `packages/ai-gateway/internal/router/types.go` (the `routingStore` interface)

Changes:

1. Extend the `routingStore` interface with `ResolveModelCandidates(ctx context.Context, code string) ([]store.Model, error)`. Both `*store.DB` (via T2) and `*cachelayer.Layer` (via T3) satisfy it.
2. Rewrite `Resolver.hydrateRequestedModel`:
   - Skip when `rctx.RequestedModel.ID == ""` (no request model set).
   - Skip when `rctx.RequestedModel.ID == "auto"` (smart router sentinel — leave `CandidateIDs` empty so `matchConditions.models` cannot accidentally match it).
   - Otherwise call `ResolveModelCandidates(ctx, rctx.RequestedModel.ID)`. For every returned `Model`, append `m.ID` to `CandidateIDs`. Fill `ProviderID`/`Type`/`ProviderModelID` from the **first** candidate when those fields are empty (preserve today's behavior; cross-provider disambiguation is the deferred VK follow-up).
   - Lookup failures (`err != nil`): log at `Debug`, leave `CandidateIDs` empty. Routing then naturally falls through to catch-all rules — same end-user behavior as today.
3. In `matcher.RuleMatchesContext`:
   - Replace the existing `conds.Models` block with:
     ```go
     if len(conds.Models) > 0 {
         if !anyOverlap(conds.Models, ctx.RequestedModel.CandidateIDs) {
             return false
         }
     }
     ```
   - Add immediately after:
     ```go
     if len(conds.RequestedModelLiterals) > 0 {
         if !stringSliceContains(conds.RequestedModelLiterals, modelID) {
             return false
         }
     }
     ```
   - Add private helper `anyOverlap(a, b []string) bool` (set intersection by linear scan; both slices are small — no map allocation needed).

### T5. Seed — write UUIDs + literals

Files:

- `tools/db-migrate/seed/seed-routing-rules.ts`

Changes:

1. `claude-sonnet-ha`: `matchConditions.models = ['claude-sonnet-4-20250514']` → `[m('claude-sonnet-4-20250514')]`.
2. `load-balance-mini`: `matchConditions.models = ['gpt-4o-mini']` → `[m('gpt-4o-mini')]`.
3. `cost-aware-routing`: `matchConditions.models = ['gpt-4o', 'gpt-4o-mini']` → `[m('gpt-4o'), m('gpt-4o-mini')]`.
4. `smart-auto-routing`: replace `matchConditions: { models: ['auto'] }` with `matchConditions: { requestedModelLiterals: ['auto'] }`.

### T6. Existing-DB migration (one-time, hand-run via psql)

Run from repo root after T1–T5 land:

```sql
-- claude-sonnet-ha
UPDATE "RoutingRule" r
SET "matchConditions" = jsonb_set(r."matchConditions", '{models}', to_jsonb(ARRAY[m.id]))
FROM "Model" m
WHERE r.name = 'claude-sonnet-ha' AND m.code = 'claude-sonnet-4-20250514';

-- load-balance-mini
UPDATE "RoutingRule" r
SET "matchConditions" = jsonb_set(r."matchConditions", '{models}', to_jsonb(ARRAY[m.id]))
FROM "Model" m
WHERE r.name = 'load-balance-mini' AND m.code = 'gpt-4o-mini';

-- cost-aware-routing (two codes)
UPDATE "RoutingRule" r
SET "matchConditions" = jsonb_set(
  r."matchConditions",
  '{models}',
  (SELECT jsonb_agg(m.id) FROM "Model" m WHERE m.code IN ('gpt-4o', 'gpt-4o-mini'))
)
WHERE r.name = 'cost-aware-routing';

-- smart-auto-routing → move "auto" out of models into requestedModelLiterals
UPDATE "RoutingRule"
SET "matchConditions" = jsonb_build_object('requestedModelLiterals', jsonb_build_array('auto'))
WHERE name = 'smart-auto-routing';
```

A throwaway script under `tools/db-migrate/manual-scripts/` is **not** added — this is a one-shot, dev-DB only operation, and the seed (T5) is the canonical reproducer.

### T7. Tests

Files:

- `packages/ai-gateway/internal/router/matcher_test.go`
- `packages/ai-gateway/internal/router/resolver_test.go`
- `packages/ai-gateway/internal/store/model_test.go` (new file if absent)

New cases:

1. `TestRuleMatchesContext_ModelsByCandidates`: `conds.Models = [uuid-A]`, `ctx.RequestedModel.CandidateIDs = [uuid-A, uuid-B]` → match. `[uuid-C]` → no match. Empty candidates + non-empty `conds.Models` → no match.
2. `TestRuleMatchesContext_RequestedModelLiterals`: `conds.RequestedModelLiterals = ["auto"]`, `modelID = "auto"` → match. `modelID = "gpt-4o"` → no match. Empty literals → ignored (today's behavior).
3. `TestRuleMatchesContext_BothFields_AreANDed`: when both `Models` and `RequestedModelLiterals` set, both must match.
4. `TestResolveModelCandidates_CodeAndAlias`: insert a `Model` with `code = "x"`, `aliases = ['y','z']`. Lookup `"x"` → 1 row; `"y"` → 1 row; `"q"` → 0 rows.
5. `TestHydrateRequestedModel_FillsCandidates`: live `RoutingContext` with `RequestedModel.ID = "gpt-4o"` and a stub store returning two `Model`s. Assert `CandidateIDs` contains both UUIDs.
6. `TestHydrateRequestedModel_AutoKeyword_LeavesCandidatesEmpty`: `ID = "auto"` → store is **not** called; `CandidateIDs` stays `nil`.

### T8. UI — no changes

The UI already stores `Model.id` UUIDs in `<option value>` and round-trips them via `parseMatchConditionsForm` / `buildMatchConditionsPayload` (`packages/control-plane-ui/src/pages/config/routing/routing-rule-config.ts:54`). No file edits required. The dropdown will start showing the right values immediately after T6 lands because the engine and the UI now agree on UUIDs.

The `requestedModelLiterals` field has no UI surface in this story — it is only set by seed for the `smart-auto-routing` rule. Surfacing it in the form is a follow-up if a customer ever needs to author such rules from the admin UI.

## Acceptance Criteria

- (AC1) Opening any routing rule in the UI shows the same Match Conditions → Models the user previously saved (no missing rows, no UUIDs leaking into option labels).
- (AC2) `curl /v1/chat/completions -d '{"model":"gpt-4o-mini",...}'` against the seeded `load-balance-mini` rule resolves to one of its weighted targets (verified via `x-nexus-aigw-rule` response header or `/internal/routing-simulate`).
- (AC3) `curl ... -d '{"model":"auto",...}'` resolves through `smart-auto-routing` (matched via `requestedModelLiterals`).
- (AC4) `go test ./packages/ai-gateway/internal/router/... ./packages/ai-gateway/internal/store/... -race -count=1` is green.
- (AC5) `npx vitest run` in `packages/control-plane-ui` is green (existing tests unchanged; UI behavior shift is observational only).
- (AC6) `SELECT "matchConditions" FROM "RoutingRule"` shows every `models` element is a UUID string (regex `^[0-9a-f-]{36}$`); `requestedModelLiterals` exists only on `smart-auto-routing`.

## Notes for reviewer

- `RequestedModel.ID` is intentionally **not** renamed in this story — see "Out of scope" — keeping the diff scoped to behavior change. A follow-up rename PR is welcome; it is purely mechanical.
- `Model.aliases` is currently empty in the seeded DB (verified: `SELECT aliases FROM "Model" WHERE array_length(aliases,1) > 0` → 0 rows), so the alias branch in `ResolveModelCandidates` is dead-on-arrival in dev. We add it now anyway because (a) the schema column already exists and (b) it costs one `ANY()` clause to enable the future "OpenAI alias `gpt-4o-2024-08-06` → `gpt-4o`" use case without another migration.
