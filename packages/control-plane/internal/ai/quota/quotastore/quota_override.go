package quotastore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrQuotaOverrideConflict is returned when a quota override already exists for the target.
var ErrQuotaOverrideConflict = errors.New("quota override already exists for this target")

// QuotaOverride represents a row from the QuotaOverride table.
type QuotaOverride struct {
	ID              string    `json:"id"`
	TargetType      string    `json:"targetType"`
	TargetID        string    `json:"targetId"`
	TargetName      string    `json:"targetName"`
	Reason          *string   `json:"reason"`
	CostLimitUsd    *float64  `json:"costLimitUsd"`
	TokenLimit      *int64    `json:"tokenLimit"`
	EnforcementMode *string   `json:"enforcementMode"`
	PeriodType      *string   `json:"periodType"`
	CreatedBy       *string   `json:"createdBy"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

const quotaOverrideColumns = `id, "targetType", "targetId", reason,
	"costLimitUsd"::double precision, "tokenLimit", "enforcementMode", "periodType",
	"createdBy", "createdAt", "updatedAt"`

func scanQuotaOverride(row pgx.Row) (*QuotaOverride, error) {
	var o QuotaOverride
	err := row.Scan(
		&o.ID, &o.TargetType, &o.TargetID, &o.Reason,
		&o.CostLimitUsd, &o.TokenLimit, &o.EnforcementMode, &o.PeriodType,
		&o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// QuotaOverrideListParams holds filter/pagination for listing quota overrides.
type QuotaOverrideListParams struct {
	TargetType string
	Q          string
	Limit      int
	Offset     int
}

// ListQuotaOverrides returns quota overrides with filtering, pagination,
// and resolved targetName via LEFT JOINs on entity tables.
func (store *Store) ListQuotaOverrides(ctx context.Context, p QuotaOverrideListParams) ([]QuotaOverride, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.TargetType != "" {
		where += fmt.Sprintf(` AND qo."targetType" = $%d`, argIdx)
		args = append(args, p.TargetType)
		argIdx++
	}
	if p.Q != "" {
		where += fmt.Sprintf(` AND (qo."targetId" ILIKE $%d OR qo.reason ILIKE $%d)`, argIdx, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "QuotaOverride" qo %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count quota overrides: %w", err)
	}

	// Resolve targetName by LEFT JOINing each entity table and using COALESCE
	query := fmt.Sprintf(`
		SELECT qo.id, qo."targetType", qo."targetId",
			COALESCE(nu."displayName", org.name, proj.name, vk.name, qo."targetId") AS "targetName",
			qo.reason,
			qo."costLimitUsd"::double precision, qo."tokenLimit", qo."enforcementMode", qo."periodType",
			qo."createdBy", qo."createdAt", qo."updatedAt"
		FROM "QuotaOverride" qo
		LEFT JOIN "NexusUser" nu ON qo."targetType" = 'user' AND qo."targetId" = nu.id
		LEFT JOIN "Organization" org ON qo."targetType" = 'organization' AND qo."targetId" = org.id
		LEFT JOIN "Project" proj ON qo."targetType" = 'project' AND qo."targetId" = proj.id
		LEFT JOIN "VirtualKey" vk ON qo."targetType" = 'vk' AND qo."targetId" = vk.id
		%s
		ORDER BY qo."createdAt" DESC
		LIMIT $%d OFFSET $%d
	`, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list quota overrides: %w", err)
	}
	defer rows.Close()

	overrides := []QuotaOverride{}
	for rows.Next() {
		var o QuotaOverride
		if err := rows.Scan(
			&o.ID, &o.TargetType, &o.TargetID, &o.TargetName, &o.Reason,
			&o.CostLimitUsd, &o.TokenLimit, &o.EnforcementMode, &o.PeriodType,
			&o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan quota override: %w", err)
		}
		overrides = append(overrides, o)
	}
	return overrides, total, rows.Err()
}

// GetQuotaOverride returns a quota override by ID with resolved targetName.
func (store *Store) GetQuotaOverride(ctx context.Context, id string) (*QuotaOverride, error) {
	q := `
		SELECT qo.id, qo."targetType", qo."targetId",
			COALESCE(nu."displayName", org.name, proj.name, vk.name, qo."targetId") AS "targetName",
			qo.reason,
			qo."costLimitUsd"::double precision, qo."tokenLimit", qo."enforcementMode", qo."periodType",
			qo."createdBy", qo."createdAt", qo."updatedAt"
		FROM "QuotaOverride" qo
		LEFT JOIN "NexusUser" nu ON qo."targetType" = 'user' AND qo."targetId" = nu.id
		LEFT JOIN "Organization" org ON qo."targetType" = 'organization' AND qo."targetId" = org.id
		LEFT JOIN "Project" proj ON qo."targetType" = 'project' AND qo."targetId" = proj.id
		LEFT JOIN "VirtualKey" vk ON qo."targetType" = 'vk' AND qo."targetId" = vk.id
		WHERE qo.id = $1
	`
	var o QuotaOverride
	err := store.pool.QueryRow(ctx, q, id).Scan(
		&o.ID, &o.TargetType, &o.TargetID, &o.TargetName,
		&o.Reason,
		&o.CostLimitUsd, &o.TokenLimit, &o.EnforcementMode, &o.PeriodType,
		&o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get quota override: %w", err)
	}
	return &o, nil
}

// GetQuotaOverrideByTarget returns the quota override for a specific target, if any.
func (store *Store) GetQuotaOverrideByTarget(ctx context.Context, targetType, targetID string) (*QuotaOverride, error) {
	q := fmt.Sprintf(`SELECT %s FROM "QuotaOverride" WHERE "targetType" = $1 AND "targetId" = $2`, quotaOverrideColumns)
	o, err := scanQuotaOverride(store.pool.QueryRow(ctx, q, targetType, targetID))
	if err != nil {
		return nil, fmt.Errorf("get quota override by target: %w", err)
	}
	return o, nil
}

// CreateQuotaOverrideParams holds fields for creating a quota override.
type CreateQuotaOverrideParams struct {
	TargetType      string
	TargetID        string
	Reason          *string
	CostLimitUsd    *float64
	TokenLimit      *int64
	EnforcementMode *string
	PeriodType      *string
	CreatedBy       *string
}

// CreateQuotaOverride inserts a new quota override.
// Returns ErrQuotaOverrideConflict if an override already exists for the target.
func (store *Store) CreateQuotaOverride(ctx context.Context, p CreateQuotaOverrideParams) (*QuotaOverride, error) {
	q := fmt.Sprintf(`
		INSERT INTO "QuotaOverride" (id, "targetType", "targetId", reason,
			"costLimitUsd", "tokenLimit", "enforcementMode", "periodType",
			"createdBy", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
		RETURNING %s
	`, quotaOverrideColumns)
	o, err := scanQuotaOverride(store.pool.QueryRow(ctx, q,
		p.TargetType, p.TargetID, p.Reason,
		p.CostLimitUsd, p.TokenLimit, p.EnforcementMode, p.PeriodType, p.CreatedBy,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrQuotaOverrideConflict
		}
		return nil, fmt.Errorf("create quota override: %w", err)
	}
	return o, nil
}

// UpdateQuotaOverrideParams holds optional fields for updating a quota override.
type UpdateQuotaOverrideParams struct {
	Reason          *string
	CostLimitUsd    *float64
	TokenLimit      *int64
	EnforcementMode *string
	PeriodType      *string
}

// UpdateQuotaOverride updates a quota override using COALESCE.
func (store *Store) UpdateQuotaOverride(ctx context.Context, id string, p UpdateQuotaOverrideParams) (*QuotaOverride, error) {
	q := fmt.Sprintf(`UPDATE "QuotaOverride" SET
		reason = COALESCE($2, reason),
		"costLimitUsd" = COALESCE($3, "costLimitUsd"),
		"tokenLimit" = COALESCE($4, "tokenLimit"),
		"enforcementMode" = COALESCE($5, "enforcementMode"),
		"periodType" = COALESCE($6, "periodType"),
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, quotaOverrideColumns)

	o, err := scanQuotaOverride(store.pool.QueryRow(ctx, q, id,
		p.Reason, p.CostLimitUsd, p.TokenLimit, p.EnforcementMode, p.PeriodType,
	))
	if err != nil {
		return nil, fmt.Errorf("update quota override: %w", err)
	}
	return o, nil
}

// DeleteQuotaOverride deletes a quota override by ID.
func (store *Store) DeleteQuotaOverride(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "QuotaOverride" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete quota override: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
