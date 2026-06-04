package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/rollup"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/store"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

const (
	quotaAlertJobID          = "quota-alert-check"
	quotaAlertJobName        = "Quota Alert Check"
	quotaAlertJobDescription = "Evaluates current-month cost usage against QuotaOverride and QuotaPolicy cost limits every minute and raises quota.threshold alerts when usage crosses configured thresholds; auto-resolves with 2pp hysteresis once usage falls back."

	// quotaThresholdRuleID is the built-in rule ID registered in
	// alerting/rules.BuiltinRules. Changing this value breaks dedup continuity.
	quotaThresholdRuleID = "quota.threshold"

	// hysteresisPoints is the percent-points below the lowest threshold at
	// which an existing firing alert auto-resolves. Prevents rapid toggling
	// around a single threshold boundary.
	hysteresisPoints = 2.0
)

// defaultAlertThresholds is used when a policy has no alertThresholds or
// when an override has no matching policy.
var defaultAlertThresholds = []int{80, 95}

// QuotaAlertCheckJob evaluates quota usage against cost limits and drives
// quota.threshold alerts through the alerting.Raiser.
//
// Phase A walks every QuotaOverride row and raises per-override alerts.
// Phase B walks every enabled QuotaPolicy row and raises per-entity alerts
// for entities that have no override (overrides always take precedence).
// Phase C scans currently firing quota.threshold alerts and resolves any
// whose usage has dropped below (min threshold − hysteresisPoints), so the
// UI inbox does not keep stale "over budget" rows forever once the spend
// rolls off.
//
// Rollup cost data is cached per-dimension within a single run so the same
// dimension isn't re-queried across multiple overrides/policies.
type QuotaAlertCheckJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// VK / Project / Org cost SELECTs and rollup-cost lookups are
	// testable via pgxmock.
	pool     defs.PgxPool
	raiser   defs.AlertRaiser
	interval time.Duration
	logger   *slog.Logger
}

// NewQuotaAlertCheck constructs the job. interval defaults to 1 minute.
// raiser may be nil only in identity-style tests that never call Run.
func NewQuotaAlertCheck(pool *pgxpool.Pool, raiser defs.AlertRaiser, interval time.Duration, logger *slog.Logger) *QuotaAlertCheckJob {
	if interval <= 0 {
		interval = time.Minute
	}
	return &QuotaAlertCheckJob{
		pool:     pool,
		raiser:   raiser,
		interval: interval,
		logger:   logger.With("job", quotaAlertJobID),
	}
}

func (j *QuotaAlertCheckJob) ID() string              { return quotaAlertJobID }
func (j *QuotaAlertCheckJob) Name() string            { return quotaAlertJobName }
func (j *QuotaAlertCheckJob) Description() string     { return quotaAlertJobDescription }
func (j *QuotaAlertCheckJob) Interval() time.Duration { return j.interval }

// thresholdTarget is a lightweight descriptor of a target under evaluation,
// used by phase C's hysteresis check to recompute current usage without
// re-walking the full override/policy lists.
type thresholdTarget struct {
	// minThreshold is the lowest threshold configured for this target. An
	// alert below (minThreshold − hysteresisPoints) auto-resolves.
	minThreshold int
	// currentPct is the usage percentage observed this run. Alerts whose
	// targetKey matches but whose pct sits below the hysteresis floor are
	// resolved.
	currentPct float64
}

