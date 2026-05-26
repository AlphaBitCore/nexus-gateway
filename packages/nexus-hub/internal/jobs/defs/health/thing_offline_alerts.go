package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

const (
	thingOfflineJobID          = "thing-offline-alerts"
	thingOfflineJobName        = "Thing Offline Alerts"
	thingOfflineJobDescription = "Raises thing.offline alerts for Things whose last_seen_at has exceeded the rule's offlineAfterSec threshold; auto-resolves firing alerts for Things that have come back fresh or have been deleted."

	thingOfflineRuleID        = "thing.offline"
	thingTargetKeyPrefix      = "thing:"
	thingOfflineResolveReason = "back-online"
)

// ruleLoader is the narrow subset of *alerting.Store used by ThingOfflineAlertsJob.
// The interface keeps tests free of the full Store construction stack.
type ruleLoader interface {
	GetRule(ctx context.Context, id string) (*alerting.AlertRule, error)
}

// ThingOfflineAlertsJob raises thing.offline alerts for Things that have not
// been seen within the rule's offlineAfterSec window. It auto-resolves firing
// alerts for Things that have come back online (last_seen_at within threshold)
// or that no longer exist.
//
// Alert lifecycle:
//
//   - Raise on every run for each Thing whose last_seen_at is older than
//     offlineAfterSec. alerting.Raiser dedups via (ruleId, targetKey).
//   - Resolve any previously-firing thing.offline alert whose Thing is now
//     within threshold or has been deleted.
type ThingOfflineAlertsJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// offline-Thing SELECT and resolve-recovered SELECT can be driven by
	// pgxmock without sharing real thing rows.
	pool       defs.PgxPool
	raiser     defs.AlertRaiser
	ruleLoader ruleLoader
	interval   time.Duration
	logger     *slog.Logger
}

// NewThingOfflineAlerts constructs the job. interval defaults to 60 seconds.
// raiser/ruleLoader may be nil only in identity-style tests that never call Run.
func NewThingOfflineAlerts(
	pool *pgxpool.Pool,
	raiser defs.AlertRaiser,
	alertStore ruleLoader,
	interval time.Duration,
	logger *slog.Logger,
) *ThingOfflineAlertsJob {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &ThingOfflineAlertsJob{
		pool:       pool,
		raiser:     raiser,
		ruleLoader: alertStore,
		interval:   interval,
		logger:     logger.With("job", thingOfflineJobID),
	}
}

func (j *ThingOfflineAlertsJob) ID() string              { return thingOfflineJobID }
func (j *ThingOfflineAlertsJob) Name() string            { return thingOfflineJobName }
func (j *ThingOfflineAlertsJob) Description() string     { return thingOfflineJobDescription }
func (j *ThingOfflineAlertsJob) Interval() time.Duration { return j.interval }

