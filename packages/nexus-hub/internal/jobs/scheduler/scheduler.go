// Package scheduler provides Hub's in-process scheduler for periodic
// housekeeping jobs. It is built on github.com/robfig/cron/v3.
//
// Design: one *cron.Cron engine drives all
// registered jobs. Per-tick wrapper chain applies SkipIfStillRunning
// (per-job singleton) and Recover (panic safety). Per-run hard
// timeout via context.WithTimeout. The scheduler holds zero pgxpool
// connections of its own — the previous per-job advisory-lock design
// caused a self-deadlock at boot when concurrent jobs each held a
// conn for their lock plus needed additional conns for their work.
//
// Multi-instance designation is by configuration: cfg.Scheduler.Enabled
// selects which Hub instance runs scheduled jobs. There is no
// runtime leader election (and no need for one at our deployment
// scale).
//
// Interval floor: github.com/robfig/cron/v3's @every parser silently
// clamps sub-second intervals to 1s. Job authors needing finer
// granularity than 1s should use a different mechanism (this is not
// what cron is for). All current Hub jobs are ≥5s; the floor never
// matters in practice.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/store"
)

// Job is a unit of scheduled work.
//
// ID is the stable slug used as the PK in the `job` table. Name and
// Description are human-readable strings shown on the admin UI; keep
// Description to a single sentence.
type Job interface {
	ID() string
	Name() string
	Description() string
	Interval() time.Duration
	Run(ctx context.Context) error
}

// OnStartRunner is an optional Job capability. When RunOnStart returns
// true the scheduler executes the job once immediately after Start,
// before the first tick fires. Useful for rollup and retention jobs
// whose interval is long (5m / 1h / daily) so the first pass is not
// delayed.
type OnStartRunner interface {
	RunOnStart() bool
}

// MaxRunDurationer is an optional Job capability. When a Job
// implements it, the scheduler wraps Run(ctx) with a context.WithTimeout
// at MaxRunDuration() instead of the default max(Interval, 60s).
// Used by jobs with legitimately long execution windows (multi-month
// retention sweeps, daily rollups over large datasets).
type MaxRunDurationer interface {
	MaxRunDuration() time.Duration
}

// JobStatus tracks the runtime state of a job. When a jobstore is
// configured most fields come from DB queries in ListJobs/GetJob; the
// in-memory copy is a cache of the last observed run.
type JobStatus struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	Interval     time.Duration `json:"interval"`
	Enabled      bool          `json:"enabled"`
	LastRun      *time.Time    `json:"lastRun"`
	LastDuration time.Duration `json:"lastDuration"`
	LastStatus   string        `json:"lastStatus"` // "success", "error", "running", "skipped"
	LastError    string        `json:"lastError,omitempty"`
	NextRun      *time.Time    `json:"nextRun"`
	RunCount     int64         `json:"runCount"`
	ErrorCount   int64         `json:"errorCount"`
}

// stopDrainTimeout caps how long Stop() waits for in-flight jobs to
// finish after the cron engine has been signalled to stop.
const stopDrainTimeout = 30 * time.Second

// minDefaultRunTimeout is the floor applied to defaultTimeout(j) so
// even a job with a 5-second interval gets a generous-enough cap.
const minDefaultRunTimeout = 60 * time.Second

// jobStoreIface is the narrow surface the scheduler uses on jobstore.Store.
// Declared here so tests can inject a fake without standing up Postgres.
// *jobstore.Store satisfies every method in production.
type jobStoreIface interface {
	UpsertJob(ctx context.Context, id, name, description string, intervalSec int) error
	GetEnabled(ctx context.Context, id string) (bool, error)
	SetEnabled(ctx context.Context, id string, enabled bool) error
	StartRun(ctx context.Context, jobID, replicaID string) (string, error)
	FinishRun(ctx context.Context, runID, status string, duration time.Duration, errMsg string) error
	ListJobsWithStats(ctx context.Context) ([]jobstore.JobWithStats, error)
	GetJobWithStats(ctx context.Context, id string) (jobstore.JobWithStats, error)
	ListRuns(ctx context.Context, jobID string, limit, offset int) ([]jobstore.JobRun, int, error)
	RecoverStaleRuns(ctx context.Context) (int64, error)
}

// Scheduler manages periodic jobs.
type Scheduler struct {
	mu        sync.RWMutex
	jobs      map[string]*entry
	cron      *cron.Cron
	cronCtx   context.Context
	cancel    context.CancelFunc
	started   atomic.Bool
	logger    *slog.Logger
	js        jobStoreIface
	replicaID string
}

