// Package store — agent_device.go
// Provides ThingNode (agent device) CRUD and fleet health queries.
// Data is stored in thing + thing_agent tables.
package agentstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
)

// ThingNode represents an agent node (thing + thing_agent join + current
// DeviceAssignment + NexusUser). PhysicalID + PrimaryIP land here for
// the Devices list / detail page identity panels; the bound-user trio
// surfaces who currently owns the device (NULL for unbound rows).
type ThingNode struct {
	ID            string          `json:"id"`
	Hostname      string          `json:"hostname"`
	OS            string          `json:"os"`
	OSVersion     string          `json:"osVersion"`
	AgentVersion  string          `json:"agentVersion"`
	Status        string          `json:"status"` // enrolled | online | offline | drift | revoked
	LastHeartbeat *time.Time      `json:"lastHeartbeat"`
	EnrolledAt    time.Time       `json:"enrolledAt"`
	EnrolledBy    string          `json:"enrolledBy"`
	CertSerial    *string         `json:"certSerial"`
	CertExpiresAt *time.Time      `json:"certExpiresAt"`
	Metadata      json.RawMessage `json:"metadata"`
	Sysinfo       json.RawMessage `json:"sysinfo,omitempty"`
	EventCount    *int            `json:"_count,omitempty"`
	// Identity fields promoted to thing top-level by migration
	// 20260522_thing_identity_columns.
	PrimaryIP  string `json:"primaryIp,omitempty"`
	PhysicalID string `json:"physicalId,omitempty"`
	// Bound user — currently-active DeviceAssignment.
	BoundUserID          string `json:"boundUserId,omitempty"`
	BoundUserDisplayName string `json:"boundUserDisplayName,omitempty"`
	BoundUserEmail       string `json:"boundUserEmail,omitempty"`
	// Free-form tags. Backed by `thing.tags TEXT[]`.
	Tags []string `json:"tags,omitempty"`
}

// The Go struct field is `Metadata` (json:"metadata") and we read
// it from the `thing.metadata` JSONB column — this is where the Hub
// writes `staticInfo` (sysinfo: hostname, OS, CPU, memory, NICs, …) via
// `UpdateStaticInfo` (see nexus-hub/internal/opsmetrics/static_info_writer.go).
// The legacy `thing.reported` column carries the applied-config shadow
// and is intentionally NOT exposed here — the Devices Detail page never
// renders config shadow, and surfacing it as `metadata` was a long-
// standing column-vs-field-name mismatch that left the System tab empty
// (the frontend looked under metadata.staticInfo for an agent that put
// it in the right place — the bug was on this end).
const agentJoinColumns = `
	t.id, COALESCE(t.hostname, ''), COALESCE(t.os, ''), COALESCE(t.os_version, ''), COALESCE(t.version, ''),
	t.status, t.last_seen_at, t.enrolled_at, COALESCE(t.enrolled_by, ''),
	ta.cert_serial, ta.cert_expires_at, t.metadata, ta.sysinfo,
	COALESCE(t.primary_ip, ''), COALESCE(t.physical_id, ''),
	COALESCE(u.id, ''), COALESCE(u."displayName", ''), COALESCE(u.email, ''),
	COALESCE(t.tags, ARRAY[]::text[])`

const agentJoinFrom = `
	thing t
	JOIN thing_agent ta ON ta.thing_id = t.id
	LEFT JOIN "DeviceAssignment" da ON da."deviceId" = t.id AND da."releasedAt" IS NULL
	LEFT JOIN "NexusUser"        u  ON u.id = da."userId"`

