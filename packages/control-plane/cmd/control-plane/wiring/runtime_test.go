package wiring

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
)

func TestInitRuntimeIntrospect_RegistersDebugEndpoint(t *testing.T) {
	e := echo.New()
	cfg := &config.Config{}
	cfg.Server.Port = 3001
	cfg.Auth.InternalServiceToken = "test-token"

	recorder := runtimeintrospect.NewKeyStateRecorder()
	InitRuntimeIntrospect(e, cfg, nil, "cp-test-1", "v0.0.1", recorder, silentLogger())

	// /debug/runtime is registered — a request without the token should return
	// 401 or 403 (not 404), proving the route was mounted.
	req := httptest.NewRequest(http.MethodGet, "/debug/runtime", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Errorf("/debug/runtime not registered (got 404)")
	}
}

func TestInitRuntimeIntrospect_WithNilDB_DoesNotPanic(t *testing.T) {
	e := echo.New()
	cfg := &config.Config{}
	cfg.Auth.InternalServiceToken = "tok"
	recorder := runtimeintrospect.NewKeyStateRecorder()

	// Must not panic when db is nil.
	InitRuntimeIntrospect(e, cfg, nil, "cp-1", "v1", recorder, silentLogger())
}

func TestInitRuntimeIntrospect_WithToken_Returns200(t *testing.T) {
	e := echo.New()
	cfg := &config.Config{}
	cfg.Server.Port = 3001
	cfg.Auth.InternalServiceToken = "secret"

	recorder := runtimeintrospect.NewKeyStateRecorder()
	InitRuntimeIntrospect(e, cfg, nil, "cp-test", "v0.1.0", recorder, silentLogger())

	req := httptest.NewRequest(http.MethodGet, "/debug/runtime", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with valid token, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}
