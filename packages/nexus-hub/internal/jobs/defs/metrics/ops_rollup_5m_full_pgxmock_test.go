// ops_rollup_5m_full_pgxmock_test.go covers the processOneBucket full transaction
// path (deleteBucket, diagAgentsInWindow, insertScalarPerThing, insertScalarFleet,
// insertHistograms, SetWatermark, Commit) and the error sub-paths within each
// helper. All DB calls are exercised via pgxmock so no live Postgres is needed.
package metrics

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// sealedBucket returns a bucket start time that is guaranteed to be before the
// current latestSealed boundary (now - opsSealedGrace truncated to opsBucketDur),
// so the Run loop processes exactly one bucket iteration.
func sealedBucket() time.Time {
	return time.Now().UTC().Add(-opsSealedGrace).Truncate(opsBucketDur).Add(-opsBucketDur)
}

// expectWatermarkAndMin registers pgxmock expectations for the resolveCursor
// prologue: GetWatermark (empty) + minSampledAt returning the provided bucket
// time. This makes Run() resolve cursor = bucket and enter the process loop.
func expectWatermarkAndMin(mock pgxmock.PgxPoolIface, bucket time.Time) {
	// GetWatermark: no row → zero watermark
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	// minSampledAt returns the bucket start (cursor = bucket)
	mock.ExpectQuery(`SELECT MIN\(sampled_at\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&bucket))
}

// expectFullProcessOneBucket_NoHistograms sets up pgxmock expectations for the
// full processOneBucket transaction with no histogram raw rows. The bucket arg
// drives the DELETE parameter.
func expectFullProcessOneBucket_NoHistograms(mock pgxmock.PgxPoolIface, bucket time.Time) pgxmock.PgxPoolIface {
	tx := mock.ExpectBegin()
	_ = tx

	// deleteBucket: DELETE FROM metric_ops_rollup_5m WHERE bucket_start = $1
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	// diagAgentsInWindow: SELECT DISTINCT thing_id FROM thing_diag_mode_window
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"})) // no diag agents

	// insertScalarPerThing: INSERT INTO metric_ops_rollup_5m ... (per-thing)
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	// insertScalarFleet: INSERT INTO metric_ops_rollup_5m ... (fleet)
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	// insertHistograms: SELECT thing_id ... FROM metric_ops_raw WHERE metric_kind = 'histogram'
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}))
	// empty result → no INSERT histogram rows

	// SetWatermark: INSERT INTO "rollup_watermark"
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	// Commit
	mock.ExpectCommit()

	return mock
}

// Full processOneBucket happy path (no histograms)

// TestOpsRollup5m_ProcessOneBucket_FullPath_NoHistograms drives the complete
// processOneBucket transaction (delete → diagAgents → scalars → histograms →
// watermark → commit) with no histogram raw rows, exercising the early-exit
// path in insertHistograms.
func TestOpsRollup5m_ProcessOneBucket_FullPath_NoHistograms(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)
	expectFullProcessOneBucket_NoHistograms(mock, bucket)

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// deleteBucket error

func TestOpsRollup5m_ProcessOneBucket_DeleteError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("delete failed"))
	mock.ExpectRollback()

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from delete failure")
	}
}

// diagAgentsInWindow error

func TestOpsRollup5m_ProcessOneBucket_DiagQueryError(t *testing.T) {
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
		WillReturnError(errors.New("diag query failed"))
	mock.ExpectRollback()

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from diag query failure")
	}
}

// insertScalarPerThing error

func TestOpsRollup5m_ProcessOneBucket_ScalarPerThingError(t *testing.T) {
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
		WillReturnError(errors.New("per-thing insert failed"))
	mock.ExpectRollback()

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from per-thing insert failure")
	}
}

// insertScalarFleet error

func TestOpsRollup5m_ProcessOneBucket_ScalarFleetError(t *testing.T) {
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
	// per-thing INSERT succeeds
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	// fleet INSERT fails
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("fleet insert failed"))
	mock.ExpectRollback()

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from fleet insert failure")
	}
}

// insertHistograms query error

func TestOpsRollup5m_ProcessOneBucket_HistogramQueryError(t *testing.T) {
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
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("histogram query failed"))
	mock.ExpectRollback()

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from histogram query failure")
	}
}

// insertHistograms with data — service thing (per-instance path)

// TestOpsRollup5m_ProcessOneBucket_WithHistogramData exercises the histogram
// accumulation + INSERT path (len(raw) > 0) using a service-type thing.
func TestOpsRollup5m_ProcessOneBucket_WithHistogramData(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{5, 3, 2, 1, 0, 0}})
	thingID := "svc-1"

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

	// insertHistograms: 1 service histogram row
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}).
			AddRow(thingID, "ai-gateway", "hook.pipeline_ms", "", histMeta))

	// INSERT merged histogram row
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), // id, bucket_start
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), // thingID, thingType, metricName, dimKey
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), // sum, count, metadata
		).WillReturnResult(pgxmock.NewResult("INSERT", 1))

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

// insertHistograms — diag agent (per-instance) path

// TestOpsRollup5m_ProcessOneBucket_WithDiagAgentHistogram exercises the branch
// where an agent is in the diag set → per-instance histogram accumulator row.
func TestOpsRollup5m_ProcessOneBucket_WithDiagAgentHistogram(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	diagAgentID := "agent-diag"
	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{2, 1, 0, 0, 0, 0}})

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	// diagAgentsInWindow: returns diagAgentID
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}).AddRow(diagAgentID))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	// insertHistograms: 1 histogram row for the diag agent
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}).
			AddRow(diagAgentID, "agent", "runtime.heap_ms", "", histMeta))
	// INSERT per-instance histogram
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).WillReturnResult(pgxmock.NewResult("INSERT", 1))
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

// insertHistograms — fleet agent (non-diag) path

// TestOpsRollup5m_ProcessOneBucket_WithFleetAgentHistogram exercises the branch
// where an agent is NOT in the diag set → fleet accumulator row (thingID nil).
func TestOpsRollup5m_ProcessOneBucket_WithFleetAgentHistogram(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{3, 2, 1, 0, 0, 0}})
	nonDiagAgentID := "agent-plain"

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	// No diag agents
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	// insertHistograms: 1 agent histogram row (non-diag → fleet)
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}).
			AddRow(nonDiagAgentID, "agent", "agent.metric", "", histMeta))
	// INSERT fleet histogram (thingID is nil)
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_5m`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).WillReturnResult(pgxmock.NewResult("INSERT", 1))
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

// insertHistograms — empty thing_type skipped silently

// TestOpsRollup5m_ProcessOneBucket_EmptyThingTypeSkipped exercises the
// default:"" case in the insertHistograms switch — empty thing_type is a data
// integrity bug and must be silently skipped (continue), leaving len(acc) == 0.
func TestOpsRollup5m_ProcessOneBucket_EmptyThingTypeSkipped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{1, 0, 0, 0, 0, 0}})

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
	// 1 histogram row with empty thing_type → skipped silently; acc stays empty
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}).
			AddRow("thing-x", "", "some.metric", "", histMeta))
	// No histogram INSERT because acc is empty after skipping empty thing_type.
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

