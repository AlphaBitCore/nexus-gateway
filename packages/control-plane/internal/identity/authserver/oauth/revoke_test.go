package oauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/oauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	cpstore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// recordingRevocation captures every Revoke call so tests can assert the
// Request shape the handler produced without touching a real DB or MQ.
type recordingRevocation struct {
	calls  int32
	last   revocation.Request
	retErr error
}

func (r *recordingRevocation) Revoke(_ context.Context, req revocation.Request) error {
	atomic.AddInt32(&r.calls, 1)
	r.last = req
	return r.retErr
}

// revokeFixture bundles the collaborators the RFC 7009 handler needs. Each
// test builds its own so refresh rows and revocation state never leak across
// cases. The fixture seeds one refresh row against a real pgx pool so the
// refresh-token branch can be exercised end-to-end; access-token tests reuse
// the same handler because the access token does not have to pre-exist.
type revokeFixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	rows     *store.RefreshTokenRow
	rawToken string
	signer   *token.Signer
	keystore *token.Keystore
	rev      *recordingRevocation
	echo     *echo.Echo
}

// seedRevokeFixtures inserts a throwaway NexusUser + OAuthClient so refresh
// rows created during the test satisfy the foreign keys on the production
// schema. The helper mirrors the one in store_test.go but stays local to the
// oauth_test package because Go's package scoping prevents reuse across
// _test packages.
func seedRevokeFixtures(t *testing.T, pool *pgxpool.Pool, ctx context.Context) (userID, clientID string) {
	t.Helper()
	userID = "oauth-revoke-user-" + time.Now().Format("150405.000000000")
	clientID = "oauth-revoke-client-" + time.Now().Format("150405.000000000")

	if _, err := pool.Exec(ctx,
		`INSERT INTO "NexusUser"(id,"displayName",email,status,"canAccessControlPlane","updatedAt")
		 VALUES ($1,$2,$3,'active',TRUE,NOW())`,
		userID, "Revoke User", userID+"@test.local",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "NexusUser" WHERE id=$1`, userID) })

	if _, err := pool.Exec(ctx,
		`INSERT INTO "OAuthClient"(id,name,type,"redirectUris","allowedScopes","updatedAt")
		 VALUES ($1,$2,'public',$3,$4,NOW())`,
		clientID, "Revoke Client",
		[]string{"http://127.0.0.1:*/callback"}, []string{"traffic:write"},
	); err != nil {
		t.Fatalf("seed client: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "OAuthClient" WHERE id=$1`, clientID) })
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE "userId"=$1`, userID) })

	return userID, clientID
}

// newRevokeFixture returns a fixture with the handler mounted at /oauth/revoke.
// When DATABASE_URL is unset the helper skips the test via storetest.Open.
func newRevokeFixture(t *testing.T) *revokeFixture {
	t.Helper()

	pool := storetest.Open(t) // skips when DATABASE_URL unset
	ctx := context.Background()
	userID, clientID := seedRevokeFixtures(t, pool, ctx)

	rs := store.NewRefreshStore(pool)
	h := token.NewRefreshHelper(rs)
	raw, _, _, err := h.NewChain(ctx, userID, clientID, "", time.Hour)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	hashed := token.DefaultRefreshHash([]byte(raw))
	row, found, err := rs.FindByTokenHash(ctx, hashed)
	if err != nil || !found {
		t.Fatalf("FindByTokenHash: found=%v err=%v", found, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE "sessionId"=$1`, row.SessionID)
	})

	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	signer := token.NewSigner(ks)

	rec := &recordingRevocation{}
	e := echo.New()
	e.POST("/oauth/revoke", oauth.RevokeHandler(oauth.RevokeDeps{
		Issuer:     "https://cp.nexus.ai",
		Keystore:   ks,
		Refresh:    rs,
		Revocation: rec,
	}))

	return &revokeFixture{
		t:        t,
		pool:     pool,
		rows:     row,
		rawToken: raw,
		signer:   signer,
		keystore: ks,
		rev:      rec,
		echo:     e,
	}
}