func scanThingNode(row pgx.Row) (*ThingNode, error) {
	var d ThingNode
	err := row.Scan(
		&d.ID, &d.Hostname, &d.OS, &d.OSVersion, &d.AgentVersion,
		&d.Status, &d.LastHeartbeat, &d.EnrolledAt, &d.EnrolledBy,
		&d.CertSerial, &d.CertExpiresAt, &d.Metadata, &d.Sysinfo,
		&d.PrimaryIP, &d.PhysicalID,
		&d.BoundUserID, &d.BoundUserDisplayName, &d.BoundUserEmail,
		&d.Tags,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ThingNodeListParams holds filter/pagination.
type ThingNodeListParams struct {
	Q      string
	Status string
	OS     string
	Limit  int
	Offset int
}

// ListThingNodes returns agent nodes with filtering via thing + thing_agent JOIN.
func (store *Store) ListThingNodes(ctx context.Context, p ThingNodeListParams) ([]ThingNode, int, error) {
	where := "WHERE t.type = 'agent'"
	args := []any{}
	argIdx := 1

	if p.Q != "" {
		where += fmt.Sprintf(` AND (t.hostname ILIKE $%d OR ta.cert_serial ILIKE $%d)`, argIdx, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}
	if p.Status != "" {
		where += fmt.Sprintf(` AND t.status = $%d`, argIdx)
		args = append(args, p.Status)
		argIdx++
	}
	if p.OS != "" {
		where += fmt.Sprintf(` AND t.os = $%d`, argIdx)
		args = append(args, p.OS)
		argIdx++
	}

	var total int
	countQ := fmt.Sprintf(`SELECT COUNT(*) FROM %s %s`, agentJoinFrom, where)
	if err := store.pool.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count agent devices: %w", err)
	}

	q := fmt.Sprintf(`
		SELECT %s,
		       (SELECT COUNT(*) FROM traffic_event e WHERE e.thing_id = t.id AND e.source = 'agent') AS event_count
		FROM %s
		%s
		ORDER BY t.enrolled_at DESC
		LIMIT $%d OFFSET $%d
	`, agentJoinColumns, agentJoinFrom, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list agent devices: %w", err)
	}
	defer rows.Close()

	devices := []ThingNode{}
	for rows.Next() {
		var d ThingNode
		var ec int
		if err := rows.Scan(
			&d.ID, &d.Hostname, &d.OS, &d.OSVersion, &d.AgentVersion,
			&d.Status, &d.LastHeartbeat, &d.EnrolledAt, &d.EnrolledBy,
			&d.CertSerial, &d.CertExpiresAt, &d.Metadata, &d.Sysinfo,
			&d.PrimaryIP, &d.PhysicalID,
			&d.BoundUserID, &d.BoundUserDisplayName, &d.BoundUserEmail,
			&d.Tags,
			&ec,
		); err != nil {
			return nil, 0, fmt.Errorf("scan agent device: %w", err)
		}
		d.EventCount = &ec
		devices = append(devices, d)
	}
	return devices, total, rows.Err()
}

// GetThingNode returns an agent node by ID.
func (store *Store) GetThingNode(ctx context.Context, id string) (*ThingNode, error) {
	q := fmt.Sprintf(`
		SELECT %s
		FROM %s
		WHERE t.id = $1`, agentJoinColumns, agentJoinFrom)
	return scanThingNode(store.pool.QueryRow(ctx, q, id))
}

// UpdateThingNodeStatus updates a node's status.
func (store *Store) UpdateThingNodeStatus(ctx context.Context, id string, status string) (*ThingNode, error) {
	_, err := store.pool.Exec(ctx, `UPDATE thing SET status = $2, updated_at = NOW() WHERE id = $1`, id, status)
	if err != nil {
		return nil, fmt.Errorf("update device status: %w", err)
	}
	return store.GetThingNode(ctx, id)
}

// AgentFleetHealth holds fleet health summary.
type AgentFleetHealth struct {
	Total       int `json:"total"`
	Active      int `json:"active"`
	Stale       int `json:"stale"`
	Critical    int `json:"critical"`
	Revoked     int `json:"revoked"`
	StalePct    int `json:"stalePct"`
	CriticalPct int `json:"criticalPct"`
}

// GetAgentFleetHealth returns fleet health summary using thing table.
func (store *Store) GetAgentFleetHealth(ctx context.Context) (*AgentFleetHealth, error) {
	now := time.Now()
	fiveMinAgo := now.Add(-5 * time.Minute)
	fifteenMinAgo := now.Add(-15 * time.Minute)

	h := &AgentFleetHealth{}
	err := store.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status != 'revoked'),
			COUNT(*) FILTER (WHERE status = 'online' AND last_seen_at >= $1),
			COUNT(*) FILTER (WHERE status = 'online' AND last_seen_at < $1 AND last_seen_at >= $2),
			COUNT(*) FILTER (WHERE status = 'online' AND last_seen_at < $2),
			COUNT(*) FILTER (WHERE status = 'revoked')
		FROM thing
		WHERE type = 'agent'
	`, fiveMinAgo, fifteenMinAgo).Scan(&h.Total, &h.Active, &h.Stale, &h.Critical, &h.Revoked)
	if err != nil {
		return nil, fmt.Errorf("fleet health: %w", err)
	}

	if h.Total > 0 {
		h.StalePct = h.Stale * 100 / h.Total
		h.CriticalPct = h.Critical * 100 / h.Total
	}
	return h, nil
}
