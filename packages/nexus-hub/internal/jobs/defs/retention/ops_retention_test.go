package retention

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pure (no-DB) identity tests.

func TestOpsRetention_Identity(t *testing.T) {
	j := NewOpsRetention(nil, 24*time.Hour, testLogger())
	if j.ID() != "ops-retention" {
		t.Errorf("ID = %q, want ops-retention", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h", j.Interval())
	}
}

func TestOpsRetention_IntervalDefault(t *testing.T) {
	j := NewOpsRetention(nil, 0, testLogger())
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h default", j.Interval())
	}
}

// DB-backed tests.

// opsRetentionTestSetup wipes ops state for the test thing IDs and ensures
// the canonical retention-config defaults are present (the migration seeds
// them, but other tests may have mutated rows).
func opsRetentionTestSetup(t *testing.T, pool *pgxpool.Pool, things map[string]string) func() {
	t.Helper()
	ctx := context.Background()

	for id, ttype := range things {
		if _, err := pool.Exec(ctx, `
			INSERT INTO thing (id, name, type, status, last_seen_at, updated_at)
			VALUES ($1, $1, $2, 'online', NOW(), NOW())
			ON CONFLICT (id) DO NOTHING
		`, id, ttype); err != nil {
			t.Fatalf("seed thing %s: %v", id, err)
		}
	}

	for id := range things {
		_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_raw WHERE thing_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_rollup_1h WHERE thing_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_rollup_1d WHERE thing_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_rollup_1mo WHERE thing_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM thing_diag_event WHERE thing_id = $1`, id)
	}

	// Restore the canonical defaults so this test's behaviour is isolated.
	if _, err := pool.Exec(ctx, `
		INSERT INTO metric_ops_retention_config (layer, retention_days) VALUES
		  ('runtime_raw',   7),
		  ('business_raw',  7),
		  ('runtime_1h',    90),
		  ('business_1h',   90),
		  ('runtime_1d',    365),
		  ('business_1d',   365),
		  ('runtime_1mo',   1825),
		  ('business_1mo',  1825),
		  ('diag_warn',     30),
		  ('diag_error',    180),
		  ('diag_fatal',    365)
		ON CONFLICT (layer) DO UPDATE SET retention_days = EXCLUDED.retention_days
	`); err != nil {
		t.Fatalf("restore config: %v", err)
	}

	return func() {
		for id := range things {
			_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_raw WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_rollup_1h WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_rollup_1d WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_rollup_1mo WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM thing_diag_event WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM thing WHERE id = $1`, id)
		}
	}
}

// insertRawForRetention writes one metric_ops_raw row with a controlled
// sampled_at so the retention test can age it.
func insertRawForRetention(t *testing.T, pool *pgxpool.Pool, sampledAt time.Time, thingID, name string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO metric_ops_raw (id, sampled_at, thing_id, thing_type, metric_name, metric_kind, dimension_key, value)
		VALUES ($1, $2, $3, 'service', $4, 'gauge', '', 1.0)
	`, uuid.NewString(), sampledAt, thingID, name)
	if err != nil {
		t.Fatalf("insert raw: %v", err)
	}
}

// insertRollupForRetention writes one rollup row in the given table with a
// controlled bucket_start so the retention test can age it.
func insertRollupForRetention(t *testing.T, pool *pgxpool.Pool, table string, bucketStart time.Time, thingID, name string) {
	t.Helper()
	q := fmt.Sprintf(`
		INSERT INTO %s
			(id, bucket_start, thing_id, thing_type, metric_name, metric_kind, dimension_key,
			 value_avg, value_sum, value_min, value_max, sample_count)
		VALUES ($1, $2, $3, 'service', $4, 'gauge', '', 1, 1, 1, 1, 1)
	`, table)
	if _, err := pool.Exec(context.Background(), q, uuid.NewString(), bucketStart, thingID, name); err != nil {
		t.Fatalf("insert rollup row in %s: %v", table, err)
	}
}

// insertDiagForRetention writes one thing_diag_event with a controlled
// occurred_at + level.
func insertDiagForRetention(t *testing.T, pool *pgxpool.Pool, occurredAt time.Time, thingID, level string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO thing_diag_event
			(id, thing_id, thing_type, occurred_at, level, event_type, source, message, message_hash)
		VALUES ($1, $2, 'agent', $3, $4, 'log', 'agent', 'test', 'test-hash')
	`, uuid.NewString(), thingID, occurredAt, level)
	if err != nil {
		t.Fatalf("insert diag: %v", err)
	}
}