// resolveCursor — bootstrap path (cold start / future-seeded watermark)

// TestOpsRollup5m_ResolveCursor_Bootstrap exercises the branch where the
// watermark is seeded into the future (the d86ca4f6 NOW() seed on a fresh
// deploy): it is not a real "last committed bucket" marker, so the cursor
// rewinds to the oldest raw hour rather than advancing past the history.
func TestOpsRollup5m_ResolveCursor_Bootstrap(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Watermark seeded 2h into the future; oldest raw = 3 hours ago. A future
	// watermark is treated as a non-marker → bootstrap to the oldest raw hour.
	futureWm := time.Now().UTC().Truncate(time.Hour).Add(2 * time.Hour)
	oldestRaw := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(futureWm))
	mock.ExpectQuery(`SELECT MIN\(sampled_at\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&oldestRaw))

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	cursor, ok, err := j.resolveCursor(context.Background())
	if err != nil {
		t.Fatalf("resolveCursor: %v", err)
	}
	if !ok {
		t.Fatal("resolveCursor ok=false, expected data")
	}
	wantCursor := oldestRaw.UTC().Truncate(time.Hour)
	if !cursor.Equal(wantCursor) {
		t.Errorf("cursor = %v, want %v (bootstrap)", cursor, wantCursor)
	}
}

// TestOpsRollup5m_ResolveCursor_NoRewindBelowPastWatermark is the regression
// guard for the steady-state death-loop. A real (past) watermark with raw still
// surviving BELOW it (the normal case under multi-day metric_ops_raw retention)
// must advance to watermark+1h — NOT rewind to the oldest raw hour. The pre-fix
// condition (bootstrap.Before(advanceFromWatermark)) rewound here on every run,
// re-rolling already-completed hours until the run budget was exhausted and the
// watermark wedged.
func TestOpsRollup5m_ResolveCursor_NoRewindBelowPastWatermark(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Watermark = 2h ago (a real committed marker). Oldest raw = 5h ago, i.e.
	// well below the watermark (already rolled up). Expected cursor = the hour
	// AFTER the watermark, never the 5h-ago bootstrap.
	pastWm := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	oldestRaw := time.Now().UTC().Truncate(time.Hour).Add(-5 * time.Hour)

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(pastWm))
	mock.ExpectQuery(`SELECT MIN\(sampled_at\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&oldestRaw))

	j := NewOpsRollup5m(nil, time.Hour, testLogger())
	j.pool = mock

	cursor, ok, err := j.resolveCursor(context.Background())
	if err != nil {
		t.Fatalf("resolveCursor: %v", err)
	}
	if !ok {
		t.Fatal("resolveCursor ok=false, expected data")
	}
	wantCursor := pastWm.Add(opsBucketDur)
	if !cursor.Equal(wantCursor) {
		t.Errorf("cursor = %v, want %v (advance from watermark, no rewind)", cursor, wantCursor)
	}
}
