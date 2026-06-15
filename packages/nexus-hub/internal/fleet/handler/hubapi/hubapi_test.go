// Package hubapi_test covers helper functions and handler-level validation
// branches in HubAPI and InternalThingsAPI. Tests exercise every named failure
// mode that does not require a live database or real manager transaction, using
// only in-process fakes and the Echo httptest pattern.
//
// Architecture reference: docs/developers/architecture/services/hub/nexus-hub-internals-architecture.md (Tier 3).
// Manager is a concrete struct, not an interface; manager-level DB flows are
// covered by pgxmock in fleet/manager/manager_pgxmock_test.go. This file
// focuses on pure-logic helpers and the HTTP-layer validation surface.
package hubapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/scheduler"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// Internal test helpers

func newTestEcho() *echo.Echo { return echo.New() }

func echoCtxJSON(e *echo.Echo, method string, body any, pathParams map[string]string) (echo.Context, *httptest.ResponseRecorder) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "/", r)
	if body != nil {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if len(pathParams) > 0 {
		var names, vals []string
		for k, v := range pathParams {
			names = append(names, k)
			vals = append(vals, v)
		}
		c.SetParamNames(names...)
		c.SetParamValues(vals...)
	}
	return c, rec
}

func echoCtxQuery(e *echo.Echo, query string) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func decodeResp(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return m
}

// errCode extracts the machine-readable code from the canonical nested error
// envelope {error:{message,type,code}} (F-0319).
func errCode(m map[string]any) any {
	if inner, ok := m["error"].(map[string]any); ok {
		return inner["code"]
	}
	return nil
}

// errMsg extracts the human-readable message from the canonical nested error
// envelope (F-0319).
func errMsg(m map[string]any) string {
	if inner, ok := m["error"].(map[string]any); ok {
		s, _ := inner["message"].(string)
		return s
	}
	return ""
}

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Pure-logic helpers (no Echo, no DB)

func TestParseIntDefault(t *testing.T) {
	tests := []struct {
		name string
		in   string
		def  int
		want int
	}{
		{"empty returns default", "", 5, 5},
		{"valid positive", "10", 1, 10},
		{"non-numeric returns default", "abc", 7, 7},
		{"zero returns default (v<1)", "0", 3, 3},
		{"negative returns default (v<1)", "-5", 3, 3},
		{"one is minimum accepted value", "1", 99, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIntDefault(tt.in, tt.def)
			if got != tt.want {
				t.Errorf("parseIntDefault(%q,%d)=%d want %d", tt.in, tt.def, got, tt.want)
			}
		})
	}
}

func TestClamp(t *testing.T) {
	tests := []struct {
		name      string
		v, lo, hi int
		want      int
	}{
		{"below min clamped to min", 0, 1, 10, 1},
		{"above max clamped to max", 200, 1, 100, 100},
		{"within range unchanged", 50, 1, 100, 50},
		{"at min unchanged", 1, 1, 100, 1},
		{"at max unchanged", 100, 1, 100, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clamp(tt.v, tt.lo, tt.hi)
			if got != tt.want {
				t.Errorf("clamp(%d,%d,%d)=%d want %d", tt.v, tt.lo, tt.hi, got, tt.want)
			}
		})
	}
}

func TestParseTimeOrNil(t *testing.T) {
	t.Run("empty returns nil", func(t *testing.T) {
		if parseTimeOrNil("") != nil {
			t.Error("expected nil for empty string")
		}
	})
	t.Run("valid RFC3339 returns time", func(t *testing.T) {
		ts := parseTimeOrNil("2024-03-15T10:00:00Z")
		if ts == nil || ts.Year() != 2024 {
			t.Errorf("unexpected result: %v", ts)
		}
	})
	t.Run("malformed string returns nil", func(t *testing.T) {
		if parseTimeOrNil("not-a-date") != nil {
			t.Error("expected nil for malformed date")
		}
	})
}

