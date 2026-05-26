package expiry

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pure (no-DB) identity tests.

func TestDiagModeExpiry_Identity(t *testing.T) {
	j := NewDiagModeExpiry(nil, time.Minute, testLogger())
	if j.ID() != "diag-mode-expiry" {
		t.Errorf("ID = %q, want diag-mode-expiry", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != time.Minute {
		t.Errorf("Interval = %v, want 1m", j.Interval())
	}
}

func TestDiagModeExpiry_IntervalDefault(t *testing.T) {
	j := NewDiagModeExpiry(nil, 0, testLogger())
	if j.Interval() != time.Minute {
		t.Errorf("Interval = %v, want 1m default", j.Interval())
	}
}

// DB-backed tests.

// diagExpiryTestSetup wipes thing + diag-mode state for the test thing IDs
// and seeds fresh thing rows.
func diagExpiryTestSetup(t *testing.T, pool *pgxpool.Pool, things []string) func() {
	t.Helper()
	ctx := context.Background()

	for _, id := range things {
		_, _ = pool.Exec(ctx, `DELETE FROM thing_diag_mode_window WHERE thing_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM thing WHERE id = $1`, id)
	}

	for _, id := range things {
		if _, err := pool.Exec(ctx, `
			INSERT INTO thing (id, name, type, status, last_seen_at, updated_at)
			VALUES ($1, $1, 'agent', 'online', NOW(), NOW())
		`, id); err != nil {
			t.Fatalf("seed thing %s: %v", id, err)
		}
	}

	return func() {
		for _, id := range things {
			_, _ = pool.Exec(ctx, `DELETE FROM thing_diag_mode_window WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM thing WHERE id = $1`, id)
		}
	}
}

// setDiagModeUntil writes a metadata JSONB containing diagModeUntil for the
// given thing. The value itself is ISO-8601 — the job only inspects the key.
func setDiagModeUntil(t *testing.T, pool *pgxpool.Pool, thingID string, until time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE thing
		   SET metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object('diagModeUntil', $2::text)
		 WHERE id = $1
	`, thingID, until.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("set diagModeUntil: %v", err)
	}
}

// insertDiagWindowExpiry writes a diag-mode window with explicit start/end.
func insertDiagWindowExpiry(t *testing.T, pool *pgxpool.Pool, thingID string, start, end time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO thing_diag_mode_window (id, thing_id, started_at, ended_at)
		VALUES ($1, $2, $3, $4)
	`, uuid.NewString(), thingID, start, end); err != nil {
		t.Fatalf("insert diag window: %v", err)
	}
}

// hasDiagModeUntil reports whether the thing's metadata still carries the flag.
func hasDiagModeUntil(t *testing.T, pool *pgxpool.Pool, thingID string) bool {
	t.Helper()
	var present bool
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(metadata ? 'diagModeUntil', false) FROM thing WHERE id = $1
	`, thingID).Scan(&present); err != nil {
		t.Fatalf("query metadata: %v", err)
	}
	return present
}

func runDiagModeExpiry(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	j := NewDiagModeExpiry(pool, time.Minute, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestDiagModeExpiry_ClearsExpired asserts an agent whose diag-mode window
// has ended_at <= NOW() and whose metadata still carries diagModeUntil has
// the flag stripped.
func TestDiagModeExpiry_ClearsExpired(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-diag-expiry-clear"
	cleanup := diagExpiryTestSetup(t, pool, []string{thingID})
	defer cleanup()

	now := time.Now().UTC()
	// Window ended 1 hour ago.
	insertDiagWindowExpiry(t, pool, thingID, now.Add(-2*time.Hour), now.Add(-time.Hour))
	setDiagModeUntil(t, pool, thingID, now.Add(-time.Hour))

	if !hasDiagModeUntil(t, pool, thingID) {
		t.Fatalf("precondition: diagModeUntil not set")
	}

	runDiagModeExpiry(t, pool)

	if hasDiagModeUntil(t, pool, thingID) {
		t.Errorf("diagModeUntil still present after expiry — flag not cleared")
	}
}

// TestDiagModeExpiry_LeavesActiveAlone asserts an agent whose window is still
// open (ended_at > NOW()) is left alone.
func TestDiagModeExpiry_LeavesActiveAlone(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-diag-expiry-active"
	cleanup := diagExpiryTestSetup(t, pool, []string{thingID})
	defer cleanup()

	now := time.Now().UTC()
	// Window ends in the future.
	insertDiagWindowExpiry(t, pool, thingID, now.Add(-time.Hour), now.Add(time.Hour))
	setDiagModeUntil(t, pool, thingID, now.Add(time.Hour))

	if !hasDiagModeUntil(t, pool, thingID) {
		t.Fatalf("precondition: diagModeUntil not set")
	}

	runDiagModeExpiry(t, pool)

	if !hasDiagModeUntil(t, pool, thingID) {
		t.Errorf("diagModeUntil cleared while window still open — incorrect")
	}
}

// TestDiagModeExpiry_NoOpWhenWindowEndedButFlagAlreadyCleared asserts that
// when the window ended but the metadata flag was already cleared (e.g. by a
// previous run), the job is a silent no-op (no error, no spurious updates).
func TestDiagModeExpiry_NoOpWhenWindowEndedButFlagAlreadyCleared(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-diag-expiry-already-clear"
	cleanup := diagExpiryTestSetup(t, pool, []string{thingID})
	defer cleanup()

	now := time.Now().UTC()
	insertDiagWindowExpiry(t, pool, thingID, now.Add(-2*time.Hour), now.Add(-time.Hour))
	// Don't set diagModeUntil — simulate a prior run that already cleared it.

	if hasDiagModeUntil(t, pool, thingID) {
		t.Fatalf("precondition: diagModeUntil unexpectedly set")
	}

	// Capture updated_at so we can detect spurious mutation.
	ctx := context.Background()
	var preUpdatedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM thing WHERE id = $1`, thingID).Scan(&preUpdatedAt); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	runDiagModeExpiry(t, pool)

	var postUpdatedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM thing WHERE id = $1`, thingID).Scan(&postUpdatedAt); err != nil {
		t.Fatalf("read updated_at after: %v", err)
	}

	if !postUpdatedAt.Equal(preUpdatedAt) {
		t.Errorf("updated_at changed (%v → %v) for thing whose flag was already clear — spurious UPDATE",
			preUpdatedAt, postUpdatedAt)
	}
}
