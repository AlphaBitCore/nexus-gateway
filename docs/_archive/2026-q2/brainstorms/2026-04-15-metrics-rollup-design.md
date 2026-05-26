# Metrics Rollup Infrastructure — Design Specification

**Date:** 2026-04-15
**Status:** Approved (brainstorm complete)
**Architecture:** [docs/dev/metrics-rollup-architecture.md](../../dev/metrics-rollup-architecture.md)

## 1. Overview

Replace direct `traffic_event` queries in all analytics/statistics pages with a pre-aggregated metrics rollup system. The system uses cascading time-bucketed aggregation (5m → 1h → 1d → 1mo) with a universal EAV dimension model, computed by scheduled jobs in the Control Plane.

## 2. Key Decisions (from brainstorm)

| # | Decision | Choice | Rationale |
|---|----------|--------|-----------|
| 1 | Freshness tiers | T+0 (real-time) / T+N min / T+N hour-day | Quota stays real-time; dashboard tolerates 1-5min; reports tolerate hours |
| 2 | Dimension model | EAV universal (dimension_key + sub_dimension) | Extensibility: new metrics/dimensions without schema changes |
| 3 | Cross-dimension filtering | Hybrid: low-cardinality as sub_dimension; high-cardinality falls back to raw table | Controlled expansion; source (3 vals) and data_classification (5 vals) pre-computed |
| 4 | Storage | 4 separate tables per granularity | Independent retention, write isolation, index efficiency |
| 5 | Computation | Cascading merge with watermark; only bottom layer touches raw table | Upper layers compute from lower layer; minimal DB load |
| 6 | Correction | Daily full recompute of T-1 | Handles late-arriving events; ensures eventual exactness |
| 7 | Distinct counts | Independent COUNT(DISTINCT) per granularity layer | Avoids PostgreSQL HLL extension dependency |
| 8 | Percentiles | 6-bucket histogram in metadata JSONB | Mergeable across time buckets; ~2% accuracy acceptable |
| 9 | Monthly table | metric_rollup_1mo with 5-year retention | Enterprise requirement: 3-5 year historical data |
| 10 | Exactly-once | DELETE + INSERT in transaction per sealed bucket | Watermark + transaction = idempotent; no duplicate accumulation |

## 3. Scope

### In Scope
- 4 new database tables (metric_rollup_5m/1h/1d/1mo)
- 6 new scheduled jobs (rollup-5m, rollup-1h, rollup-1d, rollup-1mo, rollup-correction, rollup-retention)
- Metrics Query Service (Go, in Control Plane)
- Unified query API with auto granularity selection
- Migration of existing analytics endpoints to use rollup data
- Migration of legacy MetricRollup table to new schema
- ~30 pre-aggregated metrics across 5 domains

### Out of Scope
- Real-time streaming (Quota enforcement stays in Redis — unchanged)
- External OLAP systems (ClickHouse, TimescaleDB)
- Frontend component changes (API layer adapts internally)
- New analytics UI features (this spec enables them; building them is separate)

## 4. Data Model

### 4.1 Table DDL (identical structure × 4 tables)

```sql
CREATE TABLE metric_rollup_5m (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bucket_start    TIMESTAMPTZ NOT NULL,
    metric_name     TEXT NOT NULL,
    dimension_key   TEXT NOT NULL DEFAULT '',
    sub_dimension   TEXT NOT NULL DEFAULT '',
    value           DECIMAL(24,6) NOT NULL,
    metadata        JSONB,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (bucket_start, metric_name, dimension_key, sub_dimension)
);

CREATE INDEX idx_mr5m_metric_bucket ON metric_rollup_5m (metric_name, bucket_start);
CREATE INDEX idx_mr5m_dim_bucket ON metric_rollup_5m (dimension_key, bucket_start);
```

Repeat for `metric_rollup_1h`, `metric_rollup_1d`, `metric_rollup_1mo`.

### 4.2 Watermark Table

