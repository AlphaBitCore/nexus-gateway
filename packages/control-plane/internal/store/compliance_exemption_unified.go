package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// UnifiedExemptionRow is a single row in the unified exemption list. It
// merges compliance_exemption_grant (with its computed lifecycle status)
// and PENDING exemption_request rows behind a Kind discriminator. Per-kind
// fields are nullable on the opposite kind.
type UnifiedExemptionRow struct {
	Kind            string    `json:"kind"`   // "grant" | "pending"
	Status          string    `json:"status"` // effective | oncoming | expired | pending
	ID              string    `json:"id"`
	SourceIP        string    `json:"sourceIp"`
	TargetHost      string    `json:"targetHost"`
	Reason          string    `json:"reason"`
	DurationMinutes int       `json:"durationMinutes"`
	CreatedAt       time.Time `json:"createdAt"`

	// Grant-only (nil when Kind == "pending").
	EffectiveFrom *time.Time `json:"effectiveFrom"`
	ExpiresAt     *time.Time `json:"expiresAt"`
	ApprovedBy    *string    `json:"approvedBy"`
	Inactive      *bool      `json:"inactive"`
	ActivatedAt   *time.Time `json:"activatedAt"`

	// Pending-only (nil when Kind == "grant").
	TransactionID *string `json:"transactionId"`

	// requested_by is optional on grants and required on pending requests; in
	// both cases it surfaces here as a nullable string for the unified shape.
	RequestedBy *string `json:"requestedBy"`
}

var validUnifiedExemptionTabs = map[string]struct{}{
	"all":       {},
	"effective": {},
	"oncoming":  {},
	"expired":   {},
	"pending":   {},
}

