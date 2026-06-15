package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
)

const (
	providerUnavailableJobID          = "provider-unavailable-alerts"
	providerUnavailableJobName        = "Provider Unavailable Alerts"
	providerUnavailableJobDescription = "Raises provider.unavailable alerts for Providers whose ProviderHealth status is 'unavailable'; auto-resolves firing alerts when the provider status transitions to healthy or degraded."

	providerUnavailableRuleID = "provider.unavailable"
	providerTargetKeyPrefix   = "provider:"
	providerResolveReason     = "recovered"
)

// ProviderUnavailableAlertsJob raises provider.unavailable alerts for any
// Provider whose ProviderHealth.status is 'unavailable'. It auto-resolves
// firing alerts when the provider's status transitions away from 'unavailable'.
//
// Alert lifecycle:
//
//   - Raise on every run for each provider with status='unavailable'.
//     alerting.Raiser dedups via (ruleId, targetKey) advisory lock.
//   - Resolve any previously-firing provider.unavailable alert whose provider
//     has transitioned to status IN ('healthy', 'degraded').
//
// Debounce: rule params minDownSec / recoverySec are applied by the gateway.
// ai-gateway's underlying rolling window already
// debounces "unavailable" itself, so this layer is the second filter — the
// admin gets to decide how long Hub waits beyond ai-gateway's threshold
// before firing / resolving. minDownSec=0 / recoverySec=0 disables the
// extra wait.
type ProviderUnavailableAlertsJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// ProviderHealth SELECT + resolve-recovered SELECT are testable via
	// pgxmock.
	pool       defs.PgxPool
	raiser     defs.AlertRaiser
	ruleLoader ruleLoader
	interval   time.Duration
	logger     *slog.Logger

	// Debounce state — provider IDs to first-observed timestamps. Cleared
	// when the provider transitions out of the corresponding state.
	mu               sync.Mutex
	unavailableSince map[string]time.Time
	recoveredSince   map[string]time.Time
}

// NewProviderUnavailableAlerts constructs the job. interval defaults to 60s.
func NewProviderUnavailableAlerts(
	pool *pgxpool.Pool,
	raiser defs.AlertRaiser,
	alertStore ruleLoader,
	interval time.Duration,
	logger *slog.Logger,
) *ProviderUnavailableAlertsJob {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &ProviderUnavailableAlertsJob{
		pool:             pool,
		raiser:           raiser,
		ruleLoader:       alertStore,
		interval:         interval,
		logger:           logger.With("job", providerUnavailableJobID),
		unavailableSince: make(map[string]time.Time),
		recoveredSince:   make(map[string]time.Time),
	}
}

func (j *ProviderUnavailableAlertsJob) ID() string              { return providerUnavailableJobID }
func (j *ProviderUnavailableAlertsJob) Name() string            { return providerUnavailableJobName }
func (j *ProviderUnavailableAlertsJob) Description() string     { return providerUnavailableJobDescription }
func (j *ProviderUnavailableAlertsJob) Interval() time.Duration { return j.interval }

