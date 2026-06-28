//go:build integration

// Package integration hosts cross-package integration tests for the control
// plane. They require a live PostgreSQL reachable via DATABASE_URL and skip
// automatically when the env var is unset, matching the pattern used by
// packages/shared/transport/mq.
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// Seed credentials baked into tools/db-migrate/seed/seed.ts. The super admin
// user belongs to the super-admins group which is bound to NexusSuperAdmin
// (Action: [*]) so every IAM-gated admin route grants to this principal.
const (
	seedAdminEmail    = "admin@nexus.ai"
	seedAdminPassword = "admin123"
	seedAdminUserID   = "nexus-user-super-admin"

	// cpUIClientID is the built-in public OAuth client registered by
	// auth-seed.ts. The redirect_uri used in the flow is resolved from the
	// DB row at runtime so the test stays robust against seed drift between
	// environments; /oauth/authorize only validates that the submitted URI
	// is one of the registered values, so we pick the first one.
	cpUIClientID = "cp-ui"
)

// crossAuthEnv bundles the cross-service auth harness the test needs at
// call time: the HTTP base URL to drive requests against, the signer used
// to mint custom tokens for rejection tests (re-using the server's active
// key so signatures chain to the published JWKS), and the redirect URI
// resolved from the cp-ui seed row. Server + pool teardown is registered
// via t.Cleanup at construction, so no handles leak into this struct.
type crossAuthEnv struct {
	baseURL     string
	signer      *token.Signer
	redirectURI string
}

// newCrossAuthEnv stands up a CP instance in-process backed by the real
// DATABASE_URL seed. Every dependency that Phase-2 admin auth touches is
// wired: auth server endpoints (/oauth/*, /login, /.well-known/*), admin
// API group behind the JWT+API-key middleware, JWT verifier pointing at the
// in-process JWKS, and the admin route group. Skips when DATABASE_URL is
// unset so the suite stays green without a Postgres in the sandbox.
func newCrossAuthEnv(t *testing.T) *crossAuthEnv {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping cross-service auth integration test")
	}

	ctx := context.Background()
	db, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(db.Close)

	// Sanity-check the seed: if the super-admin user is missing we cannot
	// authenticate against the local IdP, so the entire flow is untestable.
	if _, err := db.FindNexusUserByID(ctx, seedAdminUserID); err != nil {
		t.Skipf("seed user %q not present (run `npx prisma db seed`): %v", seedAdminUserID, err)
	}

	// Load the first registered redirect URI for cp-ui so the flow works
	// against any seed variant — the handler matches submitted URI against
	// the full registered list, so any element is acceptable.
	var redirectURIs []string
	if err := db.Pool.QueryRow(ctx,
		`SELECT "redirectUris" FROM "OAuthClient" WHERE id=$1`, cpUIClientID,
	).Scan(&redirectURIs); err != nil {
		t.Skipf("cp-ui OAuthClient row not present (run `npx prisma db seed`): %v", err)
	}
	if len(redirectURIs) == 0 {
		t.Skipf("cp-ui OAuthClient has no registered redirect URIs")
	}
	redirectURI := redirectURIs[0]

	// Keystore lives in a per-test temp dir so the keys do not leak between
	// runs; a single RS256 key is enough to drive both minting paths.
	keystoreDir := t.TempDir()
	ks, err := token.OpenKeystore(keystoreDir)
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	signer := token.NewSigner(ks)

	// Silent logger keeps test output clean; flip to slog.Default() when
	// debugging.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Build the Echo app and the httptest server. The server must be started
	// BEFORE we mount the auth server so we can feed httptestServer.URL back
	// in as the issuer — the verifier and the minter must agree byte-for-byte
	// on the iss claim or every Verify call returns ErrWrongIssuer.
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	server := httptest.NewServer(e)
	t.Cleanup(server.Close)
	baseURL := server.URL
	issuer := baseURL

	adminJWTVerifier := jwtverifier.New(jwtverifier.Config{
		Issuer:   issuer,
		JWKSURL:  issuer + "/.well-known/jwks.json",
		Audience: token.AdminAudience,
		Logger:   logger,
	})

	iamEngine := iam.NewEngine(db, logger)

	adminGroup := e.Group("/api/admin")
	adminGroup.Use(middleware.AdminAuth(middleware.AdminAuthConfig{
		JWTVerifier:  adminJWTVerifier,
		APIKeyLookup: db,
		DB:           db,
		Logger:       logger,
	}))

	adminHandler := &handler.AdminHandler{
		DB:     db,
		IAM:    iamEngine,
		Audit:  audit.NewWriter(nil, "nexus.event.admin-audit", logger),
		Logger: logger,
	}
	adminHandler.RegisterAdminRoutes(adminGroup)

	authserver.Mount(e, authserver.Deps{
		DB:          db.Pool,
		Keystore:    ks,
		Signer:      signer,
		Logger:      logger,
		Issuer:      issuer,
		AgentLookup: db,
	})

	return &crossAuthEnv{
		baseURL:     baseURL,
		signer:      signer,
		redirectURI: redirectURI,
	}
}

