package jobstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestErrNotFound(t *testing.T) {
	if ErrNotFound == nil {
		t.Fatal("ErrNotFound must be non-nil")
	}
	if ErrNotFound.Error() != "jobstore: not found" {
		t.Errorf("ErrNotFound = %q", ErrNotFound.Error())
	}
}

func TestNew_NilPool(t *testing.T) {
	s := New(nil)
	if s == nil {
		t.Fatal("New(nil) must return non-nil Store")
	}
}

func TestStatusConstants(t *testing.T) {
	// Guard against accidental rename; these strings are written to the DB
	// and consumed by the admin UI.
	cases := map[string]string{
		StatusRunning: "running",
		StatusSuccess: "success",
		StatusError:   "error",
		StatusSkipped: "skipped",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("status constant %q != %q", got, want)
		}
	}
}

// --- Integration tests ---
//
// These run only when NEXUS_TEST_DB is set to a pgx-compatible DSN. CI may
// opt in; local dev runs skip by default because they need the full schema
// and migrations applied.

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NEXUS_TEST_DB")
	if dsn == "" {
		t.Skip("NEXUS_TEST_DB not set; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func cleanup(ctx context.Context, t *testing.T, p *pgxpool.Pool, jobID string) {
	t.Helper()
	_, _ = p.Exec(ctx, `DELETE FROM "job" WHERE "id" = $1`, jobID)
}

func TestIntegration_UpsertAndGet(t *testing.T) {
	p := testPool(t)
	s := New(p)
	ctx := context.Background()
	id := "test-upsert-get"
	t.Cleanup(func() { cleanup(ctx, t, p, id) })

	if err := s.UpsertJob(ctx, id, "Name V1", "desc v1", 60); err != nil {
		t.Fatalf("upsert v1: %v", err)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Name V1" || got.IntervalSec != 60 || !got.Enabled {
		t.Fatalf("unexpected v1 row: %+v", got)
	}

	// Re-upsert with new metadata — enabled must be preserved.
	if err := s.SetEnabled(ctx, id, false); err != nil {
		t.Fatalf("set disabled: %v", err)
	}
	if err := s.UpsertJob(ctx, id, "Name V2", "desc v2", 120); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	got, err = s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if got.Enabled {
		t.Error("enabled flag must survive re-upsert")
	}
	if got.Name != "Name V2" || got.IntervalSec != 120 {
		t.Errorf("metadata not updated: %+v", got)
	}
}

func TestIntegration_RunLifecycleAndStats(t *testing.T) {
	p := testPool(t)
	s := New(p)
	ctx := context.Background()
	id := "test-run-lifecycle"
	t.Cleanup(func() { cleanup(ctx, t, p, id) })

	if err := s.UpsertJob(ctx, id, "Run Lifecycle", "lifecycle test", 30); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Three runs: 2 success, 1 error
	for i, status := range []string{StatusSuccess, StatusError, StatusSuccess} {
		runID, err := s.StartRun(ctx, id, "replica-test")
		if err != nil {
			t.Fatalf("start run %d: %v", i, err)
		}
		errMsg := ""
		if status == StatusError {
			errMsg = "boom"
		}
		if err := s.FinishRun(ctx, runID, status, 42*time.Millisecond, errMsg); err != nil {
			t.Fatalf("finish run %d: %v", i, err)
		}
	}

	stats, err := s.GetJobWithStats(ctx, id)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.RunCount != 3 {
		t.Errorf("RunCount = %d, want 3", stats.RunCount)
	}
	if stats.ErrorCount != 1 {
		t.Errorf("ErrorCount = %d, want 1", stats.ErrorCount)
	}
	if stats.LastStatus != StatusSuccess {
		t.Errorf("LastStatus = %q, want success", stats.LastStatus)
	}

	runs, total, err := s.ListRuns(ctx, id, 10, 0)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("len(runs) = %d, want 3", len(runs))
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	// Newest first.
	if !runs[0].StartedAt.After(runs[2].StartedAt) && !runs[0].StartedAt.Equal(runs[2].StartedAt) {
		t.Error("runs not ordered newest first")
	}

	// Empty-page fallback: offset past the end should still return the
	// correct total via the COUNT fallback path.
	emptyRuns, emptyTotal, err := s.ListRuns(ctx, id, 10, 100)
	if err != nil {
		t.Fatalf("list runs (empty page): %v", err)
	}
	if len(emptyRuns) != 0 {
		t.Errorf("len(emptyRuns) = %d, want 0", len(emptyRuns))
	}
	if emptyTotal != 3 {
		t.Errorf("emptyTotal = %d, want 3", emptyTotal)
	}
}

func TestIntegration_Prune(t *testing.T) {
	p := testPool(t)
	s := New(p)
	ctx := context.Background()
	id := "test-prune"
	t.Cleanup(func() { cleanup(ctx, t, p, id) })

	if err := s.UpsertJob(ctx, id, "Prune", "prune test", 30); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for i := range 7 {
		runID, err := s.StartRun(ctx, id, "replica-test")
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if err := s.FinishRun(ctx, runID, StatusSuccess, time.Millisecond, ""); err != nil {
			t.Fatalf("finish %d: %v", i, err)
		}
	}
	n, err := s.PruneJobRuns(ctx, 3)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 4 {
		t.Errorf("pruned = %d, want 4", n)
	}
	runs, total, err := s.ListRuns(ctx, id, 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 3 {
		t.Errorf("len(runs) = %d, want 3", len(runs))
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
}
