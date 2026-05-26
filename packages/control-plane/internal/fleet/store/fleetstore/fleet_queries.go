package fleetstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// FleetUserDevice is a ThingNode with its assignment info for fleet user views.
type FleetUserDevice struct {
	ID            string     `json:"id"`
	Hostname      string     `json:"hostname"`
	OS            string     `json:"os"`
	OSVersion     string     `json:"osVersion"`
	AgentVersion  string     `json:"agentVersion"`
	Status        string     `json:"status"`
	LastHeartbeat *time.Time `json:"lastHeartbeat"`
	AssignedAt    time.Time  `json:"assignedAt"`
	Source        string     `json:"assignmentSource"`
}

// ListDevicesByUserID returns devices currently assigned to a user.
func (store *Store) ListDevicesByUserID(ctx context.Context, userID string, limit, offset int) ([]FleetUserDevice, int, error) {
	where := `WHERE da."userId" = $1 AND da."releasedAt" IS NULL`

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT COUNT(*)
		FROM "DeviceAssignment" da
		JOIN thing t ON t.id = da."deviceId"
		%s
	`, where), userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count user devices: %w", err)
	}

	rows, err := store.pool.Query(ctx, fmt.Sprintf(`
		SELECT t.id, COALESCE(t.hostname, ''), COALESCE(t.os, ''), COALESCE(t.os_version, ''),
		       COALESCE(t.version, ''), t.status,
		       t.last_seen_at, da."assignedAt", da.source
		FROM "DeviceAssignment" da
		JOIN thing t ON t.id = da."deviceId"
		%s
		ORDER BY da."assignedAt" DESC
		LIMIT $2 OFFSET $3
	`, where), userID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list user devices: %w", err)
	}
	defer rows.Close()

	devices := []FleetUserDevice{}
	for rows.Next() {
		var d FleetUserDevice
		if err := rows.Scan(
			&d.ID, &d.Hostname, &d.OS, &d.OSVersion, &d.AgentVersion, &d.Status,
			&d.LastHeartbeat, &d.AssignedAt, &d.Source,
		); err != nil {
			return nil, 0, fmt.Errorf("scan user device: %w", err)
		}
		devices = append(devices, d)
	}
	return devices, total, rows.Err()
}

// AuditEventListParams holds filter/pagination for querying audit_event by identity.
type AuditEventListParams struct {
	SubjectID string
	DeviceID  string
	StartTime *time.Time
	EndTime   *time.Time
	Limit     int
	Offset    int
}

// AuditEventRow represents a row from the traffic_event table for fleet views.
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

// ListAuditEventsBySubjectID returns audit events for a given entity (by entity_id).
func (store *Store) ListAuditEventsBySubjectID(ctx context.Context, p AuditEventListParams) ([]AuditEventRow, int, error) {
	where := `WHERE entity_id = $1`
	args := []any{p.SubjectID}
	argIdx := 2

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

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM traffic_event %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count subject audit events: %w", err)
	}

	q := fmt.Sprintf(`
		SELECT id, source, timestamp, target_host, latency_ms,
		       entity_id, entity_type, request_hook_decision, details
		FROM traffic_event %s
		ORDER BY timestamp DESC
		LIMIT $%d OFFSET $%d
	`, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	events, err := store.queryAuditEventRows(ctx, q, args)
	if err != nil {
		return nil, 0, err
	}
	return events, total, nil
}

// ListAuditEventsByDeviceID returns audit events for a given device (thing_id with source=agent).
// Agent uploads stamp thing_id with the agent's Thing ID; entity_id is reserved
// for subject attribution (typically empty for agent traffic).
func (store *Store) ListAuditEventsByDeviceID(ctx context.Context, p AuditEventListParams) ([]AuditEventRow, int, error) {
	where := `WHERE thing_id = $1 AND source = 'agent'`
	args := []any{p.DeviceID}
	argIdx := 2

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

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM traffic_event %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count device audit events: %w", err)
	}

	q := fmt.Sprintf(`
		SELECT id, source, timestamp, target_host, latency_ms,
		       entity_id, entity_type, request_hook_decision, details
		FROM traffic_event %s
		ORDER BY timestamp DESC
		LIMIT $%d OFFSET $%d
	`, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	events, err := store.queryAuditEventRows(ctx, q, args)
	if err != nil {
		return nil, 0, err
	}
	return events, total, nil
}

func (store *Store) queryAuditEventRows(ctx context.Context, q string, args []any) ([]AuditEventRow, error) {
	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit events: %w", err)
	}
	defer rows.Close()

	events := []AuditEventRow{}
	for rows.Next() {
		var e AuditEventRow
		if err := rows.Scan(
			&e.ID, &e.Source, &e.Timestamp, &e.TargetHost, &e.LatencyMs,
			&e.EntityID, &e.EntityType, &e.HookDecision, &e.Details,
		); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// DeviceAssignmentDetail is a DeviceAssignment with joined NexusUser info.
type DeviceAssignmentDetail struct {
	ID              string     `json:"id"`
	DeviceID        string     `json:"deviceId"`
	UserID          string     `json:"userId"`
	AssignedAt      time.Time  `json:"assignedAt"`
	ReleasedAt      *time.Time `json:"releasedAt"`
	Source          string     `json:"source"`
	LoginMethod     *string    `json:"loginMethod"`
	IPAddress       *string    `json:"ipAddress"`
	TokenJti        *string    `json:"tokenJti"`
	UserDisplayName *string    `json:"userDisplayName"`
	UserOSUsername  *string    `json:"userOsUsername"`
	UserOSDomain    *string    `json:"userOsDomain"`
}

const deviceAssignmentSelectSQL = `
	SELECT da.id, da."deviceId", da."userId", da."assignedAt", da."releasedAt", da.source,
	       da.login_method, da.ip_address, da.token_jti,
	       u."displayName", u."osUsername", u."osDomain"
	FROM "DeviceAssignment" da
	LEFT JOIN "NexusUser" u ON u.id = da."userId"`

func scanDeviceAssignmentDetail(rows interface {
	Scan(dest ...any) error
}, d *DeviceAssignmentDetail) error {
	return rows.Scan(
		&d.ID, &d.DeviceID, &d.UserID, &d.AssignedAt, &d.ReleasedAt, &d.Source,
		&d.LoginMethod, &d.IPAddress, &d.TokenJti,
		&d.UserDisplayName, &d.UserOSUsername, &d.UserOSDomain,
	)
}

// ListDeviceAssignments returns all assignments (active + released) for a device.
func (store *Store) ListDeviceAssignments(ctx context.Context, deviceID string) ([]DeviceAssignmentDetail, error) {
	rows, err := store.pool.Query(ctx,
		deviceAssignmentSelectSQL+` WHERE da."deviceId" = $1 ORDER BY da."assignedAt" DESC`,
		deviceID)
	if err != nil {
		return nil, fmt.Errorf("list device assignments: %w", err)
	}
	defer rows.Close()

	var result []DeviceAssignmentDetail
	for rows.Next() {
		var d DeviceAssignmentDetail
		if err := scanDeviceAssignmentDetail(rows, &d); err != nil {
			return nil, fmt.Errorf("scan device assignment: %w", err)
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// ListDeviceAssignmentsByDevice is the paginated variant the admin Device
// Detail page uses. Returns rows + total count for the device. Same shape
// as ListDeviceAssignments but with LIMIT/OFFSET and a separate COUNT(*).
func (store *Store) ListDeviceAssignmentsByDevice(ctx context.Context, deviceID string, limit, offset int) ([]DeviceAssignmentDetail, int, error) {
	if limit <= 0 {
		limit = 25
	}
	var total int
	if err := store.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM "DeviceAssignment" WHERE "deviceId" = $1`,
		deviceID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count device assignments: %w", err)
	}

	rows, err := store.pool.Query(ctx,
		deviceAssignmentSelectSQL+` WHERE da."deviceId" = $1 ORDER BY da."assignedAt" DESC LIMIT $2 OFFSET $3`,
		deviceID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list device assignments paged: %w", err)
	}
	defer rows.Close()

	result := []DeviceAssignmentDetail{}
	for rows.Next() {
		var d DeviceAssignmentDetail
		if err := scanDeviceAssignmentDetail(rows, &d); err != nil {
			return nil, 0, fmt.Errorf("scan device assignment: %w", err)
		}
		result = append(result, d)
	}
	return result, total, rows.Err()
}

// ListDeviceAssignmentsByUser returns all assignments (active + released) for a user,
// ordered by most recent first.
func (store *Store) ListDeviceAssignmentsByUser(ctx context.Context, userID string) ([]DeviceAssignmentDetail, error) {
	rows, err := store.pool.Query(ctx,
		deviceAssignmentSelectSQL+` WHERE da."userId" = $1 ORDER BY da."assignedAt" DESC`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("list device assignments by user: %w", err)
	}
	defer rows.Close()

	var result []DeviceAssignmentDetail
	for rows.Next() {
		var d DeviceAssignmentDetail
		if err := scanDeviceAssignmentDetail(rows, &d); err != nil {
			return nil, fmt.Errorf("scan device assignment by user: %w", err)
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
