package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ExemptionRequest represents a row from the exemption_request table.
type ExemptionRequest struct {
	ID              string     `json:"id"`
	TransactionID   string     `json:"transactionId"`
	SourceIP        string     `json:"sourceIp"`
	TargetHost      string     `json:"targetHost"`
	Reason          string     `json:"reason"`
	Status          string     `json:"status"` // PENDING | APPROVED | REJECTED
	DurationMinutes int        `json:"durationMinutes"`
	ReviewedBy      *string    `json:"reviewedBy"`
	ReviewNote      *string    `json:"reviewNote"`
	ReviewedAt      *time.Time `json:"reviewedAt"`
	CreatedAt       time.Time  `json:"createdAt"`
	RequestedBy     string     `json:"requestedBy"`
}

const erColumns = `id, transaction_id, source_ip, target_host, reason, status,
	duration_minutes, reviewed_by, review_note, reviewed_at, "createdAt", requested_by`

func scanER(row pgx.Row) (*ExemptionRequest, error) {
	var r ExemptionRequest
	err := row.Scan(&r.ID, &r.TransactionID, &r.SourceIP, &r.TargetHost, &r.Reason,
		&r.Status, &r.DurationMinutes, &r.ReviewedBy, &r.ReviewNote, &r.ReviewedAt,
		&r.CreatedAt, &r.RequestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetExemptionRequest returns an exemption request by ID.
func (db *DB) GetExemptionRequest(ctx context.Context, id string) (*ExemptionRequest, error) {
	return scanER(db.pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM exemption_request WHERE id = $1`, erColumns), id))
}

// CreateExemptionRequest inserts a new exemption request.
func (db *DB) CreateExemptionRequest(ctx context.Context, p map[string]any) (*ExemptionRequest, error) {
	q := fmt.Sprintf(`
		INSERT INTO exemption_request (id, transaction_id, source_ip, target_host, reason, duration_minutes, requested_by, "createdAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, NOW())
		RETURNING %s
	`, erColumns)
	return scanER(db.pool.QueryRow(ctx, q,
		p["transactionId"], p["sourceIp"], p["targetHost"],
		p["reason"], p["durationMinutes"], p["requestedBy"]))
}

// MarkExemptionRequestRejected flips an exemption request to REJECTED via a
// single atomic UPDATE guarded by a status='PENDING' predicate. Returns
// pgx.ErrNoRows on zero rows affected so callers map both "unknown id" and
// "already reviewed" to 404/409 at the handler layer.
func (db *DB) MarkExemptionRequestRejected(ctx context.Context, id, reviewerID string) error {
	return markExemptionRequestStatus(ctx, db, id, reviewerID, "REJECTED")
}

func markExemptionRequestStatus(ctx context.Context, db *DB, id, reviewerID, status string) error {
	ct, err := db.pool.Exec(ctx, `
		UPDATE exemption_request
		SET status = $3, reviewed_by = $2, reviewed_at = NOW()
		WHERE id = $1 AND status = 'PENDING'
	`, id, reviewerID, status)
	if err != nil {
		return fmt.Errorf("update exemption_request: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
