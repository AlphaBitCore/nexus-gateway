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
)

const (
	credStaleJobID          = "credential-stale-alerts"
	credStaleJobName        = "Credential Stale Last-Success Alerts"
	credStaleJobDescription = "Polls Credential.lastSuccessAt for enabled credentials and raises credential.stale_last_success alerts when no successful use in params.staleAfterDays. Auto-resolves once a credential is used successfully again."

	credStaleRuleID       = "credential.stale_last_success"
	credStaleTargetPrefix = "credential:"
)

// CredentialStaleAlertsJob raises credential.stale_last_success alerts
// for enabled Credentials whose lastSuccessAt is older than
// params.staleAfterDays. Class 1 (state-table) — sits alongside
// thing.offline / vk.expiring, not in the streaming Engine.
type CredentialStaleAlertsJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// stale-Credential SELECT + resolve-recovered SELECT can be driven
	// by pgxmock.
	pool       defs.PgxPool
	raiser     defs.AlertRaiser
	ruleLoader ruleLoader
	interval   time.Duration
	logger     *slog.Logger
}

// NewCredentialStaleAlerts constructs the job. interval defaults to 1 hour.
func NewCredentialStaleAlerts(pool *pgxpool.Pool, raiser defs.AlertRaiser, alertStore ruleLoader, interval time.Duration, logger *slog.Logger) *CredentialStaleAlertsJob {
	if interval <= 0 {
		interval = time.Hour
	}
	return &CredentialStaleAlertsJob{
		pool:       pool,
		raiser:     raiser,
		ruleLoader: alertStore,
		interval:   interval,
		logger:     logger.With("job", credStaleJobID),
	}
}

func (j *CredentialStaleAlertsJob) ID() string              { return credStaleJobID }
func (j *CredentialStaleAlertsJob) Name() string            { return credStaleJobName }
func (j *CredentialStaleAlertsJob) Description() string     { return credStaleJobDescription }
func (j *CredentialStaleAlertsJob) Interval() time.Duration { return j.interval }

func (j *CredentialStaleAlertsJob) Run(ctx context.Context) error {
	rule, err := j.ruleLoader.GetRule(ctx, credStaleRuleID)
	if err != nil {
		if errors.Is(err, alerting.ErrNotFound) {
			j.logger.Warn("credential.stale_last_success rule not found in store, skipping")
			return nil
		}
		return fmt.Errorf("load rule: %w", err)
	}
	if rule == nil || !rule.Enabled {
		return nil
	}

	staleAfterDays := 7
	if raw, ok := rule.Params["staleAfterDays"]; ok {
		switch v := raw.(type) {
		case int:
			staleAfterDays = v
		case int64:
			staleAfterDays = int(v)
		case float64:
			staleAfterDays = int(v)
		}
	}
	if staleAfterDays <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -staleAfterDays)

	rows, err := j.pool.Query(ctx, `
		SELECT id, name, "lastSuccessAt", "lastUsedAt"
		FROM "Credential"
		WHERE enabled = true
		  AND ("lastSuccessAt" IS NULL OR "lastSuccessAt" < $1)
	`, cutoff)
	if err != nil {
		return fmt.Errorf("query stale credentials: %w", err)
	}
	defer rows.Close()

	type credRow struct {
		id            string
		name          string
		lastSuccessAt *time.Time
		lastUsedAt    *time.Time
	}
	var stale []credRow
	for rows.Next() {
		var r credRow
		if err := rows.Scan(&r.id, &r.name, &r.lastSuccessAt, &r.lastUsedAt); err != nil {
			return fmt.Errorf("scan credential row: %w", err)
		}
		stale = append(stale, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate credential rows: %w", err)
	}

	shouldFire := make(map[string]bool, len(stale))
	for _, c := range stale {
		shouldFire[c.id] = true
		details := map[string]any{
			"credentialId":   c.id,
			"credentialName": c.name,
			"staleAfterDays": staleAfterDays,
		}
		if c.lastSuccessAt != nil {
			details["lastSuccessAt"] = c.lastSuccessAt.Format(time.RFC3339)
		} else {
			details["lastSuccessAt"] = nil
		}
		if c.lastUsedAt != nil {
			details["lastUsedAt"] = c.lastUsedAt.Format(time.RFC3339)
		}
		msg := fmt.Sprintf("Credential %q has not had a successful use in %d days — investigate or disable", c.name, staleAfterDays)
		if err := j.raiser.Raise(ctx, alerting.RaiseInput{
			RuleID:      credStaleRuleID,
			TargetKey:   credStaleTargetPrefix + c.id,
			TargetLabel: c.name,
			Severity:    rule.DefaultSeverity,
			Message:     msg,
			Details:     details,
		}); err != nil {
			j.logger.Warn("raise credential stale alert failed", "credentialId", c.id, "error", err)
		}
	}

	if err := j.resolveRecovered(ctx, shouldFire); err != nil {
		j.logger.Warn("resolve recovered credential stale alerts failed", "error", err)
	}
	return nil
}

func (j *CredentialStaleAlertsJob) resolveRecovered(ctx context.Context, shouldFire map[string]bool) error {
	rows, err := j.pool.Query(ctx, `
		SELECT "targetKey"
		FROM "Alert"
		WHERE "ruleId" = $1
		  AND state IN ('FIRING'::"AlertState", 'ACKNOWLEDGED'::"AlertState")`,
		credStaleRuleID,
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
		credID := strings.TrimPrefix(targetKey, credStaleTargetPrefix)
		if credID == targetKey {
			continue
		}
		if shouldFire[credID] {
			continue
		}
		if err := j.raiser.Resolve(ctx, credStaleRuleID, targetKey, "used"); err != nil {
			j.logger.Warn("resolve credential stale alert failed", "targetKey", targetKey, "error", err)
		}
	}
	return nil
}
