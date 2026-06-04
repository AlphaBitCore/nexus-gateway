package iamstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GroupPolicyRow represents a group policy attachment.
type GroupPolicyRow struct {
	ID         string `json:"id"`
	PolicyID   string `json:"policyId"`
	PolicyName string `json:"policyName"`
	CreatedAt  any    `json:"createdAt"`
}

// ListGroupPolicies returns all policy attachments for a group.
func (store *Store) ListGroupPolicies(ctx context.Context, groupID string) ([]GroupPolicyRow, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT gpa.id, gpa."policyId", p.name, gpa."createdAt"
		FROM "IamGroupPolicyAttachment" gpa
		JOIN "IamPolicy" p ON p.id = gpa."policyId"
		WHERE gpa."groupId" = $1
		ORDER BY p.name ASC
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var policies []GroupPolicyRow
	for rows.Next() {
		var p GroupPolicyRow
		if err := rows.Scan(&p.ID, &p.PolicyID, &p.PolicyName, &p.CreatedAt); err != nil {
			return nil, err
		}
		policies = append(policies, p)
	}
	if policies == nil {
		policies = []GroupPolicyRow{}
	}
	return policies, rows.Err()
}

// AttachGroupPolicy attaches a policy to a group.
func (store *Store) AttachGroupPolicy(ctx context.Context, groupID, policyID string) (string, error) {
	var id string
	err := store.pool.QueryRow(ctx, `
		INSERT INTO "IamGroupPolicyAttachment" (id, "groupId", "policyId", "createdAt")
		VALUES (gen_random_uuid(), $1, $2, NOW())
		ON CONFLICT ("groupId", "policyId") DO UPDATE SET "groupId" = "IamGroupPolicyAttachment"."groupId"
		RETURNING id
	`, groupID, policyID).Scan(&id)
	return id, err
}

// DetachGroupPolicy removes a group policy attachment.
func (store *Store) DetachGroupPolicy(ctx context.Context, attachmentID string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "IamGroupPolicyAttachment" WHERE id = $1`, attachmentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// PrincipalPolicyAttachment is returned by ListPrincipalPolicyAttachments.
type PrincipalPolicyAttachment struct {
	ID            string  `json:"id"`
	PrincipalType string  `json:"principalType,omitempty"`
	PrincipalID   string  `json:"principalId,omitempty"`
	PolicyID      string  `json:"policyId"`
	PolicyName    string  `json:"policyName,omitempty"`
	Source        string  `json:"source"`    // "direct" or "group"
	GroupID       *string `json:"groupId"`   // set when source="group"
	GroupName     *string `json:"groupName"` // set when source="group"
	CreatedAt     any     `json:"createdAt"`
}

// ListPrincipalPolicyAttachments returns all policy attachments for a principal,
// including both direct and group-inherited ones.
func (store *Store) ListPrincipalPolicyAttachments(ctx context.Context, principalType, principalID string) ([]PrincipalPolicyAttachment, error) {
	var result []PrincipalPolicyAttachment

	// 1. Direct attachments
	rows, err := store.pool.Query(ctx, `
		SELECT a.id, a."policyId", p.name, a."createdAt"
		FROM "IamPolicyAttachment" a
		JOIN "IamPolicy" p ON p.id = a."policyId"
		WHERE a."principalType" = $1 AND a."principalId" = $2
		ORDER BY p.name ASC
	`, principalType, principalID)
	if err != nil {
		return nil, fmt.Errorf("list direct policy attachments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a PrincipalPolicyAttachment
		if err := rows.Scan(&a.ID, &a.PolicyID, &a.PolicyName, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.PrincipalType = principalType
		a.PrincipalID = principalID
		a.Source = "direct"
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 2. Group-inherited attachments (via IamGroupPolicyAttachment, not IamPolicyAttachment)
	rows2, err := store.pool.Query(ctx, `
		SELECT gpa.id, gpa."policyId", p.name, g.id AS group_id, g.name AS group_name, gpa."createdAt"
		FROM "IamGroupMembership" m
		JOIN "IamGroup" g ON g.id = m."groupId"
		JOIN "IamGroupPolicyAttachment" gpa ON gpa."groupId" = g.id
		JOIN "IamPolicy" p ON p.id = gpa."policyId"
		WHERE m."principalType" = $1 AND m."principalId" = $2
		ORDER BY g.name, p.name ASC
	`, principalType, principalID)
	if err != nil {
		return nil, fmt.Errorf("list group policy attachments: %w", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var a PrincipalPolicyAttachment
		var groupID, groupName string
		if err := rows2.Scan(&a.ID, &a.PolicyID, &a.PolicyName, &groupID, &groupName, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.PrincipalType = principalType
		a.PrincipalID = principalID
		a.Source = "group"
		a.GroupID = &groupID
		a.GroupName = &groupName
		result = append(result, a)
	}
	if err := rows2.Err(); err != nil {
		return nil, err
	}

	if result == nil {
		result = []PrincipalPolicyAttachment{}
	}
	return result, nil
}

// PolicyGroupRow is a group that has a given policy attached.
type PolicyGroupRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListGroupsForPolicy returns all groups that have policyID attached via IamGroupPolicyAttachment.
func (store *Store) ListGroupsForPolicy(ctx context.Context, policyID string) ([]PolicyGroupRow, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT g.id, g.name
		FROM "IamGroupPolicyAttachment" gpa
		JOIN "IamGroup" g ON g.id = gpa."groupId"
		WHERE gpa."policyId" = $1
		ORDER BY g.name ASC
	`, policyID)
	if err != nil {
		return nil, fmt.Errorf("list groups for policy: %w", err)
	}
	defer rows.Close()
	var result []PolicyGroupRow
	for rows.Next() {
		var g PolicyGroupRow
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		result = append(result, g)
	}
	if result == nil {
		result = []PolicyGroupRow{}
	}
	return result, rows.Err()
}

