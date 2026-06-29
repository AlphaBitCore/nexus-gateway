package metrics

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/opsmetrics"
	rollupstore "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/rollup"
)

// Pure (no-DB) identity tests.

func TestOpsRollup1d_Identity(t *testing.T) {
	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	if j.ID() != "ops-rollup-1d" {
		t.Errorf("ID = %q, want ops-rollup-1d", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h", j.Interval())
	}
}

func TestOpsRollup1d_IntervalDefault(t *testing.T) {
	j := NewOpsRollup1d(nil, 0, testLogger())
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h default", j.Interval())
	}
}

func TestOpsRollup1mo_Identity(t *testing.T) {
	j := NewOpsRollup1mo(nil, 24*time.Hour, testLogger())
	if j.ID() != "ops-rollup-1mo" {
		t.Errorf("ID = %q, want ops-rollup-1mo", j.ID())
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

func TestOpsRollup1mo_IntervalDefault(t *testing.T) {
	j := NewOpsRollup1mo(nil, 0, testLogger())
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h default", j.Interval())
	}
}

// DB-backed test helpers.

// opsCascadeTestSetup wipes ops-cascade state for the test thing IDs and the
// given watermark name, then seeds a fresh thing row per ID.
func opsCascadeTestSetup(t *testing.T, pool *pgxpool.Pool, things map[string]string, watermarkName, sourceTable, targetTable string) func() {
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
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE thing_id = $1`, sourceTable), id); err != nil {
			t.Fatalf("wipe source: %v", err)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE thing_id = $1`, targetTable), id); err != nil {
			t.Fatalf("wipe target: %v", err)
		}
	}

	if _, err := pool.Exec(ctx, `DELETE FROM rollup_watermark WHERE "jobName" = $1`, watermarkName); err != nil {
		t.Fatalf("reset watermark: %v", err)
	}

	return func() {
		for id := range things {
			_, _ = pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE thing_id = $1`, targetTable), id)
			_, _ = pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE thing_id = $1`, sourceTable), id)
			_, _ = pool.Exec(ctx, `DELETE FROM thing WHERE id = $1`, id)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM rollup_watermark WHERE "jobName" = $1`, watermarkName)
	}
}

// insertSourceRollupRow writes one row into a rollup_1h or rollup_1d table.
// thingID may be nil to represent a fleet aggregate.
func insertSourceRollupRow(t *testing.T, pool *pgxpool.Pool, table string, bucketStart time.Time, thingID *string, thingType, name, kind, dim string, avg, sum, min, max *float64, sampleCount int, histBuckets []int) {
	t.Helper()
	ctx := context.Background()

	var meta any
	if histBuckets != nil {
		b, err := json.Marshal(map[string]any{"buckets": histBuckets})
		if err != nil {
			t.Fatalf("marshal buckets: %v", err)
		}
		meta = string(b)
	}

	q := fmt.Sprintf(`
		INSERT INTO %s
			(id, bucket_start, thing_id, thing_type, metric_name, metric_kind, dimension_key,
			 value_avg, value_sum, value_min, value_max, sample_count, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb)
	`, table)

	if _, err := pool.Exec(ctx, q,
		uuid.NewString(), bucketStart, thingID, thingType, name, kind, dim,
		avg, sum, min, max, sampleCount, meta,
	); err != nil {
		t.Fatalf("insert source row: %v", err)
	}
}

// fp returns a pointer to the given float64 value.
func fp(v float64) *float64 { return &v }

// alignedDayBefore returns the start of the UTC day that ended at least
// gracePeriod ago.
func alignedDayBefore(t *testing.T, daysAgo int) time.Time {
	t.Helper()
	return time.Now().UTC().Truncate(24 * time.Hour).Add(-time.Duration(daysAgo) * 24 * time.Hour)
}

func runOpsRollup1d(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	j := NewOpsRollup1d(pool, time.Minute, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run 1d: %v", err)
	}
}

func runOpsRollup1mo(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	j := NewOpsRollup1mo(pool, time.Minute, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run 1mo: %v", err)
	}
}

// 1d cascade tests.

// TestOpsRollup1d_AggregatesFromOneHour pins sample-count-weighted averaging
// across 1h source rows: weighted_avg = SUM(avg * count) / SUM(count).
func TestOpsRollup1d_AggregatesFromOneHour(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops1d-svc-weighted"
	cleanup := opsCascadeTestSetup(t, pool, map[string]string{thingID: "service"}, "ops_1d", "metric_ops_rollup_1h", "metric_ops_rollup_1d")
	defer cleanup()

	day := alignedDayBefore(t, 2)
	// Two 1h source rows in the same day. Different averages + counts so the
	// weighted average is distinct from the unweighted average.
	//   row A: avg=10, count=10  →  sum_contrib = 100
	//   row B: avg=20, count=40  →  sum_contrib = 800
	//   weighted = 900 / 50 = 18    (unweighted would be 15)
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1h",
		day.Add(time.Hour), &thingID, "service",
		"runtime.heap_alloc_bytes", "gauge", "",
		fp(10), fp(100), fp(5), fp(15), 10, nil)
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1h",
		day.Add(2*time.Hour), &thingID, "service",
		"runtime.heap_alloc_bytes", "gauge", "",
		fp(20), fp(800), fp(8), fp(40), 40, nil)

	runOpsRollup1d(t, pool)

	ctx := context.Background()
	var avg, sum, min, max float64
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT value_avg, value_sum, value_min, value_max, sample_count
		  FROM metric_ops_rollup_1d
		 WHERE bucket_start = $1 AND thing_id = $2 AND metric_name = 'runtime.heap_alloc_bytes'
	`, day, thingID).Scan(&avg, &sum, &min, &max, &count); err != nil {
		t.Fatalf("scan 1d row: %v", err)
	}

	if avg != 18 {
		t.Errorf("weighted avg = %v, want 18 (= (10*10 + 20*40) / 50)", avg)
	}
	if sum != 900 {
		t.Errorf("sum = %v, want 900", sum)
	}
	if min != 5 {
		t.Errorf("min = %v, want 5", min)
	}
	if max != 40 {
		t.Errorf("max = %v, want 40", max)
	}
	if count != 50 {
		t.Errorf("sample_count = %d, want 50", count)
	}
}