type entry struct {
	job         Job
	enabled     atomic.Bool
	cronEntryID cron.EntryID // 0 when not registered with cron (disabled or not yet started)

	statusMu sync.Mutex
	status   JobStatus
}

// New creates a new Scheduler.
func New(logger *slog.Logger) *Scheduler {
	return &Scheduler{
		jobs:   make(map[string]*entry),
		logger: logger.With("component", "scheduler"),
	}
}

// WithJobStore attaches a jobstore for persisting definitions and run
// history. When set, ListJobs/GetJob query the DB and every run writes
// a `job_run` row.
func (s *Scheduler) WithJobStore(js *jobstore.Store) *Scheduler {
	s.js = js
	return s
}

// WithReplicaID tags persisted run rows with the replica identity so
// the admin UI can show which instance executed a given run.
func (s *Scheduler) WithReplicaID(id string) *Scheduler {
	s.replicaID = id
	return s
}

// Register adds a job. Must be called before SyncDefinitions/Start.
// New jobs default to enabled; the stored flag in the `job` table
// wins once SyncDefinitions has run.
func (s *Scheduler) Register(j Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &entry{
		job: j,
		status: JobStatus{
			ID:          j.ID(),
			Name:        j.Name(),
			Description: j.Description(),
			Interval:    j.Interval(),
			Enabled:     true,
		},
	}
	e.enabled.Store(true)
	s.jobs[j.ID()] = e
}

// SyncDefinitions upserts every registered job's metadata into the
// `job` table and seeds the in-memory enabled flag from the persisted
// value. Safe to call multiple times; metadata (name/description/
// interval) is overwritten on every call so code changes propagate,
// but the enabled column is never touched by upsert so an admin's
// disable survives restarts.
//
// Must be called before Start() when a jobstore is attached. Missing
// jobstore is a no-op.
func (s *Scheduler) SyncDefinitions(ctx context.Context) error {
	if s.js == nil {
		return nil
	}
	s.mu.RLock()
	entries := make([]*entry, 0, len(s.jobs))
	for _, e := range s.jobs {
		entries = append(entries, e)
	}
	s.mu.RUnlock()

	for _, e := range entries {
		id := e.job.ID()
		intervalSec := int(e.job.Interval() / time.Second)
		if intervalSec < 1 {
			intervalSec = 1
		}
		if err := s.js.UpsertJob(ctx, id, e.job.Name(), e.job.Description(), intervalSec); err != nil {
			return err
		}
		enabled, err := s.js.GetEnabled(ctx, id)
		if err != nil {
			return err
		}
		e.enabled.Store(enabled)
		e.statusMu.Lock()
		e.status.Enabled = enabled
		e.statusMu.Unlock()
	}
	return nil
}

// SetEnabled flips a job's enabled flag. When a jobstore is attached
// the DB row is updated first; the in-memory atomic is only flipped
// after the DB write succeeds. When the cron engine is running the
// entry is added (on enable) or removed (on disable) so the new state
// takes effect immediately. Returns ErrJobNotFound if the id is unknown.
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
	e.statusMu.Lock()
	e.status.Enabled = enabled
	e.statusMu.Unlock()

	if !s.started.Load() {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron == nil {
		return nil
	}
	if enabled {
		if e.cronEntryID == 0 {
			id, err := s.scheduleEntry(e)
			if err != nil {
				s.logger.Error("schedule entry on enable failed",
					"job", e.job.ID(), "error", err)
				return nil // DB enable succeeded; live cron rebuild will catch up on Hub restart
			}
			e.cronEntryID = id
		}
	} else {
		if e.cronEntryID != 0 {
			s.cron.Remove(e.cronEntryID)
			e.cronEntryID = 0
		}
	}
	return nil
}

// Start launches the cron engine and registers every enabled job.
// Jobs that implement OnStartRunner.RunOnStart()=true are kicked off
// once immediately in detached goroutines so Start() returns promptly.
func (s *Scheduler) Start() {
	s.cronCtx, s.cancel = context.WithCancel(context.Background())

	logAdapter := slogAdapter{inner: s.logger}
	s.cron = cron.New(cron.WithChain(
		cron.SkipIfStillRunning(logAdapter),
		cron.Recover(logAdapter),
	))

	s.mu.Lock()
	var onStartEntries []*entry
	for _, e := range s.jobs {
		if !e.enabled.Load() {
			continue
		}
		id, err := s.scheduleEntry(e)
		if err != nil {
			s.logger.Error("schedule entry failed", "job", e.job.ID(), "error", err)
			continue
		}
		e.cronEntryID = id
		if r, ok := e.job.(OnStartRunner); ok && r.RunOnStart() {
			onStartEntries = append(onStartEntries, e)
		}
	}
	s.mu.Unlock()

	s.cron.Start()
	s.started.Store(true)

	// Kick off RunOnStart jobs in detached goroutines so we don't
	// block Start. Each goes through runOne which gives it the same
	// timeout + recover treatment as a scheduled tick.
	for _, e := range onStartEntries {
		go s.runOne(e, false)
	}

	s.logger.Info("scheduler started", "jobs", len(s.jobs))
}

