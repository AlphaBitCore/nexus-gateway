package vkstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// VirtualKeyExpiry holds minimal fields for expiry notification.
type VirtualKeyExpiry struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expiresAt"`
}

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

// RenewVirtualKey extends the expiry of an active application virtual key.
func (store *Store) RenewVirtualKey(ctx context.Context, id string, newExpiresAt time.Time) error {
	tag, err := store.pool.Exec(ctx, `
		UPDATE "VirtualKey"
		SET "expiresAt" = $2, "updatedAt" = NOW()
		WHERE id = $1 AND "vkType" = 'application' AND "vkStatus" = 'active'
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

// ExpireOverdueVirtualKeys sets vkStatus='expired' for all active keys past their expiry.
// Returns the number of rows updated.
func (store *Store) ExpireOverdueVirtualKeys(ctx context.Context) (int64, error) {
	tag, err := store.pool.Exec(ctx, `
		UPDATE "VirtualKey"
		SET "vkStatus" = 'expired', "updatedAt" = NOW()
		WHERE "expiresAt" <= NOW() AND "vkStatus" = 'active'
	`)
	if err != nil {
		return 0, fmt.Errorf("expire overdue virtual keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListExpiringVirtualKeys returns active application keys expiring within the given number of days.
func (store *Store) ListExpiringVirtualKeys(ctx context.Context, withinDays int) ([]VirtualKeyExpiry, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id, name, "expiresAt"
		FROM "VirtualKey"
		WHERE "expiresAt" <= NOW() + ($1 || ' days')::interval
		  AND "expiresAt" > NOW()
		  AND "vkStatus" = 'active'
		  AND "vkType" = 'application'
		ORDER BY "expiresAt" ASC
	`, withinDays)
	if err != nil {
		return nil, fmt.Errorf("list expiring virtual keys: %w", err)
	}
	defer rows.Close()

	keys := []VirtualKeyExpiry{}
	for rows.Next() {
		var k VirtualKeyExpiry
		if err := rows.Scan(&k.ID, &k.Name, &k.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan virtual key expiry: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
