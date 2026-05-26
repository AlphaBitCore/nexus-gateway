# E20 Story 2 — Remove redundant `RoutingRule.modelId` column

**Epic:** 20 — Routing Rule Match Conditions Fix
**Story:** 2
**Status:** Draft — 2026-04-22
**Requirements:** `docs/developers/specs/e20/e20-routing-match-conditions-fix.md` §9
**OpenAPI:** `docs/users/api/openapi/admin/e20-s2-routing-rule-crud.yaml`

## User Story

> **As a** platform admin,
> **I want** the routing rule's applicability to be determined solely by its `matchConditions` block (the thing the UI edits),
> **so that** saving a rule in the UI cannot leave behind hidden filters that silently stop the rule from matching.

## Context

- `RoutingRule` carries a top-level `modelId String?` column plus `@@index([modelId])`. The routing engine AND's `rule.ModelID` with every `matchConditions` dimension in `ruleMatches` (`packages/ai-gateway/internal/router/resolver.go:158-173`).
- The Control Plane UI form never renders an input for this column. Only two places touch the value:
  - `detail/useRoutingRuleDetail.ts:162` — reads `rule.modelId` as a simulate-panel default.
  - Seed + admin API accept the field on write.
- Observed failure mode: `load-balance-mini` was seeded with `modelId='gpt-4o-mini'` AND `matchConditions.models=['gpt-4o-mini']` (consistent). Someone later edited `matchConditions.models` to `['moonshot-v1-8k']` via the UI; `modelId='gpt-4o-mini'` was not cleared (no UI surface for it), so the two filters became contradictory and the rule stopped matching anything. Traffic fell through to the lowest-priority catch-all `default-kmini-128k`, which pointed at a disabled target and returned zero routes.
- `matchConditions.models` already expresses "rule applies only to these model ids" with the same semantics. The column is redundant.

## Tasks

### T1. Schema — drop column + index

Files:

- `tools/db-migrate/schema.prisma`
- `tools/db-migrate/migrations/<yyyymmdd>_remove_routing_rule_model_id/migration.sql`

Changes:

1. In `schema.prisma`, remove the `modelId String?` line from `model RoutingRule` and the `@@index([modelId])` entry.
2. Run `npx prisma migrate dev --name remove_routing_rule_model_id` from `tools/db-migrate/` to generate the migration. The SQL MUST drop the index before the column and MUST be idempotent-safe (`DROP INDEX IF EXISTS`, `ALTER TABLE ... DROP COLUMN IF EXISTS`).

### T2. Seed — clean rule definitions

Files:

- `tools/db-migrate/seed/seed-routing-rules.ts`

Changes:

1. Remove the `modelId: 'gpt-4o-mini',` line from the `load-balance-mini` seed entry (currently L91).
2. Remove the `modelId: 'auto',` line from the `smart-auto-routing` seed entry (currently L141).
3. Verify both rules keep their `matchConditions.models` arrays unchanged — that is now the sole rule-level model filter.

### T3. Control Plane — store + handler

Files:

- `packages/control-plane/internal/store/routing_rule.go`
- `packages/control-plane/internal/handler/admin_routing.go`

Changes:

1. Remove `ModelID *string` from `RoutingRule` struct.
2. Remove `"modelId"` from `routingRuleColumns` (SELECT list) and from INSERT / UPDATE column lists. Shift `$n` placeholder numbers accordingly.
3. Remove `"modelId" = COALESCE(...)` from the UPDATE statement.
4. Remove `ModelID *string` from `CreateRoutingRuleParams` and `UpdateRoutingRuleParams`; remove its wiring in `CreateRoutingRule` / `UpdateRoutingRule`.
5. In `admin_routing.go`, remove `ModelID *string json:"modelId"` from both the create and the update request body structs; remove its assignment to store params.

### T4. AI Gateway — store + resolver

Files:

- `packages/ai-gateway/internal/store/routing.go`
- `packages/ai-gateway/internal/router/resolver.go`

Changes:

