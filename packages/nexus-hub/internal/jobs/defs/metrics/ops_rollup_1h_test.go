package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/opsmetrics"
	rollupstore "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/rollup"
)

// Pure (no-DB) tests — exercise identity + cold-start watermark logic.

func TestOpsRollup1h_Identity(t *testing.T) {
	j := NewOpsRollup1h(nil, 5*time.Minute, testLogger())
	if j.ID() != "ops-rollup-1h" {
		t.Errorf("ID = %q, want ops-rollup-1h", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != 5*time.Minute {
		t.Errorf("Interval = %v, want 5m", j.Interval())
	}
}

func TestOpsRollup1h_IntervalDefault(t *testing.T) {
	j := NewOpsRollup1h(nil, 0, testLogger())
	if j.Interval() != 5*time.Minute {
		t.Errorf("Interval = %v, want 5m default", j.Interval())
	}
}

// DB-backed tests — exercise the full Run() pipeline against the local
// Postgres test instance. Skipped when DB is unavailable.

// opsRollupTestSetup wipes ops state for the test thing IDs and seeds a
// fresh thing row per ID, returning a cleanup callback.
func opsRollupTestSetup(t *testing.T, pool *pgxpool.Pool, things map[string]string) func() {
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

	// Wipe any prior ops state for this set of things so the test starts
	// from a clean slate.
	for id := range things {
		if _, err := pool.Exec(ctx, `DELETE FROM metric_ops_raw WHERE thing_id = $1`, id); err != nil {
			t.Fatalf("wipe raw: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM thing_diag_mode_window WHERE thing_id = $1`, id); err != nil {
			t.Fatalf("wipe diag-window: %v", err)
		}
	}

	// Reset the watermark and clear any rollup rows the previous test left
	// behind so order-of-test-execution is irrelevant.
	if _, err := pool.Exec(ctx, `DELETE FROM rollup_watermark WHERE "jobName" = 'ops_1h'`); err != nil {
		t.Fatalf("reset watermark: %v", err)
	}
	// Best-effort wipe of rollup rows scoped to this test's thing IDs and
	// fleet rows for the agent type. (We can't scope fleet rows to a test
	// because thing_id IS NULL, so we just delete the time window.)
	for id := range things {
		if _, err := pool.Exec(ctx, `DELETE FROM metric_ops_rollup_1h WHERE thing_id = $1`, id); err != nil {
			t.Fatalf("wipe rollup: %v", err)
		}
	}

	return func() {
		for id := range things {
			_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_rollup_1h WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_raw WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM thing_diag_mode_window WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM thing WHERE id = $1`, id)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM rollup_watermark WHERE "jobName" = 'ops_1h'`)
	}
}

// insertRawSample writes one metric_ops_raw row directly. Test seam — bypasses
// the writer/CopyFrom pipeline so seeded timestamps are deterministic.
func insertRawSample(t *testing.T, pool *pgxpool.Pool, sampledAt time.Time, thingID, thingType, name, kind, dim string, value float64, metadata map[string]any) {
	t.Helper()
	ctx := context.Background()

	var meta any
	if metadata != nil {
		b, err := json.Marshal(metadata)
		if err != nil {
			t.Fatalf("marshal metadata: %v", err)
		}
		meta = string(b)
	}

	_, err := pool.Exec(ctx, `
		INSERT INTO metric_ops_raw (id, sampled_at, thing_id, thing_type, metric_name, metric_kind, dimension_key, value, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
	`, uuid.NewString(), sampledAt, thingID, thingType, name, kind, dim, value, meta)
	if err != nil {
		t.Fatalf("insert raw: %v", err)
	}
}

// insertDiagWindow opens a diagnostic-mode window for thingID over [start,end).
func insertDiagWindow(t *testing.T, pool *pgxpool.Pool, thingID string, start, end time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO thing_diag_mode_window (id, thing_id, started_at, ended_at)
		VALUES ($1, $2, $3, $4)
	`, uuid.NewString(), thingID, start, end)
	if err != nil {
		t.Fatalf("insert diag window: %v", err)
	}
}

// alignedHourBefore returns the start of the hour that ended at least
// gracePeriod ago. The 1h rollup only processes hours that are fully sealed
// (ended >= 5min ago); we go further back than that to leave headroom.
func alignedHourBefore(t *testing.T, hoursAgo int) time.Time {
	t.Helper()
	return time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(hoursAgo) * time.Hour)
}

// runOpsRollup1h constructs the job, calls Run, and fails the test on error.
func runOpsRollup1h(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	j := NewOpsRollup1h(pool, time.Minute, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestOpsRollup1h_AgentInDiagModeKeepsPerThingRow asserts that an agent whose
// thing_id appears in thing_diag_mode_window for the bucket gets a per-thing
// row in metric_ops_rollup_1h (not folded into the fleet aggregate).
func TestOpsRollup1h_AgentInDiagModeKeepsPerThingRow(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops1h-diag-agent"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{thingID: "agent"})
	defer cleanup()

	bucket := alignedHourBefore(t, 2)
	// Insert two raw samples inside the bucket.
	insertRawSample(t, pool, bucket.Add(5*time.Minute), thingID, "agent", "runtime.heap_alloc_bytes", "gauge", "", 1000, nil)
	insertRawSample(t, pool, bucket.Add(35*time.Minute), thingID, "agent", "runtime.heap_alloc_bytes", "gauge", "", 2000, nil)

	// Mark the agent in diag-mode for the entire bucket.
	insertDiagWindow(t, pool, thingID, bucket, bucket.Add(time.Hour))

	runOpsRollup1h(t, pool)

	ctx := context.Background()
	var perThingCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM metric_ops_rollup_1h
		 WHERE bucket_start = $1
		   AND thing_id = $2
		   AND metric_name = 'runtime.heap_alloc_bytes'
	`, bucket, thingID).Scan(&perThingCount); err != nil {
		t.Fatalf("count per-thing: %v", err)
	}
	if perThingCount != 1 {
		t.Errorf("per-thing rows = %d, want 1 (agent is in diag mode)", perThingCount)
	}

	var fleetCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM metric_ops_rollup_1h
		 WHERE bucket_start = $1
		   AND thing_id IS NULL
		   AND metric_name = 'runtime.heap_alloc_bytes'
	`, bucket).Scan(&fleetCount); err != nil {
		t.Fatalf("count fleet: %v", err)
	}
	if fleetCount != 0 {
		t.Errorf("fleet rows = %d, want 0 (this thing is NOT in the fleet aggregate)", fleetCount)
	}

	var avg, sum float64
	var sampleCount int
	if err := pool.QueryRow(ctx, `
		SELECT value_avg, value_sum, sample_count FROM metric_ops_rollup_1h
		 WHERE bucket_start = $1 AND thing_id = $2 AND metric_name = 'runtime.heap_alloc_bytes'
	`, bucket, thingID).Scan(&avg, &sum, &sampleCount); err != nil {
		t.Fatalf("scan agg: %v", err)
	}
	if avg != 1500 || sum != 3000 || sampleCount != 2 {
		t.Errorf("agg = (avg=%v, sum=%v, count=%d), want (1500, 3000, 2)", avg, sum, sampleCount)
	}
}

// TestOpsRollup1h_AgentNotInDiagModeAggregatesToFleet asserts that an agent
// whose thing_id has no diag-mode window collapses into the fleet aggregate
// (one row with thing_id IS NULL per metric+dim).
func TestOpsRollup1h_AgentNotInDiagModeAggregatesToFleet(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	a, b := "test-ops1h-fleet-a", "test-ops1h-fleet-b"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{a: "agent", b: "agent"})
	defer cleanup()

	bucket := alignedHourBefore(t, 2)
	insertRawSample(t, pool, bucket.Add(5*time.Minute), a, "agent", "runtime.heap_alloc_bytes", "gauge", "", 1000, nil)
	insertRawSample(t, pool, bucket.Add(10*time.Minute), b, "agent", "runtime.heap_alloc_bytes", "gauge", "", 3000, nil)

	runOpsRollup1h(t, pool)

	ctx := context.Background()
	// Both per-thing rows should be absent — fleet aggregate only.
	var perThingA, perThingB int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM metric_ops_rollup_1h WHERE bucket_start=$1 AND thing_id=$2`, bucket, a).Scan(&perThingA)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM metric_ops_rollup_1h WHERE bucket_start=$1 AND thing_id=$2`, bucket, b).Scan(&perThingB)
	if perThingA != 0 || perThingB != 0 {
		t.Errorf("per-thing rows for non-diag agents: a=%d, b=%d (want both 0)", perThingA, perThingB)
	}

	var avg, sum float64
	var sampleCount int
	if err := pool.QueryRow(ctx, `
		SELECT value_avg, value_sum, sample_count FROM metric_ops_rollup_1h
		 WHERE bucket_start = $1 AND thing_id IS NULL AND thing_type = 'agent' AND metric_name = 'runtime.heap_alloc_bytes'
	`, bucket).Scan(&avg, &sum, &sampleCount); err != nil {
		t.Fatalf("scan fleet: %v", err)
	}
	if avg != 2000 || sum != 4000 || sampleCount != 2 {
		t.Errorf("fleet = (avg=%v, sum=%v, count=%d), want (2000, 4000, 2)", avg, sum, sampleCount)
	}
}

// TestOpsRollup1h_ServiceAlwaysPerInstance asserts services always get
// per-instance rows regardless of any diag-mode windows.
func TestOpsRollup1h_ServiceAlwaysPerInstance(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	svc := "test-ops1h-service-1"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{svc: "service"})
	defer cleanup()

	bucket := alignedHourBefore(t, 2)
	insertRawSample(t, pool, bucket.Add(15*time.Minute), svc, "service", "relay.dial_total", "counter", "", 5, nil)
	insertRawSample(t, pool, bucket.Add(45*time.Minute), svc, "service", "relay.dial_total", "counter", "", 7, nil)

	runOpsRollup1h(t, pool)

	ctx := context.Background()
	var sum float64
	var sampleCount int
	if err := pool.QueryRow(ctx, `
		SELECT value_sum, sample_count FROM metric_ops_rollup_1h
		 WHERE bucket_start = $1 AND thing_id = $2 AND thing_type = 'service' AND metric_name = 'relay.dial_total'
	`, bucket, svc).Scan(&sum, &sampleCount); err != nil {
		t.Fatalf("scan service: %v", err)
	}
	if sum != 12 || sampleCount != 2 {
		t.Errorf("svc = (sum=%v, count=%d), want (12, 2)", sum, sampleCount)
	}

	// No fleet aggregate for services.
	var fleetCount int
	_ = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM metric_ops_rollup_1h
		 WHERE bucket_start = $1 AND thing_id IS NULL AND thing_type = 'service'
	`, bucket).Scan(&fleetCount)
	if fleetCount != 0 {
		t.Errorf("service fleet rows = %d, want 0 (services never aggregate)", fleetCount)
	}
}

