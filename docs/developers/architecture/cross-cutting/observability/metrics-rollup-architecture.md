# Metrics rollup architecture

The Hub pre-aggregates two streams of raw observations — AI traffic events and per-Thing operational samples — into time-bucketed rollup tables that the admin analytics surfaces and the alert engine read. Pre-aggregation is what lets a "last 90 days" dashboard query scan a few thousand hourly buckets instead of millions of raw rows.

This document covers the rollup pipelines: how raw data becomes bucketed rows, how the jobs stay idempotent and crash-safe, and how the read side selects a granularity. The real-time Prometheus surface that shares the same instruments is described in [prometheus-naming-architecture.md](prometheus-naming-architecture.md); the high-level metrics overview is [observability-architecture.md](observability-architecture.md) §5; the scheduler that runs these jobs and their cadences is the job catalogue in [jobs-architecture.md](../foundation/jobs-architecture.md) §5.1. Purging of aged rollup rows is owned by [data-retention-purge-architecture.md](../storage/data-retention-purge-architecture.md).

## 1. Four pipelines

| Pipeline | Source | Buckets | Producer jobs |
| --- | --- | --- | --- |
| Fleet-wide traffic | `traffic_event` | `metric_rollup_5m` → `_1h` → `_1d` → `_1mo` | `rollup-5m`, `merge-1h/1d/1mo`, `rollup-correction` |
| Per-Thing traffic | `traffic_event` rows with non-null `thing_id` | `thing_metric_rollup_5m` → `_1h` → `_1d` → `_1mo` | `thing-rollup-5m`, `thing-merge-1h/1d/1mo` |
| Device fleet | `thing` + `traffic_event` | `metric_rollup_1h` (three metric names) | `metrics-rollup` |
| Ops samples | `metric_ops_raw` (from `metrics_sample`) | `metric_ops_rollup_1h` → `_1d` → `_1mo` | `ops-rollup-1h`, `ops-rollup-1d/1mo` |

The first two pipelines re-aggregate `traffic_event` rows the Hub's MQ consumer has already written. The fourth pipeline owns its own write side — Things push samples over WebSocket and the Hub batches them into `metric_ops_raw` before rolling up. The device-fleet pipeline is a single hourly snapshot, not a cascade.

All producer jobs live under `packages/nexus-hub/internal/jobs/defs/rollup/` (traffic + per-Thing + device) and `packages/nexus-hub/internal/jobs/defs/metrics/` (ops), and are registered in `packages/nexus-hub/cmd/nexus-hub/wiring/jobs.go`.

## 2. Storage model

The rollup tables come in two column shapes (`tools/db-migrate/schema.prisma`).

**Scalar-value tables** — `metric_rollup_5m/1h/1d/1mo` and `thing_metric_rollup_5m/1h/1d/1mo`. Each row is one `(bucketStart, metricName, dimensionKey, subDimension)` tuple (the per-Thing tables add a `thing_id` column to the key) carrying a single `value` plus an optional JSON `metadata` blob. Histograms and timestamp metadata ride inside `metadata`; everything else is a single number. The natural key is a `@@unique`, which makes the producer's upsert safe (§4).

**Statistical tables** — `metric_ops_raw` and `metric_ops_rollup_1h/1d/1mo`. A raw row is one observation `(sampled_at, thing_id, thing_type, metric_name, metric_kind, dimension_key, value, metadata)`. A rollup row carries the full summary — `value_avg`, `value_sum`, `value_min`, `value_max`, and `sample_count` — so a coarser layer can re-aggregate a finer one without returning to raw. The rollup tables keep a synthetic `id` primary key and a nullable `thing_id`: a non-null `thing_id` is a per-instance row, a null `thing_id` is a fleet aggregate. Because Prisma cannot express the natural key (it needs `COALESCE(thing_id, sentinel)` with NOT-NULL semantics), the real uniqueness constraint is a partial-`COALESCE` unique index declared in raw SQL alongside the schema.

