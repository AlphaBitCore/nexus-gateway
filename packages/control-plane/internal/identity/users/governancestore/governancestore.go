// Package governancestore owns cross-path user governance queries:
// user audit events, virtual-key revocation, device revocation, and
// user suspension. Extracted from store/cross_path_governance.go so
// the identity/users/handler can depend on this narrow package directly
// instead of routing through the *store.DB god object.
package governancestore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PgxPool is the minimal pgx surface governancestore needs.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store owns the cross-path user governance query surface.
type Store struct {
	pool PgxPool
}

// New constructs a Store from a pool.
func New(pool PgxPool) *Store { return &Store{pool: pool} }

// AuditEventRow represents a row from the traffic_event table for user audit views.
type AuditEventRow struct {
	ID           string          `json:"id"`
	Source       string          `json:"source"`
	Timestamp    time.Time       `json:"timestamp"`
	TargetHost   *string         `json:"targetHost"`
	LatencyMs    *int            `json:"latencyMs"`
	EntityID     *string         `json:"entityId"`
	EntityType   *string         `json:"entityType"`
	HookDecision *string         `json:"hookDecision"`
	Details      json.RawMessage `json:"details"`
}

// UserVirtualKeySummary is a slim VirtualKey view for identity summary.
type UserVirtualKeySummary struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// UserDeviceSummary is a slim device view for identity summary.
type UserDeviceSummary struct {
	ID         string    `json:"id"`
	Hostname   string    `json:"hostname"`
	OS         string    `json:"os"`
	Status     string    `json:"status"`
	AssignedAt time.Time `json:"assignedAt"`
}

// UserAuditSummary holds per-source event counts and last activity for identity view.
type UserAuditSummary struct {
	TotalEvents  int        `json:"totalEvents"`
	VKEvents     int        `json:"vkEvents"`
	ProxyEvents  int        `json:"proxyEvents"`
	AgentEvents  int        `json:"agentEvents"`
	LastActivity *time.Time `json:"lastActivity"`
}

// GetUserAuditEvents returns audit events for a user across all paths.
// Correlates non-agent traffic via entity_id and agent traffic via thing_id
// joined back to DeviceAssignment for the user.
func (s *Store) GetUserAuditEvents(ctx context.Context, userID string, limit, offset int) ([]AuditEventRow, int, error) {
	var total int
	err := s.pool.QueryRow(ctx, `
		WITH user_devices AS (
			SELECT "deviceId" FROM "DeviceAssignment" WHERE "userId" = $1
		)
		SELECT COUNT(*) FROM traffic_event
		WHERE entity_id = $1
		   OR (source = 'agent' AND thing_id IN (SELECT "deviceId" FROM user_devices))
	`, userID).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count user audit events: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		WITH user_devices AS (
			SELECT "deviceId" FROM "DeviceAssignment" WHERE "userId" = $1
		)
		SELECT id, source, timestamp, target_host, latency_ms,
		       entity_id, entity_type, request_hook_decision, details
		FROM traffic_event
		WHERE entity_id = $1
		   OR (source = 'agent' AND thing_id IN (SELECT "deviceId" FROM user_devices))
		ORDER BY timestamp DESC
		LIMIT $2 OFFSET $3
	`, userID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query user audit events: %w", err)
	}
	defer rows.Close()

	events := []AuditEventRow{}
	for rows.Next() {
		var e AuditEventRow
		if err := rows.Scan(
			&e.ID, &e.Source, &e.Timestamp, &e.TargetHost, &e.LatencyMs,
			&e.EntityID, &e.EntityType, &e.HookDecision, &e.Details,
		); err != nil {
			return nil, 0, fmt.Errorf("scan user audit event: %w", err)
		}
		events = append(events, e)
	}
	return events, total, rows.Err()
}

// DisableVirtualKeysByOwner disables all VirtualKeys owned by a user.
// Returns the number of keys disabled.
func (s *Store) DisableVirtualKeysByOwner(ctx context.Context, ownerID string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE "VirtualKey" SET enabled = false, "updatedAt" = NOW()
		WHERE "ownerId" = $1 AND enabled = true
	`, ownerID)
	if err != nil {
		return 0, fmt.Errorf("disable virtual keys by owner: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RevokeDevicesByUser revokes all devices currently assigned to a user.
// Returns the number of devices revoked.
func (s *Store) RevokeDevicesByUser(ctx context.Context, userID string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE thing SET status = 'revoked', updated_at = NOW()
		WHERE id IN (
			SELECT "deviceId" FROM "DeviceAssignment"
			WHERE "userId" = $1 AND "releasedAt" IS NULL
		)
		AND type = 'agent'
		AND status != 'revoked'
	`, userID)
	if err != nil {
		return 0, fmt.Errorf("revoke devices by user: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SuspendUser sets a NexusUser status to 'suspended'.
func (s *Store) SuspendUser(ctx context.Context, userID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE "NexusUser" SET status = 'suspended', "updatedAt" = NOW()
		WHERE id = $1
	`, userID)
	if err != nil {
		return fmt.Errorf("suspend user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("suspend user: user not found")
	}
	return nil
}

// ListVirtualKeysByOwner returns a slim list of VirtualKeys for a given owner.
func (s *Store) ListVirtualKeysByOwner(ctx context.Context, ownerID string) ([]UserVirtualKeySummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, enabled, "createdAt"
		FROM "VirtualKey"
		WHERE "ownerId" = $1
		ORDER BY "createdAt" DESC
	`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list virtual keys by owner: %w", err)
	}
	defer rows.Close()

	keys := []UserVirtualKeySummary{}
	for rows.Next() {
		var k UserVirtualKeySummary
		if err := rows.Scan(&k.ID, &k.Name, &k.Enabled, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan virtual key summary: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// ListActiveDevicesByUser returns currently assigned devices for a user (slim view).
func (s *Store) ListActiveDevicesByUser(ctx context.Context, userID string) ([]UserDeviceSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, COALESCE(t.hostname, ''), COALESCE(t.os, ''), t.status, da."assignedAt"
		FROM "DeviceAssignment" da
		JOIN thing t ON t.id = da."deviceId"
		WHERE da."userId" = $1 AND da."releasedAt" IS NULL
		ORDER BY da."assignedAt" DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list active devices by user: %w", err)
	}
	defer rows.Close()

	devices := []UserDeviceSummary{}
	for rows.Next() {
		var d UserDeviceSummary
		if err := rows.Scan(&d.ID, &d.Hostname, &d.OS, &d.Status, &d.AssignedAt); err != nil {
			return nil, fmt.Errorf("scan device summary: %w", err)
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// GetUserAuditSummary returns per-source event counts and last activity timestamp.
func (s *Store) GetUserAuditSummary(ctx context.Context, userID string) (*UserAuditSummary, error) {
	var summary UserAuditSummary
	err := s.pool.QueryRow(ctx, `
		WITH user_devices AS (
			SELECT "deviceId" FROM "DeviceAssignment" WHERE "userId" = $1
		)
		SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE source = 'ai-gateway'),
			COUNT(*) FILTER (WHERE source = 'compliance-proxy'),
			COUNT(*) FILTER (WHERE source = 'agent'),
			MAX(timestamp)
		FROM traffic_event
		WHERE entity_id = $1
		   OR (source = 'agent' AND thing_id IN (SELECT "deviceId" FROM user_devices))
	`, userID).Scan(&summary.TotalEvents, &summary.VKEvents, &summary.ProxyEvents, &summary.AgentEvents, &summary.LastActivity)
	if err != nil {
		return nil, fmt.Errorf("user audit summary: %w", err)
	}
	return &summary, nil
}
