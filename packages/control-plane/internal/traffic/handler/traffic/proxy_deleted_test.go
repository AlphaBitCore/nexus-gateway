package traffic

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// TestLegacyProxyMutatingRoutesDeleted is a regression test that locks in the
// removal of the pre-notifier proxy routes. All mutating /api/admin/proxy/*
// endpoints for killswitch, exemptions, and the alert/* config family have
// been replaced by /api/admin/compliance/* handlers that go through the Hub
// notifier (see Tasks 4-10 of the runtimeapi-slimming plan). If any of these
// legacy routes come back by mistake, this test fails.
//
// The test uses an in-process echo.Echo with a no-op IAM middleware, so it
// does not touch the DB, Hub, or compliance-proxy runtime. Echo returns 404
// for an unregistered path and 405 when the path exists but the method does
// not; either is acceptable evidence that the route is gone.
func TestLegacyProxyMutatingRoutesDeleted(t *testing.T) {
	h := New(Deps{
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
		Proxy:  ProxyConfig{},
	})

	e := echo.New()
	g := e.Group("/api/admin")

	// Allow every route through without consulting the IAM engine.
	noopIAM := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error { return next(c) }
		}
	}

	h.RegisterProxyRoutes(g, noopIAM)

	// The 14 (method, path) pairs that must no longer be served. The plan
	// referenced 15 entries, but PUT/DELETE /proxy/alerts/channels/:id was
	// already gone at audit time, so 14 is the correct count for the current
	// codebase.
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/admin/proxy/killswitch"},
		{http.MethodPost, "/api/admin/proxy/killswitch"},
		{http.MethodPost, "/api/admin/proxy/killswitch/force-close"},
		{http.MethodGet, "/api/admin/proxy/exemptions"},
		{http.MethodPost, "/api/admin/proxy/exemptions"},
		{http.MethodDelete, "/api/admin/proxy/exemptions/x"},
		{http.MethodGet, "/api/admin/proxy/alerts/webhook"},
		{http.MethodPut, "/api/admin/proxy/alerts/webhook"},
		{http.MethodGet, "/api/admin/proxy/alerts/thresholds"},
		{http.MethodPut, "/api/admin/proxy/alerts/thresholds"},
		{http.MethodGet, "/api/admin/proxy/alerts/channels"},
		{http.MethodPost, "/api/admin/proxy/alerts/channels"},
		{http.MethodGet, "/api/admin/proxy/compliance/killswitch-history"},
		{http.MethodPut, "/api/admin/proxy/reject-config"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("route %s %s: status = %d, want 404 or 405 (legacy route must be deleted)",
					tc.method, tc.path, rec.Code)
			}
		})
	}
}

// discardWriter is an io.Writer that drops everything. Used to silence the
// admin handler's slog output during route-registration tests.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
