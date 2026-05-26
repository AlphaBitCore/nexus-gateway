package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/device"
)

// TestNowUnixImpl is a tiny sanity check — production wraps time.Now.
func TestNowUnixImpl(t *testing.T) {
	if nowUnixImpl() <= 0 {
		t.Error("nowUnixImpl should return positive epoch seconds")
	}
}

// TestLoadPreviewDevices_FnSeam covers the test-seam branch — when
// previewDevicesFn is set it short-circuits before touching Pool.
func TestLoadPreviewDevices_FnSeam(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	want := []previewDevice{{ID: "a"}, {ID: "b"}}
	h.previewDevicesFn = func(context.Context) ([]previewDevice, error) {
		return want, nil
	}
	got, err := h.loadPreviewDevices(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "a" {
		t.Errorf("got=%+v err=%v", got, err)
	}
}


func TestPreviewMembership_Happy(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	h.previewDevicesFn = func(context.Context) ([]previewDevice, error) {
		return []previewDevice{
			{ID: "a", Dev: device.Device{OS: "darwin"}},
			{ID: "b", Dev: device.Device{OS: "linux"}},
		}, nil
	}

	e := echo.New()
	e.POST("/preview-membership", h.PreviewMembership)
	body := `{"membershipQuery":{"all":[{"field":"os","op":"eq","value":"darwin"}]}}`
	req := httptest.NewRequest(http.MethodPost, "/preview-membership",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp previewMembershipResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Matched != 1 || len(resp.Sample) != 1 || resp.Sample[0] != "a" {
		t.Errorf("resp=%+v", resp)
	}
}

func TestPreviewMembership_CapsSampleAt50(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	devs := make([]previewDevice, 60)
	for i := range devs {
		devs[i] = previewDevice{ID: idN(i), Dev: device.Device{OS: "darwin"}}
	}
	h.previewDevicesFn = func(context.Context) ([]previewDevice, error) { return devs, nil }

	e := echo.New()
	e.POST("/preview-membership", h.PreviewMembership)
	body := `{"membershipQuery":{"all":[{"field":"os","op":"eq","value":"darwin"}]}}`
	req := httptest.NewRequest(http.MethodPost, "/preview-membership",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp previewMembershipResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Matched != 60 || len(resp.Sample) != 50 {
		t.Errorf("matched=%d sample=%d", resp.Matched, len(resp.Sample))
	}
}

func TestPreviewMembership_MalformedJSON(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	e.POST("/preview-membership", h.PreviewMembership)
	req := httptest.NewRequest(http.MethodPost, "/preview-membership",
		bytes.NewBufferString(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestPreviewMembership_LoadDBError(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	h.previewDevicesFn = func(context.Context) ([]previewDevice, error) {
		return nil, errors.New("planner err")
	}

	e := echo.New()
	e.POST("/preview-membership", h.PreviewMembership)
	req := httptest.NewRequest(http.MethodPost, "/preview-membership",
		bytes.NewBufferString(`{"membershipQuery":{"all":[]}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestPreviewMembership_BadPredicate(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	h.previewDevicesFn = func(context.Context) ([]previewDevice, error) {
		return []previewDevice{{ID: "a", Dev: device.Device{OS: "darwin"}}}, nil
	}

	e := echo.New()
	e.POST("/preview-membership", h.PreviewMembership)
	// unknown field is an evaluator error.
	body := `{"membershipQuery":{"all":[{"field":"unknown_field","op":"eq","value":"x"}]}}`
	req := httptest.NewRequest(http.MethodPost, "/preview-membership",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}


func TestSetGroupMembershipQuery_HappySmart(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	h := newHandlerForTest(mock, &fakeHub{}, spy)
	now := nowFixture()
	mock.ExpectQuery(`UPDATE "DeviceGroup" SET\s+membership_query`).
		WithArgs("grp-1", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))

	e := echo.New()
	e.PUT("/device-groups/:id/membership-query", h.SetGroupMembershipQuery)
	body := `{"membershipQuery":{"all":[{"field":"os","op":"eq","value":"darwin"}]}}`
	req := httptest.NewRequest(http.MethodPut, "/device-groups/grp-1/membership-query",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
	last := spy.last()
	after, _ := last["afterState"].(map[string]any)
	if after["mode"] != "smart" {
		t.Errorf("mode=%v", after["mode"])
	}
}

func TestSetGroupMembershipQuery_HappyStatic(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	h := newHandlerForTest(mock, &fakeHub{}, spy)
	now := nowFixture()
	mock.ExpectExec(`DELETE FROM device_group_membership_cache`).WithArgs("grp-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
	mock.ExpectQuery(`UPDATE "DeviceGroup" SET\s+membership_query`).
		WithArgs("grp-1", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(deviceGroupCols).AddRow(makeDeviceGroupRow(now)...))

	e := echo.New()
	e.PUT("/device-groups/:id/membership-query", h.SetGroupMembershipQuery)
	// `null` membershipQuery → static mode.
	body := `{"membershipQuery":null}`
	req := httptest.NewRequest(http.MethodPut, "/device-groups/grp-1/membership-query",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	last := spy.last()
	after, _ := last["afterState"].(map[string]any)
	if after["mode"] != "static" {
		t.Errorf("mode=%v", after["mode"])
	}
}

func TestSetGroupMembershipQuery_EmptyID(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("")
	if err := h.SetGroupMembershipQuery(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestSetGroupMembershipQuery_MalformedJSON(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	e.PUT("/device-groups/:id/membership-query", h.SetGroupMembershipQuery)
	req := httptest.NewRequest(http.MethodPut, "/device-groups/grp-1/membership-query",
		bytes.NewBufferString(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestSetGroupMembershipQuery_BadPredicate(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	e.PUT("/device-groups/:id/membership-query", h.SetGroupMembershipQuery)
	body := `{"membershipQuery":{"all":[{"field":"unknown","op":"eq","value":"x"}]}}`
	req := httptest.NewRequest(http.MethodPut, "/device-groups/grp-1/membership-query",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSetGroupMembershipQuery_UpdateDBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`UPDATE "DeviceGroup" SET\s+membership_query`).
		WithArgs("grp-1", pgxmock.AnyArg()).
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.PUT("/device-groups/:id/membership-query", h.SetGroupMembershipQuery)
	body := `{"membershipQuery":{"all":[{"field":"os","op":"eq","value":"darwin"}]}}`
	req := httptest.NewRequest(http.MethodPut, "/device-groups/grp-1/membership-query",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

// idN returns a synthetic device id "agent-NNN" for sampling-cap tests.
func idN(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "agent-0"
	}
	out := []byte{}
	x := n
	for x > 0 {
		out = append([]byte{digits[x%10]}, out...)
		x /= 10
	}
	return "agent-" + string(out)
}
