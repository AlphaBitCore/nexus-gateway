package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// TestNexusRequestID_GeneratesWhenMissing asserts that when the inbound
// request omits x-nexus-request-id, the middleware mints a fresh non-empty
// UUID, surfaces it on the response header, AND stamps it into both the
// Echo context (NexusRequestIDFromContext) and the request context via
// nexushttp.RequestIDFromContext so downstream HTTP fans-out preserve it.
func TestNexusRequestID_GeneratesWhenMissing(t *testing.T) {
	t.Parallel()

	e := echo.New()
	e.HideBanner = true

	var seenEchoID string
	var seenCtxID string
	e.Use(middleware.NexusRequestID())
	e.GET("/", func(c echo.Context) error {
		seenEchoID = middleware.NexusRequestIDFromContext(c)
		seenCtxID = nexushttp.RequestIDFromContext(c.Request().Context())
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	respID := rec.Header().Get("X-Nexus-Request-Id")
	if respID == "" {
		t.Fatal("response header X-Nexus-Request-Id empty")
	}
	if seenEchoID != respID {
		t.Errorf("echo ctx id=%q, response header=%q", seenEchoID, respID)
	}
	if seenCtxID != respID {
		t.Errorf("request ctx id=%q, response header=%q (httpclient propagation broken)", seenCtxID, respID)
	}
	// UUIDs have at least 32 hex chars + 4 dashes.
	if len(respID) < 32 {
		t.Errorf("response id %q does not look like a UUID", respID)
	}
}

// TestNexusRequestID_PreservesInbound asserts the middleware honours the
// caller-supplied x-nexus-request-id header rather than overwriting it.
// This is load-bearing for cross-service request tracing — overwriting
// would break trace stitching with Hub.
func TestNexusRequestID_PreservesInbound(t *testing.T) {
	t.Parallel()

	const inbound = "req-from-edge-12345"
	e := echo.New()
	e.HideBanner = true

	var seen string
	e.Use(middleware.NexusRequestID())
	e.GET("/", func(c echo.Context) error {
		seen = middleware.NexusRequestIDFromContext(c)
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Nexus-Request-Id", inbound)
	e.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Nexus-Request-Id"); got != inbound {
		t.Errorf("response id=%q, want preserved inbound %q", got, inbound)
	}
	if seen != inbound {
		t.Errorf("ctx id=%q, want preserved inbound %q", seen, inbound)
	}
}

// TestNexusRequestIDFromContext_Empty asserts the helper returns "" when
// no middleware set the key — callers reading the ID must not see a stale
// type-asserted value.
func TestNexusRequestIDFromContext_Empty(t *testing.T) {
	t.Parallel()
	e := echo.New()
	e.HideBanner = true
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if got := middleware.NexusRequestIDFromContext(c); got != "" {
		t.Errorf("got=%q, want empty", got)
	}
}
