package orgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Organization represents a row from the Organization table.
type Organization struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Code     string  `json:"code"`
	ParentID *string `json:"parentId"`
	// Path is the materialized path for efficient subtree queries.
	// Root orgs: /{id}/. Children: {parent.path}{id}/.
	Path         string  `json:"path"`
	Description  *string `json:"description"`
	ContactName  *string `json:"contactName"`
	ContactEmail *string `json:"contactEmail"`
	ContactPhone *string `json:"contactPhone"`
	Enabled      bool    `json:"enabled"`
	// Timezone is the org's IANA TZ for business-rule semantics
	// (daily-quota reset, "yesterday", business hours). Display
	// formatting uses NexusUser.PreferredTimezone instead.
	Timezone string `json:"timezone"`
	// Source indicates how the org was provisioned: "local" | "idp".
	Source string `json:"source"`
	// ExternalGroupID is the IdP group identifier for IdP-provisioned orgs (SCIM).
	ExternalGroupID *string   `json:"externalGroupId,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	ChildCount      *int      `json:"childCount,omitempty"`
	ProjectCount    *int      `json:"projectCount,omitempty"`
	UserCount       *int      `json:"userCount,omitempty"`
}

const orgColumns = `id, name, code, "parentId", path, description, "contactName", "contactEmail",
	"contactPhone", enabled, timezone, source, "externalGroupId", "createdAt", "updatedAt"`

// ListOrganizations returns all organizations with child/project/user counts (single query, no N+1).
func (store *Store) ListOrganizations(ctx context.Context) ([]Organization, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT o.id, o.name, o.code, o."parentId", o.path, o.description, o."contactName", o."contactEmail",
			o."contactPhone", o.enabled, o.timezone, o.source, o."externalGroupId", o."createdAt", o."updatedAt",
			COALESCE(cc.cnt, 0) AS child_count,
			COALESCE(pc.cnt, 0) AS project_count,
			COALESCE(uc.cnt, 0) AS user_count
		FROM "Organization" o
		LEFT JOIN (SELECT "parentId", COUNT(*) AS cnt FROM "Organization" GROUP BY "parentId") cc ON cc."parentId" = o.id
		LEFT JOIN (SELECT "organizationId", COUNT(*) AS cnt FROM "Project" GROUP BY "organizationId") pc ON pc."organizationId" = o.id
		LEFT JOIN (SELECT "organizationId", COUNT(*) AS cnt FROM "NexusUser" WHERE "organizationId" IS NOT NULL GROUP BY "organizationId") uc ON uc."organizationId" = o.id
		ORDER BY o."updatedAt" DESC, o.name ASC
		LIMIT 1000
	`)
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	defer rows.Close()

	orgs := []Organization{}
	for rows.Next() {
		var o Organization
		var cc, pc, uc int
		if err := rows.Scan(&o.ID, &o.Name, &o.Code, &o.ParentID, &o.Path, &o.Description,
			&o.ContactName, &o.ContactEmail, &o.ContactPhone, &o.Enabled, &o.Timezone,
			&o.Source, &o.ExternalGroupID, &o.CreatedAt, &o.UpdatedAt, &cc, &pc, &uc); err != nil {
			return nil, err
		}
		o.ChildCount = &cc
		o.ProjectCount = &pc
		o.UserCount = &uc
		orgs = append(orgs, o)
	}
	return orgs, rows.Err()
}

// ListChildOrganizations returns direct children of an organization.
func (store *Store) ListChildOrganizations(ctx context.Context, parentID string) ([]Organization, error) {
	rows, err := store.pool.Query(ctx, fmt.Sprintf(`SELECT %s FROM "Organization" WHERE "parentId" = $1 ORDER BY name ASC`, orgColumns), parentID)
	if err != nil {
		return nil, fmt.Errorf("list child organizations: %w", err)
	}
	defer rows.Close()

	var orgs []Organization
	for rows.Next() {
		var o Organization
		if err := rows.Scan(&o.ID, &o.Name, &o.Code, &o.ParentID, &o.Path, &o.Description,
			&o.ContactName, &o.ContactEmail, &o.ContactPhone, &o.Enabled, &o.Timezone,
			&o.Source, &o.ExternalGroupID, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		orgs = append(orgs, o)
	}
	return orgs, rows.Err()
}

// GetOrganization returns an organization by ID.
func (store *Store) GetOrganization(ctx context.Context, id string) (*Organization, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM "Organization" WHERE id = $1`, orgColumns), id)
	var o Organization
	err := row.Scan(&o.ID, &o.Name, &o.Code, &o.ParentID, &o.Path, &o.Description,
		&o.ContactName, &o.ContactEmail, &o.ContactPhone, &o.Enabled, &o.Timezone,
		&o.Source, &o.ExternalGroupID, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// CreateOrganization inserts a new organization and computes its materialized path.
// If parentId is provided the path is {parent.path}{newId}/; for root orgs it is /{newId}/.
func (store *Store) CreateOrganization(ctx context.Context, updates map[string]any) (*Organization, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Insert with empty path placeholder (NOT NULL DEFAULT '' allows this temporarily).
	row := tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO "Organization" (id, name, code, "parentId", description, "contactName", "contactEmail", "contactPhone", enabled, timezone, path, "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, COALESCE($8::boolean, true), COALESCE($9::text, 'UTC'), '', NOW(), NOW())
		RETURNING %s
	`, orgColumns),
		updates["name"], updates["code"], updates["parentId"],
		updates["description"], updates["contactName"], updates["contactEmail"], updates["contactPhone"],
		updates["enabled"], updates["timezone"])

	var o Organization
	if err := row.Scan(&o.ID, &o.Name, &o.Code, &o.ParentID, &o.Path, &o.Description,
		&o.ContactName, &o.ContactEmail, &o.ContactPhone, &o.Enabled, &o.Timezone,
		&o.Source, &o.ExternalGroupID, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return nil, fmt.Errorf("create organization: %w", err)
	}

	// Compute and set path based on parent.
	var computedPath string
	if o.ParentID == nil {
		computedPath = "/" + o.ID + "/"
	} else {
		var parentPath string
		if err := tx.QueryRow(ctx, `SELECT path FROM "Organization" WHERE id = $1`, *o.ParentID).Scan(&parentPath); err != nil {
			return nil, fmt.Errorf("get parent path: %w", err)
		}
		computedPath = parentPath + o.ID + "/"
	}

	if _, err := tx.Exec(ctx, `UPDATE "Organization" SET path = $1 WHERE id = $2`, computedPath, o.ID); err != nil {
		return nil, fmt.Errorf("set org path: %w", err)
	}
	o.Path = computedPath

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &o, nil
}

