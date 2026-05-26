package wiring

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/infrastructure/infra"
)

func TestNewReadinessHandler_ReturnsNonNilChecker(t *testing.T) {
	rc := NewReadinessHandler(infra.Deps{
		Logger: silentLogger(),
	})
	if rc == nil {
		t.Error("expected non-nil ReadinessChecker")
	}
}

func TestInitReadiness_RegistersBothProbes(t *testing.T) {
	e := echo.New()
	rc := NewReadinessHandler(infra.Deps{
		Logger: silentLogger(),
	})
	InitReadiness(e, rc)

	for _, path := range []string{"/ready", "/api/admin/ready"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		// Without a real DB/Hub the handler returns some non-404 status.
		if rec.Code == http.StatusNotFound {
			t.Errorf("probe %s not registered (got 404)", path)
		}
	}
}