// newPKCEPair generates an RFC 7636 verifier/challenge pair using the
// S256 transformation. 33 bytes encodes to the 44-char verifier length the
// cp-ui SPA uses; anything in [43, 128] is RFC-valid.
func newPKCEPair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	buf := make([]byte, 33)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("read random: %v", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

// newAuthClient returns a client that never auto-follows redirects. The SPA
// owns /login now, so the /oauth/authorize → /login redirect is terminal for
// this harness — we parse authctx out of the Location header directly rather
// than let the client chase a 404 at the backend's /login path.
func newAuthClient(t *testing.T, _ string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// runPKCELogin drives the full authorization-code + PKCE flow end-to-end
// against the in-process auth server and returns the minted tokens. On any
// step failure it fails the test via t.Fatalf so callers never have to
// branch on a partial result.
func runPKCELogin(t *testing.T, env *crossAuthEnv) (access, refresh string) {
	t.Helper()
	client := newAuthClient(t, env.baseURL)
	verifier, challenge := newPKCEPair(t)
	state := "state-" + time.Now().Format("150405.000000000")

	// 1. GET /oauth/authorize → 302 /login?authctx=...
	authorizeURL := env.baseURL + "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {cpUIClientID},
		"redirect_uri":          {env.redirectURI},
		"scope":                 {"admin openid profile email"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()
	resp, err := client.Get(authorizeURL)
	if err != nil {
		t.Fatalf("GET /oauth/authorize: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: want 302, got %d", resp.StatusCode)
	}
	loginLoc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse /login redirect: %v", err)
	}
	if loginLoc.Path != "/login" {
		t.Fatalf("authorize redirect path: got %q, want /login", loginLoc.Path)
	}
	authctx := loginLoc.Query().Get("authctx")
	if authctx == "" {
		t.Fatalf("authorize redirect missing authctx: %q", loginLoc.String())
	}

	// 2. POST /authserver/password → 200 JSON {redirectUri: ...}
	loginReqBody, _ := json.Marshal(map[string]string{
		"authctx":  authctx,
		"email":    seedAdminEmail,
		"password": seedAdminPassword,
	})
	loginReq, err := http.NewRequest(http.MethodPost, env.baseURL+"/authserver/password",
		bytes.NewReader(loginReqBody))
	if err != nil {
		t.Fatalf("build /authserver/password request: %v", err)
	}
	loginReq.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(loginReq)
	if err != nil {
		t.Fatalf("POST /authserver/password: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/authserver/password: want 200, got %d body %q", resp.StatusCode, string(respBody))
	}
	var loginResp struct {
		RedirectURI string `json:"redirectUri"`
	}
	if err := json.Unmarshal(respBody, &loginResp); err != nil {
		t.Fatalf("decode /authserver/password response: %v", err)
	}
	redirectURL, err := url.Parse(loginResp.RedirectURI)
	if err != nil {
		t.Fatalf("parse redirectUri %q: %v", loginResp.RedirectURI, err)
	}
	if gotState := redirectURL.Query().Get("state"); gotState != state {
		t.Fatalf("redirectUri state: got %q, want %q", gotState, state)
	}
	code := redirectURL.Query().Get("code")
	if code == "" {
		t.Fatalf("redirectUri missing code: %q", loginResp.RedirectURI)
	}

	// 3. POST /oauth/token → 200 {access_token, refresh_token, expires_in}
	tokenResp := postTokenForm(t, env.baseURL, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"client_id":     {cpUIClientID},
		"redirect_uri":  {env.redirectURI},
	})
	if tokenResp.AccessToken == "" || tokenResp.RefreshToken == "" || tokenResp.ExpiresIn <= 0 {
		t.Fatalf("token response missing fields: %+v", tokenResp)
	}
	return tokenResp.AccessToken, tokenResp.RefreshToken
}

// tokenResponseBody mirrors the RFC 6749 §5.1 success body emitted by
// /oauth/token. Kept local to the test so changes to the server struct
// surface as visible diffs here rather than compile errors in the harness.
type tokenResponseBody struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope,omitempty"`

	// Error body fields — populated only on failure.
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// postTokenForm posts to /oauth/token and decodes the success response,
// failing the test on any non-200 status. Use postTokenFormRaw when the
// test expects a deliberate error.
func postTokenForm(t *testing.T, baseURL string, form url.Values) tokenResponseBody {
	t.Helper()
	body, status := postTokenFormRaw(t, baseURL, form)
	if status != http.StatusOK {
		t.Fatalf("POST /oauth/token: status %d body %q", status, string(body))
	}
	var resp tokenResponseBody
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode token response: %v body %q", err, string(body))
	}
	return resp
}

