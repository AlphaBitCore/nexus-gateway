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
	vkExpiryJobID          = "vk-expiry"
	vkExpiryJobName        = "Virtual Key Expiry"
	vkExpiryJobDescription = "Expires virtual keys past their expiry date and raises quota.vk_expiring alerts for keys expiring within the warn window; auto-resolves once a key has been renewed or is no longer within the window."

	// vkExpiryAlertWindowDays is the outer warn-window. Keys expiring within
	// this many days trigger an alert. Urgency bands within the window
	// (30/15/7/1 days) control severity — see severityForVKExpiry.
	vkExpiryAlertWindowDays = 30

	// vkExpiringRuleID is the built-in rule registered in
	// alerting/rules.BuiltinRules. Must match the seed; changing it breaks
	// dedup continuity for outstanding expiry alerts.
	vkExpiringRuleID = "quota.vk_expiring"

	// vkTargetKeyPrefix is the leading portion of a vk_expiring alert's
	// targetKey. Consumers split on it to recover the VirtualKey ID during
	// the auto-resolve pass.
	vkTargetKeyPrefix = "vk:"
)

// VKExpiryJob expires overdue virtual keys in the database and raises
// vk_expiring alerts for keys within the warn window. Ai-gateway picks up
// expired keys on its next DB read; there is no inline cache invalidation
// signal because virtual_keys is looked up per-request.
//
// Alert lifecycle:
//
//   - Raise on every run for each VK still within the warn window. The
//     Raiser dedups via its (ruleId, targetKey) primary key.
//   - Resolve any previously-firing vk_expiring alert whose VK is no longer
//     in the expiring set — either the operator renewed it (expiresAt
//     pushed out beyond the window) or it tipped past expiry and was moved
//     to vkStatus='expired' by step 1 above.
type VKExpiryJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// quotastore helper calls + the resolve-renewed SELECT can be driven
	// by pgxmock.
	pool     defs.PgxPool
	raiser   defs.AlertRaiser
	interval time.Duration
	logger   *slog.Logger
}

// NewVKExpiry constructs the job. interval defaults to 1 hour.
// raiser may be nil only in identity-style tests that never call Run.
func NewVKExpiry(pool *pgxpool.Pool, raiser defs.AlertRaiser, interval time.Duration, logger *slog.Logger) *VKExpiryJob {
	if interval <= 0 {
		interval = time.Hour
	}
	return &VKExpiryJob{
		pool:     pool,
		raiser:   raiser,
		interval: interval,
		logger:   logger.With("job", vkExpiryJobID),
	}
}

func (j *VKExpiryJob) ID() string              { return vkExpiryJobID }
func (j *VKExpiryJob) Name() string            { return vkExpiryJobName }
func (j *VKExpiryJob) Description() string     { return vkExpiryJobDescription }
func (j *VKExpiryJob) Interval() time.Duration { return j.interval }

func (j *VKExpiryJob) Run(ctx context.Context) error {
	var errs []error

	// Step 1: expire overdue keys.
	count, err := quotastore.ExpireOverdueVirtualKeys(ctx, j.pool)
	if err != nil {
		errs = append(errs, fmt.Errorf("expire overdue keys: %w", err))
	} else if count > 0 {
		j.logger.Info("expired overdue keys", "count", count)
	}

	// Step 2: raise alerts for keys expiring within the warn window.
	expiring, err := quotastore.ListExpiringVirtualKeys(ctx, j.pool, vkExpiryAlertWindowDays)
	if err != nil {
		errs = append(errs, fmt.Errorf("list expiring keys: %w", err))
		return errors.Join(errs...)
	}

	now := time.Now().UTC()
	expiringSet := make(map[string]bool, len(expiring))
	var alertCount int

	for _, vk := range expiring {
		expiringSet[vk.ID] = true
		daysLeft := daysUntil(now, vk.ExpiresAt)
		sev := severityForVKExpiry(daysLeft)
		details := map[string]any{
			"vkId":      vk.ID,
			"name":      vk.Name,
			"expiresAt": vk.ExpiresAt.Format(time.RFC3339),
			"daysLeft":  daysLeft,
		}
		msg := fmt.Sprintf("Virtual key %q expires in %d day(s)", vk.Name, daysLeft)

		if err := j.raiser.Raise(ctx, alerting.RaiseInput{
			RuleID:      vkExpiringRuleID,
			TargetKey:   vkTargetKeyPrefix + vk.ID,
			TargetLabel: vk.Name,
			Severity:    sev,
			Message:     msg,
			Details:     details,
		}); err != nil {
			errs = append(errs, fmt.Errorf("raise vk_expiring alert for %s: %w", vk.ID, err))
			continue
		}
		alertCount++
	}

	// Step 3: auto-resolve any firing vk_expiring alerts whose VK is no
	// longer in the expiring set (renewed or already-expired).
	if err := j.resolveRenewed(ctx, expiringSet); err != nil {
		errs = append(errs, fmt.Errorf("resolve renewed: %w", err))
	}

	if alertCount > 0 {
		j.logger.Info("raised vk expiry alerts", "count", alertCount)
	}
	return errors.Join(errs...)
}

// resolveRenewed walks currently firing vk_expiring alerts and resolves any
// whose underlying VirtualKey is no longer in the expiring set — either the
// key was renewed (push expiresAt out) or it transitioned to vkStatus=expired
// and no longer returns from ListExpiringVirtualKeys.
func (j *VKExpiryJob) resolveRenewed(ctx context.Context, expiringSet map[string]bool) error {
	rows, err := j.pool.Query(ctx, `
		SELECT "targetKey"
		FROM "Alert"
		WHERE "ruleId" = $1
		  AND state IN ('FIRING'::"AlertState", 'ACKNOWLEDGED'::"AlertState")`,
		vkExpiringRuleID,
	)
	if err != nil {
		return fmt.Errorf("list firing vk_expiring alerts: %w", err)
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
		vkID := strings.TrimPrefix(targetKey, vkTargetKeyPrefix)
		if vkID == targetKey {
			// Not in the vk:<id> format — unrelated alert for this rule,
			// skip defensively.
			continue
		}
		if expiringSet[vkID] {
			continue
		}
		if err := j.raiser.Resolve(ctx, vkExpiringRuleID, targetKey, "renewed-or-expired"); err != nil {
			j.logger.Warn("resolve vk_expiring alert failed", "targetKey", targetKey, "err", err)
		}
	}
	return nil
}

// daysUntil returns the whole-day count from now to expiresAt. Partial days
// round up so a key 36 hours away reports "2 days" (matches operator mental
// model of "about two days left").
func daysUntil(now, expiresAt time.Time) int {
	d := expiresAt.Sub(now)
	if d <= 0 {
		return 0
	}
	return int((d + 24*time.Hour - time.Nanosecond) / (24 * time.Hour))
}

// severityForVKExpiry maps the warn-day urgency bands (matching the rule
// seed's warnDays=[30,15,7,1]) to alert severities.
//
//	<= 1 day  → critical
//	<= 7 days → high
//	<= 15     → medium
//	<= 30     → low  (within outer window but far out)
func severityForVKExpiry(daysLeft int) alerting.Severity {
	switch {
	case daysLeft <= 1:
		return alerting.SeverityCritical
	case daysLeft <= 7:
		return alerting.SeverityHigh
	case daysLeft <= 15:
		return alerting.SeverityMedium
	default:
		return alerting.SeverityLow
	}
}
