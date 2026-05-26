package expiry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestCredentialRetire_Identity(t *testing.T) {
	j := NewCredentialRetire(nil, 0, testLogger())
	if j.ID() != credRetireJobID {
		t.Errorf("ID = %q", j.ID())
	}
	if j.Name() != credRetireJobName {
		t.Errorf("Name = %q", j.Name())
	}
	if j.Description() != credRetireJobDesc {
		t.Errorf("Description = %q", j.Description())
	}
	if j.Interval() != time.Hour {
		t.Errorf("default interval = %v, want 1h", j.Interval())
	}
	j2 := NewCredentialRetire(nil, 7*time.Minute, testLogger())
	if j2.Interval() != 7*time.Minute {
		t.Errorf("custom interval = %v", j2.Interval())
	}
}

func TestCredentialRetire_Run_BothStepsRun(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "Credential"\s+SET status = 'retired'`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	mock.ExpectExec(`DELETE FROM "Credential"\s+WHERE status = 'retired'`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 5))

	j := &CredentialRetireJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

func TestCredentialRetire_Run_ZeroCounts(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "Credential"`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`DELETE FROM "Credential"`).WithArgs(pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("DELETE", 0))

	j := &CredentialRetireJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCredentialRetire_Run_AdvanceFails(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	sentinel := errors.New("advance failed")
	mock.ExpectExec(`UPDATE "Credential"`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(sentinel)

	j := &CredentialRetireJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestCredentialRetire_Run_DeleteFails(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	sentinel := errors.New("delete failed")
	mock.ExpectExec(`UPDATE "Credential"`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`DELETE FROM "Credential"`).WithArgs(pgxmock.AnyArg()).WillReturnError(sentinel)

	j := &CredentialRetireJob{pool: mock, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}
