package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ComplianceExemptionGrant is a row in compliance_exemption_grant.
type ComplianceExemptionGrant struct {
	ID                 string     `json:"id"`
	ExemptionRequestID *string    `json:"exemptionRequestId"`
	SourceIP           string     `json:"sourceIp"`
	TargetHost         string     `json:"targetHost"`
	Reason             string     `json:"reason"`
	DurationMinutes    int        `json:"durationMinutes"`
	EffectiveFrom      time.Time  `json:"effectiveFrom"`
	ExpiresAt          time.Time  `json:"expiresAt"`
	RequestedBy        *string    `json:"requestedBy"`
	ApprovedBy         string     `json:"approvedBy"`
	Inactive           bool       `json:"inactive"`
	ActivatedAt        *time.Time `json:"activatedAt"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

const cegColumns = `id, exemption_request_id, source_ip, target_host, reason,
	duration_minutes, effective_from, expires_at, requested_by, approved_by,
	inactive, activated_at, created_at, updated_at`

func scanComplianceExemptionGrant(row pgx.Row) (*ComplianceExemptionGrant, error) {
	var g ComplianceExemptionGrant
	var exReqID, requestedBy *string
	err := row.Scan(
		&g.ID, &exReqID, &g.SourceIP, &g.TargetHost, &g.Reason,
		&g.DurationMinutes, &g.EffectiveFrom, &g.ExpiresAt, &requestedBy, &g.ApprovedBy,
		&g.Inactive, &g.ActivatedAt, &g.CreatedAt, &g.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.ExemptionRequestID = exReqID
	g.RequestedBy = requestedBy
	return &g, nil
}

// GetComplianceExemptionGrant loads a grant by primary key.
func (db *DB) GetComplianceExemptionGrant(ctx context.Context, id string) (*ComplianceExemptionGrant, error) {
	return scanComplianceExemptionGrant(db.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s FROM compliance_exemption_grant WHERE id = $1`, cegColumns), id))
}

// GetComplianceExemptionGrantByExemptionRequestID returns the grant linked to an exemption request, if any.
func (db *DB) GetComplianceExemptionGrantByExemptionRequestID(ctx context.Context, exemptionRequestID string) (*ComplianceExemptionGrant, error) {
	return scanComplianceExemptionGrant(db.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT %s FROM compliance_exemption_grant WHERE exemption_request_id = $1 LIMIT 1`, cegColumns), exemptionRequestID))
}

// InsertComplianceExemptionGrant inserts a new grant row (admin direct create).
func (db *DB) InsertComplianceExemptionGrant(ctx context.Context, p ComplianceExemptionGrantInsert) (*ComplianceExemptionGrant, error) {
	id := uuid.NewString()
	row := db.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO compliance_exemption_grant (
			id, exemption_request_id, source_ip, target_host, reason,
			duration_minutes, effective_from, expires_at, requested_by, approved_by,
			updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		RETURNING %s`, cegColumns),
		id, p.ExemptionRequestID, p.SourceIP, p.TargetHost, p.Reason,
		p.DurationMinutes, p.EffectiveFrom, p.ExpiresAt, p.RequestedBy, p.ApprovedBy,
	)
	return scanComplianceExemptionGrant(row)
}

// ComplianceExemptionGrantInsert is input for InsertComplianceExemptionGrant.
type ComplianceExemptionGrantInsert struct {
	ExemptionRequestID *string
	SourceIP           string
	TargetHost         string
	Reason             string
	DurationMinutes    int
	EffectiveFrom      time.Time
	ExpiresAt          time.Time
	RequestedBy        *string
	ApprovedBy         string
}

// UpdateComplianceExemptionGrantInactive sets the inactive flag.
func (db *DB) UpdateComplianceExemptionGrantInactive(ctx context.Context, id string, inactive bool) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE compliance_exemption_grant SET inactive = $2, updated_at = NOW() WHERE id = $1
	`, id, inactive)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DeleteComplianceExemptionGrantIfPreActivation deletes a grant only when activated_at is still null.
func (db *DB) DeleteComplianceExemptionGrantIfPreActivation(ctx context.Context, id string) (bool, error) {
	tag, err := db.pool.Exec(ctx, `
		DELETE FROM compliance_exemption_grant WHERE id = $1 AND activated_at IS NULL
	`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ApproveExemptionRequestWithGrant marks the request APPROVED and inserts a linked grant in one transaction.
func (db *DB) ApproveExemptionRequestWithGrant(ctx context.Context, reqID, reviewerUserID, approverDisplayName string) (*ComplianceExemptionGrant, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	req, err := scanER(tx.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM exemption_request WHERE id = $1`, erColumns), reqID))
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, nil
	}

	ct, err := tx.Exec(ctx, `
		UPDATE exemption_request
		SET status = 'APPROVED', reviewed_by = $2, reviewed_at = NOW()
		WHERE id = $1 AND status = 'PENDING'
	`, reqID, reviewerUserID)
	if err != nil {
		return nil, err
	}
	if ct.RowsAffected() == 0 {
		return nil, pgx.ErrNoRows
	}

	now := time.Now().UTC()
	effectiveFrom := now
	expiresAt := now.Add(time.Duration(req.DurationMinutes) * time.Minute)
	grantID := uuid.NewString()
	g, err := scanComplianceExemptionGrant(tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO compliance_exemption_grant (
			id, exemption_request_id, source_ip, target_host, reason,
			duration_minutes, effective_from, expires_at, requested_by, approved_by,
			updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		RETURNING %s`, cegColumns),
		grantID, reqID, req.SourceIP, req.TargetHost, req.Reason,
		req.DurationMinutes, effectiveFrom, expiresAt, stringPtrOrNil(req.RequestedBy), approverDisplayName,
	))
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit approve+grant: %w", err)
	}
	return g, nil
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
