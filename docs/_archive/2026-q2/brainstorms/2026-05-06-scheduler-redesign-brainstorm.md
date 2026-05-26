# Hub Scheduler — Pool-Deadlock Fix & Redesign Brainstorm

> Status: Brainstorm — captures the design decisions for E39
> Owner: Hub team
> Date: 2026-05-06
> Purpose: Document the root-cause analysis and the chosen architecture
> for the Hub scheduler rewrite. Read this in full before drafting the
> SDD or starting code.

## 0. The incident that triggered this

On 2026-05-06 the running Hub became unresponsive on `/readyz` while
`/healthz` still returned 200. Diagnosis:

- `pgxpool` 100% saturated (`acquired=20 / max=20`, `acquire_count`
  flat for 10+ minutes — no conn turnover).
- 14 connections idle on `SELECT pg_try_advisory_lock($1)` for 1h24m
  per `pg_stat_activity`. State `idle / ClientRead`. Not a long
  transaction — these are sessions where the Go client never returned
  the conn to the pool.
- All 14 conns started within the same second window: the moment the
  Hub booted and ~14 of its 24 scheduled jobs all fired their first
  tick simultaneously.

The pool exhaustion was already partially diagnosed in commit
`c149fbc2` (May 6, 13:20 — "prevent Hub stall from materializer
retry storm + add pgxpool watchdog"). That commit added a 30-second
watchdog goroutine that emits a Warn line when `acquired/max >= 70%`.
Today we saw the watchdog working — it correctly surfaced the stall —
but the underlying scheduler defect is unrelated to the materializer
retry storm `c149fbc2` was fixing.

## 1. Root cause — architectural, not local

Hub registers ~24 scheduled jobs. Each registration spawns its own
ticker + goroutine in `internal/scheduler/scheduler.go`:

```go
for _, entry := range s.jobs {
    go s.runLoop(ctx, entry)   // 24 goroutines, each with its own ticker
}
```

Each `runJob` invocation:

```go
acquired, unlock, err := s.tryAdvisoryLock(ctx, jobID)
defer unlock()                  // releases lock + conn at function end
err := entry.job.Run(ctx)       // ← lock held for entire Run() duration
```

`tryAdvisoryLock` keeps a pool conn checked out for the **entire
duration** of `Run(ctx)`. That conn is what holds the per-job
session-level advisory lock; releasing the conn drops the lock.

**Self-deadlock condition**: every executing job holds 1 conn (lock)
and may need additional conns (the work it does inside Run). For
N concurrent jobs running, the pool sees ≥N conns held for lock plus
M conns for work, so the implicit constraint is
`pool_size > N + M`. With `pool_size = 20` and ~14 jobs all firing at
boot, the pool is exhausted before any job's work query can acquire a
conn. Some jobs end up waiting forever for a conn that another stuck
job will never release. The watchdog observes the symptom (pool at
100%) but does not break the loop.

This is **structural** — there is no "specific job that hangs". The
entire design assumes the pool can absorb `2 × max_concurrent_jobs`
connections, an invariant nothing in code enforces.

## 2. Industry survey

The pattern of "lock on acquire, not on execute" is universal across
mature schedulers. Brief comparison:

| System | Decision making | Multi-instance coord | Execution |
|---|---|---|---|
| **Quartz Clustered** (Java) | Single SchedulerThread | Row-level lock on `QRTZ_LOCKS` only during trigger acquisition, NOT during job run | ThreadPool (default 10) |
| **Sidekiq Cron** (Ruby/Redis) | Single poller process | Redis SET NX during enqueue only | Worker thread pool |
| **pg_cron** (PG extension) | Single background worker | N/A — one DB, one worker | per-job child process |
| **K8s controller-manager** | Leader-elected single instance | `coordination.k8s.io/Lease` (15s TTL) | Single instance handles all controllers |
| **Linux cron / SystemD timer** | Single daemon | N/A | fork-per-job |
| **Airflow Scheduler** | Single (with optional HA) | DB row-level claim | Celery / K8s workers |

**Five universal patterns**:

1. Decision making is single-threaded (per-process or per-leader).
2. Execution is fanned out to a thread / worker pool.
3. Locks/leases are short-lived; held only at "decide-to-run" moment.
4. Per-job hard timeout is mandatory.
5. Concurrency limit comes from a bounded worker pool, not unbounded
   spawning.

Hub's current design violates **all five**.

## 3. Our actual constraints

| Factor | Reality |
|---|---|
| Hub instance count | Single instance (pre-GA, per CLAUDE.md) |
| Future HA need | Hypothetical, not on near roadmap |
| Job count | ~24 housekeeping jobs (rollups, retention, drift, alerting) |
| Job duration | Mostly milliseconds; a few rollups can run 30s+ |
| Existing dependencies | Postgres (busy), Redis (already wired), `internal/jobstore` (history) |
| Job interface | `Job{ID(), Name(), Description(), Interval(), Run(ctx)}` — 24 implementations |
| Configuration | `cfg.Scheduler.Enabled` already exists in YAML |

**`cfg.Scheduler.Enabled` is the key**. Operators already designate
which Hub instance runs the scheduler via this config flag. There
is no need for runtime leader election — one Hub has it `true`,
others (in any future multi-instance topology) have it `false`.

This collapses several layers of the proposed design:

- ❌ Redis lease + heartbeat — not needed
- ❌ `internal/leadership/` package — not needed
- ❌ `skipIfNotLeader` wrapper — not needed
- ✅ `cfg.Scheduler.Enabled` config flag — already there

## 4. Library evaluation — Go ecosystem

| Library | Stars | Verdict |
|---|---|---|
| `robfig/cron` v3 | 14k | **Chosen.** Battle-tested (9 years). `JobWrapper` middleware chain is the right abstraction for our needs. Built-in `SkipIfStillRunning` (per-job singleton) and `Recover` (panic safety) cover our two top requirements. Adding leader election later is a single wrapper. |
| `go-co-op/gocron` v2 | 5k | Strong alternative. Built-in `DistributedLocker` is overkill for our single-instance topology. |
| `hibiken/asynq` | 9k | Overkill — full Redis-backed task queue. |
| `riverqueue/river` | 5k | Heavy schema migration (river_job, river_leader tables). Designed for high-throughput task queue. |
| Hand-rolled (~200 lines) | — | Tempting but reinvents `robfig`'s decade of bug fixes. |

We pick **robfig/cron v3** for: maturity, simplicity, ecosystem
familiarity, and the wrapper composition pattern that lets us add
new policies without changing job registrations.

## 5. Chosen architecture

```
nexus-hub.dev.yaml
─────────────────────────────────────────
scheduler:
  enabled: true        ← one Hub: true; others: false
  ...                   ← per-job intervals already here
─────────────────────────────────────────


cmd/nexus-hub/main.go
─────────────────────────────────────────
if cfg.Scheduler.Enabled {
    sched := scheduler.New(logger).
        WithJobStore(jobStore).
        WithReplicaID(cfg.Hub.ID)

    sched.Register(driftJob)        // 24× Register, same as today
    sched.Register(...)
    ...

    sched.SyncDefinitions(ctx)      // existing semantics preserved
    sched.Start()                   // internally: cron.Start()
    defer sched.Stop()              // internally: cron.Stop() + drain
}
─────────────────────────────────────────


internal/scheduler/scheduler.go (rewritten)
─────────────────────────────────────────
Internals replaced:
  ❌ DELETE: per-job goroutines + tickers
  ❌ DELETE: tryAdvisoryLock + pool dependency
  ❌ DELETE: WithPool() method
  ✅ KEEP:   Scheduler type, Register, Start, Stop, Trigger,
             SetEnabled, ListJobs, GetJob, ListRuns API surface
             (admin handlers still work unchanged)

  Cron engine:
    cron.New(cron.WithChain(
      cron.SkipIfStillRunning(slogAdapter),  // per-job singleton
      cron.Recover(slogAdapter),             // panic guard
    ))

  Each registered job becomes one cron.AddFunc("@every <interval>", ...).
  Inside the wrapper:
    ctx, cancel := context.WithTimeout(scheduler ctx, MaxRunDuration())
    defer cancel()
    err := job.Run(ctx)

  Hard timeout per job: defaults to max(interval, 60s); jobs with
  long expected runtime (rollups, retention) can implement an
  optional MaxRunDurationer interface to override.
─────────────────────────────────────────
```

## 6. Public API preserved

The `Scheduler` type's exported methods stay identical so `admin_jobs`
handlers, runtime introspection, and tests keep working without
changes:

- `Register(Job)` — same signature
- `WithJobStore(*jobstore.Store)` — same
- `WithReplicaID(string)` — same
- `SyncDefinitions(ctx) error` — same; still upserts `job` table rows
- `Start()` — same; internally starts the cron engine
- `Stop()` — same; internally calls cron.Stop() and drains
- `Trigger(ctx, id) error` — same; runs the job immediately, bypasses
  enabled flag (manual-trigger semantics from D15)
- `SetEnabled(ctx, id, bool) error` — same; flips DB row + adds/removes
  the job from the cron entry table
- `ListJobs(ctx) ([]JobStatus, error)` — same shape; NextRun is sourced
  from `cron.Entry.Next` instead of computed from LastRun + Interval
- `GetJob(ctx, id) (JobStatus, error)` — same
- `ListRuns(ctx, id, limit, offset) ([]JobRun, error)` — same; backed
  by the existing jobstore

`WithPool(*pgxpool.Pool)` is **deleted** — the new scheduler does not
need a pool reference (no advisory lock). Compile error in `main.go`
flags any caller that wasn't updated.

## 7. Job interface evolution

```go
// Existing (preserved)
type Job interface {
    ID() string
    Name() string
    Description() string
    Interval() time.Duration
    Run(ctx context.Context) error
}

// Existing (preserved)
type OnStartRunner interface {
    RunOnStart() bool
}

// New (optional — sane defaults if absent)
type MaxRunDurationer interface {
    MaxRunDuration() time.Duration
}
```

Default per-job timeout: `max(Interval, 60s)`. Job authors with
genuinely long-running work (rollups that scan multi-month windows)
implement `MaxRunDuration()` and return a higher value. **No code
change needed for the 24 existing jobs unless one of them is
expected to legitimately exceed `max(Interval, 60s)`.**

## 8. Risk matrix

| Risk | Probability | Impact | Mitigation |
|---|---|---|---|
| Cron `@every` semantic differs subtly from `time.Ticker` | Low | Low | Both fire at constant intervals from start; verify with integration test |
| `SkipIfStillRunning` skips a long-running rollup that we wanted to run anyway | Low | Low | `MaxRunDuration` keeps slow jobs bounded; if a rollup takes longer than its interval we have a different bug |
| `cron.Stop()` deadlock on shutdown if a job is wedged | Low | Medium | Wrap with 30s timeout; force exit if drain doesn't complete |
| Admin UI shows stale `NextRun` after disable/enable | Low | Low | Re-add cron entry on enable; recompute Next from cron entry table |
| New library dep widens supply chain | Low | Low | robfig is widely used, MIT, no transitive deps beyond stdlib |

## 9. Migration path

This is a single PR; no phased rollout:

1. Add `github.com/robfig/cron/v3` to `packages/nexus-hub/go.mod`.
2. Replace `internal/scheduler/scheduler.go` body. Keep test file
   compatible by adapting expectations.
3. Delete `tryAdvisoryLock` and `WithPool` (compile-error pulls
   callers).
4. Add `MaxRunDurationer` interface; document defaults.
5. Update `nexus-hub.dev.yaml` only if `Scheduler.Enabled` field
   default needs review (it's already there).
6. Restart Hub. Watch `pg_stat_activity`: idle conns on
   `pg_try_advisory_lock` should be **zero**; `acquire_count` should
   advance steadily.

## 10. Explicitly out of scope

- **Multi-instance leader election** — `cfg.Scheduler.Enabled`
  selects the scheduler instance at deploy time. Add wrapper if/when
  HA failover becomes a real requirement.
- **Cron-expression syntax** — all 24 jobs use simple intervals;
  `@every <interval>` is enough.
- **Persistent next-run-at column** — Hub restart re-computes from
  `LastRun + Interval` (or "now" for missing LastRun). Idempotent
  housekeeping jobs tolerate one extra run.
- **Worker pool tuning** — robfig spawns one goroutine per job
  invocation. With ~24 jobs and short execution windows, this is
  unbounded but bounded in practice. If we ever scale to 100s of jobs
  or per-tick concurrency becomes a concern, we add a semaphore
  wrapper.
- **Hot-reload of intervals** — interval changes require a Hub
  restart. Re-registering on the fly is possible but not in scope.

## 11. Open decisions (none — all resolved during discussion)

- ✅ Library: robfig/cron v3
- ✅ Multi-instance: config flag, no runtime election
- ✅ Per-job timeout: `max(Interval, 60s)`, optional override
- ✅ Public API: preserved (Register, Start, Stop, Trigger, etc.)
- ✅ Pool dependency: removed
- ✅ Advisory lock: removed entirely

## 12. Hand-off notes

This brainstorm is the spec for **e39-s1**. Implementation order:

1. Read this doc.
2. Read `docs/dev/architecture.md` § "Hub Scheduler" (added
   alongside this brainstorm in the same change).
3. Read `docs/sdd/e39-s1-scheduler-cron-coordinator.md` for the
   per-file change list.
4. Implement; restart; verify.
