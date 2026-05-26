package wiring

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInitEcho_ReturnsConfiguredEcho(t *testing.T) {
	e := InitEcho(silentLogger())
	if e == nil {
		t.Fatal("expected non-nil echo.Echo instance")
	}

	// /healthz must return 200 with status=ok.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz: expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	if body == "" {
		t.Error("/healthz: expected non-empty JSON body")
	}
}

func TestInitEcho_MetricsEndpointMounted(t *testing.T) {
	e := InitEcho(silentLogger())

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	// Prometheus handler returns 200 with text/plain content.
	if rec.Code != http.StatusOK {
		t.Errorf("/metrics: expected 200, got %d", rec.Code)
	}
}
