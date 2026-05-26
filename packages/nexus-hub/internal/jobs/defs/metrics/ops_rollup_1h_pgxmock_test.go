package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestOpsRollup1h_RunOnStartTrue(t *testing.T) {
	j := NewOpsRollup1h(nil, 0, testLogger())
	if !j.RunOnStart() {
		t.Errorf("RunOnStart should be true")
	}
}

func TestOpsRollup1h_Run_EmptyRawTable(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// GetWatermark returns no rows.
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	// minSampledAt returns NULL → no work.
	mock.ExpectQuery(`SELECT MIN\(sampled_at\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow((*time.Time)(nil)))

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestOpsRollup1h_Run_WatermarkAtBoundary(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Watermark recent + minSampledAt also recent → no buckets to process.
	now := time.Now().UTC().Truncate(time.Hour)
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(now))
	mock.ExpectQuery(`SELECT MIN\(sampled_at\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&now))

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestOpsRollup1h_Run_WatermarkError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("watermark boom")
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestOpsRollup1h_MinSampledAt_Error(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("min boom")
	mock.ExpectQuery(`SELECT MIN\(sampled_at\)`).WillReturnError(sentinel)

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock
	_, _, err := j.minSampledAt(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestOpsRollup1h_ProcessOneBucket_BeginError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin().WillReturnError(context.Canceled)

	j := NewOpsRollup1h(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.processOneBucket(context.Background(), time.Now().UTC().Truncate(time.Hour)); err == nil {
		t.Fatalf("expected error")
	}
}
