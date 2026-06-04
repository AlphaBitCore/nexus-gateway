package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// CredentialReliabilityAlertsJob raises and resolves three alert rules
// driven by the persisted reliability state on the Credential table:
//
//   * credential.circuit_open               — circuitState != 'closed'
//   * credential.health_unavailable         — healthStatus = 'unavailable'
//   * credential.health_degraded_sustained  — healthStatus = 'degraded' for
//                                             ≥ Thresholds.HealthSustainedDegradedSeconds
//
// Class 1 (state-table) — sits alongside thing.offline / vk.expiring /
// credential.expiring, not in the streaming alerteval engine. Each tick
// rebuilds the "should be firing" set and emits Raise/Resolve calls
// through the standard *alerting.Raiser (idempotent + audited).

const (
	credReliabilityJobID          = "credential-reliability-alerts"
	credReliabilityJobName        = "Credential Reliability Alerts"
	credReliabilityJobDescription = "Raises credential.circuit_open, credential.health_unavailable, and credential.health_degraded_sustained alerts from the persisted reliability state on the Credential table. See docs/developers/architecture/control-plane/credentials-architecture.md."

	credCircuitOpenRuleID           = "credential.circuit_open"
	credHealthUnavailableRuleID     = "credential.health_unavailable"
	credHealthDegradedSustainedRule = "credential.health_degraded_sustained"

	credReliabilityTargetPrefix = "credential:"
)

// thresholdsReader is the minimum surface of ReliabilityThresholdsLoader
// used by CredentialReliabilityAlertsJob. Declared locally so this package
// stays free of a direct dependency on rollup. *rollup.ReliabilityThresholdsLoader
// satisfies this interface.
type thresholdsReader interface {
	Thresholds(ctx context.Context) credstate.Thresholds
}

type CredentialReliabilityAlertsJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// snapshot SELECT and resolve-recovered SELECT can be driven by
	// pgxmock without sharing real Credential rows.
	pool       defs.PgxPool
	raiser     defs.AlertRaiser
	ruleLoader ruleLoader
	thresholds thresholdsReader
	interval   time.Duration
	logger     *slog.Logger
}

func NewCredentialReliabilityAlerts(pool *pgxpool.Pool, raiser defs.AlertRaiser, ruleLoader ruleLoader, thresholds thresholdsReader, interval time.Duration, logger *slog.Logger) *CredentialReliabilityAlertsJob {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &CredentialReliabilityAlertsJob{
		pool:       pool,
		raiser:     raiser,
		ruleLoader: ruleLoader,
		thresholds: thresholds,
		interval:   interval,
		logger:     logger.With("job", credReliabilityJobID),
	}
}

func (j *CredentialReliabilityAlertsJob) ID() string              { return credReliabilityJobID }
func (j *CredentialReliabilityAlertsJob) Name() string            { return credReliabilityJobName }
func (j *CredentialReliabilityAlertsJob) Description() string     { return credReliabilityJobDescription }
func (j *CredentialReliabilityAlertsJob) Interval() time.Duration { return j.interval }

// reliabilitySnapshot is the per-credential view this job evaluates.
type reliabilitySnapshot struct {
	id                    string
	name                  string
	circuitState          string
	circuitReason         *string
	circuitOpenedAt       *time.Time
	healthStatus          string
	healthDominantError   *string
	healthTrend           *string
	healthStatusChangedAt *time.Time
}

func (j *CredentialReliabilityAlertsJob) Run(ctx context.Context) error {
	rules, err := j.loadRules(ctx)
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}
	if len(rules) == 0 {
		// No rule rows in the AlertRule table (e.g. fresh DB). Nothing to do.
		return nil
	}

	snapshots, err := j.snapshot(ctx)
	if err != nil {
		return fmt.Errorf("snapshot credentials: %w", err)
	}

	t := credstate.DefaultThresholds
	if j.thresholds != nil {
		t = j.thresholds.Thresholds(ctx)
	}
	sustained := time.Duration(t.HealthSustainedDegradedSeconds) * time.Second
	now := time.Now().UTC()

	firing := make(map[string]map[string]bool, len(rules)) // ruleID → set of credID
	for ruleID := range rules {
		firing[ruleID] = make(map[string]bool)
	}

	for _, s := range snapshots {
		if rule, ok := rules[credCircuitOpenRuleID]; ok && s.circuitState != credstate.CircuitClosed {
			firing[credCircuitOpenRuleID][s.id] = true
			j.raise(ctx, rule, s, circuitOpenMessage(s))
		}
		if rule, ok := rules[credHealthUnavailableRuleID]; ok && s.healthStatus == credstate.HealthUnavailable {
			firing[credHealthUnavailableRuleID][s.id] = true
			j.raise(ctx, rule, s, healthUnavailableMessage(s))
		}
		if rule, ok := rules[credHealthDegradedSustainedRule]; ok && s.healthStatus == credstate.HealthDegraded {
			if s.healthStatusChangedAt != nil && now.Sub(*s.healthStatusChangedAt) >= sustained {
				firing[credHealthDegradedSustainedRule][s.id] = true
				j.raise(ctx, rule, s, healthDegradedSustainedMessage(s, sustained))
			}
		}
	}

	// Resolve previously-firing alerts whose state has cleared.
	for ruleID := range rules {
		if err := j.resolveRecovered(ctx, ruleID, firing[ruleID]); err != nil {
			j.logger.Warn("resolve recovered failed", "ruleId", ruleID, "error", err)
		}
	}
	return nil
}