// TestOpsRollup1d_PreservesFleetVsPerThingDistinction asserts that thing_id
// NULL rows in the 1h source remain fleet rows in the 1d target, and per-thing
// 1h rows remain per-thing in 1d.
func TestOpsRollup1d_PreservesFleetVsPerThingDistinction(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	perThingID := "test-ops1d-perthing"
	cleanup := opsCascadeTestSetup(t, pool, map[string]string{perThingID: "agent"}, "ops_1d", "metric_ops_rollup_1h", "metric_ops_rollup_1d")
	defer cleanup()

	// Wipe any leftover fleet rows in our test day so the fleet count is
	// deterministic. Fleet rows have thing_id NULL so we can't scope them by
	// thingID; rely on bucket_start + metric_name + thing_type.
	day := alignedDayBefore(t, 3)
	dayEnd := day.Add(24 * time.Hour)
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `
		DELETE FROM metric_ops_rollup_1h
		 WHERE bucket_start >= $1 AND bucket_start < $2
		   AND thing_id IS NULL
		   AND metric_name = 'agent.test_fleet_metric'
	`, day, dayEnd)
	_, _ = pool.Exec(ctx, `
		DELETE FROM metric_ops_rollup_1d
		 WHERE bucket_start = $1
		   AND thing_id IS NULL
		   AND metric_name = 'agent.test_fleet_metric'
	`, day)
	defer func() {
		_, _ = pool.Exec(ctx, `
			DELETE FROM metric_ops_rollup_1h
			 WHERE bucket_start >= $1 AND bucket_start < $2
			   AND thing_id IS NULL
			   AND metric_name = 'agent.test_fleet_metric'
		`, day, dayEnd)
		_, _ = pool.Exec(ctx, `
			DELETE FROM metric_ops_rollup_1d
			 WHERE bucket_start = $1
			   AND thing_id IS NULL
			   AND metric_name = 'agent.test_fleet_metric'
		`, day)
	}()

	// Fleet row in 1h source.
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1h",
		day.Add(time.Hour), nil, "agent",
		"agent.test_fleet_metric", "counter", "",
		fp(2), fp(4), fp(1), fp(3), 2, nil)
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1h",
		day.Add(2*time.Hour), nil, "agent",
		"agent.test_fleet_metric", "counter", "",
		fp(3), fp(6), fp(2), fp(4), 2, nil)

	// Per-thing row in 1h source.
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1h",
		day.Add(time.Hour), &perThingID, "agent",
		"agent.test_perthing_metric", "counter", "",
		fp(7), fp(14), fp(7), fp(7), 2, nil)

	runOpsRollup1d(t, pool)

	// Fleet row should be in 1d target with thing_id NULL.
	var fleetCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM metric_ops_rollup_1d
		 WHERE bucket_start = $1 AND thing_id IS NULL
		   AND metric_name = 'agent.test_fleet_metric'
	`, day).Scan(&fleetCount); err != nil {
		t.Fatalf("scan fleet: %v", err)
	}
	if fleetCount != 1 {
		t.Errorf("fleet rows = %d, want 1", fleetCount)
	}

	// Per-thing row should remain per-thing.
	var perThingCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM metric_ops_rollup_1d
		 WHERE bucket_start = $1 AND thing_id = $2
		   AND metric_name = 'agent.test_perthing_metric'
	`, day, perThingID).Scan(&perThingCount); err != nil {
		t.Fatalf("scan perthing: %v", err)
	}
	if perThingCount != 1 {
		t.Errorf("per-thing rows = %d, want 1", perThingCount)
	}
}

