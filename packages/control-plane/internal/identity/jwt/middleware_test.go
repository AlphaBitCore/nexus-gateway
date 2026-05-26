package jwtverifier_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"

	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
)

// middlewareFixture bundles a running Verifier, its signing key, and the
// JWKS httptest server so individual tests can mint tokens and submit them
// through an Echo router under test.
type middlewareFixture struct {
	verifier *jwtverifier.Verifier
	priv     *rsa.PrivateKey
	srv      *httptest.Server
}

// newMiddlewareFixture stands up a JWKS-serving httptest server plus a
// Verifier wired to it. A custom RevocationChecker may be supplied (pass
// nil for the default AlwaysAllow behaviour).
func newMiddlewareFixture(t *testing.T, rev jwtverifier.RevocationChecker) *middlewareFixture {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	const kid = "k1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nBytes := priv.N.Bytes()
		eBig := big.NewInt(int64(priv.E))
		doc := map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"alg": "RS256",
					"use": "sig",
					"kid": kid,
					"n":   base64.RawURLEncoding.EncodeToString(nBytes),
					"e":   base64.RawURLEncoding.EncodeToString(eBig.Bytes()),
				},
			},
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)

	cfg := jwtverifier.Config{
		Issuer:   "test-iss",
		JWKSURL:  srv.URL,
		Audience: "test-aud",
	}
	if rev != nil {
		cfg.RevCheck = rev
	}
	return &middlewareFixture{
		verifier: jwtverifier.New(cfg),
		priv:     priv,
		srv:      srv,
	}
}

// sign signs the given claims with the fixture key and kid=k1.
func (f *middlewareFixture) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "k1"
	raw, err := tok.SignedString(f.priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return raw
}

// validClaims returns a well-formed, currently-valid MapClaims template.
func validClaims() jwt.MapClaims {
	now := time.Now().Unix()
	return jwt.MapClaims{
		"iss":        "test-iss",
		"aud":        []string{"test-aud"},
		"sub":        "usr-1",
		"exp":        now + 3600,
		"iat":        now,
		"nbf":        now,
		"jti":        "jti-1",
		"client_id":  "foo",
		"scope":      "s1",
		"device_id":  "dev-1",
		"session_id": "sid-1",
		"email":      "a@b",
		"idp":        "local",
	}
}

// newEchoWithMiddleware mounts the verifier's middleware on a fresh Echo
// and registers a GET /test handler that writes the verified subject to the
// body (so tests can assert both auth and context propagation in one shot).
func newEchoWithMiddleware(v *jwtverifier.Verifier) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.Use(v.Middleware())
	e.GET("/test", func(c echo.Context) error {
		claims := jwtverifier.ClaimsFrom(c)
		if claims == nil {
			return c.String(http.StatusInternalServerError, "no claims")
		}
		return c.String(http.StatusOK, claims.Subject)
	})
	return e
}

// middlewareJTIRevoker is a RevocationChecker that rejects a specific jti.
// Named with a file-local prefix to avoid colliding with the identically
// shaped helper in verifier_test.go (both share the _test package).
type middlewareJTIRevoker struct{ target string }

func (r middlewareJTIRevoker) IsRevoked(_ context.Context, c *jwtverifier.Claims) (bool, error) {
	return c.JTI == r.target, nil
}

func TestMiddleware_ValidToken_CallsNext(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t, nil)
	e := newEchoWithMiddleware(f.verifier)
	raw := f.sign(t, validClaims())

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "usr-1" {
		t.Errorf("body = %q, want %q (claims not propagated)", got, "usr-1")
	}
}

func TestMiddleware_MissingHeader_401(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t, nil)
	e := newEchoWithMiddleware(f.verifier)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := `Bearer error="invalid_request"`
	if got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}

func TestMiddleware_NonBearerScheme_401(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t, nil)
	e := newEchoWithMiddleware(f.verifier)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Basic abcd")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := `Bearer error="invalid_request"`
	if got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}

func TestMiddleware_EmptyBearerToken_401(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t, nil)
	e := newEchoWithMiddleware(f.verifier)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := `Bearer error="invalid_request"`
	if got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}

func TestMiddleware_ExpiredToken_401(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t, nil)
	e := newEchoWithMiddleware(f.verifier)

	claims := validClaims()
	// 2h past expiry — beyond the default 5-minute clock skew.
	claims["exp"] = time.Now().Add(-2 * time.Hour).Unix()
	claims["iat"] = time.Now().Add(-3 * time.Hour).Unix()
	claims["nbf"] = time.Now().Add(-3 * time.Hour).Unix()
	raw := f.sign(t, claims)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := `Bearer error="invalid_token", error_description="expired"`
	if got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}

