package login_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/login"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/oidcdisco"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// RSA / JWKS / token-endpoint helpers — mirror packages/control-plane/internal/
// middleware/jwt_test.go so OIDC handler tests can drive ValidateJWT through a
// real signed token without spinning up a live IdP.

func bigIntB64(n *big.Int) string { return base64.RawURLEncoding.EncodeToString(n.Bytes()) }
func intB64(e int) string         { return base64.RawURLEncoding.EncodeToString(big.NewInt(int64(e)).Bytes()) }
func signJWT(t *testing.T, header, payload map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	hJSON, _ := json.Marshal(header)
	pJSON, _ := json.Marshal(payload)
	hB64 := base64.RawURLEncoding.EncodeToString(hJSON)
	pB64 := base64.RawURLEncoding.EncodeToString(pJSON)
	sigInput := hB64 + "." + pB64
	h := sha256.Sum256([]byte(sigInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return hB64 + "." + pB64 + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// oidcServer fakes the IdP: serves a JWKS endpoint and a token endpoint
// that returns the pre-signed id_token. Set tokenStatus to drive the
// non-200 branch; set tokenBody to replace the JSON body entirely.
type oidcServer struct {
	priv        *rsa.PrivateKey
	kid         string
	idToken     atomic.Value // string
	tokenStatus atomic.Int32 // 200 if unset
	tokenBody   atomic.Value // string (overrides default)
	srv         *httptest.Server
}

func newOIDCServer(t *testing.T, kid string) *oidcServer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa gen: %v", err)
	}
	o := &oidcServer{priv: priv, kid: kid}

	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": kid,
				"use": "sig",
				"alg": "RS256",
				"n":   bigIntB64(priv.N),
				"e":   intB64(priv.E),
			},
		},
	}
	jwksBody, _ := json.Marshal(jwks)

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 o.srv.URL,
			"authorization_endpoint": o.srv.URL + "/authorize",
			"token_endpoint":         o.srv.URL + "/token",
			"jwks_uri":               o.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksBody)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		status := int(o.tokenStatus.Load())
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body, ok := o.tokenBody.Load().(string); ok && body != "" {
			_, _ = w.Write([]byte(body))
			return
		}
		tok, _ := o.idToken.Load().(string)
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": tok})
	})
	o.srv = httptest.NewServer(mux)
	t.Cleanup(o.srv.Close)
	return o
}

func (o *oidcServer) URL() string         { return o.srv.URL }
func (o *oidcServer) JWKSURL() string     { return o.srv.URL + "/jwks" }
func (o *oidcServer) TokenURL() string    { return o.srv.URL + "/token" }
func (o *oidcServer) SetIDToken(t string) { o.idToken.Store(t) }
func (o *oidcServer) SetTokenStatus(s int) {
	if s == 0 {
		s = http.StatusOK
	}
	o.tokenStatus.Store(int32(s))
}
func (o *oidcServer) SetTokenBody(b string) { o.tokenBody.Store(b) }

func (o *oidcServer) MakeIDToken(t *testing.T, claims map[string]any) string {
	return signJWT(t, map[string]any{"alg": "RS256", "kid": o.kid}, claims, o.priv)
}

// OIDC config + query fixtures shared by the callback tests.

// oidcConfigJSON returns the JSON shape stored on IdentityProvider.config.
func oidcConfigJSON(authzURL, tokenURL, jwksURL, clientID, redirectURI, audience string) []byte {
	b, _ := json.Marshal(map[string]any{
		"authorizeUrl": authzURL,
		"tokenUrl":     tokenURL,
		"jwksUri":      jwksURL,
		"clientId":     clientID,
		"clientSecret": "shh",
		"redirectUri":  redirectURI,
		"issuer":       "https://idp.example.com",
		"audience":     audience,
		"emailClaim":   "email",
	})
	return b
}

const idpQuery = `SELECT id, type, name, enabled, config, "defaultRole", "defaultControlPlaneAccess", "jitEnabled"`

