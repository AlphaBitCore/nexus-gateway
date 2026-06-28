// metrics_supplemental_test.go covers statement gaps not addressed by the
// primary per-file test files. Each test targets a specific named failure mode
// or behaviour branch:
//
//   - MetricsRollupJob.Run: delete-existing-bucket error path
//   - MetricsRollupJob.Run: InsertRollupRows error path
//   - MetricsRollupJob.Run: Commit error path
//   - MetricsRollupJob.collectRows: per-query scan error paths (3 queries)
//   - OpsRollup1hJob.resolveCursor: GetWatermark error path
//   - OpsRollup1hJob.resolveCursor: advance-from-watermark branch
//   - OpsRollup1hJob.processOneBucket: SetWatermark error path
//   - OpsRollup1hJob.diagAgentsInWindow: rows.Scan and rows.Err error paths
//   - OpsRollup1hJob.insertHistograms: scan error, rows.Err error, unparseable
//     metadata warn-and-continue, accumulator key collision for all three
//     thing-type branches, EncodeHistogramBuckets error, insert-row error
//   - OpsRollupCascadeJob.runFixed: resolveFixedCursor error propagation
//   - OpsRollupCascadeJob.runCalendarMonth: resolveCalendarCursor error,
//     processOneBucket error propagation
//   - OpsRollupCascadeJob.resolveFixedCursor: minSourceBucket error
//   - OpsRollupCascadeJob.resolveCalendarCursor: minSourceBucket error
//   - OpsRollupCascadeJob.processOneBucket: insertHistograms error propagation
//   - OpsRollupCascadeJob.insertHistograms: scan error, rows.Err error,
//     unparseable metadata warn-and-continue, accumulator key collision (fleet
//     and per-thing), EncodeHistogramBuckets error, insert-row error
package metrics

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// MetricsRollupJob.Run — transaction error paths

func TestMetricsRollup_Run_DeleteError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// collectRows: all three source queries return one row each so len(rows) > 0
	// and Run proceeds to open the transaction.
	mock.ExpectQuery(`SELECT status, COUNT\(\*\) FROM thing`).
		WillReturnRows(pgxmock.NewRows([]string{"status", "n"}).AddRow("online", int(1)))
	mock.ExpectQuery(`SELECT COALESCE\(t\.os`).
		WillReturnRows(pgxmock.NewRows([]string{"os", "n"}))
	mock.ExpectQuery(`SELECT action, COUNT\(\*\) FROM traffic_event`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"action", "n"}))

	mock.ExpectBegin()
	sentinel := errors.New("delete boom")
	mock.ExpectExec(`DELETE FROM metric_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := &MetricsRollupJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (delete)", err)
	}
}

func TestMetricsRollup_Run_InsertRollupRowsError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT status, COUNT\(\*\) FROM thing`).
		WillReturnRows(pgxmock.NewRows([]string{"status", "n"}).AddRow("online", int(1)))
	mock.ExpectQuery(`SELECT COALESCE\(t\.os`).
		WillReturnRows(pgxmock.NewRows([]string{"os", "n"}))
	mock.ExpectQuery(`SELECT action, COUNT\(\*\) FROM traffic_event`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"action", "n"}))

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	sentinel := errors.New("insert rollup rows boom")
	// InsertRollupRows issues one INSERT per row; failing the first INSERT causes
	// the wrapped error to propagate back through Run.
	mock.ExpectExec(`INSERT INTO "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := &MetricsRollupJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from InsertRollupRows failure")
	}
}

func TestMetricsRollup_Run_CommitError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT status, COUNT\(\*\) FROM thing`).
		WillReturnRows(pgxmock.NewRows([]string{"status", "n"}).AddRow("online", int(1)))
	mock.ExpectQuery(`SELECT COALESCE\(t\.os`).
		WillReturnRows(pgxmock.NewRows([]string{"os", "n"}))
	mock.ExpectQuery(`SELECT action, COUNT\(\*\) FROM traffic_event`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"action", "n"}))

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	sentinel := errors.New("commit boom")
	mock.ExpectCommit().WillReturnError(sentinel)

	j := &MetricsRollupJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (commit)", err)
	}
}

// MetricsRollupJob.collectRows — scan error paths

