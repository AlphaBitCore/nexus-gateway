package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
)

const (
	agentCertExpiryJobID          = "agent-cert-expiration-alerts"
	agentCertExpiryJobName        = "Agent Cert Expiration Alerts"
	agentCertExpiryJobDescription = "Polls thing_agent.cert_expires_at for desktop-agent Things and raises agent.cert_expiration_imminent alerts as the expiry approaches each warn-day threshold (default 30 / 14 / 7 / 1 days). Auto-resolves when the cert is renewed."

	agentCertExpiryRuleID = "agent.cert_expiration_imminent"
	agentCertTargetPrefix = "thing:"
)

// AgentCertExpirationAlertsJob polls thing_agent.cert_expires_at for
// agent-type Things and raises agent.cert_expiration_imminent alerts
// when the cert is within any of the configured warn-day thresholds.
// Class 1 (state-table) — sits alongside thing.offline / vk.expiring,
// not in the streaming Engine.
type AgentCertExpirationAlertsJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// thing_agent SELECT + resolve-recovered SELECT can be driven by
	// pgxmock without sharing real agent rows.
	pool       defs.PgxPool
	raiser     defs.AlertRaiser
	ruleLoader ruleLoader
	interval   time.Duration
	logger     *slog.Logger
}

// NewAgentCertExpirationAlerts constructs the job. interval defaults to 1 hour.
func NewAgentCertExpirationAlerts(pool *pgxpool.Pool, raiser defs.AlertRaiser, alertStore ruleLoader, interval time.Duration, logger *slog.Logger) *AgentCertExpirationAlertsJob {
	if interval <= 0 {
		interval = time.Hour
	}
	return &AgentCertExpirationAlertsJob{
		pool:       pool,
		raiser:     raiser,
		ruleLoader: alertStore,
		interval:   interval,
		logger:     logger.With("job", agentCertExpiryJobID),
	}
}

func (j *AgentCertExpirationAlertsJob) ID() string              { return agentCertExpiryJobID }
func (j *AgentCertExpirationAlertsJob) Name() string            { return agentCertExpiryJobName }
func (j *AgentCertExpirationAlertsJob) Description() string     { return agentCertExpiryJobDescription }
func (j *AgentCertExpirationAlertsJob) Interval() time.Duration { return j.interval }

