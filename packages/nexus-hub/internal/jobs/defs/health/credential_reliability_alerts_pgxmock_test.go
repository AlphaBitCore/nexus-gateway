// Pgxmock-driven Run() coverage for CredentialReliabilityAlertsJob,
// supplementing the existing DB-backed test which skips without Postgres.

package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

func enabledReliabilityRule(id string, sev alerting.Severity) *alerting.AlertRule {
	return &alerting.AlertRule{ID: id, Enabled: true, DefaultSeverity: sev}
}

type fixedThresholds struct {
	t credstate.Thresholds
}

func (f *fixedThresholds) Thresholds(context.Context) credstate.Thresholds { return f.t }

func TestCredentialReliabilityAlerts_Identity(t *testing.T) {
	j := NewCredentialReliabilityAlerts(nil, nil, nil, nil, 0, testLogger())
	if j.ID() != credReliabilityJobID {
		t.Errorf("ID = %q", j.ID())
	}
	if j.Name() != credReliabilityJobName {
		t.Errorf("Name = %q", j.Name())
	}
	if j.Description() == "" {
		t.Errorf("Description empty")
	}
	if j.Interval() != 60*time.Second {
		t.Errorf("default Interval = %v", j.Interval())
	}
}

func TestCredentialReliabilityAlerts_Run_NoRules(t *testing.T) {
	loader := &multiRuleLoader{rules: map[string]*alerting.AlertRule{}}
	j := &CredentialReliabilityAlertsJob{ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCredentialReliabilityAlerts_Run_LoadRuleError(t *testing.T) {
	sentinel := errors.New("rule boom")
	loader := &errMultiRuleLoader{err: sentinel}
	j := &CredentialReliabilityAlertsJob{ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// errMultiRuleLoader returns a non-ErrNotFound err.
type errMultiRuleLoader struct{ err error }

func (e *errMultiRuleLoader) GetRule(context.Context, string) (*alerting.AlertRule, error) {
	return nil, e.err
}

func TestCredentialReliabilityAlerts_Run_SnapshotError(t *testing.T) {
	loader := &multiRuleLoader{rules: map[string]*alerting.AlertRule{
		credCircuitOpenRuleID: enabledReliabilityRule(credCircuitOpenRuleID, alerting.SeverityHigh),
	}}
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("snap boom")
	mock.ExpectQuery(`FROM   "Credential"`).WillReturnError(sentinel)

	j := &CredentialReliabilityAlertsJob{pool: mock, raiser: &fakeRaiser{}, ruleLoader: loader, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestCredentialReliabilityAlerts_Run_Happy(t *testing.T) {
	loader := &multiRuleLoader{rules: map[string]*alerting.AlertRule{
		credCircuitOpenRuleID:           enabledReliabilityRule(credCircuitOpenRuleID, alerting.SeverityHigh),
		credHealthUnavailableRuleID:     enabledReliabilityRule(credHealthUnavailableRuleID, alerting.SeverityHigh),
		credHealthDegradedSustainedRule: enabledReliabilityRule(credHealthDegradedSustainedRule, alerting.SeverityMedium),
	}}
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	openTime := time.Now().Add(-30 * time.Minute).UTC()
	openErr := "5xx_burst"
	trend := "degrading"
	mock.ExpectQuery(`FROM   "Credential"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "circuitState", "circuitReason", "circuitOpenedAt", "healthStatus", "healthDominantError", "healthTrend", "healthStatusChangedAt"}).
			AddRow("cred-open", "circuit-open-cred", "open", &openErr, &openTime, "healthy", (*string)(nil), &trend, &openTime).
			AddRow("cred-unavail", "unavail-cred", "closed", (*string)(nil), (*time.Time)(nil), "unavailable", &openErr, &trend, &openTime).
			AddRow("cred-degraded", "degraded-cred", "closed", (*string)(nil), (*time.Time)(nil), "degraded", &openErr, &trend, &openTime))

	// resolveRecovered for each rule.
	for range 3 {
		mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))
	}

	raiser := &fakeRaiser{}
	j := &CredentialReliabilityAlertsJob{
		pool:       mock,
		raiser:     raiser,
		ruleLoader: loader,
		thresholds: &fixedThresholds{t: credstate.Thresholds{HealthSustainedDegradedSeconds: 600}},
		logger:     testLogger(),
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) < 2 {
		t.Errorf("expected at least 2 raises, got %d", len(raiser.raises))
	}
}

func TestCredentialReliabilityAlerts_ResolveRecovered_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("resolve query boom")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(credCircuitOpenRuleID).WillReturnError(sentinel)

	j := &CredentialReliabilityAlertsJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveRecovered(context.Background(), credCircuitOpenRuleID, map[string]bool{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestCircuitOpenMessage_FormatsReason(t *testing.T) {
	r := "rate_limit"
	s := reliabilitySnapshot{name: "c1", circuitState: credstate.CircuitOpen, circuitReason: &r}
	if msg := circuitOpenMessage(s); msg == "" {
		t.Errorf("circuitOpenMessage empty")
	}
}

func TestHealthUnavailableMessage_NotEmpty(t *testing.T) {
	dom := "5xx"
	s := reliabilitySnapshot{name: "c1", healthStatus: credstate.HealthUnavailable, healthDominantError: &dom}
	if msg := healthUnavailableMessage(s); msg == "" {
		t.Errorf("healthUnavailableMessage empty")
	}
}

func TestHealthDegradedSustainedMessage_NotEmpty(t *testing.T) {
	dom := "5xx"
	s := reliabilitySnapshot{name: "c1", healthStatus: credstate.HealthDegraded, healthDominantError: &dom}
	if msg := healthDegradedSustainedMessage(s, 10*time.Minute); msg == "" {
		t.Errorf("healthDegradedSustainedMessage empty")
	}
}
