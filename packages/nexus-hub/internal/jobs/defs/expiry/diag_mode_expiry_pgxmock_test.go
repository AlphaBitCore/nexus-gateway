// Pgxmock-driven unit tests for DiagModeExpiryJob. The DB-backed
// counterparts in diag_mode_expiry_test.go skip without a live Postgres,
// so these run in every CI invocation.

package expiry

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestDiagModeExpiry_Run_NoExpiredRows(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT DISTINCT t.id`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))

	j := &DiagModeExpiryJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestDiagModeExpiry_Run_ClearsTwoRows(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT DISTINCT t.id`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("thing-1").AddRow("thing-2"))
	mock.ExpectExec(`UPDATE thing`).WithArgs("thing-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE thing`).WithArgs("thing-2").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	j := &DiagModeExpiryJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestDiagModeExpiry_Run_QueryError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	sentinel := errors.New("query boom")
	mock.ExpectQuery(`SELECT DISTINCT t.id`).WillReturnError(sentinel)

	j := &DiagModeExpiryJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestDiagModeExpiry_Run_UpdateErrorContinues(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// First UPDATE errs, second succeeds — job logs warn but doesn't abort.
	mock.ExpectQuery(`SELECT DISTINCT t.id`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("thing-1").AddRow("thing-2"))
	mock.ExpectExec(`UPDATE thing`).WithArgs("thing-1").
		WillReturnError(errors.New("permission denied"))
	mock.ExpectExec(`UPDATE thing`).WithArgs("thing-2").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	j := &DiagModeExpiryJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestDiagModeExpiry_Run_RowsErrSurfaces(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// Rows.Err() set on iteration.
	sentinel := errors.New("iter boom")
	mock.ExpectQuery(`SELECT DISTINCT t.id`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("thing-1").RowError(0, sentinel))

	j := &DiagModeExpiryJob{pool: mock, logger: testLogger()}
	err = j.Run(context.Background())
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel surface", err)
	}
}