// TestOpsRollup1d_HistogramMerges asserts element-wise merge of histogram rows
// from 1h source into one 1d row.
func TestOpsRollup1d_HistogramMerges(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops1d-hist-svc"
	cleanup := opsCascadeTestSetup(t, pool, map[string]string{thingID: "service"}, "ops_1d", "metric_ops_rollup_1h", "metric_ops_rollup_1d")
	defer cleanup()

	day := alignedDayBefore(t, 2)
	// Two 1h histogram source rows. Element-wise sum = [11, 7, 5, 2, 0, 0].
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1h",
		day.Add(time.Hour), &thingID, "service",
		"hook.pipeline_ms", "histogram", "stage=request",
		nil, fp(0), nil, nil, 10,
		[]int{10, 5, 2, 1, 0, 0})
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1h",
		day.Add(2*time.Hour), &thingID, "service",
		"hook.pipeline_ms", "histogram", "stage=request",
		nil, fp(0), nil, nil, 5,
		[]int{1, 2, 3, 1, 0, 0})

	runOpsRollup1d(t, pool)

	ctx := context.Background()
	var metaRaw []byte
	var sampleCount int
	if err := pool.QueryRow(ctx, `
		SELECT metadata, sample_count FROM metric_ops_rollup_1d
		 WHERE bucket_start = $1 AND thing_id = $2 AND metric_name = 'hook.pipeline_ms'
	`, day, thingID).Scan(&metaRaw, &sampleCount); err != nil {
		t.Fatalf("scan histogram row: %v", err)
	}
	got, err := opsmetrics.ParseHistogramBuckets(metaRaw)
	if err != nil {
		t.Fatalf("parse merged histogram: %v", err)
	}
	want := opsmetrics.HistogramBuckets{11, 7, 5, 2, 0, 0}
	if got != want {
		t.Errorf("merged buckets = %v, want %v", got, want)
	}
	// Sample-count is summed from source rows.
	if sampleCount != 15 {
		t.Errorf("sample_count = %d, want 15 (10+5)", sampleCount)
	}
}