// DirectPolicyAttachmentRow is a direct principal→policy binding.
type DirectPolicyAttachmentRow struct {
	PrincipalType string `json:"principalType"`
	PrincipalID   string `json:"principalId"`
}

// ListDirectPolicyAttachments returns all direct principal attachments for a policy.
func (store *Store) ListDirectPolicyAttachments(ctx context.Context, policyID string) ([]DirectPolicyAttachmentRow, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT a."principalType", a."principalId"
		FROM "IamPolicyAttachment" a
		WHERE a."policyId" = $1
		ORDER BY a."principalType", a."createdAt" DESC
	`, policyID)
	if err != nil {
		return nil, fmt.Errorf("list direct policy attachments: %w", err)
	}
	defer rows.Close()
	var result []DirectPolicyAttachmentRow
	for rows.Next() {
		var r DirectPolicyAttachmentRow
		if err := rows.Scan(&r.PrincipalType, &r.PrincipalID); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if result == nil {
		result = []DirectPolicyAttachmentRow{}
	}
	return result, rows.Err()
}

// ListPolicyNamesForPrincipal returns the names of all enabled policies
// for a principal (direct + group-inherited), deduplicated and sorted.
func (store *Store) ListPolicyNamesForPrincipal(ctx context.Context, principalType, principalID string) ([]string, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT DISTINCT p.name FROM (
			-- Direct attachments
			SELECT a."policyId"
			FROM "IamPolicyAttachment" a
			WHERE a."principalType" = $1 AND a."principalId" = $2
			UNION
			-- Group-inherited (via IamGroupPolicyAttachment)
			SELECT gpa."policyId"
			FROM "IamGroupMembership" m
			JOIN "IamGroupPolicyAttachment" gpa ON gpa."groupId" = m."groupId"
			WHERE m."principalType" = $1 AND m."principalId" = $2
		) AS all_policies
		JOIN "IamPolicy" p ON p.id = all_policies."policyId" AND p.enabled = true
		ORDER BY p.name ASC
	`, principalType, principalID)
	if err != nil {
		return nil, fmt.Errorf("list policy names for principal: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if names == nil {
		names = []string{}
	}
	return names, rows.Err()
}