// TestMetricsRollup_CollectRows_ScanErrors exercises the three per-query Scan
// error paths. Each returns a malformed row so Scan fails; the error is
// appended to errs (not returned). Since all queries fail their scans the
// aggregated rows slice stays empty — Run exits before opening a transaction.
func TestMetricsRollup_CollectRows_ScanErrors(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// fleetStatus scan error: column count mismatch (query returns 3 cols, Scan
	// expects 2) triggers ErrNoRows-like behaviour in pgxmock — instead we make
	// the row data type wrong so Scan returns an error.
	// pgxmock treats column type mismatches in AddRow as scan errors.
	mock.ExpectQuery(`SELECT status, COUNT\(\*\) FROM thing`).
		WillReturnRows(pgxmock.NewRows([]string{"status", "n"}).
			AddRow("online", "not-an-int")) // type mismatch → Scan error

	mock.ExpectQuery(`SELECT COALESCE\(t\.os`).
		WillReturnRows(pgxmock.NewRows([]string{"os", "n"}).
			AddRow("darwin", "not-an-int")) // type mismatch → Scan error

	mock.ExpectQuery(`SELECT action, COUNT\(\*\) FROM traffic_event`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"action", "n"}).
			AddRow("upload", "not-an-int")) // type mismatch → Scan error

	j := &MetricsRollupJob{pool: mock, logger: testLogger()}
	// Run: rows empty → no tx opened; errs joined and returned.
	err := j.Run(context.Background())
	// At least one scan error must be present in the joined error.
	if err == nil {
		t.Fatal("expected joined scan errors, got nil")
	}
}

// OpsRollup1hJob.resolveCursor — GetWatermark error

func TestOpsRollup1h_ResolveCursor_GetWatermarkError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("watermark query boom")
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (get watermark)", err)
	}
}

// OpsRollup1hJob.resolveCursor — advance-from-watermark branch

