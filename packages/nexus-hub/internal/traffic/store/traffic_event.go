package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goccy/go-json"

	"github.com/jackc/pgx/v5"
)

// PendingIdentityEvent is a traffic_event that needs identity enrichment.
type PendingIdentityEvent struct {
	ID                string `json:"id"`
	ExternalRequestID string `json:"externalRequestId"`
	// ThingID is the Hub-stamped device identity, present on agent rows. It is
	// authenticated (mTLS device token), unlike every other correlation field
	// on this struct, which is why it can carry a deterministic resolution.
	ThingID   string         `json:"thingId"`
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
		SELECT id, COALESCE(external_request_id, ''), COALESCE(thing_id, ''), COALESCE(source_ip, ''), COALESCE(entity_id, ''), COALESCE(identity, '{}'), created_at
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
		if err := rows.Scan(&e.ID, &e.ExternalRequestID, &e.ThingID, &e.SourceIP, &e.EntityID, &identityRaw, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan pending event: %w", err)
		}
		if err := decodeJSONB(identityRaw, &e.Identity, "identity"); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}

// MatchedEventByRequestID is a traffic event with resolved identity, found by
// the request id it shares with the row being enriched.
type MatchedEventByRequestID struct {
	EntityID   string         `json:"entityId"`
	EntityName string         `json:"entityName"`
	Identity   map[string]any `json:"identity"`
}

