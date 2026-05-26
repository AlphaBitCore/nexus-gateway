package revocation

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Request is the admin-facing API shape. Exactly one target pointer matches
// Scope; the service does not cross-validate because callers are internal.
type Request struct {
	Scope           Scope
	TargetJTI       *string
	TargetUserID    *string
	TargetDeviceID  *string
	TargetSessionID *string
	ExpiresAt       time.Time
	Reason          string
	Actor           *string
}

// publisher is the minimal surface Service consumes; tests drop in fakes.
type publisher interface {
	Publish(ctx context.Context, ev Event) error
}

// Service is the transactional facade wiring store + publisher.
//
// Ordering invariant: the store Insert MUST happen before Publish. If MQ is
// down, the row is still durable and the replay endpoint (section 8.4) fills
// RS gaps on reconnect. A publish failure is surfaced but does NOT roll back
// the DB row -- a revocation that is "committed locally but not fanned out
// yet" is the safer failure mode.
type Service struct {
	store        *Store
	pub          publisher
	defaultActor string
}

// NewService wires a Service. defaultActor is used when Request.Actor is nil
// (CP main passes "authserver" for system-triggered revocations).
func NewService(s *Store, p publisher, defaultActor string) *Service {
	return &Service{store: s, pub: p, defaultActor: defaultActor}
}

// Revoke inserts a revoked_token row and publishes the MQ event. The same
// RevokedAt timestamp is written to the DB row and embedded in the published
// Event so replay consumers see a single authoritative moment per revocation.
func (s *Service) Revoke(ctx context.Context, r Request) error {
	actor := s.defaultActor
	if r.Actor != nil {
		actor = *r.Actor
	}
	revokedAt := time.Now().UTC()
	row := Row{
		Scope:           r.Scope,
		TargetJTI:       r.TargetJTI,
		TargetUserID:    r.TargetUserID,
		TargetDeviceID:  r.TargetDeviceID,
		TargetSessionID: r.TargetSessionID,
		RevokedAt:       revokedAt,
		ExpiresAt:       r.ExpiresAt,
		Reason:          r.Reason,
		Actor:           &actor,
	}
	if _, err := s.store.Insert(ctx, row); err != nil {
		return err
	}
	return s.pub.Publish(ctx, Event{
		EventID:         "evt_" + uuid.NewString(),
		RevokedAt:       revokedAt,
		ExpiresAt:       r.ExpiresAt,
		Scope:           r.Scope,
		TargetJTI:       derefStr(r.TargetJTI),
		TargetUserID:    derefStr(r.TargetUserID),
		TargetDeviceID:  derefStr(r.TargetDeviceID),
		TargetSessionID: derefStr(r.TargetSessionID),
		Reason:          r.Reason,
	})
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// IsAccessTokenRevoked delegates to the underlying Store's lookup so
// callers (notably the OAuth IntrospectHandler) don't need to thread
// the Store through Deps separately. See Store.IsAccessTokenRevoked
// for scope/target semantics.
func (s *Service) IsAccessTokenRevoked(ctx context.Context, jti, userID, deviceID, sessionID string) (bool, error) {
	if s == nil || s.store == nil {
		return false, nil
	}
	return s.store.IsAccessTokenRevoked(ctx, jti, userID, deviceID, sessionID)
}
