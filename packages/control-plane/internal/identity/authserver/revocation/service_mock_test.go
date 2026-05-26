package revocation_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
)

type capturingPub struct {
	ev      revocation.Event
	called  bool
	failErr error
}

func (c *capturingPub) Publish(_ context.Context, ev revocation.Event) error {
	c.called = true
	c.ev = ev
	return c.failErr
}

// TestService_Revoke_InsertFailureSurfacesAndSkipsPublish exercises the
// "insert errors → publish never runs" branch. This is load-bearing for
// security: a stale revocation event without a durable row would silently
// disappear on RS reconnect, leaving a "revoked but not actually revoked"
// token in the wild.
func TestService_Revoke_InsertFailureSurfacesAndSkipsPublish(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock: %v", err)
	}
	t.Cleanup(mock.Close)
	store := revocation.NewStoreWithPool(mock)
	pub := &capturingPub{}
	svc := revocation.NewService(store, pub, "authserver")

	want := errors.New("disk full")
	mock.ExpectQuery(`INSERT INTO "RevokedToken"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(want)

	jti := "jti-fail"
	err = svc.Revoke(context.Background(), revocation.Request{
		Scope:     revocation.ScopeJTI,
		TargetJTI: &jti,
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
		Reason:    revocation.ReasonReplayDetected,
	})
	if !errors.Is(err, want) {
		t.Fatalf("insert err must propagate: %v", err)
	}
	if pub.called {
		t.Fatal("publish must NOT run when insert fails (would create stale event)")
	}
}

// TestService_Revoke_HappyPath: row binds with expected column order +
// Event mirror is built with derefStr for every target pointer (nil → "")
// plus the evt_ prefix on EventID.
func TestService_Revoke_HappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock: %v", err)
	}
	t.Cleanup(mock.Close)
	store := revocation.NewStoreWithPool(mock)
	pub := &capturingPub{}
	svc := revocation.NewService(store, pub, "authserver")

	jti := "jti-good"
	exp := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Millisecond)

	mock.ExpectQuery(`INSERT INTO "RevokedToken"`).
		WithArgs("jti", &jti, (*string)(nil), (*string)(nil), (*string)(nil),
			pgxmock.AnyArg(), exp, revocation.ReasonReplayDetected, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(101)))

	before := time.Now().UTC()
	if err := svc.Revoke(context.Background(), revocation.Request{
		Scope:     revocation.ScopeJTI,
		TargetJTI: &jti,
		ExpiresAt: exp,
		Reason:    revocation.ReasonReplayDetected,
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	after := time.Now().UTC()
	if !pub.called {
		t.Fatal("publisher must be invoked on successful insert")
	}
	if !strings.HasPrefix(pub.ev.EventID, "evt_") {
		t.Fatalf("event id missing evt_ prefix: %q", pub.ev.EventID)
	}
	if pub.ev.Scope != revocation.ScopeJTI || pub.ev.TargetJTI != jti {
		t.Fatalf("scope/target mismatch: %+v", pub.ev)
	}
	// Unset targets must derefStr to empty, not propagate stale strings.
	if pub.ev.TargetUserID != "" || pub.ev.TargetDeviceID != "" || pub.ev.TargetSessionID != "" {
		t.Fatalf("unset targets should be empty strings; got %+v", pub.ev)
	}
	if !pub.ev.ExpiresAt.Equal(exp) {
		t.Fatalf("expires_at mismatch: want %s got %s", exp, pub.ev.ExpiresAt)
	}
	if pub.ev.RevokedAt.Before(before) || pub.ev.RevokedAt.After(after) {
		t.Fatalf("revoked_at outside [%s, %s]: %s", before, after, pub.ev.RevokedAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestService_Revoke_ActorBinding: nil actor → defaultActor lands in
// the bound argument; explicit actor wins. The actor stamp is what audit
// logs surface as "who revoked this token" — silently swapping it would
// blind the audit trail.
func TestService_Revoke_ActorBinding(t *testing.T) {
	t.Run("default actor lands on row", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("new mock: %v", err)
		}
		t.Cleanup(mock.Close)
		store := revocation.NewStoreWithPool(mock)
		pub := &capturingPub{}
		const want = "authserver-default"
		svc := revocation.NewService(store, pub, want)

		sess := "sess-default"
		mock.ExpectQuery(`INSERT INTO "RevokedToken"`).
			WithArgs("session", (*string)(nil), (*string)(nil), (*string)(nil), &sess,
				pgxmock.AnyArg(), pgxmock.AnyArg(), revocation.ReasonUserLogout,
				ptrMatcher(want)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(2)))

		if err := svc.Revoke(context.Background(), revocation.Request{
			Scope:           revocation.ScopeSession,
			TargetSessionID: &sess,
			ExpiresAt:       time.Now().Add(time.Hour).UTC(),
			Reason:          revocation.ReasonUserLogout,
		}); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})
	t.Run("explicit actor overrides default", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("new mock: %v", err)
		}
		t.Cleanup(mock.Close)
		store := revocation.NewStoreWithPool(mock)
		pub := &capturingPub{}
		svc := revocation.NewService(store, pub, "authserver-default")

		sess := "sess-explicit"
		explicit := "admin:carol"
		mock.ExpectQuery(`INSERT INTO "RevokedToken"`).
			WithArgs("session", (*string)(nil), (*string)(nil), (*string)(nil), &sess,
				pgxmock.AnyArg(), pgxmock.AnyArg(), revocation.ReasonAdminDisable,
				ptrMatcher(explicit)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(3)))

		if err := svc.Revoke(context.Background(), revocation.Request{
			Scope:           revocation.ScopeSession,
			TargetSessionID: &sess,
			ExpiresAt:       time.Now().Add(time.Hour).UTC(),
			Reason:          revocation.ReasonAdminDisable,
			Actor:           &explicit,
		}); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})
}

// TestService_Revoke_PublishErrorDoesNotRollbackRow re-validates the
// "row durable; publish best-effort" invariant with a pgxmock store, no
// live Postgres needed. The test asserts the publish err is surfaced AND
// that the captured event carries the expected scope (proving publish
// was actually attempted post-insert).
func TestService_Revoke_PublishErrorDoesNotRollbackRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock: %v", err)
	}
	t.Cleanup(mock.Close)
	store := revocation.NewStoreWithPool(mock)
	want := errors.New("MQ down")
	pub := &capturingPub{failErr: want}
	svc := revocation.NewService(store, pub, "authserver")

	user := "user-mq-down"
	mock.ExpectQuery(`INSERT INTO "RevokedToken"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(11)))

	err = svc.Revoke(context.Background(), revocation.Request{
		Scope:        revocation.ScopeUser,
		TargetUserID: &user,
		ExpiresAt:    time.Now().Add(time.Hour).UTC(),
		Reason:       revocation.ReasonAdminDisable,
	})
	if !errors.Is(err, want) {
		t.Fatalf("publish err must surface: %v", err)
	}
	if !pub.called || pub.ev.TargetUserID != user {
		t.Fatalf("publish was reached but with wrong event: %+v", pub.ev)
	}
}