func newOIDCCallbackCtx(code, state string) (echo.Context, *httptest.ResponseRecorder) {
	target := "/authserver/oidc/callback"
	v := url.Values{}
	if code != "" {
		v.Set("code", code)
	}
	if state != "" {
		v.Set("state", state)
	}
	if q := v.Encode(); q != "" {
		target += "?" + q
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

// Callback handler — token exchange + JWT validate + JIT + redirect.

// callbackFixture wires the OIDC stack against a fake IdP HTTP server.
type callbackFixture struct {
	mock     pgxmock.PgxPoolIface
	pending  *store.PendingAuthzStore
	codes    *store.AuthCodeStore
	server   *oidcServer
	idpID    string
	subject  string
	deps     login.OIDCDeps
	authctx  string
	pendinge store.PendingAuthzEntry
	cfgJSON  []byte
}

func newCallbackFixture(t *testing.T, withJIT bool) callbackFixture {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock: %v", err)
	}
	t.Cleanup(mock.Close)

	srv := newOIDCServer(t, "kid-1")
	const issuer = "https://idp.example.com"
	cfgJSON, _ := json.Marshal(map[string]any{
		"issuer":       issuer,
		"jwksUri":      srv.JWKSURL(),
		"clientId":     "client-cb",
		"clientSecret": "shh",
		"authorizeUrl": srv.URL() + "/authorize",
		"tokenUrl":     srv.TokenURL(),
		"redirectUri":  "https://app/cb",
		"audience":     "nexus-audience",
		"emailClaim":   "email",
	})

	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	authctx := "state-cb"
	pe := store.PendingAuthzEntry{
		ClientID:      "cli-cb",
		RedirectURI:   "https://app/cb",
		Scope:         "openid",
		State:         "sso-state",
		Nonce:         "nonce-cb",
		CodeChallenge: "cc-cb",
		IdPID:         "idp-cb",
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	}
	pending.Put(authctx, pe)

	codes := store.NewAuthCodeStore(5 * time.Minute)
	t.Cleanup(codes.Close)

	return callbackFixture{
		mock:     mock,
		pending:  pending,
		codes:    codes,
		server:   srv,
		idpID:    "idp-cb",
		subject:  "subject-42",
		cfgJSON:  cfgJSON,
		authctx:  authctx,
		pendinge: pe,
		deps: login.OIDCDeps{
			IdPs:      store.NewIdPStoreWithPool(mock),
			Federated: store.NewFederatedStoreWithPool(mock),
			Pending:   pending,
			AuthCodes: codes,
		},
	}
}

func expectGetByID(mock pgxmock.PgxPoolIface, id string, cfgJSON []byte, jit bool) {
	mock.ExpectQuery(idpQuery).WithArgs(id).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "defaultRole", "defaultControlPlaneAccess", "jitEnabled",
		}).AddRow(id, "oidc", "CB IdP", true, cfgJSON, "developer", false, jit))
}

func mintIDToken(t *testing.T, srv *oidcServer, subject, email string, exp time.Time) string {
	return srv.MakeIDToken(t, map[string]any{
		"iss":   "https://idp.example.com",
		"aud":   "nexus-audience",
		"sub":   subject,
		"email": email,
		"exp":   float64(exp.Unix()),
	})
}

