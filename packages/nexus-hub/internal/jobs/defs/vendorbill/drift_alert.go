package vendorbill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
)

const (
	driftJobID   = "vendor-bill-drift-alerts"
	driftJobName = "Vendor Bill Drift Alerts"
	driftJobDesc = "Raises vendor.bill_drift alerts for scoped provider×day reconciliation rows whose drift exceeds BOTH the percentage and absolute-dollar floors; auto-resolves rows that no longer breach (e.g. after a late vendor revision)."

	driftRuleID     = "vendor.bill_drift"
	driftTargetPfx  = "vendor-bill:"
	driftResolveMsg = "within tolerance"
	// driftScanDays bounds the poll to the recent window the reconcile job
	// keeps fresh; older rows are already settled.
	driftScanDays = 7
)

// ruleLoader is the narrow subset of *alerting.Store this job needs.
type ruleLoader interface {
	GetRule(ctx context.Context, id string) (*alerting.AlertRule, error)
}

// VendorBillDriftAlertsJob is a class-1 state-poll alert job (like
// ProviderUnavailableAlertsJob): it reads settled reconciliation state and
// raises/resolves via the Raiser — it is NOT an Engine stream aggregator,
// because drift is a daily table comparison, not a live event stream.
type VendorBillDriftAlertsJob struct {
	pool       defs.PgxPool
	raiser     defs.AlertRaiser
	ruleLoader ruleLoader
	interval   time.Duration
	logger     *slog.Logger
	now        func() time.Time
}

// NewVendorBillDriftAlerts constructs the job. interval defaults to 24h.
func NewVendorBillDriftAlerts(pool *pgxpool.Pool, raiser defs.AlertRaiser, ruleStore ruleLoader, interval time.Duration, logger *slog.Logger) *VendorBillDriftAlertsJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &VendorBillDriftAlertsJob{
		pool:       pool,
		raiser:     raiser,
		ruleLoader: ruleStore,
		interval:   interval,
		logger:     logger.With("job", driftJobID),
		now:        func() time.Time { return time.Now().UTC() },
	}
}

func (j *VendorBillDriftAlertsJob) ID() string              { return driftJobID }
func (j *VendorBillDriftAlertsJob) Name() string            { return driftJobName }
func (j *VendorBillDriftAlertsJob) Description() string     { return driftJobDesc }
func (j *VendorBillDriftAlertsJob) Interval() time.Duration { return j.interval }
func (j *VendorBillDriftAlertsJob) RunOnStart() bool        { return true }

func floatParam(params map[string]any, key string, def float64) float64 {
	if params == nil {
		return def
	}
	if v, ok := params[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return def
}

func (j *VendorBillDriftAlertsJob) Run(ctx context.Context) error {
	rule, err := j.ruleLoader.GetRule(ctx, driftRuleID)
	if err != nil {
		if errors.Is(err, alerting.ErrNotFound) {
			j.logger.Warn("vendor.bill_drift rule not found, skipping")
			return nil
		}
		return fmt.Errorf("load rule: %w", err)
	}
	if rule == nil || !rule.Enabled {
		return nil
	}

	thresholdPctFrac := floatParam(rule.Params, "thresholdPct", 5.0) / 100.0
	thresholdUsd := floatParam(rule.Params, "thresholdUsd", 1.0)
	since := j.now().Truncate(24*time.Hour).AddDate(0, 0, -driftScanDays)

	// Only `scoped` rows are alert-eligible; org_only / fetch_failed /
	// priority_tier_undercount rows are display-only and filtered out here.
	rows, err := j.pool.Query(ctx, `
		SELECT r.provider_id, r.day, r.our_billed_usd, r.vendor_reported_usd, r.diff_usd, r.diff_pct,
		       COALESCE(p."displayName", p.name, r.provider_id) AS display_name
		FROM vendor_bill_reconciliation r
		LEFT JOIN "Provider" p ON p.id = r.provider_id
		WHERE r.coverage = 'scoped' AND r.diff_usd IS NOT NULL AND r.day >= $1
	`, since)
	if err != nil {
		return fmt.Errorf("query scoped rows: %w", err)
	}
	defer rows.Close()

	type driftRow struct {
		providerID  string
		day         time.Time
		our         float64
		vendor      float64
		diffUSD     float64
		diffPct     float64
		displayName string
	}
	var candidates []driftRow
	for rows.Next() {
		var r driftRow
		if err := rows.Scan(&r.providerID, &r.day, &r.our, &r.vendor, &r.diffUSD, &r.diffPct, &r.displayName); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	var errs []error
	for _, r := range candidates {
		targetKey := driftTargetPfx + r.providerID + ":" + r.day.Format("2006-01-02")
		breach := math.Abs(r.diffPct) > thresholdPctFrac && math.Abs(r.diffUSD) > thresholdUsd
		if !breach {
			// Self-heal: a row that once breached but no longer does (e.g. a
			// late vendor revision) gets resolved. Idempotent when not firing.
			if err := j.raiser.Resolve(ctx, driftRuleID, targetKey, driftResolveMsg); err != nil {
				errs = append(errs, fmt.Errorf("resolve %s: %w", targetKey, err))
			}
			continue
		}
		msg := fmt.Sprintf("Vendor bill drift for %s on %s: vendor $%.2f vs ours $%.2f (%.1f%%)",
			r.displayName, r.day.Format("2006-01-02"), r.vendor, r.our, r.diffPct*100)
		if err := j.raiser.Raise(ctx, alerting.RaiseInput{
			RuleID:      driftRuleID,
			TargetKey:   targetKey,
			TargetLabel: r.displayName,
			Severity:    rule.DefaultSeverity,
			Message:     msg,
			Details: map[string]any{
				"providerId":        r.providerID,
				"day":               r.day.Format("2006-01-02"),
				"ourBilledUsd":      r.our,
				"vendorReportedUsd": r.vendor,
				"diffUsd":           r.diffUSD,
				"diffPct":           r.diffPct,
			},
		}); err != nil {
			errs = append(errs, fmt.Errorf("raise %s: %w", targetKey, err))
		}
	}
	return errors.Join(errs...)
}