// scheduleEntry registers `e` with the cron engine. Caller holds s.mu.
func (s *Scheduler) scheduleEntry(e *entry) (cron.EntryID, error) {
	spec := fmt.Sprintf("@every %s", e.job.Interval())
	return s.cron.AddFunc(spec, func() {
		s.runOne(e, false)
	})
}

// Stop signals the cron engine to stop, drains in-flight jobs up to
// stopDrainTimeout, then cancels the scheduler context. Idempotent —
// a second call is a no-op.
func (s *Scheduler) Stop() {
	if !s.started.Swap(false) {
		return
	}
	if s.cron != nil {
		drainCtx := s.cron.Stop()
		select {
		case <-drainCtx.Done():
		case <-time.After(stopDrainTimeout):
			s.logger.Warn("scheduler stop drain timed out; jobs still running",
				"timeout", stopDrainTimeout)
		}
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.logger.Info("scheduler stopped")
}

// runOne executes a single pass of the given entry. When manual is
// true the disabled-flag check is skipped (manual triggers bypass
// enabled per D15) and the cron wrapper chain is bypassed too — so we
// install our own deferred recover here for panic safety.
func (s *Scheduler) runOne(e *entry, manual bool) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("job panic recovered",
				"job", e.job.ID(), "panic", r)
		}
	}()

	if !manual && !e.enabled.Load() {
		return
	}

	timeout := defaultTimeout(e.job)
	if mrd, ok := e.job.(MaxRunDurationer); ok {
		if v := mrd.MaxRunDuration(); v > 0 {
			timeout = v
		}
	}

	parentCtx := s.cronCtx
	if parentCtx == nil {
		// Manual trigger before Start (or after Stop). Use a fresh
		// background ctx so the run still has a hard timeout.
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	var runID string
	if s.js != nil {
		if id, err := s.js.StartRun(ctx, e.job.ID(), s.replicaID); err == nil {
			runID = id
		} else {
			s.logger.Error("jobstore start_run failed",
				"job", e.job.ID(), "error", err)
		}
	}

	e.statusMu.Lock()
	e.status.LastStatus = "running"
	e.statusMu.Unlock()

	start := time.Now()
	err := e.job.Run(ctx)
	dur := time.Since(start)

	status := "success"
	errMsg := ""
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
		s.logger.Debug("job completed",
			"job", e.job.ID(), "duration", dur)
	}

	if s.js != nil && runID != "" {
		// Use a detached ctx so a deadline-exceeded run can still
		// record its FinishRun row.
		if ferr := s.js.FinishRun(context.Background(), runID, status, dur, errMsg); ferr != nil {
			s.logger.Error("jobstore finish_run failed",
				"job", e.job.ID(), "run_id", runID, "error", ferr)
		}
	}

	now := time.Now()
	e.statusMu.Lock()
	e.status.LastStatus = status
	e.status.LastError = errMsg
	e.status.LastRun = &now
	e.status.LastDuration = dur
	e.status.RunCount++
	if status == "error" {
		e.status.ErrorCount++
	}
	next := now.Add(e.job.Interval())
	e.status.NextRun = &next
	e.statusMu.Unlock()
}

// defaultTimeout returns the per-run hard timeout used when a Job
// does not implement MaxRunDurationer. Floored at 60s so very short
// intervals still get a comfortable cap.
func defaultTimeout(j Job) time.Duration {
	iv := j.Interval()
	if iv < minDefaultRunTimeout {
		return minDefaultRunTimeout
	}
	return iv
}

// ListJobs returns the status of all registered jobs. When a jobstore
// is attached the aggregate counters and last-run fields come from the
// DB so values reflect history across restarts; otherwise the
// in-memory snapshot is returned (used by unit tests).
func (s *Scheduler) ListJobs(ctx context.Context) ([]JobStatus, error) {
	if s.js == nil {
		return s.listJobsInMemory(), nil
	}
	rows, err := s.js.ListJobsWithStats(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]JobStatus, 0, len(rows))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range rows {
		out = append(out, s.statusFromStats(r))
	}
	return out, nil
}