func TestIsJSONObject(t *testing.T) {
	tests := []struct {
		name  string
		input json.RawMessage
		want  bool
	}{
		{"valid object", json.RawMessage(`{"key":"val"}`), true},
		{"empty object", json.RawMessage(`{}`), true},
		// null unmarshals as nil map (no error) so isJSONObject returns true;
		// the handler guards against empty-state upstream (NewOverrideState rejects null).
		{"null decodes as nil map, returns true", json.RawMessage(`null`), true},
		{"array rejected", json.RawMessage(`[1,2]`), false},
		{"string rejected", json.RawMessage(`"hello"`), false},
		{"malformed rejected", json.RawMessage(`{bad`), false},
		{"number rejected", json.RawMessage(`42`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isJSONObject(tt.input)
			if got != tt.want {
				t.Errorf("isJSONObject(%s)=%v want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestValidateOverrideBody exercises each early-exit failure mode in the order
// declared in the function (blacklist → state-empty → state-shape →
// reason-length → expiresAt-window).
func TestValidateOverrideBody(t *testing.T) {
	validState := json.RawMessage(`{"enabled":true}`)
	future30m := time.Now().Add(30 * time.Minute)

	t.Run("blacklisted key credentials rejected", func(t *testing.T) {
		err := validateOverrideBody("credentials", setOverrideBody{State: validState})
		if err == nil || !strings.Contains(err.Error(), "not overridable") {
			t.Errorf("want not-overridable error, got %v", err)
		}
	})
	t.Run("blacklisted key virtual_keys rejected", func(t *testing.T) {
		if err := validateOverrideBody("virtual_keys", setOverrideBody{State: validState}); err == nil {
			t.Error("want error for virtual_keys")
		}
	})
	t.Run("empty state rejected with 'state is required'", func(t *testing.T) {
		err := validateOverrideBody("routing", setOverrideBody{State: nil})
		if err == nil || !strings.Contains(err.Error(), "state is required") {
			t.Errorf("want 'state is required', got %v", err)
		}
	})
	t.Run("array state rejected as non-JSON-object", func(t *testing.T) {
		err := validateOverrideBody("routing", setOverrideBody{State: json.RawMessage(`[1,2]`)})
		if err == nil || !strings.Contains(err.Error(), "JSON object") {
			t.Errorf("want 'JSON object' error, got %v", err)
		}
	})
	t.Run("reason over 500 chars rejected", func(t *testing.T) {
		r := strings.Repeat("x", 501)
		err := validateOverrideBody("routing", setOverrideBody{State: validState, Reason: &r})
		if err == nil || !strings.Contains(err.Error(), "500 chars") {
			t.Errorf("want reason-too-long error, got %v", err)
		}
	})
	t.Run("reason exactly 500 chars accepted", func(t *testing.T) {
		r := strings.Repeat("x", 500)
		if err := validateOverrideBody("routing", setOverrideBody{State: validState, Reason: &r}); err != nil {
			t.Errorf("want nil for 500-char reason, got %v", err)
		}
	})
	t.Run("expiresAt in past rejected as out-of-range", func(t *testing.T) {
		past := time.Now().Add(-1 * time.Minute)
		err := validateOverrideBody("routing", setOverrideBody{State: validState, ExpiresAt: &past})
		if err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Errorf("want out-of-range for past expiresAt, got %v", err)
		}
	})
	t.Run("expiresAt beyond 30 days rejected as out-of-range", func(t *testing.T) {
		far := time.Now().Add(31 * 24 * time.Hour)
		err := validateOverrideBody("routing", setOverrideBody{State: validState, ExpiresAt: &far})
		if err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Errorf("want out-of-range for >30d expiresAt, got %v", err)
		}
	})
	t.Run("valid body without optional fields passes", func(t *testing.T) {
		if err := validateOverrideBody("routing", setOverrideBody{State: validState}); err != nil {
			t.Errorf("want nil, got %v", err)
		}
	})
	t.Run("valid body with expiresAt inside 30-day window passes", func(t *testing.T) {
		if err := validateOverrideBody("routing", setOverrideBody{State: validState, ExpiresAt: &future30m}); err != nil {
			t.Errorf("want nil, got %v", err)
		}
	})
}

// TestApplyActorFromHeaders verifies header-fallback and body-field-wins semantics.
func TestApplyActorFromHeaders(t *testing.T) {
	e := newTestEcho()

	t.Run("body fields win over headers when populated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Nexus-Actor-Id", "hdr-id")
		req.Header.Set("X-Nexus-Actor-Name", "hdr-name")
		c := e.NewContext(req, httptest.NewRecorder())
		r := &manager.UpdateConfigRequest{ActorID: "body-id", ActorName: "body-name"}
		applyActorFromHeaders(c, r)
		if r.ActorID != "body-id" {
			t.Errorf("ActorID=%q want body-id", r.ActorID)
		}
		if r.ActorName != "body-name" {
			t.Errorf("ActorName=%q want body-name", r.ActorName)
		}
	})
	t.Run("empty body fields filled from headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Nexus-Actor-Id", "hdr-id")
		req.Header.Set("X-Nexus-Actor-Name", "hdr-name")
		c := e.NewContext(req, httptest.NewRecorder())
		r := &manager.UpdateConfigRequest{}
		applyActorFromHeaders(c, r)
		if r.ActorID != "hdr-id" {
			t.Errorf("ActorID=%q want hdr-id", r.ActorID)
		}
		if r.ActorName != "hdr-name" {
			t.Errorf("ActorName=%q want hdr-name", r.ActorName)
		}
	})
	t.Run("no headers and no body fields leaves them empty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		c := e.NewContext(req, httptest.NewRecorder())
		r := &manager.UpdateConfigRequest{}
		applyActorFromHeaders(c, r)
		if r.ActorID != "" || r.ActorName != "" {
			t.Errorf("want empty, got ActorID=%q ActorName=%q", r.ActorID, r.ActorName)
		}
	})
}

