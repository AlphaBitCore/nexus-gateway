package middleware_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"github.com/goccy/go-json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/hmackeyring"
)

// fakeRehasher records a lazy re-hash call so a test can assert the admin-key
// migration fires with the current-version hash + version, and that the
// matched (admission-time) hash is threaded through for the store's
// compare-and-swap guard.
type fakeRehasher struct {
	called                         bool
	id, hash, version, matchedHash string
	err                            error
}

func (f *fakeRehasher) UpdateKeyHashAndVersion(_ context.Context, id, keyHash, keyVersion, matchedHash string) error {
	f.called = true
	f.id, f.hash, f.version, f.matchedHash = id, keyHash, keyVersion, matchedHash
	return f.err
}

// fakeAPIKeyLookup is an in-memory AdminAPIKeyLookup for tests. When err is
// non-nil it is returned verbatim; otherwise keys maps keyHash → row (nil
// row signals "not found").
type fakeAPIKeyLookup struct {
	keys map[string]*store.APIKeyWithOwner
	err  error
}

func (f *fakeAPIKeyLookup) FindByKeyHash(_ context.Context, keyHash string) (*store.APIKeyWithOwner, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.keys == nil {
		return nil, nil
	}
	return f.keys[keyHash], nil
}

// authFixture bundles an RS256 signer, JWKS-serving httptest server, and a
// wired Verifier so individual tests can mint tokens and compose
// AdminAuthConfig values without repeating the setup dance.
type authFixture struct {
	priv     *rsa.PrivateKey
	jwksSrv  *httptest.Server
	verifier *jwtverifier.Verifier
}

const (
	testAdminIssuer   = "https://test.local"
	testAdminAudience = "cp-admin"
	testKID           = "k1"
)

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nBytes := priv.N.Bytes()
		eBig := big.NewInt(int64(priv.E))
		doc := map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"alg": "RS256",
					"use": "sig",
					"kid": testKID,
					"n":   base64.RawURLEncoding.EncodeToString(nBytes),
					"e":   base64.RawURLEncoding.EncodeToString(eBig.Bytes()),
				},
			},
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)

	v := jwtverifier.New(jwtverifier.Config{
		Issuer:   testAdminIssuer,
		JWKSURL:  srv.URL,
		Audience: testAdminAudience,
	})
	return &authFixture{priv: priv, jwksSrv: srv, verifier: v}
}

// signToken RS256-signs claims with the fixture key and kid=k1.
func (f *authFixture) signToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = testKID
	raw, err := tok.SignedString(f.priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return raw
}

// validAdminClaims returns a well-formed, currently-valid set of claims for a
// CP admin access token.
func validAdminClaims(subject, email string) jwt.MapClaims {
	now := time.Now().Unix()
	return jwt.MapClaims{
		"iss":       testAdminIssuer,
		"aud":       []string{testAdminAudience},
		"sub":       subject,
		"exp":       now + 3600,
		"iat":       now,
		"nbf":       now,
		"jti":       "jti-" + subject,
		"email":     email,
		"client_id": "cp-ui",
	}
}

// captureHandler writes the AdminAuth attached by the middleware into target
// (if non-nil) and returns 200. Mounted via adminHandler(e, ...) on /ping.
type captureHandler struct {
	target *auth.AdminAuth
}

func (ch *captureHandler) handler(c echo.Context) error {
	aa := middleware.AdminAuthFromContext(c)
	if aa != nil && ch.target != nil {
		*ch.target = *aa
	}
	return c.NoContent(http.StatusOK)
}

// mountEcho wires AdminAuth with the given config and a captureHandler on
// GET /ping. Returns the echo + the captured AdminAuth pointer so tests can
// assert downstream context propagation.
func mountEcho(t *testing.T, cfg middleware.AdminAuthConfig) (*echo.Echo, *auth.AdminAuth) {
	t.Helper()
	e := echo.New()
	e.HideBanner = true
	captured := &auth.AdminAuth{}
	ch := &captureHandler{target: captured}
	g := e.Group("", middleware.AdminAuth(cfg))
	g.GET("/ping", ch.handler)
	return e, captured
}

// doRequest issues a GET /ping with the given headers and returns the
// recorder.
func doRequest(e *echo.Echo, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestAdminAuth_PanicsWhenVerifierMissing(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when JWTVerifier is nil")
		}
	}()
	middleware.AdminAuth(middleware.AdminAuthConfig{
		JWTVerifier: nil,
		Logger:      slog.Default(),
	})
}

