package iamstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AddGroupMember adds a principal to a group.
func (store *Store) AddGroupMember(ctx context.Context, groupID, principalType, principalID string) (string, error) {
	var id string
	err := store.pool.QueryRow(ctx, `
		INSERT INTO "IamGroupMembership" (id, "groupId", "principalType", "principalId", "createdAt")
		VALUES (gen_random_uuid(), $1, $2, $3, NOW())
		ON CONFLICT ("groupId", "principalType", "principalId") DO UPDATE SET "groupId" = "IamGroupMembership"."groupId"
		RETURNING id
	`, groupID, principalType, principalID).Scan(&id)
	return id, err
}

// RemoveGroupMember removes a membership by ID.
func (store *Store) RemoveGroupMember(ctx context.Context, membershipID string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "IamGroupMembership" WHERE id = $1`, membershipID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// RemoveGroupMemberByPrincipal removes the membership for a specific principal from a group.
func (store *Store) RemoveGroupMemberByPrincipal(ctx context.Context, groupID, principalType, principalID string) error {
	_, err := store.pool.Exec(ctx, `
		DELETE FROM "IamGroupMembership"
		WHERE "groupId" = $1 AND "principalType" = $2 AND "principalId" = $3
	`, groupID, principalType, principalID)
	return err
}

// ListGroupMembersRaw returns member rows enriched with NexusUser display name.
// Used by SCIM to build the members array in group responses.
func (store *Store) ListGroupMembersRaw(ctx context.Context, groupID string) ([]map[string]string, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT m.id, m."principalId", COALESCE(u."displayName", m."principalId")
		FROM "IamGroupMembership" m
		LEFT JOIN "NexusUser" u ON u.id = m."principalId" AND m."principalType" = 'nexus_user'
		WHERE m."groupId" = $1
		ORDER BY m."createdAt" ASC
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var membershipID, userID, displayName string
		if err := rows.Scan(&membershipID, &userID, &displayName); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{
			"membershipId": membershipID,
			"userId":       userID,
			"displayName":  displayName,
		})
	}
	return out, rows.Err()
}

// GroupMemberRow represents a membership row.
type GroupMemberRow struct {
	ID            string `json:"id"`
	PrincipalType string `json:"principalType"`
	PrincipalID   string `json:"principalId"`
	CreatedAt     any    `json:"createdAt"`
}

// ListGroupMembers returns all memberships for a group.
func (store *Store) ListGroupMembers(ctx context.Context, groupID string) ([]GroupMemberRow, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id, "principalType", "principalId", "createdAt"
		FROM "IamGroupMembership"
		WHERE "groupId" = $1
		ORDER BY "createdAt" ASC
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []GroupMemberRow
	for rows.Next() {
		var m GroupMemberRow
		if err := rows.Scan(&m.ID, &m.PrincipalType, &m.PrincipalID, &m.CreatedAt); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	if members == nil {
		members = []GroupMemberRow{}
	}
	return members, rows.Err()
}

// ListGroupMembersPaginated returns paginated members for a group with total count.
func (store *Store) ListGroupMembersPaginated(ctx context.Context, groupID string, limit, offset int) ([]GroupMemberRow, int, error) {
	var total int
	err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM "IamGroupMembership" WHERE "groupId" = $1`, groupID).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count group members: %w", err)
	}

	rows, err := store.pool.Query(ctx, `
		SELECT m.id, m."principalType", m."principalId", m."createdAt"
		FROM "IamGroupMembership" m
		WHERE m."groupId" = $1
		ORDER BY m."createdAt" DESC
		LIMIT $2 OFFSET $3
	`, groupID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list group members paginated: %w", err)
	}
	defer rows.Close()
	var members []GroupMemberRow
	for rows.Next() {
		var m GroupMemberRow
		if err := rows.Scan(&m.ID, &m.PrincipalType, &m.PrincipalID, &m.CreatedAt); err != nil {
			return nil, 0, err
		}
		members = append(members, m)
	}
	if members == nil {
		members = []GroupMemberRow{}
	}
	return members, total, rows.Err()
}

// ListGroupNamesForPrincipal returns the IAM group names a principal belongs to.
func (store *Store) ListGroupNamesForPrincipal(ctx context.Context, principalType, principalID string) ([]string, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT g.name
		FROM "IamGroupMembership" m
		JOIN "IamGroup" g ON g.id = m."groupId"
		WHERE m."principalType" = $1 AND m."principalId" = $2
		ORDER BY g.name ASC
	`, principalType, principalID)
	if err != nil {
		return nil, err
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
	return names, rows.Err()
}

// GetGroupMembershipByID fetches the group + principal coordinates for a
// membership row, used to emit revocation events on removal. Returns
// pgx.ErrNoRows when the membership does not exist.
func (store *Store) GetGroupMembershipByID(ctx context.Context, membershipID string) (groupID, principalType, principalID string, err error) {
	err = store.pool.QueryRow(ctx, `
		SELECT "groupId", "principalType", "principalId"
		FROM "IamGroupMembership"
		WHERE id = $1
	`, membershipID).Scan(&groupID, &principalType, &principalID)
	if err != nil {
		return "", "", "", fmt.Errorf("get group membership: %w", err)
	}
	return groupID, principalType, principalID, nil
}
