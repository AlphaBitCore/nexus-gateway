// Package quotastore holds the minimum Postgres queries Hub background
// jobs need to manage virtual-key expiry and quota alerts. It is a
// Hub-internal subset of the Control Plane quota store — not shared, not
// importable from CP — so that moving scheduled jobs to Hub doesn't drag
// the CP store surface (with its 30+ admin CRUD helpers) into Hub.
package quotastore

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PgxPool is the minimum pgx pool surface this package's helpers need.
// *pgxpool.Pool satisfies it in production; pgxmock.PgxPoolIface satisfies
// it in tests, letting these helpers be unit-tested without a live Postgres.
// Mirrors the seam in packages/nexus-hub/internal/alerts/engine and
// packages/nexus-hub/internal/storage/store.
type PgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// VirtualKeyExpiry holds the minimum fields needed to emit an expiring-key
// alert.
type VirtualKeyExpiry struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// QuotaOverride holds the minimum fields needed to evaluate override-based
// quota alerts (no LEFT JOIN name resolution — alerts don't need display
// names at this stage).
type QuotaOverride struct {
	ID           string
	TargetType   string
	TargetID     string
	CostLimitUsd *float64
}

// QuotaPolicy holds the minimum fields needed to evaluate policy-based
// quota alerts.
type QuotaPolicy struct {
	ID              string
	Scope           string
	OrganizationID  *string
	CostLimitUsd    *float64
	AlertThresholds json.RawMessage
}

// UpsertQuotaAlertParams holds fields for upserting a quota alert.
type UpsertQuotaAlertParams struct {
	AlertType       string
	TargetType      string
	TargetID        string
	TargetName      *string
	PolicyID        *string
	OverrideID      *string
	PeriodKey       string
	ThresholdPct    int
	CurrentUsagePct float64
	CostLimitUsd    *float64
	CurrentCostUsd  *float64
	ExpiresAt       *time.Time
}

// ExpireOverdueVirtualKeys sets vkStatus='expired' for all active keys past
// their expiry. Returns the number of rows updated.
func ExpireOverdueVirtualKeys(ctx context.Context, pool PgxPool) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE "VirtualKey"
		SET "vkStatus" = 'expired', "updatedAt" = NOW()
		WHERE "expiresAt" <= NOW() AND "vkStatus" = 'active'
	`)
	if err != nil {
		return 0, fmt.Errorf("expire overdue virtual keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListExpiringVirtualKeys returns active application keys expiring within
// the given number of days.
func ListExpiringVirtualKeys(ctx context.Context, pool PgxPool, withinDays int) ([]VirtualKeyExpiry, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, name, "expiresAt"
		FROM "VirtualKey"
		WHERE "expiresAt" <= NOW() + ($1 * INTERVAL '1 day')
		  AND "expiresAt" > NOW()
		  AND "vkStatus" = 'active'
		  AND "vkType" = 'application'
		ORDER BY "expiresAt" ASC
	`, withinDays)
	if err != nil {
		return nil, fmt.Errorf("list expiring virtual keys: %w", err)
	}
	defer rows.Close()

	keys := []VirtualKeyExpiry{}
	for rows.Next() {
		var k VirtualKeyExpiry
		if err := rows.Scan(&k.ID, &k.Name, &k.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan virtual key expiry: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// ListActiveQuotaOverrides returns every quota override row without the
// admin-display LEFT JOINs. Alerts work from (TargetType, TargetID) alone.
func ListActiveQuotaOverrides(ctx context.Context, pool PgxPool) ([]QuotaOverride, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, "targetType", "targetId", "costLimitUsd"::double precision
		FROM "QuotaOverride"
	`)
	if err != nil {
		return nil, fmt.Errorf("list quota overrides: %w", err)
	}
	defer rows.Close()

	out := []QuotaOverride{}
	for rows.Next() {
		var o QuotaOverride
		if err := rows.Scan(&o.ID, &o.TargetType, &o.TargetID, &o.CostLimitUsd); err != nil {
			return nil, fmt.Errorf("scan quota override: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListEnabledQuotaPolicies returns enabled quota policies, ordered by the
// same (priority DESC, createdAt DESC) tiebreak used by the CP admin list,
// so alert evaluation picks the same "first match" when multiple policies
// target the same scope.
func ListEnabledQuotaPolicies(ctx context.Context, pool PgxPool) ([]QuotaPolicy, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, scope, "organizationId", "costLimitUsd"::double precision, "alertThresholds"
		FROM "QuotaPolicy"
		WHERE enabled = true
		ORDER BY priority DESC, "createdAt" DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list quota policies: %w", err)
	}
	defer rows.Close()

	out := []QuotaPolicy{}
	for rows.Next() {
		var p QuotaPolicy
		if err := rows.Scan(&p.ID, &p.Scope, &p.OrganizationID, &p.CostLimitUsd, &p.AlertThresholds); err != nil {
			return nil, fmt.Errorf("scan quota policy: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertQuotaAlert inserts or updates a quota alert on conflict
// (alertType, targetType, targetId, periodKey, thresholdPct).
func UpsertQuotaAlert(ctx context.Context, pool PgxPool, p UpsertQuotaAlertParams) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO "QuotaAlert" (id, "alertType", "targetType", "targetId", "targetName",
			"policyId", "overrideId", "periodKey", "thresholdPct",
			"currentUsagePct", "costLimitUsd", "currentCostUsd", "expiresAt", status, "createdAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'active', NOW())
		ON CONFLICT ("alertType", "targetType", "targetId", "periodKey", "thresholdPct") DO UPDATE SET
			"currentUsagePct" = EXCLUDED."currentUsagePct",
			"currentCostUsd" = EXCLUDED."currentCostUsd",
			status = 'active'
	`,
		p.AlertType, p.TargetType, p.TargetID, p.TargetName,
		p.PolicyID, p.OverrideID, p.PeriodKey, p.ThresholdPct,
		p.CurrentUsagePct, p.CostLimitUsd, p.CurrentCostUsd, p.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("upsert quota alert: %w", err)
	}
	return nil
}
