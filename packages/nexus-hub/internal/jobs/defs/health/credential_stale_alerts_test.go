package health

import (
	"context"
	"errors"
	"testing"
	"time"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/pashagolub/pgxmock/v4"
)

func makeStaleRule(staleAfterDays int, enabled bool) *alerting.AlertRule {
	return &alerting.AlertRule{
		ID:              credStaleRuleID,
		Enabled:         enabled,
		DefaultSeverity: alerting.SeverityMedium,
		Params: map[string]any{
			"staleAfterDays": float64(staleAfterDays),
		},
	}
}

func TestCredentialStaleAlerts_Identity(t *testing.T) {
	j := NewCredentialStaleAlerts(nil, nil, nil, 0, testLogger())
	if j.ID() != credStaleJobID {
		t.Errorf("ID = %q", j.ID())
	}
	if j.Name() != credStaleJobName {
		t.Errorf("Name = %q", j.Name())
	}
	if j.Description() == "" {
		t.Errorf("Description empty")
	}
	if j.Interval() != time.Hour {
		t.Errorf("default Interval = %v", j.Interval())
	}
	j2 := NewCredentialStaleAlerts(nil, nil, nil, 5*time.Minute, testLogger())
	if j2.Interval() != 5*time.Minute {
		t.Errorf("custom Interval = %v", j2.Interval())
	}
}

func TestCredentialStaleAlerts_Run_RuleNotFound(t *testing.T) {
	loader := &fakeRuleLoader{err: alerting.ErrNotFound}
	j := &CredentialStaleAlertsJob{ruleLoader: loader, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCredentialStaleAlerts_Run_LoadError(t *testing.T) {
	sentinel := errors.New("load boom")
	loader := &fakeRuleLoader{err: sentinel}
	j := &CredentialStaleAlertsJob{ruleLoader: loader, logger: testLogger()}
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestCredentialStaleAlerts_Run_RuleDisabled(t *testing.T) {
	loader := &fakeRuleLoader{rule: makeStaleRule(7, false)}
	j := &CredentialStaleAlertsJob{ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCredentialStaleAlerts_Run_NonPositiveDaysSkipped(t *testing.T) {
	loader := &fakeRuleLoader{rule: makeStaleRule(0, true)}
	j := &CredentialStaleAlertsJob{ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCredentialStaleAlerts_Run_Happy(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	loader := &fakeRuleLoader{rule: makeStaleRule(7, true)}

	pastTime := time.Now().Add(-10 * 24 * time.Hour).UTC()
	mock.ExpectQuery(`FROM "Credential"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "lastSuccessAt", "lastUsedAt"}).
			AddRow("cred-1", "c1", &pastTime, &pastTime).
			AddRow("cred-2", "c2", (*time.Time)(nil), (*time.Time)(nil))) // never succeeded
	// resolveRecovered SELECT.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(credStaleRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("credential:cred-1"). // still firing
			AddRow("credential:cred-recovered").
			AddRow("not-a-cred-key"))

	raiser := &fakeRaiser{}
	j := &CredentialStaleAlertsJob{pool: mock, raiser: raiser, ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
	if len(raiser.raises) != 2 {
		t.Errorf("raises = %d, want 2", len(raiser.raises))
	}
	if len(raiser.resolves) != 1 {
		t.Errorf("resolves = %d, want 1 (cred-recovered)", len(raiser.resolves))
	}
}

func TestCredentialStaleAlerts_Run_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	loader := &fakeRuleLoader{rule: makeStaleRule(7, true)}

	sentinel := errors.New("query boom")
	mock.ExpectQuery(`FROM "Credential"`).WithArgs(pgxmock.AnyArg()).WillReturnError(sentinel)

	j := &CredentialStaleAlertsJob{pool: mock, raiser: &fakeRaiser{}, ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestCredentialStaleAlerts_Run_RaiseErrorWarnsButContinues(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	loader := &fakeRuleLoader{rule: makeStaleRule(7, true)}

	pastTime := time.Now().Add(-10 * 24 * time.Hour).UTC()
	mock.ExpectQuery(`FROM "Credential"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "lastSuccessAt", "lastUsedAt"}).
			AddRow("cred-1", "c1", &pastTime, &pastTime))
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credStaleRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &errRaiser{raiseErr: errors.New("raise boom")}
	j := &CredentialStaleAlertsJob{pool: mock, raiser: raiser, ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run should not return raise error: %v", err)
	}
}

func TestCredentialStaleAlerts_ResolveRecovered_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("resolve query boom")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credStaleRuleID).WillReturnError(sentinel)

	j := &CredentialStaleAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	err := j.resolveRecovered(context.Background(), map[string]bool{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}
