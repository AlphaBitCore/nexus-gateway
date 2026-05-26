# E39 — Hub Scheduler Redesign (pgxpool deadlock fix)

## Background

On 2026-05-06 the running Hub became unresponsive on `/readyz` while
`/healthz` still returned 200. Diagnosis: the pgxpool reached 100%
saturation with 14 connections idle on `pg_try_advisory_lock` for
1h24m. None of the conns were ever released back to the pool.

Root cause: the `internal/scheduler/scheduler.go` design holds one
pool conn per running job for the entire duration of `Job.Run(ctx)`
(the conn carries the session-level advisory lock). With 24 jobs
firing their first tick concurrently at boot, ~14 jobs each held a
conn waiting on shared work conns that were also held by other
locked jobs. Self-deadlock.

The watchdog added in commit `c149fbc2` correctly surfaced the stall
(it logs warnings on `acquired/max >= 70%`), but the underlying
scheduler defect is unrelated to the materializer retry storm that
commit was fixing.

This epic replaces the scheduler with a design that:

- Holds zero pool conns from the scheduler itself (jobs acquire
  conns only for their own work, releasing immediately).
- Eliminates the per-job advisory lock (which was the conn-leak
  vector).
- Preserves the public `Scheduler` API so admin handlers continue
  to work without changes.

## Glossary

- **Scheduler** — the in-process component that decides when each
  registered `Job` runs and dispatches it.
- **Job** — a unit of scheduled housekeeping work (drift detector,
  retention sweep, metric rollup, alert evaluator, …). Implements
  `internal/scheduler.Job` interface.
- **OnStartRunner** — optional `Job` capability: run once immediately
  on scheduler start, then resume normal cadence. Used by long-
  interval jobs (rollups, retention) so the first pass is not
  delayed by hours.
- **MaxRunDurationer** — new optional `Job` capability: declare a
  per-run hard timeout other than the default `max(Interval, 60s)`.
- **`cfg.Scheduler.Enabled`** — single flag in YAML that selects
  whether this Hub instance runs the scheduler. In a multi-instance
  deployment, exactly one Hub sets it `true`.
- **Cron entry** — a registration in the underlying
  `github.com/robfig/cron/v3` engine, identified by `cron.EntryID`.

## Personas

- **Hub operator (DevOps)** — writes the YAML config, picks which
  instance runs the scheduler. Cares about: "Hub doesn't lock up."
- **Admin (UI user)** — sees scheduled jobs in the admin UI, can
  toggle enabled, manually trigger, browse run history. Cares
  about: "the UI shows accurate next-run times."
- **Hub engineer** — adds new scheduled jobs by implementing the
  `Job` interface and calling `sched.Register`. Cares about: "I
  don't have to think about advisory locks or pool sizing."

## Functional Requirements

### F1. Pool-deadlock immunity (must)

- F1.1 — The scheduler MUST NOT hold any pgxpool connection across
  the duration of `Job.Run(ctx)`. Pool conns held by the scheduler
  during a job's execution: **zero**.
- F1.2 — The scheduler MUST NOT use `pg_try_advisory_lock` or any
  other long-held DB-level lock. The only persistent state the
  scheduler reads from PG is row data via `internal/jobstore` (job
  metadata, run history) — short transactions only.
- F1.3 — A job whose `Run(ctx)` panics MUST NOT crash the scheduler.
  Other registered jobs continue to run on schedule.
- F1.4 — A job whose `Run(ctx)` exceeds its `MaxRunDuration` MUST be
  cancelled via `ctx.Done()`. The default `MaxRunDuration` is
  `max(Interval, 60s)`; jobs may override via `MaxRunDurationer`.
- F1.5 — A job whose previous run is still in progress MUST be
  skipped on the next tick (per-job singleton; "this job is busy").
  Skipping MUST emit a structured log line at WARN.

### F2. Public API preservation (must)

The exported surface of `*scheduler.Scheduler` MUST remain
backward-compatible so `admin_jobs` handler, runtime introspection,
and existing callers continue to work without changes:

- F2.1 — `Register(Job)` — same signature.
- F2.2 — `WithJobStore(*jobstore.Store)` — same.
- F2.3 — `WithReplicaID(string)` — same.
- F2.4 — `SyncDefinitions(ctx) error` — same; still upserts the
  `job` table and seeds the in-memory enabled flag.
- F2.5 — `Start()` / `Stop()` — same; internally start/stop the
  cron engine.
- F2.6 — `Trigger(ctx, id) error` — same; runs the job immediately,
  bypassing the enabled flag (manual-trigger semantics from D15).
- F2.7 — `SetEnabled(ctx, id, bool) error` — same; flips the DB
  enabled flag and atomically adds/removes the cron entry.
- F2.8 — `ListJobs(ctx) ([]JobStatus, error)` — same shape; `NextRun`
  is sourced from `cron.Entry.Next` (more accurate than the
  prior "LastRun + Interval" estimate).
- F2.9 — `GetJob(ctx, id) (JobStatus, error)` — same.
- F2.10 — `ListRuns(ctx, id, limit, offset) ([]JobRun, error)` —
  same; backed by the existing jobstore.

`WithPool(*pgxpool.Pool)` is **removed**. Compile errors flag any
caller that wasn't updated; the only caller is
`cmd/nexus-hub/main.go` which is updated in the same change.

### F3. Single-instance designation via config (must)

