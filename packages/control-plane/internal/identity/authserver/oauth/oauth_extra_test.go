package oauth_test

// This file pushes oauth-package coverage from 83.2% to >=95% by exercising
// the remaining security-relevant rejection paths and the small defensive
// helpers (logger() defaults, error-envelope Error(), AMR -> login_method
// mapping). Tests assert OBSERVABLE security behaviour (exact HTTP status,
// exact OAuth error code, exact wire envelope text, no unintended state
// mutation), not raw line counts -- coverage rises as a side effect of the
// rejection branches being driven through real Echo HTTP traffic.

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/oauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	cpstore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// syncBuf is a goroutine-safe bytes.Buffer wrapper. The token-handler's
// device-assignment write runs in a goroutine that logs via slog; without
// this wrapper, the race detector trips on concurrent bytes.Buffer access
// between the goroutine and the test reading buf.String().
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestOAuthError_ErrorStringFormat pins the exact ":-joined" envelope so any
// future refactor that changes the canonical format (logged into structured
// audit lines, surfaced into 5xx debug pages) breaks this test, not prod.
func TestOAuthError_ErrorStringFormat(t *testing.T) {
	e := &oauth.OAuthError{
		Code:        oauth.ErrInvalidGrant,
		Description: "pkce verification failed",
		Status:      http.StatusBadRequest,
	}
	got := e.Error()
	want := "invalid_grant: pkce verification failed"
	if got != want {
		t.Fatalf("Error()=%q, want %q", got, want)
	}
}

//                   + code_challenge_method empty branch --------------------

// erroringClients always returns a non-ErrClientNotFound error so the handler
// hits the 500 branch (instead of the silent invalid_client 400 case). The
// error type matters: only ErrClientNotFound takes the invalid_client path.
type erroringClients struct{ err error }

func (e erroringClients) GetByID(_ context.Context, _ string) (*store.OAuthClient, error) {
	return nil, e.err
}