// TestOIDCCallbackHandler_Success_ExistingUser — happy path: token
// exchange OK, JWT verifies, Federated lookup hits, auth code minted,
// browser redirected back to client with code+state.
func TestOIDCCallbackHandler_Success_ExistingUser(t *testing.T) {
	fx := newCallbackFixture(t, false)
	prod := &capturingProducer{}
	fx.deps.Audit = audit.NewWriter(prod, "admin-audit", nil)
	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, false)
	// FederatedStore.FindByIdPSubject hits the DB; existing user — one row.
	fx.mock.ExpectQuery(`SELECT id, "userId", "idpId"`).
		WithArgs(fx.idpID, fx.subject).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt",
		}).AddRow("fi-1", "user-real", fx.idpID, fx.subject, ptrStr("alice@example.com"), []byte(`{}`), time.Now(), (*time.Time)(nil)))
	// UpdateRawClaims fires after lookup (best-effort, ignored err).
	fx.mock.ExpectExec(`UPDATE "UserFederatedIdentity" SET "rawClaims"`).
		WithArgs("fi-1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	tok := mintIDToken(t, fx.server, fx.subject, "alice@example.com", time.Now().Add(time.Hour))
	fx.server.SetIDToken(tok)

	c, rec := newOIDCCallbackCtx("auth-code-xyz", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302 (body=%q)", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	// Redirect must preserve the registered URI exactly; only adds code+state.
	if u.Scheme+"://"+u.Host+u.Path != "https://app/cb" {
		t.Fatalf("redirect mutated: %q", loc)
	}
	if u.Query().Get("state") != "sso-state" {
		t.Fatalf("state lost: %q", u.Query().Get("state"))
	}
	code := u.Query().Get("code")
	if code == "" {
		t.Fatal("code missing from redirect")
	}
	entry, ok := fx.codes.Get(code)
	if !ok {
		t.Fatal("auth code not stored")
	}
	if entry.UserID != "user-real" {
		t.Fatalf("userID: got %q, want user-real", entry.UserID)
	}
	if entry.Email != "alice@example.com" {
		t.Fatalf("email: got %q", entry.Email)
	}
	if len(entry.AMR) != 1 || entry.AMR[0] != "sso" {
		t.Fatalf("AMR: got %v, want [sso]", entry.AMR)
	}
	// Pending must be consumed.
	if _, ok := fx.pending.Take(fx.authctx); ok {
		t.Fatal("pending entry survived consumption")
	}
	// A successful OIDC login must emit the admin.login.succeeded audit row.
	msgs := waitForAuditMsg(t, prod, 1)
	var ev map[string]any
	if err := json.Unmarshal(msgs[0], &ev); err != nil {
		t.Fatalf("audit unmarshal: %v", err)
	}
	if ev["action"] != "admin.login.succeeded" {
		t.Errorf("audit action = %v, want admin.login.succeeded", ev["action"])
	}
	if ev["actorLabel"] != "alice@example.com" {
		t.Errorf("audit actorLabel = %v, want alice@example.com", ev["actorLabel"])
	}
}

// ptrStr returns *string for sql nullable column.
func ptrStr(s string) *string { return &s }

// TestOIDCCallbackHandler_Success_JIT covers JIT provisioning: Federated
// lookup misses, the IdP row has jitEnabled=true, JITProvisionUser runs
// in a tx → user + identity row, auth code minted, redirect emitted.
func TestOIDCCallbackHandler_Success_JIT(t *testing.T) {
	fx := newCallbackFixture(t, true)
	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, true)
	// FindByIdPSubject → no rows.
	fx.mock.ExpectQuery(`SELECT id, "userId", "idpId"`).
		WithArgs(fx.idpID, fx.subject).
		WillReturnError(pgx.ErrNoRows)
	// JITProvisionUser runs in a tx.
	fx.mock.ExpectBegin()
	fx.mock.ExpectQuery(`SELECT id FROM "Organization"`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("org-1"))
	// DisplayName is the humanized email local-part ("alice@example.com" →
	// "alice") since this token carries no name claim.
	fx.mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs("org-1", "alice", pgxmock.AnyArg(), "oidc", false, "oidc-jit").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "displayName", "email", "status", "source",
		}).AddRow("user-jit", "alice", ptrStr("alice@example.com"), "active", "oidc"))
	fx.mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("user-jit", fx.idpID, fx.subject).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi-new"))
	// Baseline-role lookup: the fixture IdP carries defaultRole "developer";
	// it resolves to no live group here, so the membership is skipped.
	fx.mock.ExpectQuery(`SELECT id FROM "IamGroup" WHERE name`).
		WithArgs("developer").
		WillReturnError(pgx.ErrNoRows)
	fx.mock.ExpectCommit()

	tok := mintIDToken(t, fx.server, fx.subject, "alice@example.com", time.Now().Add(time.Hour))
	fx.server.SetIDToken(tok)

	c, rec := newOIDCCallbackCtx("auth-code-jit", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302 (body=%q)", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	u, _ := url.Parse(loc)
	code := u.Query().Get("code")
	entry, ok := fx.codes.Get(code)
	if !ok {
		t.Fatal("auth code not stored")
	}
	if entry.UserID != "user-jit" {
		t.Fatalf("userID: got %q, want user-jit", entry.UserID)
	}
}

// TestOIDCCallbackHandler_JITDisabled — Federated lookup misses AND the
// IdP row has jitEnabled=false. Expect 401 user_not_provisioned so the
// admin must enable JIT (or create the user via SCIM) before the user
// can log in. Critical: silently creating users when admins set
// jitEnabled=false would defeat the policy.
func TestOIDCCallbackHandler_JITDisabled(t *testing.T) {
	fx := newCallbackFixture(t, false)
	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, false) // jitEnabled=false
	fx.mock.ExpectQuery(`SELECT id, "userId", "idpId"`).
		WithArgs(fx.idpID, fx.subject).
		WillReturnError(pgx.ErrNoRows)

	tok := mintIDToken(t, fx.server, fx.subject, "alice@example.com", time.Now().Add(time.Hour))
	fx.server.SetIDToken(tok)

	c, rec := newOIDCCallbackCtx("auth-code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "user_not_provisioned" {
		t.Fatalf("error: got %q, want user_not_provisioned", body["error"])
	}
}

