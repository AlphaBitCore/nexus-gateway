package expiry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func TestPassthroughExpiry_Identity(t *testing.T) {
	j := NewPassthroughExpiryJob(nil, 0, testLogger())
	if j.ID() != passthroughExpiryJobID {
		t.Errorf("ID = %q", j.ID())
	}
	if j.Name() == "" {
		t.Errorf("Name is empty")
	}
	if j.Description() == "" {
		t.Errorf("Description is empty")
	}
	if !j.RunOnStart() {
		t.Errorf("RunOnStart should be true")
	}
	if j.Interval() != 60*time.Second {
		t.Errorf("default Interval = %v, want 60s", j.Interval())
	}
	j2 := NewPassthroughExpiryJob(nil, 5*time.Minute, testLogger())
	if j2.Interval() != 5*time.Minute {
		t.Errorf("custom Interval = %v", j2.Interval())
	}
}

func TestPassthroughExpiry_Run_AllTiersClean(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// Global: simulate pgx.ErrNoRows from RETURNING.
	mock.ExpectQuery(`UPDATE gateway_passthrough_config_global`).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`UPDATE gateway_passthrough_config_adapter`).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`UPDATE gateway_passthrough_config_provider`).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	j := &PassthroughExpiryJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestPassthroughExpiry_Run_GlobalReturnsCount(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`UPDATE gateway_passthrough_config_global`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(3)))
	mock.ExpectExec(`UPDATE gateway_passthrough_config_adapter`).
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	mock.ExpectExec(`UPDATE gateway_passthrough_config_provider`).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	j := &PassthroughExpiryJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestPassthroughExpiry_Run_AdapterFails(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	sentinel := errors.New("adapter boom")
	mock.ExpectQuery(`UPDATE gateway_passthrough_config_global`).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`UPDATE gateway_passthrough_config_adapter`).
		WillReturnError(sentinel)

	j := &PassthroughExpiryJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestPassthroughExpiry_Run_ProviderFails(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	sentinel := errors.New("provider boom")
	mock.ExpectQuery(`UPDATE gateway_passthrough_config_global`).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`UPDATE gateway_passthrough_config_adapter`).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`UPDATE gateway_passthrough_config_provider`).
		WillReturnError(sentinel)

	j := &PassthroughExpiryJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}
