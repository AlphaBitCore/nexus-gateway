# Jobs Architecture

Hub runs all of Nexus's periodic housekeeping in a **single in-process scheduler** built on `github.com/robfig/cron/v3`. There is no Sidekiq, no Celery, no Kubernetes `CronJob` resource, and no Postgres-based work queue. The 49 jobs documented in §5 are registered once at Hub boot, run inside the Hub process, and persist their definitions + run history to two Postgres tables (`job`, `job_run`) so the admin UI can show "last run / next run / 7-day run history" without scraping logs.

Anchor packages:

- `packages/nexus-hub/internal/jobs/scheduler/scheduler.go` — `*Scheduler`, the cron driver
- `packages/nexus-hub/internal/jobs/store/jobstore.go` — `*jobstore.Store`, the persistence layer for the `job` and `job_run` tables
- `packages/nexus-hub/internal/jobs/defs/` — 9 sub-domain packages, one Go file per job
- `packages/nexus-hub/internal/alerts/eval/engine.go` — the streaming alert engine, registered as one more `scheduler.Job`
- `packages/nexus-hub/cmd/nexus-hub/wiring/jobs.go` — `InitScheduler` builds every job and calls `sched.Register(...)`

## 1. What this doc covers

The scheduler is **scheduled work only**: jobs whose Run method is triggered by a cron tick (or a manual admin click), with the scheduler itself responsible for timeout, panic recovery, and run-history persistence. Three nearby things are out of scope:

- **MQ consumers** (`hub-db-writer`, `hub-alerting`) run as long-lived goroutines in the same Hub binary — see `mq-architecture.md`.
- **Per-request work** in the data-plane services (AI Gateway, Compliance Proxy, Agent) — those services have no scheduler; their periodic work (cache TTLs, metric flushes) is owned by the relevant code path's own ticker.
- **External cron** like `prod-deploy` scripts or k8s `CronJob`s — Nexus has none in production.

## 2. Scheduler core architecture

### One cron engine, no advisory locks

`scheduler.Start` constructs a single `cron.Cron` with two wrappers applied to every registered entry:

- `cron.SkipIfStillRunning(logAdapter)` — if a previous tick of the same job is still running when the next tick fires, the new tick is dropped (logged at INFO). This is what keeps a long `data-retention` sweep from stacking onto itself when it overruns the 24-hour interval.
- `cron.Recover(logAdapter)` — a panic inside a job is logged at ERROR and does not bring the cron engine down.