func TestAdminAuth_JWT_HappyPath(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	e, captured := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier: f.verifier,
		Logger:      slog.Default(),
	})

	raw := f.signToken(t, validAdminClaims("usr-1", "alice@nexus.ai"))
	rec := doRequest(e, map[string]string{
		"Authorization": "Bearer " + raw,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if captured.KeyID != "usr-1" {
		t.Errorf("KeyID=%q, want usr-1", captured.KeyID)
	}
	if captured.KeyName != "alice@nexus.ai" {
		t.Errorf("KeyName=%q, want alice@nexus.ai", captured.KeyName)
	}
	if captured.AuthPrincipalType != "admin_user" {
		t.Errorf("AuthPrincipalType=%q, want admin_user", captured.AuthPrincipalType)
	}
}

func TestAdminAuth_JWT_FallsBackToSubjectWhenEmailEmpty(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	e, captured := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier: f.verifier,
		Logger:      slog.Default(),
	})

	claims := validAdminClaims("usr-noemail", "")
	raw := f.signToken(t, claims)
	rec := doRequest(e, map[string]string{
		"Authorization": "Bearer " + raw,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if captured.KeyName != "usr-noemail" {
		t.Errorf("KeyName=%q, want fallback to subject", captured.KeyName)
	}
}

func TestAdminAuth_JWT_Rejects(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	e, _ := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier: f.verifier,
		Logger:      slog.Default(),
	})

	now := time.Now().Unix()
	cases := []struct {
		name   string
		claims jwt.MapClaims
	}{
		{
			name: "wrong_issuer",
			claims: jwt.MapClaims{
				"iss": "https://evil.example",
				"aud": []string{testAdminAudience},
				"sub": "usr-1", "exp": now + 3600, "iat": now, "nbf": now, "jti": "j1",
			},
		},
		{
			name: "wrong_audience",
			claims: jwt.MapClaims{
				"iss": testAdminIssuer,
				"aud": []string{"other-aud"},
				"sub": "usr-1", "exp": now + 3600, "iat": now, "nbf": now, "jti": "j1",
			},
		},
		{
			name: "expired",
			claims: jwt.MapClaims{
				"iss": testAdminIssuer,
				"aud": []string{testAdminAudience},
				"sub": "usr-1",
				"exp": now - 3*60*60, // 3h past expiry — well outside the 5-min skew
				"iat": now - 4*60*60,
				"nbf": now - 4*60*60,
				"jti": "j1",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := f.signToken(t, tc.claims)
			rec := doRequest(e, map[string]string{"Authorization": "Bearer " + raw})
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d want 401 body=%q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token"` {
				t.Errorf("WWW-Authenticate=%q, want Bearer error=\"invalid_token\"", got)
			}
		})
	}
}

// TestAdminAuth_JWT_RejectsEmptySubject locks the "no principal, no access"
// contract at the middleware layer. The shared verifier enforces this too,
// but the CP admin surface is the hottest consumer of that contract: a
// regression that let an empty `sub` through would silently attach an
// AdminAuth with KeyID="" to the request context and leak into every
// downstream IAM/audit path. Assert both the 401 and that the context key is
// never populated.
func TestAdminAuth_JWT_RejectsEmptySubject(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)

	// Capture whether AdminAuthFromContext was ever non-nil downstream.
	var sawAdminAuth bool
	e := echo.New()
	e.HideBanner = true
	spy := func(c echo.Context) error {
		if middleware.AdminAuthFromContext(c) != nil {
			sawAdminAuth = true
		}
		return c.NoContent(http.StatusOK)
	}
	g := e.Group("", middleware.AdminAuth(middleware.AdminAuthConfig{
		JWTVerifier: f.verifier,
		Logger:      slog.Default(),
	}))
	g.GET("/ping", spy)

	claims := validAdminClaims("", "alice@nexus.ai")
	raw := f.signToken(t, claims)
	rec := doRequest(e, map[string]string{"Authorization": "Bearer " + raw})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%q", rec.Code, rec.Body.String())
	}
	if sawAdminAuth {
		t.Error("AdminAuth context key was set despite empty subject — must not reach handler")
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token"` {
		t.Errorf("WWW-Authenticate=%q, want Bearer error=\"invalid_token\"", got)
	}
}

