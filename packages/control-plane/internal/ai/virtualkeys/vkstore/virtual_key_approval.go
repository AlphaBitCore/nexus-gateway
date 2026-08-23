package vkstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ApproveVirtualKey transitions a pending virtual key to active.
func (store *Store) ApproveVirtualKey(ctx context.Context, id, approvedBy string) error {
	tag, err := store.pool.Exec(ctx, `
		UPDATE "VirtualKey"
		SET "vkStatus" = 'active', "approvedBy" = $2, "approvedAt" = NOW(), "updatedAt" = NOW()
		WHERE id = $1 AND "vkStatus" = 'pending'
	`, id, approvedBy)
	if err != nil {
		return fmt.Errorf("approve virtual key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// RejectVirtualKey transitions a pending virtual key to rejected.
func (store *Store) RejectVirtualKey(ctx context.Context, id, rejectedBy, reason string) error {
	tag, err := store.pool.Exec(ctx, `
		UPDATE "VirtualKey"
		SET "vkStatus" = 'rejected', "rejectedBy" = $2, "rejectedAt" = NOW(), "rejectReason" = $3, "updatedAt" = NOW()
		WHERE id = $1 AND "vkStatus" = 'pending'
	`, id, rejectedBy, reason)
	if err != nil {
		return fmt.Errorf("reject virtual key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// RenewVirtualKey extends the expiry of an application virtual key and returns
// it to active.
//
// Both time-derived states are renewable. Renewing an already-expired key is
// the primary reason the endpoint exists: callers validate newExpiresAt is in
// the future, so once the row carries that date the key is by definition no
// longer expired. Accepting only 'active' would leave an expired key with no
// route back — the hourly expiry job moves active -> expired and never the
// reverse.
//
// 'revoked' and 'rejected' remain excluded: those are administrative
// decisions, not clock events, and are reversed by issuing a new key rather
// than by extending a date.
func (store *Store) RenewVirtualKey(ctx context.Context, id string, newExpiresAt time.Time) error {
	tag, err := store.pool.Exec(ctx, `
		UPDATE "VirtualKey"
		SET "expiresAt" = $2, "vkStatus" = 'active', "updatedAt" = NOW()
		WHERE id = $1 AND "vkType" = 'application' AND "vkStatus" IN ('active', 'expired')
	`, id, newExpiresAt)
	if err != nil {
		return fmt.Errorf("renew virtual key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// RevokeVirtualKey transitions an active virtual key to revoked.
func (store *Store) RevokeVirtualKey(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `
		UPDATE "VirtualKey"
		SET "vkStatus" = 'revoked', "updatedAt" = NOW()
		WHERE id = $1 AND "vkStatus" = 'active'
	`, id)
	if err != nil {
		return fmt.Errorf("revoke virtual key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