**Watermark table** — `rollup_watermark` is a single `(jobName, watermark, updatedAt)` row per producer job recording the last bucket it committed. This is the backbone of the idempotency contract.

On the Hub side, the scalar tables, the watermark table, and their query helpers are owned by the `rollupstore` package (`packages/nexus-hub/internal/quota/rollup/`), which the producer jobs import. The Control Plane reads the same tables through a parallel store — `packages/control-plane/internal/settings/store/metricsstore/` for fleet rollups and `packages/control-plane/internal/observability/thingstats/thingstore/` for per-Thing rollups — that hand-mirrors the same SQL conventions rather than importing the Hub package. The two trees are kept in step by convention, not by a shared import.

## 3. The rollup row contract

Producers and readers exchange `metrics.RollupRow` / `metrics.ThingRollupRow` values (`packages/shared/core/metrics/instruments/`). A scalar-table row names a metric (`metricName`, e.g. `billed_cost_usd`, `latency_sum`), a dimension (`dimensionKey`, encoded `name=value`), and a sub-dimension (`subDimension`). Three conventions matter:

- **Dimension values are stable identifiers, never display names.** The aggregator emits the Virtual Key id, Model UUID, org id, routing-rule id, and so on — not the human label. Analytics queries survive renames because the read-side handler joins labels at response time. `BuildDimensionKey` / `BuildSubDimension` produce the canonical key strings.
- **The model dimension is fed from the routed model, not the requested model.** A chat-completions request carries a free-text model string with no Model UUID; reading the routed column keeps the `model=` dimension populated with the UUID of the Model row that actually served the call.
- **The sub-dimension is the product-domain source** — `vk`, `proxy`, or `agent` — derived via `domain.DBSourceToDomain`. Rows whose DB source falls outside the data-plane mapping (admin lifecycle events, device events) are skipped so they never pollute data-plane rollups.

## 4. Idempotency and crash safety

Every producer follows the same transaction shape, so a replica that dies mid-bucket and restarts produces exactly the same final state:

1. `Begin` a transaction.
2. Clear the target bucket — `DELETE` for that `bucketStart` (ops and merge jobs) or rely on `ON CONFLICT … DO UPDATE` upsert (scalar inserts via `rollupstore.InsertRollupRows`).
3. Write the recomputed rows.
4. `SetWatermark` for this job to the bucket just written — in the **same** transaction, so the rows and the watermark advance atomically.
5. `Commit`.

The scalar tables can upsert on their natural key. The ops tables carry a synthetic UUID key, so their producers delete the bucket's rows first and then re-insert. Either way, re-running a bucket is a no-op-equivalent: the second run overwrites the first with identical output.

There are no advisory locks. A single scheduler leader runs the jobs (see [jobs-architecture.md](../foundation/jobs-architecture.md) §2), so the watermark is the only coordination primitive needed.

### Catch-up and the sealed-bucket rule

Each run reads its watermark and walks forward one bucket at a time up to the most recent **sealed** bucket — a bucket whose window lies entirely in the past. `latestSealed` is computed as `now - bucketDuration`, truncated to the bucket boundary, so a bucket that is still accumulating data is never rolled up early.

Some layers add extra grace beyond the bare boundary:

- `ops-rollup-1h` waits an additional five minutes past the hour so straggler raw samples (clock skew, batch-flush latency) land before the bucket is sealed.
- `ops-rollup-1d` waits one full source bucket (one hour) so late hourly aggregations across midnight are included.
- The monthly layers always exclude the current, unsealed calendar month.

### Cold start and bootstrap

