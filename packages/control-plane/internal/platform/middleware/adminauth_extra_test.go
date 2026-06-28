package middleware_test

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// failingAPIKeyLookup forces FindByKeyHash to return an error so the
// 500 AUTH_SERVICE_ERROR branch in authenticateAPIKey is reachable.
type failingAPIKeyLookup struct{ err error }

func (f *failingAPIKeyLookup) FindByKeyHash(_ context.Context, _ string) (*store.APIKeyWithOwner, error) {
	return nil, f.err
}

// TestAdminAuthFromContext_EmptyContext locks the "no AdminAuth set →
// helper returns nil, not zero-value" contract. A regression that
// returned a zero AdminAuth would silently authenticate every
// downstream IAM check as the empty principal.
func TestAdminAuthFromContext_EmptyContext(t *testing.T) {
	t.Parallel()
	e := echo.New()
	e.HideBanner = true
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if got := middleware.AdminAuthFromContext(c); got != nil {
		t.Errorf("got=%+v, want nil on empty ctx", got)
	}
}

// TestWithAdminAuth_RoundTrip locks the test-helper export: WithAdminAuth
// + AdminAuthFromContext must round-trip the exact pointer so
// downstream handler tests that bypass the JWT/APIKey path attach
// principals consistent with what the real middleware writes.
func TestWithAdminAuth_RoundTrip(t *testing.T) {
	t.Parallel()
	e := echo.New()
	e.HideBanner = true
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	in := &auth.AdminAuth{KeyID: "usr-x", AuthPrincipalType: "admin_user"}
	middleware.WithAdminAuth(c, in)
	got := middleware.AdminAuthFromContext(c)
	if got != in {
		t.Errorf("got=%p want=%p (must be same pointer)", got, in)
	}
}

// TestAdminAuth_APIKey_NilLookupReturns401 covers the
// `cfg.APIKeyLookup == nil` branch: the API-key surface is wired but
// no lookup was configured (DB not initialised, test config). Must
// 401 INVALID_CREDENTIALS rather than 500 — the request itself is
// well-formed, just unverifiable.
func TestAdminAuth_APIKey_NilLookupReturns401(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t)
	e, _ := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier:  f.verifier,
		APIKeyLookup: nil, // explicit
		Logger:       slog.Default(),
	})
	rec := doRequest(e, map[string]string{"x-admin-key": "nxk_anything"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "INVALID_CREDENTIALS" {
		t.Errorf("error.code=%q want INVALID_CREDENTIALS", env.Error.Code)
	}
}

// TestAdminAuth_APIKey_LookupErrorReturns500 covers the loader-error
// branch: lookup explodes → 500 AUTH_SERVICE_ERROR. Must NOT fall
// through to a 401 "unknown key" verdict — the difference is critical
// for ops paging (transient infra vs auth issue).
func TestAdminAuth_APIKey_LookupErrorReturns500(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t)
	lookup := &failingAPIKeyLookup{err: errors.New("DB outage")}
	e, _ := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier:  f.verifier,
		APIKeyLookup: lookup,
		Logger:       slog.Default(),
	})
	rec := doRequest(e, map[string]string{"x-admin-key": "nxk_anything"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "AUTH_SERVICE_ERROR" {
		t.Errorf("error.code=%q want AUTH_SERVICE_ERROR", env.Error.Code)
	}
}

// TestAdminAuth_BearerEmptyTokenFallsThroughToMissing covers the
// extractBearer `tok == ""` branch — "Authorization: Bearer " with
// only whitespace must NOT be treated as a present credential; the
// middleware should treat it as missing entirely and 401 with
// AUTH_REQUIRED (no WWW-Authenticate, because no JWT was verified).
func TestAdminAuth_BearerEmptyTokenFallsThroughToMissing(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t)
	e, _ := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier: f.verifier,
		Logger:      slog.Default(),
	})
	rec := doRequest(e, map[string]string{"Authorization": "Bearer    "})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate=%q want empty on missing creds", got)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "AUTH_REQUIRED" {
		t.Errorf("error.code=%q want AUTH_REQUIRED", env.Error.Code)
	}
}

// TestAdminAuth_AuthorizationNonBearerFallsThroughToMissing covers
// the extractBearer `!HasPrefix` branch — an Authorization header
// that uses Basic/Digest/etc. must fall through to "no credentials"
// rather than being silently accepted or 500'd.
func TestAdminAuth_AuthorizationNonBearerFallsThroughToMissing(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t)
	e, _ := mountEcho(t, middleware.AdminAuthConfig{
		JWTVerifier: f.verifier,
		Logger:      slog.Default(),
	})
	rec := doRequest(e, map[string]string{"Authorization": "Basic Zm9vOmJhcg=="})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "AUTH_REQUIRED" {
		t.Errorf("error.code=%q want AUTH_REQUIRED", env.Error.Code)
	}
}
