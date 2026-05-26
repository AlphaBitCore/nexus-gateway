package rollup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestRollupRetention_Identity(t *testing.T) {
	j := NewRollupRetention(nil, DefaultRollupRetention(), 24*time.Hour, testLogger())
	if j.ID() != "rollup-retention" {
		t.Errorf("ID = %q, want rollup-retention", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name must not be empty")
	}
	if j.Description() == "" {
		t.Error("Description must not be empty")
	}
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h", j.Interval())
	}
}

func TestRollupRetention_DefaultsMatchCP(t *testing.T) {
	cfg := DefaultRollupRetention()
	if cfg.Rollup5mDays != 7 || cfg.Rollup1hDays != 90 || cfg.Rollup1dDays != 365 || cfg.Rollup1moDays != 1825 {
		t.Errorf("defaults drifted: %+v", cfg)
	}
}

func TestRollupRetention_IntervalDefault(t *testing.T) {
	j := NewRollupRetention(nil, DefaultRollupRetention(), 0, testLogger())
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h default", j.Interval())
	}
}

func TestRollupRetention_Run_AllTiers(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	for range 4 {
		mock.ExpectExec(`DELETE FROM`).WithArgs(pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("DELETE", 3))
	}
	j := &RollupRetentionJob{pool: mock, cfg: DefaultRollupRetention(), logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestRollupRetention_Run_AllDisabled(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := &RollupRetentionJob{pool: mock, cfg: RollupRetentionConfig{}, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRollupRetention_Run_PartialError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("boom")
	mock.ExpectExec(`DELETE FROM`).WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectExec(`DELETE FROM`).WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`DELETE FROM`).WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`DELETE FROM`).WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	j := &RollupRetentionJob{pool: mock, cfg: DefaultRollupRetention(), logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel joined", err)
	}
}