// TestThingFromContext exercises nil-safe extraction and type-assertion guard.
func TestThingFromContext(t *testing.T) {
	e := newTestEcho()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	t.Run("missing key returns nil without panic", func(t *testing.T) {
		if got := ThingFromContext(c); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
	t.Run("wrong type returns nil (type assertion guard)", func(t *testing.T) {
		c.Set(thingContextKey, "not-a-thing-struct")
		if got := ThingFromContext(c); got != nil {
			t.Errorf("expected nil for wrong type, got %v", got)
		}
	})
	t.Run("correct *store.Thing returned", func(t *testing.T) {
		thing := &store.Thing{ID: "thing-42"}
		c.Set(thingContextKey, thing)
		got := ThingFromContext(c)
		if got == nil || got.ID != "thing-42" {
			t.Errorf("expected thing-42, got %v", got)
		}
	})
}

// TestHubAPI_logger_FallsBackToDefault verifies the nil-safe logger accessor.
func TestHubAPI_logger_FallsBackToDefault(t *testing.T) {
	h := &HubAPI{}
	if h.logger() == nil {
		t.Error("logger() must never return nil")
	}
	h2 := &HubAPI{Logger: silentLog()}
	if h2.logger() != h2.Logger {
		t.Error("logger() should return h.Logger when explicitly set")
	}
}

// TestProjectOverride_Shape verifies wire-shape projection and the defensive
// empty-state fallback (the "upstream bug" guard in hub_api_overrides.go).
func TestProjectOverride_Shape(t *testing.T) {
	t.Run("populated state projected verbatim", func(t *testing.T) {
		raw := json.RawMessage(`{"key":"val"}`)
		state, err := store.NewOverrideState(raw)
		if err != nil {
			t.Fatalf("NewOverrideState: %v", err)
		}
		reason := "test reason"
		setAt := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		ov := store.ThingConfigOverride{
			ThingID:          "t-1",
			ConfigKey:        "routing",
			State:            state,
			TemplateVerAtSet: 3,
			SetBy:            "alice",
			SetAt:            setAt,
			Reason:           &reason,
		}
		resp := projectOverride(ov, 5, true, nil)
		if resp.ConfigKey != "routing" {
			t.Errorf("ConfigKey=%q want routing", resp.ConfigKey)
		}
		if resp.CurrentTemplateVer != 5 {
			t.Errorf("CurrentTemplateVer=%d want 5", resp.CurrentTemplateVer)
		}
		if !resp.Stale {
			t.Error("Stale should be true")
		}
		if resp.SetBy != "alice" {
			t.Errorf("SetBy=%q want alice", resp.SetBy)
		}
		if string(resp.State) != `{"key":"val"}` {
			t.Errorf("State=%s want {\"key\":\"val\"}", resp.State)
		}
	})
	t.Run("empty state falls back to {} (defensive branch)", func(t *testing.T) {
		// Zero-value ThingConfigOverride has empty OverrideState — exercises
		// the "upstream bug" guard that prevents a nil-state crashing the UI.
		ov := store.ThingConfigOverride{ThingID: "t-1", ConfigKey: "routing"}
		resp := projectOverride(ov, 1, false, nil)
		if string(resp.State) != "{}" {
			t.Errorf("State=%s; expected {} fallback", resp.State)
		}
	})
	t.Run("empty state fallback logs with provided logger (no panic)", func(t *testing.T) {
		ov := store.ThingConfigOverride{ThingID: "t-1", ConfigKey: "routing"}
		resp := projectOverride(ov, 1, false, silentLog())
		if string(resp.State) != "{}" {
			t.Errorf("State=%s; expected {} fallback with logger", resp.State)
		}
	})
}

// TestHandleErr_Dispatch verifies the two-arm dispatch (ErrNotFound → 404, other → 500).
func TestHandleErr_Dispatch(t *testing.T) {
	e := newTestEcho()

	t.Run("ErrNotFound maps to 404 NOT_FOUND", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handleErr(c, store.ErrNotFound)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status=%d want 404", rec.Code)
		}
		m := decodeResp(t, rec)
		if errCode(m) != "NOT_FOUND" {
			t.Errorf("code=%v want NOT_FOUND", errCode(m))
		}
	})
	t.Run("generic error maps to 500 INTERNAL_ERROR", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = handleErr(c, someTestError("db connection refused"))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status=%d want 500", rec.Code)
		}
		m := decodeResp(t, rec)
		if errCode(m) != "INTERNAL_ERROR" {
			t.Errorf("code=%v want INTERNAL_ERROR", errCode(m))
		}
	})
}

type someTestError string

func (e someTestError) Error() string { return string(e) }

// HubAPI.ConfigUpdate — field-validation pre-manager branches

func TestHubAPI_ConfigUpdate_MissingThingType(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"configKey": "routing"}, nil)
	_ = h.ConfigUpdate(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
	if errCode(decodeResp(t, rec)) != "INVALID_REQUEST" {
		t.Error("code should be INVALID_REQUEST")
	}
}

