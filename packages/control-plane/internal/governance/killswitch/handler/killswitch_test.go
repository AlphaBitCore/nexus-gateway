package killswitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	cfginterception "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// Helpers (mirrors cache/cache_test.go pattern; copied per R6 runbook §4.2)

type auditSpy struct {
	mu    sync.Mutex
	calls [][]byte
}

func (a *auditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *auditSpy) Enqueue(_ context.Context, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	a.calls = append(a.calls, copied)
	return nil
}
func (a *auditSpy) Close() error { return nil }

func (a *auditSpy) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *auditSpy) last() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(a.calls[len(a.calls)-1], &m)
	return m
}

// echoContext builds an Echo context with the authenticated admin
// principal populated under the same key the real AdminAuth middleware
// uses.
func echoContext(req *http.Request, rec *httptest.ResponseRecorder, userName, userID string) echo.Context {
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             userID,
		KeyName:           userName,
		AuthPrincipalType: "admin_user",
	})
	return c
}

// fakeHub is a stub HubConfigChanger that captures the last request
// and returns a programmable error. Exercises both the success and
// propagation-failure branches without standing up an HTTP server.
type fakeHub struct {
	mu      sync.Mutex
	hits    int
	calls   []hub.ConfigChangeRequest
	lastReq hub.ConfigChangeRequest
	resp    *hub.ConfigChangeResponse
	err     error
}

func (f *fakeHub) NotifyConfigChange(_ context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	f.calls = append(f.calls, req)
	f.lastReq = req
	return f.resp, f.err
}

func (f *fakeHub) Last() hub.ConfigChangeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

