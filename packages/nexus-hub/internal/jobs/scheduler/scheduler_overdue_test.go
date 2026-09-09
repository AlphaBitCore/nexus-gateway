package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	jobstore "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/store"
)

// overdueStore is a jobStoreIface stub that answers ListJobsWithStats from a
// fixed set of rows. Everything else is inert: these tests are about what Start
// decides from the persisted last run, not about persistence itself.
type overdueStore struct {
	rows []jobstore.JobWithStats
	err  error
}

func (s *overdueStore) UpsertJob(context.Context, string, string, string, int) error { return nil }
func (s *overdueStore) GetEnabled(context.Context, string) (bool, error)             { return true, nil }
func (s *overdueStore) SetEnabled(context.Context, string, bool) error               { return nil }
func (s *overdueStore) StartRun(context.Context, string, string) (string, error)     { return "", nil }
func (s *overdueStore) FinishRun(context.Context, string, string, time.Duration, string) error {
	return nil
}
func (s *overdueStore) ListJobsWithStats(context.Context) ([]jobstore.JobWithStats, error) {
	return s.rows, s.err
}
func (s *overdueStore) GetJobWithStats(context.Context, string) (jobstore.JobWithStats, error) {
	return jobstore.JobWithStats{}, nil
}
func (s *overdueStore) ListRuns(context.Context, string, int, int) ([]jobstore.JobRun, int, error) {
	return nil, 0, nil
}
func (s *overdueStore) RecoverStaleRuns(context.Context) (int64, error) { return 0, nil }

func row(id string, intervalSec int, lastRun *time.Time, enabled bool) jobstore.JobWithStats {
	return jobstore.JobWithStats{
		JobRecord: jobstore.JobRecord{
			ID: id, Name: id, Description: "stub",
			IntervalSec: intervalSec, Enabled: enabled,
		},
		LastRun: lastRun,
	}
}

func ptr(t time.Time) *time.Time { return &t }

// The defect, pinned in its own name. A daily job whose last run is already
// more than a day old must run when the scheduler comes back up. robfig's
// `@every` starts counting at registration, so before this the restart pushed
// the due slot a full interval into the future and nothing recorded that a
// cycle had been skipped: lastStatus still read success. On prod this moved
// seven daily jobs — data retention purge among them — from a run due fourteen
// minutes after the deploy to one twenty-four hours later.
func TestStart_RunsAJobThatFellDueWhileTheSchedulerWasDown(t *testing.T) {
	s := New(slog.Default())
	j := &mockJob{name: "daily-purge", interval: 24 * time.Hour}
	s.Register(j)
	s.js = &overdueStore{rows: []jobstore.JobWithStats{
		row("daily-purge", 86400, ptr(time.Now().Add(-30*time.Hour)), true),
	}}

	s.Start()
	time.Sleep(150 * time.Millisecond)
	s.Stop()

	if got := j.runs.Load(); got != 1 {
		t.Errorf("a job overdue by six hours must run once at startup; got %d runs", got)
	}
}

// The other half of the same decision: a job still inside its interval must NOT
// be dragged forward. Without this the fix would turn every restart into a
// full sweep of every job, which is a different defect with the same symptom.
func TestStart_LeavesAJobThatIsNotYetDue(t *testing.T) {
	s := New(slog.Default())
	j := &mockJob{name: "daily-fresh", interval: 24 * time.Hour}
	s.Register(j)
	s.js = &overdueStore{rows: []jobstore.JobWithStats{
		row("daily-fresh", 86400, ptr(time.Now().Add(-1*time.Hour)), true),
	}}

	s.Start()
	time.Sleep(150 * time.Millisecond)
	s.Stop()

	if got := j.runs.Load(); got != 0 {
		t.Errorf("a job that ran an hour ago on a daily interval must wait; got %d runs", got)
	}
}

// A job with no run history has no slot to have missed. Firing it here would
// mean every newly-registered job runs on the deploy that introduces it, which
// is not what `@every` promises.
func TestStart_DoesNotRunAJobThatHasNeverRun(t *testing.T) {
	s := New(slog.Default())
	j := &mockJob{name: "brand-new", interval: 24 * time.Hour}
	s.Register(j)
	s.js = &overdueStore{rows: []jobstore.JobWithStats{
		row("brand-new", 86400, nil, true),
	}}

	s.Start()
	time.Sleep(150 * time.Millisecond)
	s.Stop()

	if got := j.runs.Load(); got != 0 {
		t.Errorf("a job that has never run is not overdue; got %d runs", got)
	}
}

// A disabled job stays disabled. The store row and the in-memory entry are two
// separate switches and both have to be respected.
func TestStart_DoesNotRunAnOverdueButDisabledJob(t *testing.T) {
	s := New(slog.Default())
	j := &mockJob{name: "daily-off", interval: 24 * time.Hour}
	s.Register(j)
	if err := s.SetEnabled(context.Background(), "daily-off", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	s.js = &overdueStore{rows: []jobstore.JobWithStats{
		row("daily-off", 86400, ptr(time.Now().Add(-30*time.Hour)), false),
	}}

	s.Start()
	time.Sleep(150 * time.Millisecond)
	s.Stop()

	if got := j.runs.Load(); got != 0 {
		t.Errorf("a disabled job must not be revived by the catch-up; got %d runs", got)
	}
}

// A job that opts into RunOnStart is already kicked off by the existing path.
// It must not be kicked a second time by the catch-up, or every restart
// double-runs it.
func TestStart_DoesNotDoubleRunARunOnStartJobThatIsAlsoOverdue(t *testing.T) {
	s := New(slog.Default())
	j := &onStartJob{
		mockJob:    mockJob{name: "boot-and-overdue", interval: 24 * time.Hour},
		runOnStart: true,
	}
	s.Register(j)
	s.js = &overdueStore{rows: []jobstore.JobWithStats{
		row("boot-and-overdue", 86400, ptr(time.Now().Add(-30*time.Hour)), true),
	}}

	s.Start()
	time.Sleep(200 * time.Millisecond)
	s.Stop()

	if got := j.runs.Load(); got != 1 {
		t.Errorf("RunOnStart and overdue are the same run, not two; got %d runs", got)
	}
}

// Startup must not depend on the database being reachable. A failed lookup
// leaves every job on its normal cadence — the behaviour that shipped before
// the catch-up existed — rather than blocking or panicking.
func TestStart_SurvivesAStoreThatCannotBeRead(t *testing.T) {
	s := New(slog.Default())
	j := &mockJob{name: "daily-unreadable", interval: 24 * time.Hour}
	s.Register(j)
	s.js = &overdueStore{err: context.DeadlineExceeded}

	s.Start()
	time.Sleep(150 * time.Millisecond)
	s.Stop()

	if got := j.runs.Load(); got != 0 {
		t.Errorf("an unreadable store must not invent runs; got %d runs", got)
	}
}