func (j *QuotaAlertCheckJob) Run(ctx context.Context) error {
	now := time.Now().UTC()
	periodKey := now.Format("2006-01")
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)

	overrides, err := quotastore.ListActiveQuotaOverrides(ctx, j.pool)
	if err != nil {
		return fmt.Errorf("list overrides: %w", err)
	}

	policies, err := quotastore.ListEnabledQuotaPolicies(ctx, j.pool)
	if err != nil {
		return fmt.Errorf("list policies: %w", err)
	}

	overrideTargetSet := make(map[string]bool, len(overrides))
	for _, o := range overrides {
		overrideTargetSet[o.TargetType+":"+o.TargetID] = true
	}

	rollupCache := make(map[string]map[string]float64)
	// evaluated holds every targetKey we inspected this run, for phase C's
	// resolve pass.
	evaluated := make(map[string]thresholdTarget)
	var errs []error
	var alertCount int

	// Phase A: override-based alerts.
	for _, o := range overrides {
		if o.CostLimitUsd == nil || *o.CostLimitUsd <= 0 {
			continue
		}
		dim, ok := scopeToDimension(o.TargetType)
		if !ok {
			continue
		}

		costs, err := j.loadRollupCosts(ctx, rollupCache, dim, periodStart, periodEnd)
		if err != nil {
			errs = append(errs, fmt.Errorf("rollup query dim=%s: %w", dim, err))
			continue
		}

		currentCost := costs[o.TargetID]
		costLimit := *o.CostLimitUsd
		pct := (currentCost / costLimit) * 100

		thresholds := findThresholdsForOverride(o, policies)
		targetKey := overrideTargetKey(o.ID, periodKey)
		evaluated[targetKey] = thresholdTarget{
			minThreshold: thresholds[0],
			currentPct:   pct,
		}
		alertCount += j.raiseForThresholds(ctx, raiseContext{
			targetKey:      targetKey,
			targetLabel:    o.TargetType + ":" + o.TargetID,
			targetType:     o.TargetType,
			targetID:       o.TargetID,
			overrideID:     o.ID,
			periodKey:      periodKey,
			thresholds:     thresholds,
			pct:            pct,
			costLimitUsd:   costLimit,
			currentCostUsd: currentCost,
			organizationID: "",
		}, &errs)
	}

	// Phase B: policy-based alerts for entities without overrides.
	for _, p := range policies {
		if p.CostLimitUsd == nil || *p.CostLimitUsd <= 0 {
			continue
		}
		dim, ok := scopeToDimension(p.Scope)
		if !ok {
			continue
		}

		costs, err := j.loadRollupCosts(ctx, rollupCache, dim, periodStart, periodEnd)
		if err != nil {
			errs = append(errs, fmt.Errorf("rollup query dim=%s: %w", dim, err))
			continue
		}

		thresholds := parseAlertThresholds(p.AlertThresholds)
		costLimit := *p.CostLimitUsd
		orgFilter := ""
		if p.OrganizationID != nil {
			orgFilter = *p.OrganizationID
		}

		for entityID, currentCost := range costs {
			targetType := dimensionToTargetType(dim)
			if overrideTargetSet[targetType+":"+entityID] {
				continue
			}

			if orgFilter != "" && dim == "organization" && entityID != orgFilter {
				continue
			}

			pct := (currentCost / costLimit) * 100
			targetKey := policyTargetKey(p.ID, targetType, entityID, periodKey)
			evaluated[targetKey] = thresholdTarget{
				minThreshold: thresholds[0],
				currentPct:   pct,
			}
			alertCount += j.raiseForThresholds(ctx, raiseContext{
				targetKey:      targetKey,
				targetLabel:    targetType + ":" + entityID,
				targetType:     targetType,
				targetID:       entityID,
				policyID:       p.ID,
				periodKey:      periodKey,
				thresholds:     thresholds,
				pct:            pct,
				costLimitUsd:   costLimit,
				currentCostUsd: currentCost,
				organizationID: orgFilter,
			}, &errs)
		}
	}

	// Phase C: auto-resolve stale firing alerts with 2pp hysteresis.
	if err := j.resolveStale(ctx, evaluated); err != nil {
		errs = append(errs, fmt.Errorf("resolve stale: %w", err))
	}

	if alertCount > 0 {
		j.logger.Info("alert check completed", "alerts", alertCount, "period", periodKey)
	}
	return errors.Join(errs...)
}

// overrideTargetKey returns the canonical targetKey used for override-based
// threshold alerts. Format: override:<id>|period:<YYYY-MM>.
func overrideTargetKey(overrideID, periodKey string) string {
	return "override:" + overrideID + "|period:" + periodKey
}

