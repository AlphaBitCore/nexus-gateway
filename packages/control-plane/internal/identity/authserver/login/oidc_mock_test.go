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

// Begin handler — resolveOIDCIdP + URL build + SetIdPID.

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

const idpQuery = `SELECT id, type, name, enabled, config, "roleMapping", "defaultRole", "jitEnabled"`

func newOIDCBeginCtx(authctx, idpID string) (echo.Context, *httptest.ResponseRecorder) {
	target := "/authserver/oidc/begin"
	v := url.Values{}
	if authctx != "" {
		v.Set("authctx", authctx)
	}
	if idpID != "" {
		v.Set("idp_id", idpID)
	}
	if q := v.Encode(); q != "" {
		target += "?" + q
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

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

// TestOIDCBeginHandler_Success_ExplicitIdP exercises the multi-IdP path
// where the SPA explicitly picks an IdP at the method-picker. The handler
// must (a) accept the authctx, (b) load the per-IdP config, (c) build the
// upstream authorize URL with response_type=code + the IdP's clientId,
// (d) stamp the IdP id onto the pending entry so the callback later
// knows which IdP to verify against.
func TestOIDCBeginHandler_Success_ExplicitIdP(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock: %v", err)
	}
	t.Cleanup(mock.Close)

	authz := "https://idp.example.com/authorize"
	cfg := oidcConfigJSON(authz, "https://idp.example.com/token",
		"https://idp.example.com/jwks", "client-123", "https://app/cb", "nexus")

	mock.ExpectQuery(idpQuery).WithArgs("idp-explicit").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).AddRow("idp-explicit", "oidc", "Corp IdP", true, cfg, []byte(`[]`), "developer", true))

	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	authctx := "ctx-begin-success"
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "http://127.0.0.1/cb", Scope: "openid",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	c, rec := newOIDCBeginCtx(authctx, "idp-explicit")
	if err := login.OIDCBeginHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	got, err := url.Parse(resp["authorizationUrl"])
	if err != nil {
		t.Fatalf("parse authorizationUrl: %v", err)
	}
	if got.Scheme+"://"+got.Host+got.Path != authz {
		t.Fatalf("authorize URL mutated: %q", resp["authorizationUrl"])
	}
	q := got.Query()
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type: got %q, want code", q.Get("response_type"))
	}
	if q.Get("client_id") != "client-123" {
		t.Fatalf("client_id: got %q, want client-123", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "https://app/cb" {
		t.Fatalf("redirect_uri: %q", q.Get("redirect_uri"))
	}
	if q.Get("scope") != "openid profile email" {
		t.Fatalf("scope: %q", q.Get("scope"))
	}
	if q.Get("state") != authctx {
		t.Fatalf("state must echo authctx; got %q", q.Get("state"))
	}
}

// TestOIDCBeginHandler_RejectsMissingAuthctx ensures unauthenticated
// callers cannot probe the OIDC begin endpoint to learn IdP existence.
func TestOIDCBeginHandler_RejectsMissingAuthctx(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)

	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	for _, ctx := range []string{"", "unknown"} {
		c, rec := newOIDCBeginCtx(ctx, "")
		if err := login.OIDCBeginHandler(deps)(c); err != nil {
			t.Fatalf("handler(%q): %v", ctx, err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status(%q): got %d, want 400", ctx, rec.Code)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("DB was queried with missing authctx: %v", err)
	}
}

// TestOIDCBeginHandler_ImplicitSingleOIDC drops idp_id and relies on the
// sole-OIDC-row fallback. ListEnabled returns one OIDC row + a local row;
// resolveOIDCIdP must pick the OIDC one.
func TestOIDCBeginHandler_ImplicitSingleOIDC(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)

	cfg := oidcConfigJSON("https://idp/authorize", "https://idp/token",
		"https://idp/jwks", "cid", "https://app/cb", "aud")
	mock.ExpectQuery(idpQuery).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).
			AddRow("local-1", "local", "Local", true, []byte(`{}`), []byte(`[]`), "developer", true).
			AddRow("oidc-1", "oidc", "Single Okta", true, cfg, []byte(`[]`), "developer", true))

	authctx := "ctx-implicit"
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "http://127.0.0.1/cb",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	c, rec := newOIDCBeginCtx(authctx, "")
	if err := login.OIDCBeginHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
}