// FindMatchedEventByRequestID finds the gateway's row for the SAME request,
// whose identity the virtual key already resolved, so a passive producer's row
// can inherit it.
//
// The join is on external_request_id — the request id every service on the path
// stamps — and not on trace_id, which holds the caller's own W3C trace and is
// NULL for every caller that runs no tracing.
//
// Three narrowings, and all three are load-bearing, because the request id on
// the row being enriched is SELF-REPORTED by the node that uploaded it:
//
//   - source = 'ai-gateway' — only the gateway resolves identity from a virtual
//     key, so only the gateway is a trustworthy donor. Without this, one
//     agent's row could inherit from another agent's row that itself inherited,
//     laundering an identity across two hops.
//   - same source_ip — the two rows must describe traffic from one machine.
//   - a time window — the two rows must describe one request, not the same
//     counter value a year apart.
//
// Without them, a request id like "1" — which any framework's auto-incrementing
// x-request-id produces — matches whichever row the planner returns first, and
// an enrolled agent could attribute its traffic to an arbitrary victim by
// guessing one. That is exactly the forgery the ingest path blanks the
// self-asserted attribution fields to prevent; re-opening it here through a
// join would defeat that.
func (s *Store) FindMatchedEventByRequestID(
	ctx context.Context, requestID, sourceIP string, at time.Time, window time.Duration,
) (*MatchedEventByRequestID, error) {
	if requestID == "" || sourceIP == "" {
		return nil, ErrNotFound
	}
	var m MatchedEventByRequestID
	var identityRaw []byte
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(entity_id, ''), COALESCE(entity_name, ''), COALESCE(identity, '{}')
		FROM traffic_event
		WHERE external_request_id = $1
		  AND source = 'ai-gateway'
		  AND source_ip = $2
		  AND created_at BETWEEN $3 AND $4
		  AND identity->>'status' = 'matched'
		ORDER BY created_at ASC
		LIMIT 1
	`, requestID, sourceIP, at.Add(-window), at.Add(window)).Scan(&m.EntityID, &m.EntityName, &identityRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find matched by request id: %w", err)
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
		SELECT da."userId", da."deviceId", u."displayName", COALESCE(u.email, '')
		FROM "DeviceAssignment" da
		JOIN "NexusUser" u ON u.id = da."userId"
		WHERE da.ip_address = $1
		  AND da."assignedAt" <= $2
		  AND (da."releasedAt" IS NULL OR da."releasedAt" > $2)
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

// FindAssignmentByThingAndTime resolves the user a device was bound to at ts.
//
// This is the DETERMINISTIC leg, and it exists because the agent's identity is
// authenticated: the row carries a thing_id the Hub stamped from the mTLS
// device token, not a value the node self-reported. Asking "which user held
// THIS device then" has one answer. The IP leg asks "which device held that
// address then", which behind a NAT has several — it is a fallback, and the
// order matters: a deterministic answer must not lose to a heuristic one that
// happens to run first.
//
// No LIMIT 2 ambiguity probe, unlike the IP leg. A device is bound to at most
// one user at a time; overlapping assignments for one device would be a
// corrupt DeviceAssignment table rather than the ordinary NAT contention the
// IP leg has to tolerate, so the query takes the row in force and does not
// invent an "ambiguous" verdict the schema cannot produce.
func (s *Store) FindAssignmentByThingAndTime(ctx context.Context, thingID string, ts time.Time) (*DeviceAssignmentMatch, error) {
	if thingID == "" {
		return nil, ErrNotFound
	}
	var m DeviceAssignmentMatch
	err := s.db.QueryRow(ctx, `
		SELECT da."userId", da."deviceId", u."displayName", COALESCE(u.email, '')
		FROM "DeviceAssignment" da
		JOIN "NexusUser" u ON u.id = da."userId"
		WHERE da."deviceId" = $1
		  AND da."assignedAt" <= $2
		  AND (da."releasedAt" IS NULL OR da."releasedAt" > $2)
		ORDER BY da."assignedAt" DESC
		LIMIT 1
	`, thingID, ts).Scan(&m.UserID, &m.DeviceID, &m.DisplayName, &m.Email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find assignment by thing: %w", err)
	}
	return &m, nil
}

// DeviceAssignmentWindow is one DeviceAssignment row together with the window
// it is valid for, so a caller that fetched a page of them can decide per event
// which one covers that event's timestamp.
type DeviceAssignmentWindow struct {
	DeviceAssignmentMatch
	IP         string
	AssignedAt time.Time
	ReleasedAt *time.Time
}

// Covers reports whether this assignment was in force at ts.
//
// It is the two predicates FindActiveAssignmentByIPAndTime spells in SQL,
// transcribed: assigned_at <= ts AND (released_at IS NULL OR released_at > ts).
// Transcribed rather than reworded on purpose — the boundary behaviour is the
// whole point (an assignment is in force at its own assigned_at and not at its
// released_at), and a paraphrase is where an off-by-one enters.
func (w DeviceAssignmentWindow) Covers(ts time.Time) bool {
	if w.AssignedAt.After(ts) {
		return false
	}
	return w.ReleasedAt == nil || w.ReleasedAt.After(ts)
}

// maxAssignmentPrefetchRows bounds one prefetch. Past it the read REFUSES
// rather than truncating: a truncated read can turn an IP that several
// assignments share into one that appears unique, and the caller would then
// name a user confidently and wrongly. Refusing leaves the rows pending for the
// next run, which is recoverable; a wrong attribution written into the audit
// trail is not.
const maxAssignmentPrefetchRows = 10000

// FindAssignmentsByIPsOverlapping returns every DeviceAssignment for the given
// IPs whose validity window OVERLAPS [from, to].
//
// It is deliberately a SUPERSET, not an answer: each event in the batch has its
// own timestamp, so the caller narrows with Covers. One statement per page
// replaces one statement per event, and the per-event version was the whole
// cost of this job — a page of 500 issued 500 of them, and the page loop has no
// upper bound.
func (s *Store) FindAssignmentsByIPsOverlapping(ctx context.Context, ips []string, from, to time.Time) ([]DeviceAssignmentWindow, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT da.ip_address, da."userId", da."deviceId", u."displayName", COALESCE(u.email, ''),
		       da."assignedAt", da."releasedAt"
		FROM "DeviceAssignment" da
		JOIN "NexusUser" u ON u.id = da."userId"
		WHERE da.ip_address = ANY($1)
		  AND da."assignedAt" <= $3
		  AND (da."releasedAt" IS NULL OR da."releasedAt" > $2)
		LIMIT $4
	`, ips, from, to, maxAssignmentPrefetchRows+1)
	if err != nil {
		return nil, fmt.Errorf("find assignments by ips: %w", err)
	}
	defer rows.Close()

	var out []DeviceAssignmentWindow
	for rows.Next() {
		var w DeviceAssignmentWindow
		if err := rows.Scan(&w.IP, &w.UserID, &w.DeviceID, &w.DisplayName, &w.Email,
			&w.AssignedAt, &w.ReleasedAt); err != nil {
			return nil, fmt.Errorf("scan assignment window row: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate assignment window rows: %w", err)
	}
	if len(out) > maxAssignmentPrefetchRows {
		return nil, fmt.Errorf("assignment prefetch for %d ips exceeded %d rows: refusing rather "+
			"than truncating, because a truncated read can make a shared IP look unique",
			len(ips), maxAssignmentPrefetchRows)
	}
	return out, nil
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