When `rollup_watermark` has no row for a job (fresh deployment, or a job seeded with a future timestamp), the producer picks a starting point that does not silently drop history. The fixed-duration jobs take the **earlier** of `(now − initLookback)` and `(earliest source row − one bucket)`, both truncated — so if the source already holds data older than the default lookback (a seed with historical timestamps, or retained data after a restart), the job backfills from the real start of data instead of skipping to the recent window. The ops jobs apply the same idea through `resolveCursor`, rewinding to the bucket containing the oldest raw sample when it predates the watermark; the ops jobs also run on start rather than waiting a full interval.

## 5. Fleet-wide traffic pipeline

`rollup-5m` (`rollup_5m.go`) is the canonical producer. For each sealed five-minute bucket it scans `traffic_event` over `[bucketStart, bucketStart+5m)` and expands every event into rollup rows: one row per `(metric, dimension, sub-dimension)` the event contributes to. A single event feeds many dimensions (provider, model, entity, org, routing rule, target host, Virtual Key, project, hook decision) and several metric families at once — counts, summed cost and tokens, latency sums and histograms, and distinct-entity / distinct-org / distinct-source sets. Cache-hit classification reads `gateway_cache_status` so gateway-cache hits are counted without conflating them with provider prompt-cache discounts (which are tracked on their own metric series). The hook-decision dimension collapses the request-stage and response-stage decisions to a single effective decision by worst-wins precedence (`REJECT_HARD` > `BLOCK_SOFT` > `ERROR` > `APPROVE`).

The internal-ops cost knob `excludeInternalOpsFromBilled` controls whether L2-embedding and ai-guard classifier costs fold into `billed_cost_usd`. It defaults to off — internal-ops costs are real money and count toward the quota-bearing billed total unless an operator opts out, in which case those costs stay on their dedicated `embedding_cost_usd` / `ai_guard_cost_usd` series only.

The **merge cascade** (`rollup_merge.go`) rolls the result coarser: `merge-1h` folds `metric_rollup_5m` into `metric_rollup_1h`, `merge-1d` folds `1h` into `1d`, and `merge-1mo` folds `1d` into `1mo` on calendar-month boundaries. One `RollupMergeJob` type drives all three; the layer's source table, target table, and bucket length come from a per-layer config. Each merge reads its source bucket via `rollupstore.QueryRollupMergeSource`, combines rows sharing `(metricName, dimensionKey, subDimension)` through `metrics.MergeRollupRows`, and writes the target bucket under the same delete-then-insert-and-watermark transaction.

`rollup-correction` (`rollup_correction.go`) handles late-arriving events. Events can land in `traffic_event` after their bucket has already been rolled up; once per day the correction job recomputes every five-minute bucket of the previous day, then re-merges that day's `1h` and `1d` layers (and the `1mo` layer only when the previous day was the last of its calendar month — i.e. the job runs on the 1st). It delegates to the same `rollup-5m` and merge jobs so the aggregation logic lives in exactly one place, and it deliberately does not touch the live watermark path. The "now" it derives this date arithmetic from is an injectable seam, so the month-boundary branch is covered deterministically in tests rather than only on the 1st of a month.

## 6. Per-Thing traffic pipeline

The per-Thing pipeline mirrors the fleet pipeline but keys every row by `thing_id`, so a single Thing's dashboard reads its own small table instead of filtering the fleet-wide one. `thing-rollup-5m` (`thing_rollup_5m.go`) aggregates `traffic_event` rows where `thing_id IS NOT NULL`, and `thing-merge-1h/1d/1mo` (`thing_rollup_merge.go`) cascade the result. The watermarks are independent of the fleet pipeline, so per-Thing recovery is isolated.

Only data-plane Things produce per-Thing rows (the rows with a `thing_id`). Agent-sourced rows are gated by `enableAgentRollup`, which defaults off: at fleet scale agents compute their own rollups locally, so the Hub appends `AND source != 'agent'` to the per-Thing query unless the toggle is on. Rows whose metrics evaluate to zero for a Thing are skipped at emit time, so empty rows never reach the table.

## 7. Device-fleet pipeline

