// Package authserver_test drives Mount through every branch that does NOT
// require a working Postgres connection. The DB-success path (lines after
// idps.GetLocal returns OK) is exercised by TestE2E_LocalIdPFlow in
// e2e_test.go, which skips automatically when DATABASE_URL is unset.
//
// Mount has six independent branch points; this file enumerates them:
//
//  1. d.Logger nil  vs  set     — slog.Default() fallback.
//  2. d.Keystore nil vs set     — JWKS handler registration.
//  3. d.DB nil       vs lazy   — early-return at line 47-50 vs the
//     DB-dependent body below it.
//  4. d.Revocation nil vs set  — /oauth/revoke skipped vs registered,
//     and refreshHelper.ReplayHook left nil
//     vs wired up.
//  5. d.AgentLookup nil vs set — /oauth/device-binding registered with
//     or without the mTLS middleware.
//  6. d.AuthCodes nil vs set   — Mount allocates its own store or reuses
//     the caller-supplied one (sso-enroll path).
//
// All tests below are white-box only insofar as they hit the public Mount
// entry; routes are exercised through echo.ServeHTTP so the assertions are
// strictly about observable HTTP behaviour, not internal struct shape.
package authserver_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	cpstore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	pstore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// newLazyPool returns a *pgxpool.Pool wired to a deliberately unreachable
// address. pgxpool defers connection establishment until the first query, so
// the pool itself constructs cleanly; any operation Mount triggers
// (idps.GetLocal) returns a "connection refused" error rather than an
// ErrIdPNotFound, which is exactly what we want for the post-line-176 branch.
func newLazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// Port 1 is reserved; nothing listens there. connect_timeout=1 keeps the
	// test from hanging.
	cfg, err := pgxpool.ParseConfig("postgres://nouser:nopass@127.0.0.1:1/nodb?connect_timeout=1")
	if err != nil {
		t.Fatalf("parse lazy pool config: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("build lazy pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeNodeLookup is a no-op AgentLookup; the device-binding route is wired
// behind it but the route is never invoked, so the lookup body never runs.
// It exists purely to flip the `d.AgentLookup != nil` branch.
type fakeNodeLookup struct{}

func (fakeNodeLookup) LookupThingNodeByCertSerial(context.Context, string) (*pstore.ThingNodeInfo, error) {
	return nil, nil
}

// fakeProducer is a no-op mq.Producer used to build a revocation.Service
// without standing up a real NATS connection. Mount never invokes Publish
// during registration (the closure body only runs on refresh-token replay),
// so the methods can be inert.
type fakeProducer struct{}

func (fakeProducer) Publish(context.Context, string, []byte) error { return nil }
func (fakeProducer) Enqueue(context.Context, string, []byte) error { return nil }
func (fakeProducer) Close() error                                  { return nil }

// routeRegistered returns true if echo has a handler for method+path.
// We rely on the side-effect that an unregistered route 404s, while a
// registered route returns whatever the handler decides (typically 4xx
// for incomplete requests).
func routeRegistered(t *testing.T, e *echo.Echo, method, path string) bool {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	// 404 with the default Echo body means no route matched.
	return rec.Code != http.StatusNotFound
}

// TestMount_NilDeps covers the "Logger nil + Keystore nil + DB nil" hot path:
// Mount must register the discovery endpoint (no deps), skip JWKS, and
// early-return without touching any DB-backed store. Verified by the absence
// of /oauth/token + /authserver/idps.
func TestMount_NilDeps(t *testing.T) {
	e := echo.New()
	authserver.Mount(e, authserver.Deps{}) // every field zero — logger falls back to slog.Default()

	if !routeRegistered(t, e, http.MethodGet, "/.well-known/openid-configuration") {
		t.Fatal("discovery endpoint must register without any deps")
	}
	if routeRegistered(t, e, http.MethodGet, "/.well-known/jwks.json") {
		t.Fatal("JWKS must NOT register when keystore is nil")
	}
	if routeRegistered(t, e, http.MethodPost, "/oauth/token") {
		t.Fatal("token endpoint must NOT register when DB is nil")
	}
	if routeRegistered(t, e, http.MethodGet, "/authserver/idps") {
		t.Fatal("idps endpoint must NOT register when DB is nil")
	}
}

// TestMount_KeystoreOnly covers "Keystore set + DB nil": JWKS registers but
// the DB-dependent body still short-circuits. This is the realistic
// boot-without-DB shape for diagnostic deployments.
func TestMount_KeystoreOnly(t *testing.T) {
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	e := echo.New()
	authserver.Mount(e, authserver.Deps{Keystore: ks, Logger: slog.Default()})

	if !routeRegistered(t, e, http.MethodGet, "/.well-known/jwks.json") {
		t.Fatal("JWKS must register when keystore is set")
	}
	if routeRegistered(t, e, http.MethodPost, "/oauth/token") {
		t.Fatal("token endpoint still skipped when DB is nil")
	}
}

// TestMount_LazyDB_NoOptionalDeps covers the DB-backed branch where
// idps.GetLocal fails with a connection error. Every route below the
// keystore/discovery pair and above /authserver/password should be
// registered, and the function returns at the local-IdP-missing warn log.
//
// Branch assertions:
//   - d.Revocation == nil  ⇒ /oauth/revoke skipped
//   - d.AgentLookup == nil ⇒ /oauth/device-binding registered without mTLS mw
//   - d.AuthCodes == nil   ⇒ Mount allocates its own store internally
//   - GetLocal connection error  ⇒ /authserver/password skipped, OIDC routes
//     not yet reached, function returns.
func TestMount_LazyDB_NoOptionalDeps(t *testing.T) {
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pool := newLazyPool(t)

	e := echo.New()
	authserver.Mount(e, authserver.Deps{
		DB:       pool,
		Keystore: ks,
		Signer:   token.NewSigner(ks),
		Issuer:   "https://test.local",
		Logger:   slog.New(slog.NewTextHandler(nopWriter{}, nil)),
	})

	mustRegister(t, e, http.MethodGet, "/.well-known/jwks.json")
	mustRegister(t, e, http.MethodGet, "/.well-known/openid-configuration")
	mustRegister(t, e, http.MethodGet, "/oauth/authorize")
	mustRegister(t, e, http.MethodPost, "/oauth/token")
	mustRegister(t, e, http.MethodPost, "/oauth/introspect")
	mustRegister(t, e, http.MethodPost, "/oauth/device-binding") // wired without mTLS
	mustRegister(t, e, http.MethodGet, "/authserver/idps")

	mustNotRegister(t, e, http.MethodPost, "/oauth/revoke") // Revocation nil → skipped
	// /authserver/password is gated on local-IdP lookup, which fails against
	// the unreachable pool. The OIDC routes live in the same gated block.
	mustNotRegister(t, e, http.MethodPost, "/authserver/password")
	mustNotRegister(t, e, http.MethodGet, "/authserver/oidc/begin")
	mustNotRegister(t, e, http.MethodGet, "/authserver/oidc/callback")
}

// TestMount_LazyDB_AllOptionalDeps flips the three optional-dep branches that
// TestMount_LazyDB_NoOptionalDeps left at nil:
//   - d.Revocation set     ⇒ /oauth/revoke registered AND refreshHelper
//     replay hook closure assigned (not yet invoked).
//   - d.AgentLookup set    ⇒ /oauth/device-binding sits behind the mTLS mw.
//   - d.AuthCodes set      ⇒ Mount reuses the caller's authcode store rather
//     than constructing its own.
func TestMount_LazyDB_AllOptionalDeps(t *testing.T) {
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pool := newLazyPool(t)
	revSvc := revocation.NewService(
		revocation.NewStore(pool),
		revocation.NewPublisher(fakeProducer{}),
		"test-actor",
	)
	authcodes := cpstore.NewAuthCodeStore(5 * time.Minute)

	e := echo.New()
	authserver.Mount(e, authserver.Deps{
		DB:          pool,
		Keystore:    ks,
		Signer:      token.NewSigner(ks),
		Issuer:      "https://test.local",
		Logger:      slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		AgentLookup: fakeNodeLookup{},
		Revocation:  revSvc,
		AuthCodes:   authcodes,
	})

	// All Revocation+AgentLookup-gated routes now exist.
	mustRegister(t, e, http.MethodPost, "/oauth/revoke")
	mustRegister(t, e, http.MethodPost, "/oauth/device-binding")

	// /oauth/device-binding now sits behind the mTLS middleware. Hitting it
	// without a client cert must yield 401 AGENT_CERT_REQUIRED rather than
	// reach the handler body (which would 400 on missing payload).
	req := httptest.NewRequest(http.MethodPost, "/oauth/device-binding", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("device-binding without cert: want 401, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

// Asserts the middleware was wired vs not by toggling AgentLookup nil/set
// and observing the response code shape. Without the middleware, the route
// is registered but reaches the handler — which returns a different shape
// (400 / handler-driven) than the 401 emitted by AgentMTLSAuth itself.
func TestMount_DeviceBindingMiddleware_NotWiredWhenLookupNil(t *testing.T) {
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pool := newLazyPool(t)

	e := echo.New()
	authserver.Mount(e, authserver.Deps{
		DB:       pool,
		Keystore: ks,
		Signer:   token.NewSigner(ks),
		Issuer:   "https://test.local",
		Logger:   slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		// AgentLookup intentionally nil.
	})

	// Without the mTLS middleware, hitting the route is processed by the
	// handler. The exact handler response isn't 401 AGENT_CERT_REQUIRED;
	// the response code differs from the wired-middleware case. We assert
	// observable shape: the wired-middleware test above asserts 401 with no
	// cert; here, the response must NOT be that 401.
	req := httptest.NewRequest(http.MethodPost, "/oauth/device-binding", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatal("device-binding route must register even without AgentLookup")
	}
	// Body must not be the mTLS middleware's AGENT_CERT_REQUIRED envelope.
	if got := rec.Body.String(); contains(got, "AGENT_CERT_REQUIRED") {
		t.Fatalf("device-binding unexpectedly wrapped with mTLS mw when AgentLookup nil; body=%q", got)
	}
}

// Compile-time assertion that fakeNodeLookup satisfies the middleware
// contract; a future signature change in middleware.ThingNodeLookup would
// break the build here rather than surface as a runtime nil panic.
var _ middleware.ThingNodeLookup = fakeNodeLookup{}

func mustRegister(t *testing.T, e *echo.Echo, method, path string) {
	t.Helper()
	if !routeRegistered(t, e, method, path) {
		t.Fatalf("route %s %s not registered (expected registered)", method, path)
	}
}

func mustNotRegister(t *testing.T, e *echo.Echo, method, path string) {
	t.Helper()
	if routeRegistered(t, e, method, path) {
		t.Fatalf("route %s %s registered (expected NOT registered)", method, path)
	}
}

// nopWriter discards every Write so test logs don't spam stdout. We keep a
// real slog.Handler attached (rather than passing nil to Mount) so the
// Logger != nil branch executes in coverage.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// contains is a tiny strings.Contains shim that avoids the strings import
// to keep this file's import set small and obvious.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
