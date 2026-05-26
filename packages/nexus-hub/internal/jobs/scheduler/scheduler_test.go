package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type mockJob struct {
	name     string
	interval time.Duration
	runs     atomic.Int64
	err      error
}

func (j *mockJob) ID() string              { return j.name }
func (j *mockJob) Name() string            { return j.name }
func (j *mockJob) Description() string     { return "mock job for tests" }
func (j *mockJob) Interval() time.Duration { return j.interval }
func (j *mockJob) Run(_ context.Context) error {
	j.runs.Add(1)
	return j.err
}

func TestSchedulerRegisterAndList(t *testing.T) {
	s := New(slog.Default())
	j1 := &mockJob{name: "job-a", interval: time.Minute}
	j2 := &mockJob{name: "job-b", interval: 5 * time.Minute}

	s.Register(j1)
	s.Register(j2)

	jobs, _ := s.ListJobs(context.Background())
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
}

func TestSchedulerStartAndRun(t *testing.T) {
	// robfig/cron's @every clamps sub-second durations to 1s, so this
	// test uses 1s interval + 1.5s wait to observe at least one tick.
	s := New(slog.Default())
	j := &mockJob{name: "quick", interval: 1 * time.Second}
	s.Register(j)

	s.Start()
	time.Sleep(1500 * time.Millisecond)
	s.Stop()

	if j.runs.Load() < 1 {
		t.Errorf("expected at least 1 run, got %d", j.runs.Load())
	}
}

func TestSchedulerTrigger(t *testing.T) {
	s := New(slog.Default())
	j := &mockJob{name: "manual", interval: time.Hour}
	s.Register(j)
	s.Start()
	defer s.Stop()

	err := s.Trigger(context.Background(), "manual")
	if err != nil {
		t.Fatalf("trigger error: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if j.runs.Load() < 1 {
		t.Error("expected job to run after trigger")
	}
}

func TestSchedulerTriggerNotFound(t *testing.T) {
	s := New(slog.Default())
	err := s.Trigger(context.Background(), "nonexistent")
	if !errors.Is(err, ErrJobNotFound) {
		t.Errorf("expected ErrJobNotFound, got %v", err)
	}
}

// triggerCtxJob signals `completed` only when the timer branch fires, so the
// test can distinguish the job running to completion (WithoutCancel applied)
// from the caller's ctx propagating and cancelling Run early.
type triggerCtxJob struct {
	name      string
	completed chan struct{}
}

func (j *triggerCtxJob) ID() string              { return j.name }
func (j *triggerCtxJob) Name() string            { return j.name }
func (j *triggerCtxJob) Description() string     { return "trigger ctx test job" }
func (j *triggerCtxJob) Interval() time.Duration { return time.Hour }
func (j *triggerCtxJob) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(200 * time.Millisecond):
		close(j.completed)
		return nil
	}
}

func TestTrigger_JobSurvivesCallerContextCancel(t *testing.T) {
	s := New(slog.Default())
	j := &triggerCtxJob{name: "trig-ctx", completed: make(chan struct{})}
	s.Register(j)

	callerCtx, cancel := context.WithCancel(context.Background())
	if err := s.Trigger(callerCtx, "trig-ctx"); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	cancel()

	select {
	case <-j.completed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("trigger job was cancelled by caller context (expected to run to completion)")
	}
}

type onStartJob struct {
	mockJob
	runOnStart bool
}

func (j *onStartJob) RunOnStart() bool { return j.runOnStart }

func TestSchedulerRunOnStart_Runs(t *testing.T) {
	s := New(slog.Default())
	j := &onStartJob{
		mockJob:    mockJob{name: "boot", interval: time.Hour},
		runOnStart: true,
	}
	s.Register(j)

	s.Start()
	time.Sleep(100 * time.Millisecond)
	s.Stop()

	if j.runs.Load() < 1 {
		t.Errorf("expected RunOnStart=true job to fire once immediately, got %d runs", j.runs.Load())
	}
}

func TestSchedulerRunOnStart_Skipped(t *testing.T) {
	s := New(slog.Default())
	j := &onStartJob{
		mockJob:    mockJob{name: "boot-skip", interval: time.Hour},
		runOnStart: false,
	}
	s.Register(j)

	s.Start()
	time.Sleep(100 * time.Millisecond)
	s.Stop()

	if j.runs.Load() != 0 {
		t.Errorf("expected RunOnStart=false job to wait for ticker, got %d runs", j.runs.Load())
	}
}

func TestSchedulerJobError(t *testing.T) {
	// 1s interval + 1.5s wait — see comment in TestSchedulerStartAndRun.
	s := New(slog.Default())
	j := &mockJob{name: "failing", interval: 1 * time.Second, err: errors.New("boom")}
	s.Register(j)

	s.Start()
	time.Sleep(1500 * time.Millisecond)
	s.Stop()

	jobs, _ := s.ListJobs(context.Background())
	for _, js := range jobs {
		if js.Name == "failing" {
			if js.ErrorCount == 0 {
				t.Error("expected error count > 0")
			}
			return
		}
	}
	t.Error("failing job not found in list")
}