// TestOIDCCallbackHandler_MissingArgs validates the two-arg gate at the
// top of the handler.
func TestOIDCCallbackHandler_MissingArgs(t *testing.T) {
	fx := newCallbackFixture(t, false)
	cases := []struct {
		name        string
		code, state string
	}{
		{"empty code", "", "x"},
		{"empty state", "x", ""},
		{"both empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newOIDCCallbackCtx(tc.code, tc.state)
			if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
				t.Fatalf("handler: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400", rec.Code)
			}
		})
	}
}

// TestOIDCCallbackHandler_UnknownState — pending.Take misses → 400.
func TestOIDCCallbackHandler_UnknownState(t *testing.T) {
	fx := newCallbackFixture(t, false)
	c, rec := newOIDCCallbackCtx("code", "never-stored")
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

// TestOIDCCallbackHandler_MissingIdPIDOnPending — pending entry exists but
// IdPID never got stamped (begin handler skipped). Must refuse — there
// is no safe default IdP for the callback.
func TestOIDCCallbackHandler_MissingIdPIDOnPending(t *testing.T) {
	fx := newCallbackFixture(t, false)
	// Re-put without IdPID.
	pe := fx.pendinge
	pe.IdPID = ""
	fx.pending.Put(fx.authctx, pe)

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "oidc_not_configured" {
		t.Fatalf("error: got %q, want oidc_not_configured", body["error"])
	}
}

// TestOIDCCallbackHandler_GetByIDFails — DB error after state lookup.
// Pending was already consumed by Take, so we lose the user's session
// but the response must still be 500 internal_error.
func TestOIDCCallbackHandler_GetByIDFails(t *testing.T) {
	fx := newCallbackFixture(t, false)
	fx.mock.ExpectQuery(idpQuery).WithArgs(fx.idpID).
		WillReturnError(errors.New("pool exhausted"))

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "pool exhausted") {
		t.Fatalf("driver error leaked: %q", rec.Body.String())
	}
}

// TestOIDCCallbackHandler_ConfigMissingTokenURL — IdP row exists but its
// config blob has no tokenUrl. Cannot exchange the code → 400.
func TestOIDCCallbackHandler_ConfigMissingTokenURL(t *testing.T) {
	fx := newCallbackFixture(t, false)
	cfg, _ := json.Marshal(map[string]any{
		"jwksUri":      "https://idp/jwks",
		"clientId":     "cid",
		"authorizeUrl": "https://idp/authorize",
		// no tokenUrl
	})
	fx.mock.ExpectQuery(idpQuery).WithArgs(fx.idpID).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "defaultRole", "defaultControlPlaneAccess", "jitEnabled",
		}).AddRow(fx.idpID, "oidc", "Thin", true, cfg, "developer", false, true))

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "oidc_not_configured" {
		t.Fatalf("error: got %q, want oidc_not_configured", body["error"])
	}
}

// TestOIDCCallbackHandler_TokenExchangeNon200 — the IdP rejects the
// code exchange. Handler must return 401 oidc_exchange_failed so the
// SPA can prompt the user to retry.
func TestOIDCCallbackHandler_TokenExchangeNon200(t *testing.T) {
	fx := newCallbackFixture(t, false)
	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, false)
	fx.server.SetTokenStatus(http.StatusBadRequest)
	fx.server.SetTokenBody(`{"error":"invalid_grant"}`)

	c, rec := newOIDCCallbackCtx("expired-code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "oidc_exchange_failed" {
		t.Fatalf("error: got %q, want oidc_exchange_failed", body["error"])
	}
}

// TestOIDCCallbackHandler_TokenResponseMissingIDToken — exchange returns
// 200 but the body has no id_token field. Same 401 oidc_exchange_failed.
func TestOIDCCallbackHandler_TokenResponseMissingIDToken(t *testing.T) {
	fx := newCallbackFixture(t, false)
	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, false)
	fx.server.SetTokenBody(`{"access_token":"oops"}`)

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestOIDCCallbackHandler_TokenResponseUnparseable — JSON body malformed.
func TestOIDCCallbackHandler_TokenResponseUnparseable(t *testing.T) {
	fx := newCallbackFixture(t, false)
	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, false)
	fx.server.SetTokenBody(`{not-json}`)

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestOIDCCallbackHandler_JWTInvalid — the IdP returns a syntactically-
// valid id_token but with a bad signature (signed by a different key).
// Handler must refuse with 401 oidc_token_invalid; this is the strongest
// security signal that an upstream IdP got compromised.
func TestOIDCCallbackHandler_JWTInvalid(t *testing.T) {
	fx := newCallbackFixture(t, false)
	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, false)

	// Sign with a DIFFERENT key — verification will fail.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	bad := signJWT(t,
		map[string]any{"alg": "RS256", "kid": "kid-1"},
		map[string]any{
			"iss":   "https://idp.example.com",
			"aud":   "nexus-audience",
			"sub":   fx.subject,
			"email": "alice@example.com",
			"exp":   float64(time.Now().Add(time.Hour).Unix()),
		},
		other,
	)
	fx.server.SetIDToken(bad)

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "oidc_token_invalid" {
		t.Fatalf("error: got %q, want oidc_token_invalid", body["error"])
	}
}

