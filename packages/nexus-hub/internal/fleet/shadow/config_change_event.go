package shadow

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConfigChangeEvent is a row from config_change_event.
type ConfigChangeEvent struct {
	ID                string    `json:"id"`
	Timestamp         time.Time `json:"timestamp"`
	ThingType         string    `json:"thingType"`
	ConfigKey         string    `json:"configKey"`
	Action            string    `json:"action"`
	ActorID           string    `json:"actorId"`
	ActorName         string    `json:"actorName"`
	NewState          any       `json:"newState"`
	NewVersion        int64     `json:"newVersion"`
	SourceIP          string    `json:"sourceIp"`
	EmergencyOverride bool      `json:"emergencyOverride"`
}

// InsertConfigChangeEvent records a config change audit event. Used inside a transaction.
func (s *Store) InsertConfigChangeEvent(ctx context.Context, tx pgx.Tx, e ConfigChangeEvent) error {
	stateJSON, err := json.Marshal(e.NewState)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	// The id column has no DB-side default (Prisma treats @default(uuid()) as
	// an application-layer default), so we emit one via gen_random_uuid()
	// (PostgreSQL 13+ built-in, no extension required).
	_, err = tx.Exec(ctx, `
		INSERT INTO config_change_event (id, thing_type, config_key, action, actor_id, actor_name, new_state, new_version, source_ip, emergency_override)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, e.ThingType, e.ConfigKey, e.Action, e.ActorID, e.ActorName, stateJSON, e.NewVersion, e.SourceIP, e.EmergencyOverride)
	if err != nil {
		return fmt.Errorf("insert change event: %w", err)
	}
	return nil
}

// ListConfigHistoryParams holds filters for listing config change events.
type ListConfigHistoryParams struct {
	ThingType string
	ConfigKey string
	ActorID   string
	From      *time.Time
	To        *time.Time
	Page      int
	PageSize  int
}

// ListConfigHistoryResult is a paginated list of config change events.
type ListConfigHistoryResult struct {
	Events []ConfigChangeEvent `json:"events"`
	Total  int                 `json:"total"`
}

// normalizeConfigHistoryParams applies defaults for pagination.
func normalizeConfigHistoryParams(p *ListConfigHistoryParams) {
	if p.Page < 1 {
		p.Page = 1
	}
	if p.PageSize < 1 || p.PageSize > 200 {
		p.PageSize = 50
	}
}

// ListConfigHistory returns filtered, paginated config change events.
func (s *Store) ListConfigHistory(ctx context.Context, p ListConfigHistoryParams) (*ListConfigHistoryResult, error) {
	normalizeConfigHistoryParams(&p)
	offset := (p.Page - 1) * p.PageSize

	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.ThingType != "" {
		where += fmt.Sprintf(" AND thing_type = $%d", argIdx)
		args = append(args, p.ThingType)
		argIdx++
	}
	if p.ConfigKey != "" {
		where += fmt.Sprintf(" AND config_key = $%d", argIdx)
		args = append(args, p.ConfigKey)
		argIdx++
	}
	if p.ActorID != "" {
		where += fmt.Sprintf(" AND actor_id = $%d", argIdx)
		args = append(args, p.ActorID)
		argIdx++
	}
	if p.From != nil {
		where += fmt.Sprintf(" AND timestamp >= $%d", argIdx)
		args = append(args, *p.From)
		argIdx++
	}
	if p.To != nil {
		where += fmt.Sprintf(" AND timestamp <= $%d", argIdx)
		args = append(args, *p.To)
		argIdx++
	}

	var total int
	if err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM config_change_event "+where, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count history: %w", err)
	}

	query := fmt.Sprintf(`
		SELECT id, timestamp, thing_type, config_key, action, actor_id, actor_name, new_state, new_version, COALESCE(source_ip, ''), emergency_override
		FROM config_change_event %s
		ORDER BY timestamp DESC
		LIMIT $%d OFFSET $%d
	`, where, argIdx, argIdx+1)
	args = append(args, p.PageSize, offset)

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list history: %w", err)
	}
	defer rows.Close()

	var events []ConfigChangeEvent
	for rows.Next() {
		var e ConfigChangeEvent
		var stateRaw []byte
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.ThingType, &e.ConfigKey, &e.Action, &e.ActorID, &e.ActorName, &stateRaw, &e.NewVersion, &e.SourceIP, &e.EmergencyOverride); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if err := decodeJSONB(stateRaw, &e.NewState, "new_state"); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if events == nil {
		events = []ConfigChangeEvent{}
	}
	return &ListConfigHistoryResult{Events: events, Total: total}, nil
}