// TestOpsRollup1d_BootstrapsFromMinBucketStart asserts the bootstrap path: a
// future-seeded watermark must not skip historical 1h source rows.
func TestOpsRollup1d_BootstrapsFromMinBucketStart(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops1d-bootstrap"
	cleanup := opsCascadeTestSetup(t, pool, map[string]string{thingID: "service"}, "ops_1d", "metric_ops_rollup_1h", "metric_ops_rollup_1d")
	defer cleanup()

	day := alignedDayBefore(t, 4)
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1h",
		day.Add(time.Hour), &thingID, "service",
		"relay.dial_total", "counter", "",
		fp(9), fp(9), fp(9), fp(9), 1, nil)

	// Seed watermark in the future so bootstrap path is the only way the row
	// gets processed.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO rollup_watermark ("jobName", "watermark", "updatedAt")
		VALUES ('ops_1d', NOW(), NOW())
		ON CONFLICT ("jobName") DO UPDATE SET "watermark" = EXCLUDED."watermark"
	`); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	runOpsRollup1d(t, pool)

	var sum float64
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(value_sum, 0) FROM metric_ops_rollup_1d
		 WHERE bucket_start = $1 AND thing_id = $2 AND metric_name = 'relay.dial_total'
	`, day, thingID).Scan(&sum); err != nil {
		t.Fatalf("scan bootstrap row: %v (bootstrap path broken)", err)
	}
	if sum != 9 {
		t.Errorf("bootstrap sum = %v, want 9", sum)
	}
}

// TestOpsRollup1d_IdempotentReRun asserts the DELETE+INSERT cycle converges.
func TestOpsRollup1d_IdempotentReRun(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops1d-idempotent"
	cleanup := opsCascadeTestSetup(t, pool, map[string]string{thingID: "service"}, "ops_1d", "metric_ops_rollup_1h", "metric_ops_rollup_1d")
	defer cleanup()

	day := alignedDayBefore(t, 2)
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1h",
		day.Add(time.Hour), &thingID, "service",
		"relay.dial_total", "counter", "",
		fp(4), fp(4), fp(4), fp(4), 1, nil)
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1h",
		day.Add(2*time.Hour), &thingID, "service",
		"relay.dial_total", "counter", "",
		fp(6), fp(6), fp(6), fp(6), 1, nil)

	runOpsRollup1d(t, pool)
	first := dumpRollup1d(t, pool, day, thingID)

	// Reset watermark and re-run.
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM rollup_watermark WHERE "jobName" = 'ops_1d'`); err != nil {
		t.Fatalf("reset watermark: %v", err)
	}
	runOpsRollup1d(t, pool)
	second := dumpRollup1d(t, pool, day, thingID)

	if first != second {
		t.Errorf("re-run not idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// dumpRollup1d returns a stable string snapshot of all 1d rollup rows for the
// (bucket, thing) so two runs can be compared for idempotency.
func dumpRollup1d(t *testing.T, pool *pgxpool.Pool, bucket time.Time, thingID string) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT metric_name, metric_kind, dimension_key,
		       COALESCE(value_avg, 0), COALESCE(value_sum, 0),
		       COALESCE(value_min, 0), COALESCE(value_max, 0),
		       sample_count
		  FROM metric_ops_rollup_1d
		 WHERE bucket_start = $1 AND thing_id = $2
		 ORDER BY metric_name, dimension_key
	`, bucket, thingID)
	if err != nil {
		t.Fatalf("dump rollup: %v", err)
	}
	defer rows.Close()

	var out string
	for rows.Next() {
		var name, kind, dim string
		var avg, sum, mn, mx float64
		var cnt int
		if err := rows.Scan(&name, &kind, &dim, &avg, &sum, &mn, &mx, &cnt); err != nil {
			t.Fatalf("scan dump: %v", err)
		}
		out += fmt.Sprintf("%s|%s|%s|%v|%v|%v|%v|%d\n", name, kind, dim, avg, sum, mn, mx, cnt)
	}
	return out
}