// TestOIDCCallbackHandler_JWTIssuerMismatch — token verifies but
// `iss` claim doesn't match the configured issuer. Critical: a
// compromised but valid IdP key for a different deployment must not
// be accepted.
func TestOIDCCallbackHandler_JWTIssuerMismatch(t *testing.T) {
	fx := newCallbackFixture(t, false)
	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, false)
	tok := fx.server.MakeIDToken(t, map[string]any{
		"iss":   "https://OTHER.idp.example.com",
		"aud":   "nexus-audience",
		"sub":   fx.subject,
		"email": "alice@example.com",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
	})
	fx.server.SetIDToken(tok)

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestOIDCCallbackHandler_FindByIdPSubjectFailure — DB error (not
// pgx.ErrNoRows) from Federated lookup must surface as 500
// internal_error.
func TestOIDCCallbackHandler_FindByIdPSubjectFailure(t *testing.T) {
	fx := newCallbackFixture(t, false)
	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, false)
	fx.mock.ExpectQuery(`SELECT id, "userId", "idpId"`).
		WithArgs(fx.idpID, fx.subject).
		WillReturnError(errors.New("network blip"))

	tok := mintIDToken(t, fx.server, fx.subject, "alice@example.com", time.Now().Add(time.Hour))
	fx.server.SetIDToken(tok)

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "network blip") {
		t.Fatalf("driver error leaked: %q", rec.Body.String())
	}
}

// TestOIDCCallbackHandler_JITFailure — Federated lookup misses, jit
// enabled, but the INSERT in JITProvisionUser blows up. Handler must
// return 500 internal_error without partial state.
func TestOIDCCallbackHandler_JITFailure(t *testing.T) {
	fx := newCallbackFixture(t, false)
	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, true)
	fx.mock.ExpectQuery(`SELECT id, "userId", "idpId"`).
		WithArgs(fx.idpID, fx.subject).
		WillReturnError(pgx.ErrNoRows)
	fx.mock.ExpectBegin()
	fx.mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WillReturnError(errors.New("unique violation"))
	fx.mock.ExpectRollback()

	tok := mintIDToken(t, fx.server, fx.subject, "alice@example.com", time.Now().Add(time.Hour))
	fx.server.SetIDToken(tok)

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

// TestOIDCCallbackHandler_JITUsesSubjectAsDisplayWhenEmailMissing covers
// the display-name fallback branch (display = subject when claims.Email
// is empty). Also verifies the auth code's Email field reflects the
// empty value (downstream token endpoint must omit email claim).
func TestOIDCCallbackHandler_JITUsesSubjectAsDisplayWhenEmailMissing(t *testing.T) {
	fx := newCallbackFixture(t, false)
	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, true)
	fx.mock.ExpectQuery(`SELECT id, "userId", "idpId"`).
		WithArgs(fx.idpID, fx.subject).
		WillReturnError(pgx.ErrNoRows)
	// JITProvisionParams.DisplayName = claims.Subject when email is empty,
	// so the NexusUser INSERT receives subject as the first arg.
	fx.mock.ExpectBegin()
	fx.mock.ExpectQuery(`SELECT id FROM "Organization"`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("org-1"))
	fx.mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs("org-1", fx.subject, (*string)(nil), "oidc", false, "oidc-jit").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "displayName", "email", "status", "source",
		}).AddRow("user-anon", fx.subject, (*string)(nil), "active", "oidc"))
	fx.mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("user-anon", fx.idpID, fx.subject).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi-anon"))
	fx.mock.ExpectQuery(`SELECT id FROM "IamGroup" WHERE name`).
		WithArgs("developer").
		WillReturnError(pgx.ErrNoRows)
	fx.mock.ExpectCommit()

	// Token with NO email claim — DisplayName falls back to subject.
	tok := fx.server.MakeIDToken(t, map[string]any{
		"iss": "https://idp.example.com",
		"aud": "nexus-audience",
		"sub": fx.subject,
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	fx.server.SetIDToken(tok)

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302 (body=%q)", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	u, _ := url.Parse(loc)
	entry, _ := fx.codes.Get(u.Query().Get("code"))
	if entry.Email != "" {
		t.Fatalf("authcode email: got %q, want empty (no claim)", entry.Email)
	}
}

// TestOIDCCallbackHandler_RedirectBuildFailure — pending entry holds an
// unparseable RedirectURI. The OAuth client registration layer should
// have prevented this, but the handler must still fail closed.
func TestOIDCCallbackHandler_RedirectBuildFailure(t *testing.T) {
	fx := newCallbackFixture(t, false)
	// Replace pending entry's RedirectURI with a malformed one.
	pe := fx.pendinge
	pe.RedirectURI = "://broken"
	fx.pending.Put(fx.authctx, pe)

	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, false)
	fx.mock.ExpectQuery(`SELECT id, "userId", "idpId"`).
		WithArgs(fx.idpID, fx.subject).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt",
		}).AddRow("fi-1", "user-real", fx.idpID, fx.subject, ptrStr("alice@example.com"), []byte(`{}`), time.Now(), (*time.Time)(nil)))
	fx.mock.ExpectExec(`UPDATE "UserFederatedIdentity" SET "rawClaims"`).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	tok := mintIDToken(t, fx.server, fx.subject, "alice@example.com", time.Now().Add(time.Hour))
	fx.server.SetIDToken(tok)

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