// policyTargetKey returns the canonical targetKey used for policy-based
// threshold alerts. Format: policy:<id>|entity:<type>:<id>|period:<YYYY-MM>.
func policyTargetKey(policyID, entityType, entityID, periodKey string) string {
	return "policy:" + policyID + "|entity:" + entityType + ":" + entityID + "|period:" + periodKey
}

// raiseContext bundles the per-target inputs used by raiseForThresholds so
// the call sites in Phase A/B remain readable.
type raiseContext struct {
	targetKey      string
	targetLabel    string
	targetType     string
	targetID       string
	overrideID     string
	policyID       string
	periodKey      string
	thresholds     []int
	pct            float64
	costLimitUsd   float64
	currentCostUsd float64
	organizationID string
}

// raiseForThresholds fires alerts for each threshold crossed and returns the
// count raised. Errors are accumulated into errs so a single bad target
// doesn't abort the rest of the run.
func (j *QuotaAlertCheckJob) raiseForThresholds(ctx context.Context, rc raiseContext, errs *[]error) int {
	var count int
	for _, threshold := range rc.thresholds {
		if rc.pct < float64(threshold) {
			continue
		}
		sev := severityForThreshold(threshold)
		details := map[string]any{
			"pct":            rc.pct,
			"threshold":      threshold,
			"costLimitUsd":   rc.costLimitUsd,
			"currentCostUsd": rc.currentCostUsd,
			"targetType":     rc.targetType,
			"targetId":       rc.targetID,
			"period":         rc.periodKey,
		}
		if rc.overrideID != "" {
			details["overrideId"] = rc.overrideID
		}
		if rc.policyID != "" {
			details["policyId"] = rc.policyID
		}
		if rc.organizationID != "" {
			details["organizationId"] = rc.organizationID
		}

		msg := fmt.Sprintf("Quota %d%% threshold crossed (%.1f%% of $%.2f)",
			threshold, rc.pct, rc.costLimitUsd)

		if err := j.raiser.Raise(ctx, alerting.RaiseInput{
			RuleID:      quotaThresholdRuleID,
			TargetKey:   rc.targetKey,
			TargetLabel: rc.targetLabel,
			Severity:    sev,
			Message:     msg,
			Details:     details,
		}); err != nil {
			*errs = append(*errs, fmt.Errorf("raise alert target=%s threshold=%d: %w", rc.targetKey, threshold, err))
			continue
		}
		count++
	}
	return count
}

// severityForThreshold maps a threshold percentage to an alert severity.
// Scheme (not overridable at producer time — the rule's DefaultSeverity
// is a separate concept used when no severity is supplied):
//
//	>= 95 → critical
//	>= 80 → high
//	else  → medium (custom low thresholds, e.g. 50%, still want visibility)
func severityForThreshold(threshold int) alerting.Severity {
	switch {
	case threshold >= 95:
		return alerting.SeverityCritical
	case threshold >= 80:
		return alerting.SeverityHigh
	default:
		return alerting.SeverityMedium
	}
}

