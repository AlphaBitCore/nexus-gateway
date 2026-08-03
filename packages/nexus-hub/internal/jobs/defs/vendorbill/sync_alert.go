package vendorbill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
)

const (
	syncJobID   = "vendor-bill-sync-alerts"
	syncJobName = "Vendor Bill Sync Alerts"
	syncJobDesc = "Raises vendor.bill_sync_failed when a provider's vendor cost-API sync stays fetch_failed past a full run cycle (persistent breakage: revoked key / lost scope / blocked egress); auto-resolves on recovery."

	syncRuleID     = "vendor.bill_sync_failed"
	syncTargetPfx  = "vendor-bill-sync:"
	syncResolveMsg = "vendor cost-API sync recovered"
	// syncScanDays bounds the poll to the recent window the reconcile job keeps
	// fresh; older rows are already settled.
	syncScanDays = 7
	// defaultStaleHours: a fetch_failed row must remain unhealed for longer than
	// this (one 24h reconcile cycle + margin) before it counts as a persistent
	// sync failure. A one-off transient fetch error is overwritten by the next
	// successful run's DO UPDATE well within this window, so it never ages into
	// an alert.
	defaultStaleHours = 25.0
)

// VendorBillSyncAlertsJob is a class-1 state-poll alert job (sibling of
// VendorBillDriftAlertsJob): it raises vendor.bill_sync_failed for a provider
// whose vendor cost-API sync has been failing (fetch_failed) past a full run
// cycle, and resolves it once the sync recovers. It is NOT an Engine stream
// aggregator — sync health is a daily table poll, not a live event stream.
//
// This closes the blind spot the drift alert deliberately leaves: drift fires
// only on `scoped` rows, treating `fetch_failed` as display-only, so a silently
// broken sync (revoked key / lost scope / blocked egress) raises nothing.
type VendorBillSyncAlertsJob struct {
	pool       defs.PgxPool
	raiser     defs.AlertRaiser
	ruleLoader ruleLoader
	interval   time.Duration
	logger     *slog.Logger
	now        func() time.Time
}

// NewVendorBillSyncAlerts constructs the job. interval defaults to 24h.
func NewVendorBillSyncAlerts(pool *pgxpool.Pool, raiser defs.AlertRaiser, ruleStore ruleLoader, interval time.Duration, logger *slog.Logger) *VendorBillSyncAlertsJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &VendorBillSyncAlertsJob{
		pool:       pool,
		raiser:     raiser,
		ruleLoader: ruleStore,
		interval:   interval,
		logger:     logger.With("job", syncJobID),
		now:        func() time.Time { return time.Now().UTC() },
	}
}

func (j *VendorBillSyncAlertsJob) ID() string              { return syncJobID }
func (j *VendorBillSyncAlertsJob) Name() string            { return syncJobName }
func (j *VendorBillSyncAlertsJob) Description() string     { return syncJobDesc }
func (j *VendorBillSyncAlertsJob) Interval() time.Duration { return j.interval }
func (j *VendorBillSyncAlertsJob) RunOnStart() bool        { return true }

// syncCandidate is one provider's rolled-up sync-health state, collected before
// any Raise/Resolve so the result set is closed before the raiser touches the
// pool (single-conn safety, same reason as the drift job).
type syncCandidate struct {
	providerID  string
	displayName string
	staleFailed int
	oldestDay   string // "YYYY-MM-DD" of the oldest stale day, "" when none
}

func (j *VendorBillSyncAlertsJob) Run(ctx context.Context) error {
	rule, err := j.ruleLoader.GetRule(ctx, syncRuleID)
	if err != nil {
		if errors.Is(err, alerting.ErrNotFound) {
			j.logger.Warn("vendor.bill_sync_failed rule not found, skipping")
			return nil
		}
		return fmt.Errorf("load rule: %w", err)
	}
	if rule == nil || !rule.Enabled {
		return nil
	}

	staleHours := floatParam(rule.Params, "staleHours", defaultStaleHours)
	staleCutoff := j.now().Add(-time.Duration(staleHours * float64(time.Hour)))
	scanSince := j.now().Truncate(24*time.Hour).AddDate(0, 0, -syncScanDays)

	// Per provider over the recent window: how many fetch_failed days have gone
	// unhealed past the stale cutoff, and the oldest such day (as YYYY-MM-DD,
	// COALESCEd to '' when none — MIN is NULL exactly when the count is 0, over
	// the same filter). stale_failed drives the raise/resolve decision.
	rows, err := j.pool.Query(ctx, `
		SELECT r.provider_id,
		       COALESCE(p."displayName", p.name, r.provider_id) AS display_name,
		       COUNT(*) FILTER (WHERE r.coverage = 'fetch_failed' AND r.updated_at < $1) AS stale_failed,
		       COALESCE(MIN(r.day) FILTER (WHERE r.coverage = 'fetch_failed' AND r.updated_at < $1)::text, '') AS oldest_failed_day
		FROM vendor_bill_reconciliation r
		LEFT JOIN "Provider" p ON p.id = r.provider_id
		WHERE r.day >= $2
		GROUP BY r.provider_id, p."displayName", p.name
	`, staleCutoff, scanSince)
	if err != nil {
		return fmt.Errorf("query sync health: %w", err)
	}
	defer rows.Close()

	var candidates []syncCandidate
	for rows.Next() {
		var c syncCandidate
		if err := rows.Scan(&c.providerID, &c.displayName, &c.staleFailed, &c.oldestDay); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	var errs []error
	for _, c := range candidates {
		targetKey := syncTargetPfx + c.providerID
		if c.staleFailed == 0 {
			// Healthy, or only a fresh transient fetch_failed the stale filter
			// excluded. Idempotent when not currently firing.
			if err := j.raiser.Resolve(ctx, syncRuleID, targetKey, syncResolveMsg); err != nil {
				errs = append(errs, fmt.Errorf("resolve %s: %w", targetKey, err))
			}
			continue
		}
		msg := fmt.Sprintf("Vendor cost-API sync failing for %s: %d day(s) unfetched and unhealed (oldest %s)",
			c.displayName, c.staleFailed, c.oldestDay)
		if err := j.raiser.Raise(ctx, alerting.RaiseInput{
			RuleID:      syncRuleID,
			TargetKey:   targetKey,
			TargetLabel: c.displayName,
			Severity:    rule.DefaultSeverity,
			Message:     msg,
			Details: map[string]any{
				"providerId":      c.providerID,
				"oldestFailedDay": c.oldestDay,
				"failedDayCount":  c.staleFailed,
			},
		}); err != nil {
			errs = append(errs, fmt.Errorf("raise %s: %w", targetKey, err))
		}
	}
	return errors.Join(errs...)
}
