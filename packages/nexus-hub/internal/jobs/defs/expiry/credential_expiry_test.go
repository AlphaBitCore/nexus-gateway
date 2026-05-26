package expiry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestCredentialExpiry_Identity(t *testing.T) {
	j := NewCredentialExpiry(nil, nil, 0, testLogger())
	if j.ID() != credExpiryJobID {
		t.Errorf("ID = %q", j.ID())
	}
	if j.Name() != credExpiryJobName {
		t.Errorf("Name = %q", j.Name())
	}
	if j.Description() == "" {
		t.Errorf("Description empty")
	}
	if j.Interval() != time.Hour {
		t.Errorf("default Interval = %v", j.Interval())
	}
	j2 := NewCredentialExpiry(nil, nil, 30*time.Minute, testLogger())
	if j2.Interval() != 30*time.Minute {
		t.Errorf("custom Interval = %v", j2.Interval())
	}
}

func TestCredentialExpiry_Run_Happy(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// Step 1a: ListExpiringCredentials returns 2 rows.
	mock.ExpectQuery(`FROM "Credential"`).WithArgs(credExpiryWarnDays).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "providerId", "expiresAt"}).
			AddRow("cred-1", "c1", "prov-1", nowPlusDays(3)).
			AddRow("cred-2", "c2", "prov-2", nowPlusDays(10)))
	// Step 1b: MarkCredentialsPendingRotation (Exec) — 2 IDs.
	mock.ExpectExec(`UPDATE "Credential"`).
		WithArgs("cred-1", "cred-2").
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	// Step 2: ListOverdueCredentials returns 1 row.
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "providerId", "expiresAt"}).
			AddRow("cred-3", "c3-overdue", "prov-3", nowPlusDays(-2)))
	// Step 3: resolveRecovered SELECT.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(credExpiryRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("credential:cred-1").       // still firing
			AddRow("credential:cred-no-more"). // recovered
			AddRow("malformed-key"))           // skipped

	raiser := &fakeRaiser{}
	j := &CredentialExpiryJob{pool: mock, raiser: raiser, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}

	if len(raiser.raises) != 3 {
		t.Errorf("raises = %d, want 3 (2 expiring + 1 overdue)", len(raiser.raises))
	}
	if len(raiser.resolves) != 1 {
		t.Errorf("resolves = %d, want 1 (cred-no-more)", len(raiser.resolves))
	}
}

func TestCredentialExpiry_Run_ListExpiringError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("expiring boom")
	mock.ExpectQuery(`FROM "Credential"`).WithArgs(credExpiryWarnDays).WillReturnError(sentinel)
	mock.ExpectQuery(`FROM "Credential"`). // ListOverdueCredentials still runs
						WillReturnRows(pgxmock.NewRows([]string{"id", "name", "providerId", "expiresAt"}))
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credExpiryRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	j := &CredentialExpiryJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestCredentialExpiry_Run_OverdueError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "Credential"`).WithArgs(credExpiryWarnDays).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "providerId", "expiresAt"}))
	sentinel := errors.New("overdue boom")
	mock.ExpectQuery(`FROM "Credential"`).WillReturnError(sentinel)
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credExpiryRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	j := &CredentialExpiryJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestCredentialExpiry_ResolveRecovered_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("resolve query boom")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credExpiryRuleID).WillReturnError(sentinel)

	j := &CredentialExpiryJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	err := j.resolveRecovered(context.Background(), map[string]bool{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestCredentialExpiry_ResolveRecovered_ResolveErrorWarns(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credExpiryRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).AddRow("credential:cred-x"))

	raiser := &errRaiser{resolveErr: errors.New("resolve failed")}
	j := &CredentialExpiryJob{pool: mock, raiser: raiser, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); err != nil {
		t.Errorf("unexpected err: %v", err)
	}
}
