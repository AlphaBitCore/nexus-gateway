package oauth_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/oauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	cpstore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// fakeClientChecker is an in-memory clientChecker fake so revoke tests can run
// without Postgres. Keys are client ids; absent keys surface as
// ErrClientNotFound to exercise the silent-200 anti-enumeration path.
type fakeClientChecker map[string]*store.OAuthClient

func (f fakeClientChecker) GetByID(_ context.Context, id string) (*store.OAuthClient, error) {
	c, ok := f[id]
	if !ok {
		return nil, store.ErrClientNotFound
	}
	return c, nil
}

// refreshTokenRowCols are the columns FindByTokenHash scans, in order.
var refreshTokenRowCols = []string{
	"jti", "sessionId", "parentJti",
	"userId", "clientId", "deviceId", "tokenHash",
	"usedAt", "expiresAt", "createdAt",
}

// newRevokeMockFixture builds a RevokeHandler wired to a pgxmock-backed
// RefreshStore + a recording revocation service + a freshly-generated
// keystore. No DB required; every branch is observable through the mock's
// expectations and the recorder's status/body.
type revokeMockFixture struct {
	t        *testing.T
	mock     pgxmock.PgxPoolIface
	refresh  *store.RefreshStore
	rev      *recordingRevocation
	keystore *token.Keystore
	signer   *token.Signer
	echo     *echo.Echo
	deps     oauth.RevokeDeps
	logBuf   *bytes.Buffer
}

func newRevokeMockFixture(t *testing.T) *revokeMockFixture {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)

	rs := store.NewRefreshStoreWithPool(mock)

	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	signer := token.NewSigner(ks)

	rec := &recordingRevocation{}
	logBuf := &bytes.Buffer{}
	deps := oauth.RevokeDeps{
		Issuer:     "https://cp.nexus.ai",
		Keystore:   ks,
		Refresh:    rs,
		Revocation: rec,
		Logger:     slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	e := echo.New()
	e.POST("/oauth/revoke", oauth.RevokeHandler(deps))
	return &revokeMockFixture{
		t:        t,
		mock:     mock,
		refresh:  rs,
		rev:      rec,
		keystore: ks,
		signer:   signer,
		echo:     e,
		deps:     deps,
		logBuf:   logBuf,
	}
}

// withClients re-mounts the handler using a fresh Echo instance with the given
// fakeClientChecker installed on RevokeDeps.Clients. Used by tests that need
// to exercise the d.Clients != nil branches.
func (f *revokeMockFixture) withClients(c fakeClientChecker) {
	f.t.Helper()
	d := f.deps
	d.Clients = c
	f.echo = echo.New()
	f.echo.POST("/oauth/revoke", oauth.RevokeHandler(d))
}

