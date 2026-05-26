---
doc: jobs-architecture
area: cross-cutting
service: foundation
tier: 1
updated: 2026-05-20
---

# Scheduled Jobs Architecture

> **Tier 1 architecture doc.** Read this before adding a new scheduled job, debugging job execution, or wiring an alert backed by a job. Hub is the single owner of the scheduler today; multi-instance scheduling is not in scope.

---

## 1. Scheduler model

A single Hub instance owns the scheduler. Jobs are defined in code under `packages/nexus-hub/internal/jobs/defs/<category>/` and registered at Hub startup. Each job implements the `Job` interface (`packages/nexus-hub/internal/jobs/scheduler/scheduler.go:43-49`):

```go
type Job interface {
    ID() string
    Name() string
    Description() string
    Interval() time.Duration
    Run(ctx context.Context) error
}
```

Optional capabilities: `OnStartRunner.RunOnStart() bool` (kick off once immediately after Start), `MaxRunDurationer.MaxRunDuration() time.Duration` (override the default `max(Interval, 60s)` per-run context timeout).

Registration:

```go
scheduler.Register(myJob) // myJob implements Job + optional capabilities
```

The internal cron engine is `github.com/robfig/cron/v3`. The scheduler converts a job's `Interval()` into a cron spec via `scheduleEntry` (sub-second precision is not supported — `intervalSec` floored at 1).

Multi-instance designation is by config: only the Hub whose `cfg.Scheduler.Enabled` is true runs the scheduled-job loop. There is no runtime leader election (and no need for one at our deployment scale).

## 2. Singleton semantics

Every job is single-instance per Hub. If a previous run is still in progress when the next fire-time arrives, the new run is **skipped** (not queued).

Singleton enforcement is in-process via `cron.SkipIfStillRunning` from `github.com/robfig/cron/v3` (`packages/nexus-hub/internal/jobs/scheduler/scheduler.go:277`). The earlier `pg_advisory_xact_lock` design was tried and abandoned — it caused a self-deadlock at boot when concurrent jobs each held a pgx pool connection for their lock and then needed additional connections for their work (`scheduler.go` design note, lines 1-14). Skipped runs increment the scheduler's skipped counter and log a structured event; no DB lock is consulted.

Multi-instance designation is by configuration only: `cfg.Scheduler.Enabled` selects which Hub instance runs scheduled jobs. There is no runtime leader election.

## 3. Retry semantics

Job handler failure does not retry within the scheduler. The next scheduled fire-time gets a fresh attempt. Reason: most jobs are periodic and skipping one tick is recoverable.

Resumability is an opt-in convention, not a scheduler flag. Long-running idempotent jobs (rollups, retention) persist a per-job cursor in their own state table (e.g., `rollupstore.GetWatermark` / `UpdateWatermark` for `rollup-5m`, `merge-1h`, etc.); a fresh fire-time picks up from the persisted watermark instead of restarting from scratch. The scheduler interface (`Job`) carries no `Resumable` boolean — the contract is by handler implementation.

## 4. Observability

Run-level observability is DB-backed, not Prometheus-backed. Every Job run is persisted to the `job_run` table via `jobStoreIface.StartRun` / `FinishRun` (`packages/nexus-hub/internal/jobs/scheduler/scheduler.go:98-108`) with `status ∈ {success, error, running, skipped}`, duration, and an error message column. The admin UI reads these rows (via `ListJobsWithStats` / `ListRuns`) to drive the `/infrastructure/jobs` page.

Per-job Prometheus emission is the responsibility of the job's own handler — most rollup / health / alert-raiser jobs publish their own domain counters (e.g. `traffic_event` rollup counters, alert evaluator counters). The scheduler itself does not register `nexus_job_*` counters today.

Alerts that fire on missing / late jobs come through `alerting-architecture.md` and consult `job_run` state plus the per-job domain metrics, not a generic scheduler counter.

## 5. The job catalogue (today)