func TestHubAPI_ConfigUpdate_MissingConfigKey(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"thingType": "agent"}, nil)
	_ = h.ConfigUpdate(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestHubAPI_ConfigUpdate_BothEmpty(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{}, nil)
	_ = h.ConfigUpdate(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

// HubAPI.ResyncThing — empty id rejected before manager

func TestHubAPI_ResyncThing_EmptyID_Rejected(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{}, map[string]string{"id": ""})
	_ = h.ResyncThing(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for empty id", rec.Code)
	}
}

// HubAPI.SetThingOverride — validation branches (pre-manager)

func TestHubAPI_SetThingOverride_MissingID(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodPut, map[string]any{"state": map[string]any{}},
		map[string]string{"id": "", "configKey": "routing"})
	_ = h.SetThingOverride(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestHubAPI_SetThingOverride_MissingConfigKey(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodPut, map[string]any{"state": map[string]any{}},
		map[string]string{"id": "t-1", "configKey": ""})
	_ = h.SetThingOverride(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestHubAPI_SetThingOverride_BlacklistedKey_Rejected(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	// The handler calls validateOverrideBody which blacklists "credentials".
	body := setOverrideBody{State: json.RawMessage(`{"x":1}`)}
	c, rec := echoCtxJSON(e, http.MethodPut, body,
		map[string]string{"id": "t-1", "configKey": "credentials"})
	_ = h.SetThingOverride(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for blacklisted key", rec.Code)
	}
}

func TestHubAPI_SetThingOverride_NonObjectState_Rejected(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	// JSON encodes the slice as an array — triggers isJSONObject check.
	body := map[string]any{"state": []int{1, 2, 3}}
	c, rec := echoCtxJSON(e, http.MethodPut, body,
		map[string]string{"id": "t-1", "configKey": "routing"})
	_ = h.SetThingOverride(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for non-object state", rec.Code)
	}
}

// HubAPI.ClearThingOverride — empty params rejected

func TestHubAPI_ClearThingOverride_EmptyID_Rejected(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodDelete, nil,
		map[string]string{"id": "", "configKey": "routing"})
	_ = h.ClearThingOverride(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestHubAPI_ClearThingOverride_EmptyConfigKey_Rejected(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodDelete, nil,
		map[string]string{"id": "t-1", "configKey": ""})
	_ = h.ClearThingOverride(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

// HubAPI.ListThingOverrides — empty id rejected

func TestHubAPI_ListThingOverrides_EmptyID_Rejected(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": ""})
	_ = h.ListThingOverrides(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

// HubAPI.ListGlobalOverrides — hasTtl / stale parse-error branches (returns
// 400 before touching the manager, matching the documented "surface the parse
// failure" invariant).

func TestHubAPI_ListGlobalOverrides_InvalidHasTtl_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxQuery(e, "hasTtl=yes")
	_ = h.ListGlobalOverrides(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for hasTtl=yes", rec.Code)
	}
	m := decodeResp(t, rec)
	if !strings.Contains(errMsg(m), "hasTtl") {
		t.Errorf("error msg should mention hasTtl, got %q", errMsg(m))
	}
}

func TestHubAPI_ListGlobalOverrides_InvalidStale_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxQuery(e, "stale=nope")
	_ = h.ListGlobalOverrides(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for stale=nope", rec.Code)
	}
	m := decodeResp(t, rec)
	if !strings.Contains(errMsg(m), "stale") {
		t.Errorf("error msg should mention stale, got %q", errMsg(m))
	}
}

// HubAPI.ListJobs — nil scheduler + search/enabled/pagination filters

func TestHubAPI_ListJobs_NilScheduler_Returns200WithEmptyList(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxQuery(e, "")
	_ = h.ListJobs(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200", rec.Code)
	}
	m := decodeResp(t, rec)
	if m["total"] != float64(0) {
		t.Errorf("total=%v want 0", m["total"])
	}
}

func TestHubAPI_ListJobs_SearchFilter_MatchesByNameCaseInsensitive(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{Scheduler: buildScheduler([]testJob{
		{id: "j1", name: "smart-group-recompute", enabled: true},
		{id: "j2", name: "cert-renewal", enabled: false},
		{id: "j3", name: "smart-group-evict", enabled: true},
	})}
	c, rec := echoCtxQuery(e, "search=smart")
	_ = h.ListJobs(c)
	m := decodeResp(t, rec)
	if m["total"] != float64(2) {
		t.Errorf("total=%v want 2 for search=smart", m["total"])
	}
}

func TestHubAPI_ListJobs_EnabledTrue_FiltersDisabled(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{Scheduler: buildScheduler([]testJob{
		{id: "j1", name: "a", enabled: true},
		{id: "j2", name: "b", enabled: false},
		{id: "j3", name: "c", enabled: true},
	})}
	c, rec := echoCtxQuery(e, "enabled=true")
	_ = h.ListJobs(c)
	m := decodeResp(t, rec)
	if m["total"] != float64(2) {
		t.Errorf("total=%v want 2 for enabled=true", m["total"])
	}
}

func TestHubAPI_ListJobs_EnabledFalse_ReturnsOnlyDisabled(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{Scheduler: buildScheduler([]testJob{
		{id: "j1", name: "a", enabled: true},
		{id: "j2", name: "b", enabled: false},
	})}
	c, rec := echoCtxQuery(e, "enabled=false")
	_ = h.ListJobs(c)
	m := decodeResp(t, rec)
	if m["total"] != float64(1) {
		t.Errorf("total=%v want 1 for enabled=false", m["total"])
	}
}

func TestHubAPI_ListJobs_OffsetBeyondTotal_ReturnsEmptySlice(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{Scheduler: buildScheduler([]testJob{
		{id: "j1", name: "a", enabled: true},
	})}
	c, rec := echoCtxQuery(e, "offset=999")
	_ = h.ListJobs(c)
	m := decodeResp(t, rec)
	jobs, _ := m["jobs"].([]any)
	if len(jobs) != 0 {
		t.Errorf("jobs len=%d want 0 for offset beyond total", len(jobs))
	}
	// total still reflects the unsliced count so clients can compute page counts.
	if m["total"] != float64(1) {
		t.Errorf("total=%v want 1 (unsliced count even at offset beyond)", m["total"])
	}
}

func TestHubAPI_ListJobs_PaginationSlice_CorrectWindow(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{Scheduler: buildScheduler([]testJob{
		{id: "j1", name: "a", enabled: true},
		{id: "j2", name: "b", enabled: true},
		{id: "j3", name: "c", enabled: true},
	})}
	c, rec := echoCtxQuery(e, "limit=2&offset=1")
	_ = h.ListJobs(c)
	m := decodeResp(t, rec)
	jobs, _ := m["jobs"].([]any)
	if len(jobs) != 2 {
		t.Errorf("jobs len=%d want 2 for limit=2&offset=1", len(jobs))
	}
	if m["total"] != float64(3) {
		t.Errorf("total=%v want 3", m["total"])
	}
}

// HubAPI.GetJob — nil scheduler + job not found

func TestHubAPI_GetJob_NilScheduler_Returns404(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "j1"})
	_ = h.GetJob(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404", rec.Code)
	}
}

func TestHubAPI_GetJob_UnknownJob_Returns404(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{Scheduler: buildScheduler(nil)}
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "nonexistent"})
	_ = h.GetJob(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 for missing job", rec.Code)
	}
}

// HubAPI.TriggerJob — nil scheduler + success shape

func TestHubAPI_TriggerJob_NilScheduler_Returns404(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, nil, map[string]string{"id": "j1"})
	_ = h.TriggerJob(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404", rec.Code)
	}
}