// UpdateOrganizationParams holds optional fields for updating an organization.
type UpdateOrganizationParams struct {
	Name         *string
	Code         *string
	ParentID     *string
	Description  *string
	ContactName  *string
	ContactEmail *string
	ContactPhone *string
	Enabled      *bool
	// Timezone updates the org's IANA TZ for business-rule semantics.
	// Pass nil to leave unchanged.
	Timezone *string
}

// UpdateOrganization updates an organization using COALESCE.
// When ParentID changes, the path for this org and all descendants is recomputed atomically.
func (store *Store) UpdateOrganization(ctx context.Context, id string, p UpdateOrganizationParams) (*Organization, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Fetch current org to detect parentId change.
	var currentPath string
	var currentParentID *string
	if err := tx.QueryRow(ctx, `SELECT path, "parentId" FROM "Organization" WHERE id = $1`, id).
		Scan(&currentPath, &currentParentID); err != nil {
		return nil, fmt.Errorf("fetch current org: %w", err)
	}

	// For parentId: NULL means "don't change", "" means "move to root" (set DB col to NULL),
	// any other value means "reparent to that ID".
	row := tx.QueryRow(ctx, fmt.Sprintf(`UPDATE "Organization" SET
		name = COALESCE($2, name), code = COALESCE($3, code),
		"parentId" = CASE WHEN $4::text IS NULL THEN "parentId" WHEN $4::text = '' THEN NULL ELSE $4::text END,
		description = COALESCE($5, description),
		"contactName" = COALESCE($6, "contactName"), "contactEmail" = COALESCE($7, "contactEmail"),
		"contactPhone" = COALESCE($8, "contactPhone"), enabled = COALESCE($9, enabled),
		timezone = COALESCE($10, timezone),
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, orgColumns),
		id, p.Name, p.Code, p.ParentID, p.Description, p.ContactName, p.ContactEmail, p.ContactPhone, p.Enabled, p.Timezone)

	var o Organization
	if err := row.Scan(&o.ID, &o.Name, &o.Code, &o.ParentID, &o.Path, &o.Description,
		&o.ContactName, &o.ContactEmail, &o.ContactPhone, &o.Enabled, &o.Timezone,
		&o.Source, &o.ExternalGroupID, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return nil, fmt.Errorf("update organization: %w", err)
	}

	// Cascade path recomputation when parentId changed.
	parentChanged := p.ParentID != nil && !ptrEq(p.ParentID, currentParentID)
	if parentChanged {
		var newParentPath string
		if *p.ParentID == "" {
			// Moving to root.
			newParentPath = "/"
		} else {
			if err := tx.QueryRow(ctx, `SELECT path FROM "Organization" WHERE id = $1`, *p.ParentID).
				Scan(&newParentPath); err != nil {
				return nil, fmt.Errorf("get new parent path: %w", err)
			}
		}
		newPath := newParentPath + id + "/"

		// Update this org and all descendants: replace the old path prefix with new path.
		if _, err := tx.Exec(ctx, `
			UPDATE "Organization"
			SET path = $1 || SUBSTRING(path FROM LENGTH($2) + 1)
			WHERE path LIKE $2 || '%'
		`, newPath, currentPath); err != nil {
			return nil, fmt.Errorf("cascade path update: %w", err)
		}
		o.Path = newPath
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &o, nil
}

// DeleteOrganization deletes an organization (must have no children or projects).
func (store *Store) DeleteOrganization(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "Organization" WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// CountOrgDependents returns child org count and project count.
func (store *Store) CountOrgDependents(ctx context.Context, id string) (children, projects int, err error) {
	_ = store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM "Organization" WHERE "parentId" = $1`, id).Scan(&children)
	_ = store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM "Project" WHERE "organizationId" = $1`, id).Scan(&projects)
	return
}

// ptrEq returns true when both pointers are nil or both point to the same string value.
func ptrEq(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
