package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// readLogLines splits a JSON-handler buffer into one decoded record per
// non-empty line. Tests pin specific records by inspecting the slice.
func readLogLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line not JSON: %v: %q", err, line)
		}
		out = append(out, m)
	}
	return out
}

// TestAccessLog_LevelByStatus covers the level-selection branches:
// 2xx → Info, 4xx → Warn, 5xx → Error, /healthz and /metrics → Debug.
// Each branch must also stamp method, path, status, duration, requestId
// and remoteAddr.
func TestAccessLog_LevelByStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		path       string
		status     int
		wantLevel  string
		wantMethod string
	}{
		{"ok_info", "/ok", http.StatusOK, "INFO", http.MethodGet},
		{"client_warn", "/forbidden", http.StatusForbidden, "WARN", http.MethodGet},
		{"server_error", "/boom", http.StatusInternalServerError, "ERROR", http.MethodGet},
		{"healthz_debug", "/healthz", http.StatusOK, "DEBUG", http.MethodGet},
		{"metrics_debug", "/metrics", http.StatusOK, "DEBUG", http.MethodGet},
	}

	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			// Use Debug-level threshold so the /healthz line is captured.
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			e := echo.New()
			e.HideBanner = true
			// NexusRequestID before AccessLog so requestId is populated.
			e.Use(middleware.NexusRequestID(), middleware.AccessLog(logger))
			e.GET(tc.path, func(c echo.Context) error {
				return c.NoContent(tc.status)
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.wantMethod, tc.path+"?foo=bar", nil)
			e.ServeHTTP(rec, req)

			records := readLogLines(t, buf.String())
			if len(records) != 1 {
				t.Fatalf("got %d log records, want 1: %s", len(records), buf.String())
			}
			r := records[0]
			if r["level"] != tc.wantLevel {
				t.Errorf("level=%v, want %s", r["level"], tc.wantLevel)
			}
			if r["msg"] != "http request" {
				t.Errorf("msg=%v, want http request", r["msg"])
			}
			if r["method"] != tc.wantMethod {
				t.Errorf("method=%v, want %s", r["method"], tc.wantMethod)
			}
			if r["path"] != tc.path {
				t.Errorf("path=%v, want %s", r["path"], tc.path)
			}
			if r["query"] != "foo=bar" {
				t.Errorf("query=%v, want foo=bar", r["query"])
			}
			if int(r["status"].(float64)) != tc.status {
				t.Errorf("status=%v, want %d", r["status"], tc.status)
			}
			if _, ok := r["duration"]; !ok {
				t.Error("missing duration field")
			}
			if r["requestId"] == "" || r["requestId"] == nil {
				t.Error("requestId field empty")
			}
			if r["remoteAddr"] == "" || r["remoteAddr"] == nil {
				t.Error("remoteAddr field empty")
			}
		})
	}
}

// TestAccessLog_PropagatesHandlerError asserts the middleware returns
// the underlying handler error verbatim so Echo's error handler can
// still produce the canonical error envelope. A regression that
// swallowed err would break /api/admin/* error responses.
func TestAccessLog_PropagatesHandlerError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.AccessLog(logger))
	e.GET("/err", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusTeapot, "custom 418")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/err", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status=%d want 418 — Echo error handler must still fire", rec.Code)
	}
}
