package revocation_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
)

type recordingPublisher struct {
	last revocation.Event
	err  error
}

func (r *recordingPublisher) Publish(ctx context.Context, ev revocation.Event) error {
	r.last = ev
	return r.err
}

// TestService_Revoke_PublishErrorLeavesRowDurable covers the safety property
// that a publisher failure does NOT roll back the DB row: the revocation is
// already durable and the replay endpoint will fan it out on reconnect.
func TestService_Revoke_PublishErrorLeavesRowDurable(t *testing.T) {
	ctx := context.Background()
	pool := storetest.Open(t)
	store := revocation.NewStore(pool)
	pub := &recordingPublisher{err: errors.New("boom")}
	svc := revocation.NewService(store, pub, "admin:u1")

	userID := uuid.NewString()
	err := svc.Revoke(ctx, revocation.Request{
		Scope:        revocation.ScopeUser,
		TargetUserID: &userID,
		ExpiresAt:    time.Now().Add(time.Hour).UTC(),
		Reason:       revocation.ReasonAdminDisable,
	})
	if !errors.Is(err, pub.err) {
		t.Fatalf("want publish err, got %v", err)
	}
	if pub.last.Scope != revocation.ScopeUser {
		t.Fatalf("publisher was not called: last=%+v", pub.last)
	}

	// Row must still be visible via ListSince despite the publish error.
	rows, _, lerr := store.ListSince(ctx, 0, 1000)
	if lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	found := false
	for _, r := range rows {
		if r.TargetUserID != nil && *r.TargetUserID == userID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("row not persisted after publish error")
	}
}

// TestService_Revoke_Scopes walks the four scopes and asserts the full
// contract: row lands with the correct target populated, the published Event
// mirrors the row, the event id carries the evt_ prefix, and the DB row's
// revokedAt agrees with the event's RevokedAt within a tight tolerance.
func TestService_Revoke_Scopes(t *testing.T) {
	ctx := context.Background()
	pool := storetest.Open(t)
	store := revocation.NewStore(pool)

	type targets struct {
		jti, user, device, session *string
	}

	jti := uuid.NewString()
	user := uuid.NewString()
	device := "dev_" + uuid.NewString()
	session := uuid.NewString()

	cases := []struct {
		name    string
		scope   revocation.Scope
		reason  string
		targets targets
	}{
		{"jti", revocation.ScopeJTI, revocation.ReasonReplayDetected, targets{jti: &jti}},
		{"user", revocation.ScopeUser, revocation.ReasonAdminDisable, targets{user: &user}},
		{"device", revocation.ScopeDevice, revocation.ReasonUnenroll, targets{device: &device}},
		{"session", revocation.ScopeSession, revocation.ReasonUserLogout, targets{session: &session}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := &recordingPublisher{}
			svc := revocation.NewService(store, pub, "authserver")

			expiresAt := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
			before := time.Now().UTC()
			if err := svc.Revoke(ctx, revocation.Request{
				Scope:           tc.scope,
				TargetJTI:       tc.targets.jti,
				TargetUserID:    tc.targets.user,
				TargetDeviceID:  tc.targets.device,
				TargetSessionID: tc.targets.session,
				ExpiresAt:       expiresAt,
				Reason:          tc.reason,
			}); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			after := time.Now().UTC()

			ev := pub.last
			if ev.Scope != tc.scope {
				t.Fatalf("event scope: want %q, got %q", tc.scope, ev.Scope)
			}
			if !strings.HasPrefix(ev.EventID, "evt_") {
				t.Fatalf("event_id missing evt_ prefix: %q", ev.EventID)
			}
			if ev.Reason != tc.reason {
				t.Fatalf("event reason: want %q, got %q", tc.reason, ev.Reason)
			}
			if !ev.ExpiresAt.Equal(expiresAt) {
				t.Fatalf("event expires_at: want %s, got %s", expiresAt, ev.ExpiresAt)
			}
			if ev.RevokedAt.Before(before) || ev.RevokedAt.After(after) {
				t.Fatalf("event revoked_at %s outside [%s, %s]", ev.RevokedAt, before, after)
			}

			// Assert exactly one target pointer populated, matching scope.
			switch tc.scope {
			case revocation.ScopeJTI:
				if ev.TargetJTI != jti {
					t.Fatalf("target_jti: want %q, got %q", jti, ev.TargetJTI)
				}
			case revocation.ScopeUser:
				if ev.TargetUserID != user {
					t.Fatalf("target_user_id: want %q, got %q", user, ev.TargetUserID)
				}
			case revocation.ScopeDevice:
				if ev.TargetDeviceID != device {
					t.Fatalf("target_device_id: want %q, got %q", device, ev.TargetDeviceID)
				}
			case revocation.ScopeSession:
				if ev.TargetSessionID != session {
					t.Fatalf("target_session_id: want %q, got %q", session, ev.TargetSessionID)
				}
			}

			// Find the matching row and verify scope + target + timestamp agreement.
			rows, _, err := store.ListSince(ctx, 0, 1000)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			row, ok := findRowByEvent(rows, ev)
			if !ok {
				t.Fatalf("no row matched event target for scope %s", tc.scope)
			}
			if row.Scope != tc.scope {
				t.Fatalf("row scope: want %q, got %q", tc.scope, row.Scope)
			}
			// DB round-trips the timestamp at millisecond precision
			// (TIMESTAMP(3)); allow a matching tolerance.
			if delta := row.RevokedAt.Sub(ev.RevokedAt); delta > time.Millisecond || delta < -time.Millisecond {
				t.Fatalf("row.revokedAt %s diverges from event.RevokedAt %s (delta=%s)",
					row.RevokedAt, ev.RevokedAt, delta)
			}
		})
	}
}

