package routingstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
)

// RoutingRule represents a row from the RoutingRule table.
type RoutingRule struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Description     *string         `json:"description"`
	StrategyType    string          `json:"strategyType"`
	Config          json.RawMessage `json:"config"`
	MatchConditions json.RawMessage `json:"matchConditions"`
	Priority        int             `json:"priority"`
	PipelineStage   int             `json:"pipelineStage"`
	FallbackChain   json.RawMessage `json:"fallbackChain"`
	// RetryPolicy is a per-rule override of the ai-gateway retry policy.
	// NULL in the DB (encoded here as a nil RawMessage) means "use the
	// ai-gateway YAML default". Marshaled with omitempty so the JSON
	// response omits it entirely when unset.
	RetryPolicy json.RawMessage `json:"retryPolicy,omitempty"`
	Enabled     bool            `json:"enabled"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

const rrColumns = `id, name, description, "strategyType", config, "matchConditions",
	priority, "pipelineStage", "fallbackChain", "retryPolicy",
	enabled, "createdAt", "updatedAt"`

func scanRR(row pgx.Row) (*RoutingRule, error) {
	var r RoutingRule
	err := row.Scan(
		&r.ID, &r.Name, &r.Description, &r.StrategyType, &r.Config, &r.MatchConditions,
		&r.Priority, &r.PipelineStage, &r.FallbackChain, &r.RetryPolicy,
		&r.Enabled, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// RoutingRuleListParams holds filter/pagination.
type RoutingRuleListParams struct {
	Q            string
	Enabled      *bool
	StrategyType string
	Limit        int
	Offset       int
}

// ListRoutingRules returns routing rules with filtering.
func (store *Store) ListRoutingRules(ctx context.Context, p RoutingRuleListParams) ([]RoutingRule, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.Enabled != nil {
		where += fmt.Sprintf(` AND enabled = $%d`, argIdx)
		args = append(args, *p.Enabled)
		argIdx++
	}
	if p.StrategyType != "" {
		where += fmt.Sprintf(` AND "strategyType" = $%d`, argIdx)
		args = append(args, p.StrategyType)
		argIdx++
	}
	if p.Q != "" {
		where += fmt.Sprintf(` AND (name ILIKE $%d OR description ILIKE $%d)`, argIdx, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "RoutingRule" %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count routing rules: %w", err)
	}

	q := fmt.Sprintf(`SELECT %s FROM "RoutingRule" %s ORDER BY "pipelineStage" ASC, priority DESC LIMIT $%d OFFSET $%d`,
		rrColumns, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list routing rules: %w", err)
	}
	defer rows.Close()

	rules := []RoutingRule{}
	for rows.Next() {
		var r RoutingRule
		if err := rows.Scan(
			&r.ID, &r.Name, &r.Description, &r.StrategyType, &r.Config, &r.MatchConditions,
			&r.Priority, &r.PipelineStage, &r.FallbackChain, &r.RetryPolicy,
			&r.Enabled, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan routing rule: %w", err)
		}
		rules = append(rules, r)
	}
	return rules, total, rows.Err()
}

// GetRoutingRule returns a routing rule by ID.
func (store *Store) GetRoutingRule(ctx context.Context, id string) (*RoutingRule, error) {
	q := fmt.Sprintf(`SELECT %s FROM "RoutingRule" WHERE id = $1`, rrColumns)
	r, err := scanRR(store.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, fmt.Errorf("get routing rule: %w", err)
	}
	return r, nil
}

// CreateRoutingRuleParams holds fields for creating a routing rule.
type CreateRoutingRuleParams struct {
	Name            string
	Description     *string
	StrategyType    string
	Config          json.RawMessage
	MatchConditions json.RawMessage
	Priority        int
	PipelineStage   int
	FallbackChain   json.RawMessage
	// RetryPolicy is the per-rule override. nil → store NULL (rule
	// inherits the ai-gateway YAML default at runtime).
	RetryPolicy json.RawMessage
	Enabled     bool
}

// CreateRoutingRule inserts a new routing rule.
func (store *Store) CreateRoutingRule(ctx context.Context, p CreateRoutingRuleParams) (*RoutingRule, error) {
	q := fmt.Sprintf(`
		INSERT INTO "RoutingRule" (id, name, description, "strategyType", config, "matchConditions",
			priority, "pipelineStage", "fallbackChain", "retryPolicy",
			enabled, "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW())
		RETURNING %s
	`, rrColumns)
	r, err := scanRR(store.pool.QueryRow(ctx, q,
		p.Name, p.Description, p.StrategyType, p.Config, p.MatchConditions,
		p.Priority, p.PipelineStage, p.FallbackChain, jsonOrNil(p.RetryPolicy),
		p.Enabled,
	))
	if err != nil {
		return nil, fmt.Errorf("create routing rule: %w", err)
	}
	return r, nil
}

// jsonOrNil returns nil when the raw message is empty or JSON null, so pgx
// stores SQL NULL in the JSONB column. Otherwise the bytes are passed
// through verbatim.
func jsonOrNil(raw json.RawMessage) any {
	s := string(raw)
	if s == "" || s == "null" {
		return nil
	}
	return raw
}

// UpdateRoutingRuleParams holds optional fields for updating a routing rule.
//
// For RetryPolicy the field is a pointer so callers can express three intents:
//
//	nil pointer          → no change (column left as-is)
//	pointer to null/empty → set column to SQL NULL (clear override)
//	pointer to {…}       → set column to the supplied JSON
type UpdateRoutingRuleParams struct {
	Name            *string
	Description     *string
	StrategyType    *string
	Config          json.RawMessage // nil = no change
	MatchConditions json.RawMessage // nil = no change
	Priority        *int
	PipelineStage   *int
	FallbackChain   json.RawMessage  // nil = no change
	RetryPolicy     *json.RawMessage // nil pointer = no change; see doc above
	Enabled         *bool
}

// UpdateRoutingRule updates a routing rule using COALESCE for the columns that
// follow the simple "nil = no change" contract, and an explicit
// "$N::boolean → SET vs leave alone" toggle for retryPolicy so the caller can
// clear it to SQL NULL.
func (store *Store) UpdateRoutingRule(ctx context.Context, id string, p UpdateRoutingRuleParams) (*RoutingRule, error) {
	var rpChange bool
	var rpValue any
	if p.RetryPolicy != nil {
		rpChange = true
		rpValue = jsonOrNil(*p.RetryPolicy)
	}

	q := fmt.Sprintf(`UPDATE "RoutingRule" SET
		name = COALESCE($2, name),
		description = COALESCE($3, description),
		"strategyType" = COALESCE($4, "strategyType"),
		config = COALESCE($5, config),
		"matchConditions" = COALESCE($6, "matchConditions"),
		priority = COALESCE($7, priority),
		"pipelineStage" = COALESCE($8, "pipelineStage"),
		"fallbackChain" = COALESCE($9, "fallbackChain"),
		"retryPolicy" = CASE WHEN $10::boolean THEN $11::jsonb ELSE "retryPolicy" END,
		enabled = COALESCE($12, enabled),
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, rrColumns)

	r, err := scanRR(store.pool.QueryRow(ctx, q, id,
		p.Name, p.Description, p.StrategyType, p.Config, p.MatchConditions,
		p.Priority, p.PipelineStage, p.FallbackChain, rpChange, rpValue,
		p.Enabled,
	))
	if err != nil {
		return nil, fmt.Errorf("update routing rule: %w", err)
	}
	return r, nil
}

// DeleteRoutingRule deletes a routing rule by ID.
func (store *Store) DeleteRoutingRule(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "RoutingRule" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete routing rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