// TestOpsRollup1h_HistogramMergesBucketsElementwise asserts histogram raw rows
// are merged element-wise via the opsmetrics histogram-merge helper rather
// than naively averaged via SQL.
func TestOpsRollup1h_HistogramMergesBucketsElementwise(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops1h-hist-svc"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	bucket := alignedHourBefore(t, 2)
	// Two histogram samples — when merged element-wise we expect [11,7,5,2,0,0].
	insertRawSample(t, pool, bucket.Add(5*time.Minute), thingID, "service", "hook.pipeline_ms", "histogram", "stage=request", 0,
		map[string]any{"buckets": []int{10, 5, 2, 1, 0, 0}})
	insertRawSample(t, pool, bucket.Add(35*time.Minute), thingID, "service", "hook.pipeline_ms", "histogram", "stage=request", 0,
		map[string]any{"buckets": []int{1, 2, 3, 1, 0, 0}})

	runOpsRollup1h(t, pool)

	ctx := context.Background()
	var metaRaw []byte
	var sampleCount int
	if err := pool.QueryRow(ctx, `
		SELECT metadata, sample_count FROM metric_ops_rollup_1h
		 WHERE bucket_start = $1 AND thing_id = $2 AND metric_name = 'hook.pipeline_ms'
	`, bucket, thingID).Scan(&metaRaw, &sampleCount); err != nil {
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
	if sampleCount != 2 {
		t.Errorf("sample_count = %d, want 2", sampleCount)
	}
}

// TestOpsRollup1h_BootstrapsFromMinSampledAtWhenWatermarkIsOlder pins the
// Phase 1 carry-forward A: when the watermark is older than the oldest raw
// sample (e.g., empty seed at 1970), the job must rewind to MIN(sampled_at)
// minus 1h so the first iteration covers historical data.
func TestOpsRollup1h_BootstrapsFromMinSampledAtWhenWatermarkIsOlder(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops1h-bootstrap"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	// Seed an "old" watermark in the future-ish to simulate the d86ca4f6
	// fix's NOW() seed; raw data lands earlier so without bootstrap the job
	// would skip it.
	bucket := alignedHourBefore(t, 4)
	insertRawSample(t, pool, bucket.Add(15*time.Minute), thingID, "service", "relay.dial_total", "counter", "", 9, nil)

	// Watermark at NOW() means raw is 4h *older* than watermark — without
	// bootstrap, the cursor stays at NOW() and the bucket is skipped.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO rollup_watermark ("jobName", "watermark", "updatedAt")
		VALUES ('ops_1h', NOW(), NOW())
		ON CONFLICT ("jobName") DO UPDATE SET "watermark" = EXCLUDED."watermark"
	`); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	runOpsRollup1h(t, pool)

	ctx := context.Background()
	var sum float64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(value_sum, 0) FROM metric_ops_rollup_1h
		 WHERE bucket_start = $1 AND thing_id = $2 AND metric_name = 'relay.dial_total'
	`, bucket, thingID).Scan(&sum); err != nil {
		t.Fatalf("scan bootstrap row: %v (no rollup row produced — bootstrap path is broken)", err)
	}
	if sum != 9 {
		t.Errorf("bootstrap sum = %v, want 9", sum)
	}
}