// postTokenFormRaw returns the raw bytes + status so tests inspecting error
// bodies (replayed refresh, invalid_grant) can assert on payload contents.
func postTokenFormRaw(t *testing.T, baseURL string, form url.Values) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/oauth/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /oauth/token: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read token body: %v", err)
	}
	return body, resp.StatusCode
}

// getWithBearer issues an Authorization: Bearer request against the admin
// API and returns the status + body. Callers assert on both — a 401 under
// test conditions typically requires inspecting the body to distinguish
// wrong audience from expired from tampered signatures.
func getWithBearer(t *testing.T, baseURL, path, bearer string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", path, err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, body
}

// mintCustomAccess issues an RS256-signed access token using the same signer
// the auth server uses so the signature chains to the published JWKS. This
// lets rejection tests isolate the failure mode under test (wrong aud,
// expired) without also tripping ErrInvalidSignature.
func mintCustomAccess(t *testing.T, env *crossAuthEnv, audience []string, ttl time.Duration) string {
	t.Helper()
	tok, _, err := token.IssueAccess(env.signer, token.AccessInput{
		Issuer:   env.baseURL,
		Subject:  seedAdminUserID,
		Audience: audience,
		ClientID: cpUIClientID,
		Scope:    "admin",
		Email:    seedAdminEmail,
		TTL:      ttl,
	})
	if err != nil {
		t.Fatalf("mint custom access token: %v", err)
	}
	return tok
}

