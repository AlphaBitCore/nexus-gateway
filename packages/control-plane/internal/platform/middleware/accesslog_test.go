package middleware_test

import (
	"bytes"
	"github.com/goccy/go-json"
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

// TestAccessLog_RedactsTheOAuthCallbackCredential is the regression for the one
// inbound flow in this product that carries a credential in a query string.
//
// The IdP callback's handler reads `code` (the authorization code) and `state`
// (this deployment's single-use login handle) off the query, and the access log
// recorded RawQuery verbatim — so every login wrote a credential to the log. The
// 5xx arm is the sharp one: it logs at ERROR, which the diag MultiHandler
// absorbs whole and ships to the Hub as a persisted diag_event row, and the OIDC
// handler's 500 fires on a DB failure BEFORE the code exchange, so the value
// stored is an UNREDEEMED code.
//
// The assertion is on the raw log bytes, not on the parsed "query" field: a
// credential that leaked into any other attr would still be a leak.
func TestAccessLog_RedactsTheOAuthCallbackCredential(t *testing.T) {
	t.Parallel()

	const authzCode = "4-0AVGsecretcode"
	const loginHandle = "authctx-9f2b"

	for _, status := range []int{http.StatusFound, http.StatusInternalServerError} {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		e := echo.New()
		req := httptest.NewRequest(http.MethodGet,
			"/authserver/idp/okta/callback?code="+authzCode+"&state="+loginHandle+"&idp=okta", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		h := middleware.AccessLog(logger)(func(c echo.Context) error {
			return c.NoContent(status)
		})
		if err := h(c); err != nil {
			t.Fatalf("handler: %v", err)
		}

		raw := buf.String()
		if strings.Contains(raw, authzCode) {
			t.Fatalf("status %d: the authorization code was written to the access log: %s", status, raw)
		}
		if strings.Contains(raw, loginHandle) {
			t.Fatalf("status %d: the login handle was written to the access log: %s", status, raw)
		}
		// Redaction must not blind the operator to WHICH callback failed.
		if !strings.Contains(raw, "idp=okta") {
			t.Errorf("status %d: the non-sensitive query context was dropped: %s", status, raw)
		}
		recs := readLogLines(t, raw)
		if len(recs) != 1 {
			t.Fatalf("status %d: want 1 log record, got %d", status, len(recs))
		}
		// The marker arrives percent-encoded (%2A%2A%2A) because the redacted
		// query is re-encoded, matching the outbound redactor this shares its
		// parameter list with. Either spelling proves the value was replaced.
		if q, _ := recs[0]["query"].(string); !strings.Contains(q, "***") && !strings.Contains(q, "%2A%2A%2A") {
			t.Errorf("status %d: query field carries no redaction marker: %q", status, q)
		}
	}
}