func TestHubAPI_TriggerJob_KnownJob_ReturnsOkWithTriggeredAt(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{Scheduler: buildScheduler([]testJob{{id: "j1", name: "recompute", enabled: true}})}
	c, rec := echoCtxJSON(e, http.MethodPost, nil, map[string]string{"id": "j1"})
	_ = h.TriggerJob(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200", rec.Code)
	}
	m := decodeResp(t, rec)
	if m["ok"] != true {
		t.Errorf("ok=%v want true", m["ok"])
	}
	if _, ok := m["triggeredAt"].(string); !ok {
		t.Error("triggeredAt must be a string")
	}
}

// HubAPI.UpdateJob — enabled required gate + disable/enable paths

func TestHubAPI_UpdateJob_NilScheduler_Returns404(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodPut, map[string]any{"enabled": true}, map[string]string{"id": "j1"})
	_ = h.UpdateJob(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404", rec.Code)
	}
}

func TestHubAPI_UpdateJob_MissingEnabledField_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{Scheduler: buildScheduler([]testJob{{id: "j1", name: "a", enabled: true}})}
	// Body omits "enabled" — req.Enabled == nil after Bind → 400.
	c, rec := echoCtxJSON(e, http.MethodPut, map[string]any{}, map[string]string{"id": "j1"})
	_ = h.UpdateJob(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestHubAPI_UpdateJob_DisableJob_ReturnsEnabledFalse(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{Scheduler: buildScheduler([]testJob{{id: "j1", name: "a", enabled: true}})}
	enabled := false
	c, rec := echoCtxJSON(e, http.MethodPut, map[string]any{"enabled": enabled}, map[string]string{"id": "j1"})
	_ = h.UpdateJob(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200", rec.Code)
	}
	m := decodeResp(t, rec)
	if m["enabled"] != false {
		t.Errorf("enabled=%v want false", m["enabled"])
	}
}

// HubAPI.ListJobRuns — nil scheduler returns empty

func TestHubAPI_ListJobRuns_NilScheduler_ReturnsEmpty(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "j1"})
	_ = h.ListJobRuns(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200", rec.Code)
	}
	m := decodeResp(t, rec)
	if m["total"] != float64(0) {
		t.Errorf("total=%v want 0", m["total"])
	}
}

// HubAPI.GenerateEnrollmentToken — label required

func TestHubAPI_GenerateEnrollmentToken_MissingLabel_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{}, nil)
	_ = h.GenerateEnrollmentToken(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
	if errCode(decodeResp(t, rec)) != "INVALID_REQUEST" {
		t.Error("code should be INVALID_REQUEST")
	}
}

// InternalThingsAPI.Register — id+type required

func TestInternalThingsAPI_Register_MissingID_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"type": "agent"}, nil)
	_ = h.Register(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestInternalThingsAPI_Register_MissingType_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"id": "t-1"}, nil)
	_ = h.Register(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

// InternalThingsAPI.Heartbeat — id+status required

func TestInternalThingsAPI_Heartbeat_MissingFields(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	tests := []struct {
		name string
		body map[string]any
	}{
		{"missing id", map[string]any{"status": "online"}},
		{"missing status", map[string]any{"id": "t-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, rec := echoCtxJSON(e, http.MethodPost, tt.body, nil)
			_ = h.Heartbeat(c)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status=%d want 400 (%s)", rec.Code, tt.name)
			}
		})
	}
}

// InternalThingsAPI.ShadowReport — all named validation gates

