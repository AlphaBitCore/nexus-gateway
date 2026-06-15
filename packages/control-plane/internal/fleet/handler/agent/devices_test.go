package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
)

// noopIAM is the trivial pass-through middleware used to mount the
// route group without a real IAM gate.
func noopIAM() func(string) echo.MiddlewareFunc {
	return func(string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
}

func noopIAMDev() func(string, string) echo.MiddlewareFunc {
	return func(_, _ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
}

// TestRegisterAdminAgentDeviceRoutes_MountsAll locks the route map so a
// rename/remove without updating IAM stays visible.
func TestRegisterAdminAgentDeviceRoutes_MountsAll(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	g := e.Group("/api/admin")
	h.RegisterAdminAgentDeviceRoutes(g, noopIAM(), noopIAMDev())
	want := []string{
		"GET /api/admin/agent-devices",
		"GET /api/admin/agent-devices/health",
		"GET /api/admin/agent-devices/:id",
		"GET /api/admin/agent-devices/:id/events",
		"GET /api/admin/agent-devices/:id/assignments",
		"POST /api/admin/agent-devices/enroll-token",
		"POST /api/admin/agent-devices/:id/unenroll",
		"POST /api/admin/agent-devices/:id/force-refresh",
		"PUT /api/admin/agent-devices/:id/tags",
	}
	seen := map[string]bool{}
	for _, r := range e.Routes() {
		seen[r.Method+" "+r.Path] = true
	}
	for _, k := range want {
		if !seen[k] {
			t.Errorf("missing route: %s", k)
		}
	}
}

func TestListAgentDevices_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM`).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	row := append(makeAgentDeviceRow(now), 7)
	mock.ExpectQuery(`event_count`).WithArgs(10, 0).
		WillReturnRows(pgxmock.NewRows(agentDeviceListCols).AddRow(row...))

	e := echo.New()
	e.GET("/agent-devices", h.ListAgentDevices)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices?limit=10&offset=0", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if v, _ := body["total"].(float64); v != 1 {
		t.Errorf("total=%v", body["total"])
	}
}

func TestListAgentDevices_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-devices", h.ListAgentDevices)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListAgentDevices_ScanError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM`).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`event_count`).WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("bad"))

	e := echo.New()
	e.GET("/agent-devices", h.ListAgentDevices)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListAgentDevices_WithFilters(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM`).
		WithArgs("%pro%", "online", "darwin").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	row := append(makeAgentDeviceRow(now), 7)
	mock.ExpectQuery(`event_count`).
		WithArgs("%pro%", "online", "darwin", 50, 0).
		WillReturnRows(pgxmock.NewRows(agentDeviceListCols).AddRow(row...))

	e := echo.New()
	e.GET("/agent-devices", h.ListAgentDevices)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices?q=pro&status=online&os=darwin", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetAgentDevice_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))

	e := echo.New()
	e.GET("/agent-devices/:id", h.GetAgentDevice)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetAgentDevice_NotFound(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	e.GET("/agent-devices/:id", h.GetAgentDevice)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/missing", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetAgentDevice_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("x").WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-devices/:id", h.GetAgentDevice)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/x", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListDeviceEvents_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`FROM traffic_event e\s+LEFT JOIN thing`).
		WithArgs("agent-1", 50, 0).
		WillReturnRows(pgxmock.NewRows(agentTrafficEventCols).AddRow(makeAgentTrafficEventRow(now)...))

	e := echo.New()
	e.GET("/agent-devices/:id/events", h.ListDeviceEvents)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1/events", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListDeviceEvents_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).WithArgs("agent-1").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-devices/:id/events", h.ListDeviceEvents)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1/events", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGenerateEnrollToken_NoAuth(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	e.POST("/agent-devices/enroll-token", h.GenerateEnrollToken)
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/enroll-token", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGenerateEnrollToken_HubNil(t *testing.T) {
	h := newHandlerForTest(nil, nil, nil)
	e := echo.New()
	e.POST("/agent-devices/enroll-token", h.GenerateEnrollToken, withAdminAuthMW("u-1"))
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/enroll-token", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGenerateEnrollToken_Happy(t *testing.T) {
	spy := &auditSpy{}
	hub := &fakeHub{createTokenResp: &hub.CreateEnrollmentTokenResponse{Token: "tok-x", ExpiresAt: nowFixture()}}
	h := newHandlerForTest(nil, hub, spy)
	e := echo.New()
	e.POST("/agent-devices/enroll-token", h.GenerateEnrollToken, withAdminAuthMW("u-1"))
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/enroll-token", bytes.NewBufferString(`{"hostname":"my-mac"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if hub.createTokenHits != 1 {
		t.Errorf("hub createToken hits=%d", hub.createTokenHits)
	}
	if hub.createTokenReq.Label != "my-mac" || hub.createTokenReq.CreatedBy != "u-1" {
		t.Errorf("token req: %+v", hub.createTokenReq)
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
}

func TestGenerateEnrollToken_DefaultsLabel(t *testing.T) {
	hub := &fakeHub{createTokenResp: &hub.CreateEnrollmentTokenResponse{Token: "t"}}
	h := newHandlerForTest(nil, hub, nil)
	e := echo.New()
	e.POST("/agent-devices/enroll-token", h.GenerateEnrollToken, withAdminAuthMW("u-1"))
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/enroll-token", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d", rec.Code)
	}
	if hub.createTokenReq.Label != "agent" {
		t.Errorf("default label: %q", hub.createTokenReq.Label)
	}
}