// TestService_IsAccessTokenRevoked exercises the delegation wrapper:
// nil-Service returns (false, nil) without panic; nil-Store same; happy
// path forwards args verbatim into the underlying Store.
func TestService_IsAccessTokenRevoked(t *testing.T) {
	t.Run("nil service is a safe no-op", func(t *testing.T) {
		var svc *revocation.Service
		ok, err := svc.IsAccessTokenRevoked(context.Background(), "j", "u", "d", "s")
		if ok || err != nil {
			t.Fatalf("nil receiver must return (false, nil); got (%v, %v)", ok, err)
		}
	})
	t.Run("nil store inside service is a safe no-op", func(t *testing.T) {
		svc := revocation.NewService(nil, &capturingPub{}, "authserver")
		ok, err := svc.IsAccessTokenRevoked(context.Background(), "j", "u", "d", "s")
		if ok || err != nil {
			t.Fatalf("nil store must return (false, nil); got (%v, %v)", ok, err)
		}
	})
	t.Run("delegates to Store and returns true on hit", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("new mock: %v", err)
		}
		t.Cleanup(mock.Close)
		store := revocation.NewStoreWithPool(mock)
		svc := revocation.NewService(store, &capturingPub{}, "authserver")
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("jti-1", "user-1", "dev-1", "sess-1").
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
		ok, err := svc.IsAccessTokenRevoked(context.Background(), "jti-1", "user-1", "dev-1", "sess-1")
		if err != nil || !ok {
			t.Fatalf("expected revoked=true; got ok=%v err=%v", ok, err)
		}
	})
}

// TestService_NewService_AcceptsConcreteStore — guards against signature
// drift in NewService (matters because cmd/control-plane/main.go wires
// the production *Store + *Publisher into this constructor).
func TestService_NewService_AcceptsConcreteStore(t *testing.T) {
	svc := revocation.NewService(nil, &capturingPub{}, "x")
	if svc == nil {
		t.Fatal("NewService returned nil (signature regression)")
	}
}

// ptrMatcher is a pgxmock argument matcher that asserts the bound argument
// is a *string pointing at exactly the expected value. Used to verify the
// actor pointer flows through the Service → Store → SQL bind step.
func ptrMatcher(want string) pgxmock.Argument { return stringPtrMatch(want) }

type stringPtrMatch string

func (s stringPtrMatch) Match(v any) bool {
	p, ok := v.(*string)
	if !ok || p == nil {
		return false
	}
	return *p == string(s)
}
