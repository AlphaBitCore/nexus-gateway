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

// Pure (no-DB) tests — exercise identity + cold-start watermark logic.

func TestOpsRollup5m_Identity(t *testing.T) {
	j := NewOpsRollup5m(nil, time.Minute, testLogger())
	if j.ID() != "ops-rollup-5m" {
		t.Errorf("ID = %q, want ops-rollup-5m", j.ID())
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

func TestOpsRollup5m_IntervalDefault(t *testing.T) {
	j := NewOpsRollup5m(nil, 0, testLogger())
	if j.Interval() != time.Minute {
		t.Errorf("Interval = %v, want 1m default", j.Interval())
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
	if _, err := pool.Exec(ctx, `DELETE FROM rollup_watermark WHERE "jobName" = 'ops_5m'`); err != nil {
		t.Fatalf("reset watermark: %v", err)
	}
	// Best-effort wipe of rollup rows scoped to this test's thing IDs and
	// fleet rows for the agent type. (We can't scope fleet rows to a test
	// because thing_id IS NULL, so we just delete the time window.)
	for id := range things {
		if _, err := pool.Exec(ctx, `DELETE FROM metric_ops_rollup_5m WHERE thing_id = $1`, id); err != nil {
			t.Fatalf("wipe rollup: %v", err)
		}
	}

	return func() {
		for id := range things {
			_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_rollup_5m WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_raw WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM thing_diag_mode_window WHERE thing_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM thing WHERE id = $1`, id)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM rollup_watermark WHERE "jobName" = 'ops_5m'`)
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

// alignedBucketBefore returns the start of the 5-minute bucket that ended at
// least opsSealedGrace ago. The 5m rollup only processes buckets that are fully
// sealed (ended >= 1min ago); callers go further back to leave headroom.
func alignedBucketBefore(t *testing.T, bucketsAgo int) time.Time {
	t.Helper()
	return time.Now().UTC().Truncate(opsBucketDur).Add(-time.Duration(bucketsAgo) * opsBucketDur)
}

// runOpsRollup5m constructs the job, calls Run, and fails the test on error.
func runOpsRollup5m(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	j := NewOpsRollup5m(pool, time.Minute, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestOpsRollup5m_AgentInDiagModeKeepsPerThingRow asserts that an agent whose
// thing_id appears in thing_diag_mode_window for the bucket gets a per-thing
// row in metric_ops_rollup_5m (not folded into the fleet aggregate).
func TestOpsRollup5m_AgentInDiagModeKeepsPerThingRow(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops5m-diag-agent"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{thingID: "agent"})
	defer cleanup()

	bucket := alignedBucketBefore(t, 2)
	// Insert two raw samples inside the same 5-minute bucket.
	insertRawSample(t, pool, bucket.Add(1*time.Minute), thingID, "agent", "runtime.heap_alloc_bytes", "gauge", "", 1000, nil)
	insertRawSample(t, pool, bucket.Add(2*time.Minute), thingID, "agent", "runtime.heap_alloc_bytes", "gauge", "", 2000, nil)

	// Mark the agent in diag-mode for the entire bucket.
	insertDiagWindow(t, pool, thingID, bucket, bucket.Add(opsBucketDur))

	runOpsRollup5m(t, pool)

	ctx := context.Background()
	var perThingCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM metric_ops_rollup_5m
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
		SELECT COUNT(*) FROM metric_ops_rollup_5m
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
		SELECT value_avg, value_sum, sample_count FROM metric_ops_rollup_5m
		 WHERE bucket_start = $1 AND thing_id = $2 AND metric_name = 'runtime.heap_alloc_bytes'
	`, bucket, thingID).Scan(&avg, &sum, &sampleCount); err != nil {
		t.Fatalf("scan agg: %v", err)
	}
	if avg != 1500 || sum != 3000 || sampleCount != 2 {
		t.Errorf("agg = (avg=%v, sum=%v, count=%d), want (1500, 3000, 2)", avg, sum, sampleCount)
	}
}

// TestOpsRollup5m_AgentNotInDiagModeAggregatesToFleet asserts that an agent
// whose thing_id has no diag-mode window collapses into the fleet aggregate
// (one row with thing_id IS NULL per metric+dim).
func TestOpsRollup5m_AgentNotInDiagModeAggregatesToFleet(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	a, b := "test-ops5m-fleet-a", "test-ops5m-fleet-b"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{a: "agent", b: "agent"})
	defer cleanup()

	bucket := alignedBucketBefore(t, 2)
	insertRawSample(t, pool, bucket.Add(1*time.Minute), a, "agent", "runtime.heap_alloc_bytes", "gauge", "", 1000, nil)
	insertRawSample(t, pool, bucket.Add(2*time.Minute), b, "agent", "runtime.heap_alloc_bytes", "gauge", "", 3000, nil)

	runOpsRollup5m(t, pool)

	ctx := context.Background()
	// Both per-thing rows should be absent — fleet aggregate only.
	var perThingA, perThingB int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM metric_ops_rollup_5m WHERE bucket_start=$1 AND thing_id=$2`, bucket, a).Scan(&perThingA)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM metric_ops_rollup_5m WHERE bucket_start=$1 AND thing_id=$2`, bucket, b).Scan(&perThingB)
	if perThingA != 0 || perThingB != 0 {
		t.Errorf("per-thing rows for non-diag agents: a=%d, b=%d (want both 0)", perThingA, perThingB)
	}

	var avg, sum float64
	var sampleCount int
	if err := pool.QueryRow(ctx, `
		SELECT value_avg, value_sum, sample_count FROM metric_ops_rollup_5m
		 WHERE bucket_start = $1 AND thing_id IS NULL AND thing_type = 'agent' AND metric_name = 'runtime.heap_alloc_bytes'
	`, bucket).Scan(&avg, &sum, &sampleCount); err != nil {
		t.Fatalf("scan fleet: %v", err)
	}
	if avg != 2000 || sum != 4000 || sampleCount != 2 {
		t.Errorf("fleet = (avg=%v, sum=%v, count=%d), want (2000, 4000, 2)", avg, sum, sampleCount)
	}
}

// TestOpsRollup5m_ServiceAlwaysPerInstance asserts services always get
// per-instance rows regardless of any diag-mode windows.
func TestOpsRollup5m_ServiceAlwaysPerInstance(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	svc := "test-ops5m-service-1"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{svc: "service"})
	defer cleanup()

	bucket := alignedBucketBefore(t, 2)
	insertRawSample(t, pool, bucket.Add(1*time.Minute), svc, "service", "relay.dial_total", "counter", "", 5, nil)
	insertRawSample(t, pool, bucket.Add(3*time.Minute), svc, "service", "relay.dial_total", "counter", "", 7, nil)

	runOpsRollup5m(t, pool)

	ctx := context.Background()
	var sum float64
	var sampleCount int
	if err := pool.QueryRow(ctx, `
		SELECT value_sum, sample_count FROM metric_ops_rollup_5m
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
		SELECT COUNT(*) FROM metric_ops_rollup_5m
		 WHERE bucket_start = $1 AND thing_id IS NULL AND thing_type = 'service'
	`, bucket).Scan(&fleetCount)
	if fleetCount != 0 {
		t.Errorf("service fleet rows = %d, want 0 (services never aggregate)", fleetCount)
	}
}

// TestOpsRollup5m_HistogramMergesBucketsElementwise asserts histogram raw rows
// are merged element-wise via the opsmetrics histogram-merge helper rather
// than naively averaged via SQL.
func TestOpsRollup5m_HistogramMergesBucketsElementwise(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops5m-hist-svc"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	bucket := alignedBucketBefore(t, 2)
	// Two histogram samples — when merged element-wise we expect [11,7,5,2,0,0].
	insertRawSample(t, pool, bucket.Add(1*time.Minute), thingID, "service", "hook.pipeline_ms", "histogram", "stage=request", 0,
		map[string]any{"buckets": []int{10, 5, 2, 1, 0, 0}})
	insertRawSample(t, pool, bucket.Add(2*time.Minute), thingID, "service", "hook.pipeline_ms", "histogram", "stage=request", 0,
		map[string]any{"buckets": []int{1, 2, 3, 1, 0, 0}})

	runOpsRollup5m(t, pool)

	ctx := context.Background()
	var metaRaw []byte
	var sampleCount int
	if err := pool.QueryRow(ctx, `
		SELECT metadata, sample_count FROM metric_ops_rollup_5m
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

// TestOpsRollup5m_BootstrapsFromMinSampledAtWhenWatermarkIsOlder pins the
// Phase 1 carry-forward A: when the watermark is seeded into the future (e.g.,
// the d86ca4f6 NOW() seed on a fresh deploy), the job must rewind to the bucket
// containing MIN(sampled_at) so the first iteration covers historical data.
func TestOpsRollup5m_BootstrapsFromMinSampledAtWhenWatermarkIsOlder(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops5m-bootstrap"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	// Seed a watermark in the future to simulate the d86ca4f6 NOW() seed on a
	// fresh deploy; raw data lands earlier, so without the future-watermark
	// bootstrap the cursor would advance past it and strand the history.
	bucket := alignedBucketBefore(t, 4)
	insertRawSample(t, pool, bucket.Add(1*time.Minute), thingID, "service", "relay.dial_total", "counter", "", 9, nil)

	// Watermark 2h ahead of NOW() means all raw is older than the watermark —
	// without the bootstrap branch the cursor sits past the data and the bucket
	// is skipped. (Seeding the future explicitly, rather than the bare NOW()
	// the old test used, removes the dependency on Run() executing within the
	// same instant as the seed.)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO rollup_watermark ("jobName", "watermark", "updatedAt")
		VALUES ('ops_5m', NOW() + INTERVAL '2 hours', NOW())
		ON CONFLICT ("jobName") DO UPDATE SET "watermark" = EXCLUDED."watermark"
	`); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	runOpsRollup5m(t, pool)

	ctx := context.Background()
	var sum float64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(value_sum, 0) FROM metric_ops_rollup_5m
		 WHERE bucket_start = $1 AND thing_id = $2 AND metric_name = 'relay.dial_total'
	`, bucket, thingID).Scan(&sum); err != nil {
		t.Fatalf("scan bootstrap row: %v (no rollup row produced — bootstrap path is broken)", err)
	}
	if sum != 9 {
		t.Errorf("bootstrap sum = %v, want 9", sum)
	}
}

// TestOpsRollup5m_SkipsWhenRawIsEmpty asserts an empty metric_ops_raw under
// a fresh watermark is a silent no-op (no error, no rollup rows).
func TestOpsRollup5m_SkipsWhenRawIsEmpty(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	cleanup := opsRollupTestSetup(t, pool, map[string]string{})
	defer cleanup()

	// No raw rows at all for the test scope. Ensure the job does not error
	// when MIN(sampled_at) returns NULL globally as well — guard by
	// bounding the pre-test global state.

	j := NewOpsRollup5m(pool, time.Minute, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run on empty: %v", err)
	}
}

// TestOpsRollup5m_IdempotentReRun asserts running the job twice over the same
// raw data produces the same final rollup state (DELETE+INSERT semantics).
func TestOpsRollup5m_IdempotentReRun(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops5m-idempotent"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	bucket := alignedBucketBefore(t, 2)
	insertRawSample(t, pool, bucket.Add(1*time.Minute), thingID, "service", "relay.dial_total", "counter", "", 4, nil)
	insertRawSample(t, pool, bucket.Add(3*time.Minute), thingID, "service", "relay.dial_total", "counter", "", 6, nil)

	runOpsRollup5m(t, pool)
	first := dumpRollup5m(t, pool, bucket, thingID)

	// Reset watermark so the second run re-processes the same bucket.
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM rollup_watermark WHERE "jobName" = 'ops_5m'`); err != nil {
		t.Fatalf("reset watermark: %v", err)
	}

	runOpsRollup5m(t, pool)
	second := dumpRollup5m(t, pool, bucket, thingID)

	if first != second {
		t.Errorf("re-run not idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestOpsRollup5m_AdvancesWatermarkOnlyAfterCommit asserts the watermark
// advances to the latest fully-processed bucket, never past it.
func TestOpsRollup5m_AdvancesWatermarkOnlyAfterCommit(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops5m-watermark"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	bucket := alignedBucketBefore(t, 3)
	insertRawSample(t, pool, bucket.Add(1*time.Minute), thingID, "service", "relay.dial_total", "counter", "", 1, nil)

	runOpsRollup5m(t, pool)

	wm, err := rollupstore.GetWatermark(context.Background(), pool, "ops_5m")
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
	latestSealed := time.Now().UTC().Truncate(opsBucketDur)
	if wm.After(latestSealed) {
		t.Errorf("watermark = %v, jumped past latest sealed bucket %v", wm, latestSealed)
	}
}

// TestOpsRollup5m_DoesNotRescanBelowWatermark pins the fix for the
// steady-state death-loop: a real (past) watermark must make the job resume at
// the bucket AFTER it, never rewind to MIN(sampled_at). Before the fix, any
// surviving raw older than the watermark (always true under multi-day
// retention) sent the cursor back to the oldest bucket, re-aggregating every
// completed bucket each run until the run budget was exhausted and the
// watermark wedged.
func TestOpsRollup5m_DoesNotRescanBelowWatermark(t *testing.T) {
	pool := jobsTestPool(t)
	defer pool.Close()

	thingID := "test-ops5m-no-rescan"
	cleanup := opsRollupTestSetup(t, pool, map[string]string{thingID: "service"})
	defer cleanup()

	// done   : a bucket already rolled up (sits below the watermark).
	// wmBucket: the last committed bucket — the watermark.
	// fresh  : a new sealed bucket after the watermark — the only one to process.
	done := alignedBucketBefore(t, 6)
	wmBucket := alignedBucketBefore(t, 4)
	fresh := alignedBucketBefore(t, 2)

	insertRawSample(t, pool, done.Add(1*time.Minute), thingID, "service", "relay.dial_total", "counter", "", 5, nil)
	insertRawSample(t, pool, fresh.Add(1*time.Minute), thingID, "service", "relay.dial_total", "counter", "", 7, nil)

	// Seed a real, past watermark at wmBucket (as SetWatermark would leave it).
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO rollup_watermark ("jobName", "watermark", "updatedAt")
		VALUES ('ops_5m', $1, NOW())
		ON CONFLICT ("jobName") DO UPDATE SET "watermark" = EXCLUDED."watermark"
	`, wmBucket); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	runOpsRollup5m(t, pool)

	ctx := context.Background()

	// The fresh bucket (after the watermark) must be rolled up.
	var freshCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM metric_ops_rollup_5m
		 WHERE bucket_start = $1 AND thing_id = $2 AND metric_name = 'relay.dial_total'
	`, fresh, thingID).Scan(&freshCount); err != nil {
		t.Fatalf("count fresh bucket: %v", err)
	}
	if freshCount != 1 {
		t.Errorf("fresh-bucket rollup rows = %d, want 1 (bucket after the watermark must be processed)", freshCount)
	}

	// The done bucket (below the watermark) must NOT be re-aggregated — trusting
	// the watermark is exactly what stops the full-history re-scan each run.
	var doneCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM metric_ops_rollup_5m
		 WHERE bucket_start = $1 AND thing_id = $2
	`, done, thingID).Scan(&doneCount); err != nil {
		t.Fatalf("count done bucket: %v", err)
	}
	if doneCount != 0 {
		t.Errorf("below-watermark rollup rows = %d, want 0 (job must not rewind below the watermark)", doneCount)
	}

	// Watermark advanced to include the fresh bucket.
	wm, err := rollupstore.GetWatermark(ctx, pool, "ops_5m")
	if err != nil {
		t.Fatalf("get watermark: %v", err)
	}
	if wm.Before(fresh) {
		t.Errorf("watermark = %v, want >= %v (fresh bucket)", wm, fresh)
	}
}

// dumpRollup5m returns a stable string snapshot of all rollup rows for the
// (bucket, thing) so two runs can be compared for idempotency.
func dumpRollup5m(t *testing.T, pool *pgxpool.Pool, bucket time.Time, thingID string) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT metric_name, metric_kind, dimension_key,
		       COALESCE(value_avg, 0), COALESCE(value_sum, 0),
		       COALESCE(value_min, 0), COALESCE(value_max, 0),
		       sample_count
		  FROM metric_ops_rollup_5m
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