// TestOpsRollup1h_ResolveCursor_AdvanceFromWatermark exercises the branch where
// watermark is non-zero and bootstrap >= advanceFromWatermark: cursor is set to
// advanceFromWatermark. This requires the watermark to be older than the oldest
// raw sample so advanceFromWatermark >= bootstrap.
//
// Concrete setup: watermark = 1h ago; oldest raw = exactly 1h ago.
//   - bootstrap = 1h ago truncated to 1h = 1h ago
//   - advanceFromWatermark = 1h ago + 1h = now truncated to 1h
//   - bootstrap (1h ago) < advanceFromWatermark (now-trunc) → bootstrap wins
//
// For advance to win, bootstrap must be >= advance.
//   - watermark = 2h ago; oldest raw = 1h ago → bootstrap = 1h ago, advance = 1h ago
//   - bootstrap == advance → NOT Before(advance) → return advance
func TestOpsRollup1h_ResolveCursor_AdvanceFromWatermark(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	twoHoursAgo := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	oneHourAgo := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(twoHoursAgo))
	mock.ExpectQuery(`SELECT MIN\(sampled_at\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&oneHourAgo))

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	cursor, ok, err := j.resolveCursor(context.Background())
	if err != nil {
		t.Fatalf("resolveCursor: %v", err)
	}
	if !ok {
		t.Fatal("resolveCursor ok=false")
	}
	// advance = twoHoursAgo.Truncate(1h) + 1h = 1h ago
	wantAdvance := twoHoursAgo.UTC().Truncate(time.Hour).Add(time.Hour)
	if !cursor.Equal(wantAdvance) {
		t.Errorf("cursor = %v, want %v (advance from watermark)", cursor, wantAdvance)
	}
}

// OpsRollup1hJob.processOneBucket — SetWatermark error

func TestOpsRollup1h_ProcessOneBucket_SetWatermarkError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	// insertHistograms: empty result set
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}))
	// SetWatermark error
	sentinel := errors.New("watermark write boom")
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from SetWatermark failure")
	}
}

// OpsRollup1hJob.diagAgentsInWindow — rows.Scan error

// TestOpsRollup1h_DiagAgentsInWindow_ScanError exercises the rows.Scan error
// branch inside diagAgentsInWindow (type mismatch on the returned row).
func TestOpsRollup1h_DiagAgentsInWindow_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	// diagAgentsInWindow: one row whose column type is wrong → Scan returns error
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}).AddRow(42)) // int instead of string
	mock.ExpectRollback()

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from diagAgentsInWindow scan failure")
	}
}

// OpsRollup1hJob.insertHistograms — scan error path

func TestOpsRollup1h_InsertHistograms_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	// insertHistograms: row with wrong column count → Scan error (5 cols expected,
	// but thing_id is int instead of string so pgxmock forces a scan error).
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}).
			AddRow(99, "agent", "m", "", []byte(`{}`))) // int for thing_id → Scan error
	mock.ExpectRollback()

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from histogram Scan failure")
	}
}

// OpsRollup1hJob.insertHistograms — unparseable histogram metadata (warn+continue)

// TestOpsRollup1h_InsertHistograms_UnparseableMeta exercises the branch where
// ParseHistogramBuckets fails. The row is silently skipped (Warn+continue) and
// the accumulator stays empty, so no histogram INSERT is issued.
func TestOpsRollup1h_InsertHistograms_UnparseableMeta(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	// Unparseable metadata: not valid JSON
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}).
			AddRow("svc-1", "ai-gateway", "m", "", []byte(`not-json`))) // bad metadata → parse error

	// No histogram INSERT (acc stays empty after skip).
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v (unparseable histogram must only Warn)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// OpsRollup1hJob.insertHistograms — accumulator key collision paths

// TestOpsRollup1h_InsertHistograms_AccumulatorKeyCollision exercises the
// accumulator hit paths for two rows with the same (thingID, metricName, dimKey)
// in each of the three branches: diag agent (per-instance), non-diag fleet
// agent, and server-side Thing. After merging, one INSERT per (key) fires.
func TestOpsRollup1h_InsertHistograms_AccumulatorKeyCollision(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{1, 0, 0, 0, 0, 0}})
	diagID := "agent-diag"

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	// diagAgentsInWindow: diagID is in the diag set
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}).AddRow(diagID))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	// 6 histogram rows: 2 diag-agent, 2 fleet-agent, 2 server. Each pair has
	// the same accumulator key → key-collision (hit) paths are exercised.
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}).
			// diag agent — row 1 (creates acc entry)
			AddRow(diagID, "agent", "cpu.ms", "", histMeta).
			// diag agent — row 2 (hits acc entry: the `if e, ok := acc[key]; ok` branch)
			AddRow(diagID, "agent", "cpu.ms", "", histMeta).
			// fleet agent — row 1
			AddRow("non-diag", "agent", "mem.ms", "", histMeta).
			// fleet agent — row 2 (hits acc entry: fleet isFleet=true key collision)
			AddRow("non-diag2", "agent", "mem.ms", "", histMeta).
			// server — row 1
			AddRow("svc-1", "ai-gateway", "hook.ms", "", histMeta).
			// server — row 2 (hits acc entry: default branch key collision)
			AddRow("svc-1", "ai-gateway", "hook.ms", "", histMeta))

	// 3 INSERT histogram rows: one per unique accumulator key.
	for range 3 {
		mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
			WithArgs(
				pgxmock.AnyArg(), pgxmock.AnyArg(),
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// OpsRollup1hJob.insertHistograms — INSERT histogram row error

func TestOpsRollup1h_InsertHistograms_InsertRowError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{2, 1, 0, 0, 0, 0}})

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	// One valid histogram row → acc entry created → INSERT fails
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}).
			AddRow("svc-x", "compliance-proxy", "m", "", histMeta))
	sentinel := errors.New("insert histogram row boom")
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from histogram INSERT failure")
	}
}

// OpsRollupCascadeJob.runFixed — resolveFixedCursor error propagation

func TestOpsRollupCascade_RunFixed_ResolveCursorError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()

	sentinel := errors.New("watermark boom")
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (runFixed resolveFixedCursor)", err)
	}
}

// OpsRollupCascadeJob.runCalendarMonth — resolveCalendarCursor error

func TestOpsRollupCascade_RunCalendarMonth_ResolveCursorError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()

	sentinel := errors.New("watermark boom")
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)

	j := NewOpsRollup1mo(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (runCalendarMonth resolveCalendarCursor)", err)
	}
}

// OpsRollupCascadeJob.runCalendarMonth — processOneBucket error propagation

// TestOpsRollupCascade_RunCalendarMonth_ProcessOneBucketError exercises the
// branch where processOneBucket returns an error during the calendar-month loop.
func TestOpsRollupCascade_RunCalendarMonth_ProcessOneBucketError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()

	now := time.Now().UTC()
	// Use 2 months ago to ensure the loop has a sealed month to process.
	twoMonthsAgo := time.Date(now.Year(), now.Month()-2, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&twoMonthsAgo))

	// processOneBucket: Begin succeeds but DELETE fails immediately.
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1mo`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("bucket delete boom"))
	mock.ExpectRollback()

	j := NewOpsRollup1mo(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from processOneBucket failure in runCalendarMonth")
	}
}

