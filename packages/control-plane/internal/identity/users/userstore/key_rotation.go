package userstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RegenerateAdminAPIKey replaces the key hash and prefix for an existing API key.
func (store *Store) RegenerateAdminAPIKey(ctx context.Context, id, keyHash, keyPrefix string) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE "AdminApiKey" SET "keyHash" = $2, "keyPrefix" = $3, "updatedAt" = NOW() WHERE id = $1
	`, id, keyHash, keyPrefix)
	return err
}

// RotateAdminAPIKeyParams holds fields for an atomic rotation: mint a new key
// that inherits the predecessor's name + owner, mark the predecessor as
// rotating, and link successor → predecessor via rotatedFromId.
type RotateAdminAPIKeyParams struct {
	PredecessorID string
	NewKeyHash    string
	NewKeyPrefix  string
	NewCreatedBy  string
	// NewExpiresAt is the absolute expiry stamped on the successor row.
	// nil means "inherit from predecessor" — RotateAdminAPIKey resolves this
	// inside the transaction.
	NewExpiresAt *time.Time
}

// RotateAdminAPIKeyResult bundles the two affected rows: the newly-minted
// successor and the rotated-out predecessor, both reflecting their post-
// transaction state.
type RotateAdminAPIKeyResult struct {
	Successor   *AdminAPIKey
	Predecessor *AdminAPIKey
}

// RotateAdminAPIKey atomically mints a successor row for the predecessor and
// flips the predecessor's status active → rotating. Both keys remain
// acceptable to the auth middleware during the rotation window so callers can
// swap in the new value without service interruption; the operator calls
// RetireAdminAPIKey on the predecessor once the swap is complete.
//
// Errors:
//   - pgx.ErrNoRows — predecessor does not exist
//   - status / enabled invariant errors are returned as plain errors with
//     descriptive messages; the handler maps them to HTTP 409.
func (store *Store) RotateAdminAPIKey(ctx context.Context, p RotateAdminAPIKeyParams) (*RotateAdminAPIKeyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the predecessor row so concurrent rotate calls serialise.
	var (
		predName        string
		predEnabled     bool
		predStatus      string
		predOwnerUserID *string
		predExpiresAt   *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT name, enabled, status, "ownerUserId", "expiresAt"
		FROM "AdminApiKey" WHERE id = $1 FOR UPDATE
	`, p.PredecessorID).Scan(&predName, &predEnabled, &predStatus, &predOwnerUserID, &predExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, pgx.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("rotate: lock predecessor: %w", err)
	}
	if predStatus != AdminAPIKeyStatusActive {
		return nil, fmt.Errorf("rotate: predecessor status is %q; only active keys can be rotated", predStatus)
	}

	// Resolve the successor's expiry: caller-supplied value wins, otherwise
	// inherit the predecessor's expiry so the rotation window does not
	// silently extend the operator's intended lifetime.
	successorExpiresAt := p.NewExpiresAt
	if successorExpiresAt == nil {
		successorExpiresAt = predExpiresAt
	}

	// Mint the successor with rotatedFromId pointing at the predecessor.
	successorRow := tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO "AdminApiKey" (
			id, name, "keyHash", "keyPrefix", "createdBy", "expiresAt",
			"ownerUserId", "rotatedFromId", status, "createdAt", "updatedAt"
		) VALUES (
			gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, 'active', NOW(), NOW()
		) RETURNING %s
	`, apiKeyListColumns),
		predName, p.NewKeyHash, p.NewKeyPrefix, p.NewCreatedBy,
		successorExpiresAt, predOwnerUserID, p.PredecessorID,
	)
	var successor AdminAPIKey
	if err := scanAdminAPIKey(successorRow, &successor); err != nil {
		return nil, fmt.Errorf("rotate: insert successor: %w", err)
	}

	// Flip the predecessor to rotating + stamp rotatedAt. enabled is left
	// untouched so the operator's quick-disable toggle still works.
	predRow := tx.QueryRow(ctx, fmt.Sprintf(`
		UPDATE "AdminApiKey"
		SET status = 'rotating', "rotatedAt" = NOW(), "updatedAt" = NOW()
		WHERE id = $1
		RETURNING %s
	`, apiKeyListColumns), p.PredecessorID)
	var predecessor AdminAPIKey
	if err := scanAdminAPIKey(predRow, &predecessor); err != nil {
		return nil, fmt.Errorf("rotate: update predecessor: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("rotate: commit: %w", err)
	}
	return &RotateAdminAPIKeyResult{Successor: &successor, Predecessor: &predecessor}, nil
}

// RetireAdminAPIKey transitions a key out of the accepted set. Allowed
// source states:
//   - active    — operator deciding to sunset a key
//   - rotating  — rotation window closing
//
// The target status MUST be either AdminAPIKeyStatusExpired (natural sunset)
// or AdminAPIKeyStatusUnavailable (active revocation / compromise).
//
// Returns pgx.ErrNoRows if the key does not exist; returns a descriptive
// error if the source state or target status is invalid.
func (store *Store) RetireAdminAPIKey(ctx context.Context, id, targetStatus string) (*AdminAPIKey, error) {
	if targetStatus != AdminAPIKeyStatusExpired && targetStatus != AdminAPIKeyStatusUnavailable {
		return nil, fmt.Errorf("retire: invalid target status %q (want %q or %q)",
			targetStatus, AdminAPIKeyStatusExpired, AdminAPIKeyStatusUnavailable)
	}
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE "AdminApiKey"
		SET status = $2, "updatedAt" = NOW()
		WHERE id = $1
		  AND status IN ('active', 'rotating')
		RETURNING %s
	`, apiKeyListColumns), id, targetStatus)
	var k AdminAPIKey
	err := scanAdminAPIKey(row, &k)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either the row does not exist OR it is already retired. Distinguish
		// by reading the row separately so the caller can return 404 vs 409.
		var exists bool
		if e := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM "AdminApiKey" WHERE id = $1)`, id).Scan(&exists); e != nil {
			return nil, fmt.Errorf("retire: existence probe: %w", e)
		}
		if !exists {
			return nil, pgx.ErrNoRows
		}
		return nil, fmt.Errorf("retire: key is already expired or unavailable")
	}
	if err != nil {
		return nil, fmt.Errorf("retire: %w", err)
	}
	return &k, nil
}
