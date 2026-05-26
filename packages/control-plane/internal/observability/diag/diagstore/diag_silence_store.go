package diagstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrSilenceNotFound is returned when a silence id has no matching row.
var ErrSilenceNotFound = errors.New("diag silence not found")

// DiagSilence is a row in diag_silence. A silence matches a
// (messageHash, level) pair; the /infrastructure/errors page collapses
// matching groups so triage focuses on what's new. expiresAt NULL means
// permanent — sweep with care.
type DiagSilence struct {
	ID          string     `json:"id"`
	MessageHash string     `json:"messageHash"`
	Level       string     `json:"level"`
	SilencedBy  string     `json:"silencedBy"`
	SilencedAt  time.Time  `json:"silencedAt"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}

// CreateDiagSilenceParams is the input to CreateDiagSilence.
type CreateDiagSilenceParams struct {
	MessageHash string
	Level       string
	SilencedBy  string
	ExpiresAt   *time.Time
	Reason      string
}

// CreateDiagSilence inserts a new silence and returns the persisted row.
// A duplicate (messageHash, level) ACTIVE silence is allowed — operators
// extending coverage just create a new row; the join in ListDiagGroups
// uses EXISTS so multiple rows are equivalent to one.
func (store *Store) CreateDiagSilence(ctx context.Context, p CreateDiagSilenceParams) (*DiagSilence, error) {
	if p.MessageHash == "" {
		return nil, errors.New("create_diag_silence: messageHash required")
	}
	if p.Level == "" {
		return nil, errors.New("create_diag_silence: level required")
	}
	if p.SilencedBy == "" {
		return nil, errors.New("create_diag_silence: silencedBy required")
	}

	id := uuid.NewString()
	const q = `
		INSERT INTO diag_silence (id, message_hash, level, silenced_by, expires_at, reason)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
		RETURNING id, message_hash, level, silenced_by, silenced_at, expires_at, reason
	`
	var s DiagSilence
	var reason *string
	err := store.pool.QueryRow(ctx, q, id, p.MessageHash, p.Level, p.SilencedBy, p.ExpiresAt, p.Reason).Scan(
		&s.ID, &s.MessageHash, &s.Level, &s.SilencedBy, &s.SilencedAt, &s.ExpiresAt, &reason,
	)
	if err != nil {
		return nil, fmt.Errorf("create_diag_silence: %w", err)
	}
	if reason != nil {
		s.Reason = *reason
	}
	return &s, nil
}

// DeleteDiagSilence removes a silence by id. Returns ErrSilenceNotFound when no
// matching row exists.
func (store *Store) DeleteDiagSilence(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("delete_diag_silence: id required")
	}
	const q = `DELETE FROM diag_silence WHERE id = $1`
	tag, err := store.pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("delete_diag_silence: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSilenceNotFound
	}
	return nil
}

// ListActiveDiagSilences returns currently-active silences (expires_at is
// NULL or in the future). Ordered by silenced_at DESC so newer silences
// surface first in the UI.
func (store *Store) ListActiveDiagSilences(ctx context.Context) ([]DiagSilence, error) {
	const q = `
		SELECT id, message_hash, level, silenced_by, silenced_at, expires_at, reason
		  FROM diag_silence
		 WHERE expires_at IS NULL OR expires_at > NOW()
		 ORDER BY silenced_at DESC
		 LIMIT 500
	`
	rows, err := store.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list_active_diag_silences: %w", err)
	}
	defer rows.Close()

	out := make([]DiagSilence, 0, 32)
	for rows.Next() {
		var s DiagSilence
		var reason *string
		if err := rows.Scan(&s.ID, &s.MessageHash, &s.Level, &s.SilencedBy, &s.SilencedAt, &s.ExpiresAt, &reason); err != nil {
			return nil, fmt.Errorf("list_active_diag_silences scan: %w", err)
		}
		if reason != nil {
			s.Reason = *reason
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list_active_diag_silences iterate: %w", err)
	}
	return out, nil
}

// GetDiagSilence returns a single silence by id, or pgx.ErrNoRows when
// the row doesn't exist. Used by the audit before-state snapshot in the
// DELETE handler.
func (store *Store) GetDiagSilence(ctx context.Context, id string) (*DiagSilence, error) {
	const q = `
		SELECT id, message_hash, level, silenced_by, silenced_at, expires_at, reason
		  FROM diag_silence WHERE id = $1
	`
	var s DiagSilence
	var reason *string
	err := store.pool.QueryRow(ctx, q, id).Scan(
		&s.ID, &s.MessageHash, &s.Level, &s.SilencedBy, &s.SilencedAt, &s.ExpiresAt, &reason,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSilenceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get_diag_silence: %w", err)
	}
	if reason != nil {
		s.Reason = *reason
	}
	return &s, nil
}