// post runs an x-www-form-urlencoded POST /oauth/revoke through the fixture's
// Echo. device, when non-nil, is stashed in the Echo context to simulate
// the mTLS middleware having run.
func (f *revokeMockFixture) post(form url.Values, device *cpstore.ThingNodeInfo) *httptest.ResponseRecorder {
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

// expectFindRow stages a successful FindByTokenHash returning row.
func (f *revokeMockFixture) expectFindRow(raw string, row store.RefreshTokenRow) {
	f.t.Helper()
	hash := token.DefaultRefreshHash([]byte(raw))
	r := pgxmock.NewRows(refreshTokenRowCols).
		AddRow(row.JTI, row.SessionID, row.ParentJTI,
			row.UserID, row.ClientID, row.DeviceID, row.TokenHash,
			row.UsedAt, row.ExpiresAt, row.CreatedAt)
	f.mock.ExpectQuery(`FROM "RefreshToken"`).
		WithArgs(hash).
		WillReturnRows(r)
}

// expectFindEmpty stages a FindByTokenHash that returns no rows (handler must
// fall through to the access-token branch).
func (f *revokeMockFixture) expectFindEmpty(raw string) {
	f.t.Helper()
	hash := token.DefaultRefreshHash([]byte(raw))
	f.mock.ExpectQuery(`FROM "RefreshToken"`).
		WithArgs(hash).
		WillReturnRows(pgxmock.NewRows(refreshTokenRowCols))
}

// expectFindError stages a FindByTokenHash that returns a DB error.
func (f *revokeMockFixture) expectFindError(raw string, err error) {
	f.t.Helper()
	hash := token.DefaultRefreshHash([]byte(raw))
	f.mock.ExpectQuery(`FROM "RefreshToken"`).
		WithArgs(hash).
		WillReturnError(err)
}

// expectDelete stages the DeleteBySessionID call.
func (f *revokeMockFixture) expectDelete(sid string) {
	f.t.Helper()
	f.mock.ExpectExec(`DELETE FROM "RefreshToken" WHERE "sessionId"`).
		WithArgs(sid).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
}

// expectDeleteError stages the DeleteBySessionID call returning an error.
func (f *revokeMockFixture) expectDeleteError(sid string, err error) {
	f.t.Helper()
	f.mock.ExpectExec(`DELETE FROM "RefreshToken" WHERE "sessionId"`).
		WithArgs(sid).
		WillReturnError(err)
}

// makeRefreshRow builds a complete RefreshTokenRow with sensible defaults
// for the fields the handler reads.
func makeRefreshRow() store.RefreshTokenRow {
	now := time.Now().UTC()
	return store.RefreshTokenRow{
		JTI:       "rjti-1",
		SessionID: "sid-1",
		ParentJTI: "",
		UserID:    "user-1",
		ClientID:  "cp-ui",
		DeviceID:  nil,
		TokenHash: []byte{0xDE, 0xAD, 0xBE, 0xEF},
		UsedAt:    nil,
		ExpiresAt: now.Add(24 * time.Hour),
		CreatedAt: now,
	}
}

// TestRevoke_MalformedFormBody covers the ParseForm error branch — an
// invalid Content-Length / encoded body must surface as 400 invalid_request.
func TestRevoke_MalformedFormBody(t *testing.T) {
	f := newRevokeMockFixture(t)
	// Send a body with %ZZ which is an invalid percent-encoding sequence.
	req := httptest.NewRequest(http.MethodPost, "/oauth/revoke",
		strings.NewReader("token=%ZZ"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	c := f.echo.NewContext(req, rec)
	f.echo.Router().Find(http.MethodPost, "/oauth/revoke", c)
	if err := c.Handler()(c); err != nil {
		f.echo.HTTPErrorHandler(err, c)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrInvalidRequest {
		t.Errorf("error=%q, want %q", code, oauth.ErrInvalidRequest)
	}
}

// TestRevoke_UnknownClientIDSilent200 covers the d.Clients != nil branch when
// the supplied client_id does not resolve — RFC 7009 §2.2 anti-enumeration
// forbids leaking client registration state, so the handler MUST 200 silently
// and never reach the refresh / access paths.
func TestRevoke_UnknownClientIDSilent200(t *testing.T) {
	f := newRevokeMockFixture(t)
	f.withClients(fakeClientChecker{}) // empty registry → all GetByIDs miss

	rec := f.post(url.Values{
		"token":     {"any"},
		"client_id": {"never-registered"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 per RFC 7009 §2.2", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revocation.Revoke called %d times on unknown client, want 0", n)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB calls: %v", err)
	}
}

// TestRevoke_KnownClientIDContinues covers the d.Clients != nil branch on the
// positive side: when the client_id resolves the handler proceeds to the token
// lookup. We stage an empty refresh-hash result so the access branch fires
// next (and the handler falls through to a benign 200), letting us observe
// that both DB calls happened in the correct order.
func TestRevoke_KnownClientIDContinues(t *testing.T) {
	f := newRevokeMockFixture(t)
	registered := &store.OAuthClient{ID: "cp-ui", Type: "public"}
	f.withClients(fakeClientChecker{"cp-ui": registered})

	// access first, then refresh fallback — both miss.
	f.expectFindEmpty("opaque-token-bytes")
	rec := f.post(url.Values{
		"token":     {"opaque-token-bytes"},
		"client_id": {"cp-ui"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRevoke_EmptyClientID_SkipsClientLookup proves the early skip when
// d.Clients is non-nil but the form-supplied client_id is empty: handlers
// MUST tolerate the omission (some public-client flows ship no client_id).
// We assert the refresh path runs without ever touching f.clients.GetByID.
func TestRevoke_EmptyClientID_SkipsClientLookup(t *testing.T) {
	f := newRevokeMockFixture(t)
	// fakeClientChecker is empty — any lookup would surface ErrClientNotFound
	// and 200 silently. The test passes ONLY if the lookup is skipped and
	// we reach the refresh branch (asserted by ExpectQuery).
	f.withClients(fakeClientChecker{})

	row := makeRefreshRow()
	f.expectFindRow("opaque", row)
	f.expectDelete(row.SessionID)
	rec := f.post(url.Values{
		"token":           {"opaque"},
		"token_type_hint": {"refresh_token"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 1 {
		t.Fatalf("Revoke called %d times, want 1", n)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRevoke_RefreshHint_HappyPath covers the hint=refresh_token branch where
// the DB returns a matching row. We assert the revocation request carries the
// session id from the row, the DeleteBySessionID exec ran, and the access
// branch was NOT attempted.
func TestRevoke_RefreshHint_HappyPath(t *testing.T) {
	f := newRevokeMockFixture(t)
	row := makeRefreshRow()
	f.expectFindRow("raw-rt", row)
	f.expectDelete(row.SessionID)

	rec := f.post(url.Values{
		"token":           {"raw-rt"},
		"token_type_hint": {"refresh_token"},
		"client_id":       {row.ClientID},
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
	if f.rev.last.TargetSessionID == nil || *f.rev.last.TargetSessionID != row.SessionID {
		t.Errorf("TargetSessionID=%v, want %q", f.rev.last.TargetSessionID, row.SessionID)
	}
	if f.rev.last.ExpiresAt != row.ExpiresAt {
		t.Errorf("ExpiresAt=%v, want %v (natural tail of chain)", f.rev.last.ExpiresAt, row.ExpiresAt)
	}
	if f.rev.last.Reason != revocation.ReasonUserLogout {
		t.Errorf("reason=%q, want %q", f.rev.last.Reason, revocation.ReasonUserLogout)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRevoke_RefreshHint_DBErrorLeaksAs200 covers the FindByTokenHash error
// branch. Per the handler comment "Treat as handled so we do not leak
// existence via a fall-through timing difference against the access path."
// The error must be logged but the HTTP response stays 200 with no revocation
// recorded.
func TestRevoke_RefreshHint_DBErrorLeaksAs200(t *testing.T) {
	f := newRevokeMockFixture(t)
	f.expectFindError("rt", errors.New("connection reset"))

	rec := f.post(url.Values{
		"token":           {"rt"},
		"token_type_hint": {"refresh_token"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (anti-enumeration)", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke called %d times, want 0 on DB error", n)
	}
	if !strings.Contains(f.logBuf.String(), "refresh lookup") {
		t.Errorf("expected DB-error log line, got: %s", f.logBuf.String())
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRevoke_RefreshHint_ClientIDMismatch_Silent200_NoDelete covers the
// row-found-but-client_id-mismatch branch. RFC 7009 §2.2 anti-enumeration:
// MUST 200, MUST NOT delete the session, MUST NOT record revocation.
func TestRevoke_RefreshHint_ClientIDMismatch_Silent200_NoDelete(t *testing.T) {
	f := newRevokeMockFixture(t)
	row := makeRefreshRow()
	row.ClientID = "cp-ui"
	f.expectFindRow("rt", row)
	// No ExpectExec — DeleteBySessionID must NOT fire.

	rec := f.post(url.Values{
		"token":           {"rt"},
		"token_type_hint": {"refresh_token"},
		"client_id":       {"attacker-other-client"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke called %d times, want 0 on client_id mismatch", n)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRevoke_RefreshHint_DeleteSessionError_StillRevokes200 covers the
// DeleteBySessionID error branch. The handler logs but does NOT proceed to the
// revocation publisher (returns true after the log). We MUST see status 200
// and zero Revoke calls.
func TestRevoke_RefreshHint_DeleteSessionError_NoRevokePublished(t *testing.T) {
	f := newRevokeMockFixture(t)
	row := makeRefreshRow()
	f.expectFindRow("rt", row)
	f.expectDeleteError(row.SessionID, errors.New("delete conflict"))

	rec := f.post(url.Values{
		"token":           {"rt"},
		"token_type_hint": {"refresh_token"},
		"client_id":       {row.ClientID},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke published despite DeleteBySessionID error; got %d calls", n)
	}
	if !strings.Contains(f.logBuf.String(), "delete by session") {
		t.Errorf("expected delete-by-session log, got: %s", f.logBuf.String())
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRevoke_RefreshHint_RevocationServiceError_Logged_Still200 covers the
// Revocation.Revoke error branch. The handler logs but returns 200; the DB
// delete already succeeded so the session is invalidated regardless of MQ
// fanout.
func TestRevoke_RefreshHint_RevocationServiceError_Logged_Still200(t *testing.T) {
	f := newRevokeMockFixture(t)
	row := makeRefreshRow()
	f.expectFindRow("rt", row)
	f.expectDelete(row.SessionID)
	f.rev.retErr = errors.New("mq write failed")

	rec := f.post(url.Values{
		"token":           {"rt"},
		"token_type_hint": {"refresh_token"},
		"client_id":       {row.ClientID},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 1 {
		t.Fatalf("Revoke called %d times, want 1 (publisher was invoked)", n)
	}
	if !strings.Contains(f.logBuf.String(), "revocation service") {
		t.Errorf("expected revocation-service log, got: %s", f.logBuf.String())
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRevoke_Access_MissingExpClaim covers the "claims.ExpiresAt == nil" branch.
// We craft a token whose registered exp is omitted by issuing with TTL=0 — but
// IssueAccess always stamps exp, so we instead bypass via a token forged with
// no exp claim using the same kid. Result: handler logs warning and returns
// silent 200 without revocation. The branch is hard to reach because real
// IssueAccess always sets exp; we forge a JWT directly so the access path
// can still be exercised end-to-end.
//
// Approach: parse fixture key, build a JWT manually with no exp claim, sign
// with the kid the keystore knows.
func TestRevoke_Access_MissingExpClaim_Silent200(t *testing.T) {
	f := newRevokeMockFixture(t)
	tok := forgeAccessTokenNoExp(t, f.keystore, "https://cp.nexus.ai", "cp-ui")
	// The refresh path runs first (no hint=access_token), so we need a
	// FindByTokenHash miss to fall through to tryAccess.
	f.expectFindEmpty(tok)
	// Actually since the handler default order is access-then-refresh
	// (no hint), let's match what the handler does:
	// - no hint → tryAccess first; if VerifyLocal succeeds, ExpiresAt is nil,
	//   return true (no revocation, no refresh probe).
	// So FindByTokenHash should NOT be called. Reset and try without that
	// expectation.
	f.mock, _ = pgxmock.NewPool()
	rs := store.NewRefreshStoreWithPool(f.mock)
	d := f.deps
	d.Refresh = rs
	f.echo = echo.New()
	f.echo.POST("/oauth/revoke", oauth.RevokeHandler(d))

	rec := f.post(url.Values{
		"token":     {tok},
		"client_id": {"cp-ui"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke called %d times on missing-exp token, want 0", n)
	}
	if !strings.Contains(f.logBuf.String(), "missing exp") {
		t.Errorf("expected 'missing exp' log, got: %s", f.logBuf.String())
	}
}

// TestRevoke_Access_ClientIDMismatch_Silent200 covers the access-token branch
// where the form-supplied client_id contradicts the JWT's ClientID claim.
// Must 200 with zero revocations recorded.
func TestRevoke_Access_ClientIDMismatch_Silent200(t *testing.T) {
	f := newRevokeMockFixture(t)
	accessTok, _, err := token.IssueAccess(f.signer, token.AccessInput{
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
		"client_id":       {"attacker-other-client"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke called %d times on client_id mismatch, want 0", n)
	}
}

// TestRevoke_Access_RevocationPublisherError_StillRevokes200 covers the
// Revoke()-error branch in tryAccess. The handler logs but returns 200; the
// JTI revocation request was still issued.
func TestRevoke_Access_RevocationPublisherError_Logged(t *testing.T) {
	f := newRevokeMockFixture(t)
	f.rev.retErr = errors.New("mq publish failed")
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
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 1 {
		t.Fatalf("Revoke called %d times, want 1", n)
	}
	if f.rev.last.TargetJTI == nil || *f.rev.last.TargetJTI != jti {
		t.Errorf("TargetJTI=%v, want %q", f.rev.last.TargetJTI, jti)
	}
	if !strings.Contains(f.logBuf.String(), "jti revocation") {
		t.Errorf("expected jti-revocation log, got: %s", f.logBuf.String())
	}
}

// TestRevoke_Access_AgentDesktop_NoDeviceContext_Silent200 covers the
// "agent-desktop + claims.DeviceID set + nil peer cert" branch. A leaked
// access token alone cannot trigger revocation without a matching mTLS device.
func TestRevoke_Access_AgentDesktop_NoDeviceContext_Silent200(t *testing.T) {
	f := newRevokeMockFixture(t)
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
		"token":           {accessTok},
		"token_type_hint": {"access_token"},
		"client_id":       {"agent-desktop"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke called %d times without device context, want 0", n)
	}
}

// TestRevoke_RefreshHint_FallsThroughToAccess covers the "hint=refresh_token,
// refresh path misses, access path matches" branch — the handler MUST try
// access after refresh misses with hint=refresh_token.
func TestRevoke_RefreshHint_FallsThroughToAccess(t *testing.T) {
	f := newRevokeMockFixture(t)
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
	// Refresh probe runs first per hint=refresh_token → returns no rows.
	f.expectFindEmpty(accessTok)

	rec := f.post(url.Values{
		"token":           {accessTok},
		"token_type_hint": {"refresh_token"},
		"client_id":       {"cp-ui"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 1 {
		t.Fatalf("Revoke called %d times, want 1 (access fallback)", n)
	}
	if f.rev.last.Scope != revocation.ScopeJTI {
		t.Errorf("scope=%q, want %q", f.rev.last.Scope, revocation.ScopeJTI)
	}
	if f.rev.last.TargetJTI == nil || *f.rev.last.TargetJTI != jti {
		t.Errorf("TargetJTI=%v, want %q", f.rev.last.TargetJTI, jti)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRevoke_NoHint_AccessFirstFallsToRefresh covers the default (no hint)
// path where access verify fails (not our token) and then the refresh probe
// resolves. We MUST see 1 revocation with ScopeSession.
func TestRevoke_NoHint_AccessFirstFallsToRefresh(t *testing.T) {
	f := newRevokeMockFixture(t)
	row := makeRefreshRow()
	// Opaque random refresh token — VerifyLocal will fail to parse it as a JWT.
	f.expectFindRow("opaque-rt", row)
	f.expectDelete(row.SessionID)

	rec := f.post(url.Values{
		"token":     {"opaque-rt"},
		"client_id": {row.ClientID},
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
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRevoke_BothPathsMiss_Silent200 covers the final fallthrough — neither
// branch resolves so the handler returns the anti-enumeration 200 with no
// revocation recorded.
func TestRevoke_BothPathsMiss_Silent200(t *testing.T) {
	f := newRevokeMockFixture(t)
	f.expectFindEmpty("phantom-token")
	rec := f.post(url.Values{
		"token": {"phantom-token"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke called %d times on phantom token, want 0", n)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRevoke_NilLogger_UsesDiscard ensures the Logger==nil branch picks
// io.Discard so handlers don't panic when wired without a logger. We hit an
// error path (delete fails) so the logger is actually exercised.
func TestRevoke_NilLogger_UsesDiscard(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	rs := store.NewRefreshStoreWithPool(mock)
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	rec := &recordingRevocation{}
	e := echo.New()
	e.POST("/oauth/revoke", oauth.RevokeHandler(oauth.RevokeDeps{
		Issuer:     "https://cp.nexus.ai",
		Keystore:   ks,
		Refresh:    rs,
		Revocation: rec,
		// Logger left nil intentionally.
	}))

	row := makeRefreshRow()
	hash := token.DefaultRefreshHash([]byte("rt"))
	mock.ExpectQuery(`FROM "RefreshToken"`).WithArgs(hash).
		WillReturnRows(pgxmock.NewRows(refreshTokenRowCols).
			AddRow(row.JTI, row.SessionID, row.ParentJTI,
				row.UserID, row.ClientID, row.DeviceID, row.TokenHash,
				row.UsedAt, row.ExpiresAt, row.CreatedAt))
	mock.ExpectExec(`DELETE FROM "RefreshToken"`).
		WithArgs(row.SessionID).
		WillReturnError(errors.New("oops"))

	req := httptest.NewRequest(http.MethodPost, "/oauth/revoke",
		strings.NewReader(url.Values{"token": {"rt"}, "token_type_hint": {"refresh_token"}}.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// forgeAccessTokenNoExp builds a JWT signed by ks's active key that has an
// iss/sub/aud claim set but no "exp". This is the only way to exercise the
// "claims.ExpiresAt == nil" branch — token.IssueAccess always stamps exp,
// so the handler's defensive check is otherwise dead code.
func forgeAccessTokenNoExp(t *testing.T, ks *token.Keystore, issuer, clientID string) string {
	t.Helper()
	kid := ks.ActiveKID()
	k, ok := ks.ByKID(kid)
	if !ok {
		t.Fatalf("active kid %q not in keystore", kid)
	}
	c := token.AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:   issuer,
			Subject:  "usr-1",
			Audience: jwt.ClaimStrings{token.AdminAudience},
			IssuedAt: jwt.NewNumericDate(time.Now()),
			ID:       "forged-no-exp",
			// ExpiresAt intentionally left nil — VerifyLocal accepts a JWT with
			// no exp claim because jwt/v5's RegisteredClaims.Valid only checks
			// exp if set. This exercises the handler's defensive
			// "claims.ExpiresAt == nil" branch.
		},
		ClientID: clientID,
	}
	jt := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	jt.Header["kid"] = kid
	tokStr, err := jt.SignedString(k.Priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return tokStr
}