func (j *AgentCertExpirationAlertsJob) Run(ctx context.Context) error {
	rule, err := j.ruleLoader.GetRule(ctx, agentCertExpiryRuleID)
	if err != nil {
		if errors.Is(err, alerting.ErrNotFound) {
			j.logger.Warn("agent.cert_expiration_imminent rule not found in store, skipping")
			return nil
		}
		return fmt.Errorf("load rule: %w", err)
	}
	if rule == nil || !rule.Enabled {
		return nil
	}

	warnDays, err := parseWarnDays(rule.Params)
	if err != nil {
		return fmt.Errorf("parse rule params: %w", err)
	}
	if len(warnDays) == 0 {
		return nil
	}
	// Outer window = max warnDays.
	maxWarn := warnDays[0]
	for _, d := range warnDays {
		if d > maxWarn {
			maxWarn = d
		}
	}
	now := time.Now().UTC()
	cutoff := now.AddDate(0, 0, maxWarn)

	rows, err := j.pool.Query(ctx, `
		SELECT ta.thing_id, COALESCE(t.hostname, ''), ta.cert_expires_at
		FROM thing_agent ta
		JOIN thing t ON t.id = ta.thing_id
		WHERE ta.cert_expires_at IS NOT NULL
		  AND ta.cert_expires_at <= $1
		  AND ta.cert_expires_at > $2
	`, cutoff, now)
	if err != nil {
		return fmt.Errorf("query expiring agents: %w", err)
	}
	defer rows.Close()

	type agentRow struct {
		thingID   string
		hostname  string
		expiresAt time.Time
	}
	var expiring []agentRow
	for rows.Next() {
		var r agentRow
		if err := rows.Scan(&r.thingID, &r.hostname, &r.expiresAt); err != nil {
			return fmt.Errorf("scan agent row: %w", err)
		}
		expiring = append(expiring, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate agent rows: %w", err)
	}

	shouldFire := make(map[string]bool, len(expiring))
	for _, a := range expiring {
		daysLeft := int(a.expiresAt.Sub(now).Hours() / 24)
		// Pick the smallest warnDays that's >= daysLeft (the most-urgent
		// band the cert has crossed).
		band := warnDaysBand(warnDays, daysLeft)
		if band <= 0 {
			continue
		}
		targetKey := agentCertTargetPrefix + a.thingID
		shouldFire[a.thingID] = true
		details := map[string]any{
			"thingId":   a.thingID,
			"hostname":  a.hostname,
			"daysLeft":  daysLeft,
			"warnBand":  band,
			"expiresAt": a.expiresAt.Format(time.RFC3339),
		}
		msg := fmt.Sprintf("Agent %q mTLS cert expires in %d days (warn band %dd)", a.hostname, daysLeft, band)
		if err := j.raiser.Raise(ctx, alerting.RaiseInput{
			RuleID:      agentCertExpiryRuleID,
			TargetKey:   targetKey,
			TargetLabel: a.hostname,
			Severity:    rule.DefaultSeverity,
			Message:     msg,
			Details:     details,
		}); err != nil {
			j.logger.Warn("raise agent cert expiry alert failed", "thingId", a.thingID, "error", err)
		}
	}

	// Auto-resolve: walk firing alerts and resolve any whose thing_id is
	// no longer in shouldFire (cert was renewed).
	if err := j.resolveRecovered(ctx, shouldFire); err != nil {
		j.logger.Warn("resolve recovered agent cert alerts failed", "error", err)
	}
	return nil
}

func (j *AgentCertExpirationAlertsJob) resolveRecovered(ctx context.Context, shouldFire map[string]bool) error {
	rows, err := j.pool.Query(ctx, `
		SELECT "targetKey"
		FROM "Alert"
		WHERE "ruleId" = $1
		  AND state IN ('FIRING'::"AlertState", 'ACKNOWLEDGED'::"AlertState")`,
		agentCertExpiryRuleID,
	)
	if err != nil {
		return fmt.Errorf("list firing alerts: %w", err)
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
		return fmt.Errorf("iterate firing alerts: %w", err)
	}
	for _, targetKey := range firing {
		thingID := strings.TrimPrefix(targetKey, agentCertTargetPrefix)
		if thingID == targetKey {
			continue
		}
		if shouldFire[thingID] {
			continue
		}
		if err := j.raiser.Resolve(ctx, agentCertExpiryRuleID, targetKey, "renewed"); err != nil {
			j.logger.Warn("resolve agent cert alert failed", "targetKey", targetKey, "error", err)
		}
	}
	return nil
}

// parseWarnDays normalises the params.warnDays config into a sorted-ascending
// integer slice. JSON arrays unmarshal as []any so we have to type-switch
// on each element.
func parseWarnDays(params map[string]any) ([]int, error) {
	raw, ok := params["warnDays"]
	if !ok {
		return nil, fmt.Errorf("warnDays missing from rule params")
	}
	var out []int
	switch v := raw.(type) {
	case []int:
		out = append(out, v...)
	case []any:
		for _, x := range v {
			switch n := x.(type) {
			case int:
				out = append(out, n)
			case int64:
				out = append(out, int(n))
			case float64:
				out = append(out, int(n))
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("warnDays empty / wrong type")
	}
	sort.Ints(out)
	return out, nil
}

// warnDaysBand returns the smallest warnDays threshold that daysLeft has
// crossed (so daysLeft <= band). Returns 0 if daysLeft is greater than
// every warn threshold (shouldn't fire — outside the warn window).
func warnDaysBand(warnDays []int, daysLeft int) int {
	for _, d := range warnDays {
		if daysLeft <= d {
			return d
		}
	}
	return 0
}