- F3.1 — `cfg.Scheduler.Enabled` (existing field) controls whether
  this Hub runs scheduled jobs. When `false`, no scheduler is
  constructed and no jobs are registered.
- F3.2 — When `true`, the scheduler runs all registered jobs. There
  is no runtime leader election; if two instances are deployed both
  with `enabled: true`, both will run the jobs (not desired but not
  catastrophic — housekeeping jobs are idempotent).

### F4. Job interface evolution (must)

- F4.1 — Existing `Job` interface (5 methods) is preserved.
- F4.2 — Existing `OnStartRunner` optional interface is preserved.
- F4.3 — New optional `MaxRunDurationer` interface allows a Job to
  override the default `max(Interval, 60s)` per-run timeout. Existing
  24 Job implementations DO NOT need changes.

### F5. Observability (must)

- F5.1 — The scheduler MUST log at INFO when it starts (with job
  count) and stops.
- F5.2 — Each job run MUST log at DEBUG on success (with duration)
  and at ERROR on failure (with duration + error message).
- F5.3 — A skipped tick (singleton conflict) MUST log at WARN with
  the job ID.
- F5.4 — A run that hit its MaxRunDuration MUST log at WARN with the
  job ID and the configured duration.
- F5.5 — `job_runs` table continues to record every started+finished
  run (start time, finish time, duration, status, error). On hard
  timeout the run is recorded as `status=error` with
  `error="exceeded MaxRunDuration"`.

## Non-Functional Requirements

### NFR-Performance

- N1 — Scheduler tick overhead per job: O(1). robfig/cron's internal
  heap-based dispatcher handles 24 jobs with negligible CPU.
- N2 — Per-job invocation overhead (wrapper chain): < 1ms.
- N3 — `Stop()` MUST drain in-flight jobs within 30s; if any jobs
  exceed the drain deadline, `Stop()` returns and emits a WARN
  listing the jobs that did not complete.

### NFR-Reliability

- N4 — A pool exhaustion event in any other Hub subsystem MUST NOT
  cascade into scheduler stalls. The scheduler does not depend on
  pool health for its own dispatch loop.
- N5 — A Hub crash mid-job leaves no stuck advisory locks (there
  are none). On restart the scheduler resumes normally.
- N6 — Restart correctness: jobs run correctly after a Hub restart.
  `OnStartRunner` jobs run their initial pass; others wait their
  `Interval` from start. (Trade-off: a Hub that restarts every 5
  min would never fire 1-hour interval jobs. Accepted; not a real
  scenario.)

### NFR-Observability

- N7 — Same Prometheus metrics surface as today: per-job last-run
  status / duration via the existing opsmetrics counters where
  registered. No new metrics introduced by E39.

### NFR-Compatibility

- N8 — Pre-GA per CLAUDE.md "no backward compatibility" rule. The
  rewrite is a single PR; no migration code, no feature flags, no
  parallel old/new paths.
- N9 — Job authors writing new Jobs do not need to learn cron syntax.
  The `Interval()` method (returns `time.Duration`) is the only
  scheduling primitive they touch. The scheduler internally maps it
  to `@every <duration>` for the cron engine.

## Constraints & Assumptions

- **C1** — Hub deployment topology is single-instance today; YAML
  config selects "the scheduler instance".
- **C2** — `github.com/robfig/cron/v3` is acceptable as a new
  dependency. MIT-licensed, mature (~9 years), 14k stars, no
  transitive deps beyond stdlib.
- **C3** — Existing `internal/jobstore` schema (`job`, `job_runs`
  tables) is unchanged. The scheduler still calls `UpsertJob`,
  `StartRun`, `FinishRun`, `ListRuns`, etc.
- **C4** — Job intervals stay as `time.Duration`. We do NOT introduce
  cron-expression syntax; `@every <duration>` covers all 24 jobs.
- **C5** — Drain timeout on `Stop()` is hard-coded at 30s. Operators
  cannot configure it; it bounds the SIGTERM-to-exit window.

## MoSCoW Priority

| Req | Priority | Notes |
|---|---|---|
| F1 — Pool-deadlock immunity | Must | Reason for the entire epic. |
| F2 — Public API preservation | Must | Otherwise admin UI / runtime introspect break. |
| F3 — Config-driven single-instance | Must | Replaces advisory_lock-based coordination. |
| F4 — Job interface evolution | Must | Adds `MaxRunDurationer` for safety. |
| F5 — Observability | Must | Operator visibility on skip / timeout / error events. |
| NFR-Performance | Must | < 1ms wrapper overhead, 30s shutdown drain. |
| NFR-Reliability | Must | Decouple from pool health. |
| NFR-Observability | Should | Keep existing Prometheus surface. |
| NFR-Compatibility | Must | No migration code (CLAUDE.md). |

## Out of scope

- **Multi-instance leader election** — `cfg.Scheduler.Enabled` is the
  designation mechanism.
- **Cron-expression syntax** — `@every <duration>` is sufficient.
- **Worker pool tuning** — robfig spawns one goroutine per job
  invocation; with ~24 jobs and short executions this is bounded in
  practice.
- **Hot reload of intervals at runtime** — interval changes need a
  Hub restart.
- **Job priorities or dependencies** — none of our 24 jobs need them.
- **Per-job concurrency limits** — `SkipIfStillRunning` is enough
  (singleton). If we ever need "max N runs of this job in flight"
  semantics, that is a separate epic.
