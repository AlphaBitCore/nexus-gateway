package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestMetricsRollup_Identity(t *testing.T) {
	j := NewMetricsRollup(nil, time.Hour, testLogger())
	if j.ID() != "metrics-rollup" {
		t.Errorf("ID = %q, want metrics-rollup", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name must not be empty")
	}
	if j.Description() == "" {
		t.Error("Description must not be empty")
	}
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h", j.Interval())
	}
}

func TestMetricsRollup_IntervalDefault(t *testing.T) {
	j := NewMetricsRollup(nil, 0, testLogger())
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h default", j.Interval())
	}
}

func TestMetricsRollup_Run_EmptyAllSourcesNoTx(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT status, COUNT\(\*\) FROM thing`).
		WillReturnRows(pgxmock.NewRows([]string{"status", "n"}))
	mock.ExpectQuery(`SELECT COALESCE\(t\.os`).
		WillReturnRows(pgxmock.NewRows([]string{"os", "n"}))
	mock.ExpectQuery(`SELECT action, COUNT\(\*\) FROM traffic_event`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"action", "n"}))

	j := &MetricsRollupJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestMetricsRollup_Run_FullPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT status, COUNT\(\*\) FROM thing`).
		WillReturnRows(pgxmock.NewRows([]string{"status", "n"}).
			AddRow("online", int(5)).
			AddRow("offline", int(2)))
	mock.ExpectQuery(`SELECT COALESCE\(t\.os`).
		WillReturnRows(pgxmock.NewRows([]string{"os", "n"}).
			AddRow("darwin", int(3)).
			AddRow("windows", int(2)).
			AddRow("linux", int(1))) // folded to "other"
	mock.ExpectQuery(`SELECT action, COUNT\(\*\) FROM traffic_event`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"action", "n"}).
			AddRow("upload", int(10)))

	// Tx: Begin, DELETE, repeated Exec (one per RollupRow ~7), Commit.
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM metric_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	// Each rollup row → one Exec INSERT.
	for range 6 {
		mock.ExpectExec(`INSERT INTO "metric_rollup_1h"`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	mock.ExpectCommit()

	j := &MetricsRollupJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestMetricsRollup_Run_BeginError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT status, COUNT\(\*\) FROM thing`).
		WillReturnRows(pgxmock.NewRows([]string{"status", "n"}).AddRow("online", int(1)))
	mock.ExpectQuery(`SELECT COALESCE\(t\.os`).
		WillReturnRows(pgxmock.NewRows([]string{"os", "n"}))
	mock.ExpectQuery(`SELECT action, COUNT\(\*\) FROM traffic_event`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"action", "n"}))
	sentinel := errors.New("begin boom")
	mock.ExpectBegin().WillReturnError(sentinel)

	j := &MetricsRollupJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestMetricsRollup_Run_FleetStatusQueryError(t *testing.T) {
	// All three source queries fail; rows empty so no tx; errs joined returned.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("fleet boom")
	mock.ExpectQuery(`SELECT status, COUNT\(\*\) FROM thing`).WillReturnError(sentinel)
	mock.ExpectQuery(`SELECT COALESCE\(t\.os`).WillReturnError(errors.New("os boom"))
	mock.ExpectQuery(`SELECT action, COUNT\(\*\) FROM traffic_event`).WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("actions boom"))

	j := &MetricsRollupJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel joined", err)
	}
}
