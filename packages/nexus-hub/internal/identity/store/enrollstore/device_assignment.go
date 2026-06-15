package enrollstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeviceAssignmentSource describes how a DeviceAssignment row came to
// exist. Stored as a string in the `source` column so existing rows
// remain readable; the typed constants exist to prevent typos at call
// sites that mint new rows.
type DeviceAssignmentSource string

const (
	// DeviceAssignmentSourceSSO is set when the link was minted by the
	// agent SSO self-enrollment flow.
	DeviceAssignmentSourceSSO DeviceAssignmentSource = "sso"
	// DeviceAssignmentSourceAdmin is set when an operator manually
	// assigns a device to a user via the CP admin UI.
	DeviceAssignmentSourceAdmin DeviceAssignmentSource = "admin"
	// DeviceAssignmentSourceAuto is set when the assignment was
	// inferred by an automated heuristic (e.g. seat reconciliation).
	DeviceAssignmentSourceAuto DeviceAssignmentSource = "auto"
)

// ThingAgentRecord holds the subset of thing_agent columns needed by the
// trust-level computation. The Version comes from the parent thing row.
type ThingAgentRecord struct {
	ThingID       string
	Version       string     // thing.version (agent binary version)
	CertExpiresAt *time.Time // thing_agent.cert_expires_at
}

// GetThingAgentForTrustLevel fetches the fields required to compute trust_level
// for a given device. Returns ErrNotFound if the thing_agent row does not exist.
func (s *Store) GetThingAgentForTrustLevel(ctx context.Context, thingID string) (*ThingAgentRecord, error) {
	var r ThingAgentRecord
	err := s.db.QueryRow(ctx, `
		SELECT ta.thing_id, COALESCE(t.version, ''), ta.cert_expires_at
		FROM thing_agent ta
		JOIN thing t ON t.id = ta.thing_id
		WHERE ta.thing_id = $1
	`, thingID).Scan(&r.ThingID, &r.Version, &r.CertExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get thing agent for trust level: %w", err)
	}
	return &r, nil
}

