package middleware_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// TestRecovery_PanicReturns500AndLogs covers the happy panic-recovery
// path: a handler that panics must (a) NOT crash the process, (b) return
// 500 with a JSON body that carries the panic value, (c) emit a slog
// Error record carrying method/path/stack/requestId so on-call can
// triage. The third observation is what makes Recovery worth the cost
// over Echo's built-in HTTPErrorHandler — without it the operator gets
// no stack.
func TestRecovery_PanicReturns500AndLogs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	e := echo.New()
	e.HideBanner = true
	// Recovery must run AFTER NexusRequestID so the requestId field
	// in the slog record has a value.
	e.Use(middleware.NexusRequestID(), middleware.Recovery(logger))
	e.GET("/boom", func(c echo.Context) error {
		panic("specific boom message")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v: %q", err, rec.Body.String())
	}
	if !strings.Contains(body["error"], "specific boom message") {
		t.Errorf("body[error]=%q, want it to carry the panic value", body["error"])
	}

	// slog record must have been emitted with the panic + stack.
	logged := buf.String()
	if !strings.Contains(logged, "panic recovered") {
		t.Errorf("slog output missing 'panic recovered': %s", logged)
	}
	if !strings.Contains(logged, "specific boom message") {
		t.Errorf("slog output missing panic value: %s", logged)
	}
	if !strings.Contains(logged, `"method":"GET"`) {
		t.Errorf("slog output missing method field: %s", logged)
	}
	if !strings.Contains(logged, `"path":"/boom"`) {
		t.Errorf("slog output missing path field: %s", logged)
	}
	if !strings.Contains(logged, `"stack"`) {
		t.Errorf("slog output missing stack field: %s", logged)
	}
}

// TestRecovery_HappyPath asserts that when the handler does not panic
// the middleware is a no-op pass-through (status from handler kept,
// no log emitted under the "panic recovered" message).
func TestRecovery_HappyPath(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Recovery(logger))
	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body=%q want %q", rec.Body.String(), "ok")
	}
	if strings.Contains(buf.String(), "panic recovered") {
		t.Errorf("Recovery emitted a panic log on a non-panicking request: %s", buf.String())
	}
}

// TestRecovery_PanicWithErrorValue covers slog.Any("panic", rec)
// handling when the recovered value is an error type rather than a
// string — the json formatter must still serialize it cleanly.
func TestRecovery_PanicWithErrorValue(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Recovery(logger))
	e.GET("/", func(c echo.Context) error {
		panic(errors.New("typed error panic"))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "typed error panic") {
		t.Errorf("body=%q missing panic error message", rec.Body.String())
	}
}
