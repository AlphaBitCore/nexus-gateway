package login

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/oidcdisco"
)

// startIdPRow builds a GetByID result row with an explicit type so the start
// tests can drive the oidc / saml / local dispatch arms (samlIdPRows hardcodes
// "saml"). Column order matches store.scanIdP.
func startIdPRow(id, typ, name string, enabled, jit bool, cfg []byte) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "type", "name", "enabled", "config", "defaultRole", "defaultControlPlaneAccess", "jitEnabled",
	}).AddRow(id, typ, name, enabled, cfg, "developer", false, jit)
}

// startOIDCCfg is the minimal OIDC config blob StartHandler's oidc arm needs:
// a parseable authorize URL + client id + redirect URI.
func startOIDCCfg(authorizeURL, clientID, redirectURI string) []byte {
	b, _ := json.Marshal(map[string]any{
		"authorizeUrl": authorizeURL,
		"clientId":     clientID,
		"redirectUri":  redirectURI,
		"tokenUrl":     "https://idp.example.com/token",
		"jwksUri":      "https://idp.example.com/jwks",
		"issuer":       "https://idp.example.com",
	})
	return b
}

// newStartCtx builds the GET /authserver/idp/:idpId/start echo context with the
// path param + optional authctx query set.
func newStartCtx(idpID, authctx string) (echo.Context, *httptest.ResponseRecorder) {
	target := "/authserver/idp/" + idpID + "/start"
	if authctx != "" {
		target += "?authctx=" + url.QueryEscape(authctx)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("idpId")
	c.SetParamValues(idpID)
	return c, rec
}

func newStartDeps(t *testing.T, mock pgxmock.PgxPoolIface) (StartDeps, *store.PendingAuthzStore, *store.SAMLRequestStore) {
	t.Helper()
	pending := store.NewPendingAuthzStore()
	reqs := store.NewSAMLRequestStore()
	t.Cleanup(pending.Close)
	t.Cleanup(reqs.Close)
	return StartDeps{
		IdPs:     store.NewIdPStoreWithPool(mock),
		Pending:  pending,
		Requests: reqs,
		Issuer:   samlIssuer,
	}, pending, reqs
}

// assertStartBounce asserts a 302 back to the SPA login page carrying the still
// live authctx — the uniform non-happy-path outcome.
func assertStartBounce(t *testing.T, rec *httptest.ResponseRecorder, authctx string) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("got %d, want 302 bounce (body=%q)", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location parse: %v", err)
	}
	if loc.Path != loginPagePath {
		t.Fatalf("bounce path = %q, want %q", loc.Path, loginPagePath)
	}
	if got := loc.Query().Get("authctx"); got != authctx {
		t.Fatalf("bounce authctx = %q, want %q", got, authctx)
	}
}

func liveEntry() store.PendingAuthzEntry {
	return store.PendingAuthzEntry{ClientID: "cli", RedirectURI: "http://127.0.0.1/cb", ExpiresAt: time.Now().Add(time.Minute)}
}

// startOIDCCfgIssuerOnly mirrors the real Add-IdP form output: only issuer +
// clientId + redirectUri are stored, with the authorize/token/jwks endpoints
// left to discovery. This is the config shape the discovery bug surfaced on.
func startOIDCCfgIssuerOnly(issuer, clientID, redirectURI string) []byte {
	b, _ := json.Marshal(map[string]any{
		"issuer":      issuer,
		"clientId":    clientID,
		"redirectUri": redirectURI,
	})
	return b
}

