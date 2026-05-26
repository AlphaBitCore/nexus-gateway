// DB-integration tests for CredentialReliabilityAlertsJob.
// Real Postgres (skip-on-unavailable) + fakeRaiser. No mocks.

package health

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// multiRuleLoader serves the three reliability rules. Missing entries
// trigger ErrNotFound — which the job treats as "rule not seeded" and
// skips gracefully.
type multiRuleLoader struct {
	rules map[string]*alerting.AlertRule
}

func (m *multiRuleLoader) GetRule(_ context.Context, id string) (*alerting.AlertRule, error) {
	r, ok := m.rules[id]
	if !ok {
		return nil, alerting.ErrNotFound
	}
	return r, nil
}

func reliabilityRule(id string, sev alerting.Severity) *alerting.AlertRule {
	return &alerting.AlertRule{
		ID:              id,
		Enabled:         true,
		DefaultSeverity: sev,
		Params:          map[string]any{},
	}
}

// updateCredentialReliabilityState rewrites the persisted reliability
// columns on a seeded credential row. Used by the alert tests to put
// the row into the state under evaluation without round-tripping
// through the flush/rollup jobs.
func updateCredentialReliabilityState(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string,
	circuitState, circuitReason, healthStatus string,
	healthStatusChangedAt *time.Time,
) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		UPDATE "Credential" SET
			"circuitState"          = $2,
			"circuitReason"         = NULLIF($3, ''),
			"healthStatus"          = $4,
			"healthStatusChangedAt" = $5,
			"updatedAt"             = NOW()
		WHERE id = $1
	`, id, circuitState, circuitReason, healthStatus, healthStatusChangedAt)
	if err != nil {
		t.Fatalf("update reliability state: %v", err)
	}
}

// cleanupReliabilityAlertRows removes Alert rows the test seeded so reruns
// stay idempotent.
func cleanupReliabilityAlertRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, credID string) {
	t.Helper()
	for _, ruleID := range []string{
		credCircuitOpenRuleID, credHealthUnavailableRuleID, credHealthDegradedSustainedRule,
	} {
		_, _ = pool.Exec(ctx, `DELETE FROM "Alert" WHERE "ruleId" = $1 AND "targetKey" = $2`,
			ruleID, credReliabilityTargetPrefix+credID)
	}
}

// staticThresholdsAlerts is a thresholdsReader returning a Thresholds
// with a tunable sustained-degraded horizon (1 s in milliseconds).
type staticThresholdsAlerts struct{ sustainedSec int }

func (s staticThresholdsAlerts) Thresholds(_ context.Context) credstate.Thresholds {
	t := credstate.DefaultThresholds
	if s.sustainedSec > 0 {
		t.HealthSustainedDegradedSeconds = s.sustainedSec
	}
	return t
}

// TestReliabilityAlerts_CircuitOpenRaisesAndResolves: a credential whose
// persisted circuitState != 'closed' fires credential.circuit_open;
// flipping it back to closed resolves the firing alert.
func TestReliabilityAlerts_CircuitOpenRaisesAndResolves(t *testing.T) {
	pool := healthRollupTestPool(t)
	t.Cleanup(pool.Close)
	providerID := ensureTestProvider(t, pool)
	now := time.Now().UTC()
	id, cleanup := seedCredential(t, pool, providerID,
		credstate.CircuitOpen, credstate.ReasonAuthFail, &now, nil)
	defer cleanup()
	ctx := context.Background()
	defer cleanupReliabilityAlertRows(t, ctx, pool, id)

	loader := &multiRuleLoader{rules: map[string]*alerting.AlertRule{
		credCircuitOpenRuleID:           reliabilityRule(credCircuitOpenRuleID, alerting.SeverityHigh),
		credHealthUnavailableRuleID:     reliabilityRule(credHealthUnavailableRuleID, alerting.SeverityHigh),
		credHealthDegradedSustainedRule: reliabilityRule(credHealthDegradedSustainedRule, alerting.SeverityMedium),
	}}
	raiser := &fakeRaiser{}
	job := NewCredentialReliabilityAlerts(pool, raiser, loader, staticThresholdsAlerts{}, 60*time.Second, testLogger())

	if err := job.Run(ctx); err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	if got := countRaises(raiser, credCircuitOpenRuleID, credReliabilityTargetPrefix+id); got != 1 {
		t.Fatalf("circuit_open raise count: got %d want 1", got)
	}

	// Flip back to closed; next Run should resolve the alert. The resolve
	// path needs an existing FIRING Alert row, so seed one (the job did
	// not actually write to the Alert table — we used fakeRaiser).
	updateCredentialReliabilityState(t, ctx, pool, id,
		credstate.CircuitClosed, "", credstate.HealthHealthy, &now)
	if _, err := pool.Exec(ctx, `
		INSERT INTO "Alert" (id, "ruleId", "sourceType", "targetKey", "targetLabel",
			severity, state, message, details, "firedAt", "lastSeenAt", "duplicateCount")
		VALUES (gen_random_uuid()::text, $1, 'provider', $2, $3,
			'HIGH'::"AlertSeverity", 'FIRING'::"AlertState", 'test', '{}', NOW(), NOW(), 1)`,
		credCircuitOpenRuleID, credReliabilityTargetPrefix+id, id,
	); err != nil {
		t.Fatalf("seed firing alert: %v", err)
	}

	if err := job.Run(ctx); err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if got := countResolves(raiser, credCircuitOpenRuleID, credReliabilityTargetPrefix+id); got != 1 {
		t.Fatalf("circuit_open resolve count: got %d want 1", got)
	}
}

// TestReliabilityAlerts_HealthUnavailableFires: healthStatus='unavailable'
// fires the credential.health_unavailable rule.
func TestReliabilityAlerts_HealthUnavailableFires(t *testing.T) {
	pool := healthRollupTestPool(t)
	t.Cleanup(pool.Close)
	providerID := ensureTestProvider(t, pool)
	id, cleanup := seedCredential(t, pool, providerID, credstate.CircuitClosed, "", nil, nil)
	defer cleanup()
	ctx := context.Background()
	defer cleanupReliabilityAlertRows(t, ctx, pool, id)

	now := time.Now().UTC()
	updateCredentialReliabilityState(t, ctx, pool, id,
		credstate.CircuitClosed, "", credstate.HealthUnavailable, &now)

	loader := &multiRuleLoader{rules: map[string]*alerting.AlertRule{
		credHealthUnavailableRuleID: reliabilityRule(credHealthUnavailableRuleID, alerting.SeverityHigh),
	}}
	raiser := &fakeRaiser{}
	job := NewCredentialReliabilityAlerts(pool, raiser, loader, staticThresholdsAlerts{}, 60*time.Second, testLogger())

	if err := job.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := countRaises(raiser, credHealthUnavailableRuleID, credReliabilityTargetPrefix+id); got != 1 {
		t.Fatalf("health_unavailable raise count: got %d want 1", got)
	}
}

// TestReliabilityAlerts_DegradedSustainedHorizon: degraded fires only
// once healthStatusChangedAt is older than the sustained horizon.
func TestReliabilityAlerts_DegradedSustainedHorizon(t *testing.T) {
	pool := healthRollupTestPool(t)
	t.Cleanup(pool.Close)
	providerID := ensureTestProvider(t, pool)
	id, cleanup := seedCredential(t, pool, providerID, credstate.CircuitClosed, "", nil, nil)
	defer cleanup()
	ctx := context.Background()
	defer cleanupReliabilityAlertRows(t, ctx, pool, id)

	loader := &multiRuleLoader{rules: map[string]*alerting.AlertRule{
		credHealthDegradedSustainedRule: reliabilityRule(credHealthDegradedSustainedRule, alerting.SeverityMedium),
	}}
	// Sustained horizon = 1 second. Phase 1: changedAt = now → too fresh.
	now := time.Now().UTC()
	updateCredentialReliabilityState(t, ctx, pool, id,
		credstate.CircuitClosed, "", credstate.HealthDegraded, &now)
	raiser := &fakeRaiser{}
	job := NewCredentialReliabilityAlerts(pool, raiser, loader,
		staticThresholdsAlerts{sustainedSec: 1}, 60*time.Second, testLogger())

	if err := job.Run(ctx); err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	if got := countRaises(raiser, credHealthDegradedSustainedRule, credReliabilityTargetPrefix+id); got != 0 {
		t.Fatalf("expected no raise before horizon, got %d", got)
	}

	// Phase 2: rewind changedAt past the horizon.
	past := now.Add(-2 * time.Second)
	updateCredentialReliabilityState(t, ctx, pool, id,
		credstate.CircuitClosed, "", credstate.HealthDegraded, &past)

	if err := job.Run(ctx); err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if got := countRaises(raiser, credHealthDegradedSustainedRule, credReliabilityTargetPrefix+id); got != 1 {
		t.Fatalf("expected raise after horizon, got %d", got)
	}
}

// TestReliabilityAlerts_MissingRuleSkipped: every GetRule lookup
// returning ErrNotFound makes the job a no-op. Treating absent rules as
// "not seeded" lets the binary roll out before the seed migration runs.
func TestReliabilityAlerts_MissingRuleSkipped(t *testing.T) {
	pool := healthRollupTestPool(t)
	t.Cleanup(pool.Close)
	providerID := ensureTestProvider(t, pool)
	now := time.Now().UTC()
	id, cleanup := seedCredential(t, pool, providerID,
		credstate.CircuitOpen, credstate.ReasonAuthFail, &now, nil)
	defer cleanup()
	ctx := context.Background()
	defer cleanupReliabilityAlertRows(t, ctx, pool, id)

	loader := &multiRuleLoader{rules: map[string]*alerting.AlertRule{}}
	raiser := &fakeRaiser{}
	job := NewCredentialReliabilityAlerts(pool, raiser, loader, staticThresholdsAlerts{}, 60*time.Second, testLogger())

	if err := job.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if raiser.raiseCount() != 0 {
		t.Fatalf("no rules → no raises, got %d", raiser.raiseCount())
	}
}

// helpers — used only inside this file.

func countRaises(f *fakeRaiser, ruleID, targetKey string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.raises {
		if r.RuleID == ruleID && r.TargetKey == targetKey {
			n++
		}
	}
	return n
}

func countResolves(f *fakeRaiser, ruleID, targetKey string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.resolves {
		if r.RuleID == ruleID && r.TargetKey == targetKey {
			n++
		}
	}
	return n
}
