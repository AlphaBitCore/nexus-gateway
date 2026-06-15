package authserver_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// TestE2E_LocalIdPFlow seeds a local IdP + NexusUser + OAuthClient, mounts
// the full auth server, and drives the complete authorization-code + PKCE
// flow end-to-end:
//
//  1. GET /oauth/authorize → 302 /login?authctx=...
//  2. POST /login/password → 302 <redirect>?code=...&state=...
//  3. POST /oauth/token → 200 {access_token, refresh_token, ...}
//  4. GET /.well-known/jwks.json → verify access-token RS256 signature
//
// Skips automatically when DATABASE_URL is unset so the suite stays green
// without a live Postgres.
func TestE2E_LocalIdPFlow(t *testing.T) {
	pool := storetest.Open(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")

	// --- Seed rows ----------------------------------------------------------
	// 1. IdentityProvider (local, enabled).
	// id has no DB-side default — Prisma supplies UUIDs at application layer,
	// so the test must do the same. gen_random_uuid() comes from pgcrypto.
	var idpID string
	idpName := "e2e-local-" + suffix
	if err := pool.QueryRow(ctx,
		`INSERT INTO "IdentityProvider"(id,type,name,enabled,config,"defaultRole","jitEnabled","updatedAt")
		 VALUES (gen_random_uuid(),'local',$1,TRUE,'{}'::jsonb,'developer',TRUE,NOW())
		 RETURNING id`, idpName,
	).Scan(&idpID); err != nil {
		t.Fatalf("seed idp: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "IdentityProvider" WHERE id=$1`, idpID) })

	// 2. NexusUser with a real scrypt hash of "secret123".
	userID := "e2e-user-" + suffix
	email := "e2e-" + suffix + "@example.com"
	pwdHash, err := auth.HashPassword("secret123")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO "NexusUser"(id,"displayName",email,status,"canAccessControlPlane","passwordHash","updatedAt")
		 VALUES ($1,$2,$3,'active',TRUE,$4,NOW())`,
		userID, "E2E User", email, pwdHash,
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		// RefreshToken has FK on NexusUser; clean up any rows issued during the test.
		_, _ = pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE "userId"=$1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM "NexusUser" WHERE id=$1`, userID)
	})

	// 3. UserFederatedIdentity linking user → IdP (so the Local adapter path
	//    matches a federated subject, even though password auth does not
	//    require it today — it keeps the test realistic for federated logins).
	if _, err := pool.Exec(ctx,
		`INSERT INTO "UserFederatedIdentity"("userId","idpId","externalSubject")
		 VALUES ($1,$2,$3)`,
		userID, idpID, email,
	); err != nil {
		t.Fatalf("seed federated identity: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM "UserFederatedIdentity" WHERE "userId"=$1 AND "idpId"=$2`, userID, idpID)
	})

	// 4. OAuthClient — public, PKCE-required.
	clientID := "e2e-test-client-" + suffix
	redirectURI := "http://127.0.0.1:1/callback"
	if _, err := pool.Exec(ctx,
		`INSERT INTO "OAuthClient"(id,name,type,"redirectUris","allowedScopes","accessTtlSeconds","refreshTtlSeconds","updatedAt")
		 VALUES ($1,$2,'public',$3,$4,3600,86400,NOW())`,
		clientID, "E2E Test Client", []string{redirectURI}, []string{"openid"},
	); err != nil {
		t.Fatalf("seed oauth client: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE "clientId"=$1`, clientID)
		_, _ = pool.Exec(ctx, `DELETE FROM "OAuthClient" WHERE id=$1`, clientID)
	})

	// --- Build auth server --------------------------------------------------
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	signer := token.NewSigner(ks)

	e := echo.New()
	authserver.Mount(e, authserver.Deps{
		DB:       pool,
		Keystore: ks,
		Signer:   signer,
		Issuer:   "https://test.local",
		Logger:   slog.Default(),
	})

	// --- PKCE material ------------------------------------------------------
	// 33 random bytes → 44-char base64url verifier (RFC 7636 requires 43..128).
	verifierBytes := make([]byte, 33)
	if _, err := rand.Read(verifierBytes); err != nil {
		t.Fatalf("gen verifier: %v", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	challengeSum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeSum[:])

	state := "xyz-" + suffix

	// --- 1. GET /oauth/authorize → 302 /login?authctx=... -------------------
	authorizeQuery := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	reqAuth := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+authorizeQuery.Encode(), nil)
	recAuth := httptest.NewRecorder()
	e.ServeHTTP(recAuth, reqAuth)
	if recAuth.Code != http.StatusFound {
		t.Fatalf("/oauth/authorize: want 302, got %d (body=%q)", recAuth.Code, recAuth.Body.String())
	}
	locAuth := recAuth.Header().Get("Location")
	uAuth, err := url.Parse(locAuth)
	if err != nil {
		t.Fatalf("parse authorize redirect: %v", err)
	}
	if uAuth.Path != "/login" {
		t.Fatalf("authorize redirect path: got %q, want /login", uAuth.Path)
	}
	authctx := uAuth.Query().Get("authctx")
	if authctx == "" {
		t.Fatalf("authorize redirect missing authctx: %q", locAuth)
	}

	// --- 2. POST /authserver/password → 200 JSON {redirectUri: ...} ---------
	loginBody, _ := json.Marshal(map[string]string{
		"authctx":  authctx,
		"email":    email,
		"password": "secret123",
	})
	reqLogin := httptest.NewRequest(http.MethodPost, "/authserver/password",
		strings.NewReader(string(loginBody)))
	reqLogin.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	recLogin := httptest.NewRecorder()
	e.ServeHTTP(recLogin, reqLogin)
	if recLogin.Code != http.StatusOK {
		t.Fatalf("/authserver/password: want 200, got %d (body=%q)", recLogin.Code, recLogin.Body.String())
	}
	var loginResp struct {
		RedirectURI string `json:"redirectUri"`
	}
	if err := json.Unmarshal(recLogin.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	uLogin, err := url.Parse(loginResp.RedirectURI)
	if err != nil {
		t.Fatalf("parse redirectUri: %v", err)
	}
	if uLogin.Scheme != "http" || uLogin.Host != "127.0.0.1:1" || uLogin.Path != "/callback" {
		t.Fatalf("redirectUri mutated: got %q, want prefix %q", loginResp.RedirectURI, redirectURI)
	}
	if gotState := uLogin.Query().Get("state"); gotState != state {
		t.Fatalf("redirectUri state: got %q, want %q", gotState, state)
	}
	code := uLogin.Query().Get("code")
	if code == "" {
		t.Fatalf("redirectUri missing code: %q", loginResp.RedirectURI)
	}

	// --- 3. POST /oauth/token → 200 JSON ------------------------------------
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
	}
	reqTok := httptest.NewRequest(http.MethodPost, "/oauth/token",
		strings.NewReader(tokenForm.Encode()))
	reqTok.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	recTok := httptest.NewRecorder()
	e.ServeHTTP(recTok, reqTok)
	if recTok.Code != http.StatusOK {
		t.Fatalf("/oauth/token: want 200, got %d (body=%q)", recTok.Code, recTok.Body.String())
	}
	var tokenBody struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(recTok.Body.Bytes(), &tokenBody); err != nil {
		t.Fatalf("decode token response: %v (body=%q)", err, recTok.Body.String())
	}
	if tokenBody.AccessToken == "" {
		t.Fatal("token response missing access_token")
	}
	if tokenBody.TokenType != "Bearer" {
		t.Fatalf("token_type: got %q, want Bearer", tokenBody.TokenType)
	}
	if tokenBody.ExpiresIn != 3600 {
		t.Fatalf("expires_in: got %d, want 3600", tokenBody.ExpiresIn)
	}
	if tokenBody.RefreshToken == "" {
		t.Fatal("token response missing refresh_token")
	}

	// --- 4. Verify JWT via JWKS ---------------------------------------------
	// JWKS is published on the same echo instance. We verify by parsing the
	// access token through the keystore the server used to sign it; this
	// exercises the full sign/publish/verify loop even though we reach into
	// the in-process Keystore for the public key rather than decoding JWKS
	// bytes (the JWKS route is covered directly by oauth/jwks_test.go).
	reqJwks := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	recJwks := httptest.NewRecorder()
	e.ServeHTTP(recJwks, reqJwks)
	if recJwks.Code != http.StatusOK {
		t.Fatalf("/.well-known/jwks.json: want 200, got %d", recJwks.Code)
	}
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Alg string `json:"alg"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(recJwks.Body.Bytes(), &jwks); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	if len(jwks.Keys) != 1 || jwks.Keys[0].Alg != "RS256" {
		t.Fatalf("unexpected jwks: %+v", jwks)
	}

	var claims token.AccessClaims
	parsed, err := jwt.ParseWithClaims(tokenBody.AccessToken, &claims, func(jt *jwt.Token) (any, error) {
		if _, ok := jt.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, jwt.ErrTokenSignatureInvalid
		}
		kid, _ := jt.Header["kid"].(string)
		if kid == "" {
			return nil, jwt.ErrTokenUnverifiable
		}
		k, ok := ks.ByKID(kid)
		if !ok {
			return nil, jwt.ErrTokenUnverifiable
		}
		return &k.Priv.PublicKey, nil
	})
	if err != nil || parsed == nil || !parsed.Valid {
		t.Fatalf("parse access token: %v", err)
	}
	if claims.Issuer != "https://test.local" {
		t.Fatalf("iss: got %q, want https://test.local", claims.Issuer)
	}
	if claims.Subject != userID {
		t.Fatalf("sub: got %q, want %q", claims.Subject, userID)
	}
	// Every admin access token carries the fixed aud=cp-admin, not the caller's
	// client id. client_id stays a first-class claim so introspection and audit
	// can still identify the originating client.
	const wantAud = "cp-admin"
	audOK := false
	for _, a := range claims.Audience {
		if a == wantAud {
			audOK = true
			break
		}
	}
	if !audOK {
		t.Fatalf("aud missing admin audience: got %v, want contains %q", claims.Audience, wantAud)
	}
	if claims.ClientID != clientID {
		t.Fatalf("client_id claim: got %q, want %q", claims.ClientID, clientID)
	}

	// Access-token kid must match the keystore's active kid; this catches any
	// drift between the signer's key selection and the key the verifier trusts.
	activeKID := ks.ActiveKID()
	if got, _ := parsed.Header["kid"].(string); got != activeKID {
		t.Fatalf("access-token kid: got %q, want %q", got, activeKID)
	}
}
