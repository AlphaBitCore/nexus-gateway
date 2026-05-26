# Quota System Fixes — Design Spec

**Date:** 2026-04-16
**Scope:** Fix quota alert logic, deprecate legacy Manager, cold-start backfill, VK budget enforcement, alertThresholds UI

## Problem Statement

The quota system has several correctness and completeness gaps:

1. **P1 — Alert checker ignores QuotaPolicy**: `checkQuotaAlerts` only iterates `QuotaOverride` rows. Entities governed by `QuotaPolicy` (e.g. "all Engineering users monthly $300") never produce alerts.
2. **P2 — Hardcoded alert thresholds**: Thresholds are `[80, 95]` in code. `QuotaPolicy.alertThresholds` field (DB default `[80, 90]`) is stored but never read.
3. **P3 — Dual engine with split counters**: Legacy `Manager` (Postgres `Quota` table) and new `Engine` (Redis `UsageCache`) coexist. When Engine is active, Manager is skipped, making `Quota.currentCostUsd` permanently stale.
4. **P4 — UsageCache cold-start gap**: Redis restart → all usage keys return 0 → Engine sees zero spend → limits not enforced until traffic re-accumulates.
5. **P5 — VK BudgetLimitUsd never enforced**: `VKMeta.BudgetLimitUsd` is populated from DB but never checked by either engine.
6. **P7 — No UI for alertThresholds**: QuotaPolicy Create/Edit forms omit the `alertThresholds` field. It can only be set via direct API call.

P6 (alert notifications/webhooks) is deferred to a future iteration.

## Work Items

### W1: Policy-Based Alert Generation + Configurable Thresholds

**Files changed:**
- `packages/control-plane/internal/jobs/quota_alert_check.go` (rewrite)
- `packages/control-plane/internal/store/quota_override.go` (new helper)

**Design:**

The alert check job runs every 60s and executes two phases:

**Phase A — Override-based alerts (existing, improved):**
1. Load all `QuotaOverride` rows via `ListQuotaOverrides`.
2. For each override with `CostLimitUsd > 0`:
   - Find matching `QuotaPolicy` for this target (by scope + orgId + vkType) to read `alertThresholds`.
   - If no matching policy, use default thresholds `[80, 95]`.
   - Query rollup for current period cost.
   - For each threshold where `usagePct >= threshold`, upsert alert with `OverrideID`.

**Phase B — Policy-based alerts (new):**
1. Load all enabled `QuotaPolicy` rows via `ListQuotaPolicies(Enabled: true)`.
2. Build a set of override targets (`"targetType:targetId"`) for quick lookup.
3. For each policy with `CostLimitUsd > 0`:
   - Map `policy.Scope` to rollup dimension.
   - Query rollup for current period — returns all `dimension=entityId` rows.
   - For each entity in the result set:
     - **Skip** if entity has a `QuotaOverride` (already handled in Phase A).
     - Optionally filter by `policy.OrganizationID` / `policy.VKType` if set.
     - Parse `policy.AlertThresholds` → `[]int` (default `[80, 95]` on parse failure).
     - Compute `usagePct = (entityCost / policy.CostLimitUsd) * 100`.
     - For each threshold where `usagePct >= threshold`, upsert alert with `PolicyID`.

**New helper — `parseAlertThresholds`:**
```go
func parseAlertThresholds(raw json.RawMessage) []int {
    var thresholds []int
    if err := json.Unmarshal(raw, &thresholds); err != nil || len(thresholds) == 0 {
        return []int{80, 95}
    }
    sort.Ints(thresholds)
    return thresholds
}
```

**New store method — `ListQuotaOverrideTargetKeys`:**
```go
func (db *DB) ListQuotaOverrideTargetKeys(ctx context.Context) (map[string]bool, error)
// Returns set of "targetType:targetId" strings for all QuotaOverride rows.
// Used by Phase B to skip entities that already have overrides.
```

**Alert resolution:** When a previously-breached entity drops below all thresholds, call `ResolveQuotaAlerts` (already exists) to set status='resolved'.

### W2: UI — alertThresholds in QuotaPolicy Forms

**Files changed:**
- `packages/control-plane-ui/src/pages/config/quota-policies/QuotaPolicyCreate.tsx`
- `packages/control-plane-ui/src/pages/config/quota-policies/QuotaPolicyEdit.tsx`
- `packages/control-plane-ui/src/pages/config/quota-policies/QuotaPolicyForm.tsx`
- `packages/control-plane-ui/src/pages/config/quota-policies/QuotaPolicyList.tsx`
- `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json`

**Design:**

1. **Form field**: Add to "Limits & Enforcement" section.
   - Label: en: "Alert Thresholds (%)" / es: "Umbrales de alerta (%)" (zh in locale file)
   - Input: comma-separated text field (simple, no complex chip UI needed)
   - Placeholder: "80, 90, 95"
   - Default value: `[80, 90]` (matches Prisma schema default)
   - Zod validation: array of integers, each 1-100, deduplicated, sorted ascending

2. **Zod schema addition:**
   ```typescript
   alertThresholds: z.string()
     .transform(s => s.split(',').map(v => parseInt(v.trim(), 10)).filter(n => !isNaN(n) && n >= 1 && n <= 100))
     .pipe(z.array(z.number().int().min(1).max(100)))
     .default('80, 90')
   ```