func (j *ProviderUnavailableAlertsJob) Run(ctx context.Context) error {
	rule, err := j.ruleLoader.GetRule(ctx, providerUnavailableRuleID)
	if err != nil {
		if errors.Is(err, alerting.ErrNotFound) {
			j.logger.Warn("provider.unavailable rule not found in store, skipping")
			return nil
		}
		return fmt.Errorf("load rule: %w", err)
	}
	if rule == nil || !rule.Enabled {
		return nil
	}

	minDownSec, recoverySec, err := parseProviderUnavailableParams(rule.Params)
	if err != nil {
		return fmt.Errorf("parse rule params: %w", err)
	}
	now := time.Now().UTC()

	rows, err := j.pool.Query(ctx, `
		SELECT ph."providerId", ph.provider,
		       COALESCE(p."displayName", p.name, ph.provider) AS display_name,
		       ph."rollingErrorRate", ph."lastErrorAt", ph."updatedAt"
		FROM "ProviderHealth" ph
		LEFT JOIN "Provider" p ON p.id = ph."providerId"
		WHERE ph.status = 'unavailable'
	`)
	if err != nil {
		return fmt.Errorf("query unavailable providers: %w", err)
	}
	defer rows.Close()

	type providerRow struct {
		providerID     string
		providerName   string
		displayName    string
		rollingErrRate float64
		lastErrorAt    *time.Time
		updatedAt      time.Time
	}

	var unavailable []providerRow
	for rows.Next() {
		var r providerRow
		if err := rows.Scan(&r.providerID, &r.providerName, &r.displayName,
			&r.rollingErrRate, &r.lastErrorAt, &r.updatedAt); err != nil {
			return fmt.Errorf("scan provider row: %w", err)
		}
		unavailable = append(unavailable, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate provider rows: %w", err)
	}

	shouldFireSet := make(map[string]bool, len(unavailable))
	var errs []error
	var raiseCount int

	for _, p := range unavailable {
		targetKey := providerTargetKeyPrefix + p.providerID
		shouldFireSet[p.providerID] = true

		// Provider is currently unavailable: clear any in-flight recovery
		// timer; record (or keep) the unavailable-since timestamp.
		j.mu.Lock()
		delete(j.recoveredSince, p.providerID)
		firstSeen, hadEarlier := j.unavailableSince[p.providerID]
		if !hadEarlier {
			j.unavailableSince[p.providerID] = now
			firstSeen = now
		}
		j.mu.Unlock()

		// Skip the raise until at least minDownSec has elapsed since first
		// observation. Prior raises (from before minDownSec was met) are
		// idempotent because raiser.Raise dedups via (rule, target) advisory
		// lock; we just defer the first call.
		if minDownSec > 0 && now.Sub(firstSeen).Seconds() < minDownSec {
			continue
		}

		details := map[string]any{
			"providerId":   p.providerID,
			"providerName": p.providerName,
			"displayName":  p.displayName,
			"errorRate":    p.rollingErrRate,
			"downSec":      int(now.Sub(firstSeen).Seconds()),
		}
		if p.lastErrorAt != nil {
			details["lastErrorAt"] = p.lastErrorAt.Format(time.RFC3339)
		}

		msg := fmt.Sprintf("Provider %q is unavailable (error rate %.1f%%)", p.displayName, p.rollingErrRate*100)

		if err := j.raiser.Raise(ctx, alerting.RaiseInput{
			RuleID:      providerUnavailableRuleID,
			TargetKey:   targetKey,
			TargetLabel: p.displayName,
			Severity:    rule.DefaultSeverity,
			Message:     msg,
			Details:     details,
		}); err != nil {
			errs = append(errs, fmt.Errorf("raise provider.unavailable for %s: %w", p.providerID, err))
			continue
		}
		raiseCount++
	}

	if err := j.resolveRecovered(ctx, shouldFireSet, recoverySec, now); err != nil {
		errs = append(errs, fmt.Errorf("resolve recovered: %w", err))
	}

	if raiseCount > 0 {
		j.logger.Info("raised provider.unavailable alerts", "count", raiseCount)
	}
	return errors.Join(errs...)
}

// resolveRecovered walks currently firing provider.unavailable alerts and
// resolves any whose provider is no longer in the shouldFireSet — meaning
// the provider has transitioned to healthy or degraded — AND
// has stayed recovered for at least recoverySec.
func (j *ProviderUnavailableAlertsJob) resolveRecovered(ctx context.Context, shouldFireSet map[string]bool, recoverySec float64, now time.Time) error {
	rows, err := j.pool.Query(ctx, `
		SELECT "targetKey"
		FROM "Alert"
		WHERE "ruleId" = $1
		  AND state IN ('FIRING'::"AlertState", 'ACKNOWLEDGED'::"AlertState")`,
		providerUnavailableRuleID,
	)
	if err != nil {
		return fmt.Errorf("list firing provider.unavailable alerts: %w", err)
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
		providerID := strings.TrimPrefix(targetKey, providerTargetKeyPrefix)
		if providerID == targetKey {
			// Not in provider:<id> format — defensive skip.
			continue
		}
		if shouldFireSet[providerID] {
			// Still unavailable — drop any half-recorded recovery timer.
			j.mu.Lock()
			delete(j.recoveredSince, providerID)
			j.mu.Unlock()
			continue
		}

		// Provider is no longer unavailable. Clear the unavailable-since
		// timestamp and start tracking recovery time.
		j.mu.Lock()
		delete(j.unavailableSince, providerID)
		recStart, hadEarlier := j.recoveredSince[providerID]
		if !hadEarlier {
			j.recoveredSince[providerID] = now
			recStart = now
		}
		j.mu.Unlock()

		if recoverySec > 0 && now.Sub(recStart).Seconds() < recoverySec {
			// Recovery hasn't held long enough; defer the resolve.
			continue
		}

		if err := j.raiser.Resolve(ctx, providerUnavailableRuleID, targetKey, providerResolveReason); err != nil {
			j.logger.Warn("resolve provider.unavailable alert failed", "targetKey", targetKey, "err", err)
			continue
		}
		// Successful resolve — clear the recovery timer too.
		j.mu.Lock()
		delete(j.recoveredSince, providerID)
		j.mu.Unlock()
	}
	return nil
}

// parseProviderUnavailableParams extracts minDownSec and recoverySec from the
// rule's Params map. JSON numbers unmarshal as float64; both are returned as
// such. These values are logged for operator visibility but not applied
// programmatically (see decision log §17).
func parseProviderUnavailableParams(params map[string]any) (float64, float64, error) {
	rawMin, ok := params["minDownSec"]
	if !ok {
		return 0, 0, fmt.Errorf("minDownSec missing from rule params")
	}
	minDownSec, ok := rawMin.(float64)
	if !ok {
		return 0, 0, fmt.Errorf("minDownSec must be a number, got %T", rawMin)
	}
	if minDownSec < 0 {
		return 0, 0, fmt.Errorf("minDownSec must be >= 0, got %v", minDownSec)
	}

	rawRec, ok := params["recoverySec"]
	if !ok {
		return 0, 0, fmt.Errorf("recoverySec missing from rule params")
	}
	recoverySec, ok := rawRec.(float64)
	if !ok {
		return 0, 0, fmt.Errorf("recoverySec must be a number, got %T", rawRec)
	}
	if recoverySec < 0 {
		return 0, 0, fmt.Errorf("recoverySec must be >= 0, got %v", recoverySec)
	}

	return minDownSec, recoverySec, nil
}
