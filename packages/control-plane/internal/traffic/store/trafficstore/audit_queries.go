package trafficstore

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
)

// AdminAuditLogEntry represents a row from the AdminAuditLog table.
type AdminAuditLogEntry struct {
	ID             string          `json:"id"`
	SequenceNumber int             `json:"sequenceNumber"`
	Timestamp      time.Time       `json:"timestamp"`
	ActorID        string          `json:"actorId"`
	ActorLabel     string          `json:"actorLabel"`
	ActorRole      *string         `json:"actorRole"`
	SourceIP       *string         `json:"sourceIp"`
	Action         string          `json:"action"`
	EntityType     string          `json:"entityType"`
	EntityID       *string         `json:"entityId"`
	BeforeState    json.RawMessage `json:"beforeState"`
	AfterState     json.RawMessage `json:"afterState"`
	NexusRequestID *string         `json:"nexusRequestId"`
}

// AdminAuditLogListParams holds filter/pagination for admin audit logs.
type AdminAuditLogListParams struct {
	StartTime      *time.Time
	EndTime        *time.Time
	ActorID        string
	ActorLabel     string
	ActorRole      string
	Action         string
	EntityType     string
	NexusRequestID string
	Limit          int
	Offset         int
}

const adminAuditColumns = `id, "sequenceNumber", timestamp, "actorId", "actorLabel", "actorRole",
	"sourceIp", action, "entityType", "entityId", "beforeState", "afterState",
	"nexusRequestId"`

// ListAdminAuditLogs returns admin audit logs with filtering.
func (store *Store) ListAdminAuditLogs(ctx context.Context, p AdminAuditLogListParams) ([]AdminAuditLogEntry, int, error) {
	where, args, argIdx := buildAdminAuditWhere(p)

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "AdminAuditLog" %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count admin audit logs: %w", err)
	}

	q := fmt.Sprintf(`SELECT %s FROM "AdminAuditLog" %s ORDER BY timestamp DESC, id DESC LIMIT $%d OFFSET $%d`,
		adminAuditColumns, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list admin audit logs: %w", err)
	}
	defer rows.Close()

	entries, _, err := scanAdminAuditRows(rows)
	if err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}

// ExportAdminAuditLogs returns up to maxRows admin audit logs.
func (store *Store) ExportAdminAuditLogs(ctx context.Context, p AdminAuditLogListParams, maxRows int) ([]AdminAuditLogEntry, error) {
	where, args, argIdx := buildAdminAuditWhere(p)

	q := fmt.Sprintf(`SELECT %s FROM "AdminAuditLog" %s ORDER BY timestamp DESC, id DESC LIMIT $%d`,
		adminAuditColumns, where, argIdx)
	args = append(args, maxRows)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("export admin audit logs: %w", err)
	}
	defer rows.Close()

	entries, _, err := scanAdminAuditRows(rows)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func scanAdminAuditRows(rows pgx.Rows) ([]AdminAuditLogEntry, int, error) {
	entries := []AdminAuditLogEntry{}
	for rows.Next() {
		var e AdminAuditLogEntry
		if err := rows.Scan(
			&e.ID, &e.SequenceNumber, &e.Timestamp, &e.ActorID, &e.ActorLabel, &e.ActorRole,
			&e.SourceIP, &e.Action, &e.EntityType, &e.EntityID, &e.BeforeState, &e.AfterState,
			&e.NexusRequestID,
		); err != nil {
			return nil, 0, fmt.Errorf("scan admin audit log: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, len(entries), rows.Err()
}

func buildAdminAuditWhere(p AdminAuditLogListParams) (string, []any, int) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.StartTime != nil {
		where += fmt.Sprintf(` AND timestamp >= $%d`, argIdx)
		args = append(args, *p.StartTime)
		argIdx++
	}
	if p.EndTime != nil {
		where += fmt.Sprintf(` AND timestamp <= $%d`, argIdx)
		args = append(args, *p.EndTime)
		argIdx++
	}
	if p.ActorID != "" {
		where += fmt.Sprintf(` AND "actorId" = $%d`, argIdx)
		args = append(args, p.ActorID)
		argIdx++
	}
	if p.ActorLabel != "" {
		where += fmt.Sprintf(` AND "actorLabel" ILIKE $%d`, argIdx)
		args = append(args, "%"+escapeILIKE(p.ActorLabel)+"%")
		argIdx++
	}
	if p.ActorRole != "" {
		where += fmt.Sprintf(` AND "actorRole" = $%d`, argIdx)
		args = append(args, p.ActorRole)
		argIdx++
	}
	if p.Action != "" {
		where += fmt.Sprintf(` AND action = $%d`, argIdx)
		args = append(args, p.Action)
		argIdx++
	}
	if p.EntityType != "" {
		where += fmt.Sprintf(` AND "entityType" = $%d`, argIdx)
		args = append(args, p.EntityType)
		argIdx++
	}
	if p.NexusRequestID != "" {
		where += fmt.Sprintf(` AND "nexusRequestId" = $%d`, argIdx)
		args = append(args, p.NexusRequestID)
		argIdx++
	}

	return where, args, argIdx
}

// AuditEventTypePair holds a distinct entityType+action combination from AdminAuditLog.
type AuditEventTypePair struct {
	EntityType string
	Action     string
}

// ListDistinctAuditEventTypes returns all distinct (entityType, action) pairs
// from AdminAuditLog for populating the SIEM event type selector.
func (store *Store) ListDistinctAuditEventTypes(ctx context.Context) ([]AuditEventTypePair, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT DISTINCT "entityType", "action"
		FROM "AdminAuditLog"
		WHERE "entityType" != '' AND "action" != ''
		ORDER BY "entityType", "action"
	`)
	if err != nil {
		return nil, fmt.Errorf("list distinct audit event types: %w", err)
	}
	defer rows.Close()

	var pairs []AuditEventTypePair
	for rows.Next() {
		var p AuditEventTypePair
		if err := rows.Scan(&p.EntityType, &p.Action); err != nil {
			return nil, fmt.Errorf("scan audit event type pair: %w", err)
		}
		pairs = append(pairs, p)
	}
	return pairs, rows.Err()
}