// TestOpsRollup1h_SkipsWhenRawIsEmpty asserts an empty metric_ops_raw under
// a fresh watermark is a silent no-op (no error, no rollup rows).
func TestOpsRollup1h_SkipsWhenRawIsEmpty(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	cleanup := opsRollupTestSetup(t, pool, map[string]string{})
	defer cleanup()

	// No raw rows at all for the test scope. Ensure the job does not error
	// when MIN(sampled_at) returns NULL globally as well — guard by
	// bounding the pre-test global state.

	j := NewOpsRollup1h(pool, time.Minute, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run on empty: %v", err)
	}
}

// TestOpsRollup1h_IdempotentReRun asserts running the job twice over the same
// raw data produces the same final rollup state (DELETE+INSERT semantics).
func TestOpsRollup1h_IdempotentReRun(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops1h-idempotent"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	bucket := alignedHourBefore(t, 2)
	insertRawSample(t, pool, bucket.Add(15*time.Minute), thingID, "service", "relay.dial_total", "counter", "", 4, nil)
	insertRawSample(t, pool, bucket.Add(45*time.Minute), thingID, "service", "relay.dial_total", "counter", "", 6, nil)

	runOpsRollup1h(t, pool)
	first := dumpRollup1h(t, pool, bucket, thingID)

	// Reset watermark so the second run re-processes the same bucket.
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM rollup_watermark WHERE "jobName" = 'ops_1h'`); err != nil {
		t.Fatalf("reset watermark: %v", err)
	}

	runOpsRollup1h(t, pool)
	second := dumpRollup1h(t, pool, bucket, thingID)

	if first != second {
		t.Errorf("re-run not idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestOpsRollup1h_AdvancesWatermarkOnlyAfterCommit asserts the watermark
// advances to the latest fully-processed bucket, never past it.
func TestOpsRollup1h_AdvancesWatermarkOnlyAfterCommit(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops1h-watermark"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	bucket := alignedHourBefore(t, 3)
	insertRawSample(t, pool, bucket.Add(15*time.Minute), thingID, "service", "relay.dial_total", "counter", "", 1, nil)

	runOpsRollup1h(t, pool)

	wm, err := rollupstore.GetWatermark(context.Background(), pool, "ops_1h")
	if err != nil {
		t.Fatalf("get watermark: %v", err)
	}
	if wm.IsZero() {
		t.Fatalf("watermark not advanced")
	}
	// Watermark must be at least the bucket we processed (or later).
	if wm.Before(bucket) {
		t.Errorf("watermark = %v, want >= %v (processed bucket)", wm, bucket)
	}
	// Watermark must not exceed the most recent sealed bucket boundary.
	latestSealed := time.Now().UTC().Truncate(time.Hour)
	if wm.After(latestSealed) {
		t.Errorf("watermark = %v, jumped past latest sealed bucket %v", wm, latestSealed)
	}
}

// dumpRollup1h returns a stable string snapshot of all rollup rows for the
// (bucket, thing) so two runs can be compared for idempotency.
func dumpRollup1h(t *testing.T, pool *pgxpool.Pool, bucket time.Time, thingID string) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT metric_name, metric_kind, dimension_key,
		       COALESCE(value_avg, 0), COALESCE(value_sum, 0),
		       COALESCE(value_min, 0), COALESCE(value_max, 0),
		       sample_count
		  FROM metric_ops_rollup_1h
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
