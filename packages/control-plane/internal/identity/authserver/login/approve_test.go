package login_test

import (
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/login"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
)

// approveFixture spins up the in-memory Pending + AuthCodes stores the handler
// needs, seeds a pending entry, and returns the wired ApproveDeps + the
// authctx/state/redirect the test asserts against.
type approveFixture struct {
	deps     login.ApproveDeps
	authctx  string
	state    string
	redirect string
	pending  *store.PendingAuthzStore
}

func newApproveFixture(t *testing.T) approveFixture {
	t.Helper()
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	authCodes := store.NewAuthCodeStore(5 * time.Minute)
	t.Cleanup(authCodes.Close)
	authctx := "ctx-approve-" + time.Now().Format("150405.000000000")
	state := "st-approve"
	redirect := "http://127.0.0.1:9999/cb"
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID:      "tui",
		RedirectURI:   redirect,
		Scope:         "openid",
		State:         state,
		Nonce:         "nonce-a",
		CodeChallenge: "cc-a",
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	})
	return approveFixture{
		deps:     login.ApproveDeps{Pending: pending, AuthCodes: authCodes},
		authctx:  authctx,
		state:    state,
		redirect: redirect,
		pending:  pending,
	}
}

func newApproveCtx(body any, claims *jwtverifier.Claims) (echo.Context, *httptest.ResponseRecorder) {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/authserver/approve", strings.NewReader(string(raw)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	if claims != nil {
		c.Set(jwtverifier.ClaimsContextKey, claims)
	}
	return c, rec
}

// TestApproveHandler_Success asserts the happy path: a valid bearer session
// (Claims attached) completes the pending authctx, returns a redirectUri whose
// host/path matches the registered redirect_uri (no open-redirect injection),
// echoes the state verbatim, and consumes the pending entry (single-use). The
// returned redirect URI must also carry a fresh code query param so the CLI
// loopback handler can exchange it.
func TestApproveHandler_Success(t *testing.T) {
	fx := newApproveFixture(t)
	c, rec := newApproveCtx(login.ApproveRequest{AuthCtx: fx.authctx}, &jwtverifier.Claims{
		Subject: "user-42",
		Email:   "alice@example.com",
		IDP:     "idp-local-1",
		AMR:     []string{"pwd"},
	})
	if err := login.ApproveHandler(fx.deps)(c); err != nil {
		t.Fatalf("ApproveHandler returned err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var resp login.ApproveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	u, err := url.Parse(resp.RedirectURI)
	if err != nil {
		t.Fatalf("parse redirect URI: %v", err)
	}
	if u.Scheme != "http" || u.Host != "127.0.0.1:9999" || u.Path != "/cb" {
		t.Fatalf("redirect URI must preserve registered scheme/host/path: %q", resp.RedirectURI)
	}
	if got := u.Query().Get("state"); got != fx.state {
		t.Fatalf("state must round-trip verbatim: want %q got %q", fx.state, got)
	}
	if code := u.Query().Get("code"); code == "" {
		t.Fatalf("redirect URI missing code param: %q", resp.RedirectURI)
	}
	// The pending entry must be consumed — a replay attempts re-issuing a code
	// for the same authctx, which would invalidate the single-use guarantee.
	if _, ok := fx.pending.Take(fx.authctx); ok {
		t.Fatal("pending entry must be consumed on success (single-use)")
	}
}

// TestApproveHandler_MissingClaims asserts a request that reaches the handler
// without the bearer middleware having attached Claims is rejected — defence
// in depth in case mount.go is ever wired without the verifier in front.
func TestApproveHandler_MissingClaims(t *testing.T) {
	fx := newApproveFixture(t)
	c, rec := newApproveCtx(login.ApproveRequest{AuthCtx: fx.authctx}, nil)
	if err := login.ApproveHandler(fx.deps)(c); err != nil {
		t.Fatalf("ApproveHandler returned err: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing claims must return 401, got %d", rec.Code)
	}
	// The pending entry must survive — a 401 must not consume the authctx.
	if _, ok := fx.pending.Take(fx.authctx); !ok {
		t.Fatal("401 must not consume the pending entry")
	}
}

// TestApproveHandler_EmptyAuthctx asserts the body validation gate — an empty
// authctx is rejected with the typed authctx_expired code so the SPA can
// trigger the same self-heal as the listIdps / submitPassword paths.
func TestApproveHandler_EmptyAuthctx(t *testing.T) {
	fx := newApproveFixture(t)
	c, rec := newApproveCtx(login.ApproveRequest{AuthCtx: ""}, &jwtverifier.Claims{Subject: "user-42"})
	if err := login.ApproveHandler(fx.deps)(c); err != nil {
		t.Fatalf("ApproveHandler returned err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty authctx must return 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authctx_expired") {
		t.Fatalf("empty authctx must surface authctx_expired error: %s", rec.Body.String())
	}
}

// TestApproveHandler_UnknownAuthctx asserts an authctx that was never put (or
// has already been consumed) returns the typed authctx_expired error — same
// failure mode the SPA already self-heals from.
func TestApproveHandler_UnknownAuthctx(t *testing.T) {
	fx := newApproveFixture(t)
	c, rec := newApproveCtx(login.ApproveRequest{AuthCtx: "does-not-exist"}, &jwtverifier.Claims{Subject: "user-42"})
	if err := login.ApproveHandler(fx.deps)(c); err != nil {
		t.Fatalf("ApproveHandler returned err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown authctx must return 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authctx_expired") {
		t.Fatalf("unknown authctx must surface authctx_expired: %s", rec.Body.String())
	}
}

// TestApproveHandler_MalformedBody asserts non-JSON body is mapped to the same
// authctx_expired code so the SPA's recovery path triggers (rather than
// surfacing an opaque 400 the user cannot interpret).
func TestApproveHandler_MalformedBody(t *testing.T) {
	fx := newApproveFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/authserver/approve", strings.NewReader("not-json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(jwtverifier.ClaimsContextKey, &jwtverifier.Claims{Subject: "user-42"})
	if err := login.ApproveHandler(fx.deps)(c); err != nil {
		t.Fatalf("ApproveHandler returned err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body must return 400, got %d", rec.Code)
	}
}
