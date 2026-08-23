# Metrics rollup architecture

The Hub pre-aggregates two streams of raw observations — AI traffic events and per-Thing operational samples — into time-bucketed rollup tables that the admin analytics surfaces and the alert engine read. Pre-aggregation is what lets a "last 90 days" dashboard query scan a few thousand hourly buckets instead of millions of raw rows.

This document covers the rollup pipelines: how raw data becomes bucketed rows, how the jobs stay idempotent and crash-safe, and how the read side selects a granularity. The real-time Prometheus surface that shares the same instruments is described in [prometheus-naming-architecture.md](prometheus-naming-architecture.md); the high-level metrics overview is [observability-architecture.md](observability-architecture.md) §5; the scheduler that runs these jobs and their cadences is the job catalogue in [jobs-architecture.md](../foundation/jobs-architecture.md) §5.1. Purging of aged rollup rows is owned by [data-retention-purge-architecture.md](../storage/data-retention-purge-architecture.md).

## 1. Four pipelines

| Pipeline | Source | Buckets | Producer jobs |
| --- | --- | --- | --- |
| Fleet-wide traffic | `traffic_event` | `metric_rollup_5m` → `_1h` → `_1d` → `_1mo` | `rollup-5m`, `merge-1h/1d/1mo`, `rollup-correction` |
| Per-Thing traffic | `traffic_event` rows with non-null `thing_id` | `thing_metric_rollup_5m` → `_1h` → `_1d` → `_1mo` | `thing-rollup-5m`, `thing-merge-1h/1d/1mo`, `thing-rollup-correction` |
| Device fleet | `thing` + `traffic_event` | `metric_rollup_1h` (three metric names) | `metrics-rollup` |
| Ops samples | `metric_ops_raw` (from `metrics_sample`) | `metric_ops_rollup_1h` → `_1d` → `_1mo` | `ops-rollup-1h`, `ops-rollup-1d/1mo` |

The first two pipelines re-aggregate `traffic_event` rows the Hub's MQ consumer has already written. The fourth pipeline owns its own write side — Things push samples over WebSocket and the Hub batches them into `metric_ops_raw` before rolling up. The device-fleet pipeline is a single hourly snapshot, not a cascade.

All producer jobs live under `packages/nexus-hub/internal/jobs/defs/rollup/` (traffic + per-Thing + device) and `packages/nexus-hub/internal/jobs/defs/metrics/` (ops), and are registered in `packages/nexus-hub/cmd/nexus-hub/wiring/jobs.go`.

## 2. Storage model

The rollup tables come in two column shapes (`tools/db-migrate/schema/observability.prisma`).

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

**Cost is a passthrough, never re-priced (single canonical price source).** For success + non-cache rows the rollup emits `billed_cost_usd` equal to the row's `estimated_cost_usd` — the cost the AI Gateway already computed once from the **Model table** (cache-aware) and stamped onto the event. The rollup does **not** consult any second price source or recompute cost from tokens × price; it sums the gateway-stamped value verbatim. This is what keeps enforcement and reporting in lockstep: the live quota counter is incremented by the same `estimated_cost_usd` at reconcile time, and the gateway boot Backfill re-seeds the counter from `metric_rollup_1h.billed_cost_usd` — so the runtime counter and the rolled-up ledger price a given model identically and cannot drift across a reboot (audit F-0163; the retired `provider_pricing` table that once caused this drift is gone). See [quota-architecture.md §2a](../safety/quota-architecture.md#2a-single-canonical-price-source) and [cost-estimation-architecture.md](../../services/ai-gateway/cost-estimation-architecture.md).

Internal-ops costs (L2 embedding, ai-guard classifier, smart-router call) therefore **never** fold into `billed_cost_usd` — that exclusion is structural, not configurable. The `excludeInternalOpsFromBilled` knob is vestigial and is ignored by the aggregator: it lives in the Hub's config while the gateway that charges the live counter is never told it exists, so any value that moved `billed_cost_usd` would put the two sides out of step across a reboot. Those costs stay fully visible on their own series — `embedding_cost_usd`, `ai_guard_cost_usd`, and the vendor-spend series below.

Stamping `RoutedProviderID` on the ai-guard classifier's own `traffic_event` row (needed for the `ai_guard_cost_usd` component of vendor spend below) has a side effect on the general per-row fan-out above: that row previously carried no `routed_provider` dimension at all, so an operator who enables AI-Guard now sees `request_count`, `total_tokens`, latency, status counts, and `billed_total_tokens` step up under the classifier's serving provider in Analytics → By Provider, with no config change explaining it — `billed_cost_usd` itself is unaffected, since the classifier row's `estimated_cost_usd` stays unset.

