---
doc: metrics-rollup-architecture
area: cross-cutting
service: observability
tier: 1
updated: 2026-05-20
---

# Metrics Rollup Architecture

> Enterprise-grade pre-aggregated metrics infrastructure for analytics, reporting, and compliance at scale.

## 1. Problem Statement

Nexus Gateway's analytics pages (Dashboard, Analytics, Compliance, Discovery, Fleet, Metrics Explorer) currently execute real-time `GROUP BY` / `SUM` / `COUNT` queries directly against the `traffic_event` table. The Dashboard alone issues 11+ API calls, each scanning raw data.

At enterprise scale (millions of events/day, hundreds of concurrent users), this creates:
- **Query latency spikes** as `traffic_event` grows
- **Database contention** between analytics reads and traffic writes
- **Client-side computation hacks** (e.g., P95 calculated from 1000 raw rows in the browser)
- **No long-term retention** for trend analysis beyond the `traffic_event` retention window

## 2. Design Goals

| Goal | Description |
|------|-------------|
| **Query performance** | Analytics pages respond in <100ms regardless of traffic volume |
| **Data freshness** | Dashboard-grade metrics available within 1-5 minutes of traffic events |
| **Extensibility** | New metrics and dimensions can be added without schema changes |
| **Long-term retention** | 3-5 year historical data for trend analysis and compliance |
| **Accuracy** | Daily correction job ensures eventual exactness |
| **Operational simplicity** | Scheduled jobs in Control Plane; no external dependencies (no Kafka, no ClickHouse) |

## 3. Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                        Data Sources                             │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────────────────┐  │
│  │ AI Gateway   │  │ Compliance   │  │ Desktop Agent         │  │
│  │ (source=vk)  │  │ Proxy        │  │ (source=agent)        │  │
│  │              │  │ (source=proxy)│  │                       │  │
│  └──────┬───────┘  └──────┬───────┘  └───────────┬───────────┘  │
│         └──────────────────┼──────────────────────┘              │
│                            ▼                                     │
│                    ┌───────────────┐                             │
│                    │ traffic_event │  (unified raw event table)  │
│                    └───────┬───────┘                             │
└────────────────────────────┼────────────────────────────────────┘
                             │
┌────────────────────────────┼────────────────────────────────────┐
│              Rollup Computation Pipeline                        │
│                            │                                     │
│              ┌─────────────▼──────────────┐                     │
│              │   rollup-5m job (every 60s) │                    │
│              │   watermark + full-bucket   │                    │
│              │   exactly-once semantics    │                    │
│              └─────────────┬──────────────┘                     │
│                            ▼                                     │
│                  ┌───────────────────┐                           │
│                  │ metric_rollup_5m  │  (retain 7 days)         │
│                  └────────┬──────────┘                           │
│                           │ rollup-1h job (every 5min)          │
│                           ▼                                      │
│                  ┌───────────────────┐                           │
│                  │ metric_rollup_1h  │  (retain 90 days)        │
│                  └────────┬──────────┘                           │
│                           │ rollup-1d job (every 1h)            │
│                           ▼                                      │
│                  ┌───────────────────┐                           │
│                  │ metric_rollup_1d  │  (retain 365 days)       │
│                  └────────┬──────────┘                           │
│                           │ rollup-1mo job (every 24h)          │
│                           ▼                                      │
│                  ┌───────────────────┐                           │
│                  │ metric_rollup_1mo │  (retain 5 years)        │
│                  └───────────────────┘                           │
│                                                                  │
│              ┌────────────────────────────┐                      │
│              │ rollup-correction (daily)  │                     │
│              │ Full recompute T-1 across  │                     │
│              │ all granularity layers     │                     │
│              └────────────────────────────┘                      │
└──────────────────────────────────────────────────────────────────┘
                             │
