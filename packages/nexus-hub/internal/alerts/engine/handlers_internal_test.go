package alerting_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	alertclient "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/client"
	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// mockRaiser captures calls without touching a database. It implements the
// unexported raiserAPI interface in the alerting package structurally — any
// type with matching Raise/Resolve methods satisfies it when passed to
// HandleRaise / HandleResolve.
type mockRaiser struct {
	raiseInput  *alerting.RaiseInput
	raiseErr    error
	resolveArgs *struct{ RuleID, TargetKey, Reason string }
	resolveErr  error
}

func (m *mockRaiser) Raise(_ context.Context, in alerting.RaiseInput) error {
	in2 := in
	m.raiseInput = &in2
	return m.raiseErr
}

func (m *mockRaiser) Resolve(_ context.Context, ruleID, targetKey, reason string) error {
	m.resolveArgs = &struct{ RuleID, TargetKey, Reason string }{ruleID, targetKey, reason}
	return m.resolveErr
}

func TestHandleRaise_Success(t *testing.T) {
	m := &mockRaiser{}
	h := alerting.HandleRaise(m)

	env := alertclient.AlertEnvelope{
		RuleID:      "quota.threshold",
		TargetKey:   "org:x",
		TargetLabel: "Org X",
		Severity:    "high",
		Message:     "95% used",
		Details:     map[string]any{"percent": 95.0},
		FiredAt:     time.Date(2026, 4, 21, 10, 0, 0, 0, time.UTC),
	}
	body, _ := json.Marshal(env)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/raise", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if m.raiseInput == nil {
		t.Fatal("raiser.Raise not called")
	}
	if m.raiseInput.RuleID != "quota.threshold" {
		t.Errorf("RuleID=%q", m.raiseInput.RuleID)
	}
	if m.raiseInput.TargetKey != "org:x" {
		t.Errorf("TargetKey=%q", m.raiseInput.TargetKey)
	}
	if string(m.raiseInput.Severity) != "high" {
		t.Errorf("Severity=%q", m.raiseInput.Severity)
	}
	if !m.raiseInput.FiredAt.Equal(env.FiredAt) {
		t.Errorf("FiredAt=%v", m.raiseInput.FiredAt)
	}
}

func TestHandleRaise_DefaultsFiredAtToNow(t *testing.T) {
	m := &mockRaiser{}
	h := alerting.HandleRaise(m)
	// Zero FiredAt — handler must populate it.
	env := alertclient.AlertEnvelope{
		RuleID:    "r",
		TargetKey: "t",
		Severity:  "high",
		Message:   "m",
	}
	body, _ := json.Marshal(env)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/raise", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if m.raiseInput == nil || m.raiseInput.FiredAt.IsZero() {
		t.Fatalf("FiredAt not populated: %+v", m.raiseInput)
	}
}

