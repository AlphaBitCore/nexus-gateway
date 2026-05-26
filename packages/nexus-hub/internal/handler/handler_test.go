// Tests for the handler package: pure helpers + middleware.
package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/hubstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

func TestParseIntDefault(t *testing.T) {
	cases := []struct {
		s    string
		def  int
		want int
	}{
		{"", 10, 10},
		{"abc", 10, 10},
		{"-5", 10, 10}, // < 1 → default
		{"0", 10, 10},  // < 1 → default
		{"1", 10, 1},
		{"50", 10, 50},
		{"999", 5, 999},
	}
	for _, tc := range cases {
		got := parseIntDefault(tc.s, tc.def)
		if got != tc.want {
			t.Errorf("parseIntDefault(%q, %d) = %d, want %d", tc.s, tc.def, got, tc.want)
		}
	}
}

func TestClamp(t *testing.T) {
	cases := []struct {
		v, min, max, want int
	}{
		{5, 1, 10, 5},   // in range
		{0, 1, 10, 1},   // below min
		{20, 1, 10, 10}, // above max
		{1, 1, 10, 1},   // at min boundary
		{10, 1, 10, 10}, // at max boundary
	}
	for _, tc := range cases {
		got := clamp(tc.v, tc.min, tc.max)
		if got != tc.want {
			t.Errorf("clamp(%d, %d, %d) = %d, want %d", tc.v, tc.min, tc.max, got, tc.want)
		}
	}
}

func TestParseTimeOrNil(t *testing.T) {
	// Empty string → nil.
	if got := parseTimeOrNil(""); got != nil {
		t.Errorf("parseTimeOrNil(%q) = %v, want nil", "", got)
	}

	// Invalid string → nil.
	if got := parseTimeOrNil("not-a-time"); got != nil {
		t.Errorf("parseTimeOrNil(%q) = %v, want nil", "not-a-time", got)
	}

	// Valid RFC3339 → parsed value.
	const ts = "2024-01-15T10:30:00Z"
	got := parseTimeOrNil(ts)
	if got == nil {
		t.Fatalf("parseTimeOrNil(%q) = nil, want non-nil", ts)
	}
	want := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseTimeOrNil(%q) = %v, want %v", ts, *got, want)
	}
}

