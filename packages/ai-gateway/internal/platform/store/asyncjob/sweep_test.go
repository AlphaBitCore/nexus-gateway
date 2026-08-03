package asyncjob

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// sweepFakeStore records MarkExpired calls; every other Store method is
// unused by the sweep.
type sweepFakeStore struct {
	Store             // nil embed — panic on any unexpected call
	terminalCutoff    time.Time
	nonTerminalCutoff time.Time
	calls             int
	marked            int64
	err               error
}

func (f *sweepFakeStore) MarkExpired(_ context.Context, terminalCutoff, nonTerminalCutoff time.Time) (int64, error) {
	f.calls++
	f.terminalCutoff = terminalCutoff
	f.nonTerminalCutoff = nonTerminalCutoff
	return f.marked, f.err
}

// Sweep applies the SDD §5 retention policy: terminal rows expire after 30
// days, non-terminal after 7 — the cutoffs handed to MarkExpired must be
// exactly now minus those windows.
func TestSweep_AppliesRetentionPolicy(t *testing.T) {
	f := &sweepFakeStore{marked: 3}
	before := time.Now().UTC()
	Sweep(context.Background(), f, slog.Default())
	after := time.Now().UTC()

	if f.calls != 1 {
		t.Fatalf("MarkExpired calls = %d, want 1", f.calls)
	}
	assertCutoff := func(name string, got time.Time, window time.Duration) {
		lo, hi := before.Add(-window), after.Add(-window)
		if got.Before(lo) || got.After(hi) {
			t.Errorf("%s cutoff = %v, want now-%v (between %v and %v)", name, got, window, lo, hi)
		}
	}
	assertCutoff("terminal", f.terminalCutoff, TerminalRetention)
	assertCutoff("non-terminal", f.nonTerminalCutoff, NonTerminalRetention)
	if TerminalRetention != 30*24*time.Hour || NonTerminalRetention != 7*24*time.Hour {
		t.Errorf("retention constants drifted from the SDD §5 policy (30d/7d): %v/%v",
			TerminalRetention, NonTerminalRetention)
	}
}

// A failing sweep logs and returns — periodic self-retry, never a panic.
func TestSweep_ErrorIsNonFatal(t *testing.T) {
	f := &sweepFakeStore{err: errors.New("pg down")}
	Sweep(context.Background(), f, slog.Default())
	if f.calls != 1 {
		t.Fatalf("MarkExpired calls = %d, want 1", f.calls)
	}
}