// resolveStale walks currently firing quota.threshold alerts and resolves
// any whose latest observed pct has dropped below
// (minThreshold − hysteresisPoints). Targets that weren't evaluated this
// run (e.g. override deleted, policy disabled, entity no longer reporting
// usage) are also resolved — reason "target-removed".
func (j *QuotaAlertCheckJob) resolveStale(ctx context.Context, evaluated map[string]thresholdTarget) error {
	// Directly query the Alert table. ListAlerts on the alerting Store would
	// require threading the store down; for this bounded scan over a small
	// set of firing rows a targeted query is cleaner and has no circular
	// dependency concerns.
	rows, err := j.pool.Query(ctx, `
		SELECT "targetKey"
		FROM "Alert"
		WHERE "ruleId" = $1
		  AND state IN ('FIRING'::"AlertState", 'ACKNOWLEDGED'::"AlertState")`,
		quotaThresholdRuleID,
	)
	if err != nil {
		return fmt.Errorf("list firing threshold alerts: %w", err)
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
		tgt, seen := evaluated[targetKey]
		if !seen {
			// Target not evaluated this run — override/policy removed, or
			// cost data rolled out of window. Resolve so the inbox clears.
			if err := j.raiser.Resolve(ctx, quotaThresholdRuleID, targetKey, "target-removed"); err != nil {
				j.logger.Warn("resolve stale alert failed", "targetKey", targetKey, "err", err)
			}
			continue
		}
		floor := float64(tgt.minThreshold) - hysteresisPoints
		if tgt.currentPct < floor {
			if err := j.raiser.Resolve(ctx, quotaThresholdRuleID, targetKey, "auto"); err != nil {
				j.logger.Warn("resolve alert failed", "targetKey", targetKey, "err", err)
			}
		}
	}
	return nil
}

// loadRollupCosts returns a map of entityID → total cost for a dimension in
// the given time range. Results are cached per-dimension within the run.
func (j *QuotaAlertCheckJob) loadRollupCosts(
	ctx context.Context,
	cache map[string]map[string]float64,
	dim string,
	start, end time.Time,
) (map[string]float64, error) {
	if cached, ok := cache[dim]; ok {
		return cached, nil
	}

	// Read MetricBilledCostUSD (success-only, excludes cache hits) rather than
	// gross EstimatedCostUSD to prevent quota.threshold over-firing on
	// failed/cache-hit rows. EstimatedCostUSD is still emitted by rollup_5m
	// and remains available for analytics.
	rows, err := rollupstore.QueryRollup(ctx, j.pool, metrics.MetricsQuery{
		Metrics:      []string{metrics.MetricBilledCostUSD},
		DimensionKey: dim,
		StartTime:    start,
		EndTime:      end,
	})
	if err != nil {
		return nil, err
	}

	costs := make(map[string]float64)
	prefix := dim + "="
	for _, r := range rows {
		if !strings.HasPrefix(r.DimensionKey, prefix) {
			continue
		}
		entityID := strings.TrimPrefix(r.DimensionKey, prefix)
		costs[entityID] += r.Value
	}

	cache[dim] = costs
	return costs, nil
}

// parseAlertThresholds deserializes the JSON alertThresholds from a
// QuotaPolicy. Returns defaultAlertThresholds on failure or empty input.
func parseAlertThresholds(raw json.RawMessage) []int {
	if len(raw) == 0 {
		return defaultAlertThresholds
	}
	var thresholds []int
	if err := json.Unmarshal(raw, &thresholds); err != nil || len(thresholds) == 0 {
		return defaultAlertThresholds
	}
	sort.Ints(thresholds)
	return thresholds
}

// findThresholdsForOverride finds the best-matching policy for an override
// target and returns its alertThresholds. Falls back to defaultAlertThresholds.
func findThresholdsForOverride(o quotastore.QuotaOverride, policies []quotastore.QuotaPolicy) []int {
	scope := o.TargetType
	for _, p := range policies {
		if p.Scope != scope {
			continue
		}
		return parseAlertThresholds(p.AlertThresholds)
	}
	return defaultAlertThresholds
}

// scopeToDimension maps a scope/targetType to the rollup dimension prefix.
func scopeToDimension(scope string) (string, bool) {
	m := map[string]string{
		"user":         "user",
		"vk":           "virtual_key",
		"virtual_key":  "virtual_key",
		"project":      "organization",
		"organization": "organization",
	}
	dim, ok := m[scope]
	return dim, ok
}

// dimensionToTargetType maps a rollup dimension back to the canonical
// target type stored in alert targetKeys.
func dimensionToTargetType(dim string) string {
	m := map[string]string{
		"user":         "user",
		"virtual_key":  "vk",
		"organization": "organization",
	}
	if t, ok := m[dim]; ok {
		return t
	}
	return dim
}