// TestService_Revoke_ActorFallback exercises the defaultActor substitution:
// when Request.Actor is nil the stored row carries the Service default; when
// non-nil the caller value wins.
func TestService_Revoke_ActorFallback(t *testing.T) {
	ctx := context.Background()
	pool := storetest.Open(t)
	store := revocation.NewStore(pool)
	svc := revocation.NewService(store, &recordingPublisher{}, "authserver")

	// Case 1: nil actor -> defaultActor lands on the row.
	sessionA := uuid.NewString()
	if err := svc.Revoke(ctx, revocation.Request{
		Scope:           revocation.ScopeSession,
		TargetSessionID: &sessionA,
		ExpiresAt:       time.Now().Add(time.Hour).UTC(),
		Reason:          revocation.ReasonUserLogout,
	}); err != nil {
		t.Fatalf("revoke nil-actor: %v", err)
	}

	// Case 2: explicit actor wins.
	sessionB := uuid.NewString()
	explicit := "admin:bob"
	if err := svc.Revoke(ctx, revocation.Request{
		Scope:           revocation.ScopeSession,
		TargetSessionID: &sessionB,
		ExpiresAt:       time.Now().Add(time.Hour).UTC(),
		Reason:          revocation.ReasonAdminDisable,
		Actor:           &explicit,
	}); err != nil {
		t.Fatalf("revoke explicit-actor: %v", err)
	}

	rows, _, err := store.ListSince(ctx, 0, 1000)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		if r.TargetSessionID == nil || r.Actor == nil {
			continue
		}
		got[*r.TargetSessionID] = *r.Actor
	}
	if got[sessionA] != "authserver" {
		t.Fatalf("nil-actor row: want %q, got %q", "authserver", got[sessionA])
	}
	if got[sessionB] != explicit {
		t.Fatalf("explicit-actor row: want %q, got %q", explicit, got[sessionB])
	}
}

func findRowByEvent(rows []revocation.Row, ev revocation.Event) (revocation.Row, bool) {
	for _, r := range rows {
		switch ev.Scope {
		case revocation.ScopeJTI:
			if r.TargetJTI != nil && *r.TargetJTI == ev.TargetJTI {
				return r, true
			}
		case revocation.ScopeUser:
			if r.TargetUserID != nil && *r.TargetUserID == ev.TargetUserID {
				return r, true
			}
		case revocation.ScopeDevice:
			if r.TargetDeviceID != nil && *r.TargetDeviceID == ev.TargetDeviceID {
				return r, true
			}
		case revocation.ScopeSession:
			if r.TargetSessionID != nil && *r.TargetSessionID == ev.TargetSessionID {
				return r, true
			}
		}
	}
	return revocation.Row{}, false
}