func (f *fakeHub) NotifyCalls() []hub.ConfigChangeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]hub.ConfigChangeRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// newHandler builds a *Handler with the supplied parts and a discard
// logger. A nil spy gets a fresh empty one. fakeHub defaults to a
// successful response with Version=1. The `db` argument is accepted but
// unused (the Post handler does not read CP-side templates); it stays
// in the signature so the per-test call sites read symmetrically and
// future DB-touching write paths can plumb it through without a fan-out
// rewrite of every test.
func newHandler(t *testing.T, _ any, fh *fakeHub, spy *auditSpy) *Handler {
	t.Helper()
	if spy == nil {
		spy = &auditSpy{}
	}
	if fh != nil && fh.resp == nil && fh.err == nil {
		fh.resp = &hub.ConfigChangeResponse{Version: 1, ThingsNotified: 1, ThingsOnline: 1}
	}
	var h HubConfigChanger
	if fh != nil {
		h = fh
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		logger = slog.New(slog.NewTextHandler(testLogger{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return New(Deps{
		Hub:    h,
		Audit:  audit.NewWriter(spy, "audit", logger),
		Logger: logger,
	})
}

type testLogger struct{ t *testing.T }

func (l testLogger) Write(p []byte) (int, error) {
	l.t.Helper()
	l.t.Log(string(p))
	return len(p), nil
}

// newAdminContext sugar: assembles a typical POST request + Echo context
// with an admin principal already injected.
func newAdminContext(t *testing.T, method, path string, body []byte) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return echoContext(req, rec, "alice", "user-1"), rec
}

// Post — toggle the killswitch via Hub

// TestPost_NotifiesHubWithNewState locks the happy path: a valid toggle
// reaches Hub with the full Killswitch payload, the caller identity,
// and the engage/disengage action derived from the new state. The
// kill-switch fan-out covers both compliance-proxy AND agent so an
// engaged kill switch stops every Thing that performs TLS bumping. Each
// leg gets its own NotifyConfigChange call with identical payload — the
// admin response surfaces the compliance-proxy version (the primary leg)
// plus the summed thingsNotified / thingsOnline counts across both legs.
func TestPost_NotifiesHubWithNewState(t *testing.T) {
	hub := &fakeHub{}
	aud := &auditSpy{}
	h := newHandler(t, nil, hub, aud)

	body, _ := json.Marshal(map[string]any{"engaged": true})
	c, rec := newAdminContext(t, http.MethodPost, "/api/admin/compliance/killswitch", body)

	if err := h.Post(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if aud.count() != 1 {
		t.Fatalf("admin audit enqueue count = %d; want 1", aud.count())
	}
	if got := aud.last()["entityType"]; got != "kill-switch" {
		t.Fatalf("audit entityType = %v; want kill-switch", got)
	}

	calls := hub.NotifyCalls()
	if len(calls) != 2 {
		t.Fatalf("NotifyConfigChange call count = %d; want 2 (compliance-proxy + agent); calls=%+v", len(calls), calls)
	}
	if calls[0].ThingType != "compliance-proxy" {
		t.Fatalf("first leg ThingType = %q; want compliance-proxy (primary leg must be first so its response carries the wire-compat version)", calls[0].ThingType)
	}
	if calls[1].ThingType != "agent" {
		t.Fatalf("second leg ThingType = %q; want agent", calls[1].ThingType)
	}

	for i, sent := range calls {
		if sent.ConfigKey != "killswitch" {
			t.Errorf("call #%d configKey = %q, want killswitch", i, sent.ConfigKey)
		}
		if sent.Action != "engage" {
			t.Errorf("call #%d action = %q, want engage", i, sent.Action)
		}
		if sent.ActorID != "user-1" || sent.ActorName != "alice" {
			t.Errorf("call #%d actor not propagated: id=%q name=%q", i, sent.ActorID, sent.ActorName)
		}

		// State travels as the concrete Killswitch struct. Round-trip
		// through JSON so the assertion matches what Hub actually
		// receives over the wire — both legs MUST carry the same
		// Engaged value or the fleet ends up in a split state.
		raw, err := json.Marshal(sent.State)
		if err != nil {
			t.Fatalf("call #%d marshal state: %v", i, err)
		}
		var ks cfginterception.Killswitch
		if err := json.Unmarshal(raw, &ks); err != nil {
			t.Fatalf("call #%d unmarshal state: %v", i, err)
		}
		if !ks.Engaged {
			t.Errorf("call #%d expected engaged=true in notify payload, got %+v", i, ks)
		}
	}

	var respBody struct {
		Engaged        bool  `json:"engaged"`
		Version        int64 `json:"version"`
		ThingsNotified int   `json:"thingsNotified"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if !respBody.Engaged {
		t.Errorf("response.engaged = false, want true")
	}
	if respBody.Version != 1 {
		t.Errorf("response.version = %d, want 1 (primary leg compliance-proxy)", respBody.Version)
	}
	// fakeHub returns ThingsNotified=1 per call by default; both legs
	// contribute so the admin sees the fleet-wide total.
	if respBody.ThingsNotified != 2 {
		t.Errorf("response.thingsNotified = %d, want 2 (1 per fan-out leg)", respBody.ThingsNotified)
	}
}

// TestPost_DisengageDerivesAction verifies the action field swings to
// "disengage" when engaged=false. Both fan-out legs receive the same
// action — engaging the kill switch on compliance-proxy but leaving
// the agent's "engage" state stale would silently keep half the fleet
// bumping traffic.
func TestPost_DisengageDerivesAction(t *testing.T) {
	hub := &fakeHub{}
	h := newHandler(t, nil, hub, nil)

	body, _ := json.Marshal(map[string]any{"engaged": false})
	c, rec := newAdminContext(t, http.MethodPost, "/api/admin/compliance/killswitch", body)

	if err := h.Post(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	calls := hub.NotifyCalls()
	if len(calls) != 2 {
		t.Fatalf("NotifyConfigChange call count = %d; want 2; calls=%+v", len(calls), calls)
	}
	for i, c := range calls {
		if c.Action != "disengage" {
			t.Fatalf("call #%d (thingType=%q) action = %q, want disengage", i, c.ThingType, c.Action)
		}
	}
}

// TestPost_AgentLegFailureDoesNotAbortComplianceProxy locks the
// best-effort behaviour of the secondary leg: a Hub error on the agent
// leg MUST NOT roll back the compliance-proxy update. The operator's
// intent is "stop bumping everywhere" — preserving the
// compliance-proxy success path matters more than rolling back to a
// consistent state, since the drift reconciler will re-push to agent
// on its next tick.
func TestPost_AgentLegFailureDoesNotAbortComplianceProxy(t *testing.T) {
	fh := &flakeyHub{
		failOnThingType: thingTypeAgent,
		errOnFail:       errors.New("agent leg timeout"),
		successResp:     &hub.ConfigChangeResponse{Version: 5, ThingsNotified: 1, ThingsOnline: 1},
	}
	aud := &auditSpy{}
	h := newHandler(t, nil, nil, aud)
	h.hub = fh

	body, _ := json.Marshal(map[string]any{"engaged": true})
	c, rec := newAdminContext(t, http.MethodPost, "/api/admin/compliance/killswitch", body)

	_ = h.Post(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent-leg failure must not abort compliance-proxy success: code=%d body=%s", rec.Code, rec.Body.String())
	}
	calls := fh.NotifyCalls()
	if len(calls) != 2 {
		t.Fatalf("both legs must be attempted even when one fails: got %d calls", len(calls))
	}
	if calls[0].ThingType != "compliance-proxy" || calls[1].ThingType != "agent" {
		t.Fatalf("fan-out order wrong: %q then %q", calls[0].ThingType, calls[1].ThingType)
	}
	if aud.count() != 1 {
		t.Errorf("admin audit entry expected on partial fan-out success, got %d", aud.count())
	}
	// Response should reflect only the successful leg's counts.
	var respBody struct {
		Version        int64 `json:"version"`
		ThingsNotified int   `json:"thingsNotified"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if respBody.Version != 5 {
		t.Errorf("response.version = %d; want 5 (from primary leg only)", respBody.Version)
	}
	if respBody.ThingsNotified != 1 {
		t.Errorf("response.thingsNotified = %d; want 1 (agent leg failed, only proxy contributes)", respBody.ThingsNotified)
	}
}

// flakeyHub returns successResp for every leg EXCEPT failOnThingType,
// for which it returns errOnFail. Used by
// TestPost_AgentLegFailureDoesNotAbortComplianceProxy to exercise the
// partial-fan-out path without standing up a real Hub.
type flakeyHub struct {
	mu              sync.Mutex
	calls           []hub.ConfigChangeRequest
	failOnThingType string
	errOnFail       error
	successResp     *hub.ConfigChangeResponse
}

func (f *flakeyHub) NotifyConfigChange(_ context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if req.ThingType == f.failOnThingType {
		return nil, f.errOnFail
	}
	return f.successResp, nil
}

func (f *flakeyHub) NotifyCalls() []hub.ConfigChangeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]hub.ConfigChangeRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestPost_RejectsMissingEngaged locks the 400 behaviour for a body
// that omits the required engaged flag.
func TestPost_RejectsMissingEngaged(t *testing.T) {
	hub := &fakeHub{}
	aud := &auditSpy{}
	h := newHandler(t, nil, hub, aud)

	c, rec := newAdminContext(t, http.MethodPost, "/api/admin/compliance/killswitch", []byte(`{}`))

	_ = h.Post(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.Code != "VALIDATION_ERROR" || env.Error.Type != "validation_error" {
		t.Fatalf("error envelope = %+v, want code=VALIDATION_ERROR type=validation_error", env.Error)
	}
	if len(hub.NotifyCalls()) != 0 {
		t.Fatalf("hub should not be called on validation failure, got %d calls", len(hub.NotifyCalls()))
	}
	if aud.count() != 0 {
		t.Fatalf("admin audit should not enqueue on validation failure, got %d", aud.count())
	}
}

// TestPost_HubErrorReturns502 locks the mapping from an upstream Hub
// failure to a 502 so the UI can distinguish "CP problem" from "Hub
// problem".
func TestPost_HubErrorReturns502(t *testing.T) {
	hub := &fakeHub{err: errors.New("hub timeout")}
	aud := &auditSpy{}
	h := newHandler(t, nil, hub, aud)

	body, _ := json.Marshal(map[string]any{"engaged": true})
	c, rec := newAdminContext(t, http.MethodPost, "/api/admin/compliance/killswitch", body)

	_ = h.Post(c)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rec.Body.String())
	}
	// Primary-leg push failure now routes through the shared propagation
	// helper (F-0102), so the kill-switch toggle returns the same unified
	// propagation envelope as every other security-sensitive handler.
	if env.Error.Code != "HUB_PROPAGATION_FAILED" || env.Error.Type != "propagation_error" {
		t.Fatalf("error envelope = %+v, want code=HUB_PROPAGATION_FAILED type=propagation_error", env.Error)
	}
	if aud.count() != 0 {
		t.Fatalf("admin audit should not enqueue on hub error, got %d", aud.count())
	}
}

// TestPost_HubNilReturns503 locks the "Hub not configured" path —
// CP refuses to write when there's no Hub to propagate to.
func TestPost_HubNilReturns503(t *testing.T) {
	h := newHandler(t, nil, nil, nil)

	body, _ := json.Marshal(map[string]any{"engaged": true})
	c, rec := newAdminContext(t, http.MethodPost, "/api/admin/compliance/killswitch", body)

	_ = h.Post(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

// TestPost_BindFailureReturns400 locks the bind-error path.
func TestPost_BindFailureReturns400(t *testing.T) {
	hub := &fakeHub{}
	h := newHandler(t, nil, hub, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/compliance/killswitch", bytes.NewReader([]byte(`{not json`)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")

	_ = h.Post(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on bind error, got %d", rec.Code)
	}
}

// Helper-copy invariants

func TestErrJSON_EnvelopeShape(t *testing.T) {
	env := errJSON("msg", "type", "CODE")
	inner, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("envelope missing 'error' map: %+v", env)
	}
	if inner["message"] != "msg" || inner["type"] != "type" || inner["code"] != "CODE" {
		t.Errorf("envelope inner = %+v", inner)
	}
}

func TestActorFromContext_NoAuth(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	a := actorFromContext(c)
	if a.UserID != "" || a.Name != "" {
		t.Errorf("no-auth actor = %+v, want zero", a)
	}
}

func TestActorFromContext_WithAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "bob", "user-77")
	a := actorFromContext(c)
	if a.UserID != "user-77" || a.Name != "bob" {
		t.Errorf("auth actor = %+v, want {user-77, bob}", a)
	}
}

// TestRegisterRoutes_MountsCanonicalPost confirms the killswitch group
// wires the single canonical POST endpoint. Read-side data (current
// desired state per node, history events) lives on the generic
// config-sync surface so this handler is write-only.
func TestRegisterRoutes_MountsCanonicalPost(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	e := echo.New()
	g := e.Group("/api/admin")
	noop := func(string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterRoutes(g, noop)

	want := map[string]string{
		"POST /api/admin/compliance/killswitch": "",
	}
	for _, r := range e.Routes() {
		key := r.Method + " " + r.Path
		delete(want, key)
	}
	if len(want) > 0 {
		t.Fatalf("missing routes: %v", want)
	}
}
