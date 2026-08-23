package rollup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// stubCorrectionLayer satisfies both correction seams (5m aggregation and the
// three merges) so a watermark test does not have to queue the several hundred
// pgxmock expectations a real window costs. err, when set, fails every call.
type stubCorrectionLayer struct {
	err   error
	calls int
}

func (s *stubCorrectionLayer) processBucket(_ context.Context, _ time.Time, _ bool) error {
	s.calls++
	return s.err
}

func (s *stubCorrectionLayer) mergeBucket(_ context.Context, _, _ time.Time, _ bool) error {
	s.calls++
	return s.err
}

func correctionJobWithStub(pool pgxmock.PgxPoolIface, stub *stubCorrectionLayer, now time.Time) *RollupCorrectionJob {
	return &RollupCorrectionJob{
		pool:         pool,
		r5m:          stub,
		merge1h:      stub,
		merge1d:      stub,
		merge1mo:     stub,
		lookbackDays: 1,
		interval:     24 * time.Hour,
		logger:       testLogger(),
		nowFn:        func() time.Time { return now },
	}
}

// TestRollupCorrection_PublishesWatermarkThroughYesterday pins the value other
// jobs read off this cursor: the newest UTC day the pass rebuilt, which is
// yesterday — today is still accumulating events and is never re-aggregated.
// The vendor-bill reconcile job compares day <= watermark, so a value one day
// too high would let it compare a day whose rollup rows do not exist yet, and
// one day too low would defer every day at steady state.
func TestRollupCorrection_PublishesWatermarkThroughYesterday(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(WatermarkCorrection, time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	stub := &stubCorrectionLayer{}
	j := correctionJobWithStub(mock, stub, time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC))
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// One lookback day = 288 five-minute buckets + 24 hourly merges + 1 daily
	// merge, all of them before the cursor moved.
	if want := 288 + 24 + 1; stub.calls != want {
		t.Errorf("layer calls = %d, want %d — the cursor must publish only after the whole window ran", stub.calls, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRollupCorrection_NoWatermarkWhenWindowFails: a run that aborted mid-window
// must leave the previous cursor standing. Publishing it anyway would claim days
// the pass never rebuilt, which is exactly the false "the series is absent"
// signal readers use it to avoid. No expectation is queued, so any attempt to
// open the watermark transaction surfaces as a different error than the sentinel.
func TestRollupCorrection_NoWatermarkWhenWindowFails(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	sentinel := errors.New("5m boom")
	j := correctionJobWithStub(mock, &stubCorrectionLayer{err: sentinel}, time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC))

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the aggregation sentinel (and no watermark write)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRollupCorrection_WatermarkWriteFailureFailsRun: the rollup rows are
// committed by this point, but a cursor that silently failed to advance keeps
// every reader deferring days that are in fact rebuilt. The run status is the
// only place that is visible, so it fails.
func TestRollupCorrection_WatermarkWriteFailureFailsRun(t *testing.T) {
	cases := []struct {
		name  string
		queue func(pgxmock.PgxPoolIface)
	}{
		{"begin fails", func(m pgxmock.PgxPoolIface) {
			m.ExpectBegin().WillReturnError(errors.New("begin refused"))
		}},
		{"exec fails", func(m pgxmock.PgxPoolIface) {
			m.ExpectBegin()
			m.ExpectExec(`INSERT INTO "rollup_watermark"`).
				WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
				WillReturnError(errors.New("upsert refused"))
			m.ExpectRollback()
		}},
		{"commit fails", func(m pgxmock.PgxPoolIface) {
			m.ExpectBegin()
			m.ExpectExec(`INSERT INTO "rollup_watermark"`).
				WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
				WillReturnResult(pgxmock.NewResult("INSERT", 1))
			m.ExpectCommit().WillReturnError(errors.New("commit refused"))
			m.ExpectRollback()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock: %v", err)
			}
			defer mock.Close()
			c.queue(mock)

			j := correctionJobWithStub(mock, &stubCorrectionLayer{}, time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC))
			err = j.Run(context.Background())
			if err == nil {
				t.Fatal("expected the watermark failure to fail the run")
			}
			if got := err.Error(); !strings.Contains(got, "correction watermark") {
				t.Fatalf("error = %q, want it to name the correction watermark", got)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations: %v", err)
			}
		})
	}
}
