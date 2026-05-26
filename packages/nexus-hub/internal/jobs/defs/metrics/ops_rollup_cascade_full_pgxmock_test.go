// ops_rollup_cascade_full_pgxmock_test.go covers OpsRollupCascadeJob.processOneBucket
// full transaction path (delete → insertScalars → insertHistograms → watermark →
// commit), error sub-paths within each helper, and the runFixed/runCalendarMonth
// cursor resolution paths. All DB calls are exercised via pgxmock.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// sealedDayBucket returns a bucket start time that is guaranteed to be before
// the latestSealed boundary used by runFixed (now - sealedGrace truncated to
// targetBucketDur=24h). For the 1d job, sealedGrace = 1h, so latestSealed =
// now - 1h truncated to 24h = start of today. Any time before today qualifies.
func sealedDayBucket() time.Time {
	// yesterday 00:00 UTC — safely before the current day's latestSealed.
	return time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
}

// expectCascadeWatermarkAndMin registers pgxmock expectations for the
// resolveFixedCursor prologue of the 1d cascade job.
func expectCascadeWatermarkAndMin(mock pgxmock.PgxPoolIface, oldest time.Time) {
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&oldest))
}

// processOneBucket full happy path — no histograms

// TestOpsRollupCascade_ProcessOneBucket_FullPath_NoHistograms drives the
// complete 1d cascade transaction: delete → insertScalars → insertHistograms
// (empty → early return) → SetWatermark → commit.
func TestOpsRollupCascade_ProcessOneBucket_FullPath_NoHistograms(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	day := sealedDayBucket()
	expectCascadeWatermarkAndMin(mock, day)

	mock.ExpectBegin()
	// DELETE FROM metric_ops_rollup_1d WHERE bucket_start = $1
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	// insertScalars: INSERT INTO metric_ops_rollup_1d SELECT ... FROM metric_ops_rollup_1h
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	// insertHistograms: SELECT ... FROM metric_ops_rollup_1h WHERE metric_kind = 'histogram'
	mock.ExpectQuery(`FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata", "sample_count"}))
	// SetWatermark
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

// processOneBucket — delete error

func TestOpsRollupCascade_ProcessOneBucket_DeleteError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	day := sealedDayBucket()
	expectCascadeWatermarkAndMin(mock, day)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("delete failed"))
	mock.ExpectRollback()

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from delete failure")
	}
}

// processOneBucket — insertScalars error

func TestOpsRollupCascade_ProcessOneBucket_InsertScalarsError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	day := sealedDayBucket()
	expectCascadeWatermarkAndMin(mock, day)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("scalar insert failed"))
	mock.ExpectRollback()

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from scalar insert failure")
	}
}

// processOneBucket — insertHistograms query error

func TestOpsRollupCascade_ProcessOneBucket_HistogramQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
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
	mock.ExpectQuery(`FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("histogram query failed"))
	mock.ExpectRollback()

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from histogram query failure")
	}
}

// insertHistograms with data — per-thing and fleet (nil thingID) paths

