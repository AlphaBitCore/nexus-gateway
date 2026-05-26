package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AssignmentPgxPool is the minimum pgx pool surface AssignmentStore methods
// need. The concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Mirrors the PgxPool convention from
// packages/control-plane/internal/store/db.go.
type AssignmentPgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// UpsertDeviceAssignmentParams holds the fields required to create or refresh
// a DeviceAssignment row at login time (source="login").
type UpsertDeviceAssignmentParams struct {
	// DeviceID is the Thing.id of the authenticating agent.
	DeviceID string
	// UserID is the NexusUser.id of the authenticated user.
	UserID string
	// OrgID is the NexusUser.organizationId.
	OrgID string
	// LoginMethod identifies how the user authenticated: "local" | "oidc" | "saml".
	LoginMethod string
	// TokenJTI is the JTI of the access token minted in the same request.
	TokenJTI string
	// IPAddress is the client IP extracted from the token-exchange request.
	IPAddress string
}

// AssignmentStore performs DeviceAssignment write operations needed by the
// auth server. It is deliberately separate from the read-path stores
// (UserStore, RefreshStore) so callers can inject just the write surface.
type AssignmentStore struct{ db AssignmentPgxPool }

// NewAssignmentStore returns an AssignmentStore backed by the supplied pool.
func NewAssignmentStore(db *pgxpool.Pool) *AssignmentStore {
	return &AssignmentStore{db: db}
}

// NewAssignmentStoreWithPool is the test-only constructor accepting any
// AssignmentPgxPool implementation (notably pgxmock.PgxPoolIface).
// Production callers must use NewAssignmentStore.
func NewAssignmentStoreWithPool(db AssignmentPgxPool) *AssignmentStore {
	return &AssignmentStore{db: db}
}

// UpsertDeviceAssignment records a user↔device binding at OAuth token-exchange
// time (source="login"). The operation is a best-effort three-step atomic write:
//
//  1. Release any active assignment for the same device belonging to a *different*
//     user (user switch: old session ended, new one begins).
//  2. Insert a new row for the current user+device pair. The partial unique index
//     DeviceAssignment_deviceId_active_uidx (WHERE releasedAt IS NULL) ensures at
//     most one active row per device; ON CONFLICT DO NOTHING silently discards the
//     duplicate when the same user refreshes their token.
//  3. Update thing_agent.current_assignment_id to the newly active assignment row
//     so the Hub heartbeat path can read it without a separate JOIN.
func (s *AssignmentStore) UpsertDeviceAssignment(ctx context.Context, p UpsertDeviceAssignmentParams) error {
	if p.DeviceID == "" || p.UserID == "" {
		return nil // nothing to do without both sides of the FK pair
	}

	// Step 1: release stale assignment for a different user on this device.
	_, err := s.db.Exec(ctx, `
		UPDATE "DeviceAssignment"
		SET "releasedAt" = NOW()
		WHERE "deviceId" = $1
		  AND "releasedAt" IS NULL
		  AND "userId" != $2
	`, p.DeviceID, p.UserID)
	if err != nil {
		return fmt.Errorf("upsert device assignment: release stale: %w", err)
	}

	// Step 2: insert new assignment (silently skip when the same user+device
	// pair already has an active row — the partial unique index prevents
	// duplicates and DO NOTHING avoids the error).
	_, err = s.db.Exec(ctx, `
		INSERT INTO "DeviceAssignment"
		  (id, "deviceId", "userId", source, login_method, token_jti, ip_address, "assignedAt")
		VALUES (gen_random_uuid(), $1, $2, 'login', $3, $4, $5, NOW())
		ON CONFLICT DO NOTHING
	`, p.DeviceID, p.UserID, p.LoginMethod, p.TokenJTI, p.IPAddress)
	if err != nil {
		return fmt.Errorf("upsert device assignment: insert: %w", err)
	}

	// Step 3: sync thing_agent.current_assignment_id to the now-active row so
	// the Hub heartbeat path can join without a round-trip through DeviceAssignment.
	// Run best-effort (failure here does not break the token response).
	_, _ = s.db.Exec(ctx, `
		UPDATE thing_agent
		SET current_assignment_id = (
			SELECT id FROM "DeviceAssignment"
			WHERE "deviceId" = $1 AND "releasedAt" IS NULL
			LIMIT 1
		)
		WHERE thing_id = $1
	`, p.DeviceID)

	return nil
}