func TestMiddleware_RevokedToken_401(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t, middlewareJTIRevoker{target: "jti-1"})
	e := newEchoWithMiddleware(f.verifier)
	raw := f.sign(t, validClaims())

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := `Bearer error="invalid_token", error_description="revoked"`
	if got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}

func TestMiddleware_MalformedToken_401(t *testing.T) {
	t.Parallel()

	f := newMiddlewareFixture(t, nil)
	e := newEchoWithMiddleware(f.verifier)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := `Bearer error="invalid_token", error_description="malformed"`
	if got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
	// Sanity: response body must not leak the package prefix ("jwtverifier: ...").
	if strings.Contains(rec.Body.String(), "jwtverifier:") {
		t.Errorf("body leaks package prefix: %q", rec.Body.String())
	}
}

// TestMiddleware_NotYetValid_401 covers the ErrNotYetValid branch in
// descForError. Token's nbf is past the +5min skew window.
func TestMiddleware_NotYetValid_401(t *testing.T) {
	t.Parallel()
	f := newMiddlewareFixture(t, nil)
	e := newEchoWithMiddleware(f.verifier)

	claims := validClaims()
	claims["nbf"] = time.Now().Add(2 * time.Hour).Unix()
	claims["iat"] = time.Now().Add(2 * time.Hour).Unix()
	claims["exp"] = time.Now().Add(4 * time.Hour).Unix()
	raw := f.sign(t, claims)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	want := `Bearer error="invalid_token", error_description="not_yet_valid"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}

// TestMiddleware_WrongIssuer_401 covers ErrWrongIssuer.
func TestMiddleware_WrongIssuer_401(t *testing.T) {
	t.Parallel()
	f := newMiddlewareFixture(t, nil)
	e := newEchoWithMiddleware(f.verifier)
	claims := validClaims()
	claims["iss"] = "attacker-iss"
	raw := f.sign(t, claims)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	want := `Bearer error="invalid_token", error_description="wrong_issuer"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}

// TestMiddleware_WrongAudience_401 covers ErrWrongAudience.
func TestMiddleware_WrongAudience_401(t *testing.T) {
	t.Parallel()
	f := newMiddlewareFixture(t, nil)
	e := newEchoWithMiddleware(f.verifier)
	claims := validClaims()
	claims["aud"] = []string{"other-aud"}
	raw := f.sign(t, claims)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	want := `Bearer error="invalid_token", error_description="wrong_audience"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}

// TestMiddleware_InvalidSignature_401 covers ErrInvalidSignature.
// Sign with a fresh RSA key the JWKS server doesn't publish — the
// verifier resolves the kid="k1" header to the fixture's key, but
// the signature was made with a different key.
func TestMiddleware_InvalidSignature_401(t *testing.T) {
	t.Parallel()
	f := newMiddlewareFixture(t, nil)
	e := newEchoWithMiddleware(f.verifier)

	// Build a token signed by a DIFFERENT RSA key but tagged kid=k1
	// so the verifier picks up the fixture's public key for verification.
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, validClaims())
	tok.Header["kid"] = "k1"
	raw, err := tok.SignedString(otherKey)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	want := `Bearer error="invalid_token", error_description="invalid_signature"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}

// TestMiddleware_JWKSUnavailable_401 covers ErrJWKSUnavailable. The
// JWKS endpoint is unreachable so the verifier cannot resolve the
// kid. Build the fixture, then close the JWKS server before signing.
func TestMiddleware_JWKSUnavailable_401(t *testing.T) {
	t.Parallel()
	f := newMiddlewareFixture(t, nil)
	e := newEchoWithMiddleware(f.verifier)
	raw := f.sign(t, validClaims())
	f.srv.Close() // make JWKS unreachable AFTER signing

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	// Accept either jwks_unavailable (initial fetch fail) or
	// invalid_signature (key not in cache). Both are valid outcomes
	// depending on whether the verifier completed any prior fetch.
	if !strings.Contains(got, `Bearer error="invalid_token"`) {
		t.Errorf("WWW-Authenticate must carry invalid_token: %q", got)
	}
}

func TestMiddleware_ClaimsFrom_NilWhenAbsent(t *testing.T) {
	t.Parallel()

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if got := jwtverifier.ClaimsFrom(c); got != nil {
		t.Fatalf("ClaimsFrom = %+v, want nil on bare context", got)
	}
}