func TestInternalThingsAPI_ShadowReport_ValidationBranches(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	tests := []struct {
		name        string
		body        map[string]any
		wantContain string
	}{
		{"missing id", map[string]any{"reported": map[string]any{}}, "id is required"},
		{"missing reported", map[string]any{"id": "t-1"}, "reported is required"},
		{"negative reportedVer", map[string]any{"id": "t-1", "reported": map[string]any{}, "reportedVer": -1}, "non-negative"},
		// Break-glass now has a dedicated route; the normal shadow path rejects
		// ANY reason (closes the hand-crafted /shadow break-glass bypass).
		{"reason rejected (hacky)", map[string]any{"id": "t-1", "reported": map[string]any{}, "reason": "hacky"}, "must not carry a reason"},
		{"reason rejected (break_glass)", map[string]any{"id": "t-1", "reported": map[string]any{}, "reason": "break_glass"}, "must not carry a reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, rec := echoCtxJSON(e, http.MethodPost, tt.body, nil)
			_ = h.ShadowReport(c)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status=%d want 400", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tt.wantContain) {
				t.Errorf("response %q does not contain %q", body, tt.wantContain)
			}
		})
	}
}

// TestInternalThingsAPI_BreakGlassReport_ValidationBranches exercises the named
// 400 gates on the dedicated break-glass route. Each case is rejected before
// the (nil) Manager is touched: id/reported/reportedVer shape, missing
// actorTokenId, the F-0139 allowlist (non-writable key), and the F-0139 schema
// gate (malformed state for a writable key).
func TestInternalThingsAPI_BreakGlassReport_ValidationBranches(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	tests := []struct {
		name        string
		body        map[string]any
		wantContain string
	}{
		{"missing id", map[string]any{"reported": map[string]any{}}, "id is required"},
		{"missing reported", map[string]any{"id": "t-1"}, "reported is required"},
		{"negative reportedVer", map[string]any{"id": "t-1", "reported": map[string]any{}, "reportedVer": -1}, "non-negative"},
		{"missing actorTokenId", map[string]any{"id": "t-1", "reported": map[string]any{}}, "actorTokenId"},
		{
			"non-allowlisted key rejected",
			map[string]any{
				"id": "t-1", "reported": map[string]any{"credentials": map[string]any{"x": 1}},
				"keyVersions": map[string]any{"credentials": 4}, "actorTokenId": "a1b2c3d4",
			},
			"allowlist",
		},
		{
			"malformed killswitch state rejected",
			map[string]any{
				"id": "t-1", "reported": map[string]any{"killswitch": "engaged"},
				"keyVersions": map[string]any{"killswitch": 4}, "actorTokenId": "a1b2c3d4",
			},
			"schema validation",
		},
		{
			"unknown field in killswitch state rejected",
			map[string]any{
				"id": "t-1", "reported": map[string]any{"killswitch": map[string]any{"enabled": true}},
				"keyVersions": map[string]any{"killswitch": 4}, "actorTokenId": "a1b2c3d4",
			},
			"schema validation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, rec := echoCtxJSON(e, http.MethodPost, tt.body, nil)
			_ = h.BreakGlassReport(c)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status=%d want 400; body=%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, tt.wantContain) {
				t.Errorf("response %q does not contain %q", body, tt.wantContain)
			}
		})
	}
}

// InternalThingsAPI.BulkConfigPull — type param required