// TestOIDCCallbackHandler_TokenEndpointUnreachable — pure exchangeOIDCCode
// error branch: the token endpoint is unreachable (server closed). The
// exchange MUST fail before any DB call to Federated; mocked Federated
// expectations stay quiet.
func TestOIDCCallbackHandler_TokenEndpointUnreachable(t *testing.T) {
	fx := newCallbackFixture(t, false)

	// Build a tokenURL that returns transport errors. We stop the server
	// and reuse its URL — any Do() will fail.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cfg, _ := json.Marshal(map[string]any{
		"issuer":       "https://idp.example.com",
		"jwksUri":      fx.server.JWKSURL(),
		"clientId":     "client-cb",
		"clientSecret": "shh",
		"authorizeUrl": fx.server.URL() + "/authorize",
		"tokenUrl":     deadURL,
		"redirectUri":  "https://app/cb",
		"audience":     "nexus-audience",
	})
	fx.mock.ExpectQuery(idpQuery).WithArgs(fx.idpID).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "defaultRole", "defaultControlPlaneAccess", "jitEnabled",
		}).AddRow(fx.idpID, "oidc", "CB IdP", true, cfg, "developer", false, false))

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestOIDCCallbackHandler_TokenURLMalformedRequestBuild covers
// http.NewRequestWithContext returning an error — feed in a URL with
// a control character.
func TestOIDCCallbackHandler_TokenURLMalformedRequestBuild(t *testing.T) {
	fx := newCallbackFixture(t, false)

	cfg, _ := json.Marshal(map[string]any{
		"issuer":       "https://idp.example.com",
		"jwksUri":      fx.server.JWKSURL(),
		"clientId":     "client-cb",
		"clientSecret": "shh",
		"authorizeUrl": fx.server.URL() + "/authorize",
		"tokenUrl":     "http://\x00bad",
		"redirectUri":  "https://app/cb",
		"audience":     "nexus-audience",
	})
	fx.mock.ExpectQuery(idpQuery).WithArgs(fx.idpID).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "defaultRole", "defaultControlPlaneAccess", "jitEnabled",
		}).AddRow(fx.idpID, "oidc", "CB IdP", true, cfg, "developer", false, false))

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestOIDCCallbackHandler_DiscoversEndpointsFromIssuer drives the callback's
// discovery wiring: the saved config carries only the issuer (the Add-IdP
// form's issuer-only shape), so the handler must resolve token + jwks
// endpoints from `<issuer>/.well-known/openid-configuration` before exchanging
// the code and validating the ID token. A missing resolver would 400 with
// oidc_not_configured; with it, login completes end to end.
func TestOIDCCallbackHandler_DiscoversEndpointsFromIssuer(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock: %v", err)
	}
	t.Cleanup(mock.Close)

	srv := newOIDCServer(t, "kid-disco")
	issuer := srv.URL() // the mock serves /.well-known/openid-configuration here
	const idpID = "idp-disco-cb"
	const subject = "subject-disco"

	// Issuer-only config: no tokenUrl / jwksUri / authorizeUrl persisted.
	cfgJSON, _ := json.Marshal(map[string]any{
		"issuer":       issuer,
		"clientId":     "client-disco-cb",
		"clientSecret": "shh",
		"redirectUri":  "https://app/cb",
		"audience":     "nexus-audience",
		"emailClaim":   "email",
	})

	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	authctx := "state-disco"
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID:    "cli-disco",
		RedirectURI: "https://app/cb",
		Scope:       "openid",
		State:       "sso-state",
		IdPID:       idpID,
		ExpiresAt:   time.Now().Add(5 * time.Minute),
	})
	codes := store.NewAuthCodeStore(5 * time.Minute)
	t.Cleanup(codes.Close)

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(mock),
		Pending:   pending,
		AuthCodes: codes,
		Resolver:  oidcdisco.NewResolver(),
	}

	expectGetByID(mock, idpID, cfgJSON, false)
	mock.ExpectQuery(`SELECT id, "userId", "idpId"`).
		WithArgs(idpID, subject).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt",
		}).AddRow("fi-1", "user-real", idpID, subject, ptrStr("bob@example.com"), []byte(`{}`), time.Now(), (*time.Time)(nil)))
	mock.ExpectExec(`UPDATE "UserFederatedIdentity" SET "rawClaims"`).
		WithArgs("fi-1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// The id_token issuer must match the configured issuer (server URL).
	tok := srv.MakeIDToken(t, map[string]any{
		"iss":   issuer,
		"aud":   "nexus-audience",
		"sub":   subject,
		"email": "bob@example.com",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
	})
	srv.SetIDToken(tok)

	c, rec := newOIDCCallbackCtx("auth-code-disco", authctx)
	if err := login.OIDCCallbackHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302 via discovery (body=%q)", rec.Code, rec.Body.String())
	}
	u, perr := url.Parse(rec.Header().Get("Location"))
	if perr != nil {
		t.Fatalf("parse Location: %v", perr)
	}
	code := u.Query().Get("code")
	if code == "" {
		t.Fatal("no auth code minted on the discovery path")
	}
	entry, ok := codes.Get(code)
	if !ok || entry.UserID != "user-real" {
		t.Fatalf("auth code/user wrong via discovery: ok=%v entry=%+v", ok, entry)
	}
}