The runtime authority is `packages/nexus-hub/internal/jobs/` — and more specifically `packages/nexus-hub/internal/jobs/defs/<category>/<job>.go`, where every JobID const lives next to its handler. The CI script `scripts/check-jobs-catalogue.sh` enforces lockstep between this catalogue and the live `JobID` constants. When you add a job (or a new cadence variant from a multi-cadence helper like `rollup_merge.go`), append a row in the same PR.

Cron specs / intervals are **deployment-tunable** and are passed at construction time from `packages/nexus-hub/cmd/nexus-hub/main.go`, not encoded in the job files. This catalogue therefore lists ID + purpose only; for the live interval of a specific job, read `main.go`.

### Config sync / drift

| Job ID | Purpose |
|---|---|
| `config-drift-check` | Detects Things whose reported config version differs from desired and triggers repair. |
| `stale-thing-sweep` | Marks Things offline when `last_seen_at` exceeds the per-category threshold. |
| `thing-offline-alerts` | Raises `thing.offline` alerts; auto-resolves when `last_seen_at` recovers. |
| `diag-mode-expiry` | Clears the `diagModeUntil` flag once the diag window ends. |

### Auth / enrollment

| Job ID | Purpose |
|---|---|
| `auth-cleanup` | Deletes expired refresh and revoked token rows hourly. |
| `enrollment-cleanup` | Marks expired pending agent enrollment tokens as expired. |
| `agent-cert-expiration-alerts` | Raises `agent.cert_expiration_imminent` as device certs near expiry. |

### Audit pipeline

| Job ID | Purpose |
|---|---|
| `audit-chain-verify` | Periodically validates audit hash-chain integrity. |
| `audit-freshness-check` | Alerts on audit-write lag (Hub consumer falling behind MQ). |

### Credentials (provider keys)

| Job ID | Purpose |
|---|---|
| `credential-stats-flush` | Drains per-credential Redis usage counters + timestamps into the Credential table. |
| `credential-circuit-flush` | Drains `cred:circuit:dirty` into `Credential.circuit*` columns; at-least-once via in-flight set. |
| `credential-health-rollup` | Computes per-credential health classification, dominantError, and trend. |
| `credential-reliability-alerts` | Raises `credential.circuit_open`, `health_unavailable`, `health_degraded_sustained` alerts. |
| `credential-stale-alerts` | Raises `credential.stale_last_success` when no successful use over the window. |
| `credential-expiry` | Advances `rotationState` to `pending_rotation`; raises `credential.expiring` alerts. |
| `credential-retire` | Advances retiring → retired after drain window; hard-deletes past retention. |

### Provider health

| Job ID | Purpose |
|---|---|
| `provider-health-rollup` | Recomputes `ProviderHealth` from `traffic_event` over a 30-min rolling window. |
| `provider-unavailable-alerts` | Raises `provider.unavailable` alerts; auto-resolves on recovery. |

### Virtual keys / quota

| Job ID | Purpose |
|---|---|
| `vk-expiry` | Expires VKs past their expiry date; raises `quota.vk_expiring` alerts. |
| `quota-alert-check` | Evaluates current-month cost vs `QuotaOverride` / `QuotaPolicy` limits; raises threshold alerts. |
| `override-expiry` | Sweeps expired per-Thing config overrides. |

### Metrics rollup (traffic_event)

| Job ID | Purpose |
|---|---|
| `rollup-5m` | Aggregates `traffic_event` into `metric_rollup_5m` every minute (catch-up + current bucket). |
| `merge-1h` / `merge-1d` / `merge-1mo` | Three cadences of `rollup_merge.go` — merges 5m → 1h → 1d → 1mo. |
| `rollup-correction` | Recomputes T-1 (yesterday) to absorb late-arriving events. |
| `rollup-retention` | Purges aged rows from `metric_rollup_{5m,1h,1d,1mo}`. |
| `thing-rollup-5m` | Per-Thing version of `rollup-5m` (keyed by `thing_id`). |
| `thing-merge-1h` / `thing-merge-1d` / `thing-merge-1mo` | Per-Thing merges (three cadences of `thing_rollup_merge.go`). |
| `metrics-rollup` | Aggregates device fleet status / OS and agent action volume into `metric_rollup_1h` hourly. |

