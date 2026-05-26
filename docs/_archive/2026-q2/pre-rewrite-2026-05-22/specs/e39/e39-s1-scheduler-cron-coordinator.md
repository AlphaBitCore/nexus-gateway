# E39-S1 — Scheduler Cron Coordinator (replace advisory-lock per-job goroutines)

Status: draft
Epic: E39 — Hub Scheduler Redesign
Story: S1 — Cron-based single-coordinator scheduler

## 1. Problem

`packages/nexus-hub/internal/jobs/scheduler/scheduler.go` spawns one
goroutine + `time.Ticker` per registered job (24 of them at boot)
and per tick:

1. `pool.Acquire()` to obtain a conn for `pg_try_advisory_lock(jobID)`.
2. Holds that conn for the entire duration of `Job.Run(ctx)`.
3. `Run` itself often does `pool.Acquire()` again for its own queries.

With pool size 20 and ~14 jobs firing simultaneously at boot, each
holding 1 conn for the lock and needing additional conns for work,
the pool is exhausted before any job can complete; every conn is
held by some stuck waiter; the pool never recovers.

## 2. Goal

Replace the implementation with a `github.com/robfig/cron/v3`-based
single-coordinator design:

- One `*cron.Cron` engine drives all jobs.
- Per-tick wrapper chain (`SkipIfStillRunning`, `Recover`,
  per-run `MaxRunDuration` timeout).
- The scheduler holds **zero** pool connections; each job acquires
  conns inside its own `Run(ctx)` only.
- Public API of `*scheduler.Scheduler` preserved so admin UI,
  runtime introspection, and tests are unaffected.

## 3. Non-goals

- Multi-instance leader election (handled via `cfg.Scheduler.Enabled`).
- Cron-expression syntax (`@every <Interval>` is sufficient).
- Worker-pool concurrency limits (robfig's per-tick goroutine is OK
  at our scale).
- Hot reload of intervals at runtime.

## 4. Design

### 4.1 New dependency

```bash
go get github.com/robfig/cron/v3
```

Adds one direct dep with no transitive deps beyond stdlib. License: MIT.

### 4.2 Job interface (`internal/scheduler/scheduler.go`)

Existing interfaces preserved verbatim:

```go
type Job interface {
    ID() string
    Name() string
    Description() string
    Interval() time.Duration
    Run(ctx context.Context) error
}

type OnStartRunner interface {
    RunOnStart() bool
}
```

New optional interface:

```go
// MaxRunDurationer is implemented by Jobs that need a per-run timeout
// other than the scheduler's default of max(Interval, 60s). Used by
// jobs that legitimately take longer than their interval (e.g. a
// daily rollup that scans a multi-month window).
type MaxRunDurationer interface {
    MaxRunDuration() time.Duration
}
```

### 4.3 Scheduler struct

```go
type Scheduler struct {
    mu        sync.RWMutex
    jobs      map[string]*entry
    cron      *cron.Cron
    cronCtx   context.Context     // base ctx for all job runs
    cancel    context.CancelFunc
    started   atomic.Bool
    logger    *slog.Logger
    js        *jobstore.Store
    replicaID string
}

type entry struct {
    job          Job
    enabled      atomic.Bool
    cronEntryID  cron.EntryID     // 0 when not registered with cron (disabled)
    statusMu     sync.Mutex
    status       JobStatus        // mirrors prior shape
}
```

Note: `pool` field is deleted. `WithPool(*pgxpool.Pool)` method is
deleted. `tryAdvisoryLock`, the `unlock` closure pattern, and the
`hash/fnv` import are deleted.

### 4.4 Wrapper chain

`Start()` constructs the cron engine with two wrappers:

```go
s.cron = cron.New(cron.WithChain(
    cron.SkipIfStillRunning(slogAdapter{s.logger}),  // per-job singleton
    cron.Recover(slogAdapter{s.logger}),             // panic safety
))
```

Both come from `github.com/robfig/cron/v3` directly. We do NOT add a
custom leader-check wrapper today (config flag handles single-
instance designation).

### 4.5 Job registration with cron

After `Start()` for each enabled entry:

```go
spec := fmt.Sprintf("@every %s", entry.job.Interval())
entryID, err := s.cron.AddFunc(spec, func() {
    s.runOne(entry, false)
})
entry.cronEntryID = entryID
```

`@every <duration>` is a robfig native syntax that fires at fixed
intervals (no cron-style alignment to wall clock). Interval is the
job's `Interval() time.Duration`.

### 4.6 `runOne` — the per-tick body

