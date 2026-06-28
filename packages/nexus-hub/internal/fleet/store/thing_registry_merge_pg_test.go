package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// mergePGTestPool returns a pgx pool for the per-key reported merge semantic
// test, or skips when no database is wired. Mirrors the override-test harness:
// a machine without a running stack still passes.
func mergePGTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping per-key merge integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skip: DB unavailable (%v)", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skip: DB ping failed (%v)", err)
	}
	return pool
}

const mergePGTestPrefix = "merge-pg-test-"

// TestUpdateShadowReport_PerKeyMerge_PreservesSiblings is the per-key merge
// end-to-end semantic check against real Postgres: a Thing that reports ONLY a
// single changed key must update that key's reported state while leaving every
// sibling key's reported state intact, and a stale (older-version) report must
// change nothing.
func TestUpdateShadowReport_PerKeyMerge_PreservesSiblings(t *testing.T) {
	pool := mergePGTestPool(t)
	defer pool.Close()
	ctx := context.Background()
	s := New(pool)

	id := mergePGTestPrefix + "siblings"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM thing WHERE id = $1`, id)
	})

	// Seed a Thing whose reported map already holds two sibling keys at v1.
	_, err := pool.Exec(ctx, `
		INSERT INTO thing (id, type, name, version, address, enrolled_by, auth_type, conn_protocol,
		                   status, metadata, desired, reported, desired_ver, reported_ver,
		                   last_seen_at, enrolled_at, updated_at, reported_outcomes)
		VALUES ($1, 'agent', $1, '1.0.0', '127.0.0.1', 'tester', 'bearer', 'http',
		        'online', '{}'::jsonb, '{}'::jsonb,
		        '{"alpha":{"v":1},"beta":{"v":1}}'::jsonb, 2, 1,
		        NOW(), NOW(), NOW(), '{}'::jsonb)
		ON CONFLICT (id) DO UPDATE SET
			reported     = EXCLUDED.reported,
			reported_ver = EXCLUDED.reported_ver,
			desired_ver  = EXCLUDED.desired_ver,
			status       = 'online'
	`, id)
	if err != nil {
		t.Fatalf("seed thing: %v", err)
	}

	// Per-key delta report: only the "alpha" key, at the newer version 2.
	now := time.Now().UTC()
	v2 := int64(2)
	if err := s.UpdateShadowReport(ctx, id,
		map[string]any{"alpha": map[string]any{"v": 2}}, 2,
		map[string]ReportedKeyOutcome{"alpha": {AppliedAt: &now, AppliedVersion: &v2}}); err != nil {
		t.Fatalf("UpdateShadowReport (alpha v2): %v", err)
	}

	got, err := s.GetThing(ctx, id)
	if err != nil {
		t.Fatalf("GetThing: %v", err)
	}
	// alpha updated to v2.
	alpha, _ := got.Reported["alpha"].(map[string]any)
	if alpha == nil || alpha["v"] != float64(2) {
		t.Fatalf("alpha reported = %v; want {v:2} (merged update)", got.Reported["alpha"])
	}
	// beta preserved at v1 — NOT wiped by the single-key report.
	beta, _ := got.Reported["beta"].(map[string]any)
	if beta == nil || beta["v"] != float64(1) {
		t.Fatalf("beta reported = %v; want {v:1} (sibling preserved)", got.Reported["beta"])
	}
	if got.ReportedVer != 2 {
		t.Fatalf("reported_ver = %d; want 2", got.ReportedVer)
	}
	// status flips drift→online only when reported_ver >= desired_ver(2): here
	// 2 >= 2 so the node converges.
	if got.Status != "online" {
		t.Fatalf("status = %q; want online (converged)", got.Status)
	}

	// Stale report (version 1 < stored reported_ver 2): must change nothing.
	if err := s.UpdateShadowReport(ctx, id,
		map[string]any{"alpha": map[string]any{"v": 99}}, 1, nil); err != nil {
		t.Fatalf("UpdateShadowReport (stale): %v", err)
	}
	got2, err := s.GetThing(ctx, id)
	if err != nil {
		t.Fatalf("GetThing after stale: %v", err)
	}
	alpha2, _ := got2.Reported["alpha"].(map[string]any)
	if alpha2 == nil || alpha2["v"] != float64(2) {
		t.Fatalf("alpha after stale report = %v; want unchanged {v:2}", got2.Reported["alpha"])
	}
	if got2.ReportedVer != 2 {
		t.Fatalf("reported_ver after stale = %d; want 2 (no regression)", got2.ReportedVer)
	}

	// Empty report map at an equal/newer version is a no-op merge: siblings stay.
	if err := s.UpdateShadowReport(ctx, id, map[string]any{}, 2, nil); err != nil {
		t.Fatalf("UpdateShadowReport (empty): %v", err)
	}
	got3, err := s.GetThing(ctx, id)
	if err != nil {
		t.Fatalf("GetThing after empty: %v", err)
	}
	if _, ok := got3.Reported["alpha"]; !ok {
		t.Fatalf("alpha vanished after empty report; merge must not wipe on empty map")
	}
	if _, ok := got3.Reported["beta"]; !ok {
		t.Fatalf("beta vanished after empty report; merge must not wipe on empty map")
	}
}
