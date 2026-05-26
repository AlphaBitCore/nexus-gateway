// Package dsarstore owns the DSAR (Data Subject Access Request)
// persistence — extracted from internal/store/dsar.go per R8-B18.
package dsarstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PgxPool is the minimum pgx pool surface dsarstore methods need.
// *pgxpool.Pool satisfies it in production; pgxmock satisfies it in
// tests. Mirrors store.PgxPool.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is the DSAR persistence handle.
type Store struct {
	pool PgxPool
}

// New constructs a Store from a PgxPool.
func New(pool PgxPool) *Store {
	return &Store{pool: pool}
}

// DSARRequest represents a row from the dsar_request table.
type DSARRequest struct {
	ID          string          `json:"id"`
	SubjectID   string          `json:"subjectId"`
	Contact     *string         `json:"contact"`
	Type        string          `json:"type"`   // ACCESS | ERASURE
	Status      string          `json:"status"` // PENDING | IN_PROGRESS | COMPLETED | REJECTED
	Notes       *string         `json:"notes"`
	CompletedAt *time.Time      `json:"completedAt"`
	Outcome     json.RawMessage `json:"outcome"`
	CreatedAt   time.Time       `json:"createdAt"`
	CreatedBy   string          `json:"createdBy"`
	UpdatedAt   time.Time       `json:"updatedAt"`
	UpdatedBy   *string         `json:"updatedBy"`
}

const dsarColumns = `id, subject_id, contact, type, status, notes, completed_at,
	outcome, "createdAt", created_by, "updatedAt", updated_by`