// ListUnifiedExemptionsPage returns a paginated, status-filtered list that
// merges compliance_exemption_grant rows (effective / oncoming / expired)
// with PENDING exemption_request rows. tab is one of all|effective|oncoming|
// expired|pending (case-insensitive); empty defaults to "all". Rows are
// ordered by created_at DESC. Disabled grants (inactive=true) keep their
// lifecycle status; the inactive flag rides alongside so the UI can show a
// Disabled badge without changing the lifecycle bucket.
func (db *DB) ListUnifiedExemptionsPage(ctx context.Context, tab string, now time.Time, limit, offset int) ([]UnifiedExemptionRow, int, error) {
	tab = strings.ToLower(strings.TrimSpace(tab))
	if tab == "" {
		tab = "all"
	}
	if _, ok := validUnifiedExemptionTabs[tab]; !ok {
		return nil, 0, fmt.Errorf("invalid tab %q (expected all|effective|oncoming|expired|pending)", tab)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	const baseCTE = `
		WITH grants_view AS (
			SELECT
				'grant'::text AS kind,
				CASE
					WHEN expires_at <= $1 THEN 'expired'
					WHEN effective_from > $1 THEN 'oncoming'
					ELSE 'effective'
				END AS status,
				id, source_ip, target_host, reason, duration_minutes,
				created_at,
				effective_from, expires_at, approved_by, inactive, activated_at,
				requested_by,
				NULL::text AS transaction_id
			FROM compliance_exemption_grant
		),
		pending_view AS (
			SELECT
				'pending'::text AS kind,
				'pending'::text AS status,
				id, source_ip, target_host, reason, duration_minutes,
				"createdAt" AS created_at,
				NULL::timestamptz AS effective_from,
				NULL::timestamptz AS expires_at,
				NULL::text AS approved_by,
				NULL::boolean AS inactive,
				NULL::timestamptz AS activated_at,
				requested_by,
				transaction_id
			FROM exemption_request
			WHERE status = 'PENDING'
		),
		unified AS (
			SELECT * FROM grants_view
			UNION ALL
			SELECT * FROM pending_view
		)
	`

	var statusFilter string
	countArgs := []any{now}
	if tab != "all" {
		statusFilter = ` WHERE status = $2`
		countArgs = append(countArgs, tab)
	}

	var total int
	if err := db.pool.QueryRow(ctx, baseCTE+`SELECT COUNT(*)::int FROM unified`+statusFilter, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count unified exemptions: %w", err)
	}

	args := []any{now}
	var limPlaceholder, offPlaceholder int
	if tab == "all" {
		args = append(args, limit, offset)
		limPlaceholder = 2
		offPlaceholder = 3
	} else {
		args = append(args, tab, limit, offset)
		limPlaceholder = 3
		offPlaceholder = 4
	}

	q := baseCTE + fmt.Sprintf(`
		SELECT kind, status, id, source_ip, target_host, reason, duration_minutes,
		       created_at, effective_from, expires_at, approved_by, inactive,
		       activated_at, requested_by, transaction_id
		  FROM unified%s
		 ORDER BY created_at DESC
		 LIMIT $%d OFFSET $%d`, statusFilter, limPlaceholder, offPlaceholder)

	rows, err := db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query unified exemptions: %w", err)
	}
	defer rows.Close()

	out := []UnifiedExemptionRow{}
	for rows.Next() {
		var r UnifiedExemptionRow
		if err := rows.Scan(
			&r.Kind, &r.Status, &r.ID, &r.SourceIP, &r.TargetHost, &r.Reason, &r.DurationMinutes,
			&r.CreatedAt, &r.EffectiveFrom, &r.ExpiresAt, &r.ApprovedBy, &r.Inactive,
			&r.ActivatedAt, &r.RequestedBy, &r.TransactionID,
		); err != nil {
			return nil, 0, fmt.Errorf("scan unified exemption: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate unified exemptions: %w", err)
	}
	return out, total, nil
}

// GetUnifiedExemptionByID returns one unified list row by grant id or by a
// PENDING exemption_request id. Not found yields (nil, nil).
func (db *DB) GetUnifiedExemptionByID(ctx context.Context, id string, now time.Time) (*UnifiedExemptionRow, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}

	const q = `
		WITH grants_view AS (
			SELECT
				'grant'::text AS kind,
				CASE
					WHEN expires_at <= $1 THEN 'expired'
					WHEN effective_from > $1 THEN 'oncoming'
					ELSE 'effective'
				END AS status,
				id, source_ip, target_host, reason, duration_minutes,
				created_at,
				effective_from, expires_at, approved_by, inactive, activated_at,
				requested_by,
				NULL::text AS transaction_id
			FROM compliance_exemption_grant
		),
		pending_view AS (
			SELECT
				'pending'::text AS kind,
				'pending'::text AS status,
				id, source_ip, target_host, reason, duration_minutes,
				"createdAt" AS created_at,
				NULL::timestamptz AS effective_from,
				NULL::timestamptz AS expires_at,
				NULL::text AS approved_by,
				NULL::boolean AS inactive,
				NULL::timestamptz AS activated_at,
				requested_by,
				transaction_id
			FROM exemption_request
			WHERE status = 'PENDING'
		),
		unified AS (
			SELECT * FROM grants_view
			UNION ALL
			SELECT * FROM pending_view
		)
		SELECT kind, status, id, source_ip, target_host, reason, duration_minutes,
		       created_at, effective_from, expires_at, approved_by, inactive,
		       activated_at, requested_by, transaction_id
		  FROM unified
		 WHERE id = $2
		 LIMIT 1
	`

	row := db.pool.QueryRow(ctx, q, now, id)
	var r UnifiedExemptionRow
	if err := row.Scan(
		&r.Kind, &r.Status, &r.ID, &r.SourceIP, &r.TargetHost, &r.Reason, &r.DurationMinutes,
		&r.CreatedAt, &r.EffectiveFrom, &r.ExpiresAt, &r.ApprovedBy, &r.Inactive,
		&r.ActivatedAt, &r.RequestedBy, &r.TransactionID,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get unified exemption by id: %w", err)
	}
	return &r, nil
}