func TestHandleRaise_BadJSON(t *testing.T) {
	h := alerting.HandleRaise(&mockRaiser{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/raise", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestHandleRaise_MissingRuleID(t *testing.T) {
	h := alerting.HandleRaise(&mockRaiser{})
	env := alertclient.AlertEnvelope{TargetKey: "t", Severity: "high"}
	body, _ := json.Marshal(env)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/raise", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleRaise_UnknownRule(t *testing.T) {
	m := &mockRaiser{raiseErr: errors.New("raise: unknown ruleId \"bogus\"")}
	h := alerting.HandleRaise(m)
	env := alertclient.AlertEnvelope{RuleID: "bogus", TargetKey: "t", Severity: "high"}
	body, _ := json.Marshal(env)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/raise", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	// Verify JSON error body has nested {error:{message,...}} shape (F-0319).
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	inner, _ := resp["error"].(map[string]any)
	if inner == nil {
		t.Fatalf("missing error object in body: %s", rec.Body.String())
	}
	if msg, _ := inner["message"].(string); msg == "" {
		t.Fatalf("missing error.message in body: %s", rec.Body.String())
	}
}

// raiseWithCaller posts a raise envelope with the given caller stamped into the
// request context and returns the recorder.
func raiseWithCaller(t *testing.T, m *mockRaiser, caller alerting.Caller, env alertclient.AlertEnvelope) *httptest.ResponseRecorder {
	t.Helper()
	h := alerting.HandleRaise(m)
	body, _ := json.Marshal(env)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/raise", bytes.NewReader(body))
	req = req.WithContext(alerting.WithCaller(req.Context(), caller))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandleRaise_DeviceCallerOwnTargetAllowed(t *testing.T) {
	m := &mockRaiser{}
	rec := raiseWithCaller(t, m,
		alerting.Caller{IsService: false, ThingID: "n1"},
		alertclient.AlertEnvelope{RuleID: "proxy.hook_failure_rate", TargetKey: "proxy:n1", Severity: "high"},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if m.raiseInput == nil || m.raiseInput.TargetKey != "proxy:n1" {
		t.Fatalf("raise not called for own target: %+v", m.raiseInput)
	}
}

func TestHandleRaise_DeviceCallerForeignTargetForbidden(t *testing.T) {
	m := &mockRaiser{}
	rec := raiseWithCaller(t, m,
		alerting.Caller{IsService: false, ThingID: "n1"},
		alertclient.AlertEnvelope{RuleID: "proxy.hook_failure_rate", TargetKey: "proxy:n2", Severity: "high"},
	)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if m.raiseInput != nil {
		t.Fatalf("raiser must NOT be called when device targets a foreign thing; got %+v", m.raiseInput)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	inner, _ := resp["error"].(map[string]any)
	if code, _ := inner["code"].(string); code != "FORBIDDEN" {
		t.Errorf("error envelope code=%q, want FORBIDDEN; body=%s", inner["code"], rec.Body.String())
	}
}

func TestHandleRaise_ServiceCallerAnyTargetAllowed(t *testing.T) {
	m := &mockRaiser{}
	rec := raiseWithCaller(t, m,
		alerting.Caller{IsService: true},
		alertclient.AlertEnvelope{RuleID: "quota.threshold", TargetKey: "org:x", Severity: "high"},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if m.raiseInput == nil || m.raiseInput.TargetKey != "org:x" {
		t.Fatalf("service caller raise must pass through any target: %+v", m.raiseInput)
	}
}

func TestHandleResolve_DeviceCallerForbidden(t *testing.T) {
	m := &mockRaiser{}
	h := alerting.HandleResolve(m)
	body, _ := json.Marshal(alertclient.ResolveRequest{RuleID: "r", TargetKey: "proxy:n1"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/resolve", bytes.NewReader(body))
	req = req.WithContext(alerting.WithCaller(req.Context(), alerting.Caller{IsService: false, ThingID: "n1"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if m.resolveArgs != nil {
		t.Fatalf("device caller must NOT reach Resolve; got %+v", m.resolveArgs)
	}
}

func TestHandleResolve_ServiceCallerAllowed(t *testing.T) {
	m := &mockRaiser{}
	h := alerting.HandleResolve(m)
	body, _ := json.Marshal(alertclient.ResolveRequest{RuleID: "r", TargetKey: "proxy:n1", Reason: "ok"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/resolve", bytes.NewReader(body))
	req = req.WithContext(alerting.WithCaller(req.Context(), alerting.Caller{IsService: true}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if m.resolveArgs == nil || m.resolveArgs.TargetKey != "proxy:n1" {
		t.Fatalf("service caller resolve must pass through: %+v", m.resolveArgs)
	}
}

// TestHubHTTPErr_CanonicalShape verifies F-0319/F-0320: the raw net/http error
// path emits the nested {error:{message,type,code}} envelope matching the Echo
// handlers.
func TestHubHTTPErr_CanonicalShape(t *testing.T) {
	h := alerting.HandleRaise(&mockRaiser{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/raise", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %s", rec.Body.String())
	}
	inner, _ := resp["error"].(map[string]any)
	if inner == nil {
		t.Fatalf("envelope missing error object; got %v", resp)
	}
	if msg, _ := inner["message"].(string); msg == "" {
		t.Errorf("envelope.error.message empty; got %v", resp)
	}
	if code, _ := inner["code"].(string); code != "INVALID_REQUEST" {
		t.Errorf("envelope.error.code=%q; want INVALID_REQUEST", code)
	}
}

func TestHandleResolve_Success(t *testing.T) {
	m := &mockRaiser{}
	h := alerting.HandleResolve(m)
	body, _ := json.Marshal(alertclient.ResolveRequest{
		RuleID:    "quota.threshold",
		TargetKey: "org:x",
		Reason:    "usage dropped",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/resolve", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if m.resolveArgs == nil {
		t.Fatal("raiser.Resolve not called")
	}
	if m.resolveArgs.RuleID != "quota.threshold" || m.resolveArgs.TargetKey != "org:x" || m.resolveArgs.Reason != "usage dropped" {
		t.Errorf("resolve args: %+v", m.resolveArgs)
	}
}