3. **List table**: Add column showing thresholds as inline badges, e.g. `80% · 90%`.

4. **onSubmit**: Include `alertThresholds: number[]` in the API payload.

### W3: Delete Legacy Manager + Quota Table (complete removal)

**Files deleted:**
- `packages/ai-gateway/internal/pipeline/quota/manager.go`
- `packages/control-plane/internal/handler/admin_quotas.go`
- `packages/control-plane/internal/store/quota.go` (or equivalent file with Quota CRUD)

**Files modified:**
- `packages/ai-gateway/internal/handler/proxy.go` — remove Manager fallback in `checkQuota`, remove Manager reconcile in `handleStream`/`handleNonStream`
- `packages/ai-gateway/cmd/ai-gateway/main.go` — remove `quotaManager` init, remove `Deps.QuotaManager`
- `packages/ai-gateway/internal/handler/deps.go` (or equivalent) — remove `QuotaManager` field from Deps struct
- `packages/control-plane/cmd/control-plane/main.go` — remove Quota route registration
- `packages/control-plane/internal/jobs/scheduler.go` — remove `quota-reset` job (resets expired Quota rows)
- `tools/db-migrate/schema.prisma` — delete `model Quota { ... }` and `VirtualKey.quotas` relation
- `tools/db-migrate/seed/seed.ts` — delete 6 Quota seed rows
- New Prisma migration: `DROP TABLE "Quota"`

**Verification:** `go build ./packages/ai-gateway/...` and `go build ./packages/control-plane/...` must pass with zero references to Manager or Quota CRUD.

### W4: UsageCache Cold-Start Backfill

**Files changed:**
- `packages/ai-gateway/internal/pipeline/quota/usage_cache.go` (new method)
- `packages/ai-gateway/cmd/ai-gateway/main.go` (call on startup)

**Design:**

New method on `UsageCache`:
```go
func (c *UsageCache) Backfill(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error
```

Logic:
1. Determine current monthly period key: `time.Now().UTC().Format("2006-01")`
2. Compute period start/end.
3. For each dimension type (`user`, `virtual_key`, `organization`):
   - Call `QueryRollup` with the dimension and current period range.
   - For each returned row, extract entityID from `dimensionKey` (e.g. `"user=abc"` → `"abc"`).
   - Sum cost per entity.
   - For each entity: `SETNX` the Redis key `quota:usage:{dim}:{entityID}:{periodKey}` with value `costCents`. SETNX ensures we don't overwrite keys that already have live-accumulated data.
   - Set TTL via `periodTTL`.
4. Log count of keys backfilled.

**Call site:** `main.go`, after `policyCache.Load()` and `usageCache` creation:
```go
if err := usageCache.Backfill(ctx, db.Pool, logger); err != nil {
    logger.Error("usage cache backfill failed", "error", err)
    // Non-fatal — continue startup, engine degrades to fail-open
}
```

**Note:** Backfill imports `store.QueryRollup` logic. To avoid circular dependency, `Backfill` takes a raw `*pgxpool.Pool` and runs the SQL directly (or we extract the rollup query into a shared helper). Decision: run the rollup SQL inline in `usage_cache.go` since it's a single simple query.

### W5: VK BudgetLimitUsd Enforcement

**Files changed:**
- `packages/ai-gateway/internal/handler/proxy.go` — `checkQuota` method

**Design:**

Add a pre-check at the beginning of `checkQuota`, before `Engine.Check`:

```go
// VK hard budget check — independent of Policy/Override system
if vkMeta.BudgetLimitUsd != nil && *vkMeta.BudgetLimitUsd > 0 {
    monthlyKey := time.Now().UTC().Format("2006-01")
    currentCents, _ := h.deps.QuotaEngine.UsageForTarget("virtual_key", vkMeta.ID, monthlyKey)
    currentUsd := float64(currentCents) / 100
    if currentUsd + estimate.EstimatedCost() > *vkMeta.BudgetLimitUsd {
        h.writeError(w, rec, http.StatusTooManyRequests, "Virtual key budget limit exceeded")
        return nil, 0, 0, QuotaDecision{Allowed: false, Action: "reject"}
    }
}
```

Expose a thin method on Engine (or directly on UsageCache):
```go
func (e *Engine) UsageForTarget(targetType, targetID, periodKey string) (int64, error) {
    return e.usageCache.GetUsage(context.Background(), targetType, targetID, periodKey)
}
```

**Semantics:** BudgetLimitUsd is a monthly hard reject. No downgrade, no notify — if the VK's monthly spend exceeds the budget, the request is rejected. This is the VK owner's explicit cap, distinct from organizational quota policies.

## Execution Order

```
W3 (delete Manager+Quota) → W4 (cold-start backfill) → W1 (alert logic) → W2 (UI) → W5 (VK budget)
```

## Out of Scope

- P6: Alert notifications (email, Slack, webhook) — future iteration
- Weekly/daily quota period support in alert checker (currently monthly only) — can be added later
- Quota dashboard chart/trend visualization improvements
