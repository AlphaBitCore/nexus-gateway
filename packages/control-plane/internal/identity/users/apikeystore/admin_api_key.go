package apikeystore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// APIKeyWithOwner holds an API key record together with its optional owner
// NexusUser, used by the authentication middleware to resolve the effective
// principal for API-key-based requests.
type APIKeyWithOwner struct {
	ID          string
	Name        string
	Enabled     bool
	Status      string
	OwnerUserID *string
	// Owner user fields (nil if no owner)
	OwnerID          *string
	OwnerDisplayName *string
	OwnerEnabled     *bool // derived from u.status = 'active' AND u."canAccessControlPlane"
}

// FindAPIKeyByHash looks up an AdminApiKey by its HMAC-SHA256 hash,
// including the optional owner NexusUser for delegation. The auth middleware
// treats Enabled as the combined "key may be used right now" signal — this
// helper folds (a) the operator's enabled boolean, (b) expiry, and (c) the
// lifecycle status into that single field so the middleware stays simple.
//
// Accepted lifecycle states: 'active', 'rotating'. 'expired' and
// 'unavailable' keys are surfaced with Enabled=false so the middleware
// rejects them. The raw Status is preserved on the returned struct for
// audit / observability callers that care about the distinction.
func (store *Store) FindAPIKeyByHash(ctx context.Context, keyHash string) (*APIKeyWithOwner, error) {
	row := store.pool.QueryRow(ctx, `
		SELECT k.id, k.name, k.enabled, k.status, k."expiresAt", k."ownerUserId",
		       u.id, u."displayName", (u.status = 'active' AND u."canAccessControlPlane")
		FROM "AdminApiKey" k
		LEFT JOIN "NexusUser" u ON u.id = k."ownerUserId"
		WHERE k."keyHash" = $1
	`, keyHash)

	var ak APIKeyWithOwner
	var expiresAt *time.Time
	err := row.Scan(
		&ak.ID, &ak.Name, &ak.Enabled, &ak.Status, &expiresAt, &ak.OwnerUserID,
		&ak.OwnerID, &ak.OwnerDisplayName, &ak.OwnerEnabled,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find api key by hash: %w", err)
	}

	// Treat expired-by-time as disabled.
	if expiresAt != nil && expiresAt.Before(time.Now().UTC()) {
		ak.Enabled = false
	}
	// Status 'expired' or 'unavailable' MUST be rejected by the middleware.
	// Only 'active' and 'rotating' are acceptable; anything else folds into
	// Enabled=false. This is the safety net behind the explicit status
	// transitions performed by RotateAdminAPIKey / RetireAdminAPIKey.
	if ak.Status != "active" && ak.Status != "rotating" {
		ak.Enabled = false
	}

	return &ak, nil
}

// FindByKeyHash satisfies the middleware.AdminAPIKeyLookup interface.
func (store *Store) FindByKeyHash(ctx context.Context, keyHash string) (*APIKeyWithOwner, error) {
	return store.FindAPIKeyByHash(ctx, keyHash)
}

// UpdateKeyHashAndVersion lazy-migrates an admin key's stored hash to a new HMAC
// keyring version, called from the CP auth path after the key authenticated
// under an older version. It updates ONLY keyHash + key_version — deliberately
// NOT updatedAt or any rotation-lifecycle column (status/rotatedAt/
// rotatedFromId): the keyring version is orthogonal to the admin-key rotation
// state machine, and a re-hash is an internal at-rest migration, not a
// user-visible mutation.
//
// matchedHash is the stored hash that admitted the key, and the UPDATE is a
// compare-and-swap on it: if the row's keyHash changed between admission and
// this write (an admin regenerated the key mid-flight), the UPDATE matches no
// rows and the migration is silently skipped. Without the guard, the lazy
// re-hash would overwrite the regenerated hash and resurrect the superseded
// key. Concurrent admissions of the SAME key recompute the same deterministic
// current hash, so the first writer wins and the second matches zero rows —
// idempotent either way.
func (store *Store) UpdateKeyHashAndVersion(ctx context.Context, id, keyHash, keyVersion, matchedHash string) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE "AdminApiKey"
		SET "keyHash" = $2, "key_version" = $3
		WHERE id = $1 AND "keyHash" = $4
	`, id, keyHash, keyVersion, matchedHash)
	if err != nil {
		return fmt.Errorf("update key hash and version: %w", err)
	}
	return nil
}