// TestOIDCCallbackHandler_IdPError asserts that when the IdP redirects back
// with error/error_description instead of a code (e.g. Auth0 "parameter
// organization is required"), the handler sends the browser to the terminal
// SSO-error page carrying only the bounded OAuth error code — never the
// free-text description — instead of masking it as an authctx_expired JSON 400.
func TestOIDCCallbackHandler_IdPError(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet,
		"/authserver/oidc/callback?error=invalid_request&error_description=parameter+organization+is+required&state=state-x", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	// No stores are touched on the error path — empty deps are fine.
	if err := login.OIDCCallbackHandler(login.OIDCDeps{})(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("got %d, want 302 (body=%q)", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Path != "/auth/sso-error" {
		t.Fatalf("redirect path = %q, want /auth/sso-error", loc.Path)
	}
	if loc.Query().Get("code") != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", loc.Query().Get("code"))
	}
	// The free-text description must NOT be reflected into the redirect URL.
	if strings.Contains(loc.RawQuery, "organization") {
		t.Fatalf("error_description leaked into redirect: %q", loc.RawQuery)
	}
}

// TestOIDCCallbackHandler_AudienceDefaultsToClientID covers two real-world
// Auth0 quirks at once: the saved config carries no `audience` (so the ID
// token's aud=client_id must be accepted), and the token's `iss` ends in a
// trailing slash the configured issuer lacks. Both must still validate.
func TestOIDCCallbackHandler_AudienceDefaultsToClientID(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock: %v", err)
	}
	t.Cleanup(mock.Close)

	srv := newOIDCServer(t, "kid-aud")
	issuer := srv.URL()
	const idpID = "idp-aud"
	const subject = "subject-aud"
	const clientID = "client-aud-xyz"

	// Issuer-only config: no audience, no token/jwks endpoints (discovered).
	cfgJSON, _ := json.Marshal(map[string]any{
		"issuer":       issuer,
		"clientId":     clientID,
		"clientSecret": "shh",
		"redirectUri":  "https://app/cb",
		"emailClaim":   "email",
	})

	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	authctx := "state-aud"
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID:    "cli-aud",
		RedirectURI: "https://app/cb",
		Scope:       "openid",
		State:       "sso-state",
		IdPID:       idpID,
		ExpiresAt:   time.Now().Add(5 * time.Minute),
	})
	codes := store.NewAuthCodeStore(5 * time.Minute)
	t.Cleanup(codes.Close)

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(mock),
		Pending:   pending,
		AuthCodes: codes,
		Resolver:  oidcdisco.NewResolver(),
	}

	expectGetByID(mock, idpID, cfgJSON, false)
	mock.ExpectQuery(`SELECT id, "userId", "idpId"`).
		WithArgs(idpID, subject).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt",
		}).AddRow("fi-1", "user-real", idpID, subject, ptrStr("bob@example.com"), []byte(`{}`), time.Now(), (*time.Time)(nil)))
	mock.ExpectExec(`UPDATE "UserFederatedIdentity" SET "rawClaims"`).
		WithArgs("fi-1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// aud = client_id (config has no audience), iss WITH a trailing slash.
	tok := srv.MakeIDToken(t, map[string]any{
		"iss":   issuer + "/",
		"aud":   clientID,
		"sub":   subject,
		"email": "bob@example.com",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
	})
	srv.SetIDToken(tok)

	c, rec := newOIDCCallbackCtx("auth-code-aud", authctx)
	if err := login.OIDCCallbackHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302 (aud=client_id default + trailing-slash iss; body=%q)", rec.Code, rec.Body.String())
	}
}

