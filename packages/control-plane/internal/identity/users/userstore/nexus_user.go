package userstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// NexusUser represents a row from the NexusUser table.
type NexusUser struct {
	ID                    string
	OrganizationID        string
	DisplayName           string
	Email                 *string
	Status                string
	CanAccessControlPlane bool
	Source                string
	OsUsername            *string
	OsDomain              *string
	PasswordHash          *string
	LastLoginAt           *time.Time
	PreferredTimezone     *string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

const nexusUserColumns = `id, "organizationId", "displayName", email, status,
    "canAccessControlPlane", source, "osUsername", "osDomain", "passwordHash",
    "lastLoginAt", "preferredTimezone", "createdAt", "updatedAt"`

// scanNexusUser scans a single NexusUser row. Returns nil, nil on ErrNoRows.
func scanNexusUser(row pgx.Row) (*NexusUser, error) {
	var u NexusUser
	err := row.Scan(&u.ID, &u.OrganizationID, &u.DisplayName, &u.Email,
		&u.Status, &u.CanAccessControlPlane, &u.Source,
		&u.OsUsername, &u.OsDomain,
		&u.PasswordHash, &u.LastLoginAt, &u.PreferredTimezone, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// FindNexusUserByEmail looks up a NexusUser by email that can access the control plane (for auth login).
func (store *Store) FindNexusUserByEmail(ctx context.Context, email string) (*NexusUser, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT %s
		FROM "NexusUser"
		WHERE email = $1 AND "canAccessControlPlane" = true
	`, nexusUserColumns), email)

	u, err := scanNexusUser(row)
	if err != nil {
		return nil, fmt.Errorf("find nexus user by email: %w", err)
	}
	return u, nil
}

// FindNexusUserByID looks up a NexusUser by ID.
func (store *Store) FindNexusUserByID(ctx context.Context, id string) (*NexusUser, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT %s
		FROM "NexusUser"
		WHERE id = $1
	`, nexusUserColumns), id)

	u, err := scanNexusUser(row)
	if err != nil {
		return nil, fmt.Errorf("find nexus user by id: %w", err)
	}
	return u, nil
}