func TestInternalThingsAPI_BulkConfigPull_MissingType(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	req := httptest.NewRequest(http.MethodGet, "/api/internal/things/config", nil)
	rec := httptest.NewRecorder()
	_ = h.BulkConfigPull(e.NewContext(req, rec))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

// InternalThingsAPI.SingleConfigPull — type param required

func TestInternalThingsAPI_SingleConfigPull_MissingType(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{CatB: store.NewCatBRegistry()}
	req := httptest.NewRequest(http.MethodGet, "/api/internal/things/config/routing", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("key")
	c.SetParamValues("routing")
	_ = h.SingleConfigPull(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

// InternalThingsAPI.AuditUpload — all named validation and security branches

func TestInternalThingsAPI_AuditUpload_MissingThingID(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"events": []any{map[string]any{"id": "e1"}}}, nil)
	_ = h.AuditUpload(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestInternalThingsAPI_AuditUpload_EmptyEvents(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"thingId": "t-1", "events": []any{}}, nil)
	_ = h.AuditUpload(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestInternalThingsAPI_AuditUpload_ThingIDMismatch_Returns403(t *testing.T) {
	// Device-token caller: body thingId differs from authenticated Thing → 403.
	e := newTestEcho()
	h := &InternalThingsAPI{}
	body := map[string]any{"thingId": "t-other", "events": []any{map[string]any{"id": "e1"}}}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(thingContextKey, &store.Thing{ID: "t-1", Name: "device-1"})
	_ = h.AuditUpload(c)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", rec.Code)
	}
}

func TestInternalThingsAPI_AuditUpload_NilMQProducer_Returns503(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{MQProducer: nil}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"thingId": "t-1", "events": []any{map[string]any{"id": "e1"}}}, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"}) // device-token caller on its own id
	_ = h.AuditUpload(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", rec.Code)
	}
}

func TestInternalThingsAPI_AuditUpload_SourceStamp_And_EmptyStatusStrip(t *testing.T) {
	// Named failure modes / behavioural rules from the inline comment:
	// 1. Events missing "source" must be stamped with "agent".
	// 2. Events with empty "usageExtractionStatus" must have that field stripped.
	e := newTestEcho()
	mq := &recordingMQProducer{}
	h := &InternalThingsAPI{MQProducer: mq}
	body := map[string]any{
		"thingId": "t-1",
		"events":  []any{map[string]any{"id": "e1", "usageExtractionStatus": ""}},
	}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"}) // device-token caller on its own id
	_ = h.AuditUpload(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200", rec.Code)
	}
	if len(mq.payloads) == 0 {
		t.Fatal("expected at least one enqueued payload")
	}
	var evt map[string]any
	if err := json.Unmarshal(mq.payloads[0], &evt); err != nil {
		t.Fatalf("unmarshal enqueued payload: %v", err)
	}
	if evt["source"] != "agent" {
		t.Errorf("source=%v want 'agent' (missing source must be stamped)", evt["source"])
	}
	if _, ok := evt["usageExtractionStatus"]; ok {
		t.Error("usageExtractionStatus must be stripped when empty")
	}
}

func TestInternalThingsAPI_AuditUpload_PreexistingSource_Preserved(t *testing.T) {
	// Rule: pre-existing "source" field must not be overwritten.
	e := newTestEcho()
	mq := &recordingMQProducer{}
	h := &InternalThingsAPI{MQProducer: mq}
	body := map[string]any{
		"thingId": "t-1",
		"events":  []any{map[string]any{"id": "e1", "source": "gateway"}},
	}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"}) // device-token caller on its own id
	_ = h.AuditUpload(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200", rec.Code)
	}
	var evt map[string]any
	_ = json.Unmarshal(mq.payloads[0], &evt)
	if evt["source"] != "gateway" {
		t.Errorf("source=%v want 'gateway' (pre-existing source must not be overwritten)", evt["source"])
	}
}

// InternalThingsAPI.ExemptionUpload — validation + security branches

func TestInternalThingsAPI_ExemptionUpload_MissingThingID(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"host": "example.com", "expiresAt": time.Now().Add(time.Hour)}, nil)
	_ = h.ExemptionUpload(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestInternalThingsAPI_ExemptionUpload_MissingHost(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"thingId": "t-1", "expiresAt": time.Now().Add(time.Hour)}, nil)
	_ = h.ExemptionUpload(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestInternalThingsAPI_ExemptionUpload_MissingExpiresAt(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"thingId": "t-1", "host": "example.com"}, nil)
	_ = h.ExemptionUpload(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

func TestInternalThingsAPI_ExemptionUpload_ThingIDMismatch_Returns403(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	body := map[string]any{"thingId": "t-other", "host": "example.com", "expiresAt": time.Now().Add(time.Hour)}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"})
	_ = h.ExemptionUpload(c)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", rec.Code)
	}
}

func TestInternalThingsAPI_ExemptionUpload_NilMQ_Returns503(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{MQProducer: nil}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"thingId": "t-1", "host": "example.com", "expiresAt": time.Now().Add(time.Hour)}, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"}) // device-token caller on its own id
	_ = h.ExemptionUpload(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", rec.Code)
	}
}

// InternalThingsAPI.UpdateCheck — named failure modes

func TestInternalThingsAPI_UpdateCheck_MissingCurrentVersion_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	_ = h.UpdateCheck(e.NewContext(req, rec))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

// InternalThingsAPI.Deregister — id required

