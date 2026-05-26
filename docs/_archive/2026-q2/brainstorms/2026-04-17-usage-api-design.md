# AI Gateway Usage API — Design Spec

**Date**: 2026-04-17
**Status**: Approved

## Problem

API consumers (developers using Virtual Keys) have no self-service way to check their quota status, usage trends, or per-model cost breakdowns. The existing `GET /v1/usage` endpoint returns only all-time flat totals with no time filtering, no breakdowns, and no quota context.

The Control Plane has rich analytics, but those are admin-only (session auth), inaccessible to VK-authenticated consumers.

## Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Primary consumer | VK holders + application backends | Self-service first, composable for polling |
| Time granularity | Summary + daily buckets | Covers 95% of use cases without arbitrary granularity |
| Breakdown dimensions | Model + Provider | Sufficient for cost reconciliation and routing visibility |
| Quota status | VK-level only | Org/project quotas are admin concerns |
| Endpoint structure | Two endpoints | Separate fast (Redis) from rich (DB) paths |

## Architecture

```
GET /v1/usage         → Redis UsageCache + PolicyCache (in-memory)  → <5ms
GET /v1/usage/daily   → traffic_event table (DB, time-bounded)      → 50-200ms
```

Both endpoints require VK authentication (same as `/v1/chat/completions`).

The daily endpoint queries `traffic_event` directly (not metric rollups) because:
- Rollup tables use single-dimension grouping per row; we need model × provider per VK per day
- `traffic_event` has indexes on `identity->'credential'->>'id'` and `timestamp`
- Time-bounded queries (max 90 days) keep scan sizes manageable

No new database tables or Redis structures are required.

## Endpoint 1: GET /v1/usage

**Purpose**: Real-time summary of current billing period usage + quota status.

**Auth**: VK required (Bearer token)

**Query params**: None (always returns current billing period)

**Response** (200 OK):

```json
{
  "virtualKeyId": "clx1vk000001",
  "period": "2026-04",
  "periodType": "monthly",
  "usage": {
    "totalRequests": 1542,
    "promptTokens": 482000,
    "completionTokens": 196000,
    "totalTokens": 678000,
    "estimatedCostUsd": 12.34
  },
  "quota": {
    "limitUsd": 100.00,
    "usedUsd": 12.34,
    "remainingUsd": 87.66,
    "enforcementMode": "reject",
    "rateLimitRpm": 60
  }
}
```

**`quota` field behavior**:
- `null` when no budget policy applies to this VK
- `rateLimitRpm` is `null` when no rate limit is configured
- `usedUsd` comes from Redis `quota:usage:vk:{id}:{period}` (same source as the quota enforcement engine)
- `limitUsd` comes from the in-memory PolicyCache (VK hard budget) or QuotaPolicy/QuotaOverride

**Data flow**:
1. VK authentication via `vkauth.Authenticator`
2. Read current period key from `quota.CurrentPeriodKey("monthly")`
3. Read `usedCents` from `QuotaEngine.UsageForTarget(ctx, "virtual_key", vkMeta.ID, periodKey)`
4. Read VK hard budget from `vkMeta.BudgetLimitUsd`
5. If quota engine exists, read policy/override limits from `PolicyCache.FindPolicy` / `PolicyCache.GetOverride`
6. Read request count from Redis or fall back to a lightweight DB count
7. Assemble and return JSON

**Performance**: All reads from Redis + in-memory caches. Expected latency < 5ms.

## Endpoint 2: GET /v1/usage/daily

**Purpose**: Daily time-series with per-model/provider breakdowns for trend analysis and billing reconciliation.

**Auth**: VK required (Bearer token)

**Query params**:
| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `startDate` | `YYYY-MM-DD` | 30 days ago | Inclusive start date |
| `endDate` | `YYYY-MM-DD` | Today | Inclusive end date |

**Constraints**: Maximum range of 90 days. Returns `400` with error code `USAGE_RANGE_TOO_LARGE` if exceeded.

**Response** (200 OK):

