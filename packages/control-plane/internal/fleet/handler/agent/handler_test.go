package agent

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
)

type stubHub struct{}

func (stubHub) NotifyConfigChange(_ context.Context, _ hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	return &hub.ConfigChangeResponse{Version: 1, ThingsNotified: 1, ThingsOnline: 1}, nil
}
func (stubHub) InvalidateConfig(_ context.Context, _, _ string) {}
func (stubHub) CreateEnrollmentToken(_ context.Context, _ hub.CreateEnrollmentTokenRequest) (*hub.CreateEnrollmentTokenResponse, error) {
	return &hub.CreateEnrollmentTokenResponse{}, nil
}
func (stubHub) ForceResyncAll(_ context.Context, _ string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (stubHub) RotateAgentCert(_ context.Context, _ string) (map[string]any, error) {
	return map[string]any{}, nil
}

func TestNew_NilLoggerDefaults(t *testing.T) {
	h := New(Deps{})
	if h.logger == nil {
		t.Error("logger should default to slog.Default()")
	}
}

func TestNew_PreservesLogger(t *testing.T) {
	l := slog.Default()
	h := New(Deps{Logger: l})
	if h.logger != l {
		t.Error("logger should be preserved")
	}
}

func TestErrJSON_Shape(t *testing.T) {
	env := errJSON("msg", "type", "CODE")
	inner, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("envelope: %+v", env)
	}
	if inner["message"] != "msg" || inner["type"] != "type" || inner["code"] != "CODE" {
		t.Errorf("inner: %+v", inner)
	}
}

func TestActorFromContext_NoAuth(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	a := actorFromContext(c)
	if a.UserID != "" || a.Name != "" {
		t.Errorf("expected zero-actor, got %+v", a)
	}
}

func TestDeref(t *testing.T) {
	if deref(nil) != "" {
		t.Error("nil → empty")
	}
	s := "x"
	if deref(&s) != "x" {
		t.Error("ptr → value")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if firstNonEmpty("", "", "b") != "b" {
		t.Error("first non-empty")
	}
	if firstNonEmpty() != "" {
		t.Error("empty")
	}
}

func TestParsePagination(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x?limit=200&offset=20", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 200 || pg.Offset != 20 {
		t.Errorf("%+v", pg)
	}
}

func TestParseRFC3339Flexible(t *testing.T) {
	if _, ok := parseRFC3339Flexible("2026-05-17T10:00:00Z"); !ok {
		t.Error("RFC3339 should parse")
	}
	if _, ok := parseRFC3339Flexible("garbage"); ok {
		t.Error("garbage should fail")
	}
}

func TestRegisterRoutes_Mounts(t *testing.T) {
	h := New(Deps{Hub: stubHub{}})
	e := echo.New()
	g := e.Group("/api/admin")
	noop := func(string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	noopDev := func(_, _ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterRoutes(g, noop, noopDev)
	if n := len(e.Routes()); n < 20 {
		t.Errorf("expected ≥20 routes, got %d", n)
	}
}

// TestParsePagination_ClampsAndDefaults covers the limit>1000 clamp,
// invalid string offset, and negative offset rejection.
func TestParsePagination_ClampsAndDefaults(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x?limit=5000&offset=-3", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 1000 {
		t.Errorf("limit should clamp to 1000: %d", pg.Limit)
	}
	if pg.Offset != 0 {
		t.Errorf("negative offset should default 0: %d", pg.Offset)
	}
}

func TestParsePagination_InvalidStrings(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x?limit=abc&offset=def", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 50 || pg.Offset != 0 {
		t.Errorf("invalid → defaults: %+v", pg)
	}
}

func TestSourceIP(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Real-IP", "1.2.3.4")
	c := e.NewContext(req, httptest.NewRecorder())
	if ip := sourceIP(c); ip == "" {
		t.Error("sourceIP should return non-empty for any request")
	}
}

func TestInternalServerError(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := internalServerError(c, "boom"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d", rec.Code)
	}
}

func TestParseTimeRange_StartAndEnd(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		"/x?startTime=2026-05-01T00:00:00Z&endTime=2026-05-17T00:00:00Z", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	s, end := parseTimeRange(c)
	if s == nil || end == nil {
		t.Errorf("times: %v %v", s, end)
	}
}

func TestParseTimeRange_FallbackToShortKeys(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		"/x?start=2026-05-01T00:00:00Z&end=2026-05-17T00:00:00Z", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	s, end := parseTimeRange(c)
	if s == nil || end == nil {
		t.Errorf("fallback short keys: %v %v", s, end)
	}
}

func TestParseTimeRange_BadStringsIgnored(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x?startTime=zzz&endTime=zzz", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	s, end := parseTimeRange(c)
	if s != nil || end != nil {
		t.Errorf("bad strings should be nil: %v %v", s, end)
	}
}

func TestParseRFC3339Flexible_Nano(t *testing.T) {
	if _, ok := parseRFC3339Flexible("2026-05-17T10:00:00.123456789Z"); !ok {
		t.Error("RFC3339Nano should parse")
	}
}

func TestCurrentUserID(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	if id := currentUserID(c); id != "" {
		t.Errorf("no auth → empty: %q", id)
	}
	// Now attach an admin auth and retest.
	c2, _ := echoCtxAdmin(httptest.NewRequest(http.MethodGet, "/x", nil),
		httptest.NewRecorder(), "u-1")
	if id := currentUserID(c2); id != "u-1" {
		t.Errorf("with auth → %q", id)
	}
}

func TestActorFromContext_WithAuth(t *testing.T) {
	c, _ := echoCtxAdmin(httptest.NewRequest(http.MethodGet, "/x", nil),
		httptest.NewRecorder(), "u-1")
	a := actorFromContext(c)
	if a.UserID != "u-1" || a.Name != "admin-u-1" {
		t.Errorf("actor: %+v", a)
	}
}

func TestParseAdminAuditParams(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		"/x?limit=10&offset=5&action=create&entityType=user&startTime=2026-05-01T00:00:00Z&endTime=2026-05-17T00:00:00Z", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	p := parseAdminAuditParams(c)
	if p.Limit != 10 || p.Offset != 5 || p.Action != "create" || p.EntityType != "user" {
		t.Errorf("params: %+v", p)
	}
	if p.StartTime == nil || p.EndTime == nil {
		t.Errorf("times missing: %+v", p)
	}
}
