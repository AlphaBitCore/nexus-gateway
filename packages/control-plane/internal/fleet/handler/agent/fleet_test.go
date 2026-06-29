package agent

import (
	"bytes"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

// TestRegisterFleetRoutes_MountsAll locks the fleet route map.
func TestRegisterFleetRoutes_MountsAll(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	g := e.Group("/api/admin")
	h.RegisterFleetRoutes(g, noopIAM())
	want := []string{
		"GET /api/admin/agent-users",
		"GET /api/admin/agent-users/:id",
		"GET /api/admin/agent-users/:id/devices",
		"GET /api/admin/agent-users/:id/audit",
		"POST /api/admin/agent-users/:id/suspend",
		"POST /api/admin/agent-users/:id/activate",
		"GET /api/admin/agent-devices/:id/audit",
		"GET /api/admin/agent-devices/:id/config",
		"GET /api/admin/agent-devices/:id/timeline",
		"GET /api/admin/me/agent-devices",
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

func TestListAgentUsers_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "NexusUser"`).
		WithArgs(false).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`FROM "NexusUser" u\s+LEFT JOIN "Organization"`).
		WithArgs(false, 50, 0).
		WillReturnRows(pgxmock.NewRows(nexusUserSafeListCols).AddRow(makeAgentUserSafeRow(now)...))

	e := echo.New()
	e.GET("/agent-users", h.ListAgentUsers)
	req := httptest.NewRequest(http.MethodGet, "/agent-users", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListAgentUsers_FilterEnabled(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "NexusUser"`).
		WithArgs(false).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM "NexusUser" u`).
		WithArgs(false, 50, 0).
		WillReturnRows(pgxmock.NewRows(nexusUserSafeListCols))

	e := echo.New()
	e.GET("/agent-users", h.ListAgentUsers)
	req := httptest.NewRequest(http.MethodGet, "/agent-users?enabled=true", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListAgentUsers_FilterDisabled(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "NexusUser"`).
		WithArgs(false).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM "NexusUser" u`).
		WithArgs(false, 50, 0).
		WillReturnRows(pgxmock.NewRows(nexusUserSafeListCols))

	e := echo.New()
	e.GET("/agent-users", h.ListAgentUsers)
	req := httptest.NewRequest(http.MethodGet, "/agent-users?enabled=false", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListAgentUsers_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-users", h.ListAgentUsers)
	req := httptest.NewRequest(http.MethodGet, "/agent-users", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetAgentUser_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`FROM "NexusUser"\s+WHERE id`).WithArgs("u-1").
		WillReturnRows(pgxmock.NewRows(nexusUserCols).AddRow(makeAgentUserRow(now)...))

	e := echo.New()
	e.GET("/agent-users/:id", h.GetAgentUser)
	req := httptest.NewRequest(http.MethodGet, "/agent-users/u-1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetAgentUser_RejectsAdmin(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`FROM "NexusUser"\s+WHERE id`).WithArgs("u-1").
		WillReturnRows(pgxmock.NewRows(nexusUserCols).AddRow(makeAdminUserRow(now)...))

	e := echo.New()
	e.GET("/agent-users/:id", h.GetAgentUser)
	req := httptest.NewRequest(http.MethodGet, "/agent-users/u-1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetAgentUser_NotFound(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM "NexusUser"\s+WHERE id`).WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	e.GET("/agent-users/:id", h.GetAgentUser)
	req := httptest.NewRequest(http.MethodGet, "/agent-users/missing", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetAgentUser_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM "NexusUser"\s+WHERE id`).WithArgs("x").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-users/:id", h.GetAgentUser)
	req := httptest.NewRequest(http.MethodGet, "/agent-users/x", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListAgentUserDevices_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`COUNT\(\*\)\s+FROM "DeviceAssignment"`).WithArgs("u-1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("u-1", 50, 0).
		WillReturnRows(pgxmock.NewRows(fleetUserDeviceCols).AddRow(makeFleetUserDeviceRow(now)...))

	e := echo.New()
	e.GET("/agent-users/:id/devices", h.ListAgentUserDevices)
	req := httptest.NewRequest(http.MethodGet, "/agent-users/u-1/devices", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListAgentUserDevices_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`COUNT\(\*\)`).WithArgs("u-1").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-users/:id/devices", h.ListAgentUserDevices)
	req := httptest.NewRequest(http.MethodGet, "/agent-users/u-1/devices", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListMyAgentDevices_NoAuth(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	e.GET("/me/agent-devices", h.ListMyAgentDevices)
	req := httptest.NewRequest(http.MethodGet, "/me/agent-devices", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListMyAgentDevices_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`COUNT\(\*\)\s+FROM "DeviceAssignment"`).WithArgs("u-1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("u-1", 50, 0).
		WillReturnRows(pgxmock.NewRows(fleetUserDeviceCols).AddRow(makeFleetUserDeviceRow(now)...))

	e := echo.New()
	e.GET("/me/agent-devices", h.ListMyAgentDevices, withAdminAuthMW("u-1"))
	req := httptest.NewRequest(http.MethodGet, "/me/agent-devices", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListMyAgentDevices_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`COUNT\(\*\)`).WithArgs("u-1").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/me/agent-devices", h.ListMyAgentDevices, withAdminAuthMW("u-1"))
	req := httptest.NewRequest(http.MethodGet, "/me/agent-devices", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListAgentUserAudit_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`COUNT\(\*\) FROM traffic_event`).WithArgs("u-1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`FROM traffic_event\s+WHERE entity_id`).WithArgs("u-1", 50, 0).
		WillReturnRows(pgxmock.NewRows(auditEventCols).AddRow(makeAuditEventRow(now)...))

	e := echo.New()
	e.GET("/agent-users/:id/audit", h.ListAgentUserAudit)
	req := httptest.NewRequest(http.MethodGet, "/agent-users/u-1/audit", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListAgentUserAudit_WithTimeRange(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`COUNT\(\*\) FROM traffic_event`).
		WithArgs("u-1", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs("u-1", pgxmock.AnyArg(), pgxmock.AnyArg(), 50, 0).
		WillReturnRows(pgxmock.NewRows(auditEventCols))

	e := echo.New()
	e.GET("/agent-users/:id/audit", h.ListAgentUserAudit)
	req := httptest.NewRequest(http.MethodGet,
		"/agent-users/u-1/audit?start=2026-05-01T00:00:00Z&end=2026-05-17T00:00:00Z", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListAgentUserAudit_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`COUNT\(\*\)`).WithArgs("u-1").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-users/:id/audit", h.ListAgentUserAudit)
	req := httptest.NewRequest(http.MethodGet, "/agent-users/u-1/audit", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

// SuspendAgentUser / ActivateAgentUser

func TestSuspendAgentUser_Happy(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	h := newHandlerForTest(mock, &fakeHub{}, spy)
	now := nowFixture()
	mock.ExpectQuery(`FROM "NexusUser"\s+WHERE id`).WithArgs("u-1").
		WillReturnRows(pgxmock.NewRows(nexusUserCols).AddRow(makeAgentUserRow(now)...))
	mock.ExpectQuery(`UPDATE "NexusUser" SET`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(nexusUserSafeCols).AddRow(makeAgentUserUpdateRow(now, "suspended")...))

	e := echo.New()
	e.POST("/agent-users/:id/suspend", h.SuspendAgentUser)
	req := httptest.NewRequest(http.MethodPost, "/agent-users/u-1/suspend", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
}

func TestActivateAgentUser_Happy(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	h := newHandlerForTest(mock, &fakeHub{}, spy)
	now := nowFixture()
	mock.ExpectQuery(`FROM "NexusUser"\s+WHERE id`).WithArgs("u-1").
		WillReturnRows(pgxmock.NewRows(nexusUserCols).AddRow(makeAgentUserRow(now)...))
	mock.ExpectQuery(`UPDATE "NexusUser" SET`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(nexusUserSafeCols).AddRow(makeAgentUserUpdateRow(now, "active")...))

	e := echo.New()
	e.POST("/agent-users/:id/activate", h.ActivateAgentUser)
	req := httptest.NewRequest(http.MethodPost, "/agent-users/u-1/activate", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSetAgentUserStatus_NotFound(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM "NexusUser"\s+WHERE id`).WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	e.POST("/agent-users/:id/suspend", h.SuspendAgentUser)
	req := httptest.NewRequest(http.MethodPost, "/agent-users/missing/suspend", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestSetAgentUserStatus_RejectsAdmin(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`FROM "NexusUser"\s+WHERE id`).WithArgs("u-1").
		WillReturnRows(pgxmock.NewRows(nexusUserCols).AddRow(makeAdminUserRow(now)...))

	e := echo.New()
	e.POST("/agent-users/:id/suspend", h.SuspendAgentUser)
	req := httptest.NewRequest(http.MethodPost, "/agent-users/u-1/suspend", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestSetAgentUserStatus_FindDBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM "NexusUser"`).WithArgs("x").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.POST("/agent-users/:id/suspend", h.SuspendAgentUser)
	req := httptest.NewRequest(http.MethodPost, "/agent-users/x/suspend", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestSetAgentUserStatus_UpdateDBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`FROM "NexusUser"\s+WHERE id`).WithArgs("u-1").
		WillReturnRows(pgxmock.NewRows(nexusUserCols).AddRow(makeAgentUserRow(now)...))
	mock.ExpectQuery(`UPDATE "NexusUser" SET`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("update boom"))

	e := echo.New()
	e.POST("/agent-users/:id/suspend", h.SuspendAgentUser)
	req := httptest.NewRequest(http.MethodPost, "/agent-users/u-1/suspend", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListDeviceAudit_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`COUNT\(\*\) FROM traffic_event`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`FROM traffic_event\s+WHERE thing_id`).
		WithArgs("agent-1", 50, 0).
		WillReturnRows(pgxmock.NewRows(auditEventCols).AddRow(makeAuditEventRow(now)...))

	e := echo.New()
	e.GET("/agent-devices/:id/audit", h.ListDeviceAudit)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1/audit", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListDeviceAudit_WithTimeRange(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`COUNT\(\*\) FROM traffic_event`).
		WithArgs("agent-1", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs("agent-1", pgxmock.AnyArg(), pgxmock.AnyArg(), 50, 0).
		WillReturnRows(pgxmock.NewRows(auditEventCols))

	e := echo.New()
	e.GET("/agent-devices/:id/audit", h.ListDeviceAudit)
	req := httptest.NewRequest(http.MethodGet,
		"/agent-devices/agent-1/audit?start=2026-05-01T00:00:00Z&end=2026-05-17T00:00:00Z", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListDeviceAudit_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`COUNT\(\*\)`).WithArgs("agent-1").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-devices/:id/audit", h.ListDeviceAudit)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1/audit", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetDeviceConfig_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`SELECT p\.id, p\.name`).
		WithArgs(1000, 0).
		WillReturnRows(pgxmock.NewRows(providerListCols).AddRow(makeProviderRow(now)...))

	e := echo.New()
	e.GET("/agent-devices/:id/config", h.GetDeviceConfig)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1/config", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	domains, _ := body["aiDomains"].([]any)
	if len(domains) != 1 {
		t.Errorf("domains: %v", body["aiDomains"])
	}
}

func TestGetDeviceConfig_FiltersDisabledProvider(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))
	row := makeProviderRow(now)
	row[9] = false // enabled = false
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`SELECT p\.id`).WithArgs(1000, 0).
		WillReturnRows(pgxmock.NewRows(providerListCols).AddRow(row...))

	e := echo.New()
	e.GET("/agent-devices/:id/config", h.GetDeviceConfig)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1/config", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if d, _ := body["aiDomains"].([]any); len(d) != 0 {
		t.Errorf("expected zero domains when provider disabled: %v", d)
	}
}

func TestGetDeviceConfig_DeviceNotFound(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	e.GET("/agent-devices/:id/config", h.GetDeviceConfig)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/missing/config", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetDeviceConfig_DeviceGetError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("x").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-devices/:id/config", h.GetDeviceConfig)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/x/config", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetDeviceConfig_ProvidersListError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Provider"`).
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-devices/:id/config", h.GetDeviceConfig)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1/config", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetDeviceTimeline_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`FROM "DeviceAssignment" da`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(deviceAssignmentCols).AddRow(makeDeviceAssignmentRow(now)...))

	e := echo.New()
	e.GET("/agent-devices/:id/timeline", h.GetDeviceTimeline)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1/timeline", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetDeviceTimeline_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM "DeviceAssignment" da`).WithArgs("agent-1").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/agent-devices/:id/timeline", h.GetDeviceTimeline)
	req := httptest.NewRequest(http.MethodGet, "/agent-devices/agent-1/timeline", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

// silence unused imports on platforms where the helpers above migrate.
var _ = bytes.NewBuffer
