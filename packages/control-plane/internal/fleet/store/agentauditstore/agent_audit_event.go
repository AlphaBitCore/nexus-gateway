package agentauditstore

import (
	"context"
	"fmt"
	"time"
)

// AgentTrafficEvent represents a row from the traffic_event table for agent events.
type AgentTrafficEvent struct {
	ID            string    `json:"id"`
	DeviceID      string    `json:"deviceId"`
	Timestamp     time.Time `json:"timestamp"`
	SourceProcess string    `json:"sourceProcess"`
	SourceUser    *string   `json:"sourceUser"`
	SubjectID     *string   `json:"subjectId"`
	DestHost      string    `json:"destHost"`
	DestIP        string    `json:"destIp"`
	DestPort      int       `json:"destPort"`
	Action        string    `json:"action"`
	PolicyRuleID  *string   `json:"policyRuleId"`
	BumpStatus    *string   `json:"bumpStatus"`
	BytesIn       *int      `json:"bytesIn"`
	BytesOut      *int      `json:"bytesOut"`
	Duration      *int      `json:"duration"`
	HookDecision  *string   `json:"hookDecision"`
	CreatedAt     time.Time `json:"createdAt"`
	// Joined fields
	DeviceHostname *string `json:"deviceHostname,omitempty"`
	DeviceOS       *string `json:"deviceOs,omitempty"`
}

// AgentAuditEventInput is a single event from agent audit upload.
type AgentAuditEventInput struct {
	ID             string   `json:"id"`
	Timestamp      string   `json:"timestamp"`
	SourceProcess  string   `json:"sourceProcess"`
	SourceUser     *string  `json:"sourceUser"`
	SubjectID      *string  `json:"subjectId"`
	DestHost       string   `json:"destHost"`
	DestIP         string   `json:"destIp"`
	DestPort       int      `json:"destPort"`
	Action         string   `json:"action"`
	PolicyRuleID   *string  `json:"policyRuleId"`
	BumpStatus     *string  `json:"bumpStatus"`
	BytesIn        *int     `json:"bytesIn"`
	BytesOut       *int     `json:"bytesOut"`
	DurationMs     *int     `json:"durationMs"`
	HookDecision   *string  `json:"hookDecision"`
	ComplianceTags []string `json:"complianceTags"`
}

// AgentTrafficEventListParams holds filter/pagination for agent traffic events.
type AgentTrafficEventListParams struct {
	Q         string
	DeviceID  string
	Action    string
	StartTime *time.Time
	EndTime   *time.Time
	Limit     int
	Offset    int
}

// ListAgentTrafficEvents returns agent traffic events with filtering.
func (store *Store) ListAgentTrafficEvents(ctx context.Context, p AgentTrafficEventListParams) ([]AgentTrafficEvent, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	where += ` AND e.source = 'agent'`

	if p.DeviceID != "" {
		// Agent uploads stamp thing_id with the agent's Thing ID and
		// leave entity_id empty; subject attribution (when present) is
		// projected from entity_id below into SubjectID, but the device
		// identity that this filter targets lives in thing_id.
		where += fmt.Sprintf(` AND e.thing_id = $%d`, argIdx)
		args = append(args, p.DeviceID)
		argIdx++
	}
	if p.Action != "" {
		where += fmt.Sprintf(` AND e.action = $%d`, argIdx)
		args = append(args, p.Action)
		argIdx++
	}
	if p.Q != "" {
		where += fmt.Sprintf(` AND (e.source_process ILIKE $%d OR e.target_host ILIKE $%d)`, argIdx, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}
	if p.StartTime != nil {
		where += fmt.Sprintf(` AND e.timestamp >= $%d`, argIdx)
		args = append(args, *p.StartTime)
		argIdx++
	}
	if p.EndTime != nil {
		where += fmt.Sprintf(` AND e.timestamp <= $%d`, argIdx)
		args = append(args, *p.EndTime)
		argIdx++
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM traffic_event e %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count agent events: %w", err)
	}

	q := fmt.Sprintf(`
		SELECT e.id, COALESCE(e.thing_id, ''), e.timestamp,
		       COALESCE(e.source_process, ''),
		       e.details->>'sourceUser',
		       e.entity_id,
		       COALESCE(e.target_host, ''),
		       COALESCE(e.details->>'destIp', ''),
		       COALESCE((e.details->>'destPort')::int, 0),
		       COALESCE(e.action, ''),
		       e.details->>'policyRuleId',
		       e.bump_status,
		       (e.details->>'bytesIn')::int,
		       (e.details->>'bytesOut')::int,
		       e.latency_ms,
		       e.request_hook_decision,
		       e.created_at,
		       COALESCE(t.hostname, ''), COALESCE(t.os, '')
		FROM traffic_event e
		LEFT JOIN thing t ON t.id = e.thing_id
		%s
		ORDER BY e.timestamp DESC, e.id DESC
		LIMIT $%d OFFSET $%d
	`, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list agent events: %w", err)
	}
	defer rows.Close()

	events := []AgentTrafficEvent{}
	for rows.Next() {
		var e AgentTrafficEvent
		if err := rows.Scan(
			&e.ID, &e.DeviceID, &e.Timestamp, &e.SourceProcess, &e.SourceUser, &e.SubjectID,
			&e.DestHost, &e.DestIP, &e.DestPort, &e.Action, &e.PolicyRuleID,
			&e.BumpStatus, &e.BytesIn, &e.BytesOut, &e.Duration, &e.HookDecision, &e.CreatedAt,
			&e.DeviceHostname, &e.DeviceOS,
		); err != nil {
			return nil, 0, fmt.Errorf("scan agent event: %w", err)
		}
		events = append(events, e)
	}
	return events, total, rows.Err()
}