`metrics-rollup` (`metrics_rollup.go`) is a once-an-hour snapshot rather than a cascade. It writes three metric names into `metric_rollup_1h`: agent fleet status counts (`thing` rows of type `agent` grouped by status), fleet OS distribution (grouped by OS, with anything outside the major platforms folded to `other`), and agent action volume over the trailing hour (from `traffic_event` where `source = 'agent'`). Each run deletes its three metric names for the current bucket and re-inserts, so the hourly snapshot is idempotent. Source queries tolerate per-query failure: a transient error on one query does not starve the other two.

## 8. Ops-metrics pipeline

### Write side

Every Thing — services and agents — periodically pushes a `metrics_sample` WebSocket payload carrying a `SampleBatch{ThingID, SampledAt, Samples[]}` (`packages/shared/core/metrics/registry/types.go`). A `Sample` names a metric, a `MetricKind` (`gauge`, `counter`, or `histogram`), a dimension key, a value, and optional metadata. For histogram samples the value is zero and the bucket counts live in `metadata.buckets` as a six-element array.

The Hub ingests these through the `Writer` (`packages/nexus-hub/internal/observability/opsmetrics/writer.go`) — one bounded-channel batcher per Hub instance, shared across all WebSocket connections. `Enqueue` is non-blocking: when the queue is full the payload is dropped and a drop counter is incremented, so the WebSocket read pump never blocks waiting on the database. The background loop accumulates rows up to a batch size or a latency deadline (a thousand samples or two hundred milliseconds, whichever comes first) and issues one `pgx.CopyFrom` per batch into `metric_ops_raw`. A sample whose metadata cannot be marshalled is dropped individually with its own drop reason rather than failing the batch.

The fleet traffic pipelines do **not** use this write path — they read `traffic_event` rows that the MQ traffic-event consumer already persisted.

### Rollup and the partitioning rule

`ops-rollup-1h` (`ops_rollup_1h.go`) aggregates each sealed hour of `metric_ops_raw` into `metric_ops_rollup_1h`. How a row is partitioned depends on the Thing type:

- **Server-side Things** (`thing_type` other than `agent` — `ai-gateway`, `compliance-proxy`, `nexus-hub`, `control-plane`) always keep a per-instance row, so a Stats tab can attribute a metric to a specific service node.
- **Agents in diagnostic mode** for the bucket keep a per-instance row. The job reads `thing_diag_mode_window` for windows overlapping the bucket to find them.
- **Plain agents** fold into a single fleet-aggregate row per `(metric, dimension)` with `thing_id` null, so a fleet of thousands of agents does not explode the rollup row count.

Scalar metrics aggregate in SQL (`AVG`, `SUM`, `MIN`, `MAX`, `COUNT`). Histograms cannot be combined by SQL aggregates over an element-wise bucket array, so the job reads each histogram raw row, folds the six-element buckets together in Go (`MergeHistogramBuckets`), and writes one rollup row whose `value_sum` is the total count across buckets.

### Cascade and weighted averaging

`ops-rollup-1d` and `ops-rollup-1mo` (`ops_rollup_cascade.go`) roll the hourly layer up to daily and then to calendar-monthly buckets. The source layer has already split per-instance versus fleet rows, so the cascade simply groups by the identity columns (null-safe on `thing_id`). Averages combine by sample-count weighting — `SUM(value_avg × sample_count) / SUM(sample_count)` — computed in SQL to keep the merge atomic and avoid loading thousands of agent rows into memory. Histograms merge element-wise in Go, exactly as in the hourly job.

Two six-element histogram representations exist — `instruments.Histogram` on the traffic side and `opsmetrics.HistogramBuckets` on the ops side (`packages/nexus-hub/internal/observability/opsmetrics/histogram_merge.go`). Both follow the canonical layout of five finite millisecond bounds plus an implicit `+Inf` bucket, and both merge by summing corresponding buckets.

## 9. Read path