func TestAdminAuth_JWT_MalformedToken(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	e, _ := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier: f.verifier,
		Logger:      slog.Default(),
	})

	rec := doRequest(e, map[string]string{"Authorization": "Bearer not.a.jwt"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
}

func TestAdminAuth_APIKey_HappyPath_AsAPIKey(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	rawKey := "nxk_unit-test-key-1"
	hash := auth.HashAPIKey(rawKey)
	lookup := &fakeAPIKeyLookup{
		keys: map[string]*store.APIKeyWithOwner{
			hash: {
				ID:      "ak-1",
				Name:    "ci-runner",
				Enabled: true,
			},
		},
	}

	e, captured := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier:  f.verifier,
		APIKeyLookup: lookup,
		Logger:       slog.Default(),
	})

	rec := doRequest(e, map[string]string{"x-admin-key": rawKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if captured.KeyID != "ak-1" || captured.KeyName != "ci-runner" {
		t.Errorf("AdminAuth=%+v, want KeyID=ak-1 KeyName=ci-runner", *captured)
	}
	if captured.AuthPrincipalType != "api_key" {
		t.Errorf("AuthPrincipalType=%q, want api_key", captured.AuthPrincipalType)
	}
}

// TestAdminAuth_APIKey_LazyRehashOnOldVersion is the keyring-versioning core
// regression: a key whose stored hash was sealed under an OLDER keyring version
// still admits (try-all-versions, current-first) AND is lazy-migrated to the
// current version (UpdateKeyHashAndVersion called with the current-version hash).
// NOT parallel — it mutates the package-global injected keyring and restores the
// TestMain default before the parallel tests resume.
func TestAdminAuth_APIKey_LazyRehashOnOldVersion(t *testing.T) {
	f := newAuthFixture(t)
	kr, err := hmackeyring.New("v1:old-secret,*v2:current-secret")
	if err != nil {
		t.Fatalf("hmackeyring.New: %v", err)
	}
	if err := auth.InitHMACKeyring(kr); err != nil {
		t.Fatalf("InitHMACKeyring: %v", err)
	}
	t.Cleanup(func() {
		def, _ := hmackeyring.Single("test-hmac-secret")
		_ = auth.InitHMACKeyring(def)
	})

	rawKey := "nxk_rotate-me"
	// The row was sealed under v1 (the old version); current is v2.
	var oldHash, curHash string
	for _, v := range auth.HashAPIKeyVersions(rawKey) {
		switch v.Version {
		case "v1":
			oldHash = v.Hash
		case "v2":
			curHash = v.Hash
		}
	}
	if oldHash == curHash {
		t.Fatal("old and current version hashes must differ for this test to be meaningful")
	}
	lookup := &fakeAPIKeyLookup{keys: map[string]*store.APIKeyWithOwner{
		oldHash: {ID: "ak-old", Name: "rotated", Enabled: true},
	}}
	rehasher := &fakeRehasher{}

	e, captured := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier:    f.verifier,
		APIKeyLookup:   lookup,
		APIKeyRehasher: rehasher,
		Logger:         slog.Default(),
	})

	rec := doRequest(e, map[string]string{"x-admin-key": rawKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q (a key sealed under an old version must still admit)", rec.Code, rec.Body.String())
	}
	if captured.KeyID != "ak-old" {
		t.Errorf("admitted KeyID=%q, want ak-old", captured.KeyID)
	}
	if !rehasher.called {
		t.Fatal("expected lazy re-hash to fire on old-version admission")
	}
	if rehasher.id != "ak-old" || rehasher.hash != curHash || rehasher.version != "v2" {
		t.Errorf("rehash args = (%q,%q,%q), want (ak-old, <current-hash>, v2)", rehasher.id, rehasher.hash, rehasher.version)
	}
	// The admission-time hash must thread through for the store's
	// compare-and-swap: it is the only value that lets the UPDATE detect a
	// concurrent regenerate and skip instead of resurrecting the old key.
	if rehasher.matchedHash != oldHash {
		t.Errorf("rehash matchedHash = %q, want the v1 admission hash", rehasher.matchedHash)
	}
}