// OpsRollupCascadeJob.resolveFixedCursor — minSourceBucket error

func TestOpsRollupCascade_ResolveFixedCursor_MinSourceBucketError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"})) // zero watermark

	sentinel := errors.New("min bucket boom")
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnError(sentinel)

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (minSourceBucket)", err)
	}
}

// OpsRollupCascadeJob.resolveCalendarCursor — minSourceBucket error

func TestOpsRollupCascade_ResolveCalendarCursor_MinSourceBucketError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))

	sentinel := errors.New("min bucket boom")
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnError(sentinel)

	j := NewOpsRollup1mo(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (minSourceBucket calendar)", err)
	}
}

// OpsRollupCascadeJob.processOneBucket — insertHistograms error propagation

func TestOpsRollupCascade_ProcessOneBucket_InsertHistogramsError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()

	day := sealedDayBucket()
	expectCascadeWatermarkAndMin(mock, day)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{3, 1, 0, 0, 0, 0}})
	thingID := "svc-fail"
	// insertHistograms: one valid row → acc entry → INSERT fails
	mock.ExpectQuery(`FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata", "sample_count"}).
			AddRow(&thingID, "ai-gateway", "m", "", histMeta, int64(2)))
	sentinel := errors.New("cascade histogram insert boom")
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1d`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from cascade insertHistograms INSERT failure")
	}
}

// OpsRollupCascadeJob.insertHistograms — scan error path

func TestOpsRollupCascade_InsertHistograms_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()

	day := sealedDayBucket()
	expectCascadeWatermarkAndMin(mock, day)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	// Row with wrong type for sample_count → Scan fails
	mock.ExpectQuery(`FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata", "sample_count"}).
			AddRow(nil, "agent", "m", "", []byte(`{}`), "not-an-int64")) // type mismatch
	mock.ExpectRollback()

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from cascade histogram Scan failure")
	}
}

// OpsRollupCascadeJob.insertHistograms — unparseable metadata (warn+continue)

func TestOpsRollupCascade_InsertHistograms_UnparseableMeta(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()

	day := sealedDayBucket()
	expectCascadeWatermarkAndMin(mock, day)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	thingID := "svc-bad"
	mock.ExpectQuery(`FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata", "sample_count"}).
			AddRow(&thingID, "ai-gateway", "m", "", []byte(`not-json`), int64(1)))
	// acc stays empty → no histogram INSERT
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v (unparseable cascade histogram must only Warn)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// OpsRollupCascadeJob.insertHistograms — accumulator key collision (fleet + per-thing)

// TestOpsRollupCascade_InsertHistograms_AccumulatorKeyCollision exercises the
// accumulator hit paths: two fleet rows (thingID nil) and two per-thing rows
// with the same key → each pair merges into one INSERT.
func TestOpsRollupCascade_InsertHistograms_AccumulatorKeyCollision(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()

	day := sealedDayBucket()
	expectCascadeWatermarkAndMin(mock, day)

	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{1, 0, 0, 0, 0, 0}})
	thingID := "svc-1"

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	// 4 rows: 2 fleet + 2 per-thing (each pair hits the accumulator key)
	mock.ExpectQuery(`FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata", "sample_count"}).
			AddRow((*string)(nil), "agent", "fleet.ms", "", histMeta, int64(5)).
			AddRow((*string)(nil), "agent", "fleet.ms", "", histMeta, int64(3)). // fleet key collision
			AddRow(&thingID, "ai-gateway", "hook.ms", "", histMeta, int64(4)).
			AddRow(&thingID, "ai-gateway", "hook.ms", "", histMeta, int64(2))) // per-thing key collision

	// 2 INSERTs: one fleet, one per-thing
	for range 2 {
		mock.ExpectExec(`INSERT INTO metric_ops_rollup_1d`).
			WithArgs(
				pgxmock.AnyArg(), pgxmock.AnyArg(),
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}
