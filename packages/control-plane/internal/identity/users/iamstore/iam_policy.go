package iamstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
)

// LoadPolicies implements iam.PolicyLoader. It loads all IAM policies
// for a principal (direct attachments + group-inherited).
func (store *Store) LoadPolicies(ctx context.Context, principalType, principalID string) ([]iam.LoadedPolicy, error) {
	var policies []iam.LoadedPolicy

	// 1. Direct policy attachments.
	// Filter expired attachments (`expires_at <= NOW()`).
	// NULL expires_at is permanent and continues to match.
	rows, err := store.pool.Query(ctx, `
		SELECT p.id, p.name, p.document
		FROM "IamPolicyAttachment" a
		JOIN "IamPolicy" p ON p.id = a."policyId"
		WHERE a."principalType" = $1
		  AND a."principalId" = $2
		  AND p.enabled = true
		  AND (a.expires_at IS NULL OR a.expires_at > NOW())
	`, principalType, principalID)
	if err != nil {
		return nil, fmt.Errorf("load direct policies: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, name string
		var docBytes []byte
		if err := rows.Scan(&id, &name, &docBytes); err != nil {
			return nil, fmt.Errorf("scan direct policy: %w", err)
		}
		var doc iam.PolicyDocument
		if err := json.Unmarshal(docBytes, &doc); err != nil {
			continue // skip malformed policies
		}
		policies = append(policies, iam.LoadedPolicy{
			ID: id, Name: name, Document: doc, Source: "direct",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate direct policies: %w", err)
	}

	// 2. Group-inherited policies
	rows2, err := store.pool.Query(ctx, `
		SELECT p.id, p.name, p.document, g.name
		FROM "IamGroupMembership" m
		JOIN "IamGroup" g ON g.id = m."groupId"
		JOIN "IamGroupPolicyAttachment" gpa ON gpa."groupId" = g.id
		JOIN "IamPolicy" p ON p.id = gpa."policyId"
		WHERE m."principalType" = $1 AND m."principalId" = $2 AND p.enabled = true
	`, principalType, principalID)
	if err != nil {
		return nil, fmt.Errorf("load group policies: %w", err)
	}
	defer rows2.Close()

	seen := make(map[string]bool)
	for _, p := range policies {
		seen[p.ID] = true
	}

	for rows2.Next() {
		var id, name, groupName string
		var docBytes []byte
		if err := rows2.Scan(&id, &name, &docBytes, &groupName); err != nil {
			return nil, fmt.Errorf("scan group policy: %w", err)
		}
		if seen[id] {
			continue // skip duplicates
		}
		seen[id] = true
		var doc iam.PolicyDocument
		if err := json.Unmarshal(docBytes, &doc); err != nil {
			continue
		}
		policies = append(policies, iam.LoadedPolicy{
			ID: id, Name: name, Document: doc, Source: "group", GroupName: groupName,
		})
	}
	if err := rows2.Err(); err != nil {
		return nil, fmt.Errorf("iterate group policies: %w", err)
	}

	return policies, nil
}

// PolicyRow represents a row from the IamPolicy table.
type PolicyRow struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description *string         `json:"description"`
	Type        string          `json:"type"`
	Document    json.RawMessage `json:"document"`
	Enabled     bool            `json:"enabled"`
	CreatedBy   *string         `json:"createdBy"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

const iamPolicyColumns = `id, name, description, type, document, enabled, "createdBy", "createdAt", "updatedAt"`

// ListIamPolicies returns IAM policies with optional filtering.
func (store *Store) ListIamPolicies(ctx context.Context, q, typeFilter string, enabled *bool, limit, offset int) ([]PolicyRow, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if q != "" {
		where += fmt.Sprintf(` AND (name ILIKE $%d OR description ILIKE $%d)`, argIdx, argIdx)
		args = append(args, "%"+escapeILIKE(q)+"%")
		argIdx++
	}
	if typeFilter != "" {
		where += fmt.Sprintf(` AND type = $%d`, argIdx)
		args = append(args, typeFilter)
		argIdx++
	}
	if enabled != nil {
		where += fmt.Sprintf(` AND enabled = $%d`, argIdx)
		args = append(args, *enabled)
		argIdx++
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "IamPolicy" %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := fmt.Sprintf(`SELECT %s FROM "IamPolicy" %s ORDER BY "updatedAt" DESC, name ASC LIMIT $%d OFFSET $%d`,
		iamPolicyColumns, where, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := store.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	policies := []PolicyRow{}
	for rows.Next() {
		var p PolicyRow
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Type, &p.Document, &p.Enabled, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, 0, err
		}
		policies = append(policies, p)
	}
	return policies, total, rows.Err()
}

// GetIamPolicy returns a policy by ID.
func (store *Store) GetIamPolicy(ctx context.Context, id string) (*PolicyRow, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM "IamPolicy" WHERE id = $1`, iamPolicyColumns), id)
	var p PolicyRow
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Type, &p.Document, &p.Enabled, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// CreateIamPolicy inserts a new IAM policy.
func (store *Store) CreateIamPolicy(ctx context.Context, name string, description *string, document json.RawMessage, createdBy string) (*PolicyRow, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO "IamPolicy" (id, name, description, type, document, "createdBy", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, 'custom', $3, $4, NOW(), NOW())
		RETURNING %s
	`, iamPolicyColumns), name, description, document, createdBy)
	var p PolicyRow
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Type, &p.Document, &p.Enabled, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	return &p, err
}

// UpdateIamPolicyParams holds optional fields for updating an IAM policy.
type UpdateIamPolicyParams struct {
	Name        *string
	Description *string
	Document    json.RawMessage // nil = no change
	Enabled     *bool
}

// UpdateIamPolicy updates an IAM policy using COALESCE.
func (store *Store) UpdateIamPolicy(ctx context.Context, id string, p UpdateIamPolicyParams) (*PolicyRow, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`UPDATE "IamPolicy" SET
		name = COALESCE($2, name), description = COALESCE($3, description),
		document = COALESCE($4, document), enabled = COALESCE($5, enabled),
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, iamPolicyColumns),
		id, p.Name, p.Description, p.Document, p.Enabled)
	var pol PolicyRow
	err := row.Scan(&pol.ID, &pol.Name, &pol.Description, &pol.Type, &pol.Document, &pol.Enabled, &pol.CreatedBy, &pol.CreatedAt, &pol.UpdatedAt)
	return &pol, err
}

// DeleteIamPolicy deletes an IAM policy.
func (store *Store) DeleteIamPolicy(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "IamPolicy" WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
