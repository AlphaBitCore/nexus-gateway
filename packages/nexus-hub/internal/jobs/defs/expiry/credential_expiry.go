package expiry

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
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/store"
)

const (
	credExpiryJobID          = "credential-expiry"
	credExpiryJobName        = "Credential Expiry Check"
	credExpiryJobDescription = "Advances rotationState to 'pending_rotation' and raises credential.expiring alerts for credentials approaching their expiresAt. Raises CRITICAL alerts for overdue credentials (no auto-disable). Auto-resolves once a credential is no longer in the expiring set."

	// credExpiryWarnDays is the outer warn window. Credentials expiring within
	// this many days have rotationState promoted to pending_rotation.
	credExpiryWarnDays = 14

	credExpiryRuleID       = "credential.expiring"
	credExpiryTargetPrefix = "credential:"
)

// CredentialExpiryJob advances rotationState and raises alerts for credentials
// approaching or past their expiresAt. Per operator decision, credentials that
// have passed their expiry date are warned (CRITICAL) but NOT auto-disabled,
// because disabling a live credential without a confirmed replacement would
// break traffic.
//
// Alert lifecycle:
//
//   - On every run, raise an alert for each credential still in the warn window
//     or already past expiry. The Raiser dedups via (ruleId, targetKey).
//   - Auto-resolve any firing credential.expiring alert whose credential is
//     no longer in the combined expiring+overdue set — the operator added a
//     replacement and the old one was disabled, or expiresAt was pushed out.
type CredentialExpiryJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// quotastore helper calls + the resolve-recovered SELECT can be
	// driven by pgxmock without sharing the real Credential / Alert
	// tables.
	pool     defs.PgxPool
	raiser   defs.AlertRaiser
	interval time.Duration
	logger   *slog.Logger
}

// NewCredentialExpiry constructs the job. interval defaults to 1 hour.
func NewCredentialExpiry(pool *pgxpool.Pool, raiser defs.AlertRaiser, interval time.Duration, logger *slog.Logger) *CredentialExpiryJob {
	if interval <= 0 {
		interval = time.Hour
	}
	return &CredentialExpiryJob{
		pool:     pool,
		raiser:   raiser,
		interval: interval,
		logger:   logger.With("job", credExpiryJobID),
	}
}

func (j *CredentialExpiryJob) ID() string              { return credExpiryJobID }
func (j *CredentialExpiryJob) Name() string            { return credExpiryJobName }
func (j *CredentialExpiryJob) Description() string     { return credExpiryJobDescription }
func (j *CredentialExpiryJob) Interval() time.Duration { return j.interval }

func (j *CredentialExpiryJob) Run(ctx context.Context) error {
	var errs []error
	now := time.Now().UTC()
	firingSet := make(map[string]bool)

	// Step 1: advance state + raise alerts for credentials in the warn window.
	expiring, err := quotastore.ListExpiringCredentials(ctx, j.pool, credExpiryWarnDays)
	if err != nil {
		errs = append(errs, fmt.Errorf("list expiring credentials: %w", err))
	} else {
		// Promote rotationState for those still at 'none'.
		ids := make([]string, 0, len(expiring))
		for _, c := range expiring {
			ids = append(ids, c.ID)
		}
		if n, err := quotastore.MarkCredentialsPendingRotation(ctx, j.pool, ids); err != nil {
			errs = append(errs, fmt.Errorf("mark credentials pending rotation: %w", err))
		} else if n > 0 {
			j.logger.Info("promoted credentials to pending_rotation", "count", n)
		}

		for _, c := range expiring {
			firingSet[c.ID] = true
			daysLeft := daysUntil(now, c.ExpiresAt)
			sev := severityForCredExpiry(daysLeft)
			msg := fmt.Sprintf("Credential %q expires in %d day(s) — add a replacement and disable this key", c.Name, daysLeft)
			details := map[string]any{
				"credentialId": c.ID,
				"name":         c.Name,
				"providerId":   c.ProviderID,
				"expiresAt":    c.ExpiresAt.Format(time.RFC3339),
				"daysLeft":     daysLeft,
			}
			if err := j.raiser.Raise(ctx, alerting.RaiseInput{
				RuleID:      credExpiryRuleID,
				TargetKey:   credExpiryTargetPrefix + c.ID,
				TargetLabel: c.Name,
				Severity:    sev,
				Message:     msg,
				Details:     details,
			}); err != nil {
				errs = append(errs, fmt.Errorf("raise credential.expiring alert for %s: %w", c.ID, err))
			}
		}
		if len(expiring) > 0 {
			j.logger.Info("raised credential expiry alerts", "count", len(expiring))
		}
	}

	// Step 2: raise CRITICAL alerts for overdue credentials (past expiresAt).
	overdue, err := quotastore.ListOverdueCredentials(ctx, j.pool)
	if err != nil {
		errs = append(errs, fmt.Errorf("list overdue credentials: %w", err))
	} else {
		for _, c := range overdue {
			firingSet[c.ID] = true
			msg := fmt.Sprintf("Credential %q has passed its expiry date — replace immediately", c.Name)
			details := map[string]any{
				"credentialId": c.ID,
				"name":         c.Name,
				"providerId":   c.ProviderID,
				"expiresAt":    c.ExpiresAt.Format(time.RFC3339),
				"daysLeft":     0,
			}
			if err := j.raiser.Raise(ctx, alerting.RaiseInput{
				RuleID:      credExpiryRuleID,
				TargetKey:   credExpiryTargetPrefix + c.ID,
				TargetLabel: c.Name,
				Severity:    alerting.SeverityCritical,
				Message:     msg,
				Details:     details,
			}); err != nil {
				errs = append(errs, fmt.Errorf("raise overdue credential.expiring alert for %s: %w", c.ID, err))
			}
		}
		if len(overdue) > 0 {
			j.logger.Info("raised overdue credential alerts", "count", len(overdue))
		}
	}

	// Step 3: auto-resolve firing alerts for credentials no longer in the set.
	if err := j.resolveRecovered(ctx, firingSet); err != nil {
		errs = append(errs, fmt.Errorf("resolve recovered: %w", err))
	}

	return errors.Join(errs...)
}

func (j *CredentialExpiryJob) resolveRecovered(ctx context.Context, firingSet map[string]bool) error {
	rows, err := j.pool.Query(ctx, `
		SELECT "targetKey"
		FROM "Alert"
		WHERE "ruleId" = $1
		  AND state IN ('FIRING'::"AlertState", 'ACKNOWLEDGED'::"AlertState")`,
		credExpiryRuleID,
	)
	if err != nil {
		return fmt.Errorf("list firing credential.expiring alerts: %w", err)
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
		credID := strings.TrimPrefix(targetKey, credExpiryTargetPrefix)
		if credID == targetKey {
			continue
		}
		if firingSet[credID] {
			continue
		}
		if err := j.raiser.Resolve(ctx, credExpiryRuleID, targetKey, "rotated-or-extended"); err != nil {
			j.logger.Warn("resolve credential.expiring alert failed", "targetKey", targetKey, "err", err)
		}
	}
	return nil
}

// severityForCredExpiry maps urgency bands (matching the rule's warnDays
// config) to alert severities.
//
//	<= 1 day  → critical
//	<= 7 days → high
//	<= 14     → medium
func severityForCredExpiry(daysLeft int) alerting.Severity {
	switch {
	case daysLeft <= 1:
		return alerting.SeverityCritical
	case daysLeft <= 7:
		return alerting.SeverityHigh
	default:
		return alerting.SeverityMedium
	}
}