```sql
CREATE TABLE rollup_watermark (
    job_name        TEXT PRIMARY KEY,
    watermark       TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

Tracks per-job progress: `rollup-5m`, `merge-1h`, `merge-1d`, `merge-1mo`.

### 4.3 Retention

| Table | Retention | Cleanup |
|-------|-----------|---------|
| `metric_rollup_5m` | 7 days | Daily `DELETE WHERE bucket_start < NOW() - 7 days` |
| `metric_rollup_1h` | 90 days | Daily `DELETE WHERE bucket_start < NOW() - 90 days` |
| `metric_rollup_1d` | 365 days | Daily `DELETE WHERE bucket_start < NOW() - 365 days` |
| `metric_rollup_1mo` | 5 years | Daily `DELETE WHERE bucket_start < NOW() - 1825 days` |

## 5. Complete Metrics Catalog

### 5.1 Traffic Core (Tier 1)

| Metric | Aggregation | Merge Rule |
|--------|-------------|------------|
| `request_count` | COUNT(*) | SUM |
| `status_2xx_count` | COUNT(status < 400) | SUM |
| `status_4xx_count` | COUNT(400 ≤ status < 500) | SUM |
| `status_5xx_count` | COUNT(status ≥ 500) | SUM |
| `timeout_count` | COUNT(timeout condition) | SUM |
| `cache_hit_count` | COUNT(cache_hit = true) | SUM |
| `prompt_tokens` | SUM(prompt_tokens) | SUM |
| `completion_tokens` | SUM(completion_tokens) | SUM |
| `total_tokens` | SUM(total_tokens) | SUM |
| `estimated_cost_usd` | SUM(estimated_cost_usd) | SUM |
| `cache_saved_cost_usd` | SUM(cost WHERE cache_hit) | SUM |
| `wasted_cost_usd` | SUM(cost WHERE status ≥ 400) | SUM |
| `latency_sum` | SUM(latency_ms) | SUM |
| `latency_count` | COUNT(latency_ms IS NOT NULL) | SUM |
| `latency_histogram` | 6-bucket counts in metadata | Element-wise add |
| `ttft_sum` | SUM(details->ttft_ms) | SUM |
| `ttft_count` | COUNT(ttft IS NOT NULL) | SUM |

### 5.2 Routing (Tier 1)

| Metric | Aggregation | Merge Rule |
|--------|-------------|------------|
| `routing_fallback_count` | COUNT(routed_provider ≠ provider) | SUM |
| `routing_rule_hit_count` | COUNT(routing_rule_id NOT NULL) | SUM |
| `model_shift_count` | COUNT(model ≠ routed_model) | SUM |

### 5.3 Compliance & Proxy (Tier 2)

| Metric | Aggregation | Merge Rule |
|--------|-------------|------------|
| `hook_allow_count` | COUNT(hook_decision = 'allow') | SUM |
| `hook_deny_count` | COUNT(hook_decision = 'deny') | SUM |
| `hook_error_count` | COUNT(hook_decision = 'error') | SUM |
| `hook_unknown_count` | COUNT(hook_decision = 'unknown') | SUM |
| `hook_latency_sum` | SUM(hookLatencyMs) | SUM |
| `hook_latency_count` | COUNT(hookLatencyMs) | SUM |
| `hook_latency_histogram` | 6-bucket counts in metadata | Element-wise add |
| `bump_success_count` | COUNT(BUMP_SUCCESS) | SUM |
| `bump_failed_count` | COUNT(BUMP_FAILED_PASSTHROUGH) | SUM |
| `bump_exempt_count` | COUNT(exempt statuses) | SUM |
| `bump_disabled_count` | COUNT(BUMP_DISABLED_EMERGENCY) | SUM |
| `proxy_request_count` | COUNT(source = 'proxy') | SUM |
| `classification_count` | COUNT per data_classification level | SUM |
| `reject_count` | COUNT(hook_decision = 'reject') | SUM |

### 5.4 Usage & Growth (Tier 2)

| Metric | Aggregation | Merge Rule |
|--------|-------------|------------|
| `active_users` | COUNT(DISTINCT user_id) | Independent per layer |
| `active_virtual_keys` | COUNT(DISTINCT virtual_key_id) | Independent per layer |
| `active_devices` | COUNT(DISTINCT device_id) | Independent per layer |
| `active_organizations` | COUNT(DISTINCT organization_id) | Independent per layer |

### 5.5 Discovery-Specific (Tier 2)

| Metric | Aggregation | Merge Rule |
|--------|-------------|------------|
| `distinct_sources` | COUNT(DISTINCT source_ip) | Independent per layer |
| `first_seen` | MIN(timestamp) in metadata | MIN of MINs |
| `last_seen` | MAX(timestamp) in metadata | MAX of MAXs |

### 5.6 Fleet — Migrated from Legacy (Tier 3)

| Metric | Notes |
|--------|-------|
| `device_fleet_status` | Existing logic → metric_rollup_1h |
| `device_fleet_os` | Same |
| `agent_action_volume` | Same |

### 5.7 Histogram Buckets

```
[0, 50ms)  [50, 100ms)  [100, 200ms)  [200, 500ms)  [500, 1000ms)  [1000ms, +∞)