// TestStartHandler_OIDCDiscovery covers the SSO-start leg resolving the
// authorize endpoint from the issuer's discovery document when the saved
// config omits it (the admin form's issuer-only path).
func TestStartHandler_OIDCDiscovery(t *testing.T) {
	t.Run("issuer-only config → discover authorize endpoint → 302 to IdP", func(t *testing.T) {
		var disco *httptest.Server
		mux := http.NewServeMux()
		mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"authorization_endpoint":"` + disco.URL + `/authorize",` +
				`"token_endpoint":"` + disco.URL + `/token","jwks_uri":"` + disco.URL + `/jwks"}`))
		})
		disco = httptest.NewServer(mux)
		t.Cleanup(disco.Close)

		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		cfg := startOIDCCfgIssuerOnly(disco.URL, "client-disco", "https://app/cb")
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-disco").
			WillReturnRows(startIdPRow("idp-disco", "oidc", "Auth0", true, true, cfg))
		d, pending, _ := newStartDeps(t, mock)
		d.Resolver = oidcdisco.NewResolver(oidcdisco.WithInsecureSkipHostCheck())
		pending.Put("ctxd", liveEntry())

		c, rec := newStartCtx("idp-disco", "ctxd")
		if err := StartHandler(d)(c); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("got %d, want 302 (body=%q)", rec.Code, rec.Body.String())
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		if loc.Path != "/authorize" || loc.Host != mustHost(t, disco.URL) {
			t.Fatalf("Location = %q, want discovered authorize endpoint", rec.Header().Get("Location"))
		}
		if loc.Query().Get("client_id") != "client-disco" || loc.Query().Get("state") != "ctxd" {
			t.Fatalf("authorize query missing client_id/state: %v", loc.Query())
		}
	})

	t.Run("discovery unreachable → bounce to login (no server error body)", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		// Issuer points at a dead port → discovery fetch fails.
		cfg := startOIDCCfgIssuerOnly("http://127.0.0.1:1", "cid", "https://app/cb")
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-dead").
			WillReturnRows(startIdPRow("idp-dead", "oidc", "Dead", true, true, cfg))
		d, pending, _ := newStartDeps(t, mock)
		d.Resolver = oidcdisco.NewResolver(oidcdisco.WithInsecureSkipHostCheck())
		pending.Put("ctxdead", liveEntry())

		c, rec := newStartCtx("idp-dead", "ctxdead")
		_ = StartHandler(d)(c)
		assertStartBounce(t, rec, "ctxdead")
	})
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}

// TestStartHandler_OIDCSendsNonce asserts the SSO-start leg generates a nonce,
// sends it as the authorize `nonce` parameter, and stamps the same value on the
// pending entry for the callback to verify.
func TestStartHandler_OIDCSendsNonce(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	cfg := startOIDCCfg("https://idp.example.com/authorize", "cid", "https://app/cb")
	mock.ExpectQuery(idpRowQuery).WithArgs("idp-nonce").
		WillReturnRows(startIdPRow("idp-nonce", "oidc", "Okta", true, true, cfg))
	d, pending, _ := newStartDeps(t, mock)
	pending.Put("ctxn", liveEntry())

	c, rec := newStartCtx("idp-nonce", "ctxn")
	if err := StartHandler(d)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	nonce := mustQuery(t, rec.Header().Get("Location")).Get("nonce")
	if nonce == "" {
		t.Fatal("authorize URL missing nonce")
	}
	e, ok := pending.Take("ctxn")
	if !ok || e.IdPNonce != nonce {
		t.Fatalf("pending IdPNonce = %q (ok=%v), want %q stamped", e.IdPNonce, ok, nonce)
	}
}

// TestStartHandler_OIDCSetsStateCookie asserts the SSO-start leg sets the
// HMAC-signed oidc_state HttpOnly cookie that binds the callback to this
// browser (login-CSRF defense-in-depth): the cookie value must verify back to
// the authctx, and the attributes (HttpOnly, Secure, Lax, callback-scoped path,
// 5-min max-age) must match the design so the browser only replays it on the
// return leg over TLS.
func TestStartHandler_OIDCSetsStateCookie(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	cfg := startOIDCCfg("https://idp.example.com/authorize", "cid", "https://app/cb")
	mock.ExpectQuery(idpRowQuery).WithArgs("idp-cookie").
		WillReturnRows(startIdPRow("idp-cookie", "oidc", "Okta", true, true, cfg))
	d, pending, _ := newStartDeps(t, mock)
	signer := newStateSigner([]byte("0123456789abcdef0123456789abcdef"))
	d.StateSigner = signer
	pending.Put("ctxck", liveEntry())

	c, rec := newStartCtx("idp-cookie", "ctxck")
	if err := StartHandler(d)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	var got *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == oidcStateCookieName {
			got = ck
		}
	}
	if got == nil {
		t.Fatal("oidc_state cookie not set on the start leg")
	}
	if !got.HttpOnly || !got.Secure || got.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie attributes wrong: HttpOnly=%v Secure=%v SameSite=%v",
			got.HttpOnly, got.Secure, got.SameSite)
	}
	if got.Path != oidcStateCookiePath {
		t.Fatalf("cookie path = %q, want %q", got.Path, oidcStateCookiePath)
	}
	if got.MaxAge != oidcStateCookieMaxAge {
		t.Fatalf("cookie max-age = %d, want %d", got.MaxAge, oidcStateCookieMaxAge)
	}
	bound, err := signer.verify(got.Value)
	if err != nil {
		t.Fatalf("cookie value does not verify: %v", err)
	}
	if bound != "ctxck" {
		t.Fatalf("cookie binds authctx %q, want ctxck", bound)
	}
}

// TestStartHandler_OIDCNilSignerNoCookie — when no signer is wired (test
// harness / CSPRNG-failure fallback) the start leg still redirects but sets no
// state cookie, rather than panicking.
func TestStartHandler_OIDCNilSignerNoCookie(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	cfg := startOIDCCfg("https://idp.example.com/authorize", "cid", "https://app/cb")
	mock.ExpectQuery(idpRowQuery).WithArgs("idp-nosig").
		WillReturnRows(startIdPRow("idp-nosig", "oidc", "Okta", true, true, cfg))
	d, pending, _ := newStartDeps(t, mock) // StateSigner left nil
	pending.Put("ctxns", liveEntry())

	c, rec := newStartCtx("idp-nosig", "ctxns")
	if err := StartHandler(d)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("got %d, want 302", rec.Code)
	}
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == oidcStateCookieName {
			t.Fatal("nil signer must not set a state cookie")
		}
	}
}

// TestStartHandler_OIDCAuthorizeParams covers the extra-authorize-params
// config: admin-supplied key/value pairs are appended to the IdP authorize
// URL, while reserved OAuth params can never be overridden.
func TestStartHandler_OIDCAuthorizeParams(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	cfg, _ := json.Marshal(map[string]any{
		"authorizeUrl": "https://idp.example.com/authorize",
		"clientId":     "client-xyz",
		"redirectUri":  "https://app/cb",
		"issuer":       "https://idp.example.com",
		"authorizeParams": []map[string]string{
			{"key": "organization", "value": "org_abc123"},
			{"key": "prompt", "value": "login"},
			{"key": "scope", "value": "HACKED"}, // reserved → must be ignored
			{"key": "", "value": "skipme"},      // empty key → skipped
		},
	})
	mock.ExpectQuery(idpRowQuery).WithArgs("idp-params").
		WillReturnRows(startIdPRow("idp-params", "oidc", "Auth0", true, true, cfg))
	d, pending, _ := newStartDeps(t, mock)
	pending.Put("ctxp", liveEntry())

	c, rec := newStartCtx("idp-params", "ctxp")
	if err := StartHandler(d)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("got %d, want 302 (body=%q)", rec.Code, rec.Body.String())
	}
	q := mustQuery(t, rec.Header().Get("Location"))
	if q.Get("organization") != "org_abc123" || q.Get("prompt") != "login" {
		t.Fatalf("extra params not appended: %v", q)
	}
	if q.Get("scope") != "openid profile email" {
		t.Fatalf("reserved param scope was overridden by config: %q", q.Get("scope"))
	}
	if q.Get("client_id") != "client-xyz" || q.Get("state") != "ctxp" {
		t.Fatalf("standard params lost: %v", q)
	}
}

func TestStartHandler(t *testing.T) {
	t.Run("oidc → 302 to the IdP authorization endpoint with state=authctx", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		cfg := startOIDCCfg("https://idp.example.com/authorize", "client-xyz", "https://app/cb")
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-oidc").
			WillReturnRows(startIdPRow("idp-oidc", "oidc", "Okta", true, true, cfg))
		d, pending, _ := newStartDeps(t, mock)
		pending.Put("ctx1", liveEntry())

		c, rec := newStartCtx("idp-oidc", "ctx1")
		if err := StartHandler(d)(c); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("got %d, want 302 (body=%q)", rec.Code, rec.Body.String())
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		if loc.Host != "idp.example.com" || loc.Path != "/authorize" {
			t.Fatalf("Location = %q, want the IdP authorize endpoint", rec.Header().Get("Location"))
		}
		q := loc.Query()
		if q.Get("client_id") != "client-xyz" || q.Get("state") != "ctx1" || q.Get("response_type") != "code" {
			t.Fatalf("authorize query missing/incorrect: %v", q)
		}
		// IdPID must be stamped so OIDCCallbackHandler loads the same config.
		if e, ok := pending.Take("ctx1"); !ok || e.IdPID != "idp-oidc" {
			t.Fatalf("pending IdPID = %q (ok=%v), want idp-oidc stamped", e.IdPID, ok)
		}
	})

	t.Run("saml → 200 auto-submitting AuthnRequest POST form", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		kp := newTestIDPKeypair(t)
		cfg := samlConfigJSON("https://idp.acme.test/metadata", "https://idp.acme.test/sso", kp.CertPEM)
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-saml").
			WillReturnRows(startIdPRow("idp-saml", "saml", "Acme", true, true, cfg))
		d, pending, reqs := newStartDeps(t, mock)
		pending.Put("ctxs", liveEntry())

		c, rec := newStartCtx("idp-saml", "ctxs")
		if err := StartHandler(d)(c); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d, want 200 (body=%q)", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "SAMLRequest") || !strings.Contains(body, "https://idp.acme.test/sso") {
			t.Fatalf("auto-POST form missing SAMLRequest/action: %q", body)
		}
		if !strings.Contains(body, "ctxs") {
			t.Errorf("RelayState authctx not embedded in form: %q", body)
		}
		// The AuthnRequest ID must be recorded for InResponseTo validation on ACS.
		if _, ok := reqs.Take("ctxs"); !ok {
			t.Error("start did not record an AuthnRequest ID for the authctx")
		}
		if e, ok := pending.Take("ctxs"); !ok || e.IdPID != "idp-saml" {
			t.Fatalf("pending IdPID = %q (ok=%v), want idp-saml stamped", e.IdPID, ok)
		}
	})

	t.Run("local IdP is not SSO-startable → bounce to login page", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-local").
			WillReturnRows(startIdPRow("idp-local", "local", "Nexus Local", true, false, []byte(`{}`)))
		d, pending, _ := newStartDeps(t, mock)
		pending.Put("ctxl", liveEntry())

		c, rec := newStartCtx("idp-local", "ctxl")
		_ = StartHandler(d)(c)
		assertStartBounce(t, rec, "ctxl")
	})

	t.Run("disabled IdP → bounce (in-flight start invalidated)", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		cfg := startOIDCCfg("https://idp/authorize", "cid", "https://app/cb")
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-off").
			WillReturnRows(startIdPRow("idp-off", "oidc", "Off", false, true, cfg))
		d, pending, _ := newStartDeps(t, mock)
		pending.Put("ctxo", liveEntry())

		c, rec := newStartCtx("idp-off", "ctxo")
		_ = StartHandler(d)(c)
		assertStartBounce(t, rec, "ctxo")
	})

	t.Run("IdP lookup error → bounce", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-x").WillReturnError(errors.New("db down"))
		d, pending, _ := newStartDeps(t, mock)
		pending.Put("ctxx", liveEntry())

		c, rec := newStartCtx("idp-x", "ctxx")
		_ = StartHandler(d)(c)
		assertStartBounce(t, rec, "ctxx")
	})

	t.Run("missing authctx → /login, no IdP lookup", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		d, _, _ := newStartDeps(t, mock)

		c, rec := newStartCtx("idp-any", "")
		_ = StartHandler(d)(c)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != loginPagePath {
			t.Fatalf("got %d Location=%q, want 302 %q", rec.Code, rec.Header().Get("Location"), loginPagePath)
		}
		// The handler must return before any IdP query.
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unexpected query ran: %v", err)
		}
	})

	t.Run("unknown authctx → /login", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		d, _, _ := newStartDeps(t, mock)

		c, rec := newStartCtx("idp-any", "ghost")
		_ = StartHandler(d)(c)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != loginPagePath {
			t.Fatalf("got %d Location=%q, want 302 %q", rec.Code, rec.Header().Get("Location"), loginPagePath)
		}
	})

	t.Run("oidc config incomplete (no authorize URL) → bounce", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		cfg := startOIDCCfg("", "cid", "https://app/cb")
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-thin").
			WillReturnRows(startIdPRow("idp-thin", "oidc", "Thin", true, true, cfg))
		d, pending, _ := newStartDeps(t, mock)
		pending.Put("ctxt", liveEntry())

		c, rec := newStartCtx("idp-thin", "ctxt")
		_ = StartHandler(d)(c)
		assertStartBounce(t, rec, "ctxt")
	})

	t.Run("oidc malformed authorize URL → bounce", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		cfg := startOIDCCfg("http://%zz", "cid", "https://app/cb") // invalid %-escape → url.Parse errors
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-badurl").
			WillReturnRows(startIdPRow("idp-badurl", "oidc", "Bad", true, true, cfg))
		d, pending, _ := newStartDeps(t, mock)
		pending.Put("ctxb", liveEntry())

		c, rec := newStartCtx("idp-badurl", "ctxb")
		_ = StartHandler(d)(c)
		assertStartBounce(t, rec, "ctxb")
	})

	t.Run("saml unusable config (bad cert) → bounce", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		cfg := samlConfigJSON("https://idp/metadata", "https://idp/sso", "not-a-valid-cert")
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-badcert").
			WillReturnRows(startIdPRow("idp-badcert", "saml", "Bad", true, true, cfg))
		d, pending, _ := newStartDeps(t, mock)
		pending.Put("ctxc", liveEntry())

		c, rec := newStartCtx("idp-badcert", "ctxc")
		_ = StartHandler(d)(c)
		assertStartBounce(t, rec, "ctxc")
	})

	// Race coverage for the SetIdPID-after-Take branch in each arm: Has() at the
	// top of StartHandler sees a live entry, the IdP query is delayed long enough
	// for a concurrent Take to drain the entry, and SetIdPID then returns false.
	t.Run("oidc SetIdPID race → /login", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		cfg := startOIDCCfg("https://idp/authorize", "cid", "https://app/cb")
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-race").
			WillReturnRows(startIdPRow("idp-race", "oidc", "Race", true, true, cfg)).
			WillDelayFor(100 * time.Millisecond)
		d, pending, _ := newStartDeps(t, mock)
		pending.Put("ctxr", liveEntry())
		go func() { time.Sleep(25 * time.Millisecond); pending.Take("ctxr") }()

		c, rec := newStartCtx("idp-race", "ctxr")
		_ = StartHandler(d)(c)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != loginPagePath {
			t.Fatalf("got %d Location=%q, want 302 %q (race)", rec.Code, rec.Header().Get("Location"), loginPagePath)
		}
	})

	t.Run("saml SetIdPID race → /login, no AuthnRequest recorded", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		kp := newTestIDPKeypair(t)
		cfg := samlConfigJSON("https://idp.acme.test/metadata", "https://idp.acme.test/sso", kp.CertPEM)
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-srace").
			WillReturnRows(startIdPRow("idp-srace", "saml", "Race", true, true, cfg)).
			WillDelayFor(100 * time.Millisecond)
		d, pending, reqs := newStartDeps(t, mock)
		pending.Put("ctxsr", liveEntry())
		go func() { time.Sleep(25 * time.Millisecond); pending.Take("ctxsr") }()

		c, rec := newStartCtx("idp-srace", "ctxsr")
		_ = StartHandler(d)(c)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != loginPagePath {
			t.Fatalf("got %d Location=%q, want 302 %q (saml race)", rec.Code, rec.Header().Get("Location"), loginPagePath)
		}
		// SetIdPID is checked before Requests.Put, so no AuthnRequest ID should leak.
		if _, ok := reqs.Take("ctxsr"); ok {
			t.Error("AuthnRequest ID recorded despite SetIdPID failure")
		}
	})
}
