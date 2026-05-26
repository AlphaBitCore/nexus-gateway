package wiring

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestInitMiddleware_DoesNotPanic(t *testing.T) {
	e := echo.New()
	// InitMiddleware must not panic and must register middleware.
	// After registration, a basic request must pass through without error.
	InitMiddleware(e, silentLogger())

	e.GET("/ping", func(c echo.Context) error {
		return c.String(http.StatusOK, "pong")
	})

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 from /ping after middleware registration, got %d", rec.Code)
	}
}