```go
func (s *Scheduler) runOne(e *entry, manual bool) {
    if !manual && !e.enabled.Load() {
        return  // disabled at runtime; defensive (cron entry would have been removed)
    }

    timeout := defaultTimeout(e.job)
    if mrd, ok := e.job.(MaxRunDurationer); ok {
        if v := mrd.MaxRunDuration(); v > 0 {
            timeout = v
        }
    }

    ctx, cancel := context.WithTimeout(s.cronCtx, timeout)
    defer cancel()

    var runID string
    if s.js != nil {
        if id, err := s.js.StartRun(ctx, e.job.ID(), s.replicaID); err == nil {
            runID = id
        } else {
            s.logger.Error("jobstore start_run failed", "job", e.job.ID(), "error", err)
        }
    }

    e.setStatusRunning()

    start := time.Now()
    err := e.job.Run(ctx)
    dur := time.Since(start)

    status, errMsg := "success", ""
    if err != nil {
        status = "error"
        errMsg = err.Error()
        if errors.Is(err, context.DeadlineExceeded) {
            s.logger.Warn("job exceeded MaxRunDuration",
                "job", e.job.ID(), "duration", dur, "max", timeout)
        } else {
            s.logger.Error("job failed",
                "job", e.job.ID(), "duration", dur, "error", err)
        }
    } else {
        s.logger.Debug("job completed", "job", e.job.ID(), "duration", dur)
    }

    if s.js != nil && runID != "" {
        if ferr := s.js.FinishRun(ctx, runID, status, dur, errMsg); ferr != nil {
            s.logger.Error("jobstore finish_run failed",
                "job", e.job.ID(), "run_id", runID, "error", ferr)
        }
    }

    e.recordCompletion(status, errMsg, dur)
}

func defaultTimeout(j Job) time.Duration {
    iv := j.Interval()
    if iv < 60*time.Second {
        return 60 * time.Second
    }
    return iv
}
```

### 4.7 Start / Stop semantics

```go
func (s *Scheduler) Start() {
    s.cronCtx, s.cancel = context.WithCancel(context.Background())
    s.cron = cron.New(cron.WithChain(
        cron.SkipIfStillRunning(slogAdapter{s.logger}),
        cron.Recover(slogAdapter{s.logger}),
    ))

    s.mu.RLock()
    var onStartEntries []*entry
    for _, e := range s.jobs {
        if !e.enabled.Load() {
            continue
        }
        if id := s.scheduleEntry(e); id != 0 {
            e.cronEntryID = id
        }
        if r, ok := e.job.(OnStartRunner); ok && r.RunOnStart() {
            onStartEntries = append(onStartEntries, e)
        }
    }
    s.mu.RUnlock()

    s.cron.Start()
    s.started.Store(true)

    // Run OnStartRunner jobs in a detached goroutine so Start() returns
    // promptly. Each runs through the same wrapper chain via runOne.
    for _, e := range onStartEntries {
        go s.runOne(e, false)
    }

    s.logger.Info("scheduler started", "jobs", len(s.jobs))
}

func (s *Scheduler) Stop() {
    if !s.started.Swap(false) {
        return
    }

    drainCtx := s.cron.Stop()  // returns ctx.Done() when current jobs finish
    select {
    case <-drainCtx.Done():
    case <-time.After(30 * time.Second):
        s.logger.Warn("scheduler stop drain timed out; jobs still running")
    }
    if s.cancel != nil {
        s.cancel()  // signals s.cronCtx for any straggler that ignored the per-run timeout
    }
    s.logger.Info("scheduler stopped")
}
```

### 4.8 SetEnabled — add/remove cron entry on toggle

```go
func (s *Scheduler) SetEnabled(ctx context.Context, id string, enabled bool) error {
    s.mu.RLock()
    e, ok := s.jobs[id]
    s.mu.RUnlock()
    if !ok {
        return ErrJobNotFound
    }

    if s.js != nil {
        if err := s.js.SetEnabled(ctx, id, enabled); err != nil {
            if errors.Is(err, jobstore.ErrNotFound) {
                return ErrJobNotFound
            }
            return err
        }
    }
    e.enabled.Store(enabled)
    e.setStatusEnabled(enabled)

    if s.cron == nil || !s.started.Load() {
        return nil
    }

    s.mu.Lock()
    defer s.mu.Unlock()
    if enabled {
        if e.cronEntryID == 0 {
            e.cronEntryID = s.scheduleEntry(e)
        }
    } else {
        if e.cronEntryID != 0 {
            s.cron.Remove(e.cronEntryID)
            e.cronEntryID = 0
        }
    }
    return nil
}
```

### 4.9 Trigger

```go
func (s *Scheduler) Trigger(ctx context.Context, id string) error {
    s.mu.RLock()
    e, ok := s.jobs[id]
    s.mu.RUnlock()
    if !ok {
        return ErrJobNotFound
    }
    // Detached ctx: HTTP request that triggered may complete before run.
    go s.runOne(e, true /* manual */)
    return nil
}
```

The cron-engine wrapper chain does NOT apply to `Trigger` — manual
triggers explicitly bypass `SkipIfStillRunning` (admin chose to fire
this NOW). They DO get the panic guard via deferred recover inside
`runOne`. Implementation: add an inline `defer recoverPanic(...)` at
the top of `runOne` so manual triggers also get panic safety even
though they don't go through the cron wrapper chain.

### 4.10 ListJobs / GetJob / NextRun

`NextRun` is now sourced from `s.cron.Entry(entryID).Next` when the
cron is started and the entry exists; falls back to `LastRun + Interval`
estimate when not (e.g., scheduler not started yet, or job disabled).

