// Third batch of extra tests for the health package covering the remaining
// rows.Err() / warn-log branches left after the second pass.

package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// agent_cert_expiration_alerts.go: remaining error branches

// TestAgentCertExpiration_Run_RowsErrInScanLoop covers the rows.Err() path
// in the agent scan loop (line 118-120).
func TestAgentCertExpiration_Run_RowsErrInScanLoop(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	loader := &fakeRuleLoader{rule: makeAgentCertRule(true, []any{float64(30)})}
	sentinel := errors.New("agent rows err")
	soon := time.Now().UTC().Add(3 * 24 * time.Hour)
	mock.ExpectQuery(`FROM thing_agent ta`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "hostname", "cert_expires_at"}).
			AddRow("agent-1", "host-1", soon).RowError(0, sentinel))

	j := &AgentCertExpirationAlertsJob{pool: mock, raiser: &fakeRaiser{}, ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestAgentCertExpiration_Run_WarnDaysEmptyByValue covers the
// "warnDays empty / wrong type" error path in Run (line 79-81) — the
// condition is reached when parseWarnDays returns an error.
func TestAgentCertExpiration_Run_WarnDaysWrongType(t *testing.T) {
	rule := makeAgentCertRule(true, nil) // warnDays key exists but is nil → empty []any → error
	// Override warnDays to a value that causes empty-output error.
	rule.Params["warnDays"] = []any{"string-not-number"}
	loader := &fakeRuleLoader{rule: rule}
	j := &AgentCertExpirationAlertsJob{ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected parseWarnDays error")
	}
}

// TestAgentCertExpiration_Run_RaiseErrorWarnsNoReturn covers the raise error
// warn path (line 148-150) — raise fails but Run still succeeds.
func TestAgentCertExpiration_Run_RaiseErrorWarnsNoReturn(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	loader := &fakeRuleLoader{rule: makeAgentCertRule(true, []any{float64(30)})}
	soon := time.Now().UTC().Add(3 * 24 * time.Hour)
	mock.ExpectQuery(`FROM thing_agent ta`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "hostname", "cert_expires_at"}).
			AddRow("agent-warn", "warn-host", soon))
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(agentCertExpiryRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	j := &AgentCertExpirationAlertsJob{
		pool:       mock,
		raiser:     &errRaiser{raiseErr: errors.New("raise boom")},
		ruleLoader: loader,
		logger:     testLogger(),
	}
	// Raise error is just logged; Run must return nil.
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run must not return raise warn error; got %v", err)
	}
}

// TestAgentCertExpiration_Run_ResolveRecoveredWarnNoReturn covers the
// resolveRecovered error warn path (line 155-157) — resolveRecovered fails
// but Run still returns nil.
func TestAgentCertExpiration_Run_ResolveRecoveredWarnNoReturn(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	loader := &fakeRuleLoader{rule: makeAgentCertRule(true, []any{float64(30)})}
	// No expiring agents.
	mock.ExpectQuery(`FROM thing_agent ta`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "hostname", "cert_expires_at"}))
	// resolveRecovered query fails.
	sentinel := errors.New("resolve boom")
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(agentCertExpiryRuleID).
		WillReturnError(sentinel)

	j := &AgentCertExpirationAlertsJob{pool: mock, raiser: &fakeRaiser{}, ruleLoader: loader, logger: testLogger()}
	// resolveRecovered error is just logged (Warn) — Run must return nil.
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run must not return resolveRecovered error; got %v", err)
	}
}

// TestAgentCertExpiration_ResolveRecovered_RowsErrPropagates covers the
// rows.Err() path in resolveRecovered (line 182-184).
func TestAgentCertExpiration_ResolveRecovered_RowsErrPropagates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("iter err")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(agentCertExpiryRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("thing:agent-1").RowError(0, sentinel))

	j := &AgentCertExpirationAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestAgentCertExpiration_ResolveRecovered_ResolveWarnNoReturn covers the
// resolve failure warn path (line 192-194) in resolveRecovered.
func TestAgentCertExpiration_ResolveRecovered_ResolveWarnNoReturn(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(agentCertExpiryRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("thing:agent-gone"))

	raiser := &errRaiser{resolveErr: errors.New("resolve boom")}
	j := &AgentCertExpirationAlertsJob{pool: mock, raiser: raiser, logger: testLogger()}
	// Resolve failure is just logged — resolveRecovered must return nil.
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); err != nil {
		t.Fatalf("resolveRecovered must not return resolve warn error; got %v", err)
	}
}

// cache_quality_monitor.go: revertToDryRun — rows.Err in the scan loop

// TestCacheQualityMonitor_RevertToDryRun_InnerRowsErr covers the rows.Err()
// path after the scan loop body (line 173-175) that calls rows.Close() first.
func TestCacheQualityMonitor_RevertToDryRun_InnerRowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("inner iter err")
	// First row succeeds with no-rules config, second row triggers RowError.
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("openai", []byte(`{"other": true}`)).
			AddRow("gemini", []byte(`{"rules":{"r1":{"enabled":false}}}`)).
			RowError(1, sentinel))

	j := &CacheQualityMonitorJob{pool: mock, logger: testLogger()}
	if err := j.revertToDryRun(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// credential_stale_alerts.go: resolveRecovered — resolve alert failed warn

// TestCredentialStaleAlerts_ResolveRecovered_ResolveWarnNoReturn covers the
// resolve alert failed warn path (line 184-186).
func TestCredentialStaleAlerts_ResolveRecovered_ResolveWarnNoReturn(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credStaleRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("credential:stale-gone"))

	raiser := &errRaiser{resolveErr: errors.New("resolve boom")}
	j := &CredentialStaleAlertsJob{pool: mock, raiser: raiser, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); err != nil {
		t.Fatalf("resolveRecovered must not return resolve warn error; got %v", err)
	}
}

// TestCredentialStaleAlerts_ResolveRecovered_RowsErrFromRowError covers the
// rows.Err() path in resolveRecovered (iterate firing alerts — line 173-175).
func TestCredentialStaleAlerts_ResolveRecovered_IterFiringRowsErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("firing rows err")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credStaleRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("credential:c1").RowError(0, sentinel))

	j := &CredentialStaleAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), map[string]bool{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestCredentialStaleAlerts_Run_ResolveRecoveredWarnNoReturn covers the warn
// path when resolveRecovered fails inside Run (line 147-149).
func TestCredentialStaleAlerts_Run_ResolveRecoveredWarnNoReturn(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	loader := &fakeRuleLoader{rule: makeStaleRule(7, true)}
	// No stale credentials.
	mock.ExpectQuery(`FROM "Credential"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "lastSuccessAt", "lastUsedAt"}))
	// resolveRecovered query fails.
	sentinel := errors.New("resolve boom")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credStaleRuleID).
		WillReturnError(sentinel)

	j := &CredentialStaleAlertsJob{pool: mock, raiser: &fakeRaiser{}, ruleLoader: loader, logger: testLogger()}
	// resolveRecovered error is logged as Warn inside Run — Run returns nil.
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run must not return resolveRecovered warn error; got %v", err)
	}
}