┌────────────────────────────┼────────────────────────────────────┐
│                    Query Layer                                   │
│                            ▼                                     │
│              ┌─────────────────────────┐                        │
│              │  Metrics Query Service  │                        │
│              │  - Auto granularity     │                        │
│              │  - Unified interface    │                        │
│              │  - Fallback to raw      │                        │
│              └─────────────┬───────────┘                        │
│                            │                                     │
│         ┌──────────────────┼──────────────────┐                 │
│         ▼                  ▼                  ▼                  │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────┐           │
│  │  Dashboard   │  │  Analytics   │  │  Compliance  │  ...     │
│  │  /dashboard  │  │  /analytics  │  │  /proxy/*    │           │
│  └─────────────┘  └──────────────┘  └──────────────┘           │
└──────────────────────────────────────────────────────────────────┘
```

## 4. Data Model

### 4.1 Table Schema (identical for all 4 tables)

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

Tables: `metric_rollup_5m`, `metric_rollup_1h`, `metric_rollup_1d`, `metric_rollup_1mo` — same DDL, different retention.

### 4.2 EAV Dimension Model

| Field | Purpose | Examples |
|-------|---------|---------|
| `metric_name` | Metric identifier | `request_count`, `estimated_cost_usd`, `latency_histogram` |
| `dimension_key` | Primary grouping dimension | `""` (global), `provider=openai`, `user=u123`, `target_host=api.openai.com` |
| `sub_dimension` | Low-cardinality filter dimension | `""` (unfiltered), `source=vk`, `source=vk;data_classification=CONFIDENTIAL` |
| `value` | Numeric aggregate | COUNT/SUM values; 0 for histogram metrics (data in metadata) |
| `metadata` | Extension data | Histogram buckets, MIN/MAX timestamps, HLL sketches |

### 4.3 Supported Dimensions

**Primary dimensions** (dimension_key, one row per value):
`provider`, `model`, `project`, `organization`, `department`, `user`, `virtual_key`, `routed_provider`, `routing_rule`, `target_host`, `device`

**Secondary dimensions** (sub_dimension, pre-computed low-cardinality):
- `source` — vk / proxy / agent (3 values)
- `data_classification` — PUBLIC / INTERNAL / CONFIDENTIAL / RESTRICTED / UNKNOWN (5 values)

### 4.4 Retention Policy

| Table | Granularity | Retention | Cleanup Job |
|-------|-------------|-----------|-------------|
| `metric_rollup_5m` | 5 minutes | 7 days | `rollup-retention` (daily) |
| `metric_rollup_1h` | 1 hour | 90 days | `rollup-retention` (daily) |
| `metric_rollup_1d` | 1 day | 365 days | `rollup-retention` (daily) |
| `metric_rollup_1mo` | 1 month | 5 years | `rollup-retention` (daily) |

## 5. Metrics Catalog

### 5.1 Tier 1 — AI Traffic Core

| Metric | Aggregation | Source Column |
|--------|-------------|---------------|
| `request_count` | COUNT(*) | — |
| `status_2xx_count` | COUNT(status_code < 400) | `status_code` |
| `status_4xx_count` | COUNT(400 ≤ status_code < 500) | `status_code` |
| `status_5xx_count` | COUNT(status_code ≥ 500) | `status_code` |
| `timeout_count` | COUNT(specific condition) | `status_code` / `details` |
| `cache_hit_count` | COUNT(cache_hit = true) | `cache_hit` |
| `prompt_tokens` | SUM | `prompt_tokens` |
| `completion_tokens` | SUM | `completion_tokens` |
| `total_tokens` | SUM | `total_tokens` |
| `estimated_cost_usd` | SUM | `estimated_cost_usd` |
| `billed_cost_usd` | SUM (success non-cache-hit + optional internal-ops) | `estimated_cost_usd` + `embedding_cost_usd` + `ai_guard_cost_usd` (toggle) |
| `embedding_cost_usd` | SUM (semantic-cache lookups) | `embedding_cost_usd` |
| `ai_guard_cost_usd` | SUM (internal ai-guard classifier calls) | `ai_guard_cost_usd` |
| `cache_saved_cost_usd` | SUM (estimated cost when cache_hit) | `estimated_cost_usd`, `cache_hit` |
| `wasted_cost_usd` | SUM (cost where status ≥ 400) | `estimated_cost_usd`, `status_code` |
| `latency_sum` | SUM | `latency_ms` |
| `latency_count` | COUNT (non-null latency) | `latency_ms` |
| `latency_histogram` | 6-bucket count in metadata | `latency_ms` |
| `ttft_sum` | SUM | `details->ttft_ms` (if available) |
| `ttft_count` | COUNT (non-null ttft) | `details->ttft_ms` |
| `routing_fallback_count` | COUNT(routed_provider ≠ provider) | `provider_name`, `routed_provider_name` |
| `routing_rule_hit_count` | COUNT(routing_rule_id IS NOT NULL) | `routing_rule_id` |
| `model_shift_count` | COUNT(model ≠ routed_model) | `model_name`, `routed_model_name` |

**`billed_cost_usd` semantics + the `excludeInternalOpsFromBilledCost` toggle.** `billed_cost_usd` is the **operator-billable** cost: upstream provider charges PLUS Nexus internal-ops costs (semantic-cache embedding + internal ai-guard classifier). Failed requests (status ≥ 400) and gateway cache HITs are excluded from billed_cost. The `excludeInternalOpsFromBilledCost` flag in `nexus-hub.yaml` (default `false` = "都是钱", count them) controls whether internal-ops costs are folded into the `billed_cost_usd` metric. The rollup-5m job applies this toggle when aggregating; see `packages/nexus-hub/internal/jobs/defs/rollup/rollup_5m.go` and `cost-estimation-architecture.md` § 6.6 for the producer-side contract.

### 5.2 Tier 2 — Compliance & Proxy

| Metric | Aggregation | Source Column |
|--------|-------------|---------------|
| `hook_allow_count` | COUNT(hook_decision = 'allow') | `hook_decision` |
| `hook_deny_count` | COUNT(hook_decision = 'deny') | `hook_decision` |
| `hook_error_count` | COUNT(hook_decision = 'error') | `hook_decision` |
| `hook_unknown_count` | COUNT(hook_decision = 'unknown') | `hook_decision` |
| `hook_latency_sum` | SUM | `details->hookLatencyMs` |
| `hook_latency_count` | COUNT | `details->hookLatencyMs` |
| `hook_latency_histogram` | 6-bucket count in metadata | `details->hookLatencyMs` |
| `bump_success_count` | COUNT(bump_status = 'BUMP_SUCCESS') | `bump_status` |
| `bump_failed_count` | COUNT(bump_status = 'BUMP_FAILED_PASSTHROUGH') | `bump_status` |
| `bump_exempt_count` | COUNT(bump_status IN (...exempt...)) | `bump_status` |
| `bump_disabled_count` | COUNT(bump_status = 'BUMP_DISABLED_EMERGENCY') | `bump_status` |
| `proxy_request_count` | COUNT(source = 'proxy') | `source` |
| `classification_count` | COUNT, dimension_key per level | `data_classification` |
| `reject_count` | COUNT(hook_decision = 'reject') | `hook_decision` |

### 5.3 Tier 2 — Usage & Growth

| Metric | Aggregation | Notes |
|--------|-------------|-------|
| `active_users` | COUNT(DISTINCT user_id) | Computed independently per granularity layer |
| `active_virtual_keys` | COUNT(DISTINCT virtual_key_id) | Same |
| `active_devices` | COUNT(DISTINCT device_id) | Same |
| `active_organizations` | COUNT(DISTINCT organization_id) | Same |

### 5.4 Tier 2 — Discovery-Specific

| Metric | Aggregation | Notes |
|--------|-------------|-------|
| `distinct_sources` | COUNT(DISTINCT source_ip) | Per target_host dimension, independent per layer |
| `first_seen` | MIN(timestamp) | Stored in metadata as ISO string |
| `last_seen` | MAX(timestamp) | Stored in metadata as ISO string |

### 5.5 Tier 3 — Fleet (migrate from existing MetricRollup)

| Metric | Notes |
|--------|-------|
| `device_fleet_status` | Existing logic migrated to metric_rollup_1h |
| `device_fleet_os` | Same |
| `agent_action_volume` | Same |

### 5.6 Histogram Bucket Definition

```
Latency:       [0,50ms) [50,100ms) [100,200ms) [200,500ms) [500,1000ms) [1000ms,+∞)
Hook Latency:  [0,50ms) [50,100ms) [100,200ms) [200,500ms) [500,1000ms) [1000ms,+∞)

Stored as metadata: {"buckets": [count0, count1, count2, count3, count4, count5]}
```

Percentile approximation: interpolate within the bucket containing the target percentile.

## 6. Computation Pipeline

### 6.1 Job Schedule

| Job (`ID()`) | Interval | Input | Output |
|---|---|---|---|
| `rollup-5m` | 60s | `traffic_event` (incremental) | `metric_rollup_5m` |
| `rollup-correction` | 24h | `traffic_event` (T-1 full recompute); internally drives the merge jobs for the corrected window | All tables for T-1 |
| `rollup-retention` | 24h | — | DELETE expired rows per table |

Upper-layer merges (5m → 1h → 1d → 1mo) are NOT registered as independent scheduled jobs with their own IDs. The codebase has **one** `RollupMergeJob` type (`packages/nexus-hub/internal/jobs/defs/rollup/rollup_merge.go`) and three constructors that produce 1h / 1d / 1mo variants (`NewRollupMerge1h` / `NewRollupMerge1d` / `NewRollupMerge1mo`) — these are owned by the correction job (`packages/nexus-hub/internal/jobs/defs/rollup/rollup_correction.go::merge1h / merge1d / merge1mo`) and run as part of the correction sequence. The `ops-rollup-1h` / `ops-rollup-1d` / `ops-rollup-1mo` job IDs referenced elsewhere belong to the **ops metrics** pipeline, not this main-traffic rollup.

### 6.2 rollup-5m Algorithm (exactly-once)

```
Every 60 seconds:
1. Read watermark (last completed 5m bucket boundary)
2. Identify sealed buckets: watermark < bucket_start <= NOW() - 5min
3. For each sealed bucket, in a single transaction:
   a. DELETE FROM metric_rollup_5m WHERE bucket_start = this_bucket
   b. INSERT INTO metric_rollup_5m
      SELECT <aggregations> FROM traffic_event
      WHERE timestamp >= bucket_start AND timestamp < bucket_start + 5min
      GROUP BY (metric_name, dimension_key, sub_dimension)
   c. UPDATE rollup_watermark SET watermark = this_bucket
   COMMIT
4. Unseal current bucket is skipped (processed next round)
```

### 6.3 Upper-Layer Merge Algorithm

```
rollup-1h (every 5 minutes):
1. Read merge watermark for 1h layer
2. Identify sealed hours: all 5m buckets within the hour exist
3. For each sealed hour, in a single transaction:
   a. DELETE FROM metric_rollup_1h WHERE bucket_start = this_hour
   b. INSERT INTO metric_rollup_1h
      SELECT date_trunc('hour', bucket_start), metric_name, dimension_key, sub_dimension,
             SUM(value), merge_metadata(metadata)
      FROM metric_rollup_5m
      WHERE bucket_start >= this_hour AND bucket_start < this_hour + 1h
      GROUP BY 1,2,3,4
   c. UPDATE merge watermark
   COMMIT
```

Same pattern for `rollup-1d` (from 1h) and `rollup-1mo` (from 1d).

### 6.4 Dimension Expansion

Each `traffic_event` row expands into multiple rollup rows:

```
Per event, for each metric:
  × 1 global dimension_key ("")
  × N non-null dimension values (provider=X, model=Y, user=Z, ...)
  × applicable sub_dimension combinations (source=vk, source=vk;data_classification=CONFIDENTIAL)

Within a 5-minute bucket, same (metric, dimension_key, sub_dimension) tuples merge via SUM/COUNT.
```

### 6.5 Special Metric Merge Rules

| Type | 5m Computation | Upper-Layer Merge |
|------|----------------|-------------------|
| COUNT/SUM | Standard aggregation | SUM(lower.value) |
| AVG (latency) | Store as sum + count (two metric rows) | SUM(sum), SUM(count); query divides |
| Histogram | Bucket counts in metadata JSON | Element-wise addition of bucket arrays |
| MIN/MAX (first/last_seen) | MIN/MAX(timestamp) in metadata | MIN of MINs, MAX of MAXs |
| DISTINCT (active_users) | COUNT(DISTINCT) per granularity | Independent computation; not merged from lower layer |

### 6.6 Correction Job

```
Once per 24h interval:
1. Delete metric_rollup_5m rows for T-1 (yesterday)
2. Full recompute from traffic_event for T-1
3. Trigger 1h merge for T-1 hours
4. Trigger 1d merge for T-1
5. If T-1 is last day of month, trigger 1mo merge
```

### 6.7 Fault Tolerance

| Failure | Recovery |
|---------|----------|
| Job crash mid-transaction | Transaction rolls back; watermark unchanged; next run retries |
| DB connection lost | Job skips this tick; next tick retries from watermark |
| Duplicate execution | DELETE + INSERT in transaction = idempotent |
| Late-arriving events | Correction job recomputes T-1; events older than T-1 require manual trigger |

## 7. Query Layer

### 7.1 Auto Granularity Selection

| Query Time Span | Table | Bucket Granularity |
|-----------------|-------|--------------------|
| ≤ 6 hours | `metric_rollup_5m` | 5 min |
| 6h – 7 days | `metric_rollup_1h` | 1 hour |
| 7d – 90 days | `metric_rollup_1d` | 1 day |
| > 90 days | `metric_rollup_1mo` | 1 month |

### 7.2 Unified Query Interface

```go
type MetricsQuery struct {
    Metrics      []string   // e.g. ["request_count", "estimated_cost_usd"]
    DimensionKey string     // "provider" for grouping; "" for global
    SubDimension string     // "source=vk" for filtering; "" for none
    StartTime    time.Time
    EndTime      time.Time
    TopN         int        // >0 for Top-N ranking queries
    TimeSeries   bool       // true for time-bucketed series
}

type MetricsResult struct {
    Granularity string
    Summary     map[string]float64
    Series      []MetricsBucket
    Groups      []MetricsGroup
    Metadata    map[string]interface{}
}
```

### 7.3 Degradation Strategy

When rollup data is unavailable (cold start, job failure):
1. Query rollup table
2. If no data → fall back to direct `traffic_event` query (existing logic)
3. Set response header `X-Metrics-Source: raw` to notify frontend
4. Frontend may show informational badge: "Data is being pre-computed"

## 8. Migration Strategy

### Phase 1: Schema + Jobs (no API changes)
- Create 4 new rollup tables
- Deploy rollup jobs alongside existing jobs
- Jobs begin populating data; existing APIs unchanged

### Phase 2: Query Layer (internal switch)
- Implement Metrics Query Service
- Existing API handlers call MetricsQuery internally
- Response format unchanged; frontend unaware
- `X-Metrics-Source` header indicates data source

### Phase 3: Migrate Legacy MetricRollup
- Move fleet/device metrics from old `MetricRollup` table to `metric_rollup_1h`
- Update fleet analytics endpoints
- Drop old `MetricRollup` table

### Phase 4: New Metrics + API Optimization
- Add business metrics (active_users, cache_saved_cost_usd, etc.)
- Add new API endpoints for metrics not previously available
- Frontend progressively adopts new capabilities

## 9. Operational Considerations

### Monitoring
- **Rollup lag**: time between latest `traffic_event` and latest `metric_rollup_5m` bucket
- **Job duration**: each rollup job should complete well within its interval
- **Row counts**: per-table row counts for capacity planning
- **Correction delta**: difference between incremental and corrected values (data quality signal)

### Scaling Path
- **PostgreSQL partitioning**: `metric_rollup_5m` can be range-partitioned by `bucket_start` if needed
- **Read replicas**: analytics queries can target a read replica
- **Parallel computation**: rollup-5m can shard by dimension_key prefix if needed

### Storage Estimates
Assuming 10K events/minute, 12 dimensions, 15 metrics, 2 sub_dimensions:
- `metric_rollup_5m`: ~360 unique (metric, dim, sub_dim) combinations × 288 buckets/day × 7 days ≈ 725K rows
- `metric_rollup_1h`: ~360 × 24 × 90 ≈ 778K rows
- `metric_rollup_1d`: ~360 × 365 ≈ 131K rows
- `metric_rollup_1mo`: ~360 × 60 months ≈ 22K rows
- **Total: ~1.7M rows** — trivial for PostgreSQL