1. Remove `ModelID *string` from `store.RoutingRule`.
2. Remove `"modelId"` from the SELECT list in `GetEnabledRoutingRules`.
3. In `resolver.go`, simplify `ruleMatches` to:
   ```go
   func (r *Resolver) ruleMatches(rule store.RoutingRule, modelID string, rctx *RoutingContext) bool {
       if len(rule.MatchConditions) > 0 {
           var conds MatchConditions
           if err := json.Unmarshal(rule.MatchConditions, &conds); err != nil {
               return false
           }
           return RuleMatchesContext(&conds, modelID, rctx)
       }
       return true
   }
   ```

### T5. Control Plane UI — type + simulate hook

Files:

- `packages/control-plane-ui/src/api/services/routing.ts`
- `packages/control-plane-ui/src/pages/config/routing/detail/useRoutingRuleDetail.ts`

Changes:

1. Remove `modelId?: string;` from the `RoutingRule` TypeScript interface. Leave the unrelated `fallbackChain` entry shape alone.
2. In `useRoutingRuleDetail.ts:162`, change
   ```ts
   setSimModelId(rule.modelId ?? mc.models[0] ?? '');
   ```
   to
   ```ts
   setSimModelId(mc.models[0] ?? '');
   ```

### T6. OpenAPI — rule CRUD spec

Files:

- `docs/users/api/openapi/admin/e20-s2-routing-rule-crud.yaml`

Changes:

1. Create a new OpenAPI 3.1 document covering:
   - `GET /api/admin/routing-rules` — list enabled + disabled
   - `POST /api/admin/routing-rules` — create
   - `GET /api/admin/routing-rules/{id}` — read one
   - `PUT /api/admin/routing-rules/{id}` — update (partial update via nullable fields, backed by SQL `COALESCE`)
   - `DELETE /api/admin/routing-rules/{id}` — delete
2. The `RoutingRule` response schema MUST NOT include `modelId`. Fields exposed: `id`, `name`, `description`, `strategyType`, `config`, `matchConditions`, `priority`, `pipelineStage`, `fallbackChain`, `enabled`, `createdAt`, `updatedAt`.
3. Request bodies (create + update) MUST NOT include `modelId`.

### T7. Regression test

Files:

- `packages/ai-gateway/internal/router/resolver_test.go`

Changes:

1. Add a table-driven test `TestRuleMatches_MatchConditionsOnly` asserting:
   - Rule with `matchConditions = {}` → matches every requested model.
   - Rule with `matchConditions = {"models":["gpt-4"]}` → matches `gpt-4`, does not match `gpt-3.5`.
   - Rule with `matchConditions = {"models":["gpt-4"], "providers":["openai"]}` → both dimensions AND'd.

## Acceptance Criteria

| AC | Description |
|---|---|
| AC1 | `docker exec … psql … -c '\d "RoutingRule"'` shows no `modelId` column and no `RoutingRule_modelId_idx` index. |
| AC2 | `go build ./...` succeeds at repo root. |
| AC3 | `go test -race -count=1 ./packages/ai-gateway/... ./packages/control-plane/...` passes, including the new `TestRuleMatches_MatchConditionsOnly` cases. |
| AC4 | `npm test --workspace=packages/control-plane-ui` passes; `RoutingRule` type in `api/services/routing.ts` no longer declares `modelId`. |
| AC5 | Manual simulate: POST `/api/admin/routing-rules/{id}/simulate` with `{modelId:"moonshot-v1-8k", endpointType:"chat/completions"}` against the `load-balance-mini` rule returns `ruleName: "load-balance-mini"` (not `default-kmini-128k`). |
| AC6 | `docs/users/api/openapi/admin/e20-s2-routing-rule-crud.yaml` validates as OpenAPI 3.1 and does not list `modelId` anywhere in the `RoutingRule` schema or request bodies. |

## Out of Scope

- Data migration for existing dev DB rows beyond `prisma migrate dev` + `prisma db seed`.
- Any other refactor of the routing engine, narrowing logic, or strategy evaluation.
- Introducing `@deprecated` markers on any field (forbidden per dev-phase policy).

## Risks

- **`load-balance-mini` behavior change (intentional):** with the column gone, its `matchConditions.models=['moonshot-v1-8k']` is the sole filter — the rule starts matching `moonshot-v1-8k` requests. This is the bug fix this story exists to deliver; not a regression.
- **Go workspace cross-module coupling:** struct fields removed in `control-plane/internal/store` force matching edits in the admin handler. All edits must land in one atomic commit so the `go.work` build is never broken at HEAD.