// nonceCallbackDeps builds a callback against a fresh mock IdP for the nonce
// tests, with the pending entry carrying expectNonce and the ID token carrying
// tokenNonce. Returns the wired context + recorder. The federated lookup is
// only expected when the nonce is expected to match (the mismatch path bails
// before any DB read).
func nonceCallbackDeps(t *testing.T, expectNonce, tokenNonce string, federatedExpected bool) (echo.HandlerFunc, echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock: %v", err)
	}
	t.Cleanup(mock.Close)

	srv := newOIDCServer(t, "kid-nonce")
	issuer := srv.URL()
	const idpID = "idp-nonce"
	const subject = "subject-nonce"
	const clientID = "client-nonce"

	cfgJSON, _ := json.Marshal(map[string]any{
		"issuer":       issuer,
		"clientId":     clientID,
		"clientSecret": "shh",
		"redirectUri":  "https://app/cb",
		"emailClaim":   "email",
	})

	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	authctx := "state-nonce"
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "https://app/cb", Scope: "openid",
		State: "sso-state", IdPID: idpID, IdPNonce: expectNonce,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	codes := store.NewAuthCodeStore(5 * time.Minute)
	t.Cleanup(codes.Close)

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(mock),
		Pending:   pending,
		AuthCodes: codes,
		Resolver:  oidcdisco.NewResolver(),
	}
	expectGetByID(mock, idpID, cfgJSON, false)
	if federatedExpected {
		mock.ExpectQuery(`SELECT id, "userId", "idpId"`).
			WithArgs(idpID, subject).
			WillReturnRows(pgxmock.NewRows([]string{
				"id", "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt",
			}).AddRow("fi-1", "user-real", idpID, subject, ptrStr("z@example.com"), []byte(`{}`), time.Now(), (*time.Time)(nil)))
		mock.ExpectExec(`UPDATE "UserFederatedIdentity" SET "rawClaims"`).
			WithArgs("fi-1", pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	}

	tok := srv.MakeIDToken(t, map[string]any{
		"iss": issuer, "aud": clientID, "sub": subject,
		"email": "z@example.com", "nonce": tokenNonce,
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	srv.SetIDToken(tok)

	c, rec := newOIDCCallbackCtx("auth-code-nonce", authctx)
	return login.OIDCCallbackHandler(deps), c, rec
}

// TestOIDCCallbackHandler_NonceMismatch asserts a token whose nonce does not
// echo the one we sent at SSO start is rejected — ID-token replay/injection.
func TestOIDCCallbackHandler_NonceMismatch(t *testing.T) {
	h, c, rec := nonceCallbackDeps(t, "expected-nonce", "WRONG-nonce", false)
	if err := h(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401 on nonce mismatch (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestOIDCCallbackHandler_NonceMatch asserts a token echoing the sent nonce
// passes the binding check and completes login.
func TestOIDCCallbackHandler_NonceMatch(t *testing.T) {
	h, c, rec := nonceCallbackDeps(t, "good-nonce", "good-nonce", true)
	if err := h(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302 on nonce match (body=%q)", rec.Code, rec.Body.String())
	}
}

// Sentinel — ensures unused atomic import is fine.
var _ = sync.Mutex{}
var _ = context.Background