// TestOIDCBeginHandler_ImplicitMultipleOIDC_RequiresIdP exercises the
// `idp_id_required` branch: two OIDC rows but no idp_id query param —
// the handler must refuse rather than silently picking one. Picking
// silently would defeat the multi-IdP method picker.
func TestOIDCBeginHandler_ImplicitMultipleOIDC_RequiresIdP(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)

	cfg := oidcConfigJSON("https://idp/authorize", "https://idp/token",
		"https://idp/jwks", "cid", "https://app/cb", "aud")
	mock.ExpectQuery(idpQuery).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).
			AddRow("oidc-A", "oidc", "Okta-A", true, cfg, []byte(`[]`), "developer", true).
			AddRow("oidc-B", "oidc", "Okta-B", true, cfg, []byte(`[]`), "developer", true))

	authctx := "ctx-multi"
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "http://127.0.0.1/cb",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	c, rec := newOIDCBeginCtx(authctx, "")
	if err := login.OIDCBeginHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "idp_id_required" {
		t.Fatalf("error: got %q, want idp_id_required", body["error"])
	}
}

// TestOIDCBeginHandler_NoOIDCRows returns `oidc_not_configured` so the
// SPA can show a useful message instead of an empty button list.
func TestOIDCBeginHandler_NoOIDCRows(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	mock.ExpectQuery(idpQuery).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).
			AddRow("local-1", "local", "Only Local", true, []byte(`{}`), []byte(`[]`), "developer", true))

	authctx := "ctx-no-oidc"
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "http://127.0.0.1/cb",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	c, rec := newOIDCBeginCtx(authctx, "")
	if err := login.OIDCBeginHandler(deps)(c); err != nil {
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

// TestOIDCBeginHandler_ListEnabledFailure hits the internal_error path
// when ListEnabled returns a DB error. The driver error must not leak
// into the JSON body.
func TestOIDCBeginHandler_ListEnabledFailure(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	mock.ExpectQuery(idpQuery).WillReturnError(errors.New("conn reset by peer"))

	authctx := "ctx-list-fail"
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "http://127.0.0.1/cb",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	c, rec := newOIDCBeginCtx(authctx, "")
	if err := login.OIDCBeginHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	// resolveOIDCIdP folds the DB error into a resolveErr string (so callers
	// see the same JSON shape they get for "no oidc"); the handler returns
	// 400 for any non-empty resolveErr. The body MUST still carry the
	// `internal_error` marker so ops dashboards distinguish it from
	// configuration faults, and the driver error string MUST NOT leak.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (resolve err is folded into the 400 envelope)", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "internal_error" {
		t.Fatalf("error: got %q, want internal_error", body["error"])
	}
	if strings.Contains(rec.Body.String(), "conn reset by peer") {
		t.Fatalf("driver error leaked: %q", rec.Body.String())
	}
}