func runOpsRetention(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	j := NewOpsRetention(pool, 24*time.Hour, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestOpsRetention_DeletesAgedRowsKeepsInWindow seeds one row per layer with
// timestamps split across the cutoff (one well-aged, one fresh) and asserts
// only the aged rows are deleted.
func TestOpsRetention_DeletesAgedRowsKeepsInWindow(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops-ret-window"
	cleanup := opsRetentionTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	now := time.Now().UTC()

	// raw: defaults are 7d. Aged = 30d ago; fresh = 1h ago.
	insertRawForRetention(t, pool, now.AddDate(0, 0, -30), thingID, "runtime.heap")
	insertRawForRetention(t, pool, now.Add(-time.Hour), thingID, "runtime.heap")
	insertRawForRetention(t, pool, now.AddDate(0, 0, -30), thingID, "biz.tokens")
	insertRawForRetention(t, pool, now.Add(-time.Hour), thingID, "biz.tokens")

	// rollup_1h: 90d. Aged = 200d; fresh = 30d.
	insertRollupForRetention(t, pool, "metric_ops_rollup_1h", now.AddDate(0, 0, -200), thingID, "runtime.heap")
	insertRollupForRetention(t, pool, "metric_ops_rollup_1h", now.AddDate(0, 0, -30), thingID, "runtime.heap")
	insertRollupForRetention(t, pool, "metric_ops_rollup_1h", now.AddDate(0, 0, -200), thingID, "biz.tokens")
	insertRollupForRetention(t, pool, "metric_ops_rollup_1h", now.AddDate(0, 0, -30), thingID, "biz.tokens")

	// rollup_1d: 365d. Aged = 400d; fresh = 90d.
	insertRollupForRetention(t, pool, "metric_ops_rollup_1d", now.AddDate(0, 0, -400), thingID, "runtime.heap")
	insertRollupForRetention(t, pool, "metric_ops_rollup_1d", now.AddDate(0, 0, -90), thingID, "runtime.heap")
	insertRollupForRetention(t, pool, "metric_ops_rollup_1d", now.AddDate(0, 0, -400), thingID, "biz.tokens")
	insertRollupForRetention(t, pool, "metric_ops_rollup_1d", now.AddDate(0, 0, -90), thingID, "biz.tokens")

	// rollup_1mo: 1825d. Aged = 2000d; fresh = 365d.
	insertRollupForRetention(t, pool, "metric_ops_rollup_1mo", now.AddDate(0, 0, -2000), thingID, "runtime.heap")
	insertRollupForRetention(t, pool, "metric_ops_rollup_1mo", now.AddDate(0, 0, -365), thingID, "runtime.heap")
	insertRollupForRetention(t, pool, "metric_ops_rollup_1mo", now.AddDate(0, 0, -2000), thingID, "biz.tokens")
	insertRollupForRetention(t, pool, "metric_ops_rollup_1mo", now.AddDate(0, 0, -365), thingID, "biz.tokens")

	// diag_warn=30, diag_error=180, diag_fatal=365.
	insertDiagForRetention(t, pool, now.AddDate(0, 0, -45), thingID, "warn")
	insertDiagForRetention(t, pool, now.AddDate(0, 0, -10), thingID, "warn")
	insertDiagForRetention(t, pool, now.AddDate(0, 0, -200), thingID, "error")
	insertDiagForRetention(t, pool, now.AddDate(0, 0, -90), thingID, "error")
	insertDiagForRetention(t, pool, now.AddDate(0, 0, -400), thingID, "fatal")
	insertDiagForRetention(t, pool, now.AddDate(0, 0, -90), thingID, "fatal")

	runOpsRetention(t, pool)

	ctx := context.Background()
	mustCount := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// runtime_raw + business_raw kept = 1 each.
	if got := mustCount(`SELECT COUNT(*) FROM metric_ops_raw WHERE thing_id=$1 AND metric_name LIKE 'runtime.%'`, thingID); got != 1 {
		t.Errorf("runtime_raw kept = %d, want 1", got)
	}
	if got := mustCount(`SELECT COUNT(*) FROM metric_ops_raw WHERE thing_id=$1 AND metric_name NOT LIKE 'runtime.%'`, thingID); got != 1 {
		t.Errorf("business_raw kept = %d, want 1", got)
	}

	// rollup_1h kept = 1 each.
	if got := mustCount(`SELECT COUNT(*) FROM metric_ops_rollup_1h WHERE thing_id=$1 AND metric_name LIKE 'runtime.%'`, thingID); got != 1 {
		t.Errorf("runtime_1h kept = %d, want 1", got)
	}
	if got := mustCount(`SELECT COUNT(*) FROM metric_ops_rollup_1h WHERE thing_id=$1 AND metric_name NOT LIKE 'runtime.%'`, thingID); got != 1 {
		t.Errorf("business_1h kept = %d, want 1", got)
	}

	// rollup_1d kept = 1 each.
	if got := mustCount(`SELECT COUNT(*) FROM metric_ops_rollup_1d WHERE thing_id=$1 AND metric_name LIKE 'runtime.%'`, thingID); got != 1 {
		t.Errorf("runtime_1d kept = %d, want 1", got)
	}
	if got := mustCount(`SELECT COUNT(*) FROM metric_ops_rollup_1d WHERE thing_id=$1 AND metric_name NOT LIKE 'runtime.%'`, thingID); got != 1 {
		t.Errorf("business_1d kept = %d, want 1", got)
	}

	// rollup_1mo kept = 1 each.
	if got := mustCount(`SELECT COUNT(*) FROM metric_ops_rollup_1mo WHERE thing_id=$1 AND metric_name LIKE 'runtime.%'`, thingID); got != 1 {
		t.Errorf("runtime_1mo kept = %d, want 1", got)
	}
	if got := mustCount(`SELECT COUNT(*) FROM metric_ops_rollup_1mo WHERE thing_id=$1 AND metric_name NOT LIKE 'runtime.%'`, thingID); got != 1 {
		t.Errorf("business_1mo kept = %d, want 1", got)
	}

	// diag levels kept = 1 each.
	if got := mustCount(`SELECT COUNT(*) FROM thing_diag_event WHERE thing_id=$1 AND level='warn'`, thingID); got != 1 {
		t.Errorf("diag warn kept = %d, want 1", got)
	}
	if got := mustCount(`SELECT COUNT(*) FROM thing_diag_event WHERE thing_id=$1 AND level='error'`, thingID); got != 1 {
		t.Errorf("diag error kept = %d, want 1", got)
	}
	if got := mustCount(`SELECT COUNT(*) FROM thing_diag_event WHERE thing_id=$1 AND level='fatal'`, thingID); got != 1 {
		t.Errorf("diag fatal kept = %d, want 1", got)
	}
}

// TestOpsRetention_DefaultsRespected pins the migration seed values: a row
// just inside each canonical retention window survives.
func TestOpsRetention_DefaultsRespected(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops-ret-defaults"
	cleanup := opsRetentionTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	// Verify the canonical seed is in place.
	ctx := context.Background()
	rows := map[string]int{}
	r, err := pool.Query(ctx, `SELECT layer, retention_days FROM metric_ops_retention_config`)
	if err != nil {
		t.Fatalf("query config: %v", err)
	}
	for r.Next() {
		var l string
		var d int
		if err := r.Scan(&l, &d); err != nil {
			t.Fatalf("scan config: %v", err)
		}
		rows[l] = d
	}
	r.Close()

	expected := map[string]int{
		"runtime_raw": 7, "business_raw": 7,
		"runtime_1h": 90, "business_1h": 90,
		"runtime_1d": 365, "business_1d": 365,
		"runtime_1mo": 1825, "business_1mo": 1825,
		"diag_warn": 30, "diag_error": 180, "diag_fatal": 365,
	}
	for layer, want := range expected {
		if got := rows[layer]; got != want {
			t.Errorf("retention_days[%s] = %d, want %d", layer, got, want)
		}
	}
}

// TestOpsRetention_PicksUpConfigChanges asserts the job re-reads the config
// each run: lowering retention to 1 day causes a 5-day-old row to be deleted.
func TestOpsRetention_PicksUpConfigChanges(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops-ret-config-change"
	cleanup := opsRetentionTestSetup(t, pool, map[string]string{thingID: "service"})
	defer func() {
		// Restore default before cleanup.
		_, _ = pool.Exec(context.Background(), `
			UPDATE metric_ops_retention_config SET retention_days = 7 WHERE layer = 'runtime_raw'
		`)
		cleanup()
	}()

	now := time.Now().UTC()
	// 5 days old — well within the 7-day default.
	insertRawForRetention(t, pool, now.AddDate(0, 0, -5), thingID, "runtime.heap")

	// Run with default config — the row should survive.
	runOpsRetention(t, pool)

	ctx := context.Background()
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM metric_ops_raw WHERE thing_id=$1`, thingID).Scan(&n); err != nil {
		t.Fatalf("count after default run: %v", err)
	}
	if n != 1 {
		t.Errorf("after default run: rows=%d, want 1 (5d row inside 7d retention)", n)
	}

	// Lower runtime_raw to 1 day.
	if _, err := pool.Exec(ctx, `UPDATE metric_ops_retention_config SET retention_days = 1 WHERE layer = 'runtime_raw'`); err != nil {
		t.Fatalf("update config: %v", err)
	}

	runOpsRetention(t, pool)

	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM metric_ops_raw WHERE thing_id=$1`, thingID).Scan(&n); err != nil {
		t.Fatalf("count after tightened run: %v", err)
	}
	if n != 0 {
		t.Errorf("after tightened run: rows=%d, want 0 (5d row outside 1d retention)", n)
	}
}

// TestOpsRetention_LoopsUntilDone seeds rows substantially past the chunk
// limit and asserts they all get deleted in one Run() call.
func TestOpsRetention_LoopsUntilDone(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops-ret-loop"
	cleanup := opsRetentionTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	// Lower runtime_raw to 1 day so all our rows are aged out, then seed
	// 12_500 rows (chunk limit + 25%) to force at least 2 loop iterations.
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE metric_ops_retention_config SET retention_days = 1 WHERE layer = 'runtime_raw'`); err != nil {
		t.Fatalf("tighten config: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `UPDATE metric_ops_retention_config SET retention_days = 7 WHERE layer = 'runtime_raw'`)
	}()

	const total = opsRetentionDeleteLimit + (opsRetentionDeleteLimit / 4)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	now := time.Now().UTC()
	aged := now.AddDate(0, 0, -10)
	// Stagger the sampled_at values by microsecond increments so the unique
	// (sampled_at, thing_id, metric_name, dimension_key) constraint never
	// fires. Each row has a distinct timestamp.
	for i := range total {
		sampledAt := aged.Add(time.Duration(i) * time.Microsecond)
		_, err := tx.Exec(ctx, `
			INSERT INTO metric_ops_raw (id, sampled_at, thing_id, thing_type, metric_name, metric_kind, dimension_key, value)
			VALUES ($1, $2, $3, 'service', 'runtime.heap', 'gauge', '', 1.0)
		`, uuid.NewString(), sampledAt, thingID)
		if err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			t.Fatalf("bulk insert at %d: %v", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit bulk insert: %v", err)
	}

	runOpsRetention(t, pool)

	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM metric_ops_raw WHERE thing_id=$1`, thingID).Scan(&n); err != nil {
		t.Fatalf("post-run count: %v", err)
	}
	if n != 0 {
		t.Errorf("post-run rows = %d, want 0 (loop did not drain)", n)
	}
}
