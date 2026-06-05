// mount_factory_test.go drives the post-line-175 branch of Mount — the path
// where idps.GetLocal succeeds and the /authserver/password + OIDC routes
// register — and exercises the ReplayHook closure body. Both paths are
// unreachable from mount_branches_test.go because the lazy-pgxpool stub
// cannot return a real IdentityProvider row.
//
// The seam is the unexported StoreFactory interface plus MountWithFactory:
// the test factory wraps a pgxmock pool for the IdPStore (whose only
// dependency is the slim IdPPgxPool interface) and routes every other store
// through the same lazy pool used elsewhere in this test file. The closure
// body tolerates lazy-pool connection errors (DeleteBySessionID + revoke
// failures are error-logged, not returned), so the closure runs to
// completion and every statement under lines 87-104 of mount.go is hit.
package authserver_test

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	cpstore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// testStoreFactory satisfies authserver.StoreFactory by overriding the
// IdPStore (which needs a pgxmock-backed pool to make GetLocal succeed) and
// delegating every other store to the same unreachable lazy pool the rest of
// this test file uses. Lazy-pool failures are tolerated by the consumers we
// exercise in this file (the ReplayHook closure error-logs and returns nil;
// the password + OIDC routes are exercised only by registration, not by
// invocation).
type testStoreFactory struct {
	idps *cpstore.IdPStore
	pool *pgxpool.Pool
}

func (f testStoreFactory) Clients() *cpstore.ClientStore  { return cpstore.NewClientStore(f.pool) }
func (f testStoreFactory) Users() *cpstore.UserStore      { return cpstore.NewUserStore(f.pool) }
func (f testStoreFactory) IdPs() *cpstore.IdPStore        { return f.idps }
func (f testStoreFactory) Refresh() *cpstore.RefreshStore { return cpstore.NewRefreshStore(f.pool) }
func (f testStoreFactory) Assignments() *cpstore.AssignmentStore {
	return cpstore.NewAssignmentStore(f.pool)
}
func (f testStoreFactory) Federated() *cpstore.FederatedStore {
	return cpstore.NewFederatedStore(f.pool)
}

// buildIdPMock returns a pgxmock pool primed to answer the GetLocal SELECT
// with a single IdentityProvider row. Caller is responsible for calling Close
// via t.Cleanup; the helper does NOT register cleanup itself so the test can
// fail fast on unsatisfied expectations via mock.ExpectationsWereMet().
func buildIdPMock(t *testing.T, idpID string) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock NewPool: %v", err)
	}
	// IdPStore.GetLocal SELECTs id,type,name,enabled,config,
	// defaultRole,defaultControlPlaneAccess,jitEnabled. We respond with a
	// synthetic local-IdP row; only ID is consumed by Mount (idp.NewLocal uses it).
	mock.ExpectQuery(`SELECT id, type, name, enabled, config, "defaultRole", "defaultControlPlaneAccess", "jitEnabled"\s+FROM "IdentityProvider"\s+WHERE type = 'local'`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "defaultRole", "defaultControlPlaneAccess", "jitEnabled",
		}).AddRow(
			idpID, "local", "Local", true, []byte(`{}`), "developer", false, true,
		))
	return mock
}