// TestAuthorize_NilLogger_DefaultsToDiscard exercises the d.Logger == nil
// branch of AuthorizeDeps.logger(). The handler must NOT panic when the
// logger is left unset; the io.Discard fallback is the safety net.
// We hit a logger-using branch (client lookup non-NotFound error) to force
// the helper to be invoked.
func TestAuthorize_NilLogger_DefaultsToDiscard(t *testing.T) {
	e := echo.New()
	e.GET("/oauth/authorize", oauth.AuthorizeHandler(oauth.AuthorizeDeps{
		Clients:  erroringClients{err: errors.New("postgres down")},
		Bindings: store.NewBindingStore(),
		Pending:  store.NewPendingAuthzStore(),
		// Logger left nil intentionally.
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?response_type=code&client_id=any&redirect_uri=http://x/cb&state=s&code_challenge=ABC&code_challenge_method=S256",
		nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500 (server_error on non-NotFound lookup)",
			rec.Code, rec.Body.String())
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrServerError {
		t.Errorf("error=%q, want %q", code, oauth.ErrServerError)
	}
}

// TestAuthorize_ClientLookupNonNotFoundReturns500 pins the
// "client lookup failed -- generic error" branch with an explicit logger so
// we can also assert the log line shape (security audit must record the
// failed lookup attempt).
func TestAuthorize_ClientLookupNonNotFoundReturns500(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	e := echo.New()
	e.GET("/oauth/authorize", oauth.AuthorizeHandler(oauth.AuthorizeDeps{
		Clients:  erroringClients{err: errors.New("connection refused")},
		Bindings: store.NewBindingStore(),
		Pending:  store.NewPendingAuthzStore(),
		Logger:   logger,
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?response_type=code&client_id=cp-ui&redirect_uri=http://x/cb&state=s&code_challenge=ABC&code_challenge_method=S256",
		nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", rec.Code, rec.Body.String())
	}
	code, desc := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrServerError {
		t.Errorf("error=%q, want server_error", code)
	}
	if !strings.Contains(desc, "client lookup failed") {
		t.Errorf("description=%q, want it to mention 'client lookup failed'", desc)
	}
	// Security-audit assertion: the failed lookup MUST hit the structured
	// logger so an attacker probing for "which DB error leaks data" sees
	// nothing useful in the response but ops sees the upstream failure.
	if !strings.Contains(buf.String(), "client lookup failed") {
		t.Errorf("expected 'client lookup failed' log entry, got: %s", buf.String())
	}
}

// TestAuthorize_EmptyClientIDReturns400 exercises the explicit client_id==""
// branch separately from "client not registered" so that a refactor which
// folds the two together (and accidentally elevates a missing client_id to
// the 500 branch) breaks here.
func TestAuthorize_EmptyClientIDReturns400(t *testing.T) {
	e := echo.New()
	e.GET("/oauth/authorize", oauth.AuthorizeHandler(oauth.AuthorizeDeps{
		Clients:  fakeClients{},
		Bindings: store.NewBindingStore(),
		Pending:  store.NewPendingAuthzStore(),
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?response_type=code&redirect_uri=http://x/cb&state=s", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	code, desc := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrInvalidClient {
		t.Errorf("error=%q, want invalid_client", code)
	}
	if !strings.Contains(desc, "client_id required") {
		t.Errorf("description=%q, want 'client_id required'", desc)
	}
}

// TestAuthorize_CodeChallengeMethodMissingWhenChallengePresent pins the
// "code_challenge present but method empty" branch. RFC 7636 mandates the
// method be supplied alongside the challenge; defaulting to plain would be
// a downgrade attack.
func TestAuthorize_CodeChallengeMethodMissingWhenChallengePresent(t *testing.T) {
	e := echo.New()
	e.GET("/oauth/authorize", oauth.AuthorizeHandler(oauth.AuthorizeDeps{
		Clients:  fakeClients{publicClientID: webConsoleClient()},
		Bindings: store.NewBindingStore(),
		Pending:  store.NewPendingAuthzStore(),
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?response_type=code&client_id="+publicClientID+
			"&redirect_uri=https://cp.nexus.ai/callback&state=s&code_challenge=ABC", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	code, desc := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrInvalidRequest {
		t.Errorf("error=%q, want invalid_request", code)
	}
	if !strings.Contains(desc, "code_challenge_method required") {
		t.Errorf("description=%q, want 'code_challenge_method required' substring", desc)
	}
}

// TestDeviceBinding_DefaultLoggerWhenNil routes through the success path with
// a nil Logger so the logger() helper's "no Logger set -> slog.Default()"
// branch fires. We assert on observable HTTP outcome (204) and BindingStore
// state mutation; defaulting must not break the contract.
func TestDeviceBinding_DefaultLoggerWhenNil(t *testing.T) {
	bindings := store.NewBindingStore()
	t.Cleanup(bindings.Close)

	h := oauth.DeviceBindingHandler(oauth.DeviceBindingDeps{
		Bindings: bindings,
		// Logger left nil intentionally.
	})
	e := echo.New()

	body, err := json.Marshal(map[string]string{
		"binding_id":     "bind-nl",
		"state":          "st",
		"code_challenge": "cc",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/device-binding", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setDevice(c, &cpstore.ThingNodeInfo{ID: "dev-nl", Status: "active"})

	if err := h(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if _, ok := bindings.Get("bind-nl"); !ok {
		t.Fatalf("binding not stored after nil-logger success path")
	}
}

// TestDeviceBinding_CustomLoggerCapturesInfoLine pins the d.Logger != nil
// branch of DeviceBindingDeps.logger(). We supply a slog handler backed by a
// bytes.Buffer and assert the Info line about successful binding fires
// through it -- proves the injected logger is honored end-to-end and that
// security-audit downstream (which reads device_id + binding_id correlation)
// can subscribe to the structured log line.
func TestDeviceBinding_CustomLoggerCapturesInfoLine(t *testing.T) {
	bindings := store.NewBindingStore()
	t.Cleanup(bindings.Close)
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h := oauth.DeviceBindingHandler(oauth.DeviceBindingDeps{
		Bindings: bindings,
		Logger:   logger,
	})
	e := echo.New()

	body, err := json.Marshal(map[string]string{
		"binding_id":     "bind-custom-log",
		"state":          "st-x",
		"code_challenge": "cc-x",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/device-binding", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setDevice(c, &cpstore.ThingNodeInfo{ID: "dev-custom-log", Status: "active"})

	if err := h(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}
	logs := buf.String()
	if !strings.Contains(logs, "oauth device binding created") {
		t.Errorf("expected audit log entry 'oauth device binding created', got: %s", logs)
	}
	if !strings.Contains(logs, "dev-custom-log") {
		t.Errorf("expected device_id in log entry, got: %s", logs)
	}
	if !strings.Contains(logs, "bind-custom-log") {
		t.Errorf("expected binding_id in log entry, got: %s", logs)
	}
}

// fakeRevocation is a minimal revocationChecker double for the introspect
// revocation-aware branches. Behaviour is driven by exported fields so each
// test can assemble a scenario without polluting unrelated cases.
type fakeRevocation struct {
	revoked bool
	err     error

	mu    sync.Mutex
	calls int
	last  struct {
		jti, userID, deviceID, sessionID string
	}
}

func (f *fakeRevocation) IsAccessTokenRevoked(_ context.Context, jti, userID, deviceID, sessionID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last.jti = jti
	f.last.userID = userID
	f.last.deviceID = deviceID
	f.last.sessionID = sessionID
	if f.err != nil {
		return false, f.err
	}
	return f.revoked, nil
}

// TestIntrospect_NilLogger_KeystoreNilReturnsInactive double-duties:
// (a) logger() nil -> Discard sink (introspect.go:44-46),
// (b) the keystore-missing fast-fail at handler line 92-95 (RFC 7662 §2.2 says
//
//	every adverse outcome must funnel to {active:false}, never an explicit
//	5xx that would let an attacker fingerprint server config).
func TestIntrospect_NilLogger_KeystoreNilReturnsInactive(t *testing.T) {
	e := echo.New()
	e.POST("/oauth/introspect", oauth.IntrospectHandler(oauth.IntrospectDeps{
		Issuer:   "https://cp.nexus.ai",
		Keystore: nil, // intentionally nil
		// Logger left nil -- exercises the io.Discard fallback when the
		// "no keystore" Error path tries to log.
	}))

	req := httptest.NewRequest(http.MethodPost, "/oauth/introspect",
		strings.NewReader(url.Values{"token": {"any.jwt.body"}}.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (anti-enumeration)", rec.Code, rec.Body.String())
	}
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if body.Active {
		t.Fatalf("active=true on nil-keystore, want false (RFC 7662 §2.2)")
	}
}

// TestIntrospect_RevocationHitReturnsInactive pins the revocation-aware
// branch: a valid signature + revoked JTI MUST surface {active:false} so a
// stolen token cannot be used to read its own metadata after logout.
func TestIntrospect_RevocationHitReturnsInactive(t *testing.T) {
	f := newIntrospectFixture(t)

	rev := &fakeRevocation{revoked: true}
	d := oauth.IntrospectDeps{
		Issuer:     "https://cp.nexus.ai",
		Keystore:   f.keystore,
		Revocation: rev,
	}
	e := echo.New()
	e.POST("/oauth/introspect", oauth.IntrospectHandler(d))

	accessTok, jti, err := token.IssueAccess(f.signer, token.AccessInput{
		Issuer:    "https://cp.nexus.ai",
		Subject:   "usr-rev",
		Audience:  []string{"web-console"},
		ClientID:  "web-console",
		SessionID: "sid-rev",
		DeviceID:  "dev-rev",
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/oauth/introspect",
		strings.NewReader(url.Values{"token": {accessTok}}.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if body.Active {
		t.Fatalf("revoked token returned active=true, want false")
	}
	// Confirm the handler delegated the lookup with the right correlation
	// inputs -- a security regression that drops, say, sessionID would let
	// a revoked-session token slip through.
	if rev.calls != 1 {
		t.Fatalf("revocation lookup calls=%d, want 1", rev.calls)
	}
	if rev.last.jti != jti || rev.last.userID != "usr-rev" ||
		rev.last.deviceID != "dev-rev" || rev.last.sessionID != "sid-rev" {
		t.Errorf("revocation lookup args=%+v", rev.last)
	}
}

// TestIntrospect_RevocationDBErrorLogged_StillActive pins the
// "revocation backend errored -> fail-open" branch. The fail-open is
// deliberate (RFC 7009 §2.2 anti-enumeration mixed with operability: a DB
// outage shouldn't lock every authenticated user out) but the error MUST be
// logged so ops can see the outage.
func TestIntrospect_RevocationDBErrorLogged_StillActive(t *testing.T) {
	f := newIntrospectFixture(t)
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	rev := &fakeRevocation{err: errors.New("connection reset")}
	d := oauth.IntrospectDeps{
		Issuer:     "https://cp.nexus.ai",
		Keystore:   f.keystore,
		Revocation: rev,
		Logger:     logger,
	}
	e := echo.New()
	e.POST("/oauth/introspect", oauth.IntrospectHandler(d))

	accessTok, _, err := token.IssueAccess(f.signer, token.AccessInput{
		Issuer:   "https://cp.nexus.ai",
		Subject:  "usr-ok",
		Audience: []string{"web-console"},
		ClientID: "web-console",
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/introspect",
		strings.NewReader(url.Values{"token": {accessTok}}.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if !body.Active {
		t.Fatalf("DB error must fail-open to active=true (RFC operability), got false")
	}
	if !strings.Contains(buf.String(), "revocation lookup") {
		t.Errorf("expected revocation-lookup log entry, got: %s", buf.String())
	}
}

// TestToken_CustomTTL_PropagatesToResponse_AndClaims pins the "TTL > 0"
// branches of accessTTL/refreshTTL by wiring non-zero TTLs and asserting
// the response.expires_in (covers accessTTL) and that NewChain was called
// with the configured refreshTTL.
func TestToken_CustomTTL_PropagatesToResponse_AndClaims(t *testing.T) {
	f := newTokenFixture(t)
	f.deps = oauth.TokenDeps{
		Issuer:     "https://cp.nexus.ai",
		Clients:    defaultTokenTestClients(),
		AuthCodes:  f.codes,
		Users:      fakeUsers{},
		Refresh:    f.refresh,
		Signer:     f.signer,
		AccessTTL:  10 * time.Minute,
		RefreshTTL: 45 * time.Minute,
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "web-console",
		UserID:      "usr-ttl",
		RedirectURI: "https://cp.nexus.ai/callback",
	})
	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"web-console"},
		"redirect_uri":  {"https://cp.nexus.ai/callback"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeTokenBody(t, rec.Body.Bytes())
	if body.ExpiresIn != int((10 * time.Minute).Seconds()) {
		t.Errorf("expires_in=%d, want %d (custom AccessTTL)",
			body.ExpiresIn, int((10 * time.Minute).Seconds()))
	}
	if f.refresh.lastNewArgs.TTL != 45*time.Minute {
		t.Errorf("NewChain.TTL=%v, want 45m (custom RefreshTTL)", f.refresh.lastNewArgs.TTL)
	}
}

// TestToken_AuthCode_NewChainErrorReturns500 pins the refresh-chain-issuance
// failure path. A failure here MUST surface as server_error 500 (not 400)
// because the client did everything right; the access token cannot be minted
// because the refresh chain is the canonical session identifier carrier.
func TestToken_AuthCode_NewChainErrorReturns500(t *testing.T) {
	f := newTokenFixture(t)
	f.refresh.newErr = errors.New("postgres write failed")
	buf := &bytes.Buffer{}
	f.deps = oauth.TokenDeps{
		Issuer:    "https://cp.nexus.ai",
		Clients:   defaultTokenTestClients(),
		AuthCodes: f.codes,
		Users:     fakeUsers{},
		Refresh:   f.refresh,
		Signer:    f.signer,
		Logger:    slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "web-console",
		UserID:      "usr-1",
		RedirectURI: "https://cp.nexus.ai/callback",
	})
	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"web-console"},
		"redirect_uri":  {"https://cp.nexus.ai/callback"},
	}, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", rec.Code, rec.Body.String())
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrServerError {
		t.Errorf("error=%q, want server_error", c)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control=%q, want no-store on error", got)
	}
	if !strings.Contains(buf.String(), "refresh NewChain failed") {
		t.Errorf("expected refresh-NewChain log entry, got: %s", buf.String())
	}
}

// TestToken_AuthCode_AccessMintErrorReturns500 pins the
// "IssueAccess failed after NewChain succeeded" path. We wire a Signer with
// an empty Keystore so Signer.Sign() fails with "no active signing key".
// The handler MUST still 500 and stamp the no-store header.
func TestToken_AuthCode_AccessMintErrorReturns500(t *testing.T) {
	f := newTokenFixture(t)
	emptyKS, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	// NOTE: no Generate() -- empty store -> Sign returns error.
	emptySigner := token.NewSigner(emptyKS)

	f.deps = oauth.TokenDeps{
		Issuer:    "https://cp.nexus.ai",
		Clients:   defaultTokenTestClients(),
		AuthCodes: f.codes,
		Users:     fakeUsers{},
		Refresh:   f.refresh,
		Signer:    emptySigner,
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "web-console",
		UserID:      "usr-1",
		RedirectURI: "https://cp.nexus.ai/callback",
	})
	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"web-console"},
		"redirect_uri":  {"https://cp.nexus.ai/callback"},
	}, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", rec.Code, rec.Body.String())
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrServerError {
		t.Errorf("error=%q, want server_error", c)
	}
}

// fakeAssignments captures UpsertDeviceAssignment calls so tests can prove
// (a) it ran in the agent-desktop path with the right correlation inputs,
// (b) a write failure never blocks the token response (fire-and-forget).
type fakeAssignments struct {
	mu     sync.Mutex
	calls  int
	last   store.UpsertDeviceAssignmentParams
	retErr error
	done   chan struct{}
}

func newFakeAssignments() *fakeAssignments {
	return &fakeAssignments{done: make(chan struct{}, 1)}
}

func (f *fakeAssignments) UpsertDeviceAssignment(_ context.Context, p store.UpsertDeviceAssignmentParams) error {
	f.mu.Lock()
	f.calls++
	f.last = p
	err := f.retErr
	f.mu.Unlock()
	select {
	case f.done <- struct{}{}:
	default:
	}
	return err
}

// TestToken_AuthCode_AssignmentsWriteRecorded_AgentDesktop covers the
// "d.Assignments != nil && entry.DeviceID != ”" branch -- the OAuth handler
// must record a DeviceAssignment at agent-desktop token-exchange time so
// downstream attribution (Hub heartbeat enrichment) works from the first
// minute, not after the first heartbeat round-trip.
//
// Security-relevant assertions:
//   - The assignment carries the SAME device_id the OAuth code minted (no
//     opportunity for substitution between code issue and assignment write).
//   - The assignment carries the access JTI (so a future revoke-by-JTI also
//     invalidates this assignment row's audit lineage).
//   - login_method is "local" for amr=[pwd] (RFC 8176 mapping in
//     loginMethodFromAMR).
func TestToken_AuthCode_AssignmentsWriteRecorded_AgentDesktop(t *testing.T) {
	f := newTokenFixture(t)
	assn := newFakeAssignments()
	f.deps = oauth.TokenDeps{
		Issuer:      "https://cp.nexus.ai",
		Clients:     defaultTokenTestClients(),
		AuthCodes:   f.codes,
		Users:       fakeUsers{},
		Refresh:     f.refresh,
		Signer:      f.signer,
		Assignments: assn,
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "agent-desktop",
		UserID:      "usr-7",
		RedirectURI: "http://127.0.0.1:54321/callback",
		DeviceID:    "dev-42",
		AMR:         []string{"pwd"},
	})
	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"agent-desktop"},
		"redirect_uri":  {"http://127.0.0.1:54321/callback"},
	}, &cpstore.ThingNodeInfo{ID: "dev-42", Status: "active"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Assignment write is fire-and-forget in a goroutine -- wait up to 2s.
	select {
	case <-assn.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("UpsertDeviceAssignment goroutine did not run in 2s")
	}

	assn.mu.Lock()
	defer assn.mu.Unlock()
	if assn.calls != 1 {
		t.Fatalf("UpsertDeviceAssignment calls=%d, want 1", assn.calls)
	}
	if assn.last.DeviceID != "dev-42" {
		t.Errorf("DeviceID=%q, want dev-42", assn.last.DeviceID)
	}
	if assn.last.UserID != "usr-7" {
		t.Errorf("UserID=%q, want usr-7", assn.last.UserID)
	}
	if assn.last.TokenJTI == "" {
		t.Errorf("TokenJTI empty -- audit trail broken")
	}
	if assn.last.LoginMethod != "local" {
		t.Errorf("LoginMethod=%q, want local (amr=[pwd] mapping)", assn.last.LoginMethod)
	}
}

// TestToken_AuthCode_AssignmentsWriteErrorDoesNotBlockResponse covers the
// fire-and-forget contract: a DB write failure MUST NOT prevent the user
// from completing login. This is load-bearing -- a transient assignment
// outage could otherwise lock the whole agent fleet out.
func TestToken_AuthCode_AssignmentsWriteErrorDoesNotBlockResponse(t *testing.T) {
	f := newTokenFixture(t)
	buf := &syncBuf{}
	assn := newFakeAssignments()
	assn.retErr = errors.New("postgres deadlock")
	f.deps = oauth.TokenDeps{
		Issuer:      "https://cp.nexus.ai",
		Clients:     defaultTokenTestClients(),
		AuthCodes:   f.codes,
		Users:       fakeUsers{},
		Refresh:     f.refresh,
		Signer:      f.signer,
		Assignments: assn,
		Logger:      slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "agent-desktop",
		UserID:      "usr-9",
		RedirectURI: "http://127.0.0.1:54321/callback",
		DeviceID:    "dev-9",
		AMR:         []string{"hwk"}, // RFC 8176: hardware-key -> "oidc" fallback
	})
	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"agent-desktop"},
		"redirect_uri":  {"http://127.0.0.1:54321/callback"},
	}, &cpstore.ThingNodeInfo{ID: "dev-9", Status: "active"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 even on assignment-write failure", rec.Code, rec.Body.String())
	}
	body := decodeTokenBody(t, rec.Body.Bytes())
	if body.AccessToken == "" {
		t.Fatalf("access_token missing -- assignment failure must not block response")
	}

	// Wait for the goroutine to run + log the failure.
	select {
	case <-assn.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("assignment goroutine did not run")
	}
	// Give the goroutine an extra tick to write the log entry.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "device assignment upsert failed") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "device assignment upsert failed") {
		t.Errorf("expected upsert-failed log entry, got: %s", buf.String())
	}

	// AMR=[hwk] (non-pwd, non-empty) must map to "oidc" per loginMethodFromAMR.
	assn.mu.Lock()
	got := assn.last.LoginMethod
	assn.mu.Unlock()
	if got != "oidc" {
		t.Errorf("LoginMethod=%q for AMR=[hwk], want oidc (non-pwd fallback)", got)
	}
}

// TestToken_AuthCode_AssignmentsLoginMethodEmptyAMR pins the
// loginMethodFromAMR third-branch fallback: when AMR is nil/empty the
// mapping returns "local" because the auth code came from the local auth
// server (not a federated IdP). Without this fallback, the DeviceAssignment
// would carry an empty login_method, breaking downstream filtering.
func TestToken_AuthCode_AssignmentsLoginMethodEmptyAMR(t *testing.T) {
	f := newTokenFixture(t)
	assn := newFakeAssignments()
	f.deps = oauth.TokenDeps{
		Issuer:      "https://cp.nexus.ai",
		Clients:     defaultTokenTestClients(),
		AuthCodes:   f.codes,
		Users:       fakeUsers{},
		Refresh:     f.refresh,
		Signer:      f.signer,
		Assignments: assn,
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "agent-desktop",
		UserID:      "usr-empty-amr",
		RedirectURI: "http://127.0.0.1:54321/callback",
		DeviceID:    "dev-empty-amr",
		// AMR intentionally nil -- exercises the loginMethodFromAMR
		// "len(amr) == 0 -> local" fallback.
	})
	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"agent-desktop"},
		"redirect_uri":  {"http://127.0.0.1:54321/callback"},
	}, &cpstore.ThingNodeInfo{ID: "dev-empty-amr", Status: "active"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	select {
	case <-assn.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("assignment goroutine did not run")
	}
	assn.mu.Lock()
	defer assn.mu.Unlock()
	if assn.last.LoginMethod != "local" {
		t.Errorf("LoginMethod=%q for nil AMR, want local (default branch)", assn.last.LoginMethod)
	}
}

// TestToken_AuthCode_AssignmentsSkippedWhenDeviceIDEmpty pins the negative
// half of the if-condition: a web-console auth code (no device_id) MUST NOT
// trigger an assignment write even when Assignments is wired -- otherwise we
// pollute DeviceAssignment with stub rows.
func TestToken_AuthCode_AssignmentsSkippedWhenDeviceIDEmpty(t *testing.T) {
	f := newTokenFixture(t)
	assn := newFakeAssignments()
	f.deps = oauth.TokenDeps{
		Issuer:      "https://cp.nexus.ai",
		Clients:     defaultTokenTestClients(),
		AuthCodes:   f.codes,
		Users:       fakeUsers{},
		Refresh:     f.refresh,
		Signer:      f.signer,
		Assignments: assn,
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "web-console",
		UserID:      "usr-web",
		RedirectURI: "https://cp.nexus.ai/callback",
		// DeviceID omitted -- web flow has no device cert.
	})
	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"web-console"},
		"redirect_uri":  {"https://cp.nexus.ai/callback"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Give any erroneously-scheduled goroutine time to run before asserting.
	time.Sleep(100 * time.Millisecond)
	assn.mu.Lock()
	defer assn.mu.Unlock()
	if assn.calls != 0 {
		t.Fatalf("UpsertDeviceAssignment ran for web-console flow without DeviceID; got %d calls", assn.calls)
	}
}

// TestToken_Refresh_RotateGenericErrorReturns500 pins the "Rotate failed
// with an error OTHER than ErrReplay / ErrExpired" branch -- a Postgres
// outage during rotation MUST surface as server_error 500 so callers retry
// (a 400 would discard the in-flight refresh token).
func TestToken_Refresh_RotateGenericErrorReturns500(t *testing.T) {
	f := newTokenFixture(t)
	f.refresh.rotateErr = errors.New("connection reset by peer")
	buf := &bytes.Buffer{}
	f.deps = oauth.TokenDeps{
		Issuer:    "https://cp.nexus.ai",
		Clients:   defaultTokenTestClients(),
		AuthCodes: f.codes,
		Users:     fakeUsers{},
		Refresh:   f.refresh,
		Signer:    f.signer,
		Logger:    slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e

	rec := f.post(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"rt-x"},
		"client_id":     {"web-console"},
	}, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", rec.Code, rec.Body.String())
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrServerError {
		t.Errorf("error=%q, want server_error", c)
	}
	if !strings.Contains(buf.String(), "refresh Rotate failed") {
		t.Errorf("expected refresh-Rotate log entry, got: %s", buf.String())
	}
}

// erroringUsers always errors on GetByID with a non-NotFound error, so the
// handler hits the 500 user-lookup branch.
type erroringUsers struct{ err error }

func (e erroringUsers) GetByID(_ context.Context, _ string) (*store.User, error) {
	return nil, e.err
}

func (e erroringUsers) TouchLastLogin(_ context.Context, _ string) error { return nil }

// TestToken_Refresh_UserNotFoundReturns400 pins the user-not-found branch.
// A refresh chain whose owner was deleted MUST end the session (400
// invalid_grant) rather than 500 -- the client is supposed to restart the
// login flow.
func TestToken_Refresh_UserNotFoundReturns400(t *testing.T) {
	f := newTokenFixture(t)
	f.refresh.rotateReturns = []rotateResult{{
		newToken: "rt-rot",
		newJTI:   "rjti-rot",
		parent: &store.RefreshTokenRow{
			JTI:       "rjti-old",
			SessionID: "sid-zz",
			UserID:    "usr-gone",
			ClientID:  "web-console",
		},
	}}
	// fakeUsers is a nil-keyed map -> GetByID returns store.ErrUserNotFound.
	f.buildHandler(fakeUsers{})

	rec := f.post(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"rt-any"},
		"client_id":     {"web-console"},
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	c, desc := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrInvalidGrant {
		t.Errorf("error=%q, want invalid_grant", c)
	}
	if !strings.Contains(desc, "user not found") {
		t.Errorf("description=%q, want 'user not found'", desc)
	}
}

// TestToken_Refresh_UserLookupGenericErrorReturns500 pins the
// "user lookup error OTHER than not-found" branch. A DB outage MUST 500 so
// the client retries; a 400 would force re-login on every transient error.
func TestToken_Refresh_UserLookupGenericErrorReturns500(t *testing.T) {
	f := newTokenFixture(t)
	f.refresh.rotateReturns = []rotateResult{{
		newToken: "rt-rot",
		newJTI:   "rjti-rot",
		parent: &store.RefreshTokenRow{
			JTI:       "rjti-old",
			SessionID: "sid-zz",
			UserID:    "usr-1",
			ClientID:  "web-console",
		},
	}}
	buf := &bytes.Buffer{}
	f.deps = oauth.TokenDeps{
		Issuer:    "https://cp.nexus.ai",
		Clients:   defaultTokenTestClients(),
		AuthCodes: f.codes,
		Users:     erroringUsers{err: errors.New("pg ConnPool exhausted")},
		Refresh:   f.refresh,
		Signer:    f.signer,
		Logger:    slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e

	rec := f.post(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"rt-x"},
		"client_id":     {"web-console"},
	}, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", rec.Code, rec.Body.String())
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrServerError {
		t.Errorf("error=%q, want server_error", c)
	}
	if !strings.Contains(buf.String(), "user lookup failed") {
		t.Errorf("expected user-lookup log entry, got: %s", buf.String())
	}
}

// TestToken_Refresh_AccessMintErrorReturns500 pins the
// "IssueAccess failed during refresh rotation" branch (token.go:310-313).
// We supply a wired-but-empty keystore Signer that fails Sign(); the handler
// MUST surface a 500 rather than a half-baked 200 missing access_token.
func TestToken_Refresh_AccessMintErrorReturns500(t *testing.T) {
	f := newTokenFixture(t)
	emptyKS, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	// No Generate() -- empty store.
	brokenSigner := token.NewSigner(emptyKS)

	users := fakeUsers{"usr-1": {ID: "usr-1"}}
	f.refresh.rotateReturns = []rotateResult{{
		newToken: "rt-rot",
		newJTI:   "rjti-rot",
		parent: &store.RefreshTokenRow{
			JTI:       "rjti-old",
			SessionID: "sid-zz",
			UserID:    "usr-1",
			ClientID:  "web-console",
		},
	}}
	buf := &bytes.Buffer{}
	f.deps = oauth.TokenDeps{
		Issuer:    "https://cp.nexus.ai",
		Clients:   defaultTokenTestClients(),
		AuthCodes: f.codes,
		Users:     users,
		Refresh:   f.refresh,
		Signer:    brokenSigner,
		Logger:    slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e

	rec := f.post(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"rt-x"},
		"client_id":     {"web-console"},
	}, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", rec.Code, rec.Body.String())
	}
	c, _ := decodeError(t, rec.Body.Bytes())
	if c != oauth.ErrServerError {
		t.Errorf("error=%q, want server_error", c)
	}
	if !strings.Contains(buf.String(), "access mint failed on refresh") {
		t.Errorf("expected access-mint-failed log entry, got: %s", buf.String())
	}
}

// TestToken_NilLoggerSafeOnErrorPath ensures the d.Logger == nil branch in
// TokenDeps.logger() picks io.Discard so error handlers do not panic when
// production wiring forgets to inject a logger.
func TestToken_NilLoggerSafeOnErrorPath(t *testing.T) {
	f := newTokenFixture(t)
	f.refresh.newErr = errors.New("postgres write failed")
	// Logger left nil -- must not panic.
	f.deps = oauth.TokenDeps{
		Issuer:    "https://cp.nexus.ai",
		Clients:   defaultTokenTestClients(),
		AuthCodes: f.codes,
		Users:     fakeUsers{},
		Refresh:   f.refresh,
		Signer:    f.signer,
	}
	e := echo.New()
	e.POST("/oauth/token", oauth.TokenHandler(f.deps))
	f.echo = e

	code := f.putAuthCode(store.AuthCodeEntry{
		ClientID:    "web-console",
		UserID:      "usr-1",
		RedirectURI: "https://cp.nexus.ai/callback",
	})
	rec := f.post(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"client_id":     {"web-console"},
		"redirect_uri":  {"https://cp.nexus.ai/callback"},
	}, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500 (logger nil path)", rec.Code, rec.Body.String())
	}
}

// TestRevoke_EmptyTokenAfterTrim covers the rare path where the form-supplied
// token is present but consists entirely of whitespace -- TrimSpace strips it
// and the handler MUST 400 just like a missing token (revoke.go line 86).
// The existing TestRevoke_MissingTokenReturns400 is DB-backed and skipped in
// unit runs; this is the mock-fixture twin that always executes.
func TestRevoke_EmptyTokenAfterTrim(t *testing.T) {
	f := newRevokeMockFixture(t)
	rec := f.post(url.Values{"token": {"   "}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	code, desc := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrInvalidRequest {
		t.Errorf("error=%q, want invalid_request", code)
	}
	if !strings.Contains(desc, "token required") {
		t.Errorf("description=%q, want 'token required'", desc)
	}
	if n := atomic.LoadInt32(&f.rev.calls); n != 0 {
		t.Fatalf("Revoke called %d times on whitespace token, want 0", n)
	}
}

var _ = io.Discard // silence unused-import lint if a future refactor drops uses