func scanDSAR(row pgx.Row) (*DSARRequest, error) {
	var d DSARRequest
	err := row.Scan(
		&d.ID, &d.SubjectID, &d.Contact, &d.Type, &d.Status, &d.Notes,
		&d.CompletedAt, &d.Outcome, &d.CreatedAt, &d.CreatedBy, &d.UpdatedAt, &d.UpdatedBy,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDSARRequests returns paginated DSAR requests optionally filtered by status.
// Returns the matching rows and the total unfiltered count.
func (store *Store) ListDSARRequests(ctx context.Context, status string, limit, offset int) ([]DSARRequest, int, error) {
	if limit <= 0 {
		limit = 20
	}
	where := "WHERE 1=1"
	args := []any{}
	n := 1
	if status != "" {
		where += fmt.Sprintf(` AND status = $%d`, n)
		args = append(args, status)
		n++
	}

	var total int
	if err := store.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM dsar_request %s`, where), args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count dsar: %w", err)
	}

	q := fmt.Sprintf(`SELECT %s FROM dsar_request %s ORDER BY "createdAt" DESC LIMIT $%d OFFSET $%d`,
		dsarColumns, where, n, n+1)
	args = append(args, limit, offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list dsar: %w", err)
	}
	defer rows.Close()

	requests := []DSARRequest{}
	for rows.Next() {
		var d DSARRequest
		if err := rows.Scan(
			&d.ID, &d.SubjectID, &d.Contact, &d.Type, &d.Status, &d.Notes,
			&d.CompletedAt, &d.Outcome, &d.CreatedAt, &d.CreatedBy, &d.UpdatedAt, &d.UpdatedBy,
		); err != nil {
			return nil, 0, err
		}
		requests = append(requests, d)
	}
	return requests, total, rows.Err()
}

// GetDSARRequest returns a DSAR request by ID.
func (store *Store) GetDSARRequest(ctx context.Context, id string) (*DSARRequest, error) {
	q := fmt.Sprintf(`SELECT %s FROM dsar_request WHERE id = $1`, dsarColumns)
	return scanDSAR(store.pool.QueryRow(ctx, q, id))
}

// CreateDSARRequestParams holds fields for creating a DSAR request.
type CreateDSARRequestParams struct {
	SubjectID string
	Contact   *string
	Type      string // ACCESS | ERASURE
	Notes     *string
	CreatedBy string
}

// CreateDSARRequest inserts a new DSAR request.
func (store *Store) CreateDSARRequest(ctx context.Context, p CreateDSARRequestParams) (*DSARRequest, error) {
	q := fmt.Sprintf(`
		INSERT INTO dsar_request (id, subject_id, contact, type, status, notes, created_by, "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, 'PENDING', $4, $5, NOW(), NOW())
		RETURNING %s
	`, dsarColumns)
	return scanDSAR(store.pool.QueryRow(ctx, q, p.SubjectID, p.Contact, p.Type, p.Notes, p.CreatedBy))
}

// UpdateDSARParams holds optional fields for updating a DSAR request.
type UpdateDSARParams struct {
	Status      *string
	Notes       *string
	CompletedAt *time.Time
	Outcome     json.RawMessage // nil = no change
	UpdatedBy   *string
}

// UpdateDSARRequest updates a DSAR request using COALESCE.
func (store *Store) UpdateDSARRequest(ctx context.Context, id string, p UpdateDSARParams) (*DSARRequest, error) {
	q := fmt.Sprintf(`UPDATE dsar_request SET
		status = COALESCE($2, status),
		notes = COALESCE($3, notes),
		completed_at = COALESCE($4, completed_at),
		outcome = COALESCE($5, outcome),
		updated_by = COALESCE($6, updated_by),
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, dsarColumns)
	return scanDSAR(store.pool.QueryRow(ctx, q, id, p.Status, p.Notes, p.CompletedAt, p.Outcome, p.UpdatedBy))
}

// DSARAccessExport holds data for ACCESS fulfillment — actual rows capped at 10K per source.
type DSARAccessExport struct {
	VKRows    []map[string]any `json:"vk"`
	AgentRows []map[string]any `json:"agent"`
	Devices   []map[string]any `json:"devices"` // devices assigned to this user (for context)
}

// FulfillDSARAccess queries traffic_event for a NexusUser and returns all related data.
// subjectID is the NexusUser.id. It finds:
//   - AI Gateway traffic via entity_id (= NexusUser.id)
//   - Agent traffic via DeviceAssignment joined on thing_id (= agent Thing ID),
//     respecting assignment time windows
func (store *Store) FulfillDSARAccess(ctx context.Context, subjectID string) (*DSARAccessExport, error) {
	const maxRows = 10000
	result := &DSARAccessExport{}

	// 1. AI Gateway traffic: entity_id matches the NexusUser ID
	vkRows, err := store.pool.Query(ctx, `
		SELECT id, timestamp, COALESCE(provider_name,''), method, path, status_code,
			model_name, estimated_cost_usd, prompt_tokens, completion_tokens
		FROM traffic_event
		WHERE source = 'ai-gateway' AND entity_id = $1
		ORDER BY timestamp DESC LIMIT $2
	`, subjectID, maxRows)
	if err != nil {
		return nil, fmt.Errorf("dsar access vk query: %w", err)
	}
	for vkRows.Next() {
		var id, provider string
		var method, path, model *string
		var ts any
		var statusCode, pt, ct *int
		var cost *float64
		if err := vkRows.Scan(&id, &ts, &provider, &method, &path, &statusCode, &model, &cost, &pt, &ct); err == nil {
			result.VKRows = append(result.VKRows, map[string]any{
				"id": id, "timestamp": ts, "provider": provider,
				"method": method, "path": path, "statusCode": statusCode,
				"modelUsed": model, "estimatedCostUsd": cost,
				"promptTokens": pt, "completionTokens": ct,
			})
		}
	}
	vkRows.Close()

	// 2. Agent traffic: find devices assigned to this user, then query traffic
	//    within each assignment window (assignedAt <= timestamp < releasedAt).
	agentRows, err := store.pool.Query(ctx, `
		SELECT t.id, t.timestamp, t.thing_id,
			COALESCE(t.source_process,''), COALESCE(t.target_host,''),
			t.action, t.request_hook_decision, t.latency_ms
		FROM traffic_event t
		JOIN "DeviceAssignment" da ON da."deviceId" = t.thing_id
		WHERE da."userId" = $1
		  AND t.source = 'agent'
		  AND t.timestamp >= da."assignedAt"
		  AND (da."releasedAt" IS NULL OR t.timestamp < da."releasedAt")
		ORDER BY t.timestamp DESC LIMIT $2
	`, subjectID, maxRows)
	if err != nil {
		return nil, fmt.Errorf("dsar access agent query: %w", err)
	}
	for agentRows.Next() {
		var id, srcProc, destHost string
		var deviceID *string
		var ts any
		var action, hookDec *string
		var latency *int
		if err := agentRows.Scan(&id, &ts, &deviceID, &srcProc, &destHost, &action, &hookDec, &latency); err == nil {
			result.AgentRows = append(result.AgentRows, map[string]any{
				"id": id, "timestamp": ts, "deviceId": deviceID,
				"sourceProcess": srcProc, "destHost": destHost,
				"action": action, "hookDecision": hookDec, "latencyMs": latency,
			})
		}
	}
	agentRows.Close()

	// 3. Device assignment history for context
	devRows, err := store.pool.Query(ctx, `
		SELECT da."deviceId", COALESCE(t.hostname, ''), da."assignedAt", da."releasedAt"
		FROM "DeviceAssignment" da
		JOIN thing t ON t.id = da."deviceId"
		WHERE da."userId" = $1
		ORDER BY da."assignedAt" DESC
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("dsar device assignments: %w", err)
	}
	for devRows.Next() {
		var deviceID, hostname string
		var assignedAt any
		var releasedAt *time.Time
		if err := devRows.Scan(&deviceID, &hostname, &assignedAt, &releasedAt); err == nil {
			result.Devices = append(result.Devices, map[string]any{
				"deviceId": deviceID, "hostname": hostname,
				"assignedAt": assignedAt, "releasedAt": releasedAt,
			})
		}
	}
	devRows.Close()

	return result, nil
}

// DSARErasureResult holds counts for ERASURE fulfillment.
type DSARErasureResult struct {
	VKAnonymised    int `json:"vkAnonymised"`
	AgentAnonymised int `json:"agentAnonymised"`
	TotalAnonymised int `json:"totalAnonymised"`
}

// FulfillDSARErasure anonymises all traffic data for a NexusUser.
// subjectID is the NexusUser.id. It anonymises:
//   - AI Gateway traffic via entity_id (= NexusUser.id)
//   - Agent traffic via DeviceAssignment joined on thing_id, respecting
//     assignment time windows
func (store *Store) FulfillDSARErasure(ctx context.Context, subjectID string) (*DSARErasureResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin erasure tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	result := &DSARErasureResult{}

	// 1. Anonymise VK traffic
	tag1, err := tx.Exec(ctx, `
		UPDATE traffic_event
		SET entity_id = NULL, source_ip = NULL
		WHERE source = 'ai-gateway' AND entity_id = $1
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("anonymise vk traffic: %w", err)
	}
	result.VKAnonymised = int(tag1.RowsAffected())

	// 2. Anonymise agent traffic within assignment windows
	tag2, err := tx.Exec(ctx, `
		UPDATE traffic_event t
		SET source_ip = NULL, source_process = NULL
		FROM "DeviceAssignment" da
		WHERE da."userId" = $1
		  AND t.thing_id = da."deviceId"
		  AND t.source = 'agent'
		  AND t.timestamp >= da."assignedAt"
		  AND (da."releasedAt" IS NULL OR t.timestamp < da."releasedAt")
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("anonymise agent traffic: %w", err)
	}
	result.AgentAnonymised = int(tag2.RowsAffected())

	result.TotalAnonymised = result.VKAnonymised + result.AgentAnonymised

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit erasure: %w", err)
	}
	return result, nil
}

// DSARStatusCounts holds counts by DSAR status.
type DSARStatusCounts struct {
	Pending    int `json:"pending"`
	InProgress int `json:"inProgress"`
	Completed  int `json:"completed"`
	Rejected   int `json:"rejected"`
}

// GetDSARStatusCounts returns counts of DSAR requests by status using a single query.
func (store *Store) GetDSARStatusCounts(ctx context.Context) (*DSARStatusCounts, error) {
	var s DSARStatusCounts
	err := store.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = 'PENDING'),
			COUNT(*) FILTER (WHERE status = 'IN_PROGRESS'),
			COUNT(*) FILTER (WHERE status = 'COMPLETED'),
			COUNT(*) FILTER (WHERE status = 'REJECTED')
		FROM dsar_request
	`).Scan(&s.Pending, &s.InProgress, &s.Completed, &s.Rejected)
	if err != nil {
		return nil, fmt.Errorf("dsar status counts: %w", err)
	}
	return &s, nil
}

// GetDSARCompletedInPeriod returns count of DSAR requests completed in the given period.
func (store *Store) GetDSARCompletedInPeriod(ctx context.Context, start, end time.Time) (int, error) {
	var count int
	err := store.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM dsar_request
		WHERE status = 'COMPLETED' AND completed_at >= $1 AND completed_at <= $2
	`, start, end).Scan(&count)
	return count, err
}