// TestMountWithFactory_GetLocalSuccess covers Mount's post-line-175 branch:
// idps.GetLocal returns a real IdentityProvider, so /authserver/password
// AND both OIDC routes register. This is the last block uncovered by the
// lazy-DB tests in mount_branches_test.go and accounts for lines 182-200 of
// the source file.
func TestMountWithFactory_GetLocalSuccess(t *testing.T) {
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	const idpID = "00000000-0000-0000-0000-000000000001"
	mock := buildIdPMock(t, idpID)
	t.Cleanup(mock.Close)
	lazyPool := newLazyPool(t)

	factory := testStoreFactory{
		idps: cpstore.NewIdPStoreWithPool(mock),
		pool: lazyPool,
	}

	e := echo.New()
	mounted := authserver.MountWithFactory(e, authserver.Deps{
		DB:       lazyPool, // Mount no longer reads it directly, but Deps.DB
		Keystore: ks,       // is documented as the pool; pass the lazy one
		Signer:   token.NewSigner(ks),
		Issuer:   "https://test.local",
		Logger:   slog.New(slog.NewTextHandler(nopWriter{}, nil)),
	}, factory)
	if mounted == nil {
		t.Fatal("MountWithFactory returned nil *Mounted")
	}

	// Password route now exists.
	mustRegister(t, e, http.MethodPost, "/authserver/password")
	// The SSO start entry + return-leg routes register on the GetLocal-success path.
	mustRegister(t, e, http.MethodGet, "/authserver/idp/:idpId/start")
	mustRegister(t, e, http.MethodGet, "/authserver/oidc/callback")
	mustRegister(t, e, http.MethodPost, "/authserver/saml/acs")

	// The pgxmock ExpectQuery for GetLocal must have been hit exactly once.
	// Catches the regression "Mount swallowed the error and skipped GetLocal".
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

// TestMountWithFactory_ReplayHookClosureFires drives the ReplayHook closure
// body (mount.go lines 87-104) end-to-end. The closure:
//
//   - calls refresh.DeleteBySessionID — which fails against the lazy pool
//     and is error-logged (no return).
//   - calls revSvc.Revoke — which also fails for the same reason and is
//     error-logged.
//   - returns nil (the "fire and log" semantics documented in the comment).
//
// We can't drive this from a full /oauth/token replay because that would
// require a working RefreshToken table. Instead, MountWithFactory exposes
// the wired *RefreshHelper so the test invokes its ReplayHook field directly
// with a synthetic RefreshTokenRow. Every statement in the closure runs.
func TestMountWithFactory_ReplayHookClosureFires(t *testing.T) {
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	mock := buildIdPMock(t, "00000000-0000-0000-0000-000000000002")
	t.Cleanup(mock.Close)
	lazyPool := newLazyPool(t)

	factory := testStoreFactory{
		idps: cpstore.NewIdPStoreWithPool(mock),
		pool: lazyPool,
	}

	revSvc := revocation.NewService(
		revocation.NewStore(lazyPool),
		revocation.NewPublisher(fakeProducer{}),
		"test-actor",
	)

	e := echo.New()
	mounted := authserver.MountWithFactory(e, authserver.Deps{
		DB:         lazyPool,
		Keystore:   ks,
		Signer:     token.NewSigner(ks),
		Issuer:     "https://test.local",
		Logger:     slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		Revocation: revSvc,
	}, factory)

	if mounted == nil || mounted.RefreshHelper == nil {
		t.Fatal("MountWithFactory returned no RefreshHelper")
	}
	if mounted.RefreshHelper.ReplayHook == nil {
		t.Fatal("ReplayHook closure unset despite Revocation being wired")
	}

	// Drive the closure directly. The body deletes the session chain, calls
	// revocation.Revoke, and returns nil; lazy-pool errors are tolerated and
	// error-logged, which is the documented "fire and log" behaviour.
	row := &cpstore.RefreshTokenRow{
		JTI:       "test-jti",
		SessionID: "test-session-id",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := mounted.RefreshHelper.ReplayHook(context.Background(), row); err != nil {
		t.Fatalf("ReplayHook must always return nil per fire-and-log contract; got %v", err)
	}
}

// TestMountWithFactory_ReplayHookSkippedWhenRevocationNil locks the
// "Revocation nil → ReplayHook stays nil" branch by asserting on the wired
// helper's state. The branch is already covered indirectly by
// TestMount_LazyDB_NoOptionalDeps in mount_branches_test.go (which observes
// /oauth/revoke not registering), but asserting on the helper itself gives
// us the closure-skip path in a single statement.
func TestMountWithFactory_ReplayHookSkippedWhenRevocationNil(t *testing.T) {
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	mock := buildIdPMock(t, "00000000-0000-0000-0000-000000000003")
	t.Cleanup(mock.Close)
	lazyPool := newLazyPool(t)
	factory := testStoreFactory{
		idps: cpstore.NewIdPStoreWithPool(mock),
		pool: lazyPool,
	}

	e := echo.New()
	mounted := authserver.MountWithFactory(e, authserver.Deps{
		DB:       lazyPool,
		Keystore: ks,
		Signer:   token.NewSigner(ks),
		Issuer:   "https://test.local",
		Logger:   slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		// Revocation intentionally nil.
	}, factory)

	if mounted == nil || mounted.RefreshHelper == nil {
		t.Fatal("MountWithFactory returned no RefreshHelper")
	}
	if mounted.RefreshHelper.ReplayHook != nil {
		t.Fatal("ReplayHook must stay nil when Revocation is unwired")
	}
}

// TestMountWithFactory_NilFactoryWarnReturn covers the explicit nil-factory
// branch added when Mount was refactored: MountWithFactory(e, d, nil) must
// register only the no-DB routes (discovery + JWKS if Keystore set) and
// return an empty *Mounted. Mirrors TestMount_NilDeps but exercises the
// factory entry point directly.
func TestMountWithFactory_NilFactoryWarnReturn(t *testing.T) {
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	e := echo.New()
	mounted := authserver.MountWithFactory(e, authserver.Deps{
		Keystore: ks,
		Logger:   slog.New(slog.NewTextHandler(nopWriter{}, nil)),
	}, nil)
	if mounted == nil {
		t.Fatal("MountWithFactory must return non-nil *Mounted even on nil factory")
		return
	}
	if mounted.RefreshHelper != nil {
		t.Fatal("RefreshHelper must be nil when factory is nil (no stores to wire)")
	}

	mustRegister(t, e, http.MethodGet, "/.well-known/jwks.json")
	mustRegister(t, e, http.MethodGet, "/.well-known/openid-configuration")
	mustNotRegister(t, e, http.MethodPost, "/oauth/token")
	mustNotRegister(t, e, http.MethodGet, "/authserver/idps")
}
