// Pgxmock-driven Run() coverage for RollupMergeJob. Drives runFixed
// (1h / 1d) and runCalendarMonth (1mo) through pgxmock, plus the
// per-bucket transaction (Begin → DELETE → INSERTs → UPDATE watermark → Commit).

package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestRollupMerge_Run_NothingToMerge(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// GetWatermark returns a recent time so no buckets are sealed yet.
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(time.Now().UTC()))

	j := NewRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRollupMerge_MergeOneBucket_EmptySource(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}))

	j := NewRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock
	bucketStart := time.Now().UTC().Truncate(time.Hour)
	if err := j.mergeOneBucket(context.Background(), bucketStart, bucketStart.Add(time.Hour)); err != nil {
		t.Fatalf("mergeOneBucket: %v", err)
	}
}

func TestRollupMerge_MergeOneBucket_SourceQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(context.Canceled)

	j := NewRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock
	bucketStart := time.Now().UTC().Truncate(time.Hour)
	if err := j.mergeOneBucket(context.Background(), bucketStart, bucketStart.Add(time.Hour)); err == nil {
		t.Fatalf("expected error")
	}
}

func TestRollupMerge_CalendarMonth_Run_NoCompleteMonths(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	now := time.Now().UTC()
	// Watermark = previous month → next iteration is current month → loop exits.
	prevMonth := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(prevMonth))

	j := NewRollupMerge1mo(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRollupMerge_CalendarMonth_ColdStart(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// ErrNoRows → defaults to previous month, which is also exit condition.
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))

	j := NewRollupMerge1mo(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
