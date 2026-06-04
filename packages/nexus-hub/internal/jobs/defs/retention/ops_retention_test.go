package retention

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pure (no-DB) identity + default tests.

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

// jobsTestPool + testLogger live in helpers_test.go (shared with the other
// retention DB tests). These DB-backed tests require a live Postgres and are
// skipped automatically when DATABASE_URL is unset / unreachable.
//
// Behavior under test reflects the post-partition design: ops-retention
// chunked-DELETEs the rollup tiers (5m/1h/1d/1mo) + thing_diag_event, but does
// NOT touch metric_ops_raw — raw is day-partitioned and aged out by the
// separate ops-raw-partition job (whole-day partition drops).

// runOpsRetention constructs the job with the shared pool and runs it once.
func runOpsRetention(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	j := NewOpsRetention(pool, 0, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("run ops retention: %v", err)
	}
}

// cleanupOpsRetention removes any rows for the given thing IDs across the
// metric_ops_* + thing_diag_event tables so each test starts clean.
func cleanupOpsRetention(t *testing.T, pool *pgxpool.Pool, ids []string) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{"metric_ops_rollup_5m", "metric_ops_rollup_1h", "metric_ops_rollup_1d", "metric_ops_rollup_1mo"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE thing_id = ANY($1)`, tbl), ids); err != nil {
			t.Fatalf("cleanup %s: %v", tbl, err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM thing_diag_event WHERE thing_id = ANY($1)`, ids); err != nil {
		t.Fatalf("cleanup diag: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM metric_ops_raw WHERE thing_id = ANY($1)`, ids); err != nil {
		t.Fatalf("cleanup raw: %v", err)
	}
}

// seedRetentionConfig sets retention_days for each layer used by the tests.
// Raw is intentionally absent: it is no longer a retention-config layer.
func seedRetentionConfig(t *testing.T, pool *pgxpool.Pool) {
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO metric_ops_retention_config (layer, retention_days, updated_at)
		VALUES
		  ('runtime_5m',    7, NOW()),
		  ('business_5m',   7, NOW()),
		  ('runtime_1h',    7, NOW()),
		  ('business_1h',   7, NOW()),
		  ('runtime_1d',    7, NOW()),
		  ('business_1d',   7, NOW()),
		  ('runtime_1mo',   7, NOW()),
		  ('business_1mo',  7, NOW()),
		  ('diag_info',     7, NOW()),
		  ('diag_warn',     7, NOW())
		ON CONFLICT (layer) DO UPDATE SET retention_days = EXCLUDED.retention_days
	`); err != nil {
		t.Fatalf("seed retention config: %v", err)
	}
}

// insertRawOpsSample writes one metric_ops_raw row at the given age in days.
func insertRawOpsSample(t *testing.T, pool *pgxpool.Pool, thingID string, ageDays int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO metric_ops_raw (id, sampled_at, thing_id, thing_type, metric_name, metric_kind, dimension_key, value)
		VALUES (gen_random_uuid(), NOW() - ($1 || ' days')::interval, $2, 'service', 'runtime.cpu', 'gauge', '', 1.0)
	`, ageDays, thingID)
	if err != nil {
		t.Fatalf("insert raw sample: %v", err)
	}
}

// insertRollupOpsSample writes one metric_ops_rollup_<tier> row at the given age.
func insertRollupOpsSample(t *testing.T, pool *pgxpool.Pool, table, thingID string, ageDays int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), fmt.Sprintf(`
		INSERT INTO %s (id, bucket_start, thing_id, thing_type, metric_name, metric_kind, dimension_key, value_avg, value_sum, value_min, value_max, sample_count)
		VALUES (gen_random_uuid(), NOW() - ($1 || ' days')::interval, $2, 'service', 'runtime.cpu', 'gauge', '', 1.0, 1.0, 1.0, 1.0, 1)
	`, table), ageDays, thingID)
	if err != nil {
		t.Fatalf("insert rollup sample: %v", err)
	}
}

