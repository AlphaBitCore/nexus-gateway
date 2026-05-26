package thingstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ThingRegistry represents a row from the thing table.
// A Thing is any Hub-managed node: ai-gateway, compliance-proxy, nexus-hub,
// control-plane, or agent instance.
type ThingRegistry struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Name         *string         `json:"name,omitempty"`
	Version      *string         `json:"version,omitempty"`
	Address      *string         `json:"address,omitempty"`
	EnrolledBy   *string         `json:"enrolledBy,omitempty"`
	AuthType     string          `json:"authType"`
	ConnProtocol string          `json:"connProtocol"`
	Status       string          `json:"status"`
	EnrolledAt   time.Time       `json:"enrolledAt"`
	LastSeenAt   *time.Time      `json:"lastSeenAt,omitempty"`
	UpdatedAt    time.Time       `json:"updatedAt"`
	Desired      json.RawMessage `json:"desired"`
	Reported     json.RawMessage `json:"reported"`
	DesiredVer   int64           `json:"desiredVer"`
	ReportedVer  int64           `json:"reportedVer"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
}

// ThingTypeSummary holds per-type aggregate counts.
type ThingTypeSummary struct {
	Type    string `json:"type"`
	Total   int    `json:"total"`
	Online  int    `json:"online"`
	Offline int    `json:"offline"`
	Drift   int    `json:"drift"`
}

// DriftSummary holds the result of the drift detection query.
type DriftSummary struct {
	Total       int             `json:"total"`
	ByType      map[string]int  `json:"byType"`
	TotalByType map[string]int  `json:"totalByType"`
	Things      []ThingRegistry `json:"things"`
}

const thingSelectColumns = `
	id, type, name, version, address, enrolled_by, auth_type, conn_protocol,
	status, enrolled_at, last_seen_at, updated_at,
	desired, reported, desired_ver, reported_ver, metadata`

func scanThing(row interface {
	Scan(dest ...any) error
}) (ThingRegistry, error) {
	var t ThingRegistry
	err := row.Scan(
		&t.ID, &t.Type, &t.Name, &t.Version, &t.Address, &t.EnrolledBy,
		&t.AuthType, &t.ConnProtocol,
		&t.Status, &t.EnrolledAt, &t.LastSeenAt, &t.UpdatedAt,
		&t.Desired, &t.Reported, &t.DesiredVer, &t.ReportedVer, &t.Metadata,
	)
	return t, err
}

// UpdateThingStatus updates the status and last_seen_at for a Thing.
func (store *Store) UpdateThingStatus(ctx context.Context, id, status string) error {
	query := `
		UPDATE thing
		SET status = $2, last_seen_at = NOW(), updated_at = NOW()
		WHERE id = $1`
	_, err := store.pool.Exec(ctx, query, id, status)
	if err != nil {
		return fmt.Errorf("update thing status: %w", err)
	}
	return nil
}

// GetThing returns a single Thing by its ID.
// Returns (nil, nil) if not found.
func (store *Store) GetThing(ctx context.Context, id string) (*ThingRegistry, error) {
	query := `SELECT ` + thingSelectColumns + ` FROM thing WHERE id = $1`
	t, err := scanThing(store.pool.QueryRow(ctx, query, id))
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("get thing: %w", err)
	}
	return &t, nil
}

// ListThings returns Things filtered by optional type and status.
// Pass empty strings to skip those filters.
func (store *Store) ListThings(ctx context.Context, thingType, status string) ([]ThingRegistry, error) {
	query := `SELECT ` + thingSelectColumns + ` FROM thing WHERE 1=1`
	args := []any{}
	n := 1
	if thingType != "" {
		query += fmt.Sprintf(" AND type = $%d", n)
		args = append(args, thingType)
		n++
	}
	if status != "" {
		query += fmt.Sprintf(" AND status = $%d", n)
		args = append(args, status)
	}
	query += " ORDER BY type ASC, id ASC"

	rows, err := store.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list things: %w", err)
	}
	defer rows.Close()

	var result []ThingRegistry
	for rows.Next() {
		t, err := scanThing(rows)
		if err != nil {
			return nil, fmt.Errorf("scan thing: %w", err)
		}
		result = append(result, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate things: %w", err)
	}
	return result, nil
}

// ListDriftedThings returns all Things where reported_ver < desired_ver (drift state).
func (store *Store) ListDriftedThings(ctx context.Context) ([]ThingRegistry, error) {
	query := `
		SELECT ` + thingSelectColumns + `
		FROM thing
		WHERE reported_ver < desired_ver
		ORDER BY type ASC, id ASC`

	rows, err := store.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list drifted things: %w", err)
	}
	defer rows.Close()

	var result []ThingRegistry
	for rows.Next() {
		t, err := scanThing(rows)
		if err != nil {
			return nil, fmt.Errorf("scan drifted thing: %w", err)
		}
		result = append(result, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate drifted things: %w", err)
	}
	return result, nil
}

// GetThingTypeSummaries returns per-type aggregate counts grouped by status.
func (store *Store) GetThingTypeSummaries(ctx context.Context) ([]ThingTypeSummary, error) {
	query := `
		SELECT
			type,
			COUNT(*)                                    AS total,
			COUNT(*) FILTER (WHERE status = 'online')  AS online,
			COUNT(*) FILTER (WHERE status = 'offline') AS offline,
			COUNT(*) FILTER (WHERE status = 'drift')   AS drift
		FROM thing
		GROUP BY type
		ORDER BY type`

	rows, err := store.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("get thing type summaries: %w", err)
	}
	defer rows.Close()

	var result []ThingTypeSummary
	for rows.Next() {
		var s ThingTypeSummary
		if err := rows.Scan(&s.Type, &s.Total, &s.Online, &s.Offline, &s.Drift); err != nil {
			return nil, fmt.Errorf("scan thing type summary: %w", err)
		}
		result = append(result, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate thing type summaries: %w", err)
	}
	return result, nil
}