func newEchoContext(t *testing.T) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func TestHandleErr_NotFound(t *testing.T) {
	c, rec := newEchoContext(t)
	err := handleErr(c, hubstore.ErrNotFound)
	if err != nil {
		t.Fatalf("handleErr returned unexpected error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleErr_OtherError(t *testing.T) {
	c, rec := newEchoContext(t)
	err := handleErr(c, errors.New("db error"))
	if err != nil {
		t.Fatalf("handleErr returned unexpected error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// error helpers (badRequest, unauthorized, forbidden, notFound, internalError,
// serviceUnavailable)

func TestErrorHelpers(t *testing.T) {
	tests := []struct {
		name   string
		fn     func(echo.Context, string) error
		status int
	}{
		{"badRequest", badRequest, http.StatusBadRequest},
		{"unauthorized", unauthorized, http.StatusUnauthorized},
		{"forbidden", forbidden, http.StatusForbidden},
		{"notFound", notFound, http.StatusNotFound},
		{"internalError", internalError, http.StatusInternalServerError},
		{"serviceUnavailable", serviceUnavailable, http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newEchoContext(t)
			if err := tc.fn(c, "test message"); err != nil {
				t.Fatalf("%s returned unexpected error: %v", tc.name, err)
			}
			if rec.Code != tc.status {
				t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.status)
			}
		})
	}
}

// ServiceAuth middleware

func TestServiceAuth_MissingHeader(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	mw := ServiceAuth("secret")
	h := mw(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	_ = h(c)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (missing header)", rec.Code)
	}
}

func TestServiceAuth_InvalidToken(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	mw := ServiceAuth("secret")
	h := mw(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	_ = h(c)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (invalid token)", rec.Code)
	}
}

func TestServiceAuth_ValidToken(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer mysecret")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	mw := ServiceAuth("mysecret")
	called := false
	h := mw(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})
	_ = h(c)

	if !called {
		t.Error("handler not called with valid token")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// NexusRequestID middleware

func TestNexusRequestID_GeneratesID(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	mw := NexusRequestID()
	var capturedID string
	h := mw(func(c echo.Context) error {
		capturedID = NexusRequestIDFromContext(c)
		return c.String(http.StatusOK, "ok")
	})
	_ = h(c)

	if capturedID == "" {
		t.Error("request ID not set in context")
	}
	if rec.Header().Get(nexusRequestIDHeader) == "" {
		t.Error("x-nexus-request-id header not set on response")
	}
}

func TestNexusRequestID_ReusesExistingID(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	const existingID = "my-existing-request-id"
	req.Header.Set(nexusRequestIDHeader, existingID)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	mw := NexusRequestID()
	var capturedID string
	h := mw(func(c echo.Context) error {
		capturedID = NexusRequestIDFromContext(c)
		return c.String(http.StatusOK, "ok")
	})
	_ = h(c)

	if capturedID != existingID {
		t.Errorf("request ID = %q, want %q (should reuse existing)", capturedID, existingID)
	}
}

func TestNexusRequestIDFromContext_NoID(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	got := NexusRequestIDFromContext(c)
	if got != "" {
		t.Errorf("expected empty string when no ID set, got %q", got)
	}
}

func TestNexusRequestIDFromContext_WrongType(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("nexusRequestId", 12345) // wrong type — should gracefully return ""

	got := NexusRequestIDFromContext(c)
	if got != "" {
		t.Errorf("expected empty string for wrong type, got %q", got)
	}
}

// handleErr with wrapped ErrNotFound (errors.Is chain)

func TestHandleErr_WrappedNotFound(t *testing.T) {
	c, rec := newEchoContext(t)
	wrapped := &wrappedErr{inner: store.ErrNotFound}
	err := handleErr(c, wrapped)
	if err != nil {
		t.Fatalf("handleErr returned unexpected error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for wrapped ErrNotFound", rec.Code)
	}
}

// wrappedErr wraps an error for the errors.Is chain test.
type wrappedErr struct{ inner error }

func (e *wrappedErr) Error() string { return "wrapped: " + e.inner.Error() }
func (e *wrappedErr) Unwrap() error { return e.inner }

// SetupRoutes — minimal wiring coverage

// TestSetupRoutes_MinimalNilFields exercises SetupRoutes with all optional
// fields nil (Store=nil, AgentCA=nil, CpURL="", Raiser=nil, AlertStore=nil,
// WSServer=nil, OpsDiagPool=nil, SpillBackend=default).
// The function should complete without panic and return nil enrollAPI.
func TestSetupRoutes_MinimalNilFields(t *testing.T) {
	e := echo.New()
	cfg := RouteConfig{
		Echo:         e,
		ServiceToken: "token",
	}
	enrollAPI := SetupRoutes(cfg)
	if enrollAPI != nil {
		t.Errorf("expected nil enrollAPI when AgentCA is nil, got %v", enrollAPI)
	}
}

// TestSetupRoutes_WithStore exercises the cfg.Store != nil branch (registers
// /things/:id/runtime when Store is non-nil). Pool() returns nil when using
// NewWithPgxPool so bridge.DB stays nil (non-nil pool guard).
func TestSetupRoutes_WithStore(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s := store.NewWithPgxPool(mock)

	e := echo.New()
	cfg := RouteConfig{
		Echo:         e,
		ServiceToken: "token",
		Store:        s,
	}
	enrollAPI := SetupRoutes(cfg)
	if enrollAPI != nil {
		t.Errorf("expected nil enrollAPI when AgentCA is nil")
	}
}

// TestSetupRoutes_WithCpURL exercises the cfg.CpURL != "" branch which
// registers /api/public/agent-bootstrap. DBPool is nil (safe default).
func TestSetupRoutes_WithCpURL(t *testing.T) {
	e := echo.New()
	cfg := RouteConfig{
		Echo:         e,
		ServiceToken: "token",
		CpURL:        "https://cp.example.com",
	}
	enrollAPI := SetupRoutes(cfg)
	if enrollAPI != nil {
		t.Errorf("expected nil enrollAPI when AgentCA is nil")
	}
}

// TestSetupRoutes_LocalfsSpillBackend exercises the SpillBackend=="localfs"
// branch which registers /api/internal/spill/blob/:token.
func TestSetupRoutes_LocalfsSpillBackend(t *testing.T) {
	e := echo.New()
	cfg := RouteConfig{
		Echo:         e,
		ServiceToken: "token",
		SpillBackend: "localfs",
	}
	enrollAPI := SetupRoutes(cfg)
	if enrollAPI != nil {
		t.Errorf("expected nil enrollAPI when AgentCA is nil")
	}
}

// TestSetupRoutes_WithWSServer exercises the cfg.WSServer != nil branch.
// WSServer is an interface; a nil *ws.Server would still be != nil (typed nil),
// so we skip that branch here — just verify the nil WSServer path compiles
// and completes cleanly.
func TestSetupRoutes_WithOpsDiagPoolNil(t *testing.T) {
	e := echo.New()
	cfg := RouteConfig{
		Echo:         e,
		ServiceToken: "token",
		OpsDiagPool:  nil, // nil — diag endpoint NOT registered
	}
	enrollAPI := SetupRoutes(cfg)
	if enrollAPI != nil {
		t.Errorf("expected nil enrollAPI when AgentCA is nil")
	}
}

// TestSetupRoutes_WithRaiser exercises the cfg.Raiser != nil branch, which
// registers /api/v1/alerts/raise and /api/v1/alerts/resolve endpoints.
func TestSetupRoutes_WithRaiser(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	alertStore := alerting.NewStoreWithPgxPool(mock)
	dispatcher := alerting.NewDispatcher(alertStore, &minSenderRegistry{}, slog.Default())
	raiser := alerting.NewRaiserWithPool(mock, alertStore, dispatcher, slog.Default())

	e := echo.New()
	cfg := RouteConfig{
		Echo:         e,
		ServiceToken: "token",
		Raiser:       raiser,
	}
	enrollAPI := SetupRoutes(cfg)
	if enrollAPI != nil {
		t.Errorf("expected nil enrollAPI when AgentCA is nil")
	}
}

// TestSetupRoutes_WithAlertStore exercises the cfg.AlertStore/Rules/Senders
// branch, registering the admin alerting API routes.
func TestSetupRoutes_WithAlertStore(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	alertStore := alerting.NewStoreWithPgxPool(mock)
	ruleReg := &minRuleRegistry{}
	senderReg := &minSenderRegistry{}

	e := echo.New()
	cfg := RouteConfig{
		Echo:         e,
		ServiceToken: "token",
		AlertStore:   alertStore,
		AlertRules:   ruleReg,
		AlertSenders: senderReg,
	}
	enrollAPI := SetupRoutes(cfg)
	if enrollAPI != nil {
		t.Errorf("expected nil enrollAPI when AgentCA is nil")
	}
}

// minRuleRegistry is a minimal alerting.RuleRegistry for route wiring tests.
type minRuleRegistry struct{}

func (r *minRuleRegistry) Lookup(_ string) (alerting.RuleDefault, bool) {
	return alerting.RuleDefault{}, false
}

// minSenderRegistry is a minimal alerting.SenderRegistry for route wiring tests.
type minSenderRegistry struct{}

func (r *minSenderRegistry) Get(_ string) (alerting.Sender, error) {
	return nil, errors.New("no sender")
}

// minDispatcher satisfies alerting.Dispatcher — compile-time guard only.
var _ alerting.Dispatcher = (*minDispatcher)(nil)

type minDispatcher struct{}

func (d *minDispatcher) Dispatch(_ context.Context, _ alerting.Alert) {}

// TestSetupRoutes_WithAgentCA exercises the cfg.AgentCA != nil branch which
// creates an EnrollmentAPI, calls Init(), and registers /api/internal/things/enroll.
func TestSetupRoutes_WithAgentCA(t *testing.T) {
	dir := t.TempDir()
	ca, err := agentca.New(dir, slog.Default())
	if err != nil {
		t.Fatalf("agentca.New: %v", err)
	}

	e := echo.New()
	cfg := RouteConfig{
		Echo:         e,
		ServiceToken: "token",
		AgentCA:      ca,
	}
	enrollAPI := SetupRoutes(cfg)
	if enrollAPI == nil {
		t.Error("expected non-nil enrollAPI when AgentCA is set")
	}
	// Clean up the background goroutine started by Init().
	if enrollAPI != nil {
		enrollAPI.Close()
	}
}
