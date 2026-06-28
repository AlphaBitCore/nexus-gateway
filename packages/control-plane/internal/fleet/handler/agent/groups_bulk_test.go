package agent

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
)

// TestRunBulkFanout_AllSucceed locks the happy path: every device
// callback returns nil → results all {ok:true} in the original order.
func TestRunBulkFanout_AllSucceed(t *testing.T) {
	devices := []string{"a", "b", "c"}
	r := runBulkFanout(context.Background(), devices, func(_ context.Context, _ string) error {
		return nil
	})
	if len(r) != 3 {
		t.Fatalf("len=%d", len(r))
	}
	for i, want := range devices {
		if r[i].DeviceID != want || !r[i].OK || r[i].Error != "" {
			t.Errorf("idx %d: %+v", i, r[i])
		}
	}
}

// TestRunBulkFanout_PartialFailure mixes successes + failures.
func TestRunBulkFanout_PartialFailure(t *testing.T) {
	devices := []string{"a", "b", "c"}
	r := runBulkFanout(context.Background(), devices, func(_ context.Context, did string) error {
		if did == "b" {
			return errors.New("b broke")
		}
		return nil
	})
	if r[0].OK != true || r[1].OK != false || r[2].OK != true {
		t.Errorf("ok bits: %+v", r)
	}
	if r[1].Error != "b broke" {
		t.Errorf("err msg: %q", r[1].Error)
	}
}

// TestRunBulkFanout_Empty returns empty slice for empty input.
func TestRunBulkFanout_Empty(t *testing.T) {
	r := runBulkFanout(context.Background(), nil, func(context.Context, string) error { return nil })
	if len(r) != 0 {
		t.Errorf("expected empty: %+v", r)
	}
}

// TestSummarize counts succeeded + failed.
func TestSummarize(t *testing.T) {
	got := []bulkActionResult{{OK: true}, {OK: false}, {OK: true}, {OK: false}, {OK: false}}
	succ, fail := summarize(got)
	if succ != 2 || fail != 3 {
		t.Errorf("succ=%d fail=%d", succ, fail)
	}
}

func TestBulkForceRefreshGroup_Happy(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	hub := &fakeHub{forceResyncResp: map[string]any{"ok": true}}
	h := newHandlerForTest(mock, hub, spy)
	now := nowFixture()
	mock.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))
	mock.ExpectQuery(`FROM "DeviceGroupMembership"\s+WHERE "groupId"`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows([]string{"deviceId"}).AddRow("agent-1").AddRow("agent-2"))

	e := echo.New()
	e.POST("/device-groups/:id/force-refresh", h.BulkForceRefreshGroup)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/grp-1/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if hub.forceResyncHits != 2 {
		t.Errorf("hub hits=%d", hub.forceResyncHits)
	}
	var body bulkActionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Total != 2 || body.Succeeded != 2 || body.Failed != 0 {
		t.Errorf("body: %+v", body)
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
}

func TestBulkForceRefreshGroup_PartialFailure207(t *testing.T) {
	mock := newMockPool(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))
	mock.ExpectQuery(`FROM "DeviceGroupMembership"`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows([]string{"deviceId"}).AddRow("a").AddRow("b"))

	// Wrap the hub with a counter that fails on second call.
	hh := &countingHub{HubAPI: &fakeHub{}, failNth: 2}
	h := newHandlerForTest(mock, hh, nil)
	e := echo.New()
	e.POST("/device-groups/:id/force-refresh", h.BulkForceRefreshGroup)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/grp-1/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestBulkForceRefreshGroup_HubNotConfiguredPerDevice(t *testing.T) {
	mock := newMockPool(t)
	hub := &fakeHub{forceResyncErr: hub.ErrNotConfigured}
	h := newHandlerForTest(mock, hub, nil)
	now := nowFixture()
	mock.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))
	mock.ExpectQuery(`FROM "DeviceGroupMembership"`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows([]string{"deviceId"}).AddRow("a"))

	e := echo.New()
	e.POST("/device-groups/:id/force-refresh", h.BulkForceRefreshGroup)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/grp-1/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	// All-failed branch: status stays 200 (no succ>0), with results=failed.
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var body bulkActionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Failed != 1 {
		t.Errorf("failed=%d", body.Failed)
	}
}

func TestBulkForceRefreshGroup_NilHubPerDevice(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, nil, nil)
	now := nowFixture()
	mock.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))
	mock.ExpectQuery(`FROM "DeviceGroupMembership"`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows([]string{"deviceId"}).AddRow("a"))

	e := echo.New()
	e.POST("/device-groups/:id/force-refresh", h.BulkForceRefreshGroup)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/grp-1/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestBulkForceRefreshGroup_EmptyID(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("")
	if err := h.BulkForceRefreshGroup(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestBulkForceRefreshGroup_GroupGetDBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("x").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.POST("/device-groups/:id/force-refresh", h.BulkForceRefreshGroup)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/x/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestBulkForceRefreshGroup_GroupNotFound(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	e.POST("/device-groups/:id/force-refresh", h.BulkForceRefreshGroup)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/missing/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestBulkForceRefreshGroup_MembersDBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`FROM "DeviceGroup" WHERE id`).WithArgs("grp-1").
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))
	mock.ExpectQuery(`FROM "DeviceGroupMembership"`).WithArgs("grp-1").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.POST("/device-groups/:id/force-refresh", h.BulkForceRefreshGroup)
	req := httptest.NewRequest(http.MethodPost, "/device-groups/grp-1/force-refresh", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

// countingHub wraps a HubAPI and fails the Nth ForceResyncAll call to
// drive partial-success 207 paths. The mutex makes the counter safe for
// the goroutine-fanout under -race.
type countingHub struct {
	HubAPI
	mu      sync.Mutex
	count   int
	failNth int
}

func (c *countingHub) ForceResyncAll(ctx context.Context, id string) (map[string]any, error) {
	c.mu.Lock()
	c.count++
	n := c.count
	fail := n == c.failNth
	c.mu.Unlock()
	if fail {
		return nil, errors.New("bulk fail")
	}
	return c.HubAPI.ForceResyncAll(ctx, id)
}
