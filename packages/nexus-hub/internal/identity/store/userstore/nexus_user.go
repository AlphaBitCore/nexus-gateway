package userstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// NexusUserInfo holds user information for identity enrichment.
type NexusUserInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// GetNexusUser looks up a user by ID.
func (s *Store) GetNexusUser(ctx context.Context, userID string) (*NexusUserInfo, error) {
	var u NexusUserInfo
	err := s.db.QueryRow(ctx, `
		SELECT id, COALESCE(name, ''), COALESCE(email, '')
		FROM nexus_user WHERE id = $1
	`, userID).Scan(&u.ID, &u.Name, &u.Email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get user: %w", err)
	}
	return &u, nil
}

// OrgInfo holds organization information.
type OrgInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GetOrganization looks up an organization by ID.
func (s *Store) GetOrganization(ctx context.Context, orgID string) (*OrgInfo, error) {
	var o OrgInfo
	err := s.db.QueryRow(ctx, `
		SELECT id, COALESCE(name, '') FROM organization WHERE id = $1
	`, orgID).Scan(&o.ID, &o.Name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get org: %w", err)
	}
	return &o, nil
}