// TestOIDCBeginHandler_ExplicitIdP_NotFound covers GetByID returning
// ErrIdPNotFound — handler maps it to `oidc_not_configured`.
func TestOIDCBeginHandler_ExplicitIdP_NotFound(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	mock.ExpectQuery(idpQuery).WithArgs("missing-idp").
		WillReturnError(pgx.ErrNoRows)

	authctx := "ctx-missing"
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "http://127.0.0.1/cb",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	c, rec := newOIDCBeginCtx(authctx, "missing-idp")
	if err := login.OIDCBeginHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

// TestOIDCBeginHandler_ExplicitIdP_WrongType — a non-OIDC IdP id passed
// in idp_id must be rejected so attackers cannot reuse a local-IdP id
// to trigger the OIDC flow against an invalid config.
func TestOIDCBeginHandler_ExplicitIdP_WrongType(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	mock.ExpectQuery(idpQuery).WithArgs("local-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).AddRow("local-1", "local", "Local", true, []byte(`{}`), []byte(`[]`), "developer", true))

	authctx := "ctx-wrong-type"
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "http://127.0.0.1/cb",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	c, rec := newOIDCBeginCtx(authctx, "local-1")
	if err := login.OIDCBeginHandler(deps)(c); err != nil {
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

// TestOIDCBeginHandler_ConfigIncomplete — IdP row exists and is OIDC,
// but its config is missing clientId/authorizeUrl. Begin must refuse
// rather than build a half-baked authorize URL.
func TestOIDCBeginHandler_ConfigIncomplete(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	// Config blob lacks authorizeUrl — DecodeOIDCConfig succeeds but the
	// emptiness gate fires.
	cfg, _ := json.Marshal(map[string]any{"clientId": "x"})
	mock.ExpectQuery(idpQuery).WithArgs("idp-thin").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).AddRow("idp-thin", "oidc", "Thin OIDC", true, cfg, []byte(`[]`), "developer", true))

	authctx := "ctx-thin"
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "http://127.0.0.1/cb",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	c, rec := newOIDCBeginCtx(authctx, "idp-thin")
	if err := login.OIDCBeginHandler(deps)(c); err != nil {
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

// TestOIDCBeginHandler_MalformedAuthorizeURL feeds a config whose
// authorizeUrl contains a parse error. Even though DecodeOIDCConfig
// succeeds, url.Parse should fail → 500 internal_error. The handler must
// not crash or build a garbage authorization URL.
func TestOIDCBeginHandler_MalformedAuthorizeURL(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	cfg, _ := json.Marshal(map[string]any{
		"authorizeUrl": "://broken",
		"tokenUrl":     "https://idp/token",
		"jwksUri":      "https://idp/jwks",
		"clientId":     "cid",
		"redirectUri":  "https://app/cb",
	})
	mock.ExpectQuery(idpQuery).WithArgs("idp-bad-url").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).AddRow("idp-bad-url", "oidc", "Bad URL", true, cfg, []byte(`[]`), "developer", true))

	authctx := "ctx-bad-url"
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "http://127.0.0.1/cb",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	c, rec := newOIDCBeginCtx(authctx, "idp-bad-url")
	if err := login.OIDCBeginHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500 (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestOIDCBeginHandler_SetIdPIDRace covers the rare race where Has()
// returned true but the pending entry vanished before SetIdPID ran
// (e.g. concurrent Take by another login attempt). We force the
// race by delaying the IdP query just long enough for a goroutine to
// Take the pending entry — exercising the explicit
// `if !d.Pending.SetIdPID(...)` failure branch.
func TestOIDCBeginHandler_SetIdPIDRace(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	cfg := oidcConfigJSON("https://idp/authorize", "https://idp/token",
		"https://idp/jwks", "cid", "https://app/cb", "aud")

	authctx := "ctx-race"
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "http://127.0.0.1/cb",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	// Drain the entry while the handler is between Has() and SetIdPID().
	// pgxmock's WillDelayFor blocks the IdP query for 50ms — Has runs
	// first, the goroutine fires, then SetIdPID returns false because
	// the pending map no longer has the entry.
	mock.ExpectQuery(idpQuery).WithArgs("idp-race").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).AddRow("idp-race", "oidc", "Race", true, cfg, []byte(`[]`), "developer", true)).
		WillDelayFor(100 * time.Millisecond)

	go func() {
		time.Sleep(25 * time.Millisecond)
		// Take consumes the entry, so SetIdPID's subsequent map lookup
		// misses → returns false → handler returns authctx_expired.
		pending.Take(authctx)
	}()

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	c, rec := newOIDCBeginCtx(authctx, "idp-race")
	if err := login.OIDCBeginHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (race=SetIdPID-after-Take)", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "authctx_expired" {
		t.Fatalf("error: got %q, want authctx_expired", body["error"])
	}
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
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).AddRow(id, "oidc", "CB IdP", true, cfgJSON, []byte(`[]`), "developer", jit))
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
	fx.mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs("alice@example.com", pgxmock.AnyArg(), "oidc-jit").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "displayName", "email", "status", "source",
		}).AddRow("user-jit", "alice@example.com", ptrStr("alice@example.com"), "active", "oidc"))
	fx.mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("user-jit", fx.idpID, fx.subject).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi-new"))
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
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).AddRow(fx.idpID, "oidc", "Thin", true, cfg, []byte(`[]`), "developer", true))

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
	fx.mock.ExpectQuery(`INSERT INTO "NexusUser"`).
		WithArgs(fx.subject, (*string)(nil), "oidc-jit").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "displayName", "email", "status", "source",
		}).AddRow("user-anon", fx.subject, (*string)(nil), "active", "oidc"))
	fx.mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).
		WithArgs("user-anon", fx.idpID, fx.subject).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi-anon"))
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
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).AddRow(fx.idpID, "oidc", "CB IdP", true, cfg, []byte(`[]`), "developer", false))

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
			"id", "type", "name", "enabled", "config", "roleMapping", "defaultRole", "jitEnabled",
		}).AddRow(fx.idpID, "oidc", "CB IdP", true, cfg, []byte(`[]`), "developer", false))

	c, rec := newOIDCCallbackCtx("code", fx.authctx)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// Sentinel — ensures unused atomic import is fine.
var _ = sync.Mutex{}
var _ = context.Background