metadata format: {"buckets": [1234, 567, 890, 234, 56, 12]}
```

## 6. Computation Pipeline

### 6.1 Job Registry

| Job | Interval | Source → Target |
|-----|----------|-----------------|
| `rollup-5m` | 60s | traffic_event → metric_rollup_5m |
| `rollup-1h` | 5min | metric_rollup_5m → metric_rollup_1h |
| `rollup-1d` | 1h | metric_rollup_1h → metric_rollup_1d |
| `rollup-1mo` | 24h | metric_rollup_1d → metric_rollup_1mo |
| `rollup-correction` | 24h @ 03:00 | traffic_event → full T-1 recompute |
| `rollup-retention` | 24h | DELETE expired per table |

### 6.2 rollup-5m: Exactly-Once Semantics

```
Per sealed 5-minute bucket, in one transaction:
  1. DELETE FROM metric_rollup_5m WHERE bucket_start = ?
  2. INSERT INTO metric_rollup_5m SELECT <aggregations>
     FROM traffic_event
     WHERE timestamp >= bucket_start AND timestamp < bucket_start + 5min
  3. UPDATE rollup_watermark SET watermark = bucket_start WHERE job_name = 'rollup-5m'
  COMMIT
```

Unseal (current) bucket is never processed — wait for next round.

### 6.3 Upper-Layer Merge

```
Per sealed time boundary, in one transaction:
  1. DELETE target rows for this boundary
  2. INSERT INTO target SELECT
       date_trunc(granularity, bucket_start),
       metric_name, dimension_key, sub_dimension,
       SUM(value), merge_metadata(metadata)
     FROM source_table
     WHERE bucket_start >= boundary AND bucket_start < boundary + interval
     GROUP BY 1,2,3,4
  3. UPDATE merge watermark
  COMMIT
```

### 6.4 Dimension Expansion

Each traffic_event expands to:
- 1 global dimension_key (`""`)
- N non-null dimension values (up to 11)
- × applicable sub_dimension combinations

Within a 5-minute bucket, identical tuples collapse via aggregation — actual row count is bounded by unique combinations.

### 6.5 Correction Job

```
Daily at 03:00:
  1. Delete all metric_rollup_5m for T-1
  2. Full recompute from traffic_event for T-1
  3. Re-merge 1h for T-1
  4. Re-merge 1d for T-1
  5. If T-1 = last day of month → re-merge 1mo
```

## 7. Query Layer

### 7.1 Auto Granularity

| Time Span | Table | Granularity |
|-----------|-------|-------------|
| ≤ 6h | metric_rollup_5m | 5 min |
| 6h – 7d | metric_rollup_1h | 1 hour |
| 7d – 90d | metric_rollup_1d | 1 day |
| > 90d | metric_rollup_1mo | 1 month |

### 7.2 Unified Interface

```go
type MetricsQuery struct {
    Metrics      []string
    DimensionKey string
    SubDimension string
    StartTime    time.Time
    EndTime      time.Time
    TopN         int
    TimeSeries   bool
}

