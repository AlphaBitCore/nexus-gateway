package quotastore

import (
	"context"
	"fmt"
	"time"
)

// QuotaAlert represents a row from the QuotaAlert table.
type QuotaAlert struct {
	ID              string     `json:"id"`
	AlertType       string     `json:"alertType"`
	TargetType      string     `json:"targetType"`
	TargetID        string     `json:"targetId"`
	TargetName      *string    `json:"targetName"`
	PolicyID        *string    `json:"policyId"`
	OverrideID      *string    `json:"overrideId"`
	PeriodKey       *string    `json:"periodKey"`
	ThresholdPct    *int       `json:"thresholdPct"`
	CurrentUsagePct *float64   `json:"currentUsagePct"`
	CostLimitUsd    *float64   `json:"costLimitUsd"`
	CurrentCostUsd  *float64   `json:"currentCostUsd"`
	ExpiresAt       *time.Time `json:"expiresAt"`
	Status          string     `json:"status"`
	AcknowledgedBy  *string    `json:"acknowledgedBy"`
	AcknowledgedAt  *time.Time `json:"acknowledgedAt"`
	CreatedAt       time.Time  `json:"createdAt"`
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

// UpsertQuotaAlert inserts or updates a quota alert on conflict.
// On conflict (alertType, targetType, targetId, periodKey, thresholdPct) it updates usage fields and resets status to 'active'.
func (store *Store) UpsertQuotaAlert(ctx context.Context, p UpsertQuotaAlertParams) error {
	_, err := store.pool.Exec(ctx, `
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