The Hub-side helpers `rollupstore.QueryRollup` (fleet) and `rollupstore.QueryThingRollup` (per-Thing, where a `ThingID` is mandatory to prevent unbounded scans) read a single granularity table. The Control Plane analytics surfaces use the richer readers in `metricsstore` — `QueryRollupCascade` for totals and group-by queries and `QueryRollupAware` for fixed-granularity time series — plus `thingstore.QueryThingRollup` for per-Thing reads. All of them call `metrics.SelectGranularity`, which maps the requested time span to the coarsest table that still gives useful resolution:

| Span | Granularity | Table |
| --- | --- | --- |
| ≤ 6 hours | 5m | `metric_rollup_5m` |
| ≤ 90 days | 1h | `metric_rollup_1h` |
| ≤ 1 year | 1d | `metric_rollup_1d` |
| > 1 year | 1mo | `metric_rollup_1mo` |

Because a coarse table only holds buckets the merge cascade has already sealed, a "right now" query against, say, `metric_rollup_1h` would miss the request a user just sent until the next merge tick. The `metricsstore` readers close that gap by stitching at the merge watermark: `QueryRollupAware` reads sealed buckets from the coarse table and the trailing unsealed window from the finer `metric_rollup_5m`, and `QueryRollupCascade` unions all four layers between consecutive merge watermarks so an aggregate query is always complete up to the latest sealed five-minute bucket.

The Control Plane analytics and quota handlers (`packages/control-plane/internal/traffic/analytics/handler/`, `packages/control-plane/internal/settings/store/metricsstore/`, `packages/control-plane/internal/ai/quota/handler/`) are the consumers; they resolve dimension-value identifiers back to display labels at response time.

## 10. Operational invariants

- A bucket is rolled up only after it is sealed; the watermark and the rolled rows advance in one transaction, so the pipeline is exactly-once per bucket and safe across replica restarts.
- Re-running any producer for any bucket is idempotent — scalar tables upsert on the natural key, ops tables delete-then-insert on the synthetic key.
- Dimension values stored in rollup rows are stable identifiers; display labels are never persisted in rollups.
- Plain agents never produce per-instance ops rows or (by default) per-Thing traffic rows; they collapse into fleet aggregates so the row count stays bounded as the fleet grows.
- Late-arriving traffic events are absorbed by the daily correction job, not by widening the live sealed-bucket grace.
- Retention of aged rollup rows is a separate concern handled by the retention jobs; see [data-retention-purge-architecture.md](../storage/data-retention-purge-architecture.md).

## References

- `packages/nexus-hub/internal/jobs/defs/rollup/` — fleet, per-Thing, device, and health rollup producers
- `packages/nexus-hub/internal/jobs/defs/metrics/` — ops-metrics rollup producers
- `packages/nexus-hub/internal/quota/rollup/` — `rollupstore` watermark, insert, purge, and query helpers
- `packages/nexus-hub/internal/observability/opsmetrics/` — ops-sample writer and histogram merge
- `packages/nexus-hub/cmd/nexus-hub/wiring/jobs.go` — scheduler registration of all rollup jobs
- `packages/shared/core/metrics/instruments/` — `RollupRow`, `Histogram`, dimension-key builders, granularity selection
- `packages/shared/core/metrics/registry/` — `SampleBatch` / `Sample` WebSocket payload types
- `packages/control-plane/internal/settings/store/metricsstore/` — Control Plane fleet rollup reader (`QueryRollupCascade`, `QueryRollupAware`)
- `packages/control-plane/internal/observability/thingstats/thingstore/` — Control Plane per-Thing rollup reader
- `packages/control-plane/internal/traffic/analytics/handler/` — read-side analytics consumers
- `tools/db-migrate/schema.prisma` — `MetricRollup*`, `ThingMetricRollup*`, `MetricOpsRaw`, `MetricOpsRollup*`, `RollupWatermark` models
