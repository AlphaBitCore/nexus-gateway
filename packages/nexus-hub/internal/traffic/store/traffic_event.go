package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PendingIdentityEvent is a traffic_event that needs identity enrichment.
type PendingIdentityEvent struct {
	ID        string         `json:"id"`
	TraceID   string         `json:"traceId"`
	SourceIP  string         `json:"sourceIp"`
	EntityID  string         `json:"entityId"`
	Identity  map[string]any `json:"identity"`
	CreatedAt time.Time      `json:"createdAt"`
}

// FindPendingIdentityEvents returns up to `limit` traffic events whose
// identity status is "pending" within the lookback window.
//
// No OFFSET parameter: callers should call this in a loop with the same
// limit. After each batch, UpdateEventIdentity flips the identity
// status of every returned row away from "pending", so the next call
// naturally yields the NEXT pending rows in created_at order. Adding
// a moving OFFSET would double-skip rows once status flips remove them
// from the result set (see IdentityEnricher.Run docs).
func (s *Store) FindPendingIdentityEvents(ctx context.Context, lookback time.Duration, limit int) ([]PendingIdentityEvent, error) {
	cutoff := time.Now().Add(-lookback)
	rows, err := s.db.Query(ctx, `
		SELECT id, COALESCE(trace_id, ''), COALESCE(source_ip, ''), COALESCE(entity_id, ''), COALESCE(identity, '{}'), created_at
		FROM traffic_event
		WHERE identity->>'status' = 'pending'
		  AND created_at >= $1
		ORDER BY created_at ASC
		LIMIT $2
	`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("find pending identity events: %w", err)
	}
	defer rows.Close()

	var events []PendingIdentityEvent
	for rows.Next() {
		var e PendingIdentityEvent
		var identityRaw []byte
		if err := rows.Scan(&e.ID, &e.TraceID, &e.SourceIP, &e.EntityID, &identityRaw, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan pending event: %w", err)
		}
		if err := decodeJSONB(identityRaw, &e.Identity, "identity"); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}

// MatchedEventByTraceID is a traffic event with resolved identity found by trace_id.
type MatchedEventByTraceID struct {
	EntityID   string         `json:"entityId"`
	EntityName string         `json:"entityName"`
	Identity   map[string]any `json:"identity"`
}

// FindMatchedEventByTraceID finds another event with the same trace_id that has been matched.
func (s *Store) FindMatchedEventByTraceID(ctx context.Context, traceID string) (*MatchedEventByTraceID, error) {
	if traceID == "" {
		return nil, ErrNotFound
	}
	var m MatchedEventByTraceID
	var identityRaw []byte
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(entity_id, ''), COALESCE(entity_name, ''), COALESCE(identity, '{}')
		FROM traffic_event
		WHERE trace_id = $1
		  AND identity->>'status' = 'matched'
		LIMIT 1
	`, traceID).Scan(&m.EntityID, &m.EntityName, &identityRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find matched by trace: %w", err)
	}
	if err := decodeJSONB(identityRaw, &m.Identity, "identity"); err != nil {
		return nil, err
	}
	return &m, nil
}

// AgentByIP is a Thing (agent) found by IP address.
type AgentByIP struct {
	ID       string         `json:"id"`
	Metadata map[string]any `json:"metadata"`
}

// FindAgentByIP finds an online/degraded agent by IP address match.
func (s *Store) FindAgentByIP(ctx context.Context, ip string) (*AgentByIP, error) {
	if ip == "" {
		return nil, ErrNotFound
	}
	var a AgentByIP
	var metaRaw []byte
	err := s.db.QueryRow(ctx, `
		SELECT id, COALESCE(metadata, '{}')
		FROM thing
		WHERE type = 'agent'
		  AND status IN ('online', 'enrolled')
		  AND (address = $1 OR address LIKE $1 || ':%')
		LIMIT 1
	`, ip).Scan(&a.ID, &metaRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find agent by ip: %w", err)
	}
	if err := decodeJSONB(metaRaw, &a.Metadata, "metadata"); err != nil {
		return nil, err
	}
	return &a, nil
}

// UpdateEventIdentityParams holds params for updating a traffic event's identity.
type UpdateEventIdentityParams struct {
	EventID    string
	EntityID   string
	EntityName string
	Identity   map[string]any
}

// DeviceAssignmentMatch holds the result of an active DeviceAssignment lookup by IP + time.
type DeviceAssignmentMatch struct {
	UserID      string `json:"userId"`
	DeviceID    string `json:"deviceId"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

// FindActiveAssignmentByIPAndTime finds THE active DeviceAssignment
// whose ip_address matches and whose window
// [assigned_at, released_at) covers the given timestamp.
//
// Returns:
//   - the match + nil err   → exactly one DA row resolved
//   - ErrNotFound           → zero matching rows
//   - ErrAmbiguous          → 2+ matching rows (same NAT egress IP
//     shared by multiple enrolled agents)
//
// LIMIT 2 is intentional — it lets us cheaply detect ambiguity without
// pulling N rows. The job stamps identity.status="ambiguous" on
// receipt of ErrAmbiguous so operators see contention rather than a
// confidently-wrong user attribution. Office / VPN egresses where
// many users share one public IP are the typical trigger.
func (s *Store) FindActiveAssignmentByIPAndTime(ctx context.Context, ip string, ts time.Time) (*DeviceAssignmentMatch, error) {
	if ip == "" {
		return nil, ErrNotFound
	}
	rows, err := s.db.Query(ctx, `
		SELECT da.user_id, da.device_id, u."displayName", COALESCE(u.email, '')
		FROM "DeviceAssignment" da
		JOIN "NexusUser" u ON u.id = da.user_id
		WHERE da.ip_address = $1
		  AND da.assigned_at <= $2
		  AND (da.released_at IS NULL OR da.released_at > $2)
		LIMIT 2
	`, ip, ts)
	if err != nil {
		return nil, fmt.Errorf("find active assignment by ip: %w", err)
	}
	defer rows.Close()

	var matches []DeviceAssignmentMatch
	for rows.Next() {
		var m DeviceAssignmentMatch
		if err := rows.Scan(&m.UserID, &m.DeviceID, &m.DisplayName, &m.Email); err != nil {
			return nil, fmt.Errorf("scan active assignment row: %w", err)
		}
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active assignment rows: %w", err)
	}
	switch len(matches) {
	case 0:
		return nil, ErrNotFound
	case 1:
		return &matches[0], nil
	default:
		return nil, ErrAmbiguous
	}
}

// UpdateEventIdentity updates the identity fields on a traffic event (idempotent: only updates if still pending).
func (s *Store) UpdateEventIdentity(ctx context.Context, p UpdateEventIdentityParams) error {
	identityJSON, err := json.Marshal(p.Identity)
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}

	_, err = s.db.Exec(ctx, `
		UPDATE traffic_event
		SET entity_id = $2, entity_name = $3, identity = $4
		WHERE id = $1 AND identity->>'status' = 'pending'
	`, p.EventID, p.EntityID, p.EntityName, identityJSON)
	return err
}