// post runs an x-www-form-urlencoded POST /oauth/revoke. When device is
// non-nil it is stashed in the Echo context under the key the mTLS middleware
// writes, so agent-desktop cross-check paths see a peer cert without TLS.
func (f *revokeFixture) post(form url.Values, device *cpstore.ThingNodeInfo) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/revoke", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	c := f.echo.NewContext(req, rec)
	setDevice(c, device)
	f.echo.Router().Find(http.MethodPost, "/oauth/revoke", c)
	h := c.Handler()
	if h == nil {
		f.t.Fatalf("no handler matched /oauth/revoke")
	}
	if err := h(c); err != nil {
		f.echo.HTTPErrorHandler(err, c)
	}
	return rec
}

// TestRevoke_MissingTokenReturns400 covers the only RFC 7009 section 2.1
// error we MUST surface: a missing token parameter is a malformed request.
// Every other failure mode funnels to 200 to avoid leaking token existence.
func TestRevoke_MissingTokenReturns400(t *testing.T) {
	f := newRevokeFixture(t)
	rec := f.post(url.Values{"client_id": {"cp-ui"}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke called %d times, want 0 on malformed request", n)
	}
}

// TestRevoke_UnknownRefreshToken_Returns200 exercises the RFC 7009 section 2.2
// anti-enumeration rule: an unknown token must succeed silently so attackers
// cannot probe the token store.
func TestRevoke_UnknownRefreshToken_Returns200(t *testing.T) {
	f := newRevokeFixture(t)
	rec := f.post(url.Values{
		"token":           {"never-issued"},
		"token_type_hint": {"refresh_token"},
		"client_id":       {"cp-ui"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 per RFC 7009 section 2.2", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke called %d times, want 0 for unknown token", n)
	}
}

// TestRevoke_ValidRefreshToken_RevokesSession covers the happy path: a valid
// raw refresh token maps to a RefreshStore row; the handler must delete the
// entire session chain and record a session-scoped revocation.
func TestRevoke_ValidRefreshToken_RevokesSession(t *testing.T) {
	f := newRevokeFixture(t)
	sid := f.rows.SessionID

	rec := f.post(url.Values{
		"token":           {f.rawToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {f.rows.ClientID},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	if n := atomic.LoadInt32(&f.rev.calls); n != 1 {
		t.Fatalf("Revoke called %d times, want 1", n)
	}
	if f.rev.last.Scope != revocation.ScopeSession {
		t.Errorf("scope=%q, want %q", f.rev.last.Scope, revocation.ScopeSession)
	}
	if f.rev.last.TargetSessionID == nil || *f.rev.last.TargetSessionID != sid {
		t.Errorf("TargetSessionID=%v, want %q", f.rev.last.TargetSessionID, sid)
	}
	if f.rev.last.Reason != revocation.ReasonUserLogout {
		t.Errorf("reason=%q, want %q", f.rev.last.Reason, revocation.ReasonUserLogout)
	}

	// The refresh row must be gone after DeleteBySessionID runs.
	rs := store.NewRefreshStore(f.pool)
	if _, found, err := rs.FindByTokenHash(context.Background(), token.DefaultRefreshHash([]byte(f.rawToken))); err != nil || found {
		t.Fatalf("refresh row not deleted: found=%v err=%v", found, err)
	}
}

// TestRevoke_AccessToken_RevokesJTI exercises the access-token branch: a
// signed access token that our keystore verifies must result in a JTI-scoped
// revocation with the claim's exp as ExpiresAt.
func TestRevoke_AccessToken_RevokesJTI(t *testing.T) {
	f := newRevokeFixture(t)
	accessTok, jti, err := token.IssueAccess(f.signer, token.AccessInput{
		Issuer:   "https://cp.nexus.ai",
		Subject:  "usr-1",
		Audience: []string{token.AdminAudience},
		ClientID: "cp-ui",
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	rec := f.post(url.Values{
		"token":           {accessTok},
		"token_type_hint": {"access_token"},
		"client_id":       {"cp-ui"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 1 {
		t.Fatalf("Revoke called %d times, want 1", n)
	}
	if f.rev.last.Scope != revocation.ScopeJTI {
		t.Errorf("scope=%q, want %q", f.rev.last.Scope, revocation.ScopeJTI)
	}
	if f.rev.last.TargetJTI == nil || *f.rev.last.TargetJTI != jti {
		t.Errorf("TargetJTI=%v, want %q", f.rev.last.TargetJTI, jti)
	}
	if f.rev.last.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt zero; want access token exp")
	}
}

// TestRevoke_AgentDesktop_DeviceMismatch covers the cross-check: an
// access token whose device_id claim does NOT match the mTLS peer cert
// must result in a silent 200 and no revocation recorded.
func TestRevoke_AgentDesktop_DeviceMismatch(t *testing.T) {
	f := newRevokeFixture(t)
	accessTok, _, err := token.IssueAccess(f.signer, token.AccessInput{
		Issuer:   "https://cp.nexus.ai",
		Subject:  "usr-1",
		Audience: []string{token.AdminAudience},
		ClientID: "agent-desktop",
		DeviceID: "dev-42",
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	rec := f.post(url.Values{
		"token":     {accessTok},
		"client_id": {"agent-desktop"},
	}, &cpstore.ThingNodeInfo{ID: "dev-different", Status: "active"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke called %d times, want 0 on device mismatch", n)
	}
}

// TestRevoke_AgentDesktop_DeviceMatch proves the positive side of the
// cross-check: when the peer cert id matches the access token's device_id
// claim the revocation proceeds normally.
func TestRevoke_AgentDesktop_DeviceMatch(t *testing.T) {
	f := newRevokeFixture(t)
	accessTok, _, err := token.IssueAccess(f.signer, token.AccessInput{
		Issuer:   "https://cp.nexus.ai",
		Subject:  "usr-1",
		Audience: []string{token.AdminAudience},
		ClientID: "agent-desktop",
		DeviceID: "dev-42",
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	rec := f.post(url.Values{
		"token":     {accessTok},
		"client_id": {"agent-desktop"},
	}, &cpstore.ThingNodeInfo{ID: "dev-42", Status: "active"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 1 {
		t.Fatalf("Revoke called %d times, want 1", n)
	}
	if f.rev.last.Scope != revocation.ScopeJTI {
		t.Errorf("scope=%q, want %q", f.rev.last.Scope, revocation.ScopeJTI)
	}
}

// TestRevoke_ClientIDMismatch covers the RFC 7009 section 2.2 silent-200 rule
// when the caller supplies a client_id that does not match the refresh row's
// owner. The server must not leak the mismatch back to the caller.
func TestRevoke_ClientIDMismatch(t *testing.T) {
	f := newRevokeFixture(t)
	rec := f.post(url.Values{
		"token":           {f.rawToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {"attacker-client"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke called %d times, want 0 on client_id mismatch", n)
	}
}

// TestRevoke_HintMissing_FallsThroughBothPaths documents the hint-advisory
// behaviour: without token_type_hint the handler tries access first, then
// refresh, and the refresh path still succeeds when the token is a refresh.
func TestRevoke_HintMissing_FallsThroughBothPaths(t *testing.T) {
	f := newRevokeFixture(t)
	rec := f.post(url.Values{
		"token":     {f.rawToken},
		"client_id": {f.rows.ClientID},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 1 {
		t.Fatalf("Revoke called %d times, want 1", n)
	}
	if f.rev.last.Scope != revocation.ScopeSession {
		t.Errorf("scope=%q, want %q (refresh path resolved)", f.rev.last.Scope, revocation.ScopeSession)
	}
}
