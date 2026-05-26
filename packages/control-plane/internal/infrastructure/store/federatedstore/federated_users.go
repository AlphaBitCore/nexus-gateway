package federatedstore

import (
	"context"
	"fmt"
)

// ListUserIDsByIdPType returns NexusUser IDs federated to any enabled
// IdentityProvider of the given type (e.g. "oidc" or "saml"). The
// per-IdP DELETE / disable flow uses ListUserIDsByIdP instead
// (idP-id-scoped, not type-scoped). Kept for any future "kick everyone
// on type X" admin operation.
func (store *Store) ListUserIDsByIdPType(ctx context.Context, idpType string) ([]string, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT DISTINCT ufi."userId"
		FROM "UserFederatedIdentity" ufi
		JOIN "IdentityProvider" idp ON idp.id = ufi."idpId"
		WHERE idp.type = $1 AND idp.enabled = TRUE
	`, idpType)
	if err != nil {
		return nil, fmt.Errorf("list user ids by idp type: %w", err)
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

// ListUserIDsByIdP returns NexusUser IDs linked to a specific
// IdentityProvider row, regardless of the row's enabled state. Used by
// the admin IdP delete / disable handlers to snapshot the set of
// users whose sessions need revoking BEFORE the cascade clears the
// UserFederatedIdentity rows.
//
// Note: the result is a snapshot — callers must invoke this before
// running any cascade that would clear the federated links. Once
// cleared, the membership is gone and revocation fan-out cannot
// reconstruct it.
func (store *Store) ListUserIDsByIdP(ctx context.Context, idpID string) ([]string, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT DISTINCT "userId"
		FROM "UserFederatedIdentity"
		WHERE "idpId" = $1
	`, idpID)
	if err != nil {
		return nil, fmt.Errorf("list user ids by idp: %w", err)
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