func (s *Scheduler) listJobsInMemory() []JobStatus {
	s.mu.RLock()
	entries := make([]*entry, 0, len(s.jobs))
	for _, e := range s.jobs {
		entries = append(entries, e)
	}
	s.mu.RUnlock()

	result := make([]JobStatus, 0, len(entries))
	for _, e := range entries {
		e.statusMu.Lock()
		st := e.status
		e.statusMu.Unlock()
		// Refresh NextRun from cron when the engine is up.
		if next := s.nextRunFor(e); next != nil {
			st.NextRun = next
		}
		result = append(result, st)
	}
	return result
}

// GetJob returns the status of a single job by ID. Returns ErrJobNotFound
// when the ID is unknown.
func (s *Scheduler) GetJob(ctx context.Context, id string) (JobStatus, error) {
	if s.js == nil {
		s.mu.RLock()
		e, ok := s.jobs[id]
		s.mu.RUnlock()
		if !ok {
			return JobStatus{}, ErrJobNotFound
		}
		e.statusMu.Lock()
		st := e.status
		e.statusMu.Unlock()
		if next := s.nextRunFor(e); next != nil {
			st.NextRun = next
		}
		return st, nil
	}
	row, err := s.js.GetJobWithStats(ctx, id)
	if err != nil {
		if errors.Is(err, jobstore.ErrNotFound) {
			return JobStatus{}, ErrJobNotFound
		}
		return JobStatus{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statusFromStats(row), nil
}

// statusFromStats converts a jobstore aggregate row into the JobStatus
// shape the admin UI expects. NextRun is sourced from the cron engine
// when the job is currently scheduled; falls back to a LastRun+Interval
// estimate otherwise. Caller holds s.mu (read lock OK).
func (s *Scheduler) statusFromStats(r jobstore.JobWithStats) JobStatus {
	interval := time.Duration(r.IntervalSec) * time.Second
	var lastDur time.Duration
	if r.LastDurationMs != nil {
		lastDur = time.Duration(*r.LastDurationMs) * time.Millisecond
	}

	st := JobStatus{
		ID:           r.ID,
		Name:         r.Name,
		Description:  r.Description,
		Interval:     interval,
		Enabled:      r.Enabled,
		LastRun:      r.LastRun,
		LastDuration: lastDur,
		LastStatus:   r.LastStatus,
		LastError:    r.LastError,
		RunCount:     r.RunCount,
		ErrorCount:   r.ErrorCount,
	}
	if e, ok := s.jobs[r.ID]; ok {
		if next := s.nextRunForLocked(e); next != nil {
			st.NextRun = next
		} else if r.LastRun != nil {
			n := r.LastRun.Add(e.job.Interval())
			st.NextRun = &n
		}
	}
	return st
}

// nextRunFor returns the next-tick time for an entry. Takes s.mu read
// lock; safe from any goroutine.
func (s *Scheduler) nextRunFor(e *entry) *time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nextRunForLocked(e)
}

// nextRunForLocked is the lock-free variant for callers that already
// hold s.mu (read or write).
func (s *Scheduler) nextRunForLocked(e *entry) *time.Time {
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

// ListRuns returns the most recent job_run rows for a job (newest
// first) along with the total run count across all pages. Returns an
// empty slice and 0 total without error when no jobstore is attached.
func (s *Scheduler) ListRuns(ctx context.Context, id string, limit, offset int) ([]jobstore.JobRun, int, error) {
	if s.js == nil {
		return []jobstore.JobRun{}, 0, nil
	}
	return s.js.ListRuns(ctx, id, limit, offset)
}

// Trigger runs a job immediately by ID, bypassing the enabled flag.
// Returns ErrJobNotFound if not registered. The job runs in a detached
// goroutine so it survives cancellation of the caller's ctx (e.g. the
// HTTP request that invoked the trigger completing before the job
// finishes). Scheduler shutdown still terminates the run via the
// scheduler-wide ctx.
func (s *Scheduler) Trigger(_ context.Context, id string) error {
	s.mu.RLock()
	e, ok := s.jobs[id]
	s.mu.RUnlock()
	if !ok {
		return ErrJobNotFound
	}
	go s.runOne(e, true /* manual */)
	return nil
}

// RecoverStaleRuns marks every job_run row still in status='running' as
// 'interrupted'. Call this once after SyncDefinitions and before Start so
// orphaned rows from the previous process do not appear as perpetually
// running in the admin UI. No-op when no jobstore is attached.
func (s *Scheduler) RecoverStaleRuns(ctx context.Context) error {
	if s.js == nil {
		return nil
	}
	n, err := s.js.RecoverStaleRuns(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		s.logger.Info("startup recovery: marked stale job runs as interrupted", "count", n)
	}
	return nil
}

// ErrJobNotFound is returned when a job ID doesn't match any registered job.
var ErrJobNotFound = errors.New("scheduler: job not found")