// TestCrossServiceJWTAuth exercises the full Phase-2 admin auth surface end
// to end. The subtests share a single CP instance because every scenario
// downstream of the initial login only needs read-only DB access, and
// standing up the server is the expensive part. Each subtest still runs
// through its own token grant so refresh-rotation replay assertions cannot
// interfere with the Bearer-call assertions.
func TestCrossServiceJWTAuth(t *testing.T) {
	env := newCrossAuthEnv(t)

	t.Run("PKCELoginIssuesBearerAndRefresh", func(t *testing.T) {
		access, refresh := runPKCELogin(t, env)
		if access == "" {
			t.Fatal("empty access token")
		}
		if refresh == "" {
			t.Fatal("empty refresh token")
		}
	})

	t.Run("BearerGrantsAccessToAdminAPI", func(t *testing.T) {
		access, _ := runPKCELogin(t, env)

		// /api/admin/me returns the authenticated principal; roles must be
		// non-empty because the super-admins group membership is seeded.
		status, body := getWithBearer(t, env.baseURL, "/api/admin/me", access)
		if status != http.StatusOK {
			t.Fatalf("/me: want 200, got %d body %q", status, string(body))
		}
		var me struct {
			KeyID string   `json:"keyId"`
			Roles []string `json:"roles"`
			Email string   `json:"email"`
		}
		if err := json.Unmarshal(body, &me); err != nil {
			t.Fatalf("decode /me: %v body %q", err, string(body))
		}
		if me.KeyID != seedAdminUserID {
			t.Fatalf("/me keyId: got %q want %q", me.KeyID, seedAdminUserID)
		}
		if len(me.Roles) == 0 {
			t.Fatalf("/me roles: expected non-empty, got %v", me.Roles)
		}
		if me.Email != seedAdminEmail {
			t.Fatalf("/me email: got %q want %q", me.Email, seedAdminEmail)
		}

		// /api/admin/users is gated by admin:ReadUser. super-admins hits it
		// via the NexusSuperAdmin [*] allow statement.
		status, body = getWithBearer(t, env.baseURL, "/api/admin/users", access)
		if status != http.StatusOK {
			t.Fatalf("/users: want 200, got %d body %q", status, string(body))
		}
	})

	t.Run("RefreshRotatesAndRejectsReplay", func(t *testing.T) {
		_, refresh := runPKCELogin(t, env)

		// Rotate once — must succeed and return a brand-new refresh token.
		rotated := postTokenForm(t, env.baseURL, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {refresh},
		})
		if rotated.AccessToken == "" {
			t.Fatal("rotation: missing access_token")
		}
		if rotated.RefreshToken == "" {
			t.Fatal("rotation: missing refresh_token")
		}
		if rotated.RefreshToken == refresh {
			t.Fatal("rotation returned the same refresh token")
		}

		// Replay the original refresh token — RFC 6749 §5.2 mandates
		// invalid_grant on a consumed refresh token.
		body, status := postTokenFormRaw(t, env.baseURL, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {refresh},
		})
		if status != http.StatusBadRequest {
			t.Fatalf("replay: want 400, got %d body %q", status, string(body))
		}
		var errResp tokenResponseBody
		if err := json.Unmarshal(body, &errResp); err != nil {
			t.Fatalf("decode replay error: %v body %q", err, string(body))
		}
		if errResp.Error != "invalid_grant" {
			t.Fatalf("replay: error=%q, want invalid_grant (body=%q)", errResp.Error, string(body))
		}
	})

	t.Run("TamperedSignatureRejected", func(t *testing.T) {
		access, _ := runPKCELogin(t, env)
		tampered := flipLastSignatureByte(t, access)
		status, _ := getWithBearer(t, env.baseURL, "/api/admin/me", tampered)
		if status != http.StatusUnauthorized {
			t.Fatalf("tampered token: want 401, got %d", status)
		}
	})

	t.Run("WrongAudienceRejected", func(t *testing.T) {
		bad := mintCustomAccess(t, env, []string{"wrong-audience"}, time.Hour)
		status, _ := getWithBearer(t, env.baseURL, "/api/admin/me", bad)
		if status != http.StatusUnauthorized {
			t.Fatalf("wrong-aud token: want 401, got %d", status)
		}
	})

	t.Run("ExpiredTokenRejected", func(t *testing.T) {
		// -10 minutes comfortably outruns the verifier's 5-minute clock skew
		// leeway so the token is unambiguously expired.
		expired := mintCustomAccess(t, env, []string{token.AdminAudience}, -10*time.Minute)
		status, _ := getWithBearer(t, env.baseURL, "/api/admin/me", expired)
		if status != http.StatusUnauthorized {
			t.Fatalf("expired token: want 401, got %d", status)
		}
	})
}

// flipLastSignatureByte returns the JWT with the final base64url signature
// byte incremented by one. The resulting signature parses as valid
// base64url but no longer verifies, so the middleware surfaces 401 with
// INVALID_TOKEN rather than a parse error.
func flipLastSignatureByte(t *testing.T, jwt string) string {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d segments, want 3", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(sig) == 0 {
		t.Fatal("empty signature")
	}
	sig[len(sig)-1] ^= 0x01
	return fmt.Sprintf("%s.%s.%s",
		parts[0], parts[1], base64.RawURLEncoding.EncodeToString(sig))
}