The scheduler **holds zero pgxpool connections of its own**. It delegates concurrency control to the job itself (either idempotent SQL, watermarks in `system_metadata`, or per-row Postgres row locks taken during the job's own transactions).

### `cfg.Scheduler.Enabled` is the leader switch

There is no runtime leader election. Multi-Hub deployments designate a single Hub as the scheduler leader by setting `cfg.Scheduler.Enabled: true` on exactly one replica; the others set it to `false` and `InitScheduler` returns `nil` early. At today's deployment scale (single-region, two-Hub HA pair where one is hot, one is warm), this is the right complexity boundary — adding etcd/Raft leader election would be infrastructure cost with no observed benefit.

The leader writes its `cfg.Hub.ID` into the `replica_id` column of every `job_run` row, so the admin UI can show which instance executed which run. When the leader fails over, the new leader inherits the `job` table state but `RecoverStaleRuns` (§3) cleans up orphan rows from the previous leader.

### 1-second interval floor

`robfig/cron/v3`'s `@every` parser silently clamps sub-second intervals to 1 second. Every Nexus job today has Interval ≥ 5 seconds (the fastest are `semantic-cache-reindex` and `alerteval-engine` at 5 s; the next tier is `stale-thing-sweep` + `credential-circuit-flush` at 30 s), so this floor never matters in practice. Job authors needing sub-second granularity should pick a different mechanism — cron is for housekeeping, not control-loop tight cycles.

## 3. Job lifecycle

### The `Job` interface

```go
type Job interface {
    ID() string
    Name() string
    Description() string
    Interval() time.Duration
    Run(ctx context.Context) error
}
```

`ID` is the stable slug used as the primary key in the `job` table — renaming it breaks the admin UI's history continuity, so renames are migration-grade changes. `Name` and `Description` are human-readable; `Description` should fit in one sentence (the admin UI renders it inline).

Two optional capabilities are detected by type assertion:

- `OnStartRunner` — `RunOnStart() bool` returning true makes the scheduler execute the job once immediately after `Start`, before the first tick. Used by long-interval jobs (5 m / 1 h / daily rollups) so the first pass is not delayed for the full interval after boot. About a third of the jobs implement this.
- `MaxRunDurationer` — `MaxRunDuration() time.Duration` overrides the default per-run timeout (default is `max(Interval, 60s)`). Used by long-running sweeps (monthly retention over large tables) whose Run legitimately exceeds the interval-derived default.

### Boot sequence

`InitScheduler` runs four steps in order, and each must finish before the next starts:

1. `New(logger).WithJobStore(jobStore).WithReplicaID(hubID)` — wire dependencies. No I/O yet.
2. `sched.Register(...)` × N (49 calls) — populate the in-memory `jobs map[string]*entry`. Each entry defaults to enabled; the persisted `enabled` flag from `job.enabled` will overwrite this in step 3.
3. `sched.SyncDefinitions(ctx)` — for every registered job, upsert `(id, name, description, interval_sec)` into the `job` table, then read back `enabled` and reconcile the in-memory atomic. **`SyncDefinitions` does not touch the `enabled` column on upsert** — admin disables survive Hub restarts, code changes to name/description/interval propagate.
4. `sched.RecoverStaleRuns(ctx)` — flip every `job_run` row still in `status='running'` to `'interrupted'`. These are orphans from the previous Hub process that died mid-Run; without this step the admin UI would show them as perpetually running.
5. `sched.Start()` — construct the cron engine, register every enabled entry via `cron.AddFunc("@every <interval>", runOne)`, start the engine, then kick off every `OnStartRunner.RunOnStart()=true` entry in a detached goroutine so `Start` returns promptly.

### `SetEnabled` is hot

Admin can toggle a job from the UI without restarting Hub. `SetEnabled(ctx, id, true)` writes `enabled=true` to the `job` table, flips the in-memory atomic, and **immediately adds the entry to the running cron engine** via `cron.AddFunc`. Disable removes the entry. The DB write happens first; if it fails, the in-memory state is not touched and the live cron is not modified — the admin gets the error and tries again.

### `Trigger` is manual + bypasses enabled

`Trigger(ctx, id)` runs a job once immediately in a detached goroutine, **bypassing the enabled flag** (so an admin can manually invoke a disabled job for one-off forensics). The run still goes through the same `runOne` path that records to `job_run` and respects the per-job timeout. The caller's ctx is intentionally *not* propagated into the Run — the manual trigger survives the admin HTTP request completing before the job finishes. Only scheduler `Stop` (or process death) terminates the in-flight run.

## 4. JobStore + run history

Two tables back the scheduler: `job` (one row per registered job, mutable enabled flag) and `job_run` (one row per Run invocation, append-only history).

### `job` table contract

- `id` (PK) — matches `Job.ID()`
- `name`, `description`, `interval_sec` — overwritten by every `SyncDefinitions` call; code changes propagate
- `enabled` — admin-controlled, **never** overwritten by `SyncDefinitions`. Defaults to true for newly-registered jobs (the first `SyncDefinitions` after registration inserts the row with `enabled=true`); subsequent `SyncDefinitions` calls leave the column alone.

### `job_run` table contract

- `id` (PK, ULID) — assigned by `StartRun` and returned to the scheduler so `FinishRun` can update the same row
- `job_id` (FK to `job.id`)
- `replica_id` — set from `WithReplicaID(hubID)` so the admin UI shows which Hub instance executed the run
- `started_at`, `finished_at`, `duration_ms`
- `status` — `running` | `success` | `error` | `interrupted` (the last set by `RecoverStaleRuns` for orphans)
- `error_message` — empty on success; populated on `error` / DB-stored stack on panic

### `FinishRun` uses a detached ctx

The Run's ctx has a hard deadline (§6). If a Run hits `context.DeadlineExceeded`, its own ctx is dead — but the scheduler still needs to record `status='error'` + `error_message='context deadline exceeded'` so the admin UI shows the failure. The scheduler uses `context.Background()` for the `FinishRun` call to escape the dead ctx:

```go
if ferr := s.js.FinishRun(context.Background(), runID, status, dur, errMsg); ferr != nil {
    ...
}
```

A deadline-exceeded run that *also* fails to record its FinishRun row (DB outage at the same moment) shows up in the next boot's `RecoverStaleRuns` sweep as `interrupted`. This is the worst-case path and is observable.

## 5. Job catalogue

The 49 rows below are the complete production catalogue. Every row is anchored to a `Register()` call in `cmd/nexus-hub/wiring/jobs.go` (or `registerAlertEvalEngine` for `alerteval-engine`). Default cadences come from the job constructor's `interval` default; admins override via the corresponding `cfg.Scheduler.Intervals.*` field (and `cfg.Scheduler.Retention.*` for retention windows). Some constructors take cadence directly from `cfg.Scheduler.*Interval` with no zero-default — those rows show `(cfg)` and are documented in the operator config doc.

The 8 multi-cadence variants (rows whose file column says "cadence variant") are constructed at runtime from helper structs (`rollup_merge.go`, `thing_rollup_merge.go`, `ops_rollup_cascade.go`) rather than having one `JobID` const each. These IDs are the only ones the `scripts/check-jobs-catalogue.sh` pre-commit gate machine-verifies; the rest are honor-system but every static `JobID` const present in the tree must appear below.

### 5.1 Traffic + ops rollup pipeline (15 jobs)

The 5-minute → 1-hour → 1-day → 1-month rollup cascade for both fleet-wide traffic metrics and per-Thing traffic metrics, plus the parallel ops-metrics cascade.

| ID | File | Default cadence | Purpose |
|---|---|---|---|
| `rollup-5m` | `defs/rollup/rollup_5m.go` | 1 min | Aggregates `traffic_event` rows into `metric_rollup_5m` every minute, catching up from the last committed bucket to the most recent sealed 5-minute bucket. |
| `merge-1h` | `defs/rollup/rollup_merge.go` (cadence variant) | 5 min | Merges `metric_rollup_5m` into `metric_rollup_1h`, catching up from the persisted watermark to the most recent sealed 1-hour bucket. |
| `merge-1d` | `defs/rollup/rollup_merge.go` (cadence variant) | 1 hour | Merges `metric_rollup_1h` into `metric_rollup_1d`, catching up to the most recent sealed 1-day bucket. |
| `merge-1mo` | `defs/rollup/rollup_merge.go` (cadence variant) | 24 hour | Merges `metric_rollup_1d` into `metric_rollup_1mo` using calendar-month bucket boundaries. |
| `thing-rollup-5m` | `defs/rollup/thing_rollup_5m.go` | 1 min | Per-Thing variant of `rollup-5m`: aggregates `traffic_event` rows with `thing_id` into `thing_metric_rollup_5m`, keyed by (thing_id, metric, dim, sub-dim). |
| `thing-merge-1h` | `defs/rollup/thing_rollup_merge.go` (cadence variant) | 5 min | Per-Thing variant of `merge-1h`. |
| `thing-merge-1d` | `defs/rollup/thing_rollup_merge.go` (cadence variant) | 1 hour | Per-Thing variant of `merge-1d`. |
| `thing-merge-1mo` | `defs/rollup/thing_rollup_merge.go` (cadence variant) | 24 hour | Per-Thing variant of `merge-1mo` (calendar-month boundaries). |
| `rollup-correction` | `defs/rollup/rollup_correction.go` | 24 hour | Recomputes fleet rollups across the trailing correction window (default 7 days) to absorb late-arriving events. Re-runs every 5-minute bucket then re-merges the 1h, 1d, and (for any sealed month the window touched) 1mo layers, with the watermark write suppressed so the backfill never rewinds the live cursor. |
| `thing-rollup-correction` | `defs/rollup/thing_rollup_correction.go` | 24 hour | Per-Thing twin of `rollup-correction`: re-aggregates the per-Thing pipeline over the trailing window so late events whose per-Thing 5m bucket already sealed are not permanently under-counted. |
| `metrics-rollup` | `defs/metrics/metrics_rollup.go` | 1 hour | Aggregates device fleet status/OS (GAUGE snapshots, excluded from the merge cascade) and agent action volume into `metric_rollup_1h`. |
| `ops-rollup-5m` | `defs/metrics/ops_rollup_5m.go` | 1 min | Aggregates `metric_ops_raw` into `metric_ops_rollup_5m` — the tier that owns the raw read. Services keep per-instance rows; agents collapse into a single fleet-aggregate row (`thing_id IS NULL`) unless in diagnostic mode for the bucket. |
| `ops-rollup-1h` | `defs/metrics/ops_rollup_1h.go` | 5 min | Aggregates `metric_ops_raw` into `metric_ops_rollup_1h`. Services keep per-instance rows; agents collapse into a single fleet-aggregate row (`thing_id IS NULL`) per metric+dim unless in diagnostic mode for the bucket. |
| `ops-rollup-1d` | `defs/metrics/ops_rollup_cascade.go` (cadence variant) | 1 hour | Aggregates `metric_ops_rollup_1h` into `metric_ops_rollup_1d`. Sample-count-weighted averages; histograms merge element-wise. Preserves the fleet vs per-thing distinction from the 1h layer. |
| `ops-rollup-1mo` | `defs/metrics/ops_rollup_cascade.go` (cadence variant) | 24 hour | Aggregates `metric_ops_rollup_1d` into `metric_ops_rollup_1mo` once per day, calendar-month boundaries; current (unsealed) month always excluded. |

### 5.2 Health + reliability rollup (2 jobs)

Per-credential and per-provider health rollups feeding the `credential.*` and `provider.unavailable` alert families.

| ID | File | Default cadence | Purpose |
|---|---|---|---|
| `credential-health-rollup` | `defs/rollup/credential_health_rollup.go` | 5 min | Computes per-credential health (healthy / degraded / unavailable / collecting / unknown), dominantError, and trend (improving / stable / degrading) from `traffic_event` over a short (5 min) and long (1 h) window. Persists to `Credential.health*` columns; only rows whose status changed update `healthStatusChangedAt`. |
| `provider-health-rollup` | `defs/rollup/provider_health_rollup.go` | 5 min | Recomputes `ProviderHealth` (error rate, avg latency, sample count, status) from `traffic_event` over a 30-minute rolling window. Replaces the AI Gateway in-process HealthTracker DB flush. |

### 5.3 Retention + flush (7 jobs)

Periodic purge and Redis-to-DB drain jobs.

| ID | File | Default cadence | Purpose |
|---|---|---|---|
| `data-retention` | `defs/retention/data_retention.go` | 24 hour | Deletes audit and rollup rows older than the configured retention period (`cfg.Scheduler.Retention.{TrafficEventDays, TrafficEventPayloadDays, AdminAuditLogDays, MetricRollupDays}`). |
| `ops-retention` | `defs/retention/ops_retention.go` | 24 hour | Chunked-DELETEs aged `metric_ops_rollup_5m/1h/1d/1mo` and `thing_diag_event` rows. Per-class retention is read live from `metric_ops_retention_config` (one row per layer: runtime_5m, business_5m, runtime/business_1h, runtime/business_1d, runtime/business_1mo, diag_info, diag_warn, diag_error, diag_fatal). Does NOT touch `metric_ops_raw` — that is partition-dropped by `ops-raw-partition`. Deletes run in chunks of 10k rows so transactions stay short. |
| `ops-raw-partition` | `defs/retention/ops_raw_partition.go` | 6 hour | Maintains the daily RANGE partitions of `metric_ops_raw`: pre-creates upcoming day partitions and DROPs partitions older than `NEXUS_HUB_SCHEDULER_OPS_RAW_DAYS` (default 30). Replaces row-by-row DELETE of raw with O(1) whole-day partition drops. |
| `rollup-retention` | `defs/rollup/rollup_retention.go` | 24 hour | Purges aged rows from `metric_rollup_5m/1h/1d/1mo` based on per-tier retention days (`cfg.Scheduler.Retention.Rollup{5m,1h,1d,1mo}Days`). |
| `job-retention` | `defs/retention/job_retention.go` | 24 hour | Prunes `job_run` rows, keeping the N most recent runs per job (default N=100). |
| `credential-stats-flush` | `defs/retention/credential_stats_flush.go` | 60 sec | Drains per-credential usage counters and timestamps from Redis into the `Credential` table. Runs frequently to keep `lastUsedAt`, `lastSuccessAt`, `lastFailureAt`, and `totalUsageCount` up to date without high-frequency concurrent DB writes. |
| `credential-circuit-flush` | `defs/retention/credential_circuit_flush.go` | 30 sec | Drains `cred:circuit:dirty` Redis set into `Credential.circuit*` columns. Uses an in-flight working set for at-least-once delivery; rehydrates Redis from DB on first run after restart. |

### 5.4 State-poll alerts (6 jobs)

Class-1 alert jobs (in the alerting taxonomy): poll DB state every interval and raise/resolve alerts based on the snapshot. Distinct from `alerteval-engine` (§5.9), which is class-4 streaming over MQ.

| ID | File | Default cadence | Purpose |
|---|---|---|---|
| `thing-offline-alerts` | `defs/health/thing_offline_alerts.go` | 60 sec | Raises `thing.offline` for Things whose `last_seen_at` has exceeded the rule's `offlineAfterSec` threshold; auto-resolves firing alerts for Things that have come back fresh or have been deleted. |
| `provider-unavailable-alerts` | `defs/health/provider_unavailable_alerts.go` | 60 sec | Raises `provider.unavailable` for Providers whose `ProviderHealth.status` is `unavailable`; auto-resolves when the provider recovers to healthy, recovering, or degraded. |
| `credential-reliability-alerts` | `defs/health/credential_reliability_alerts.go` | 60 sec | Raises `credential.circuit_open`, `credential.health_unavailable`, and `credential.health_degraded_sustained` alerts from the persisted reliability state on the `Credential` table. |
| `credential-stale-alerts` | `defs/health/credential_stale_alerts.go` | 1 hour | Polls `Credential.lastSuccessAt` for enabled credentials and raises `credential.stale_last_success` when no successful use in `params.staleAfterDays`; auto-resolves once a credential is used successfully again. |
| `agent-cert-expiration-alerts` | `defs/health/agent_cert_expiration_alerts.go` | 1 hour | Polls `thing_agent.cert_expires_at` for desktop-agent Things and raises `agent.cert_expiration_imminent` as each warn-day threshold (default 30 / 14 / 7 / 1 days) approaches. Auto-resolves on renewal. |
| `cache-quality-monitor` | `defs/health/cache_quality_monitor.go` | 5 min | Detects elevated error rates in normaliser-modified requests over the past 30 minutes and auto-reverts all active rules to `dry_run_always` when the error rate exceeds 3× baseline, preventing normaliser-induced quality regressions. |

### 5.5 Expiry + auto-revert (7 jobs)

Time-based state transitions: an admin set a window, the window has passed, the scheduler flips the flag.

| ID | File | Default cadence | Purpose |
|---|---|---|---|
| `auth-cleanup` | `defs/expiry/auth_cleanup.go` | 1 hour | Deletes expired refresh and revoked token rows. |
| `enrollment-cleanup` | `defs/expiry/enrollment_cleanup.go` | 1 hour | Marks expired pending agent enrollment tokens as `expired`. |
| `vk-expiry` | `defs/expiry/vk_expiry.go` | 1 hour | Expires virtual keys past their expiry date and raises `quota.vk_expiring` alerts for keys expiring within the warn window. Auto-resolves on renewal or once outside the window. |
| `credential-expiry` | `defs/expiry/credential_expiry.go` | 1 hour | Advances `rotationState` to `pending_rotation` and raises `credential.expiring` alerts for credentials approaching `expiresAt`. Raises CRITICAL alerts for overdue credentials (no auto-disable). Auto-resolves once a credential is no longer in the expiring set. |
| `credential-retire` | `defs/expiry/credential_retire.go` | 1 hour | Advances credentials from `retiring` → `retired` (after drain window) and hard-deletes retired credentials past their `retireAt` date. |
| `override-expiry` | `defs/expiry/override_expiry.go` | (cfg, RunOnStart) | Clears `thing_config_override` rows past their `expires_at` — including the per-thing `diag_mode` override that carries a diagnostic-mode window (`expires_at` = window end), so an expired window stops raising the agent's log level without a dedicated job. |
| `passthrough.expiry` | `defs/expiry/passthrough_expiry.go` | 60 sec | Flips `enabled=false` on emergency-passthrough rows whose `expires_at` has passed across global / adapter / provider tiers. |

### 5.6 Drift + housekeeping (5 jobs)

Reconciliation jobs that compare desired state vs reported state vs actual state.

| ID | File | Default cadence | Purpose |
|---|---|---|---|
| `config-drift-check` | `defs/drift/drift.go` | (cfg) | Detects Things whose reported config version differs from desired and triggers repair via the fleet manager. |
| `stale-thing-sweep` | `defs/drift/stale_thing.go` | 30 sec | Marks Things offline when their `last_seen_at` exceeds the per-category threshold (default 90 s for service Things — 3x the 30 s ping interval so a single jittered ping cannot flap a healthy service offline). |
| `smart-group-recompute` | `defs/drift/smart_group_recompute.go` | 60 sec | Re-evaluates every smart `DeviceGroup`'s `membership_query` against the current device fleet and replaces the `device_group_membership_cache` rows. Runs every 60 s as a safety net; heartbeat-driven recomputes handle the steady-state. |
| `user-identity-enrichment` | `defs/drift/identity_enrichment.go` | (cfg) | Backfills user identity fields into recent `traffic_event` rows using IAM lookups. |
| `exemption-gc` | `defs/drift/exemption_gc.go` | 5 min | Fires a Cat B invalidate signal when compliance exemption grants have recently expired so compliance-proxy refreshes its in-memory view without waiting for the next admin mutation. |

### 5.7 Audit (4 jobs)

Integrity + observability of the admin audit pipeline.

| ID | File | Default cadence | Purpose |
|---|---|---|---|
| `audit-chain-verify` | `defs/audit/audit_chain_verify.go` | (cfg, RunOnStart) | Walks the `AdminAuditLog` hash chain (`previousHash` / `integrityHash`) and reports tamper detection at ERROR level. |
| `audit-freshness-check` | `defs/audit/audit_freshness_check.go` | 60 sec | Alarms when the most recent admin audit row is older than 5 min — catches the silent-stall failure class where the MQ consumer pulled the message but the INSERT failed. |
| `normalize-backfill` | `defs/audit/normalize_backfill.go` | 5 min | Re-runs normalize against the raw request/response bytes for `traffic_event_normalized` rows whose sidecar is missing, all-NULL, **or stamped with a `normalize_version` other than the current schema version** — bumping `normcore.SchemaVersion` heals every historical row through this one mechanism (≈200 rows / 5-min tick, newest-first). Inline bodies are read directly; ref-only spilled bodies are fetched from the hub `SpillStore` (64 MiB read cap). Rows that cannot be filled are recorded in the `traffic_event_normalize_skip` ledger (`reason` ∈ `spill_ref_only` (no spill store wired) / `spill_fetch_failed` / `no_payload_produced`) with the schema version of the attempt; the scan excludes a skip-marked row only while its stamped version matches the current one, so every previously unfillable row re-admits exactly once per version bump — the newest-first `LIMIT` batch always advances, and the "bump heals everything" invariant covers skip-marked rows too. The marker is backfill-internal (no CP store / Traffic drawer reads it). `nexus_normalize_backfill_skipped_total{reason}` is a one-time-per-row-per-version tally, not a recurring rate. |
| `siem-bridge` | `defs/audit/siem_bridge.go` | `bridge.PollInterval()` | Polls `traffic_event` and `AdminAuditLog` for new rows, classifies them, and forwards them to the configured SIEM sink. Checkpoints persisted in `system_metadata`. Registered unconditionally whenever the scheduler is enabled; the bridge self-activates when `siem.config.enabled` is set in `system_metadata`. |

### 5.8 Quota (1 job)

| ID | File | Default cadence | Purpose |
|---|---|---|---|
| `quota-alert-check` | `defs/quota/quota_alert_check.go` | 60 sec | Evaluates current-month cost usage against `QuotaOverride` and `QuotaPolicy` cost limits every minute and raises `quota.threshold` alerts when usage crosses configured thresholds. Auto-resolves with 2 percentage-point hysteresis once usage falls back. |

### 5.9 Cache infrastructure (1 job)

| ID | File | Default cadence | Purpose |
|---|---|---|---|
| `semantic-cache-reindex` | `defs/semanticcacheflush/job.go` | 5 sec | Blue/green Valkey vector index swap when the embedding model fingerprint changes. Creates the new FT index, drops the old one, and stamps an audit row. No-op when fingerprints already match. |

### 5.10 Streaming alert engine (1 job, registered as a scheduler.Job)

| ID | File | Default cadence | Purpose |
|---|---|---|---|
| `alerteval-engine` | `alerts/eval/engine.go` | 5 sec (TickSec) | Subscribes to MQ traffic + audit events under the `hub-alerting` consumer group, maintains in-memory ring buffers per registered Aggregator, and evaluates threshold rules every tick. Lives outside `internal/jobs/defs/` but implements `scheduler.Job` so its status appears alongside the others in the admin UI. |

### Catalogue totals

| Sub-domain | Count |
|---|---|
| 5.1 Traffic + ops rollup | 15 |
| 5.2 Health rollup | 2 |
| 5.3 Retention + flush | 7 |
| 5.4 State-poll alerts | 6 |
| 5.5 Expiry + auto-revert | 7 |
| 5.6 Drift + housekeeping | 5 |
| 5.7 Audit | 4 |
| 5.8 Quota | 1 |
| 5.9 Cache infrastructure | 1 |
| 5.10 Streaming alert engine | 1 |
| **Total** | **49** |

These 49 rows correspond 1-to-1 with the `Register()` / `registerAlertEvalEngine()` calls in `packages/nexus-hub/cmd/nexus-hub/wiring/jobs.go`. Every row, including `siem-bridge`, is registered unconditionally whenever `cfg.Scheduler.Enabled` is true.

## 6. Failure semantics

### Per-run hard timeout

`runOne` wraps every Run in `context.WithTimeout`. The timeout is:

- `MaxRunDuration()` if the job implements `MaxRunDurationer`, else
- `max(Interval, 60s)` — at least 60 s so a 5-second-interval job still has a generous cap; otherwise `Interval` so the next tick is never blocked by an overrunning Run.

A `context.DeadlineExceeded` failure is logged at WARN level (with `job`, `duration`, and `max` keys) — this is "expected" timeout, treated differently from "the job genuinely errored". Any other error is logged at ERROR. Both still write `status='error'` + the error message to `job_run`.

### Panic recovery

Panics inside a tick-triggered Run are caught by `cron.Recover(logAdapter)` registered in the wrapper chain at `Start`. Panics inside a `Trigger`-triggered (manual) Run are caught by a `defer recover()` directly in `runOne` because the manual path bypasses the cron wrapper chain. Either way, the panic is logged at ERROR with `job` + `panic` keys and the scheduler keeps running.

### Skip-if-still-running

`cron.SkipIfStillRunning(logAdapter)` is applied to every entry. If a previous tick's Run is still executing when the next tick fires, the new tick is dropped and logged at INFO. This is what keeps a long `data-retention` sweep from stacking onto itself when row counts spike — the next 24-hour tick will simply skip until the previous one finishes. Job authors should size their work to complete within the interval; persistent skipping is a signal to either raise the interval or shard the work.

### Graceful stop

`Stop` signals the cron engine to stop accepting new ticks, then waits up to `stopDrainTimeout` (30 s) for in-flight Runs to finish, then cancels the scheduler-wide ctx. A Run still executing past the drain timeout gets its ctx cancelled — well-behaved jobs check ctx and return; ill-behaved jobs may leave their Run goroutine alive until the process exits. `Stop` is idempotent; a second call is a no-op.

## 7. Admin UI / introspection

The admin UI's Infrastructure → Jobs page reads from the scheduler via the Hub HTTP API:

- `ListJobs(ctx)` — returns `[]JobStatus` for all registered jobs. When a jobstore is attached, the aggregate counters (`RunCount`, `ErrorCount`), `LastRun`, `LastDuration`, `LastStatus`, `LastError` come from `ListJobsWithStats` so values reflect history across restarts. `NextRun` is sourced live from the cron engine when the job is currently scheduled (entry registered, not disabled); falls back to a `LastRun + Interval` estimate when the engine has stopped or the job is disabled.
- `GetJob(ctx, id)` — same shape, single job.
- `SetEnabled(ctx, id, enabled)` — toggle. DB write first; in-memory atomic + live cron entry add/remove only on DB success.
- `Trigger(ctx, id)` — manual run, bypassing enabled, in a detached goroutine. Returns immediately; the caller polls `ListRuns` to see the result.
- `ListRuns(ctx, id, limit, offset)` — paginated `job_run` history newest-first, plus a total count for paginator UI.

`statusFromStats` is the function that joins the persisted aggregate row with the live cron entry to produce a `JobStatus` — read this when wondering why a `NextRun` value on the UI looks stale (most common cause: the job is disabled, so there's no live cron entry, and the value is the `LastRun + Interval` estimate).

## 8. Operations

### Designating the scheduler leader

Set `cfg.Scheduler.Enabled: true` on **exactly one** Hub replica in a multi-Hub deployment. On every other replica set it to `false`. `InitScheduler` returns `nil` early when disabled, so other Hub functions (HTTP API, MQ consumers, fleet manager) keep running normally — only the scheduler is gated.

Two leaders simultaneously enabled is a misconfiguration: both will try to advance the same `job_run` rows, both will try to acquire row-level locks on `traffic_event` during rollup, and you will see duplicate runs in the admin UI plus elevated DB CPU. There is no detection or auto-correction — the operator must reconcile config.

### Tuning per-job cadence

Most cadences are driven by the constructor's `interval` default but can be overridden via `cfg.Scheduler.Intervals.*` fields (see `packages/nexus-hub/internal/config/config.go` for the canonical struct). The `(cfg)` annotation in §5 marks jobs whose constructor has no default and reads the interval verbatim from the config struct.

Retention windows are separate from cadence: `cfg.Scheduler.Retention.{TrafficEventDays, AdminAuditLogDays, MetricRollupDays, Rollup5mDays, Rollup1hDays, Rollup1dDays, Rollup1moDays, TrafficEventPayloadDays}` control *how much data each retention sweep deletes*, not how often the sweep runs. Per-class ops retention (the rich set in `ops-retention`) lives in the `metric_ops_retention_config` table and is editable at runtime without a Hub restart.

### Adding a new job

Four steps:

1. **Code** — create `packages/nexus-hub/internal/jobs/defs/<sub-domain>/<name>.go` with a struct that implements `scheduler.Job`. Define a `<name>JobID = "<slug>"` const at top-of-file (the lockstep script's grep pattern is `[a-zA-Z]+JobID\s*=`). If the cadence should be admin-tunable, accept it from a config field; if it has a sensible default, fall back to that default in the constructor.
2. **Wire** — add a `sched.Register(...)` call in the appropriate clause of `InitScheduler` in `packages/nexus-hub/cmd/nexus-hub/wiring/jobs.go`. Pick the right arguments — most jobs take `pool` (pgxpool), some take `raiser` (for alert-raising jobs), some take `alertStore` (for jobs that need rule params).
3. **Catalogue** — add a row to the right §5 sub-section of this doc. The pre-commit `check-jobs-catalogue.sh` script enforces that every static `JobID` const in the tree appears in §5 (plus the 8 multi-cadence variants by name). If you skip this the gate fails.
4. **Config** — if the cadence is admin-tunable, add a field to `cfg.Scheduler.Intervals` and document it in the operator config doc.

### The lockstep gate's current scope

`scripts/check-jobs-catalogue.sh` machine-enforces two things:

1. The 8 multi-cadence variant IDs (`merge-1h`, `merge-1d`, `merge-1mo`, `thing-merge-1h`, `thing-merge-1d`, `thing-merge-1mo`, `ops-rollup-1d`, `ops-rollup-1mo`) must each appear in this doc.
2. Every `<name>JobID = "..."` const under `packages/nexus-hub/internal/jobs/` must have a matching row in this doc. The scan uses a recursive `find "$JOBS_DIR" -name '*.go'` so it reaches the jobs under `defs/<sub-domain>/`.

The lockstep is skipped entirely when this doc is absent — an escape hatch documented at the top of the script; removing it re-enables strict mode.

### Reading runs during incidents

The admin UI's Infrastructure → Jobs page is the primary surface. For shell-side investigation:

```sql
-- last 20 runs of a job, newest first
SELECT id, started_at, finished_at, duration_ms, status, error_message
FROM job_run
WHERE job_id = 'rollup-5m'
ORDER BY started_at DESC
LIMIT 20;

-- jobs with the most errors in the last 24 h
SELECT job_id, COUNT(*) FILTER (WHERE status = 'error') AS errs, COUNT(*) AS total
FROM job_run
WHERE started_at > now() - interval '24 hours'
GROUP BY job_id
ORDER BY errs DESC;

-- orphan rows that should have been cleaned by RecoverStaleRuns
-- (a non-empty result is a bug — file an issue)
SELECT * FROM job_run WHERE status = 'running' AND started_at < now() - interval '2 hours';
```

A `status='error'` run's `error_message` is the formatted output of `err.Error()` from the Run; pair it with the Hub's `slog` ERROR line at the same timestamp for full context (stack trace, job-specific structured fields).
