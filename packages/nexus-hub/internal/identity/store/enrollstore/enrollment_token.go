package enrollstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnrollmentToken represents a row from the enrollment_token table.
type EnrollmentToken struct {
	ID        string         `json:"id"`
	TokenHash string         `json:"-"`
	ThingType string         `json:"thingType"`
	ThingID   *string        `json:"thingId,omitempty"`
	Label     string         `json:"label"`
	Status    string         `json:"status"`
	ExpiresAt time.Time      `json:"expiresAt"`
	UsedAt    *time.Time     `json:"usedAt,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedBy *string        `json:"createdBy,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

const enrollmentTokenColumns = `id, token_hash, thing_type, thing_id, label, status, expires_at, used_at, metadata, created_by, created_at`

func scanEnrollmentToken(row pgx.Row) (*EnrollmentToken, error) {
	var et EnrollmentToken
	var metaRaw []byte
	err := row.Scan(
		&et.ID, &et.TokenHash, &et.ThingType, &et.ThingID, &et.Label,
		&et.Status, &et.ExpiresAt, &et.UsedAt, &metaRaw, &et.CreatedBy, &et.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if err := decodeJSONB(metaRaw, &et.Metadata, "metadata"); err != nil {
		return nil, err
	}
	return &et, nil
}

// InsertEnrollmentTokenParams holds fields for creating an enrollment token.
type InsertEnrollmentTokenParams struct {
	ThingType string
	Label     string
	ExpiresIn time.Duration
	Metadata  map[string]any
	CreatedBy string
}

// InsertEnrollmentToken creates a new enrollment token.
// Returns the token record and the raw token string (only available at creation time).
func (s *Store) InsertEnrollmentToken(ctx context.Context, p InsertEnrollmentTokenParams) (*EnrollmentToken, string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, "", fmt.Errorf("generate token bytes: %w", err)
	}
	rawToken := "enroll-" + hex.EncodeToString(tokenBytes)
	tokenHash := hashTokenSHA256(rawToken)

	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, "", fmt.Errorf("generate id: %w", err)
	}
	id := hex.EncodeToString(idBytes)

	if p.ExpiresIn <= 0 {
		p.ExpiresIn = 24 * time.Hour
	}
	expiresAt := time.Now().Add(p.ExpiresIn)

	metaJSON, _ := json.Marshal(p.Metadata)
	if p.Metadata == nil {
		metaJSON = nil
	}

	et, err := scanEnrollmentToken(s.db.QueryRow(ctx, `
		INSERT INTO enrollment_token (id, token_hash, thing_type, label, status, expires_at, metadata, created_by, created_at)
		VALUES ($1, $2, $3, $4, 'pending', $5, $6, $7, NOW())
		RETURNING `+enrollmentTokenColumns,
		id, tokenHash, p.ThingType, p.Label, expiresAt, metaJSON, p.CreatedBy,
	))
	if err != nil {
		return nil, "", fmt.Errorf("insert enrollment token: %w", err)
	}
	return et, rawToken, nil
}

// ValidateEnrollmentToken checks a raw token string against the DB.
// Returns the token row if found (caller checks status/expiry).
func (s *Store) ValidateEnrollmentToken(ctx context.Context, rawToken string) (*EnrollmentToken, error) {
	tokenHash := hashTokenSHA256(rawToken)
	et, err := scanEnrollmentToken(s.db.QueryRow(ctx, `
		SELECT `+enrollmentTokenColumns+`
		FROM enrollment_token
		WHERE token_hash = $1`, tokenHash))
	if err != nil {
		return nil, fmt.Errorf("validate enrollment token: %w", err)
	}
	return et, nil
}

// MarkEnrollmentTokenUsed marks a token as used and links it to a thing.
func (s *Store) MarkEnrollmentTokenUsed(ctx context.Context, id, thingID string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE enrollment_token
		SET status = 'used', thing_id = $2, used_at = NOW()
		WHERE id = $1 AND status = 'pending'`, id, thingID)
	if err != nil {
		return fmt.Errorf("mark token used: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeEnrollmentToken sets a token's status to revoked.
func (s *Store) RevokeEnrollmentToken(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE enrollment_token SET status = 'revoked' WHERE id = $1 AND status = 'pending'`, id)
	if err != nil {
		return fmt.Errorf("revoke enrollment token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListEnrollmentTokens returns enrollment tokens filtered by optional type and status.
func (s *Store) ListEnrollmentTokens(ctx context.Context, thingType, status string) ([]EnrollmentToken, error) {
	where := "WHERE 1=1"
	args := []any{}
	n := 1
	if thingType != "" {
		where += fmt.Sprintf(" AND thing_type = $%d", n)
		args = append(args, thingType)
		n++
	}
	if status != "" {
		where += fmt.Sprintf(" AND status = $%d", n)
		args = append(args, status)
	}

	rows, err := s.db.Query(ctx, `SELECT `+enrollmentTokenColumns+` FROM enrollment_token `+where+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list enrollment tokens: %w", err)
	}
	defer rows.Close()

	var result []EnrollmentToken
	for rows.Next() {
		var et EnrollmentToken
		var metaRaw []byte
		if err := rows.Scan(
			&et.ID, &et.TokenHash, &et.ThingType, &et.ThingID, &et.Label,
			&et.Status, &et.ExpiresAt, &et.UsedAt, &metaRaw, &et.CreatedBy, &et.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan enrollment token: %w", err)
		}
		if err := decodeJSONB(metaRaw, &et.Metadata, "metadata"); err != nil {
			return nil, err
		}
		result = append(result, et)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate enrollment tokens: %w", err)
	}
	return result, nil
}

// CleanupExpiredEnrollmentTokens marks expired pending tokens as 'expired'.
// Returns the number of tokens updated.
func (s *Store) CleanupExpiredEnrollmentTokens(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE enrollment_token SET status = 'expired'
		WHERE status = 'pending' AND expires_at < NOW()`)
	if err != nil {
		return 0, fmt.Errorf("cleanup expired tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}

func hashTokenSHA256(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