```json
{
  "virtualKeyId": "clx1vk000001",
  "startDate": "2026-03-18",
  "endDate": "2026-04-17",
  "daily": [
    {
      "date": "2026-04-17",
      "requests": 85,
      "promptTokens": 24000,
      "completionTokens": 9800,
      "totalTokens": 33800,
      "costUsd": 0.68,
      "models": [
        {
          "model": "gpt-4o",
          "provider": "openai",
          "requests": 50,
          "promptTokens": 18000,
          "completionTokens": 7200,
          "totalTokens": 25200,
          "costUsd": 0.52
        },
        {
          "model": "claude-sonnet-4-20250514",
          "provider": "anthropic",
          "requests": 35,
          "promptTokens": 6000,
          "completionTokens": 2600,
          "totalTokens": 8600,
          "costUsd": 0.16
        }
      ]
    }
  ],
  "totals": {
    "requests": 1542,
    "promptTokens": 482000,
    "completionTokens": 196000,
    "totalTokens": 678000,
    "costUsd": 12.34
  }
}
```

**`daily` array**: One entry per date with data. Days with zero usage are omitted (sparse).

**`models` array**: Sorted by `costUsd` descending within each day. Each entry is a unique (model, provider) pair.

**`totals`**: Convenience aggregation across the entire requested range.

**Data flow**:
1. VK authentication
2. Parse and validate `startDate`/`endDate` (max 90 days)
3. Query `traffic_event`:
   ```sql
   SELECT
       date_trunc('day', timestamp)::date AS day,
       model_name,
       provider_name,
       COUNT(*) AS requests,
       COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
       COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
       COALESCE(SUM(total_tokens), 0) AS total_tokens,
       COALESCE(SUM(estimated_cost_usd), 0)::float8 AS cost_usd
   FROM traffic_event
   WHERE source = 'ai-gateway'
     AND identity->'credential'->>'id' = $1
     AND timestamp >= $2
     AND timestamp < $3
   GROUP BY day, model_name, provider_name
   ORDER BY day DESC, cost_usd DESC
   ```
4. Transform rows into the nested daily → models structure
5. Compute `totals` from the daily sums
6. Return JSON

**Performance**: Indexed on `(identity->'credential'->>'id', timestamp)`. 90-day window for a single VK typically scans thousands to low millions of rows. Expected latency 50-200ms.

**Caching**: Results can be cached in Redis for 5 minutes keyed by `usage:daily:{vkId}:{startDate}:{endDate}`. Not implemented in v1 — add if latency becomes a concern.

## Error Responses

All errors use the existing `writeDetailedErr` pattern:

| Status | Code | Condition |
|--------|------|-----------|
| 401 | `AUTH_MISSING` | No VK token provided |
| 401 | `AUTH_INVALID` | VK token invalid or expired |
| 400 | `USAGE_RANGE_TOO_LARGE` | Requested range > 90 days |
| 400 | `USAGE_INVALID_DATE` | Malformed date parameter |
| 500 | `USAGE_QUERY_FAILED` | Database error |

## Backward Compatibility

The existing `GET /v1/usage` response is a strict subset of the new response:
- Old fields (`virtualKeyId`, `totalRequests`, `promptTokens`, `completionTokens`, `totalTokens`, `estimatedCostUsd`) remain at `usage.*`
- New fields (`period`, `periodType`, `quota`) are additive
- The old flat response shape is replaced, but the data is equivalent

Since this is a pre-release API, breaking changes are acceptable. If backward compatibility were needed, the old response shape could be preserved behind `Accept: application/vnd.nexus.usage.v1+json`.

## Files to Create/Modify

| File | Action | Description |
|------|--------|-------------|
| `packages/ai-gateway/internal/handler/usage.go` | Rewrite | New `UsageSummaryHandler` + `UsageDailyHandler` replacing old `UsageHandler` |
| `packages/ai-gateway/internal/store/usage.go` | Add | `GetDailyUsageForVK(ctx, vkID, start, end)` query |
| `packages/ai-gateway/cmd/ai-gateway/main.go` | Modify | Register `GET /v1/usage/daily`, update `/v1/usage` handler |
| `docs/openapi/ai-gateway-v1.yaml` | Modify | Add both endpoint specs with schemas |
| `packages/ai-gateway/internal/handler/usage_test.go` | Create | Table-driven tests for both handlers |
| `packages/ai-gateway/internal/store/usage_test.go` | Modify | Add test for daily query builder |

## Out of Scope

- Hourly or weekly granularity (future enhancement)
- Org/project-level quota visibility (admin concern, stays in CP)
- Usage webhooks / budget alerts (separate feature)
- Response caching for daily endpoint (add later if needed)
- Historical quota limit snapshots (complex, low priority)
