package hookstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
)

// HookConfig represents a row from the HookConfig table.
type HookConfig struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	Type              string          `json:"type"`
	ImplementationID  string          `json:"implementationId"`
	Stage             string          `json:"stage"`
	Category          *string         `json:"category"`
	Endpoint          *string         `json:"endpoint"`
	Script            *string         `json:"script"`
	Config            json.RawMessage `json:"config"`
	Priority          int             `json:"priority"`
	TimeoutMs         int             `json:"timeoutMs"`
	FailBehavior      string          `json:"failBehavior"`
	Enabled           bool            `json:"enabled"`
	ApplicableIngress []string        `json:"applicableIngress"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
}

const hcColumns = `id, name, type, "implementationId", stage, category, endpoint, script,
	config, priority, "timeoutMs", "failBehavior", enabled, "applicableIngress", "createdAt", "updatedAt"`

func scanHC(row pgx.Row) (*HookConfig, error) {
	var h HookConfig
	err := row.Scan(
		&h.ID, &h.Name, &h.Type, &h.ImplementationID, &h.Stage, &h.Category,
		&h.Endpoint, &h.Script, &h.Config, &h.Priority, &h.TimeoutMs,
		&h.FailBehavior, &h.Enabled, &h.ApplicableIngress, &h.CreatedAt, &h.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// HookConfigListParams holds filter/pagination.
type HookConfigListParams struct {
	Q        string
	Enabled  *bool
	Pipeline string // request | response
	Limit    int
	Offset   int
}

// ListHookConfigs returns hook configs with filtering.
func (store *Store) ListHookConfigs(ctx context.Context, p HookConfigListParams) ([]HookConfig, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.Q != "" {
		where += fmt.Sprintf(` AND (name ILIKE $%d OR endpoint ILIKE $%d OR type ILIKE $%d)`, argIdx, argIdx, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}
	if p.Enabled != nil {
		where += fmt.Sprintf(` AND enabled = $%d`, argIdx)
		args = append(args, *p.Enabled)
		argIdx++
	}
	switch p.Pipeline {
	case "request":
		where += ` AND stage = 'request'`
	case "response":
		where += ` AND stage = 'response'`
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "HookConfig" %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count hook configs: %w", err)
	}

	q := fmt.Sprintf(`SELECT %s FROM "HookConfig" %s ORDER BY priority ASC LIMIT $%d OFFSET $%d`,
		hcColumns, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list hook configs: %w", err)
	}
	defer rows.Close()

	hooks := []HookConfig{}
	for rows.Next() {
		var h HookConfig
		if err := rows.Scan(
			&h.ID, &h.Name, &h.Type, &h.ImplementationID, &h.Stage, &h.Category,
			&h.Endpoint, &h.Script, &h.Config, &h.Priority, &h.TimeoutMs,
			&h.FailBehavior, &h.Enabled, &h.ApplicableIngress, &h.CreatedAt, &h.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan hook config: %w", err)
		}
		hooks = append(hooks, h)
	}
	return hooks, total, rows.Err()
}

// GetHookConfig returns a hook config by ID.
func (store *Store) GetHookConfig(ctx context.Context, id string) (*HookConfig, error) {
	q := fmt.Sprintf(`SELECT %s FROM "HookConfig" WHERE id = $1`, hcColumns)
	h, err := scanHC(store.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, fmt.Errorf("get hook config: %w", err)
	}
	return h, nil
}

// CreateHookConfigParams holds fields for creating a hook config.
//
// ApplicableIngress nil → rely on DB default (`{ALL}`). Explicit empty slice
// is rejected at the handler boundary so a serialized `[]` cannot silently
// disable the row on every ingress.
type CreateHookConfigParams struct {
	Name              string
	Type              string
	ImplementationID  string
	Stage             string
	Category          *string
	Endpoint          *string
	Script            *string
	Config            json.RawMessage
	Priority          int
	TimeoutMs         int
	FailBehavior      string
	Enabled           bool
	ApplicableIngress []string
}

// CreateHookConfig inserts a new hook config.
func (store *Store) CreateHookConfig(ctx context.Context, p CreateHookConfigParams) (*HookConfig, error) {
	// COALESCE($13::text[], '{ALL}') makes nil fall through to the schema
	// default without duplicating the default literal in Go.
	q := fmt.Sprintf(`
		INSERT INTO "HookConfig" (id, name, type, "implementationId", stage, category,
			endpoint, script, config, priority, "timeoutMs", "failBehavior", enabled,
			"applicableIngress", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			COALESCE($13::text[], ARRAY['ALL']::text[]), NOW(), NOW())
		RETURNING %s
	`, hcColumns)
	h, err := scanHC(store.pool.QueryRow(ctx, q,
		p.Name, p.Type, p.ImplementationID, p.Stage, p.Category,
		p.Endpoint, p.Script, p.Config, p.Priority, p.TimeoutMs,
		p.FailBehavior, p.Enabled, p.ApplicableIngress,
	))
	if err != nil {
		return nil, fmt.Errorf("create hook config: %w", err)
	}
	return h, nil
}

// UpdateHookConfigParams holds optional fields for updating a hook config.
//
// ApplicableIngress nil → unchanged. Explicit empty slice is rejected at the
// handler boundary for the same reason as Create.
type UpdateHookConfigParams struct {
	Name              *string
	Type              *string
	ImplementationID  *string
	Stage             *string
	Category          *string
	Endpoint          *string
	Script            *string
	Config            json.RawMessage // nil = no change
	Priority          *int
	TimeoutMs         *int
	FailBehavior      *string
	Enabled           *bool
	ApplicableIngress []string // nil = no change
}

// UpdateHookConfig updates a hook config using COALESCE.
func (store *Store) UpdateHookConfig(ctx context.Context, id string, p UpdateHookConfigParams) (*HookConfig, error) {
	q := fmt.Sprintf(`UPDATE "HookConfig" SET
		name = COALESCE($2, name),
		type = COALESCE($3, type),
		"implementationId" = COALESCE($4, "implementationId"),
		stage = COALESCE($5, stage),
		category = COALESCE($6, category),
		endpoint = COALESCE($7, endpoint),
		script = COALESCE($8, script),
		config = COALESCE($9, config),
		priority = COALESCE($10, priority),
		"timeoutMs" = COALESCE($11, "timeoutMs"),
		"failBehavior" = COALESCE($12, "failBehavior"),
		enabled = COALESCE($13, enabled),
		"applicableIngress" = COALESCE($14::text[], "applicableIngress"),
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, hcColumns)

	h, err := scanHC(store.pool.QueryRow(ctx, q, id,
		p.Name, p.Type, p.ImplementationID, p.Stage, p.Category,
		p.Endpoint, p.Script, p.Config, p.Priority, p.TimeoutMs,
		p.FailBehavior, p.Enabled, p.ApplicableIngress))
	if err != nil {
		return nil, fmt.Errorf("update hook config: %w", err)
	}
	return h, nil
}

// DeleteHookConfig deletes a hook config by ID.
func (store *Store) DeleteHookConfig(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "HookConfig" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete hook config: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ReorderHooksByStage updates priorities for all hooks in a given stage.
func (store *Store) ReorderHooksByStage(ctx context.Context, stage string, ids []string) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Verify all IDs belong to the stage
	var count int
	err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM "HookConfig" WHERE stage = $1`, stage).Scan(&count)
	if err != nil {
		return fmt.Errorf("count hooks for stage: %w", err)
	}
	if count != len(ids) {
		return fmt.Errorf("must provide exactly %d hook IDs for stage %q, got %d", count, stage, len(ids))
	}

	for i, id := range ids {
		_, err := tx.Exec(ctx, `UPDATE "HookConfig" SET priority = $1, "updatedAt" = NOW() WHERE id = $2 AND stage = $3`,
			i, id, stage)
		if err != nil {
			return fmt.Errorf("update priority for hook %s: %w", id, err)
		}
	}

	return tx.Commit(ctx)
}