### Ops metrics (per-Thing telemetry)

| Job ID | Purpose |
|---|---|
| `ops-rollup-1h` | Aggregates `metric_ops_raw` into `metric_ops_rollup_1h` every 5 minutes. |
| `ops-rollup-1d` / `ops-rollup-1mo` | Two cadences of `ops_rollup_cascade.go` — daily and monthly cascades. |
| `ops-retention` | Purges aged `metric_ops_raw` / `metric_ops_rollup_{1h,1d,1mo}` / `thing_diag_event` rows. |

### Retention / cleanup

| Job ID | Purpose |
|---|---|
| `data-retention` | Deletes audit and rollup rows older than the configured retention period daily. |
| `job-retention` | Prunes `job_run` rows, keeping the N most recent runs per job. |

### Compliance / exemptions / kill-switch passthrough

| Job ID | Purpose |
|---|---|
| `exemption-gc` | Prunes expired entries from the compliance-proxy `active_exemptions` template. |
| `cache-quality-monitor` | Detects elevated error rates in `wirerewrite`-modified requests over 30 min; auto-reverts rules to dry-run. |
| `passthrough.expiry` | Auto-reverts kill-switch passthrough at expiry (binding via E48). **Note: ID uses a dot, not a dash** — kept for historical compatibility with admin shadow keys. |

### Hub-level

| Job ID | Purpose |
|---|---|
| `smart-group-recompute` | E52-S2 — re-evaluates each smart DeviceGroup's `membership_query` against the current device fleet. |
| `user-identity-enrichment` | Backfills user identity fields into recent `traffic_event` rows via IAM lookups. |
| `siem-bridge` | Polls `traffic_event` + `AdminAuditLog` for new rows, classifies, forwards to the configured SIEM sink. |

### Semantic cache (E61)

| Job ID | Purpose |
|---|---|
| `semantic-cache-reindex` | E61-S3d — blue/green Valkey vector index swap when the embedding model fingerprint changes. Creates the new `FT` index, drops the old one, stamps an audit row. No-op when fingerprints already match. |

### Runtime engines (registered alongside scheduled jobs)

| Engine | Purpose |
|---|---|
| `alerteval` runtime | The MQ-streaming alert rule evaluator (`packages/nexus-hub/internal/alerts/eval/`). Registered via `sched.Register(eng)` so it participates in the same lifecycle / metrics surface as cron jobs. |

This produces roughly 45 cron-scheduled job IDs (37 const-defined under `packages/nexus-hub/internal/jobs/defs/<category>/` plus 8 cadence variants from `rollup_merge.go`, `thing_rollup_merge.go`, and `ops_rollup_cascade.go`) plus the `alerteval` runtime. The CI script `scripts/check-jobs-catalogue.sh` enforces that every const-defined `JobID` appears as a row above.

Adding a new job: extend this table in the same PR. Adding a new cadence variant from a multi-cadence helper (`rollup_merge`, `thing_rollup_merge`, `ops_rollup_cascade`): add the new ID to its category table.

## 6. Job ↔ alert ↔ audit chain

Jobs feed the alerting system through their Prometheus counters / gauges (§4); the alert rules in `packages/nexus-hub/internal/alerts/engine/rules/builtin.go` watch those metrics and raise alert rows when thresholds are crossed.

There is no universal `system:<job>.started / .completed / .failed` audit-event convention emitted by every job today; jobs that mutate audited resources (e.g., the `passthrough.expiry` auto-revert) are individually responsible for emitting the appropriate domain audit event.

## 7. The passthrough auto-revert loop (binding via E48)

`passthrough.expiry` (JobID with a literal dot — see `packages/nexus-hub/internal/jobs/defs/expiry/passthrough_expiry.go`) is the **mandatory auto-revert mechanism** for the emergency-passthrough flow:

