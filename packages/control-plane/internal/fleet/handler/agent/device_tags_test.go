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

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

// PutDeviceTags happy path uses the updateDeviceTagsFn test seam so
// the concrete *pgxpool.Pool isn't required.
func TestPutDeviceTags_Happy(t *testing.T) {
	mock := newMockPool(t)
	spy := &auditSpy{}
	h := newHandlerForTest(mock, &fakeHub{}, spy)
	now := nowFixture()
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))

	var gotID string
	var gotTags []string
	h.updateDeviceTagsFn = func(_ context.Context, id string, tags []string) error {
		gotID = id
		gotTags = tags
		return nil
	}

	e := echo.New()
	e.PUT("/agent-devices/:id/tags", h.PutDeviceTags)
	req := httptest.NewRequest(http.MethodPut, "/agent-devices/agent-1/tags",
		bytes.NewBufferString(`{"tags":["finance","byod"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotID != "agent-1" || !equalStrSlice(gotTags, []string{"finance", "byod"}) {
		t.Errorf("seam not called correctly: id=%q tags=%v", gotID, gotTags)
	}
	if spy.count() != 1 {
		t.Errorf("audit count=%d", spy.count())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["id"] != "agent-1" {
		t.Errorf("response id: %v", body["id"])
	}
}

func TestPutDeviceTags_EmptyDeviceID(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/",
		bytes.NewBufferString(`{"tags":["x"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("")
	if err := h.PutDeviceTags(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestPutDeviceTags_MalformedJSON(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	e.PUT("/agent-devices/:id/tags", h.PutDeviceTags)
	req := httptest.NewRequest(http.MethodPut, "/agent-devices/agent-1/tags",
		bytes.NewBufferString(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestPutDeviceTags_EmptyTagRejected(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	e.PUT("/agent-devices/:id/tags", h.PutDeviceTags)
	req := httptest.NewRequest(http.MethodPut, "/agent-devices/agent-1/tags",
		bytes.NewBufferString(`{"tags":["valid",""]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INVALID_TAG") {
		t.Errorf("body missing INVALID_TAG: %s", rec.Body.String())
	}
}

func TestPutDeviceTags_TagTooLong(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	e.PUT("/agent-devices/:id/tags", h.PutDeviceTags)
	long := strings.Repeat("a", 65)
	req := httptest.NewRequest(http.MethodPut, "/agent-devices/agent-1/tags",
		bytes.NewBufferString(`{"tags":["`+long+`"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "TAG_TOO_LONG") {
		t.Errorf("body missing TAG_TOO_LONG: %s", rec.Body.String())
	}
}

func TestPutDeviceTags_DeviceNotFound(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	e.PUT("/agent-devices/:id/tags", h.PutDeviceTags)
	req := httptest.NewRequest(http.MethodPut, "/agent-devices/missing/tags",
		bytes.NewBufferString(`{"tags":["x"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestPutDeviceTags_GetDeviceDBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("x").
		WillReturnError(errors.New("planner err"))

	e := echo.New()
	e.PUT("/agent-devices/:id/tags", h.PutDeviceTags)
	req := httptest.NewRequest(http.MethodPut, "/agent-devices/x/tags",
		bytes.NewBufferString(`{"tags":["a"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

// TestPutDeviceTags_UpdateExecError exercises the seam returning an
// error to drive the 500 path inside runUpdateDeviceTags.
func TestPutDeviceTags_UpdateExecError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	now := nowFixture()
	mock.ExpectQuery(`WHERE t\.id`).WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows(agentDeviceCols).AddRow(makeAgentDeviceRow(now)...))
	h.updateDeviceTagsFn = func(context.Context, string, []string) error {
		return errors.New("update boom")
	}

	e := echo.New()
	e.PUT("/agent-devices/:id/tags", h.PutDeviceTags)
	req := httptest.NewRequest(http.MethodPut, "/agent-devices/agent-1/tags",
		bytes.NewBufferString(`{"tags":["a"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
