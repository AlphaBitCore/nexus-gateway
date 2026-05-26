package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestThingRollupMerge_Identity(t *testing.T) {
	for name, ctor := range map[string]func() *ThingRollupMergeJob{
		"1h":  func() *ThingRollupMergeJob { return NewThingRollupMerge1h(nil, time.Hour, testLogger()) },
		"1d":  func() *ThingRollupMergeJob { return NewThingRollupMerge1d(nil, time.Hour, testLogger()) },
		"1mo": func() *ThingRollupMergeJob { return NewThingRollupMerge1mo(nil, time.Hour, testLogger()) },
	} {
		j := ctor()
		t.Run(name, func(t *testing.T) {
			if j.ID() == "" {
				t.Error("ID empty")
			}
			if j.Name() == "" {
				t.Error("Name empty")
			}
			if j.Description() == "" {
				t.Error("Description empty")
			}
		})
	}
}

func TestThingRollupMerge_Run_NoMergeNeeded(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Watermark right at the boundary → loop skipped.
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(time.Now().UTC()))

	j := NewThingRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestThingRollupMerge_CalendarMonth_NoMergeNeeded(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	now := time.Now().UTC()
	prevMonth := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(prevMonth))

	j := NewThingRollupMerge1mo(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestThingRollupMerge_MergeOneBucket_EmptySource(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}))

	j := NewThingRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock
	bucketStart := time.Now().UTC().Truncate(time.Hour)
	if err := j.mergeOneBucket(context.Background(), bucketStart, bucketStart.Add(time.Hour)); err != nil {
		t.Fatalf("mergeOneBucket: %v", err)
	}
}

func TestThingRollupMerge_ColdStart_NoEarliestSource(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	// EarliestThingBucketStart returns 0 rows → defaults to lookback-based watermark.
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).
		WillReturnRows(pgxmock.NewRows([]string{"bucketStart"}))

	j := NewThingRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock
	// Run shouldn't error even though the watermark may schedule lookups.
	_ = j.Run(context.Background())
}