// TestOpsRollupCascade_ProcessOneBucket_WithHistogramData exercises the
// histogram accumulation + INSERT path with both a per-thing row (thingID
// non-nil) and a fleet row (thingID nil) in the source.
func TestOpsRollupCascade_ProcessOneBucket_WithHistogramData(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	day := sealedDayBucket()
	expectCascadeWatermarkAndMin(mock, day)

	histMeta, _ := json.Marshal(map[string]any{"buckets": []int{5, 3, 2, 1, 0, 0}})
	thingID := "svc-cascade-1"

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	// insertHistograms source: 1 per-thing + 1 fleet row
	mock.ExpectQuery(`FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata", "sample_count"}).
			AddRow(&thingID, "ai-gateway", "hook.pipeline_ms", "", histMeta, int64(10)).
			AddRow((*string)(nil), "agent", "agent.fleet_ms", "", histMeta, int64(5)))

	// INSERT per-thing histogram row
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1d`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).WillReturnResult(pgxmock.NewResult("INSERT", 1))

	// INSERT fleet histogram row
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1d`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).WillReturnResult(pgxmock.NewResult("INSERT", 1))

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

// runCalendarMonth — full path with a sealed month

// TestOpsRollupCascade_RunCalendarMonth_FullPath exercises runCalendarMonth
// with a sealed past month: resolveCalendarCursor → bootstrap → processOneBucket.
func TestOpsRollupCascade_RunCalendarMonth_FullPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Use 2 months ago so the bucket is before the current month.
	now := time.Now().UTC()
	twoMonthsAgo := time.Date(now.Year(), now.Month()-2, 1, 0, 0, 0, 0, time.UTC)
	// The source (1d) table's oldest bucket_start
	oldestDay := twoMonthsAgo.Add(24 * time.Hour)

	// resolveCalendarCursor: watermark empty, minSourceBucket = oldestDay
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&oldestDay))

	// processOneBucket for twoMonthsAgo (month start)
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1mo`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1mo`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(`FROM metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata", "sample_count"}))
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	// processOneBucket for one month ago
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_ops_rollup_1mo`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO metric_ops_rollup_1mo`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(`FROM metric_ops_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "thing_type", "metric_name", "dimension_key", "metadata", "sample_count"}))
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewOpsRollup1mo(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// resolveCalendarCursor — advance-from-watermark path

// TestOpsRollupCascade_ResolveCalendarCursor_AdvanceFromWatermark exercises the
// branch where the watermark is set to a recent month and the oldest source
// bucket is older → advance-from-watermark wins.
func TestOpsRollupCascade_ResolveCalendarCursor_AdvanceFromWatermark(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Watermark = 2 months ago. Bootstrap = firstOfMonth(3 months ago) = 3mo ago.
	// advance = nextOpsMonth(2mo ago) = 1mo ago.
	// bootstrap < advance → bootstrap wins... wait: 3mo < 1mo so bootstrap IS older.
	// Actually we want advance to win: set watermark to 3 months ago, oldest to 4 months ago.
	// bootstrap = 4mo, advance = nextOpsMonth(3mo) = 2mo. 4mo < 2mo? No, 4mo ago is more negative
	// on the timeline. So firstOfMonth(4 months ago) is before nextOpsMonth(3 months ago).
	// That means bootstrap wins again. Let's flip: watermark at 1mo ago, oldest at 3mo ago.
	// bootstrap = firstOfMonth(3mo ago) = ~3mo; advance = nextOpsMonth(1mo ago) = ~now.
	// bootstrap < advance? yes → bootstrap wins.
	//
	// For advance-from-watermark to win: bootstrap >= advance.
	// bootstrap = firstOfMonth(oldest). advance = nextOpsMonth(watermark).
	// If watermark = 3mo ago, advance = 2mo ago. oldest = 2mo ago → bootstrap = 2mo ago.
	// 2mo ago >= 2mo ago → advance wins (returns advance).
	now := time.Now().UTC()
	threeMonthsAgo := time.Date(now.Year(), now.Month()-3, 1, 0, 0, 0, 0, time.UTC)
	twoMonthsAgo := time.Date(now.Year(), now.Month()-2, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(threeMonthsAgo))
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&twoMonthsAgo))

	j := NewOpsRollup1mo(nil, time.Hour, testLogger())
	j.pool = mock

	cursor, ok, err := j.resolveCalendarCursor(context.Background())
	if err != nil {
		t.Fatalf("resolveCalendarCursor: %v", err)
	}
	if !ok {
		t.Fatal("resolveCalendarCursor ok=false, expected data")
	}
	// advance = nextOpsMonth(3mo ago) = 2mo ago
	wantAdvance := nextOpsMonth(threeMonthsAgo)
	if !cursor.Equal(wantAdvance) {
		t.Errorf("cursor = %v, want %v (advance from watermark)", cursor, wantAdvance)
	}
}

// resolveFixedCursor — advance-from-watermark path (watermark >= bootstrap)

// TestOpsRollupCascade_ResolveFixedCursor_AdvanceFromWatermark exercises the
// branch where advance-from-watermark takes precedence over bootstrap.
func TestOpsRollupCascade_ResolveFixedCursor_AdvanceFromWatermark(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Watermark = 2 days ago. Oldest = also 2 days ago.
	// bootstrap = 2d truncated to 24h. advance = watermark + 24h = 1 day ago.
	// bootstrap >= advance? 2d is NOT >= 1d, so this means bootstrap < advance → bootstrap wins.
	// We need bootstrap >= advance: oldest must be newer than advance.
	// advance = watermark + 24h = 1d ago. oldest = 1d ago → bootstrap = 1d ago. bootstrap == advance → bootstrap >= advance.
	// But the condition is watermark.IsZero() || bootstrap < advance → if !IsZero and bootstrap >= advance → return advance.
	twoDaysAgo := time.Now().UTC().Truncate(24 * time.Hour).Add(-48 * time.Hour)
	oneDayAgo := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(twoDaysAgo))
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&oneDayAgo))

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock

	cursor, ok, err := j.resolveFixedCursor(context.Background())
	if err != nil {
		t.Fatalf("resolveFixedCursor: %v", err)
	}
	if !ok {
		t.Fatal("resolveFixedCursor ok=false")
	}
	// advance = twoDaysAgo + 24h = 1 day ago
	wantAdvance := twoDaysAgo.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	if !cursor.Equal(wantAdvance) {
		t.Errorf("cursor = %v, want %v (advance from watermark)", cursor, wantAdvance)
	}
}

// runFixed — watermark at boundary (no buckets to process)

// TestOpsRollupCascade_RunFixed_CursorNotBeforeLatestSealed asserts that when
// the cursor is at or after latestSealed (nothing to process) Run returns nil
// without calling processOneBucket.
func TestOpsRollupCascade_RunFixed_CursorNotBeforeLatestSealed(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Set watermark and oldest to the current (unsealed) day so cursor =
	// today, which is not before latestSealed (today) — no buckets.
	todayStart := time.Now().UTC().Truncate(24 * time.Hour)

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(todayStart))
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&todayStart))

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// runCalendarMonth — cursor not before current month (no-op)

// TestOpsRollupCascade_RunCalendarMonth_CursorAtCurrentMonth asserts that when
// cursor = start of current month no buckets are processed.
func TestOpsRollupCascade_RunCalendarMonth_CursorAtCurrentMonth(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now().UTC()
	currentMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&currentMonthStart))

	j := NewOpsRollup1mo(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