func TestInternalThingsAPI_Deregister_MissingID_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{}, nil)
	_ = h.Deregister(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

// Stubs / fakes

// recordingMQProducer records every Enqueue payload for assertion.
type recordingMQProducer struct {
	mu       sync.Mutex
	payloads [][]byte
}

func (r *recordingMQProducer) Enqueue(_ context.Context, _ string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	r.payloads = append(r.payloads, cp)
	return nil
}

func (r *recordingMQProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (r *recordingMQProducer) Close() error                                        { return nil }

// testJob is a simple descriptor for seeding a real *scheduler.Scheduler.
type testJob struct {
	id, name, description string
	enabled               bool
}

// simpleJob implements scheduler.Job for seeding.
type simpleJob struct {
	id, name, description string
}

func (j simpleJob) ID() string                  { return j.id }
func (j simpleJob) Name() string                { return j.name }
func (j simpleJob) Description() string         { return j.description }
func (j simpleJob) Interval() time.Duration     { return 5 * time.Minute }
func (j simpleJob) Run(_ context.Context) error { return nil }

// buildScheduler creates a real *scheduler.Scheduler seeded with the given jobs.
// scheduler.Register defaults every job to enabled=true; jobs with enabled=false
// have their enabled flag manually set via SetEnabled so the handler's filter
// tests work correctly. (SetEnabled with no store only flips the in-memory flag.)
func buildScheduler(jobs []testJob) *scheduler.Scheduler {
	s := scheduler.New(silentLog())
	for _, j := range jobs {
		s.Register(simpleJob{id: j.id, name: j.name, description: j.description})
		if !j.enabled {
			// SetEnabled with no attached store flips the in-memory enabled
			// flag without a DB call, which is what we want for handler tests.
			_ = s.SetEnabled(context.Background(), j.id, false)
		}
	}
	return s
}

// --- F-0060: cross-Thing identity binding on the internal Things API ---

// TestRequireThingMatch unit-tests the shared authorization predicate: a
// service-token caller (no context Thing) is always allowed; a device-token
// caller is allowed only for its own id and blocked (403) for any other id.
func TestRequireThingMatch(t *testing.T) {
	e := newTestEcho()
	mk := func() (echo.Context, *httptest.ResponseRecorder) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		return e.NewContext(req, rec), rec
	}

	if c, _ := mk(); requireThingMatch(c, "anything") {
		t.Error("service-token caller (nil Thing) must not be blocked")
	}
	cMatch, _ := mk()
	cMatch.Set(thingContextKey, &store.Thing{ID: "t-1"})
	if requireThingMatch(cMatch, "t-1") {
		t.Error("device caller operating on its own id must not be blocked")
	}
	cMis, rec := mk()
	cMis.Set(thingContextKey, &store.Thing{ID: "t-1"})
	if !requireThingMatch(cMis, "t-2") {
		t.Fatal("device caller operating on another id must be blocked")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if errCode(body) != "FORBIDDEN" {
		t.Errorf("code=%v want FORBIDDEN", errCode(body))
	}
}

// TestInternalThingsAPI_CrossThingBinding_DeviceMismatch_Returns403 is the
// F-0060 regression guard: every handler that operates on a body/query thing id
// rejects a device token whose authenticated id differs from the operated id. A
// bare handler suffices — the guard short-circuits before any manager use.
func TestInternalThingsAPI_CrossThingBinding_DeviceMismatch_Returns403(t *testing.T) {
	e := newTestEcho()
	const self, victim = "t-self", "t-victim"

	cases := []struct {
		name   string
		ctx    func() (echo.Context, *httptest.ResponseRecorder)
		invoke func(h *InternalThingsAPI, c echo.Context) error
	}{
		{
			name: "Register",
			ctx: func() (echo.Context, *httptest.ResponseRecorder) {
				return echoCtxJSON(e, http.MethodPost, map[string]any{"id": victim, "type": "agent"}, nil)
			},
			invoke: func(h *InternalThingsAPI, c echo.Context) error { return h.Register(c) },
		},
		{
			name: "Heartbeat",
			ctx: func() (echo.Context, *httptest.ResponseRecorder) {
				return echoCtxJSON(e, http.MethodPost, map[string]any{"id": victim, "status": "online"}, nil)
			},
			invoke: func(h *InternalThingsAPI, c echo.Context) error { return h.Heartbeat(c) },
		},
		{
			name: "ShadowReport",
			ctx: func() (echo.Context, *httptest.ResponseRecorder) {
				return echoCtxJSON(e, http.MethodPost, map[string]any{"id": victim, "reported": map[string]any{}, "reportedVer": 0}, nil)
			},
			invoke: func(h *InternalThingsAPI, c echo.Context) error { return h.ShadowReport(c) },
		},
		{
			name: "BreakGlassReport",
			ctx: func() (echo.Context, *httptest.ResponseRecorder) {
				// Valid break-glass body (killswitch + keyVersions) so the F-0139
				// validation passes and the request reaches the object-authority
				// gate, which then blocks the cross-Thing access.
				return echoCtxJSON(e, http.MethodPost, map[string]any{"id": victim, "reported": map[string]any{"killswitch": map[string]any{"engaged": true}}, "reportedVer": 4, "keyVersions": map[string]any{"killswitch": 4}, "actorTokenId": "a1b2c3d4"}, nil)
			},
			invoke: func(h *InternalThingsAPI, c echo.Context) error { return h.BreakGlassReport(c) },
		},
		{
			name: "Deregister",
			ctx: func() (echo.Context, *httptest.ResponseRecorder) {
				return echoCtxJSON(e, http.MethodPost, map[string]any{"id": victim}, nil)
			},
			invoke: func(h *InternalThingsAPI, c echo.Context) error { return h.Deregister(c) },
		},
		{
			name:   "BulkConfigPull",
			ctx:    func() (echo.Context, *httptest.ResponseRecorder) { return echoCtxQuery(e, "type=agent&id="+victim) },
			invoke: func(h *InternalThingsAPI, c echo.Context) error { return h.BulkConfigPull(c) },
		},
		{
			name: "GetAttestationPubKey",
			ctx: func() (echo.Context, *httptest.ResponseRecorder) {
				return echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": victim})
			},
			invoke: func(h *InternalThingsAPI, c echo.Context) error { return h.GetAttestationPubKey(c) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &InternalThingsAPI{}
			c, rec := tc.ctx()
			c.Set(thingContextKey, &store.Thing{ID: self})
			_ = tc.invoke(h, c)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d want 403 (cross-Thing access must be blocked); body=%s", rec.Code, rec.Body.String())
			}
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if errCode(body) != "FORBIDDEN" {
				t.Errorf("code=%v want FORBIDDEN", errCode(body))
			}
		})
	}
}