// TestAdminAuth_APIKey_NoRehashOnCurrentVersion: a key already on the current
// version admits on the first probe and is NOT re-hashed (no wasted UPDATE).
// Uses the TestMain default single-version (v1) keyring.
func TestAdminAuth_APIKey_NoRehashOnCurrentVersion(t *testing.T) {
	f := newAuthFixture(t)
	rawKey := "nxk_already-current"
	hash := auth.HashAPIKey(rawKey) // current version
	lookup := &fakeAPIKeyLookup{keys: map[string]*store.APIKeyWithOwner{
		hash: {ID: "ak-cur", Name: "current", Enabled: true},
	}}
	rehasher := &fakeRehasher{}

	e, _ := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier:    f.verifier,
		APIKeyLookup:   lookup,
		APIKeyRehasher: rehasher,
		Logger:         slog.Default(),
	})

	rec := doRequest(e, map[string]string{"x-admin-key": rawKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if rehasher.called {
		t.Error("a current-version key must NOT be lazy re-hashed")
	}
}

func TestAdminAuth_APIKey_DelegatesToOwner(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	rawKey := "nxk_unit-test-key-2"
	hash := auth.HashAPIKey(rawKey)
	ownerID := "usr-owner-1"
	ownerName := "Delegated Owner"
	ownerActive := true
	lookup := &fakeAPIKeyLookup{
		keys: map[string]*store.APIKeyWithOwner{
			hash: {
				ID:               "ak-2",
				Name:             "personal-token",
				Enabled:          true,
				OwnerUserID:      &ownerID,
				OwnerID:          &ownerID,
				OwnerDisplayName: &ownerName,
				OwnerEnabled:     &ownerActive,
			},
		},
	}

	e, captured := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier:  f.verifier,
		APIKeyLookup: lookup,
		Logger:       slog.Default(),
	})

	rec := doRequest(e, map[string]string{"x-admin-key": rawKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if captured.KeyID != ownerID {
		t.Errorf("KeyID=%q, want owner id %q", captured.KeyID, ownerID)
	}
	if captured.AuthPrincipalType != "admin_user" {
		t.Errorf("AuthPrincipalType=%q, want admin_user (delegated)", captured.AuthPrincipalType)
	}
	if captured.DelegatedFromAPIKeyID != "ak-2" {
		t.Errorf("DelegatedFromAPIKeyID=%q, want ak-2", captured.DelegatedFromAPIKeyID)
	}
}

func TestAdminAuth_APIKey_DisabledKey(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	rawKey := "nxk_disabled-key"
	hash := auth.HashAPIKey(rawKey)
	lookup := &fakeAPIKeyLookup{
		keys: map[string]*store.APIKeyWithOwner{
			hash: {ID: "ak-3", Name: "retired", Enabled: false},
		},
	}

	e, _ := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier:  f.verifier,
		APIKeyLookup: lookup,
		Logger:       slog.Default(),
	})

	rec := doRequest(e, map[string]string{"x-admin-key": rawKey})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
}

func TestAdminAuth_APIKey_UnknownKey(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	lookup := &fakeAPIKeyLookup{keys: map[string]*store.APIKeyWithOwner{}}

	e, _ := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier:  f.verifier,
		APIKeyLookup: lookup,
		Logger:       slog.Default(),
	})

	rec := doRequest(e, map[string]string{"x-admin-key": "nxk_nope"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
}

func TestAdminAuth_NoCredentials(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	e, _ := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier: f.verifier,
		Logger:      slog.Default(),
	})

	rec := doRequest(e, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
	// WWW-Authenticate MUST NOT be set for plain missing-auth: the middleware
	// only emits it on JWT-verify failures.
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate=%q, want empty on missing creds", got)
	}
}

func TestAdminAuth_BearerPreferredOverAPIKey(t *testing.T) {
	t.Parallel()

	// When both headers are present, AdminAuth must pick the JWT path. This
	// documents the explicit precedence rule and guards against a regression
	// where the header order sniff changes.
	f := newAuthFixture(t)
	rawKey := "nxk_ignored"
	hash := auth.HashAPIKey(rawKey)
	lookup := &fakeAPIKeyLookup{
		keys: map[string]*store.APIKeyWithOwner{
			hash: {ID: "ak-should-not-hit", Name: "unused", Enabled: true},
		},
	}

	e, captured := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier:  f.verifier,
		APIKeyLookup: lookup,
		Logger:       slog.Default(),
	})

	raw := f.signToken(t, validAdminClaims("usr-jwt", "jwt@nexus.ai"))
	rec := doRequest(e, map[string]string{
		"Authorization": "Bearer " + raw,
		"x-admin-key":   rawKey,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if captured.KeyID != "usr-jwt" {
		t.Errorf("KeyID=%q, want usr-jwt (JWT must win over API key)", captured.KeyID)
	}
	if captured.AuthPrincipalType != "admin_user" {
		t.Errorf("AuthPrincipalType=%q, want admin_user", captured.AuthPrincipalType)
	}
}