// countOpsRows returns the row count for the given table + thing scope.
func countOpsRows(t *testing.T, pool *pgxpool.Pool, table, thingID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE thing_id = $1`, table), thingID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestOpsRetention_DeletesAgedRollupsRawExempt is the end-to-end DB test: seed
// rows at various ages across raw + the 5m/1h rollup tiers, run the job, and
// assert (a) aged rollup rows are gone, (b) fresh rollup rows survive, and
// (c) raw is left entirely untouched (the partition job owns raw, not this one).
func TestOpsRetention_DeletesAgedRollupsRawExempt(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	cleanupOpsRetention(t, pool, []string{"ret-raw", "ret-5m", "ret-1h", "ret-keep"})
	seedRetentionConfig(t, pool)

	// Two raw samples (one aged 10d, one fresh 1d). Both must survive: the
	// retention job never deletes raw.
	thingRaw := "ret-raw"
	insertRawOpsSample(t, pool, thingRaw, 10)
	insertRawOpsSample(t, pool, thingRaw, 1)

	// 5m tier: aged 10d (deleted at 7d) + fresh 1d (kept).
	insertRollupOpsSample(t, pool, "metric_ops_rollup_5m", "ret-5m", 10)
	insertRollupOpsSample(t, pool, "metric_ops_rollup_5m", "ret-5m", 1)

	// 1h tier: aged 10d (deleted) + fresh 1d (kept).
	insertRollupOpsSample(t, pool, "metric_ops_rollup_1h", "ret-1h", 10)
	insertRollupOpsSample(t, pool, "metric_ops_rollup_1h", "ret-1h", 1)

	// A thing whose rows are all fresh — must survive entirely.
	insertRollupOpsSample(t, pool, "metric_ops_rollup_1h", "ret-keep", 1)

	runOpsRetention(t, pool)

	// raw: untouched — both rows survive (partition job, not retention, ages raw).
	if got := countOpsRows(t, pool, "metric_ops_raw", thingRaw); got != 2 {
		t.Errorf("raw rows after retention = %d, want 2 (raw is exempt from chunked delete)", got)
	}
	// 5m: aged deleted, fresh kept.
	if got := countOpsRows(t, pool, "metric_ops_rollup_5m", "ret-5m"); got != 1 {
		t.Errorf("runtime_5m kept = %d, want 1", got)
	}
	// 1h: aged deleted, fresh kept.
	if got := countOpsRows(t, pool, "metric_ops_rollup_1h", "ret-1h"); got != 1 {
		t.Errorf("runtime_1h kept = %d, want 1", got)
	}
	// all-fresh thing untouched.
	if got := countOpsRows(t, pool, "metric_ops_rollup_1h", "ret-keep"); got != 1 {
		t.Errorf("fresh thing kept = %d, want 1", got)
	}
}

// TestOpsRetention_DiagPurge asserts the diag layers also purge.
func TestOpsRetention_DiagPurge(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	cleanupOpsRetention(t, pool, []string{"ret-diag"})
	seedRetentionConfig(t, pool)

	_, err := pool.Exec(context.Background(), `
		INSERT INTO thing_diag_event (id, thing_id, thing_type, occurred_at, received_at, level, event_type, source, message, message_hash)
		VALUES (gen_random_uuid(), $1, 'agent', NOW() - interval '10 days', NOW(), 'warn', 'test', 'test', 'm', 'h')
	`, "ret-diag")
	if err != nil {
		t.Fatalf("insert diag: %v", err)
	}

	runOpsRetention(t, pool)

	var n int
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM thing_diag_event WHERE thing_id = $1`, "ret-diag").Scan(&n)
	if n != 0 {
		t.Errorf("aged diag kept = %d, want 0 (warn retention 7d)", n)
	}
}

// TestOpsRetention_FreshKept asserts a freshly-aged row (younger than the
// retention horizon) survives a retention run across raw + rollup tiers.
func TestOpsRetention_FreshKept(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	cleanupOpsRetention(t, pool, []string{"ret-fresh"})
	seedRetentionConfig(t, pool)

	insertRawOpsSample(t, pool, "ret-fresh", 1)
	insertRollupOpsSample(t, pool, "metric_ops_rollup_5m", "ret-fresh", 1)
	insertRollupOpsSample(t, pool, "metric_ops_rollup_1h", "ret-fresh", 1)

	runOpsRetention(t, pool)

	if got := countOpsRows(t, pool, "metric_ops_raw", "ret-fresh"); got != 1 {
		t.Errorf("fresh raw kept = %d, want 1", got)
	}
	if got := countOpsRows(t, pool, "metric_ops_rollup_5m", "ret-fresh"); got != 1 {
		t.Errorf("fresh 5m rollup kept = %d, want 1", got)
	}
	if got := countOpsRows(t, pool, "metric_ops_rollup_1h", "ret-fresh"); got != 1 {
		t.Errorf("fresh 1h rollup kept = %d, want 1", got)
	}
	// runtime_5m config row exists (the smallest chunked-delete tier).
	var cfg int
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM metric_ops_retention_config WHERE layer = 'runtime_5m'`).Scan(&cfg)
	if cfg < 1 {
		t.Errorf("runtime_5m config row missing = %d", cfg)
	}
}

// TestOpsRetention_ZeroRetentionDisables asserts a layer with retention_days
// <= 0 is treated as "keep forever" (no deletes for that layer).
func TestOpsRetention_ZeroRetentionDisables(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	cleanupOpsRetention(t, pool, []string{"ret-zero"})
	seedRetentionConfig(t, pool)

	// Seed an aged 5m rollup row that would be deleted at 7d.
	insertRollupOpsSample(t, pool, "metric_ops_rollup_5m", "ret-zero", 10)
	// Disable the runtime_5m layer (retention_days = 0 → keep forever).
	if _, err := pool.Exec(context.Background(), `UPDATE metric_ops_retention_config SET retention_days = 0 WHERE layer = 'runtime_5m'`); err != nil {
		t.Fatalf("disable runtime_5m: %v", err)
	}

	runOpsRetention(t, pool)

	// The aged 5m row survives because runtime_5m is disabled.
	if got := countOpsRows(t, pool, "metric_ops_rollup_5m", "ret-zero"); got != 1 {
		t.Errorf("disabled-layer 5m kept = %d, want 1", got)
	}
}