// 1mo cascade tests.

// TestOpsRollup1mo_VariableMonthLength pins calendar-month bucketing: the
// bucket boundary advances by exactly one calendar month, regardless of length
// (Feb 28/29 vs 31).
func TestOpsRollup1mo_VariableMonthLength(t *testing.T) {
	// Pure (no DB) test of the helper. February 2024 was a leap year (29 days);
	// March 2024 has 31 days. nextOpsMonth must advance by one calendar month
	// in either case.
	feb1 := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	mar1 := nextOpsMonth(feb1)
	if !mar1.Equal(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Feb→Mar = %v, want 2024-03-01", mar1)
	}

	apr1 := nextOpsMonth(mar1)
	if !apr1.Equal(time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Mar→Apr = %v, want 2024-04-01", apr1)
	}

	// Year wraparound.
	dec1 := time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC)
	jan1 := nextOpsMonth(dec1)
	if !jan1.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Dec→Jan = %v, want 2025-01-01", jan1)
	}
}

// TestOpsRollup1mo_AggregatesFromOneDay seeds 1d rows in a recent past month
// and asserts the 1mo cascade folds them into one row per (thing, metric, dim).
func TestOpsRollup1mo_AggregatesFromOneDay(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops1mo-svc-weighted"
	cleanup := opsCascadeTestSetup(t, pool, map[string]string{thingID: "service"}, "ops_1mo", "metric_ops_rollup_1d", "metric_ops_rollup_1mo")
	defer cleanup()

	// Use last calendar month (sealed). For the test to be deterministic we
	// pick the first day of the previous month and seed two 1d rows inside it.
	now := time.Now().UTC()
	prevMonthStart := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)

	// Two daily source rows with distinct sample-count weighting:
	//   day-1: avg=10, count=10  → contrib = 100
	//   day-2: avg=20, count=40  → contrib = 800
	//   weighted = 900 / 50 = 18
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1d",
		prevMonthStart.Add(24*time.Hour), &thingID, "service",
		"runtime.heap_alloc_bytes", "gauge", "",
		fp(10), fp(100), fp(5), fp(15), 10, nil)
	insertSourceRollupRow(t, pool, "metric_ops_rollup_1d",
		prevMonthStart.Add(48*time.Hour), &thingID, "service",
		"runtime.heap_alloc_bytes", "gauge", "",
		fp(20), fp(800), fp(8), fp(40), 40, nil)

	runOpsRollup1mo(t, pool)

	ctx := context.Background()
	var avg, sum, min, max float64
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT value_avg, value_sum, value_min, value_max, sample_count
		  FROM metric_ops_rollup_1mo
		 WHERE bucket_start = $1 AND thing_id = $2 AND metric_name = 'runtime.heap_alloc_bytes'
	`, prevMonthStart, thingID).Scan(&avg, &sum, &min, &max, &count); err != nil {
		t.Fatalf("scan 1mo row: %v", err)
	}

	if avg != 18 {
		t.Errorf("weighted avg = %v, want 18", avg)
	}
	if sum != 900 {
		t.Errorf("sum = %v, want 900", sum)
	}
	if min != 5 {
		t.Errorf("min = %v, want 5", min)
	}
	if max != 40 {
		t.Errorf("max = %v, want 40", max)
	}
	if count != 50 {
		t.Errorf("sample_count = %d, want 50", count)
	}

	// Watermark should be at prevMonthStart.
	wm, err := rollupstore.GetWatermark(ctx, pool, "ops_1mo")
	if err != nil {
		t.Fatalf("get watermark: %v", err)
	}
	if !wm.Equal(prevMonthStart) {
		t.Errorf("watermark = %v, want %v", wm, prevMonthStart)
	}
}
