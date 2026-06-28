package iamstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- IAM Groups ---

// GroupRow represents an IAM group.
type GroupRow struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	CreatedBy   *string   `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

const IamGroupColumns = `id, name, description, "createdBy", "createdAt", "updatedAt"`

// ListIamGroups returns IAM groups, capped at 1000.
func (store *Store) ListIamGroups(ctx context.Context) ([]GroupRow, error) {
	groups, _, err := store.ListIamGroupsPage(ctx, "", 1000, 0)
	return groups, err
}

// ListIamGroupsPage returns IAM groups with optional text search and pagination.
func (store *Store) ListIamGroupsPage(ctx context.Context, q string, limit, offset int) ([]GroupRow, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if q != "" {
		where += fmt.Sprintf(` AND (name ILIKE $%d OR description ILIKE $%d)`, argIdx, argIdx)
		args = append(args, "%"+escapeILIKE(q)+"%")
		argIdx++
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "IamGroup" %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := fmt.Sprintf(`SELECT %s FROM "IamGroup" %s ORDER BY "updatedAt" DESC, name ASC LIMIT $%d OFFSET $%d`,
		IamGroupColumns, where, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := store.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	groups := []GroupRow{}
	for rows.Next() {
		var g GroupRow
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, 0, err
		}
		groups = append(groups, g)
	}
	return groups, total, rows.Err()
}

// GetIamGroup returns a group by ID.
func (store *Store) GetIamGroup(ctx context.Context, id string) (*GroupRow, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM "IamGroup" WHERE id = $1`, IamGroupColumns), id)
	var g GroupRow
	err := row.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &g, err
}

// CreateIamGroup inserts a new group.
func (store *Store) CreateIamGroup(ctx context.Context, name string, description *string, createdBy string) (*GroupRow, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO "IamGroup" (id, name, description, "createdBy", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, NOW(), NOW())
		RETURNING %s
	`, IamGroupColumns), name, description, createdBy)
	var g GroupRow
	err := row.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt)
	return &g, err
}

// UpdateIamGroupParams holds optional fields for updating a group.
type UpdateIamGroupParams struct {
	Name        *string
	Description *string
}

// UpdateIamGroup updates a group using COALESCE.
func (store *Store) UpdateIamGroup(ctx context.Context, id string, p UpdateIamGroupParams) (*GroupRow, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`UPDATE "IamGroup" SET
		name = COALESCE($2, name), description = COALESCE($3, description), "updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, IamGroupColumns), id, p.Name, p.Description)
	var g GroupRow
	err := row.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt)
	return &g, err
}

// DeleteIamGroup deletes a group (cascade).
func (store *Store) DeleteIamGroup(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "IamGroup" WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