### Vendor-spend series (`vendor_spend_usd`, `vendor_spend_internal_usd`)

`rollup-5m` also emits the two series a vendor-bill reconciliation reads: every dollar the gateway caused a vendor to charge, attributed to the provider that was **actually charged**. This is the one emission in the aggregator that does not follow the per-row dimension fan-out above (`rollup_5m_vendor_spend.go`), because a single `traffic_event` row can owe money to several vendors at once: a request served by Anthropic whose smart-router ran on OpenAI and whose L2 embedding ran on a third provider owes all three. Vendor spend is therefore emitted **per cost component**, each under the `routed_provider=<provider id>` dimension of the provider that billed for that component — never under a global dimension, since summing the per-provider rows is the reconciliation total and a global row would double it.

| Component | Provider dimension | Included when |
| --- | --- | --- |
| `estimated_cost_usd` (customer traffic) | `routed_provider_id` | tokens were produced and the response was not a gateway-cache hit — status-agnostic |
| `router_cost_usd` (smart-router call) | `router_provider_id` | non-zero |
| `embedding_cost_usd` (L2 lookup) | `embedding_provider_id` | non-zero |
| `ai_guard_cost_usd` (classifier) | `routed_provider_id` — on an ai-guard row the classifier call *is* the request | non-zero |

`vendor_spend_internal_usd` is the internal-ops subset of the same total (the last three rows), so a report can separate estimator error from internal overhead. Three rules carry the correctness of the series:

- **Inclusion is per component, never per row.** The customer component is gated on tokens + non-cache-hit and is deliberately status-agnostic (a client abort or a post-generation block still produced tokens the vendor charges for). The internal components take no gate at all: a non-zero value already means the call happened. A row-level cache gate would be wrong — the embedding cost is stamped on every L1 miss that triggered an embedding call, *including a row that then scored an L2 hit*, which is exactly the case the L2 cache exists to create.
- **A component with no provider id is dropped, not folded.** Rows written before the attribution columns shipped carry internal-ops costs with no provider. A cost no vendor's bill can be matched against is reported as a gap; folding it into the serving provider is the misattribution the series exists to eliminate.
- **Not quota-bearing.** `billed_cost_usd` remains the only series the live quota counter and its boot backfill consume (see above). Both vendor-spend series are plain additive sums, so they ride the merge cascade's `Sum` default from 5m through 1h / 1d / 1mo unchanged.

### Error-class series (`traffic.error_class.*`)

`rollup-5m` additionally emits the series behind the Control Plane's error-governance view (`/api/admin/traffic/errors/groups`): for every failed data-plane row (`status_code >= 400`, internal operational traffic excluded) it accumulates `traffic.error_class.count` plus a `traffic.error_class.seen` timestamp row (registered `AggregationTimestamp`) under a composite dimension `error_class=<errorCode|statusRange|provider|model>` with the source domain as sub-dimension (`rollup_5m_error_class.go`; encode/decode in `instruments/error_class.go`, `%`/`|` escaped per part).

Deliberate deviations from the conventions above:

- **The class key stores stamped names, not catalog UUIDs.** Early rejections (auth failures, routing no-match) carry no resolved Model/Provider UUID at all, and the read side must produce identical class keys to the direct `traffic_event` `GROUP BY COALESCE(routed_*_name, *_name, '')` it falls back to and tail-merges with. A rename therefore starts a new class — which matches the view's semantics (classes are what was stamped on the failing events), at the cost of the rename-immunity UUID dimensions get. Name parts are truncated to 256 characters when keyed (the model part originates from the raw client body with no upstream length bound); classes with absurdly long names diverge from the untruncated direct path by design.
- **Per-bucket class cap, two layers.** The model part of the key is caller-controlled, so a client spraying unique model strings through a rejecting endpoint would otherwise mint unbounded rollup rows. Each bucket emits at most 500 distinct classes (deterministic selection: count descending, key ascending — so bucket re-runs are idempotent); the excess folds into one `__overflow__` class per status range, preserving the total count while shedding class identity. A second, in-memory admission bound (2000 classes) folds NEW classes into overflow at observe time so the accumulator's memory cannot grow with a spray either — beyond it, which classes keep their identity depends on scan order (totals stay exact; determinism is only sacrificed under active attack conditions).
- **Known freshness deviation.** An event written after its 5-minute bucket sealed (ingest lag beyond ~5 minutes — rare outside heavy storms; the audit-freshness alarm fires at 300s) is invisible to the rollup path until the daily correction job re-aggregates it; `mode=direct` sees it immediately. First/last-seen on the rollup path carry whole-second precision (the direct path is microsecond-precise).