// HasActiveDeviceAssignment returns true when there is at least one active
// (releasedAt IS NULL) DeviceAssignment row for the given device.
func (s *Store) HasActiveDeviceAssignment(ctx context.Context, thingID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM "DeviceAssignment"
			WHERE "deviceId" = $1 AND "releasedAt" IS NULL
		)
	`, thingID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("has active device assignment: %w", err)
	}
	return exists, nil
}

// ActiveDeviceAssignmentSnapshot is the small subset of DeviceAssignment
// columns the device-assignment audit emit captures as BeforeState when
// an existing binding is being overwritten by a new SSO enrollment or
// admin reassignment. The shape mirrors the AfterState map the caller
// builds, so SIEM consumers can diff before/after mechanically.
type ActiveDeviceAssignmentSnapshot struct {
	UserID      string
	Source      string
	LoginMethod string
	IPAddress   string
	AssignedAt  time.Time
}

// GetActiveDeviceAssignment returns the currently-active DeviceAssignment
// row for thingID (releasedAt IS NULL), or (nil, nil) when none exists.
// Used by the device-assignment audit-emit path to populate BeforeState
// when a rebind occurs.
func (s *Store) GetActiveDeviceAssignment(ctx context.Context, thingID string) (*ActiveDeviceAssignmentSnapshot, error) {
	if thingID == "" {
		return nil, nil
	}
	var snap ActiveDeviceAssignmentSnapshot
	var loginMethod, ipAddress *string
	err := s.db.QueryRow(ctx, `
		SELECT "userId", source, COALESCE(login_method, ''), COALESCE(ip_address, ''), "assignedAt"
		FROM "DeviceAssignment"
		WHERE "deviceId" = $1 AND "releasedAt" IS NULL
		ORDER BY "assignedAt" DESC
		LIMIT 1
	`, thingID).Scan(&snap.UserID, &snap.Source, &loginMethod, &ipAddress, &snap.AssignedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get active device assignment: %w", err)
	}
	if loginMethod != nil {
		snap.LoginMethod = *loginMethod
	}
	if ipAddress != nil {
		snap.IPAddress = *ipAddress
	}
	return &snap, nil
}

// UpdateThingAgentTrustLevel persists the computed trust_level into thing_agent.
// Returns ErrNotFound when the thing_agent row does not exist.
func (s *Store) UpdateThingAgentTrustLevel(ctx context.Context, thingID string, trustLevel int) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE thing_agent SET trust_level = $2 WHERE thing_id = $1
	`, thingID, trustLevel)
	if err != nil {
		return fmt.Errorf("update thing agent trust level: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpsertDeviceAssignmentParams holds the fields for a Hub-side
// DeviceAssignment row. LoginMethod and IPAddress are surfaced in the
// admin UI's "User Devices" tab (login method / IP address columns);
// leaving them empty produces dashes there.
type UpsertDeviceAssignmentParams struct {
	ThingID     string
	UserID      string
	Source      DeviceAssignmentSource
	LoginMethod string // "sso" for SSO enrollment, "admin" for manual assign, etc.
	IPAddress   string // request RealIP at the moment the assignment was minted
}

// UpsertDeviceAssignment creates or refreshes the DeviceAssignment row that
// links a device to a user. It follows the three-step pattern used by the
// CP auth server's assignment store. Step 3 (thing_agent.current_assignment_id
// sync) is best-effort: an error there leaves the join column out of date
// but does not fail enrollment — operators see a Warn log so the drift can
// be reconciled by the next sync pass.
func (s *Store) UpsertDeviceAssignment(ctx context.Context, p UpsertDeviceAssignmentParams) error {
	if p.ThingID == "" || p.UserID == "" {
		return nil
	}

	// Step 1: release stale assignment for a different user on this device.
	_, err := s.db.Exec(ctx, `
		UPDATE "DeviceAssignment"
		SET "releasedAt" = NOW()
		WHERE "deviceId" = $1
		  AND "releasedAt" IS NULL
		  AND "userId" != $2
	`, p.ThingID, p.UserID)
	if err != nil {
		return fmt.Errorf("upsert device assignment: release stale: %w", err)
	}

	// Step 2: insert new assignment (silently skip if same user+device already active).
	// login_method / ip_address are nullable; pgx encodes a Go empty string
	// as SQL '' (not NULL), which is what existing rows show, so callers
	// that have nothing to record should pass "" and the UI will render the
	// dash placeholder consistently.
	_, err = s.db.Exec(ctx, `
		INSERT INTO "DeviceAssignment"
		  (id, "deviceId", "userId", source, login_method, ip_address, "assignedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NOW())
		ON CONFLICT DO NOTHING
	`, p.ThingID, p.UserID, string(p.Source), p.LoginMethod, p.IPAddress)
	if err != nil {
		return fmt.Errorf("upsert device assignment: insert: %w", err)
	}

	// Step 3: sync thing_agent.current_assignment_id. Failure here leaves
	// the join out of sync — Warn so reconciliation can pick it up rather
	// than silently dropping the error.
	if _, err := s.db.Exec(ctx, `
		UPDATE thing_agent
		SET current_assignment_id = (
			SELECT id FROM "DeviceAssignment"
			WHERE "deviceId" = $1 AND "releasedAt" IS NULL
			LIMIT 1
		)
		WHERE thing_id = $1
	`, p.ThingID); err != nil {
		slog.Warn("upsert device assignment: thing_agent sync failed",
			"thingID", p.ThingID, "error", err)
	}

	return nil
}

// RefreshActiveDeviceAssignmentIP updates DeviceAssignment.ip_address on
// the currently-active row for thingID when newIP differs from what's
// stored. Returns (changed, nil) on success — changed is true only when
// a row was actually updated. No-op (changed=false, nil) when:
//
//   - newIP is empty, or
//   - thingID has no active assignment, or
//   - the active assignment already records newIP.
//
// Used by the heartbeat path to keep the IdentityEnricher's ip_address
// match key current as agents move networks. The job's lookup
// (FindActiveAssignmentByIPAndTime) keys on ip_address + time window;
// without periodic refresh the column stays frozen at enrollment time
// and traffic from later NAT IPs can never match.
func (s *Store) RefreshActiveDeviceAssignmentIP(ctx context.Context, thingID, newIP string) (bool, error) {
	if thingID == "" || newIP == "" {
		return false, nil
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE "DeviceAssignment"
		   SET ip_address = $2
		 WHERE "deviceId" = $1
		   AND "releasedAt" IS NULL
		   AND (ip_address IS NULL OR ip_address <> $2)
	`, thingID, newIP)
	if err != nil {
		return false, fmt.Errorf("refresh device assignment ip: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