// AttachPrincipalPolicy attaches a policy directly to a principal.
// expiresAt is optional — nil means permanent (today's default);
// non-nil scopes the attachment to a window after which
// Engine.loadPolicies's SQL filter drops the policy from the
// principal's effective set. Cache convergence is bounded by L2 TTL
// (60s default) — operators wanting instant revocation should call
// PolicyCache.Invalidate. ON CONFLICT updates expires_at on the
// existing row so callers can extend a time-bounded grant by
// re-POSTing without dropping/re-creating.
func (store *Store) AttachPrincipalPolicy(ctx context.Context, principalType, principalID, policyID string, expiresAt *time.Time) (string, error) {
	var id string
	err := store.pool.QueryRow(ctx, `
		INSERT INTO "IamPolicyAttachment" (id, "principalType", "principalId", "policyId", "createdAt", expires_at)
		VALUES (gen_random_uuid(), $1, $2, $3, NOW(), $4)
		ON CONFLICT ("principalType", "principalId", "policyId") DO UPDATE SET expires_at = EXCLUDED.expires_at
		RETURNING id
	`, principalType, principalID, policyID, expiresAt).Scan(&id)
	return id, err
}

// DetachPrincipalPolicy removes a principal policy attachment.
func (store *Store) DetachPrincipalPolicy(ctx context.Context, attachmentID string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "IamPolicyAttachment" WHERE id = $1`, attachmentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// GetPrincipalPolicyAttachmentByID fetches the principal + policy coordinates
// for a direct attachment, used to emit revocation events on detach. Returns
// pgx.ErrNoRows when the attachment does not exist.
func (store *Store) GetPrincipalPolicyAttachmentByID(ctx context.Context, attachmentID string) (principalType, principalID, policyID string, err error) {
	err = store.pool.QueryRow(ctx, `
		SELECT "principalType", "principalId", "policyId"
		FROM "IamPolicyAttachment"
		WHERE id = $1
	`, attachmentID).Scan(&principalType, &principalID, &policyID)
	if err != nil {
		return "", "", "", fmt.Errorf("get principal policy attachment: %w", err)
	}
	return principalType, principalID, policyID, nil
}

// GetGroupPolicyAttachmentByID fetches the group + policy coordinates for a
// group policy attachment, used to fan out revocations on detach. Returns
// pgx.ErrNoRows when the attachment does not exist.
func (store *Store) GetGroupPolicyAttachmentByID(ctx context.Context, attachmentID string) (groupID, policyID string, err error) {
	err = store.pool.QueryRow(ctx, `
		SELECT "groupId", "policyId"
		FROM "IamGroupPolicyAttachment"
		WHERE id = $1
	`, attachmentID).Scan(&groupID, &policyID)
	if err != nil {
		return "", "", fmt.Errorf("get group policy attachment: %w", err)
	}
	return groupID, policyID, nil
}

// ListPolicyAttachedUserIDs returns the set of nexus_user principal IDs that
// have this policy attached either directly (IamPolicyAttachment) or through
// group membership (IamGroupMembership -> IamGroupPolicyAttachment). Used by
// UpdateIAMPolicy to fan out revocations when a policy body changes so every
// affected user sees the new permissions on their next token mint.
func (store *Store) ListPolicyAttachedUserIDs(ctx context.Context, policyID string) ([]string, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT DISTINCT "principalId" FROM (
			SELECT a."principalId"
			FROM "IamPolicyAttachment" a
			WHERE a."policyId" = $1 AND a."principalType" = 'nexus_user'
			UNION
			SELECT m."principalId"
			FROM "IamGroupMembership" m
			JOIN "IamGroupPolicyAttachment" gpa ON gpa."groupId" = m."groupId"
			WHERE gpa."policyId" = $1 AND m."principalType" = 'nexus_user'
		) AS affected
	`, policyID)
	if err != nil {
		return nil, fmt.Errorf("list policy attached user ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, rows.Err()
}