func (j *CredentialReliabilityAlertsJob) loadRules(ctx context.Context) (map[string]*alerting.AlertRule, error) {
	ids := []string{credCircuitOpenRuleID, credHealthUnavailableRuleID, credHealthDegradedSustainedRule}
	out := make(map[string]*alerting.AlertRule, len(ids))
	for _, id := range ids {
		r, err := j.ruleLoader.GetRule(ctx, id)
		if err != nil {
			if errors.Is(err, alerting.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("get rule %s: %w", id, err)
		}
		if r != nil && r.Enabled {
			out[id] = r
		}
	}
	return out, nil
}

func (j *CredentialReliabilityAlertsJob) snapshot(ctx context.Context) ([]reliabilitySnapshot, error) {
	rows, err := j.pool.Query(ctx, `
		SELECT id, name,
		       "circuitState", "circuitReason", "circuitOpenedAt",
		       "healthStatus", "healthDominantError", "healthTrend", "healthStatusChangedAt"
		FROM   "Credential"
		WHERE  enabled = TRUE
	`)
	if err != nil {
		return nil, fmt.Errorf("query credentials: %w", err)
	}
	defer rows.Close()
	var out []reliabilitySnapshot
	for rows.Next() {
		var s reliabilitySnapshot
		if err := rows.Scan(&s.id, &s.name,
			&s.circuitState, &s.circuitReason, &s.circuitOpenedAt,
			&s.healthStatus, &s.healthDominantError, &s.healthTrend, &s.healthStatusChangedAt,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (j *CredentialReliabilityAlertsJob) raise(ctx context.Context, rule *alerting.AlertRule, s reliabilitySnapshot, msg string) {
	details := map[string]any{
		"credentialId":   s.id,
		"credentialName": s.name,
		"circuitState":   s.circuitState,
		"healthStatus":   s.healthStatus,
	}
	if s.circuitReason != nil {
		details["circuitReason"] = *s.circuitReason
	}
	if s.circuitOpenedAt != nil {
		details["circuitOpenedAt"] = s.circuitOpenedAt.Format(time.RFC3339)
	}
	if s.healthDominantError != nil {
		details["healthDominantError"] = *s.healthDominantError
	}
	if s.healthTrend != nil {
		details["healthTrend"] = *s.healthTrend
	}
	if s.healthStatusChangedAt != nil {
		details["healthStatusChangedAt"] = s.healthStatusChangedAt.Format(time.RFC3339)
	}
	err := j.raiser.Raise(ctx, alerting.RaiseInput{
		RuleID:      rule.ID,
		TargetKey:   credReliabilityTargetPrefix + s.id,
		TargetLabel: s.name,
		Severity:    rule.DefaultSeverity,
		Message:     msg,
		Details:     details,
	})
	if err != nil {
		j.logger.Warn("raise alert failed", "ruleId", rule.ID, "credentialId", s.id, "error", err)
	}
}

func (j *CredentialReliabilityAlertsJob) resolveRecovered(ctx context.Context, ruleID string, currentlyFiring map[string]bool) error {
	rows, err := j.pool.Query(ctx, `
		SELECT "targetKey"
		FROM   "Alert"
		WHERE  "ruleId" = $1
		  AND  state IN ('FIRING'::"AlertState", 'ACKNOWLEDGED'::"AlertState")
	`, ruleID)
	if err != nil {
		return fmt.Errorf("list firing %s: %w", ruleID, err)
	}
	defer rows.Close()
	var firing []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return fmt.Errorf("scan targetKey: %w", err)
		}
		firing = append(firing, k)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, targetKey := range firing {
		credID := strings.TrimPrefix(targetKey, credReliabilityTargetPrefix)
		if credID == targetKey {
			continue
		}
		if currentlyFiring[credID] {
			continue
		}
		if err := j.raiser.Resolve(ctx, ruleID, targetKey, "recovered"); err != nil {
			j.logger.Warn("resolve alert failed", "ruleId", ruleID, "targetKey", targetKey, "error", err)
		}
	}
	return nil
}

// Message builders — kept top-level so tests can assert content stability.

func circuitOpenMessage(s reliabilitySnapshot) string {
	reason := "unknown"
	if s.circuitReason != nil && *s.circuitReason != "" {
		reason = *s.circuitReason
	}
	return fmt.Sprintf("Credential %q circuit is %s (reason: %s) — admin reset may be required",
		s.name, s.circuitState, reason)
}

func healthUnavailableMessage(s reliabilitySnapshot) string {
	dom := "mixed"
	if s.healthDominantError != nil && *s.healthDominantError != "" {
		dom = *s.healthDominantError
	}
	return fmt.Sprintf("Credential %q is unavailable; dominant failure: %s — investigate or rotate the key",
		s.name, dom)
}

func healthDegradedSustainedMessage(s reliabilitySnapshot, sustained time.Duration) string {
	dom := "mixed"
	if s.healthDominantError != nil && *s.healthDominantError != "" {
		dom = *s.healthDominantError
	}
	return fmt.Sprintf("Credential %q has been degraded for ≥ %s; dominant failure: %s",
		s.name, sustained.Round(time.Second), dom)
}
