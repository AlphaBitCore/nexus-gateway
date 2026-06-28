package quotastore

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
)

// QuotaPolicy represents a row from the QuotaPolicy table.
type QuotaPolicy struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Description     *string         `json:"description"`
	Scope           string          `json:"scope"`
	OrganizationID  *string         `json:"organizationId"`
	VKType          *string         `json:"vkType"`
	PeriodType      string          `json:"periodType"`
	CostLimitUsd    *float64        `json:"costLimitUsd"`
	EnforcementMode string          `json:"enforcementMode"`
	AlertThresholds json.RawMessage `json:"alertThresholds"`
	Priority        int             `json:"priority"`
	Enabled         bool            `json:"enabled"`
	CreatedBy       *string         `json:"createdBy"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
}

const quotaPolicyColumns = `id, name, description, scope, "organizationId", "vkType",
	"periodType", "costLimitUsd"::double precision, "enforcementMode",
	"alertThresholds", priority, enabled, "createdBy", "createdAt", "updatedAt"`

func scanQuotaPolicy(row pgx.Row) (*QuotaPolicy, error) {
	var p QuotaPolicy
	err := row.Scan(
		&p.ID, &p.Name, &p.Description, &p.Scope, &p.OrganizationID, &p.VKType,
		&p.PeriodType, &p.CostLimitUsd, &p.EnforcementMode,
		&p.AlertThresholds, &p.Priority, &p.Enabled, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// QuotaPolicyListParams holds filter/pagination for listing quota policies.
type QuotaPolicyListParams struct {
	Scope   string
	VKType  string
	Enabled *bool
	Q       string
	Limit   int
	Offset  int
}

// ListQuotaPolicies returns quota policies with filtering and pagination.
func (store *Store) ListQuotaPolicies(ctx context.Context, p QuotaPolicyListParams) ([]QuotaPolicy, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.Scope != "" {
		where += fmt.Sprintf(` AND scope = $%d`, argIdx)
		args = append(args, p.Scope)
		argIdx++
	}
	if p.VKType != "" {
		where += fmt.Sprintf(` AND "vkType" = $%d`, argIdx)
		args = append(args, p.VKType)
		argIdx++
	}
	if p.Enabled != nil {
		where += fmt.Sprintf(` AND enabled = $%d`, argIdx)
		args = append(args, *p.Enabled)
		argIdx++
	}
	if p.Q != "" {
		where += fmt.Sprintf(` AND name ILIKE $%d`, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "QuotaPolicy" %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count quota policies: %w", err)
	}

	query := fmt.Sprintf(`SELECT %s FROM "QuotaPolicy" %s ORDER BY priority DESC, "createdAt" DESC LIMIT $%d OFFSET $%d`,
		quotaPolicyColumns, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list quota policies: %w", err)
	}
	defer rows.Close()

	policies := []QuotaPolicy{}
	for rows.Next() {
		var pol QuotaPolicy
		if err := rows.Scan(
			&pol.ID, &pol.Name, &pol.Description, &pol.Scope, &pol.OrganizationID, &pol.VKType,
			&pol.PeriodType, &pol.CostLimitUsd, &pol.EnforcementMode,
			&pol.AlertThresholds, &pol.Priority, &pol.Enabled, &pol.CreatedBy, &pol.CreatedAt, &pol.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan quota policy: %w", err)
		}
		policies = append(policies, pol)
	}
	return policies, total, rows.Err()
}

// ListEnabledPoliciesForScopes returns every enabled quota policy whose scope is
// in the given set, ordered by priority DESC then createdAt DESC — the same
// ordering the ai-gateway policy cache applies (policy_cache.go Load + FindPolicy)
// so the first row that matches an entity's org/vkType is the policy the gateway
// engine would resolve. Used by the quota analytics dashboard to mirror the
// gateway's override→policy precedence rather than reading overrides alone.
func (store *Store) ListEnabledPoliciesForScopes(ctx context.Context, scopes []string) ([]QuotaPolicy, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	q := fmt.Sprintf(`SELECT %s FROM "QuotaPolicy" WHERE enabled = true AND scope = ANY($1) ORDER BY priority DESC, "createdAt" DESC`, quotaPolicyColumns)
	rows, err := store.pool.Query(ctx, q, scopes)
	if err != nil {
		return nil, fmt.Errorf("list enabled policies for scopes: %w", err)
	}
	defer rows.Close()

	policies := []QuotaPolicy{}
	for rows.Next() {
		var pol QuotaPolicy
		if err := rows.Scan(
			&pol.ID, &pol.Name, &pol.Description, &pol.Scope, &pol.OrganizationID, &pol.VKType,
			&pol.PeriodType, &pol.CostLimitUsd, &pol.EnforcementMode,
			&pol.AlertThresholds, &pol.Priority, &pol.Enabled, &pol.CreatedBy, &pol.CreatedAt, &pol.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan quota policy: %w", err)
		}
		policies = append(policies, pol)
	}
	return policies, rows.Err()
}

// GetQuotaPolicy returns a quota policy by ID.
func (store *Store) GetQuotaPolicy(ctx context.Context, id string) (*QuotaPolicy, error) {
	q := fmt.Sprintf(`SELECT %s FROM "QuotaPolicy" WHERE id = $1`, quotaPolicyColumns)
	pol, err := scanQuotaPolicy(store.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, fmt.Errorf("get quota policy: %w", err)
	}
	return pol, nil
}

// CreateQuotaPolicyParams holds fields for creating a quota policy.
type CreateQuotaPolicyParams struct {
	Name            string
	Description     *string
	Scope           string
	OrganizationID  *string
	VKType          *string
	PeriodType      string
	CostLimitUsd    *float64
	EnforcementMode string
	AlertThresholds json.RawMessage
	Priority        int
	Enabled         bool
	CreatedBy       *string
}

// CreateQuotaPolicy inserts a new quota policy.
func (store *Store) CreateQuotaPolicy(ctx context.Context, p CreateQuotaPolicyParams) (*QuotaPolicy, error) {
	q := fmt.Sprintf(`
		INSERT INTO "QuotaPolicy" (id, name, description, scope, "organizationId", "vkType",
			"periodType", "costLimitUsd", "enforcementMode",
			"alertThresholds", priority, enabled, "createdBy", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW(), NOW())
		RETURNING %s
	`, quotaPolicyColumns)
	pol, err := scanQuotaPolicy(store.pool.QueryRow(ctx, q,
		p.Name, p.Description, p.Scope, p.OrganizationID, p.VKType,
		p.PeriodType, p.CostLimitUsd, p.EnforcementMode,
		p.AlertThresholds, p.Priority, p.Enabled, p.CreatedBy,
	))
	if err != nil {
		return nil, fmt.Errorf("create quota policy: %w", err)
	}
	return pol, nil
}

// UpdateQuotaPolicyParams holds optional fields for updating a quota policy.
type UpdateQuotaPolicyParams struct {
	Name            *string
	Description     *string
	Scope           *string
	OrganizationID  *string
	VKType          *string
	PeriodType      *string
	CostLimitUsd    *float64
	EnforcementMode *string
	AlertThresholds json.RawMessage // nil = no change
	Priority        *int
	Enabled         *bool
}

// UpdateQuotaPolicy updates a quota policy using COALESCE.
func (store *Store) UpdateQuotaPolicy(ctx context.Context, id string, p UpdateQuotaPolicyParams) (*QuotaPolicy, error) {
	q := fmt.Sprintf(`UPDATE "QuotaPolicy" SET
		name = COALESCE($2, name),
		description = COALESCE($3, description),
		scope = COALESCE($4, scope),
		"organizationId" = COALESCE($5, "organizationId"),
		"vkType" = COALESCE($6, "vkType"),
		"periodType" = COALESCE($7, "periodType"),
		"costLimitUsd" = COALESCE($8, "costLimitUsd"),
		"enforcementMode" = COALESCE($9, "enforcementMode"),
		"alertThresholds" = COALESCE($10, "alertThresholds"),
		priority = COALESCE($11, priority),
		enabled = COALESCE($12, enabled),
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, quotaPolicyColumns)

	pol, err := scanQuotaPolicy(store.pool.QueryRow(ctx, q, id,
		p.Name, p.Description, p.Scope, p.OrganizationID, p.VKType,
		p.PeriodType, p.CostLimitUsd, p.EnforcementMode,
		p.AlertThresholds, p.Priority, p.Enabled,
	))
	if err != nil {
		return nil, fmt.Errorf("update quota policy: %w", err)
	}
	return pol, nil
}

// DeleteQuotaPolicy deletes a quota policy by ID.
func (store *Store) DeleteQuotaPolicy(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "QuotaPolicy" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete quota policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
