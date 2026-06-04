package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestOpsRollup5m_RunOnStartTrue(t *testing.T) {
	j := NewOpsRollup5m(nil, 0, testLogger())
	if !j.RunOnStart() {
		t.Errorf("RunOnStart should be true")
	}
}

func TestOpsRollup5m_Run_EmptyRawTable(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()
	// GetWatermark returns no rows.
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	// minSampledAt returns NULL → no work.
	mock.ExpectQuery(`SELECT MIN\(sampled_at\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow((*time.Time)(nil)))

	j := NewOpsRollup5m(nil, time.Minute, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestOpsRollup5m_Run_WatermarkAtBoundary(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()
	// Watermark recent + minSampledAt also recent → no sealed bucket past the
	// watermark, so Run resolves a cursor at/after latestSealed and no-ops.
	// Aligned to the 5-minute bucket boundary (opsBucketDur) so advanceFrom-
	// Watermark = now+5m is never before latestSealed = (now-1m).Truncate(5m).
	now := time.Now().UTC().Truncate(opsBucketDur)
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(now))
	mock.ExpectQuery(`SELECT MIN\(sampled_at\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&now))

	j := NewOpsRollup5m(nil, time.Minute, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestOpsRollup5m_Run_WatermarkError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()
	sentinel := errors.New("watermark boom")
	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)

	j := NewOpsRollup5m(nil, time.Minute, testLogger())
	j.pool = mock
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestOpsRollup5m_MinSampledAt_Error(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()
	sentinel := errors.New("min boom")
	mock.ExpectQuery(`SELECT MIN\(sampled_at\)`).WillReturnError(sentinel)

	j := NewOpsRollup5m(nil, time.Minute, testLogger())
	j.pool = mock
	_, _, err := j.minSampledAt(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestOpsRollup5m_ProcessOneBucket_BeginError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	mock.MatchExpectationsInOrder(false) // histogram INSERT order is map-iteration nondeterministic
	defer mock.Close()
	mock.ExpectBegin().WillReturnError(context.Canceled)

	j := NewOpsRollup5m(nil, time.Minute, testLogger())
	j.pool = mock
	if err := j.processOneBucket(context.Background(), time.Now().UTC().Truncate(opsBucketDur)); err == nil {
		t.Fatalf("expected error")
	}
}
