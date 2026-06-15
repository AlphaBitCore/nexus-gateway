package agent

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

// TestRegisterDeviceGroupRoutes locks the device-group route map.
func TestRegisterDeviceGroupRoutes_MountsAll(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	g := e.Group("/api/admin")
	h.RegisterDeviceGroupRoutes(g, noopIAM())
	want := []string{
		"GET /api/admin/device-groups",
		"POST /api/admin/device-groups",
		"GET /api/admin/device-groups/:id",
		"PUT /api/admin/device-groups/:id",
		"DELETE /api/admin/device-groups/:id",
		"POST /api/admin/device-groups/preview-membership",
		"PUT /api/admin/device-groups/:id/membership-query",
		"POST /api/admin/device-groups/:id/force-refresh",
		"POST /api/admin/device-groups/:id/members",
		"DELETE /api/admin/device-groups/:id/members/:deviceId",
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

func TestListDeviceGroups_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "DeviceGroup"`).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`SELECT g\..* member_count\s+FROM "DeviceGroup"`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows(deviceGroupListCols).AddRow(makeDeviceGroupListRow(now)...))

	e := echo.New()
	e.GET("/device-groups", h.ListDeviceGroups)
	req := httptest.NewRequest(http.MethodGet, "/device-groups", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListDeviceGroups_WithQuery(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).WithArgs("%eng%").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`FROM "DeviceGroup"`).WithArgs("%eng%", 50, 0).
		WillReturnRows(pgxmock.NewRows(deviceGroupListCols).AddRow(makeDeviceGroupListRow(now)...))

	e := echo.New()
	e.GET("/device-groups", h.ListDeviceGroups)
	req := httptest.NewRequest(http.MethodGet, "/device-groups?q=eng", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListDeviceGroups_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/device-groups", h.ListDeviceGroups)
	req := httptest.NewRequest(http.MethodGet, "/device-groups", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetDeviceGroup_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))
	mock.ExpectQuery(`FROM "DeviceGroupMembership" m\s+JOIN thing t`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows(deviceGroupMembershipDetailCols).AddRow(makeDeviceGroupMembershipRow(now)...))

	e := echo.New()
	e.GET("/device-groups/:id", h.GetDeviceGroup)
	req := httptest.NewRequest(http.MethodGet, "/device-groups/grp-1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetDeviceGroup_NotFound(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	e.GET("/device-groups/:id", h.GetDeviceGroup)
	req := httptest.NewRequest(http.MethodGet, "/device-groups/missing", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetDeviceGroup_MembershipsError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))
	mock.ExpectQuery(`FROM "DeviceGroupMembership" m\s+JOIN thing t`).WithArgs("grp-1").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.GET("/device-groups/:id", h.GetDeviceGroup)
	req := httptest.NewRequest(http.MethodGet, "/device-groups/grp-1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreateDeviceGroup_Happy(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	h := newHandlerForTest(mock, &fakeHub{}, spy)
	now := nowFixture()
	mock.ExpectQuery(`INSERT INTO "DeviceGroup"`).
		WithArgs("Engineering", pgxmock.AnyArg(), "u-1").
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))

	e := echo.New()
	e.POST("/device-groups", h.CreateDeviceGroup, withAdminAuthMW("u-1"))
	req := httptest.NewRequest(http.MethodPost, "/device-groups",
		bytes.NewBufferString(`{"name":"Engineering","description":"engineering laptops"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
}

func TestCreateDeviceGroup_DefaultsCreatedBy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`INSERT INTO "DeviceGroup"`).
		WithArgs("Eng", pgxmock.AnyArg(), "unknown").
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))

	e := echo.New()
	e.POST("/device-groups", h.CreateDeviceGroup)
	req := httptest.NewRequest(http.MethodPost, "/device-groups",
		bytes.NewBufferString(`{"name":"Eng"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreateDeviceGroup_MissingName(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	e.POST("/device-groups", h.CreateDeviceGroup)
	req := httptest.NewRequest(http.MethodPost, "/device-groups",
		bytes.NewBufferString(`{"description":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreateDeviceGroup_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`INSERT INTO "DeviceGroup"`).
		WillReturnError(errors.New("dup name"))

	e := echo.New()
	e.POST("/device-groups", h.CreateDeviceGroup)
	req := httptest.NewRequest(http.MethodPost, "/device-groups",
		bytes.NewBufferString(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUpdateDeviceGroup_Happy(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	h := newHandlerForTest(mock, &fakeHub{}, spy)
	now := nowFixture()
	mock.ExpectQuery(`UPDATE "DeviceGroup" SET`).
		WithArgs("grp-1", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))

	e := echo.New()
	e.PUT("/device-groups/:id", h.UpdateDeviceGroup)
	req := httptest.NewRequest(http.MethodPut, "/device-groups/grp-1",
		bytes.NewBufferString(`{"name":"NewName"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
}

func TestUpdateDeviceGroup_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`UPDATE "DeviceGroup" SET`).
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.PUT("/device-groups/:id", h.UpdateDeviceGroup)
	req := httptest.NewRequest(http.MethodPut, "/device-groups/grp-1",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestDeleteDeviceGroup_Happy(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	h := newHandlerForTest(mock, &fakeHub{}, spy)
	// DeleteDeviceGroup now does a single DELETE — membership cascade and
	// thing.desired_ver bump were removed when device_group_config was
	// retired (groups became targeting-only, no config payload to
	// invalidate). Mock only the DELETE the handler actually issues.
	mock.ExpectExec(`DELETE FROM "DeviceGroup"`).WithArgs("grp-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))

	e := echo.New()
	e.DELETE("/device-groups/:id", h.DeleteDeviceGroup)
	req := httptest.NewRequest(http.MethodDelete, "/device-groups/grp-1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
}

func TestDeleteDeviceGroup_NotFound(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectExec(`DELETE FROM "DeviceGroup"`).WithArgs("grp-1").
		WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	e.DELETE("/device-groups/:id", h.DeleteDeviceGroup)
	req := httptest.NewRequest(http.MethodDelete, "/device-groups/grp-1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAddGroupMember_Happy(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	h := newHandlerForTest(mock, &fakeHub{}, spy)
	mock.ExpectQuery(`INSERT INTO "DeviceGroupMembership"`).
		WithArgs("grp-1", "agent-1", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("m-1"))
	mock.ExpectQuery(`SELECT GREATEST`).
		WillReturnRows(pgxmock.NewRows([]string{"v"}).AddRow(int64(10)))
	mock.ExpectExec(`UPDATE thing\s+SET desired_ver`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	e := echo.New()
	e.POST("/device-groups/:id/members", h.AddGroupMember)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/grp-1/members",
		bytes.NewBufferString(`{"deviceId":"agent-1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
}

func TestAddGroupMember_WithExpiresAt(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	future := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	mock.ExpectQuery(`INSERT INTO "DeviceGroupMembership"`).
		WithArgs("grp-1", "agent-1", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("m-1"))
	mock.ExpectQuery(`SELECT GREATEST`).
		WillReturnRows(pgxmock.NewRows([]string{"v"}).AddRow(int64(10)))
	mock.ExpectExec(`UPDATE thing\s+SET desired_ver`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	e := echo.New()
	e.POST("/device-groups/:id/members", h.AddGroupMember)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/grp-1/members",
		bytes.NewBufferString(`{"deviceId":"agent-1","expiresAt":"`+future+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAddGroupMember_MissingDeviceID(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	e.POST("/device-groups/:id/members", h.AddGroupMember)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/grp-1/members",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAddGroupMember_InvalidExpiresAt(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	e.POST("/device-groups/:id/members", h.AddGroupMember)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/grp-1/members",
		bytes.NewBufferString(`{"deviceId":"agent-1","expiresAt":"not-a-time"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAddGroupMember_ExpiresInPast(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	e := echo.New()
	e.POST("/device-groups/:id/members", h.AddGroupMember)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/grp-1/members",
		bytes.NewBufferString(`{"deviceId":"agent-1","expiresAt":"`+past+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAddGroupMember_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`INSERT INTO "DeviceGroupMembership"`).
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.POST("/device-groups/:id/members", h.AddGroupMember)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/grp-1/members",
		bytes.NewBufferString(`{"deviceId":"agent-1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestRemoveGroupMember_Happy(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	h := newHandlerForTest(mock, &fakeHub{}, spy)
	mock.ExpectExec(`DELETE FROM "DeviceGroupMembership"`).
		WithArgs("grp-1", "agent-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	mock.ExpectQuery(`SELECT GREATEST`).
		WillReturnRows(pgxmock.NewRows([]string{"v"}).AddRow(int64(11)))
	mock.ExpectExec(`UPDATE thing\s+SET desired_ver`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	e := echo.New()
	e.DELETE("/device-groups/:id/members/:deviceId", h.RemoveGroupMember)
	req := httptest.NewRequest(http.MethodDelete, "/device-groups/grp-1/members/agent-1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
}

func TestRemoveGroupMember_NotFound(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectExec(`DELETE FROM "DeviceGroupMembership"`).
		WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	e.DELETE("/device-groups/:id/members/:deviceId", h.RemoveGroupMember)
	req := httptest.NewRequest(http.MethodDelete, "/device-groups/grp-1/members/agent-1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}
