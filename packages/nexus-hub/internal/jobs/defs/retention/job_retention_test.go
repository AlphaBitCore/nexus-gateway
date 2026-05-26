package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	jobstore "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/store"
)

// fakeJobRetentionStore is a hand-rolled fake for the
// jobRetentionStore interface.
type fakeJobRetentionStore struct {
	calls []int
	ret   int64
	err   error
}

func (f *fakeJobRetentionStore) PruneJobRuns(_ context.Context, keepN int) (int64, error) {
	f.calls = append(f.calls, keepN)
	return f.ret, f.err
}

func TestJobRetention_Identity(t *testing.T) {
	j := NewJobRetention(jobstore.New(nil), time.Hour, 50, testLogger())
	if j.ID() != "job-retention" {
		t.Errorf("ID = %q, want job-retention", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name must not be empty")
	}
	if j.Description() == "" {
		t.Error("Description must not be empty")
	}
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h", j.Interval())
	}
	if !j.RunOnStart() {
		t.Error("RunOnStart must be true — first prune should not wait a full interval")
	}
}

func TestJobRetention_DefaultsApplied(t *testing.T) {
	cases := []struct {
		name      string
		interval  time.Duration
		keep      int
		wantIntvl time.Duration
		wantKeepN bool // true when we expect keepPerJob to be defaulted to 100
	}{
		{"zero interval", 0, 50, 24 * time.Hour, false},
		{"negative interval", -1 * time.Second, 50, 24 * time.Hour, false},
		{"zero keep", time.Hour, 0, time.Hour, true},
		{"negative keep", time.Hour, -10, time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := NewJobRetention(jobstore.New(nil), tc.interval, tc.keep, testLogger())
			if j.Interval() != tc.wantIntvl {
				t.Errorf("Interval = %v, want %v", j.Interval(), tc.wantIntvl)
			}
			if tc.wantKeepN && j.keepPerJob != 100 {
				t.Errorf("keepPerJob = %d, want default 100", j.keepPerJob)
			}
			if !tc.wantKeepN && j.keepPerJob != tc.keep {
				t.Errorf("keepPerJob = %d, want %d (no default)", j.keepPerJob, tc.keep)
			}
		})
	}
}

func TestJobRetention_Run_Success(t *testing.T) {
	fs := &fakeJobRetentionStore{ret: 17}
	j := &JobRetention{store: fs, keepPerJob: 50, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fs.calls) != 1 || fs.calls[0] != 50 {
		t.Errorf("calls = %v, want [50]", fs.calls)
	}
}

func TestJobRetention_Run_Zero(t *testing.T) {
	fs := &fakeJobRetentionStore{ret: 0}
	j := &JobRetention{store: fs, keepPerJob: 100, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestJobRetention_Run_Error(t *testing.T) {
	sentinel := errors.New("prune boom")
	fs := &fakeJobRetentionStore{err: sentinel}
	j := &JobRetention{store: fs, keepPerJob: 50, logger: testLogger()}
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}
