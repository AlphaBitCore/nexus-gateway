package login_test

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/idp"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/login"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
)

// passwordFixture wires up a freshly-seeded local IdP + user + OAuthClient
// and returns the PasswordDeps plus the pending authctx handle. Callers get
// teardown through t.Cleanup.
type passwordFixture struct {
	deps        login.PasswordDeps
	authctx     string
	redirectURI string
	state       string
	userID      string
}

func seedPasswordFixture(t *testing.T) passwordFixture {
	t.Helper()
	pool := storetest.Open(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")

	var idpID string
	err := pool.QueryRow(ctx,
		`INSERT INTO "IdentityProvider"(type,name,enabled,config,"defaultRole","jitEnabled","updatedAt")
		 VALUES ('local','test-local-pw-'||$1,TRUE,'{}'::jsonb,'developer',TRUE,NOW())
		 RETURNING id`, suffix,
	).Scan(&idpID)
	if err != nil {
		t.Fatalf("seed idp: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "IdentityProvider" WHERE id=$1`, idpID) })

	userID := "test-user-pw-" + suffix
	email := userID + "@test.local"
	hash, err := auth.HashPassword("hunter2")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO "NexusUser"(id,"displayName",email,status,"canAccessControlPlane","passwordHash","updatedAt")
		 VALUES ($1,$2,$3,'active',TRUE,$4,NOW())`,
		userID, "Password User", email, hash)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "NexusUser" WHERE id=$1`, userID) })

	clientID := "test-pw-client-" + suffix
	redirectURI := "http://127.0.0.1:54321/callback"
	_, err = pool.Exec(ctx,
		`INSERT INTO "OAuthClient"(id,name,type,"redirectUris","allowedScopes","updatedAt")
		 VALUES ($1,$2,'public',$3,$4,NOW())`,
		clientID, "Password Test Client",
		[]string{redirectURI}, []string{"openid"})
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "OAuthClient" WHERE id=$1`, clientID) })

	users := store.NewUserStore(pool)
	local := idp.NewLocal(users, idpID)
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	authCodes := store.NewAuthCodeStore(5 * time.Minute)
	t.Cleanup(authCodes.Close)

	authctx := store.RandomOpaqueToken(16)
	state := "st-" + suffix
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		Scope:         "openid",
		State:         state,
		Nonce:         "n-" + suffix,
		CodeChallenge: "cc-" + suffix,
		DeviceID:      "",
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	})

	return passwordFixture{
		deps: login.PasswordDeps{
			Local:     local,
			Pending:   pending,
			AuthCodes: authCodes,
			Limiter:   login.NewLimiter(),
		},
		authctx:     authctx,
		redirectURI: redirectURI,
		state:       state,
		userID:      userID,
	}
}

// postJSON builds an echo.Context around a JSON body. Tests use this instead
// of hand-rolling httptest.NewRequest each time.
func postJSON(body any) (echo.Context, *httptest.ResponseRecorder) {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/authserver/password", strings.NewReader(string(raw)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

// TestPasswordHandler_Success posts valid credentials and verifies the
// handler returns 200 with a redirectUri that carries the issued code and
// state appended to the registered redirect URI.
func TestPasswordHandler_Success(t *testing.T) {
	fx := seedPasswordFixture(t)
	email := fx.userID + "@test.local"

	c, rec := postJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    email,
		Password: "hunter2",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	var resp login.PasswordSubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v (%q)", err, rec.Body.String())
	}
	if resp.RedirectURI == "" {
		t.Fatal("missing redirectUri")
	}
	u, err := url.Parse(resp.RedirectURI)
	if err != nil {
		t.Fatalf("parse redirectUri: %v", err)
	}
	if u.Scheme != "http" || u.Host != "127.0.0.1:54321" || u.Path != "/callback" {
		t.Fatalf("redirect target mutated: got %q, want %q", resp.RedirectURI, fx.redirectURI)
	}
	if got := u.Query().Get("state"); got != fx.state {
		t.Fatalf("state: got %q, want %q", got, fx.state)
	}
	code := u.Query().Get("code")
	if code == "" {
		t.Fatal("redirect missing code")
	}

	entry, ok := fx.deps.AuthCodes.Get(code)
	if !ok {
		t.Fatal("authorization code not stored")
	}
	if entry.UserID != fx.userID {
		t.Fatalf("authcode UserID: got %q, want %q", entry.UserID, fx.userID)
	}
	if entry.Email != email {
		t.Fatalf("authcode Email: got %q, want %q", entry.Email, email)
	}
	if len(entry.AMR) != 1 || entry.AMR[0] != "pwd" {
		t.Fatalf("authcode AMR: got %v, want [pwd]", entry.AMR)
	}
	if !store.RedirectAllowed(store.OAuthClient{RedirectURIs: []string{fx.redirectURI}}, fx.redirectURI) {
		t.Fatal("registered redirect URI no longer matches itself")
	}
	if _, ok := fx.deps.Pending.Take(fx.authctx); ok {
		t.Fatal("pending entry should have been consumed by successful login")
	}
}

// TestPasswordHandler_InvalidCredentials posts a wrong password and checks
// the handler returns 401 with error=invalid_credentials. The pending entry
// must not be consumed so the user can retry.
func TestPasswordHandler_InvalidCredentials(t *testing.T) {
	fx := seedPasswordFixture(t)
	email := fx.userID + "@test.local"

	c, rec := postJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    email,
		Password: "definitely-wrong",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "invalid_credentials" {
		t.Fatalf("error: got %q, want invalid_credentials", body["error"])
	}
	if _, ok := fx.deps.Pending.Take(fx.authctx); !ok {
		t.Fatal("pending entry should survive a failed login")
	}
}

// oneShotLimiter allows exactly one call; every subsequent Allow returns false.
type oneShotLimiter struct{ used bool }

func (l *oneShotLimiter) Allow(_, _ string) bool {
	if l.used {
		return false
	}
	l.used = true
	return true
}

// TestPasswordHandler_RateLimited exhausts the limiter budget and verifies
// the next attempt is rejected with 429 error=rate_limited.
func TestPasswordHandler_RateLimited(t *testing.T) {
	fx := seedPasswordFixture(t)
	email := fx.userID + "@test.local"
	fx.deps.Limiter = &oneShotLimiter{}

	c, rec := postJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    email,
		Password: "hunter2",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("first handler call: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("first call: want 200, got %d (%q)", rec.Code, rec.Body.String())
	}

	fx.deps.Pending.Put(fx.authctx, store.PendingAuthzEntry{
		ClientID:      "x",
		RedirectURI:   fx.redirectURI,
		CodeChallenge: "cc",
		State:         fx.state,
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	c2, rec2 := postJSON(login.PasswordSubmitRequest{
		AuthCtx:  fx.authctx,
		Email:    email,
		Password: "hunter2",
	})
	if err := login.PasswordHandler(fx.deps)(c2); err != nil {
		t.Fatalf("second handler call: %v", err)
	}
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second call: want 429, got %d", rec2.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "rate_limited" {
		t.Fatalf("error: got %q, want rate_limited", body["error"])
	}
}

// TestPasswordHandler_ExpiredAuthctx posts valid credentials but with an
// unknown authctx. Expect 400 error=authctx_expired so the SPA restarts the
// authorize flow.
func TestPasswordHandler_ExpiredAuthctx(t *testing.T) {
	fx := seedPasswordFixture(t)
	email := fx.userID + "@test.local"

	c, rec := postJSON(login.PasswordSubmitRequest{
		AuthCtx:  "nope-" + fx.authctx,
		Email:    email,
		Password: "hunter2",
	})
	if err := login.PasswordHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "authctx_expired" {
		t.Fatalf("error: got %q, want authctx_expired", body["error"])
	}
}