type MetricsResult struct {
    Granularity string
    Summary     map[string]float64
    Series      []MetricsBucket
    Groups      []MetricsGroup
    Metadata    map[string]interface{}
}
```

### 7.3 Existing API Migration Map

| Current Endpoint | MetricsQuery Translation |
|------------------|--------------------------|
| `GET /analytics/summary` | Metrics=[request_count, status_4xx_count, status_5xx_count, total_tokens, prompt_tokens, completion_tokens, estimated_cost_usd, latency_sum, latency_count], DimensionKey="", SubDimension="source=vk" |
| `GET /analytics/by-provider` | Metrics=[request_count, total_tokens, estimated_cost_usd, latency_sum, latency_count], DimensionKey="provider", SubDimension="source=vk" |
| `GET /analytics/usage?groupBy=X` | Metrics=[request_count, prompt_tokens, completion_tokens, total_tokens], DimensionKey=X, SubDimension="source=vk" |
| `GET /analytics/cost?groupBy=X` | Metrics=[estimated_cost_usd, request_count, total_tokens], DimensionKey=X, SubDimension="source=vk" |
| `GET /proxy/compliance/coverage` | Metrics=[bump_success_count, bump_failed_count, bump_exempt_count, bump_disabled_count, proxy_request_count], DimensionKey="", SubDimension="source=proxy" |
| `GET /proxy/compliance/hook-health` | Metrics=[hook_allow_count, hook_deny_count, hook_error_count, hook_unknown_count, hook_latency_histogram], DimensionKey="", SubDimension="source=proxy" |
| `GET /proxy/compliance/reject-stats` | Metrics=[reject_count], DimensionKey="target_host"/"source_ip"/"reason_code", SubDimension="source=proxy", TopN=10 |
| `GET /proxy/discovery/hosts` | Metrics=[request_count, bump_success_count, bump_failed_count, bump_exempt_count, bump_disabled_count, latency_histogram, distinct_sources, first_seen, last_seen], DimensionKey="target_host", SubDimension="source=proxy" |
| `GET /fleet-analytics/trends` | Metrics=[device_fleet_status], DimensionKey="", TimeSeries=true |
| `GET /fleet-analytics/top-destinations` | Metrics=[request_count, active_devices], DimensionKey="target_host", SubDimension="source=agent", TopN=10 |
| `GET /metrics/aggregates` | Metrics=[request_count, token_usage, estimated_cost, error_count, cache_hits, latency_p50], TimeSeries=true |
| Dashboard P95 Latency | Metrics=[latency_histogram], DimensionKey="", SubDimension="source=vk" → compute P95 from histogram |
| Dashboard Sparklines | Metrics=[request_count, ...], TimeSeries=true, auto 1d granularity |

### 7.4 Degradation

```
Rollup query → has data → return (X-Metrics-Source: rollup)
            → no data  → fallback to traffic_event query (X-Metrics-Source: raw)
```

Frontend can optionally show "Data is being pre-computed" when source is `raw`.

## 8. Implementation Phases

### Phase 1: Foundation (Schema + Computation)
- Prisma migration: 4 rollup tables + watermark table
- Go types: metric names, dimension enums, query/result structs
- rollup-5m job: full computation pipeline with watermark + transaction
- rollup-retention job
- Unit tests for aggregation logic

### Phase 2: Cascading Merge + Correction
- rollup-1h, rollup-1d, rollup-1mo merge jobs
- rollup-correction daily job
- Integration tests: verify cascading produces correct results

### Phase 3: Query Service + API Migration
- MetricsQueryService with auto granularity selection
- Migrate existing analytics handlers one by one
- Degradation fallback logic
- Verify all analytics pages still work identically

### Phase 4: Legacy Migration + New Metrics
- Migrate fleet metrics from old MetricRollup table
- Drop old MetricRollup table
- Add Tier 2 metrics (active_users, cache_saved_cost, etc.)
- New API endpoints for previously unavailable metrics

## 9. Acceptance Criteria

1. All existing analytics pages display identical data (within histogram approximation tolerance)
2. Dashboard page loads in <200ms total API time (currently 2-5s under load)
3. Analytics queries do not touch `traffic_event` under normal operation
4. 5-minute data freshness for dashboard-grade metrics
5. Daily correction ensures data accuracy within 24 hours
6. Rollup data retained: 7d (5m), 90d (1h), 365d (1d), 5y (1mo)
7. No PostgreSQL extensions required (pure SQL + JSONB)
8. All rollup jobs are idempotent and crash-safe
9. Existing MetricRollup table fully migrated and dropped
10. Frontend requires zero component changes (API layer adapts)

## 10. Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Dimension expansion produces too many rows | Storage + write pressure | Monitor row counts; cap dimensions if needed |
| 6-bucket histogram too imprecise for P95 | Inaccurate percentile display | Can increase to 10-12 buckets later; design supports it |
| Late events beyond T-1 correction window | Missing data in rollups | Manual correction trigger endpoint; monitor late-event volume |
| Rollup job takes longer than interval | Data lag accumulates | Monitor job duration; alert if > 50% of interval |
| TTFT data not available in traffic_event | ttft_sum/ttft_count always 0 | Graceful handling; add TTFT capture to audit pipeline separately |