The read side lives in the Control Plane's `trafficstore` (`traffic_error_groups_rollup.go`), next to the direct query it must agree with: it reads the tier matching the response bin width (5m/1h/1d) up to that tier's merge watermark, fills the gap behind it from the finer tier, direct-scans only the unsealed window past the rollup-5m watermark (residuals beyond the tail's row cap fold into `__overflow__`, count-preserving), and serves the full direct scan instead whenever the rollup cannot honestly cover the request: the pipeline never ran, a planned tier's earliest surviving bucket (probed on `request_count`) postdates the window start — tier retention is shorter than `traffic_event`'s for the 5m tier (7d vs 90d) — the window reaches before the error-class series' own earliest bucket in the tier (buckets aggregated before the series shipped carry `request_count` but no error-class rows; this deploy-transition fallback self-heals as windows slide past the series' first bucket, and triggering `rollup-correction` after the shipping deploy backfills the trailing week immediately), or the rollup-5m watermark is stalled more than an hour behind the window end. The handler snaps `from`/`to` outward to whole bucket boundaries so both paths cover the same event population, and reports the snapped window (`effectiveFrom`/`effectiveTo`). `affectedEndUsers` cannot ride the rollup (distinct counts don't merge — see §5 below); it is computed exactly via a per-class point query for classes up to a 50000-row bound — served by the `traffic_event_error_class_idx` expression partial index, whose expressions must stay byte-identical to the query's — and returned null above the bound or when retention has purged the class's raw rows. The 1mo tier is unused: the 1d tier's 365-day retention exceeds `traffic_event`'s own horizon. The overflow pseudo-class reports attribution `mixed` and is excluded by any attribution filter.

The **merge cascade** (`rollup_merge.go`) rolls the result coarser: `merge-1h` folds `metric_rollup_5m` into `metric_rollup_1h`, `merge-1d` folds `1h` into `1d`, and `merge-1mo` folds `1d` into `1mo` on calendar-month boundaries. One `RollupMergeJob` type drives all three; the layer's source table, target table, and bucket length come from a per-layer config. Each merge reads its source bucket via `rollupstore.QueryRollupMergeSource`, combines rows sharing `(metricName, dimensionKey, subDimension)` through `metrics.MergeRollupRows`, and writes the target bucket under the same delete-then-insert-and-watermark transaction.

### Per-metric aggregation kind (how rows fold)

Folding two rollup rows that share a logical identity is **not** always a sum. The rule is carried by the per-metric **aggregation-kind registry** in `packages/shared/core/metrics/instruments/aggregation.go`, consulted in both places that fold rows: the merge cascade (`MergeRollupRows` / `MergeThingRollupRows`) and the read-side aggregation that collapses every bucket in a query window (`BuildResult`). A metric maps to one of:

| Kind | Fold | Metrics |
| --- | --- | --- |
| `Sum` (default) | add the values | all additive counters and money — `request_count`, `*_tokens`, `*_cost_usd`, `latency_sum`, … and **every unregistered metric** |
| `Distinct` | take the **max** | `active_entities`, `active_organizations`, `distinct_sources` |
| `Histogram` | element-wise add of the six-bucket metadata array | `latency_histogram`, `hook_latency_histogram` |
| `Timestamp` | MIN(first_seen), MAX(last_seen) in metadata | `first_seen`, `last_seen` |

The default is `Sum`, so the registry is additive — only the explicitly classified metrics diverge, and `IsHistogramMetric` / `IsTimestampMetric` derive from the same table for a single source of truth.

**Gauge snapshots are excluded from the merge cascade.** The device-fleet metrics `device_fleet_status` and `device_fleet_os` are point-in-time GAUGE counts (agent Things grouped by status / OS) that the `metrics-rollup` job writes directly into `metric_rollup_1h` every hour. They are read only from the `1h` tier and have no meaningful coarser aggregate: summing successive hourly snapshots into a `1d` (or `1mo`) bucket would over-count by the number of snapshots folded (~24× at `1d`, ~720× at `1mo`). The merge cascade therefore drops these gauge metrics from its source rows before folding (`excludeGaugeRows` in `rollup_merge.go`), so they never propagate past the `1h` tier.

**Distinct-cardinality merge.** The producer emits the three distinct metrics as `Value = len(set)` per five-minute bucket — the count of distinct entities / orgs / source IPs seen *in that bucket*. The union cardinality across buckets cannot be recovered from per-bucket counts alone, so summing them is wrong: it inflates with the number of buckets folded (a 24-hour read at 1h granularity would sum up to 288 per-5m counts — 12 five-minute buckets × 24 hours). These metrics therefore fold by **MAX** — the largest single-bucket distinct count. This is a guaranteed **lower bound** on the true union (it can under-report when distinct sets differ across buckets) but eliminates the catastrophic over-count. A fully accurate union would require carrying a mergeable sketch (e.g. HyperLogLog) on the emit side instead of a scalar count; that is a deliberate follow-up, not in this change.

`rollup-correction` (`rollup_correction.go`) handles late-arriving events. Events can land in `traffic_event` after their bucket has already been rolled up; once per day the correction job recomputes every five-minute bucket of the **trailing correction window** (default 7 days, sized to cover an agent that buffered offline for several days), re-merges those days' `1h` and `1d` layers, and re-merges the `1mo` layer for any fully-sealed month the window reached into. The window must be at least as wide as the longest offline-buffer horizon: an event written more than that many days after its timestamp would otherwise land in a sealed bucket outside the window and appear in raw `traffic_event` but in no rollup.

**The window is clamped to stay inside 5m retention.** `clampToRetention` caps the effective lookback at `Rollup5mDays - 1`, so at the shipped defaults a requested 7-day window walks 6 days. Without it the two jobs overlap: `rollup-retention` purges `metric_rollup_5m` rows older than `now - Rollup5mDays` — a wall-clock instant partway through that UTC day — while a 7-day correction window starts at that same day's *midnight*. The buckets in between belong to both jobs on the same tick, and both write them: retention with one wide range `DELETE`, correction with a per-bucket `DELETE` followed by per-row `INSERT`s. Scanning the same rows in different orders deadlocks, which is what killed the 2026-08-07 run on `5m bucket 2026-07-31T01:00:00Z: … deadlock detected (SQLSTATE 40P01)` and, by leaving the cursor unadvanced, starved `vendor-bill-reconcile` behind it. Losing that race silently is the worse outcome: a pass that rebuilds a half-purged 5m tier then merges `1h` and `1d` from it publishes an understated day *as corrected*. Rebuilding buckets that are about to be deleted has no value anyway, so the window is held a whole day clear of the horizon rather than merely up to it.

The correction job calls the same per-bucket aggregation as `rollup-5m` and the merge jobs (so the logic lives in exactly one place) but with the watermark write **suppressed** (`writeWatermark = false`): re-aggregating historical buckets must not rewind the live `rollup-5m` / `merge-*` watermarks, which would force the next live tick to re-scan the whole intervening window. The DELETE+INSERT still commits per bucket, so the backfill is durable and idempotent. The "now" the date arithmetic derives from is an injectable seam, so the month-boundary branch is covered deterministically in tests rather than only on the 1st of a month.

**The correction cursor.** After a run has committed every layer of its window,
the fleet correction job upserts one `rollup_watermark` row named
`rollup-correction` (`rollup.WatermarkCorrection`) holding the newest UTC day it
rebuilt — always *yesterday*, since today is still accumulating events. This is
the one watermark here that is not a resume cursor: the correction window is the
trailing `lookbackDays` regardless of it. It is a published fact — "the 1d tier
has been re-aggregated from `traffic_event` through this day" — and it exists
because a reader of a rollup series otherwise cannot tell an absent series apart
from one that has simply not been rebuilt yet. A run that failed mid-window
leaves the previous value standing, and a failure to write it fails the run, so
the cursor never claims a day the pass did not finish.

Its consumer today is `vendor-bill-reconcile`, which compares a vendor bill
against `vendor_spend_usd` and must not read a day whose tier rows are still
being written: both jobs are 24h jobs that fire on the same scheduler tick, and
the reconcile pass finishes in ~3s while the correction pass takes ~100s on a
week of buckets. Because the cursor is published only at the very end of a run,
a single read from the reconcile job always observes the *previous* day's value,
which at steady state lands exactly on its window end with zero margin — so the
consumer waits (bounded) for the cursor to reach its window rather than racing
it. The scheduler has no ordering primitive: `Job` exposes only `Interval()`, so
a dependency between two jobs can only be expressed by the dependent job waiting
on the fact it needs. See the `vendor-bill-reconcile` row in
`jobs-architecture.md`.

`thing-rollup-correction` (`thing_rollup_correction.go`) is the per-Thing twin: the per-Thing pipeline seals buckets behind its own watermark exactly like the fleet pipeline, so without a correction sibling a late event whose per-Thing 5m bucket had already sealed would never be re-aggregated and per-Thing dashboards would permanently under-count. It shares the same `runCorrection` logic via the `correctionRollup` / `correctionMerge` seams and likewise suppresses the watermark write.

## 6. Per-Thing traffic pipeline

The per-Thing pipeline mirrors the fleet pipeline but keys every row by `thing_id`, so a single Thing's dashboard reads its own small table instead of filtering the fleet-wide one. `thing-rollup-5m` (`thing_rollup_5m.go`) aggregates `traffic_event` rows where `thing_id IS NOT NULL`, `thing-merge-1h/1d/1mo` (`thing_rollup_merge.go`) cascade the result, and `thing-rollup-correction` (`thing_rollup_correction.go`) re-aggregates the trailing correction window for late events. The watermarks are independent of the fleet pipeline, so per-Thing recovery is isolated.

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

The read collapse honors the same per-metric aggregation kind as the merge cascade: `BuildResult` folds additive metrics by sum and distinct-cardinality metrics by max across the buckets in the window (see §5 "Per-metric aggregation kind"). This is why a multi-bucket read of a distinct metric — e.g. the fleet "Top Destinations" `DeviceCount`, which reads `active_entities` grouped by `target_host` over 24 hours — returns the per-bucket peak rather than the sum of every bucket's count.

The Control Plane analytics and quota handlers (`packages/control-plane/internal/traffic/analytics/handler/`, `packages/control-plane/internal/settings/store/metricsstore/`, `packages/control-plane/internal/ai/quota/handler/`) are the consumers; they resolve dimension-value identifiers back to display labels at response time.

## 10. Operational invariants

- A bucket is rolled up only after it is sealed; the watermark and the rolled rows advance in one transaction, so the pipeline is exactly-once per bucket and safe across replica restarts.
- Re-running any producer for any bucket is idempotent — scalar tables upsert on the natural key, ops tables delete-then-insert on the synthetic key.
- Dimension values stored in rollup rows are stable identifiers; display labels are never persisted in rollups.
- Plain agents never produce per-instance ops rows or (by default) per-Thing traffic rows; they collapse into fleet aggregates so the row count stays bounded as the fleet grows.
- Late-arriving traffic events are absorbed by the daily correction job, not by widening the live sealed-bucket grace. The correction job's trailing window (default 7 days) bounds how late an event can arrive and still be folded in; an event written more than that many days after its timestamp stays in raw `traffic_event` but enters no rollup. Widen the window to match the longest agent offline-buffer horizon if that bound is too tight.
- Retention of aged rollup rows is a separate concern handled by the retention jobs; see [data-retention-purge-architecture.md](../storage/data-retention-purge-architecture.md).

## References

- `packages/nexus-hub/internal/jobs/defs/rollup/` — fleet, per-Thing, device, and health rollup producers
- `packages/nexus-hub/internal/jobs/defs/metrics/` — ops-metrics rollup producers
- `packages/nexus-hub/internal/quota/rollup/` — `rollupstore` watermark, insert, purge, and query helpers
- `packages/nexus-hub/internal/observability/opsmetrics/` — ops-sample writer and histogram merge
- `packages/nexus-hub/cmd/nexus-hub/wiring/jobs.go` — scheduler registration of all rollup jobs
- `packages/shared/core/metrics/instruments/` — `RollupRow`, `Histogram`, dimension-key builders, granularity selection, the per-metric aggregation-kind registry (`aggregation.go`), and the `MergeRollupRows` / `BuildResult` folders
- `packages/shared/core/metrics/registry/` — `SampleBatch` / `Sample` WebSocket payload types
- `packages/control-plane/internal/settings/store/metricsstore/` — Control Plane fleet rollup reader (`QueryRollupCascade`, `QueryRollupAware`)
- `packages/control-plane/internal/observability/thingstats/thingstore/` — Control Plane per-Thing rollup reader
- `packages/control-plane/internal/traffic/analytics/handler/` — read-side analytics consumers
- `tools/db-migrate/schema/observability.prisma` — `MetricRollup*`, `ThingMetricRollup*`, `MetricOpsRaw`, `MetricOpsRollup*`, `RollupWatermark` models
