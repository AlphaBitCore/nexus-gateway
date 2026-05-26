package iamstore

import (
	"context"
	"encoding/json"
	"fmt"

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
