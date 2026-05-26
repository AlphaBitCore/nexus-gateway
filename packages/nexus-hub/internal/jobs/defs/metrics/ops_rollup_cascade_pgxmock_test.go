package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestOpsRollupCascade_RunOnStartTrue(t *testing.T) {
	j := NewOpsRollup1d(nil, 0, testLogger())
	if !j.RunOnStart() {
		t.Errorf("RunOnStart should be true")
	}
}

func TestOpsRollupCascade_Run_EmptySource_Fixed(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow((*time.Time)(nil)))

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestOpsRollupCascade_Run_EmptySource_Calendar(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow((*time.Time)(nil)))

	j := NewOpsRollup1mo(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestOpsRollupCascade_Run_FixedWatermarkAtBoundary(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	now := time.Now().UTC().Truncate(24 * time.Hour)
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(now))
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&now))

	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestOpsRollupCascade_ResolveFixedCursor_WatermarkError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("wm boom")
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)
	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock
	if _, _, err := j.resolveFixedCursor(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestOpsRollupCascade_ResolveCalendarCursor_WatermarkError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("wm boom")
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)
	j := NewOpsRollup1mo(nil, time.Hour, testLogger())
	j.pool = mock
	if _, _, err := j.resolveCalendarCursor(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestOpsRollupCascade_MinSourceBucket_Error(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("min boom")
	mock.ExpectQuery(`SELECT MIN\(bucket_start\)`).WillReturnError(sentinel)
	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock
	_, _, err := j.minSourceBucket(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestOpsRollupCascade_ProcessOneBucket_BeginError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin().WillReturnError(context.Canceled)
	j := NewOpsRollup1d(nil, time.Hour, testLogger())
	j.pool = mock
	if err := j.processOneBucket(context.Background(),
		time.Now().UTC().Truncate(24*time.Hour),
		time.Now().UTC().Truncate(24*time.Hour).Add(24*time.Hour),
	); err == nil {
		t.Fatalf("expected error")
	}
}

func TestFirstOfMonthAndNextOpsMonth(t *testing.T) {
	in := time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)
	if got := firstOfMonth(in); !got.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("firstOfMonth = %v", got)
	}
	if got := nextOpsMonth(in); !got.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("nextOpsMonth = %v", got)
	}
	dec := time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC)
	if got := nextOpsMonth(dec); !got.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("nextOpsMonth(dec 5) = %v", got)
	}
}
