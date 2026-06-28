package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
)

// TestSetupRoutes_ConfigTokenAuthoritySplit is the regression test. It pins the
// closed invariant: the Hub config-WRITE authority
// (/api/hub) is gated by the dedicated HubConfigToken, NOT the fleet-wide
// InternalServiceToken; and the device-API group (/api/internal/things) is
// gated by the InternalServiceToken, NOT the HubConfigToken. Before the fix one
// shared token opened both, so a leak of any data-plane service's service token
// could inject fleet config. After the fix the two authorities are split: each
// token opens only its own group.
//
// The two cross-token negatives (#1, #4) are the decisive assertions — they hit
// the auth middleware only (no handler/Store access) and would have returned a
// past-auth status before the fix. The Recover middleware turns the
// nil-dependency handler panic on the positive (correct-token) paths into a 500
// so the assertion "got past auth" (status != 401 && != 403) is panic-safe.
//
// /api/v1/admin/alerts shares the identical ServiceAuth(cfg.HubConfigToken)
// gate as /api/hub (routes.go), so /api/hub conclusively exercises the
// HubConfigToken gating mechanism; the admin-alerts group is additionally
// covered end-to-end by the e2e unified_alerting suite.
func TestSetupRoutes_ConfigTokenAuthoritySplit(t *testing.T) {
	const (
		svcTok = "internal-service-token-value"
		cfgTok = "hub-config-token-value"
	)

	e := echo.New()
	// Turn a nil-dependency handler panic (Mgr is nil) into a 500 so the
	// past-auth assertions never crash the test binary.
	e.Use(echomw.Recover())
	SetupRoutes(RouteConfig{
		Echo:           e,
		ServiceToken:   svcTok,
		HubConfigToken: cfgTok,
	})

	do := func(method, path, bearer string) int {
		req := httptest.NewRequest(method, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}

	t.Run("config_write_rejects_fleet_service_token", func(t *testing.T) {
		// THE closed invariant: the fleet service token no longer opens the
		// /api/hub config-write surface. Was past-auth before the fix.
		if code := do(http.MethodGet, "/api/hub/things", svcTok); code != http.StatusForbidden {
			t.Fatalf("/api/hub with InternalServiceToken: want 403, got %d", code)
		}
	})

	t.Run("config_write_accepts_hub_config_token", func(t *testing.T) {
		// The dedicated config token passes the gate (handler then 500s on the
		// nil Mgr via Recover — that is past auth, which is what we assert).
		code := do(http.MethodGet, "/api/hub/things", cfgTok)
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Fatalf("/api/hub with HubConfigToken: want past-auth (not 401/403), got %d", code)
		}
	})

	t.Run("config_write_rejects_missing_token", func(t *testing.T) {
		if code := do(http.MethodGet, "/api/hub/things", ""); code != http.StatusUnauthorized {
			t.Fatalf("/api/hub with no token: want 401, got %d", code)
		}
	})

	t.Run("device_api_rejects_hub_config_token", func(t *testing.T) {
		// Least privilege the other direction: the config token is NOT accepted
		// as the service token on the device-API group. With no X-Thing-Id it
		// falls through to the device path and is rejected (401) — proving it
		// did not take the service-token fast path (which would be past-auth).
		if code := do(http.MethodGet, "/api/internal/things/config", cfgTok); code != http.StatusUnauthorized {
			t.Fatalf("/api/internal/things with HubConfigToken: want 401 (not service-token fast path), got %d", code)
		}
	})

	t.Run("device_api_accepts_service_token", func(t *testing.T) {
		// The internal service token still opens the device-API group (handler
		// then 500s on the nil Mgr via Recover — past auth).
		code := do(http.MethodGet, "/api/internal/things/config", svcTok)
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Fatalf("/api/internal/things with InternalServiceToken: want past-auth (not 401/403), got %d", code)
		}
	})
}