func (j *ThingOfflineAlertsJob) Run(ctx context.Context) error {
	rule, err := j.ruleLoader.GetRule(ctx, thingOfflineRuleID)
	if err != nil {
		if errors.Is(err, alerting.ErrNotFound) {
			j.logger.Warn("thing.offline rule not found in store, skipping")
			return nil
		}
		return fmt.Errorf("load rule: %w", err)
	}
	if rule == nil || !rule.Enabled {
		return nil
	}

	offlineAfterSec, excludeKinds, err := parseThingOfflineParams(rule.Params)
	if err != nil {
		return fmt.Errorf("parse rule params: %w", err)
	}

	rows, err := j.pool.Query(ctx, `
		SELECT id, type, name, last_seen_at
		FROM thing
		WHERE last_seen_at IS NOT NULL
		  AND last_seen_at < NOW() - make_interval(secs => $1::double precision)
		  AND NOT (type = ANY($2))`,
		offlineAfterSec, excludeKinds,
	)
	if err != nil {
		return fmt.Errorf("query stale things: %w", err)
	}
	defer rows.Close()

	type thingRow struct {
		id         string
		thingType  string
		name       *string
		lastSeenAt time.Time
	}

	var stale []thingRow
	for rows.Next() {
		var r thingRow
		if err := rows.Scan(&r.id, &r.thingType, &r.name, &r.lastSeenAt); err != nil {
			return fmt.Errorf("scan thing row: %w", err)
		}
		stale = append(stale, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate thing rows: %w", err)
	}

	shouldFireSet := make(map[string]bool, len(stale))
	var errs []error
	var raiseCount int

	for _, t := range stale {
		targetKey := thingTargetKeyPrefix + t.id
		shouldFireSet[t.id] = true

		displayName := t.id
		if t.name != nil && *t.name != "" {
			displayName = *t.name
		}

		details := map[string]any{
			"thingId":    t.id,
			"type":       t.thingType,
			"name":       displayName,
			"lastSeenAt": t.lastSeenAt.Format(time.RFC3339),
		}
		msg := fmt.Sprintf("Thing %q (%s) last seen %s", displayName, t.thingType, t.lastSeenAt.Format(time.RFC3339))
		label := fmt.Sprintf("%s (%s)", displayName, t.thingType)

		if err := j.raiser.Raise(ctx, alerting.RaiseInput{
			RuleID:      thingOfflineRuleID,
			TargetKey:   targetKey,
			TargetLabel: label,
			Severity:    rule.DefaultSeverity,
			Message:     msg,
			Details:     details,
		}); err != nil {
			errs = append(errs, fmt.Errorf("raise thing.offline for %s: %w", t.id, err))
			continue
		}
		raiseCount++
	}

	if err := j.resolveRecovered(ctx, shouldFireSet); err != nil {
		errs = append(errs, fmt.Errorf("resolve recovered: %w", err))
	}

	if raiseCount > 0 {
		j.logger.Info("raised thing.offline alerts", "count", raiseCount)
	}
	return errors.Join(errs...)
}

// resolveRecovered walks currently firing thing.offline alerts and resolves
// any whose underlying Thing is no longer in the shouldFireSet — either the
// Thing came back online (last_seen_at within threshold) or it was deleted.
func (j *ThingOfflineAlertsJob) resolveRecovered(ctx context.Context, shouldFireSet map[string]bool) error {
	rows, err := j.pool.Query(ctx, `
		SELECT "targetKey"
		FROM "Alert"
		WHERE "ruleId" = $1
		  AND state IN ('FIRING'::"AlertState", 'ACKNOWLEDGED'::"AlertState")`,
		thingOfflineRuleID,
	)
	if err != nil {
		return fmt.Errorf("list firing thing.offline alerts: %w", err)
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
		thingID := strings.TrimPrefix(targetKey, thingTargetKeyPrefix)
		if thingID == targetKey {
			// Not in thing:<id> format — defensive skip.
			continue
		}
		if shouldFireSet[thingID] {
			continue
		}
		if err := j.raiser.Resolve(ctx, thingOfflineRuleID, targetKey, thingOfflineResolveReason); err != nil {
			j.logger.Warn("resolve thing.offline alert failed", "targetKey", targetKey, "err", err)
		}
	}
	return nil
}

// parseThingOfflineParams extracts offlineAfterSec and excludeKinds from the
// rule's Params map. JSON numbers unmarshal as float64; []any unmarshal as
// []interface{}; both are converted here.
func parseThingOfflineParams(params map[string]any) (float64, []string, error) {
	raw, ok := params["offlineAfterSec"]
	if !ok {
		return 0, nil, fmt.Errorf("offlineAfterSec missing from rule params")
	}
	offlineAfterSec, ok := raw.(float64)
	if !ok {
		return 0, nil, fmt.Errorf("offlineAfterSec must be a number, got %T", raw)
	}
	if offlineAfterSec <= 0 {
		return 0, nil, fmt.Errorf("offlineAfterSec must be > 0, got %v", offlineAfterSec)
	}

	excludeKinds := []string{}
	if rawKinds, ok := params["excludeKinds"]; ok && rawKinds != nil {
		kindSlice, ok := rawKinds.([]any)
		if !ok {
			return 0, nil, fmt.Errorf("excludeKinds must be an array, got %T", rawKinds)
		}
		for i, v := range kindSlice {
			s, ok := v.(string)
			if !ok {
				return 0, nil, fmt.Errorf("excludeKinds[%d] must be a string, got %T", i, v)
			}
			excludeKinds = append(excludeKinds, s)
		}
	}

	return offlineAfterSec, excludeKinds, nil
}