```go
func (s *Scheduler) nextRunFor(e *entry) *time.Time {
    if s.cron == nil || e.cronEntryID == 0 {
        return nil
    }
    entry := s.cron.Entry(e.cronEntryID)
    if entry.ID == 0 {
        return nil
    }
    next := entry.Next
    return &next
}
```

### 4.11 slog → cron.Logger adapter

`cron.SkipIfStillRunning` and `cron.Recover` take a `cron.Logger`.
Write a small adapter:

```go
type slogAdapter struct{ inner *slog.Logger }

func (a slogAdapter) Info(msg string, kv ...any) {
    a.inner.Info(msg, kv...)
}
func (a slogAdapter) Error(err error, msg string, kv ...any) {
    if err != nil {
        kv = append(kv, "error", err.Error())
    }
    a.inner.Error(msg, kv...)
}
```

### 4.12 cmd/nexus-hub/main.go

The change is minimal — same `Register` calls, just remove the
`sched.WithPool(dbPool)` line:

```go
if cfg.Scheduler.Enabled {
    jobStore := jobstore.New(dbPool)
    sched = scheduler.New(logger).
        WithJobStore(jobStore).
        WithReplicaID(cfg.Hub.ID)

    // ... 24× sched.Register(...)  unchanged
    sched.SyncDefinitions(ctx)
    sched.Start()
    defer sched.Stop()
}
```

The fluent builder pattern (`New(...).WithJobStore(...).WithReplicaID(...)`)
is added to make this read better; the underlying methods are still
named the same and still return `*Scheduler`.

## 5. Tasks

- T1 — Add `github.com/robfig/cron/v3` to `packages/nexus-hub/go.mod`
  and `go.sum` via `go get`.
- T2 — Define `MaxRunDurationer` interface; document defaults.
- T3 — Rewrite `internal/scheduler/scheduler.go` body. Keep the file
  path; change internals.
- T4 — Add `slogAdapter` (cron.Logger implementation) in same package.
- T5 — Adapt `internal/scheduler/scheduler_test.go` (and any other
  test files) to the new internals — public API tests should pass
  unchanged; internal tests of advisory-lock are deleted.
- T6 — Update `cmd/nexus-hub/main.go`: drop `sched.WithPool(dbPool)`;
  optionally adopt fluent builder.
- T7 — Update `cmd/nexus-hub/main.go`: ensure `defer sched.Stop()`
  runs before the process exits so the drain logic actually fires.
- T8 — Verify build of full Hub module (`go build ./packages/nexus-hub/...`).
- T9 — Run `go test -race -count=1 ./packages/nexus-hub/internal/jobs/scheduler/...`.
- T10 — Restart the Hub locally; observe `pg_stat_activity` shows
  zero `pg_try_advisory_lock` idle conns; observe Hub log shows no
  "db pool high utilization" warnings during boot or normal operation.

## 6. Acceptance criteria

- AC1 — `git grep "tryAdvisoryLock\|pg_try_advisory_lock" packages/nexus-hub`
  returns zero hits.
- AC2 — `git grep "WithPool" packages/nexus-hub/internal/jobs/scheduler`
  returns zero hits.
- AC3 — `go build ./packages/nexus-hub/...` succeeds.
- AC4 — `go test -race -count=1 ./packages/nexus-hub/internal/jobs/scheduler/...`
  passes.
- AC5 — A live Hub running >5 minutes shows `acquired_conns / max_conns`
  rising and falling normally (pool turnover); no `db pool high
  utilization` warnings; `acquire_count` advances continuously in the
  watchdog log lines.
- AC6 — A `SELECT * FROM pg_stat_activity WHERE query LIKE
  '%advisory_lock%'` returns zero rows after Hub has been running.
- AC7 — A test that registers a job whose `Run(ctx)` panics confirms
  the scheduler keeps running and re-fires the same job on the next
  interval.
- AC8 — A test that registers a slow job (sleeps past `MaxRunDuration`)
  confirms the run is cancelled at the timeout and recorded as
  `status=error` in `job_runs`.
- AC9 — A test that registers a job with interval=200ms and Run that
  takes 600ms confirms `SkipIfStillRunning` skips the second tick
  but lets the third tick fire after the first run completes.

## 7. Risks

- **R1** — `cron.@every` semantics interact subtly with the
  `Stop()` drain. Mitigation: the AC9 test exercises the boundary.
- **R2** — Admin UI shows stale `NextRun` after `SetEnabled(false)`
  → `SetEnabled(true)`. Mitigation: re-add the cron entry on enable;
  recompute `Next` from the new entry.
- **R3** — A new dependency widens supply chain. Mitigation:
  robfig is widely used (k8s controller-runtime indirectly,
  Grafana, Prometheus operator), MIT-licensed, audited frequently.
- **R4** — Test flake on `time.Sleep`-based asserts. Mitigation:
  use longer intervals (200ms+) and `time.After(longer)` rather
  than scheduling-thread races.
