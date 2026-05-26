package orgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Project represents a row from the Project table.
type Project struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Code           string    `json:"code"`
	OrganizationID string    `json:"organizationId"`
	Description    *string   `json:"description"`
	ContactName    *string   `json:"contactName"`
	ContactEmail   *string   `json:"contactEmail"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

const projectColumns = `id, name, code, "organizationId", description, "contactName", "contactEmail", status, "createdAt", "updatedAt"`

// ProjectListParams holds filter/pagination for listing projects.
type ProjectListParams struct {
	Q              string
	Status         string
	OrganizationID string
	Limit          int
	Offset         int
}

// ListProjects returns paginated projects with total count.
func (store *Store) ListProjects(ctx context.Context, p ProjectListParams) ([]Project, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	i := 1

	if p.Q != "" {
		where += fmt.Sprintf(` AND (name ILIKE $%d OR code ILIKE $%d)`, i, i+1)
		q := "%" + p.Q + "%"
		args = append(args, q, q)
		i += 2
	}
	if p.Status != "" {
		where += fmt.Sprintf(` AND status = $%d`, i)
		args = append(args, p.Status)
		i++
	}
	if p.OrganizationID != "" {
		where += fmt.Sprintf(` AND "organizationId" = $%d`, i)
		args = append(args, p.OrganizationID)
		i++
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "Project" %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count projects: %w", err)
	}

	limit := p.Limit
	if limit <= 0 {
		limit = 20
	}
	args = append(args, limit, p.Offset)
	rows, err := store.pool.Query(ctx, fmt.Sprintf(
		`SELECT %s FROM "Project" %s ORDER BY "updatedAt" DESC, name ASC LIMIT $%d OFFSET $%d`,
		projectColumns, where, i, i+1,
	), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var pr Project
		if err := rows.Scan(&pr.ID, &pr.Name, &pr.Code, &pr.OrganizationID,
			&pr.Description, &pr.ContactName, &pr.ContactEmail, &pr.Status,
			&pr.CreatedAt, &pr.UpdatedAt); err != nil {
			return nil, 0, err
		}
		projects = append(projects, pr)
	}
	return projects, total, rows.Err()
}

// GetProject returns a project by ID.
func (store *Store) GetProject(ctx context.Context, id string) (*Project, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM "Project" WHERE id = $1`, projectColumns), id)
	var pr Project
	err := row.Scan(&pr.ID, &pr.Name, &pr.Code, &pr.OrganizationID,
		&pr.Description, &pr.ContactName, &pr.ContactEmail, &pr.Status,
		&pr.CreatedAt, &pr.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pr, nil
}

// CreateProject inserts a new project.
func (store *Store) CreateProject(ctx context.Context, updates map[string]any) (*Project, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO "Project" (id, name, code, "organizationId", description, "contactName", "contactEmail", status, "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		RETURNING %s
	`, projectColumns),
		updates["name"], updates["code"], updates["organizationId"],
		updates["description"], updates["contactName"], updates["contactEmail"],
		updates["status"])

	var pr Project
	if err := row.Scan(&pr.ID, &pr.Name, &pr.Code, &pr.OrganizationID,
		&pr.Description, &pr.ContactName, &pr.ContactEmail, &pr.Status,
		&pr.CreatedAt, &pr.UpdatedAt); err != nil {
		return nil, fmt.Errorf("create project: %w", err)
	}
	return &pr, nil
}

// UpdateProjectParams holds optional fields for updating a project.
type UpdateProjectParams struct {
	Name           *string
	Code           *string
	OrganizationID *string
	Description    *string
	ContactName    *string
	ContactEmail   *string
	Status         *string
}

// UpdateProject updates a project using COALESCE.
func (store *Store) UpdateProject(ctx context.Context, id string, p UpdateProjectParams) (*Project, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE "Project" SET
			name = COALESCE($2, name),
			code = COALESCE($3, code),
			"organizationId" = COALESCE($4, "organizationId"),
			description = COALESCE($5, description),
			"contactName" = COALESCE($6, "contactName"),
			"contactEmail" = COALESCE($7, "contactEmail"),
			status = COALESCE($8, status),
			"updatedAt" = NOW()
		WHERE id = $1
		RETURNING %s
	`, projectColumns),
		id, p.Name, p.Code, p.OrganizationID, p.Description, p.ContactName, p.ContactEmail, p.Status)

	var pr Project
	if err := row.Scan(&pr.ID, &pr.Name, &pr.Code, &pr.OrganizationID,
		&pr.Description, &pr.ContactName, &pr.ContactEmail, &pr.Status,
		&pr.CreatedAt, &pr.UpdatedAt); err != nil {
		return nil, fmt.Errorf("update project: %w", err)
	}
	return &pr, nil
}

// DeleteProject deletes a project by ID.
func (store *Store) DeleteProject(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "Project" WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// CountProjectVirtualKeys returns the number of virtual keys belonging to a project.
func (store *Store) CountProjectVirtualKeys(ctx context.Context, projectID string) (int, error) {
	var count int
	err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM "VirtualKey" WHERE "projectId" = $1`, projectID).Scan(&count)
	return count, err
}