func TestGenerateEnrollToken_HubNotConfigured(t *testing.T) {
	hub := &fakeHub{createTokenErr: hub.ErrNotConfigured}
	h := newHandlerForTest(nil, hub, nil)
	e := echo.New()
	e.POST("/agent-devices/enroll-token", h.GenerateEnrollToken, withAdminAuthMW("u-1"))
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/enroll-token", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGenerateEnrollToken_HubError(t *testing.T) {
	hub := &fakeHub{createTokenErr: errors.New("hub boom")}
	h := newHandlerForTest(nil, hub, nil)
	e := echo.New()
	e.POST("/agent-devices/enroll-token", h.GenerateEnrollToken, withAdminAuthMW("u-1"))
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/enroll-token", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUnenrollDevice_Happy(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	h := newHandlerForTest(mock, &fakeHub{}, spy)
	now := nowFixture()
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))
	mock.ExpectExec(`UPDATE thing SET status`).
		WithArgs("agent-1", "revoked").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))

	e := echo.New()
	e.POST("/agent-devices/:id/unenroll", h.UnenrollDevice)
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/agent-1/unenroll", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
}

func TestUnenrollDevice_NotFound(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)
	// scanThingNode returns (nil, nil) on ErrNoRows → handler 404.

	e := echo.New()
	e.POST("/agent-devices/:id/unenroll", h.UnenrollDevice)
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/missing/unenroll", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUnenrollDevice_DBErrorOnGet(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("x").WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.POST("/agent-devices/:id/unenroll", h.UnenrollDevice)
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/x/unenroll", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUnenrollDevice_UpdateError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))
	mock.ExpectExec(`UPDATE thing SET status`).WithArgs("agent-1", "revoked").
		WillReturnError(errors.New("update err"))

	e := echo.New()
	e.POST("/agent-devices/:id/unenroll", h.UnenrollDevice)
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/agent-1/unenroll", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListDeviceAssignments_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "DeviceAssignment"`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`FROM "DeviceAssignment" da`).WithArgs("agent-1", 50, 0).
		WillReturnRows(pgxmock.NewRows(deviceAssignmentCols).AddRow(makeDeviceAssignmentRow(now)...))

	e := echo.New()
	e.GET("/agent-devices/:id/assignments", h.ListDeviceAssignments)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1/assignments", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListDeviceAssignments_EmptyID(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("")
	if err := h.ListDeviceAssignments(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListDeviceAssignments_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).WithArgs("agent-1").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-devices/:id/assignments", h.ListDeviceAssignments)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1/assignments", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestForceRefreshAgentDevice_Happy(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	hub := &fakeHub{forceResyncResp: map[string]any{"ok": true}}
	h := newHandlerForTest(mock, hub, spy)
	now := nowFixture()
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))

	e := echo.New()
	e.POST("/agent-devices/:id/force-refresh", h.ForceRefreshAgentDevice)
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/agent-1/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if hub.forceResyncHits != 1 || hub.forceResyncReq != "agent-1" {
		t.Errorf("hub: hits=%d req=%q", hub.forceResyncHits, hub.forceResyncReq)
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
}

func TestForceRefreshAgentDevice_EmptyID(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("")
	if err := h.ForceRefreshAgentDevice(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestForceRefreshAgentDevice_DBErrorOnGet(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("x").WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.POST("/agent-devices/:id/force-refresh", h.ForceRefreshAgentDevice)
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/x/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestForceRefreshAgentDevice_NotFound(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("x").WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	e.POST("/agent-devices/:id/force-refresh", h.ForceRefreshAgentDevice)
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/x/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestForceRefreshAgentDevice_HubNotConfigured(t *testing.T) {
	mock := newMockPool(t)
	hub := &fakeHub{forceResyncErr: hub.ErrNotConfigured}
	h := newHandlerForTest(mock, hub, nil)
	now := nowFixture()
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))

	e := echo.New()
	e.POST("/agent-devices/:id/force-refresh", h.ForceRefreshAgentDevice)
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/agent-1/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestForceRefreshAgentDevice_HubError(t *testing.T) {
	mock := newMockPool(t)
	hub := &fakeHub{forceResyncErr: errors.New("hub boom")}
	h := newHandlerForTest(mock, hub, nil)
	now := nowFixture()
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))

	e := echo.New()
	e.POST("/agent-devices/:id/force-refresh", h.ForceRefreshAgentDevice)
	req := httptest.NewRequest(http.MethodPost, "/agent-devices/agent-1/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAgentFleetHealth_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM thing\s+WHERE type = 'agent'`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"total", "active", "stale", "critical", "revoked"}).AddRow(10, 8, 1, 0, 1))

	e := echo.New()
	e.GET("/agent-devices/health", h.AgentFleetHealth)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAgentFleetHealth_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM thing\s+WHERE type = 'agent'`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-devices/health", h.AgentFleetHealth)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

// quick sanity ensure non-tests symbols don't break compile (some
// imports flagged when only a subset used).
func TestDevicesSanity_TypesUsed(t *testing.T) {
	_ = strings.HasPrefix
	_ = time.Now
	_ = context.Background
}
