package health

import (
	"context"
	"errors"
	"testing"
	"time"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/pashagolub/pgxmock/v4"
)

func makeAgentCertRule(enabled bool, warnDays []any) *alerting.AlertRule {
	return &alerting.AlertRule{
		ID:              agentCertExpiryRuleID,
		Enabled:         enabled,
		DefaultSeverity: alerting.SeverityHigh,
		Params: map[string]any{
			"warnDays": warnDays,
		},
	}
}

func TestAgentCertExpiration_Identity(t *testing.T) {
	j := NewAgentCertExpirationAlerts(nil, nil, nil, 0, testLogger())
	if j.ID() != agentCertExpiryJobID {
		t.Errorf("ID = %q", j.ID())
	}
	if j.Name() != agentCertExpiryJobName {
		t.Errorf("Name = %q", j.Name())
	}
	if j.Description() == "" {
		t.Errorf("Description empty")
	}
	if j.Interval() != time.Hour {
		t.Errorf("default Interval = %v", j.Interval())
	}
}

func TestAgentCertExpiration_Run_RuleNotFound(t *testing.T) {
	loader := &fakeRuleLoader{err: alerting.ErrNotFound}
	j := &AgentCertExpirationAlertsJob{ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestAgentCertExpiration_Run_LoadError(t *testing.T) {
	sentinel := errors.New("load boom")
	loader := &fakeRuleLoader{err: sentinel}
	j := &AgentCertExpirationAlertsJob{ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestAgentCertExpiration_Run_RuleDisabled(t *testing.T) {
	loader := &fakeRuleLoader{rule: makeAgentCertRule(false, []any{30.0, 7.0, 1.0})}
	j := &AgentCertExpirationAlertsJob{ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestAgentCertExpiration_Run_WarnDaysMissing(t *testing.T) {
	rule := &alerting.AlertRule{
		ID:      agentCertExpiryRuleID,
		Enabled: true,
		Params:  map[string]any{},
	}
	loader := &fakeRuleLoader{rule: rule}
	j := &AgentCertExpirationAlertsJob{ruleLoader: loader, logger: testLogger()}
	err := j.Run(context.Background())
	if err == nil {
		t.Fatalf("expected parse error")
	}
}

func TestAgentCertExpiration_Run_Happy(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	loader := &fakeRuleLoader{rule: makeAgentCertRule(true, []any{30.0, 7.0, 1.0})}

	soon := time.Now().UTC().Add(3 * 24 * time.Hour)
	mock.ExpectQuery(`FROM thing_agent ta`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"thing_id", "hostname", "cert_expires_at"}).
			AddRow("agent-1", "host-1", soon).
			AddRow("agent-2", "host-2", soon))
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(agentCertExpiryRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("thing:agent-1").
			AddRow("thing:agent-renewed").
			AddRow("malformed-key"))

	raiser := &fakeRaiser{}
	j := &AgentCertExpirationAlertsJob{pool: mock, raiser: raiser, ruleLoader: loader, logger: testLogger()}
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
		t.Errorf("resolves = %d, want 1 (agent-renewed)", len(raiser.resolves))
	}
}

func TestAgentCertExpiration_Run_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	loader := &fakeRuleLoader{rule: makeAgentCertRule(true, []any{30.0})}
	sentinel := errors.New("query boom")
	mock.ExpectQuery(`FROM thing_agent`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)
	j := &AgentCertExpirationAlertsJob{pool: mock, raiser: &fakeRaiser{}, ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestAgentCertExpiration_ResolveRecovered_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("resolve query boom")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(agentCertExpiryRuleID).WillReturnError(sentinel)

	j := &AgentCertExpirationAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	err := j.resolveRecovered(context.Background(), map[string]bool{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}
