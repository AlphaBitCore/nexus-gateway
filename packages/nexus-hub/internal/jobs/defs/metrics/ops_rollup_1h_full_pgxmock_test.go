// ops_rollup_1h_full_pgxmock_test.go covers the processOneBucket full transaction
// path (deleteBucket, diagAgentsInWindow, insertScalarPerThing, insertScalarFleet,
// insertHistograms, SetWatermark, Commit) and the error sub-paths within each
// helper. All DB calls are exercised via pgxmock so no live Postgres is needed.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
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

	// deleteBucket: DELETE FROM metric_ops_rollup_1h WHERE bucket_start = $1
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	// diagAgentsInWindow: SELECT DISTINCT thing_id FROM thing_diag_mode_window
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"})) // no diag agents

	// insertScalarPerThing: INSERT INTO metric_ops_rollup_1h ... (per-thing)
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	// insertScalarFleet: INSERT INTO metric_ops_rollup_1h ... (fleet)
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
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

// TestOpsRollup1h_ProcessOneBucket_FullPath_NoHistograms drives the complete
// processOneBucket transaction (delete → diagAgents → scalars → histograms →
// watermark → commit) with no histogram raw rows, exercising the early-exit
// path in insertHistograms.
func TestOpsRollup1h_ProcessOneBucket_FullPath_NoHistograms(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)
	expectFullProcessOneBucket_NoHistograms(mock, bucket)

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// deleteBucket error

func TestOpsRollup1h_ProcessOneBucket_DeleteError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("delete failed"))
	mock.ExpectRollback()

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from delete failure")
	}
}

// diagAgentsInWindow error

func TestOpsRollup1h_ProcessOneBucket_DiagQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("diag query failed"))
	mock.ExpectRollback()

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from diag query failure")
	}
}

// insertScalarPerThing error

func TestOpsRollup1h_ProcessOneBucket_ScalarPerThingError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("per-thing insert failed"))
	mock.ExpectRollback()

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from per-thing insert failure")
	}
}

// insertScalarFleet error

func TestOpsRollup1h_ProcessOneBucket_ScalarFleetError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}))
	// per-thing INSERT succeeds
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	// fleet INSERT fails
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("fleet insert failed"))
	mock.ExpectRollback()

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from fleet insert failure")
	}
}

// insertHistograms query error

func TestOpsRollup1h_ProcessOneBucket_HistogramQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("histogram query failed"))
	mock.ExpectRollback()

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from histogram query failure")
	}
}

// insertHistograms with data — service thing (per-instance path)

// TestOpsRollup1h_ProcessOneBucket_WithHistogramData exercises the histogram
// accumulation + INSERT path (len(raw) > 0) using a service-type thing.
func TestOpsRollup1h_ProcessOneBucket_WithHistogramData(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{5, 3, 2, 1, 0, 0}})
	thingID := "svc-1"

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	// insertHistograms: 1 service histogram row
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}).
			AddRow(thingID, "ai-gateway", "hook.pipeline_ms", "", histMeta))

	// INSERT merged histogram row
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), // id, bucket_start
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), // thingID, thingType, metricName, dimKey
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), // sum, count, metadata
		).WillReturnResult(pgxmock.NewResult("INSERT", 1))

	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// insertHistograms — diag agent (per-instance) path

// TestOpsRollup1h_ProcessOneBucket_WithDiagAgentHistogram exercises the branch
// where an agent is in the diag set → per-instance histogram accumulator row.
func TestOpsRollup1h_ProcessOneBucket_WithDiagAgentHistogram(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	diagAgentID := "agent-diag"
	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{2, 1, 0, 0, 0, 0}})

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	// diagAgentsInWindow: returns diagAgentID
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}).AddRow(diagAgentID))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	// insertHistograms: 1 histogram row for the diag agent
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}).
			AddRow(diagAgentID, "agent", "runtime.heap_ms", "", histMeta))
	// INSERT per-instance histogram
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// insertHistograms — fleet agent (non-diag) path

// TestOpsRollup1h_ProcessOneBucket_WithFleetAgentHistogram exercises the branch
// where an agent is NOT in the diag set → fleet accumulator row (thingID nil).
func TestOpsRollup1h_ProcessOneBucket_WithFleetAgentHistogram(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{3, 2, 1, 0, 0, 0}})
	nonDiagAgentID := "agent-plain"

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	// No diag agents
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	// insertHistograms: 1 agent histogram row (non-diag → fleet)
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata"}).
			AddRow(nonDiagAgentID, "agent", "agent.metric", "", histMeta))
	// INSERT fleet histogram (thingID is nil)
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// insertHistograms — empty thing_type skipped silently

// TestOpsRollup1h_ProcessOneBucket_EmptyThingTypeSkipped exercises the
// default:"" case in the insertHistograms switch — empty thing_type is a data
// integrity bug and must be silently skipped (continue), leaving len(acc) == 0.
func TestOpsRollup1h_ProcessOneBucket_EmptyThingTypeSkipped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := sealedBucket()
	expectWatermarkAndMin(mock, bucket)

	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{1, 0, 0, 0, 0, 0}})

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`SELECT DISTINCT thing_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id"}))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1h`).
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

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// resolveCursor — bootstrap path (minSampledAt older than watermark+1h)

// TestOpsRollup1h_ResolveCursor_Bootstrap exercises the branch where the oldest
// raw sample is before the advance-from-watermark cursor: bootstrap takes
// precedence and returns the truncated hour of the oldest sample.
func TestOpsRollup1h_ResolveCursor_Bootstrap(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Watermark = 1 hour ago (very recent). Oldest raw = 3 hours ago.
	// Bootstrap = 3h ago truncated; advance = 1h ago + 1h = now truncated.
	// bootstrap < advance → bootstrap wins.
	recentWm := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	oldestRaw := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(recentWm))
	mock.ExpectQuery(`SELECT MIN\(sampled_at\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&oldestRaw))

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
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