- Runs on a cron schedule (deployment-tunable from `cmd/nexus-hub/main.go`; cluster default is ~60s).
- Scans the `gateway_passthrough` shadow state on each affected ai-gateway Thing:
  - If the configured `expiresAt` has passed: revert to the safe default (`bypassHooks=false`, etc.), push the new state via the standard config-change signal, and emit `system:passthrough_expiry_revert` audit.
  - If active and within window: no-op.
- The "Hub reconciles every 60 s" promise in `multi-endpoint-coordination-architecture.md` §4 is this job.

There is no separate `kill_switch.reconcile` JobID — the agent / compliance-proxy kill switch is a Cat A inline state on the shadow (`interception.Killswitch{Enabled bool}`) and is driven by the standard change-signal path, not a recurring reconcile job. Memory `project_e48_emergency_passthrough` records the end-to-end verification.

## 8. Failure modes

| Failure | Behaviour |
|---|---|
| Job handler panics | Scheduler recovers + logs; metric increments. Next tick attempts. |
| Job runs over `MaxDuration` | Logged; metric increments. Singleton lock is released by transaction commit even on timeout (so next tick can attempt). |
| Postgres advisory lock unavailable | Skip + metric. Investigate Postgres health. |
| Job tries to update something a service is also writing | Use the appropriate Postgres isolation level; document in the job. |
| Hub restart mid-job | Job is interrupted; next tick attempts. Resumable jobs continue from cursor. |

## 9. Adding a new job

Checklist:

1. Define the handler in `packages/nexus-hub/internal/jobs/defs/<category>/<job_name>.go`.
2. Register in `Init()` with cron spec + singleton + max duration.
3. Add to §5 catalogue above.
4. Wire metrics (auto-instrumented if using the standard `JobRunner` wrapper).
5. Add alert rules on failure + stall + duration.
6. Add audit emits on start / complete / fail.
7. Document idempotency assumptions in the handler comment.
8. Smoke test: trigger manually via admin endpoint (if exposed) or test harness.

## 10. Known limitations (open-source readiness)

- **Single-instance Hub scheduler.** A single Hub instance owns the scheduler today (§1). There is no leader election; running two Hub binaries against the same DB would race on every job. Multi-instance scheduling is feasible (advisory-lock-based leader election wrapping the scheduler) but not implemented. Operators running this code at scale need to plan for: (a) graceful Hub restart playbooks (next tick resumes), (b) single-instance failover via process supervisors, or (c) wait for the leader-election work to land.
- **Job-catalogue authority lives in code, not the doc.** The §5 catalogue is enforced lockstep with `packages/nexus-hub/internal/jobs/` via `scripts/check-jobs-catalogue.sh`, but the catalogue itself does not record actual cron specs (those are passed at construction time from `cmd/nexus-hub/main.go` and are deployment-tunable). To learn a specific job's interval, read `main.go`. Open-source consumers tuning a job for their environment should expect to edit `main.go`, not a config file.

## 11. Sources

- `packages/nexus-hub/internal/jobs/scheduler/` — cron parser, tick loop, singleton advisory-lock acquisition, and per-job metrics wrap (`scheduler.go`).
- `packages/nexus-hub/internal/jobs/defs/<category>/<job>.go` — JobID const + handler implementation per job.
- `packages/nexus-hub/internal/jobs/store/` — `job_run` table accessors for resumable cursors.
- `packages/shared/audit/` — audit emission.
- `tools/db-migrate/schema.prisma` — any per-job state tables (`job_run` + resumable cursors).

## 12. Cross-references

- `alerting-architecture.md` — job-status alert rules.
- `audit-pipeline-architecture.md` — `system:*` audit events.
- `multi-endpoint-coordination-architecture.md` §4 — kill-switch reconcile.
- `credentials-architecture.md` — provider.health_rollup feeds credential health.
- `tenancy-architecture.md` — retention can become per-tenant in the future.
