package ws

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// fakeManager satisfies thingManager without touching Postgres. Captures
// every call so tests can assert observable side effects (registration
// occurred, liveness ticked, offline marked).
type fakeManager struct {
	mu            sync.Mutex
	registerCalls []manager.RegisterRequest
	registerResp  *manager.RegisterResponse
	registerErr   error
	shadowCalls   []manager.ShadowReportRequest
	shadowErr     error
	livenessCalls []string
	offlineCalls  []string
}

func (f *fakeManager) RegisterThing(_ context.Context, req manager.RegisterRequest) (*manager.RegisterResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerCalls = append(f.registerCalls, req)
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	if f.registerResp != nil {
		return f.registerResp, nil
	}
	return &manager.RegisterResponse{Desired: map[string]any{}, DesiredVer: 0}, nil
}

func (f *fakeManager) HandleShadowReport(_ context.Context, req manager.ShadowReportRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shadowCalls = append(f.shadowCalls, req)
	return f.shadowErr
}

func (f *fakeManager) TouchLiveness(_ context.Context, thingID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.livenessCalls = append(f.livenessCalls, thingID)
}

func (f *fakeManager) MarkOffline(_ context.Context, thingID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offlineCalls = append(f.offlineCalls, thingID)
}

func (f *fakeManager) registerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.registerCalls)
}

func (f *fakeManager) shadowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.shadowCalls)
}

func (f *fakeManager) offlineCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.offlineCalls)
}

// fakeValidator satisfies tokenValidator. Returns the configured thing
// when the hash matches the expected device-token hash; returns the
// configured err otherwise.
type fakeValidator struct {
	expectedHash string
	thing        *store.Thing
	err          error
	// thingStatuses drives GetThingStatus responses by thingID.
	// If the id is absent the default status is "online".
	thingStatuses map[string]string
	statusErr     error
}

func (f *fakeValidator) ValidateDeviceToken(_ context.Context, _, tokenHash string) (*store.Thing, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.expectedHash != "" && tokenHash != f.expectedHash {
		return nil, errors.New("hash mismatch")
	}
	return f.thing, nil
}

func (f *fakeValidator) GetThingStatus(_ context.Context, id string) (string, error) {
	if f.statusErr != nil {
		return "", f.statusErr
	}
	if st, ok := f.thingStatuses[id]; ok {
		return st, nil
	}
	return "online", nil
}

// fakeOps captures opsmetrics dispatch calls.
type fakeOps struct {
	mu          sync.Mutex
	sampleCalls int
	diagCalls   int
	staticCalls int
	sampleErr   error
	diagErr     error
	staticErr   error
}

func (f *fakeOps) HandleMetricsSample(_ context.Context, _, _ string, _ json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sampleCalls++
	return f.sampleErr
}

func (f *fakeOps) HandleDiagEvent(_ context.Context, _, _ string, _ json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diagCalls++
	return f.diagErr
}

func (f *fakeOps) HandleStaticInfo(_ context.Context, _, _ string, _ json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staticCalls++
	return f.staticErr
}

func (f *fakeOps) samples() int { f.mu.Lock(); defer f.mu.Unlock(); return f.sampleCalls }
func (f *fakeOps) diags() int   { f.mu.Lock(); defer f.mu.Unlock(); return f.diagCalls }
func (f *fakeOps) statics() int { f.mu.Lock(); defer f.mu.Unlock(); return f.staticCalls }

// nullLogger discards all log output so tests don't spam stderr.
func nullLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug + 10}))
}

func newFakeServer(t *testing.T, fm *fakeManager, fv *fakeValidator) (*Server, *Pool, *fakeOps) {
	t.Helper()
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	srv := newServerWithDeps(pool, fm, fv, "test-hub", testServiceToken, nil, true, nullLogger())
	ops := &fakeOps{}
	srv.SetOpsMetricsHandler(ops)
	return srv, pool, ops
}

func TestExtractBearerToken_AuthorizationHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer my-token")
	if got := extractBearerToken(req); got != "my-token" {
		t.Fatalf("Authorization header: got %q want %q", got, "my-token")
	}
}

func TestExtractBearerToken_AuthorizationHeaderWithWhitespace(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer   spaced-token   ")
	if got := extractBearerToken(req); got != "spaced-token" {
		t.Fatalf("got %q want %q", got, "spaced-token")
	}
}

func TestExtractBearerToken_AuthorizationHeaderWrongScheme(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic abc123")
	// Per code, any non-empty Authorization header that is not "Bearer "-prefixed
	// returns "" — Subprotocol fallback is NOT consulted in that case.
	if got := extractBearerToken(req); got != "" {
		t.Fatalf("non-bearer scheme should return empty, got %q", got)
	}
}

func TestExtractBearerToken_SubprotocolValid(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "nexus.bearer, my-subproto-token")
	if got := extractBearerToken(req); got != "my-subproto-token" {
		t.Fatalf("subprotocol: got %q want %q", got, "my-subproto-token")
	}
}

func TestExtractBearerToken_SubprotocolMalformedLengthOne(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "nexus.bearer")
	if got := extractBearerToken(req); got != "" {
		t.Fatalf("single-part subprotocol should return empty, got %q", got)
	}
}

func TestExtractBearerToken_SubprotocolMalformedThreeParts(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "nexus.bearer, tok, extra")
	if got := extractBearerToken(req); got != "" {
		t.Fatalf("three-part subprotocol should return empty, got %q", got)
	}
}

func TestExtractBearerToken_SubprotocolWrongName(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "other.proto, tok")
	if got := extractBearerToken(req); got != "" {
		t.Fatalf("wrong subprotocol name should return empty, got %q", got)
	}
}

func TestExtractBearerToken_NoneSet(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := extractBearerToken(req); got != "" {
		t.Fatalf("no token should return empty, got %q", got)
	}
}

func TestAuthenticate_EmptyToken(t *testing.T) {
	srv, _, _ := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	if _, _, err := srv.authenticate(req); !errors.Is(err, errUnauthorized) {
		t.Fatalf("got err=%v want errUnauthorized", err)
	}
}

func TestAuthenticate_ServiceToken_OK(t *testing.T) {
	srv, _, _ := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	req := httptest.NewRequest(http.MethodGet, "/ws?id=svc-1&type=ai-gateway", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	id, typ, err := srv.authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id != "svc-1" || typ != "ai-gateway" {
		t.Fatalf("got id=%q type=%q want svc-1/ai-gateway", id, typ)
	}
}

func TestAuthenticate_ServiceToken_MissingIDType(t *testing.T) {
	srv, _, _ := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	cases := []struct {
		name string
		url  string
	}{
		{"missing id", "/ws?type=ai-gateway"},
		{"missing type", "/ws?id=svc-1"},
		{"both empty", "/ws"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			req.Header.Set("Authorization", "Bearer "+testServiceToken)
			if _, _, err := srv.authenticate(req); !errors.Is(err, errUnauthorized) {
				t.Fatalf("got err=%v want errUnauthorized", err)
			}
		})
	}
}

func TestAuthenticate_DeviceToken_OK(t *testing.T) {
	plain := "abcd1234"
	hash, err := agentca.HashDeviceToken(plain)
	if err != nil {
		t.Fatalf("HashDeviceToken: %v", err)
	}
	fv := &fakeValidator{
		expectedHash: hash,
		thing:        &store.Thing{ID: "thing-1", Type: "agent"},
	}
	srv, _, _ := newFakeServer(t, &fakeManager{}, fv)

	req := httptest.NewRequest(http.MethodGet, "/ws?id=thing-1", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	id, typ, err := srv.authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id != "thing-1" || typ != "agent" {
		t.Fatalf("got id=%q type=%q", id, typ)
	}
}

func TestAuthenticate_DeviceToken_MissingID(t *testing.T) {
	srv, _, _ := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Authorization", "Bearer not-the-service-token")
	if _, _, err := srv.authenticate(req); !errors.Is(err, errUnauthorized) {
		t.Fatalf("got err=%v want errUnauthorized", err)
	}
}

func TestAuthenticate_DeviceToken_ValidatorRejects(t *testing.T) {
	fv := &fakeValidator{err: errors.New("not found")}
	srv, _, _ := newFakeServer(t, &fakeManager{}, fv)
	req := httptest.NewRequest(http.MethodGet, "/ws?id=thing-1", nil)
	// HashDeviceToken expects a hex-encoded token — supply one so we hit
	// the validator-rejection branch instead of the hash-error branch.
	req.Header.Set("Authorization", "Bearer abcdef0123456789")
	if _, _, err := srv.authenticate(req); !errors.Is(err, errUnauthorized) {
		t.Fatalf("got err=%v want errUnauthorized", err)
	}
}

// TestAuthenticate_DeviceToken_HashError covers the agentca.HashDeviceToken
// failure branch — a non-hex token wraps into an error other than
// errUnauthorized.
func TestAuthenticate_DeviceToken_HashError(t *testing.T) {
	srv, _, _ := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	req := httptest.NewRequest(http.MethodGet, "/ws?id=thing-1", nil)
	req.Header.Set("Authorization", "Bearer not-hex-data!")
	_, _, err := srv.authenticate(req)
	if err == nil {
		t.Fatal("expected hash error to be surfaced")
	}
	// Must NOT be errUnauthorized — should be the wrapped hash error.
	if errors.Is(err, errUnauthorized) {
		t.Fatalf("hash-error path should not return errUnauthorized: %v", err)
	}
	if !strings.Contains(err.Error(), "hash token") {
		t.Fatalf("expected wrapped 'hash token' err, got: %v", err)
	}
}

func TestHandleMessage_InvalidJSON(t *testing.T) {
	srv, _, ops := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	srv.handleMessage("thing-x", "agent", []byte("not-json"))
	if ops.samples()+ops.diags()+ops.statics() != 0 {
		t.Fatal("invalid json should not dispatch")
	}
}

func TestHandleMessage_ShadowReport_OK(t *testing.T) {
	fm := &fakeManager{}
	srv, _, _ := newFakeServer(t, fm, &fakeValidator{})
	msg := `{"type":"shadow_report","reported":{"k":"v"},"reportedVer":7}`
	srv.handleMessage("thing-x", "agent", []byte(msg))
	if fm.shadowCount() != 1 {
		t.Fatalf("expected 1 shadow call, got %d", fm.shadowCount())
	}
	fm.mu.Lock()
	call := fm.shadowCalls[0]
	fm.mu.Unlock()
	if call.ID != "thing-x" || call.ReportedVer != 7 {
		t.Fatalf("call mismatch: %+v", call)
	}
}

func TestHandleMessage_ShadowReport_InvalidPayload(t *testing.T) {
	fm := &fakeManager{}
	srv, _, _ := newFakeServer(t, fm, &fakeValidator{})
	// Type matches but payload reportedVer is a string — json.Unmarshal fails
	// at the struct decode step.
	msg := `{"type":"shadow_report","reportedVer":"not-a-number"}`
	srv.handleMessage("thing-x", "agent", []byte(msg))
	if fm.shadowCount() != 0 {
		t.Fatalf("expected 0 shadow calls on bad payload, got %d", fm.shadowCount())
	}
}

func TestHandleMessage_ShadowReport_ManagerError(t *testing.T) {
	fm := &fakeManager{shadowErr: errors.New("boom")}
	srv, _, _ := newFakeServer(t, fm, &fakeValidator{})
	msg := `{"type":"shadow_report","reported":{},"reportedVer":1}`
	srv.handleMessage("thing-x", "agent", []byte(msg))
	if fm.shadowCount() != 1 {
		t.Fatalf("expected handler to be invoked even on error, got %d", fm.shadowCount())
	}
}

// TestHandleMessage_BreakGlass_ReachesHandler drives the EXACT wire frame the
// production thingclient emits (thingclient.SendBreakGlassShadowReport, type
// "shadow_report_break_glass") through the WS dispatcher and asserts it reaches
// HandleShadowReport with the break_glass reconciliation sentinel stamped and
// the audit context (KeyVersions / ActorTokenID / SourceIP) carried. This is
// the regression guard for the break-glass frame — before the fix it fell to the
// dispatcher's default case and was silently dropped, so the emergency override
// never reached Hub adoption.
func TestHandleMessage_BreakGlass_ReachesHandler(t *testing.T) {
	fm := &fakeManager{}
	srv, _, _ := newFakeServer(t, fm, &fakeValidator{})
	// Byte-for-byte the frame thingclient sends: raw state (no {state,version}
	// wrapper), a human reason string, and the per-key version map.
	msg := `{"type":"shadow_report_break_glass","reported":{"killswitch":{"engaged":true}},"reportedVer":4,"reason":"incident-1234","sourceIp":"10.0.0.7","actorTokenId":"a1b2c3d4","keyVersions":{"killswitch":4}}`
	srv.handleMessage("proxy-1", "compliance-proxy", []byte(msg))

	if fm.shadowCount() != 1 {
		t.Fatalf("break-glass frame did not reach HandleShadowReport (got %d calls) — wire is dead", fm.shadowCount())
	}
	fm.mu.Lock()
	call := fm.shadowCalls[0]
	fm.mu.Unlock()
	if call.ID != "proxy-1" {
		t.Errorf("ID = %q, want proxy-1 (WS-authenticated identity)", call.ID)
	}
	// The message TYPE is the signal — Hub stamps the sentinel, not the wire's
	// human reason field.
	if call.Reason != "break_glass" {
		t.Errorf("Reason = %q, want break_glass (sentinel stamped by dispatcher)", call.Reason)
	}
	if call.ReportedVer != 4 {
		t.Errorf("ReportedVer = %d, want 4", call.ReportedVer)
	}
	if call.ActorTokenID != "a1b2c3d4" {
		t.Errorf("ActorTokenID = %q, want a1b2c3d4", call.ActorTokenID)
	}
	if call.SourceIP != "10.0.0.7" {
		t.Errorf("SourceIP = %q, want 10.0.0.7", call.SourceIP)
	}
	if call.KeyVersions["killswitch"] != 4 {
		t.Errorf("KeyVersions[killswitch] = %d, want 4", call.KeyVersions["killswitch"])
	}
	ks, ok := call.Reported["killswitch"].(map[string]any)
	if !ok || ks["engaged"] != true {
		t.Errorf("Reported[killswitch] = %v, want {engaged:true}", call.Reported["killswitch"])
	}
}

// TestHandleMessage_BreakGlass_InvalidPayload covers the malformed-frame branch:
// a break-glass type with a non-numeric reportedVer fails the struct decode and
// must NOT reach the handler.
func TestHandleMessage_BreakGlass_InvalidPayload(t *testing.T) {
	fm := &fakeManager{}
	srv, _, _ := newFakeServer(t, fm, &fakeValidator{})
	msg := `{"type":"shadow_report_break_glass","reportedVer":"nope"}`
	srv.handleMessage("proxy-1", "compliance-proxy", []byte(msg))
	if fm.shadowCount() != 0 {
		t.Fatalf("expected 0 calls on bad break-glass payload, got %d", fm.shadowCount())
	}
}

// TestHandleMessage_BreakGlass_ManagerError asserts the dispatcher invokes the
// handler even when reconciliation errors (the error is logged, not retried).
func TestHandleMessage_BreakGlass_ManagerError(t *testing.T) {
	fm := &fakeManager{shadowErr: errors.New("boom")}
	srv, _, _ := newFakeServer(t, fm, &fakeValidator{})
	msg := `{"type":"shadow_report_break_glass","reported":{"killswitch":{"engaged":true}},"reportedVer":4,"actorTokenId":"a1b2c3d4","keyVersions":{"killswitch":4}}`
	srv.handleMessage("proxy-1", "compliance-proxy", []byte(msg))
	if fm.shadowCount() != 1 {
		t.Fatalf("expected handler invoked even on error, got %d", fm.shadowCount())
	}
}

func TestHandleMessage_MetricsSample_DispatchedWhenOpsConfigured(t *testing.T) {
	srv, _, ops := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	srv.handleMessage("thing-x", "agent", []byte(`{"type":"metrics_sample","samples":[]}`))
	if ops.samples() != 1 {
		t.Fatalf("expected 1 sample dispatch, got %d", ops.samples())
	}
}

func TestHandleMessage_MetricsSample_DroppedWhenOpsNil(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	srv := newServerWithDeps(pool, &fakeManager{}, &fakeValidator{}, "test-hub", testServiceToken, nil, true, nullLogger())
	// ops intentionally not set — must be silently dropped.
	srv.handleMessage("thing-x", "agent", []byte(`{"type":"metrics_sample"}`))
	// no panic, no dispatch — success criterion is "no panic".
}

func TestHandleMessage_MetricsSample_DispatchErrorLogged(t *testing.T) {
	ops := &fakeOps{sampleErr: errors.New("dispatch boom")}
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	srv := newServerWithDeps(pool, &fakeManager{}, &fakeValidator{}, "test-hub", testServiceToken, nil, true, nullLogger())
	srv.SetOpsMetricsHandler(ops)
	srv.handleMessage("thing-x", "agent", []byte(`{"type":"metrics_sample"}`))
	if ops.samples() != 1 {
		t.Fatalf("dispatch should have been attempted, got %d", ops.samples())
	}
}

func TestHandleMessage_DiagEvent_Dispatched(t *testing.T) {
	srv, _, ops := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	srv.handleMessage("thing-x", "agent", []byte(`{"type":"diag_event","events":[]}`))
	if ops.diags() != 1 {
		t.Fatalf("expected 1 diag dispatch, got %d", ops.diags())
	}
}

func TestHandleMessage_DiagEvent_DroppedWhenOpsNil(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	srv := newServerWithDeps(pool, &fakeManager{}, &fakeValidator{}, "test-hub", testServiceToken, nil, true, nullLogger())
	srv.handleMessage("thing-x", "agent", []byte(`{"type":"diag_event"}`))
}

func TestHandleMessage_DiagEvent_DispatchErrorLogged(t *testing.T) {
	ops := &fakeOps{diagErr: errors.New("diag boom")}
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	srv := newServerWithDeps(pool, &fakeManager{}, &fakeValidator{}, "test-hub", testServiceToken, nil, true, nullLogger())
	srv.SetOpsMetricsHandler(ops)
	srv.handleMessage("thing-x", "agent", []byte(`{"type":"diag_event"}`))
	if ops.diags() != 1 {
		t.Fatalf("expected 1 diag dispatch attempt, got %d", ops.diags())
	}
}

func TestHandleMessage_StaticInfo_Dispatched(t *testing.T) {
	srv, _, ops := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	srv.handleMessage("thing-x", "agent", []byte(`{"type":"static_info","os":"darwin"}`))
	if ops.statics() != 1 {
		t.Fatalf("expected 1 static dispatch, got %d", ops.statics())
	}
}

func TestHandleMessage_StaticInfo_DroppedWhenOpsNil(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	srv := newServerWithDeps(pool, &fakeManager{}, &fakeValidator{}, "test-hub", testServiceToken, nil, true, nullLogger())
	srv.handleMessage("thing-x", "agent", []byte(`{"type":"static_info"}`))
}

func TestHandleMessage_StaticInfo_DispatchErrorLogged(t *testing.T) {
	ops := &fakeOps{staticErr: errors.New("static boom")}
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	srv := newServerWithDeps(pool, &fakeManager{}, &fakeValidator{}, "test-hub", testServiceToken, nil, true, nullLogger())
	srv.SetOpsMetricsHandler(ops)
	srv.handleMessage("thing-x", "agent", []byte(`{"type":"static_info"}`))
	if ops.statics() != 1 {
		t.Fatalf("expected 1 static dispatch attempt, got %d", ops.statics())
	}
}

func TestHandleMessage_UnknownType(t *testing.T) {
	srv, _, ops := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	srv.handleMessage("thing-x", "agent", []byte(`{"type":"unknown_message"}`))
	if ops.samples()+ops.diags()+ops.statics() != 0 {
		t.Fatal("unknown type should not dispatch")
	}
}

func TestParseIncoming_OK(t *testing.T) {
	data := []byte(`{"type":"shadow_report","reported":{"a":1}}`)
	msg, err := ParseIncoming(data)
	if err != nil {
		t.Fatalf("ParseIncoming: %v", err)
	}
	if msg.Type != "shadow_report" {
		t.Fatalf("type: got %q", msg.Type)
	}
	if string(msg.Raw) != string(data) {
		t.Fatalf("Raw should preserve original payload")
	}
}

func TestParseIncoming_BadJSON(t *testing.T) {
	if _, err := ParseIncoming([]byte("not-json")); err == nil {
		t.Fatal("expected error on bad JSON")
	}
}

func TestServer_PoolAndClose(t *testing.T) {
	srv, pool, _ := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	if srv.Pool() != pool {
		t.Fatalf("Pool() should return the underlying pool")
	}
	// Close on empty pool must not panic.
	srv.Close()
}

func TestConn_WriteAndAccessors(t *testing.T) {
	// We don't need a real WS connection for Write to enqueue onto outCh,
	// but newConn dereferences ws.SetReadLimit inside Run only — Write
	// itself only touches outCh and thingID, so a stub Conn is fine.
	c := &Conn{
		thingID:   "agent-1",
		thingType: "agent",
		outCh:     make(chan []byte, 2),
		done:      make(chan struct{}),
		logger:    nullLogger(),
	}
	if c.ThingID() != "agent-1" || c.ThingType() != "agent" {
		t.Fatalf("accessors mismatch")
	}
	if err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("Write into empty buffer: %v", err)
	}
	if err := c.Write([]byte("world")); err != nil {
		t.Fatalf("Write second: %v", err)
	}
	// Third Write hits the full-buffer path.
	err := c.Write([]byte("overflow"))
	if err == nil {
		t.Fatal("expected error when buffer is full")
	}
	if !strings.Contains(err.Error(), "write buffer full for agent-1") {
		t.Fatalf("unexpected err: %v", err)
	}
}

func newStubConn(id, typ string) *Conn {
	return &Conn{
		thingID:   id,
		thingType: typ,
		outCh:     make(chan []byte, 4),
		done:      make(chan struct{}),
		logger:    nullLogger(),
	}
}

// stubConnWithFullBuffer returns a Conn whose outCh is at capacity so Write
// drops with an error (used to assert Broadcast and Send failure branches). It
// is backed by a real (discard) websocket because the buffer-full drop path now
// closes the connection, which dereferences c.ws.
func stubConnWithFullBuffer(t *testing.T, id, typ string) *Conn {
	t.Helper()
	c := &Conn{
		ws:        newDiscardWS(t),
		thingID:   id,
		thingType: typ,
		outCh:     make(chan []byte, 1),
		done:      make(chan struct{}),
		logger:    nullLogger(),
	}
	c.outCh <- []byte("pre-existing")
	return c
}

func TestPool_AddRemoveCountConnected(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())

	c1 := newStubConn("agent-1", "agent")
	pool.Add(c1)
	if pool.Count() != 1 {
		t.Fatalf("Count after Add: got %d want 1", pool.Count())
	}
	if !pool.IsConnected("agent-1") {
		t.Fatal("IsConnected should be true after Add")
	}

	// Send to the connected thing should succeed.
	if !pool.Send("agent-1", []byte("ping")) {
		t.Fatal("Send to connected should return true")
	}

	pool.Remove(c1)
	if pool.Count() != 0 {
		t.Fatalf("Count after Remove: got %d", pool.Count())
	}
	if pool.IsConnected("agent-1") {
		t.Fatal("IsConnected should be false after Remove")
	}
}

func TestPool_Add_ReplacesExisting(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())

	first := newStubConn("agent-1", "agent")
	first.ws = newDiscardWS(t) // Conn.Close calls ws.Close — provide a real conn.
	pool.Add(first)

	second := newStubConn("agent-1", "agent")
	second.ws = newDiscardWS(t)
	pool.Add(second)
	// Count must still be 1 (the second replaces the first).
	if pool.Count() != 1 {
		t.Fatalf("Count after replace: got %d", pool.Count())
	}
	// The first connection should be closed by the replace path.
	select {
	case <-first.done:
		// closed as expected
	default:
		t.Fatal("first connection should be closed when replaced")
	}
}

func TestPool_Remove_MissingIsNoop(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	// Remove a connection that was never added — must not panic.
	pool.Remove(newStubConn("ghost", "agent"))
	if pool.Count() != 0 {
		t.Fatalf("Count after no-op Remove: %d", pool.Count())
	}
}

func TestPool_Broadcast_FanOut(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	a := newStubConn("agent-1", "agent")
	b := newStubConn("agent-2", "agent")
	c := newStubConn("cp-1", "control-plane")
	pool.Add(a)
	pool.Add(b)
	pool.Add(c)

	sent := pool.Broadcast("agent", []byte("hi"))
	if sent != 2 {
		t.Fatalf("Broadcast agent: got %d want 2", sent)
	}
	// Drain to assert delivery.
	for _, conn := range []*Conn{a, b} {
		select {
		case <-conn.outCh:
		case <-time.After(50 * time.Millisecond):
			t.Fatalf("conn %s did not receive broadcast", conn.thingID)
		}
	}
	// The cp-1 conn should not have received the broadcast.
	select {
	case <-c.outCh:
		t.Fatal("cp-1 should not have received agent broadcast")
	default:
	}
}

func TestPool_Broadcast_PartialFailureBumpsOnlyDelivered(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	good := newStubConn("agent-good", "agent")
	bad := stubConnWithFullBuffer(t, "agent-bad", "agent")
	pool.Add(good)
	pool.Add(bad)
	sent := pool.Broadcast("agent", []byte("hi"))
	if sent != 1 {
		t.Fatalf("expected sent=1 (only good), got %d", sent)
	}
}

func TestPool_Send_BufferFullReturnsFalse(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	bad := stubConnWithFullBuffer(t, "agent-bad", "agent")
	pool.Add(bad)
	if pool.Send("agent-bad", []byte("won't fit")) {
		t.Fatal("Send into full buffer should return false")
	}
}

func TestPool_CloseAll_ClosesEveryConn(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	conns := make([]*Conn, 0, 3)
	for i := range 3 {
		c := newStubConn(fmt.Sprintf("agent-%d", i), "agent")
		// CloseAll calls Conn.Close which closes ws — so we must give each
		// stub a real (but unused) websocket.Conn-equivalent. The stub uses
		// closeOnce + done to short-circuit, but Close also calls ws.Close()
		// which is nil for stubs. Provide a stub server to avoid nil deref.
		c.ws = newDiscardWS(t)
		conns = append(conns, c)
		pool.Add(c)
	}
	pool.CloseAll()
	// All done channels closed.
	for _, c := range conns {
		select {
		case <-c.done:
		default:
			t.Fatalf("conn %s not closed by CloseAll", c.thingID)
		}
	}
}

func TestPool_NilRegistry(t *testing.T) {
	// NewPool should work when reg=nil — production must not panic if
	// opsmetrics is disabled by config.
	pool := NewPool(nil, nullLogger())
	c := newStubConn("agent-1", "agent")
	c.ws = newDiscardWS(t)
	pool.Add(c)
	if pool.Count() != 1 {
		t.Fatalf("count: %d", pool.Count())
	}
	pool.Remove(c)
	if pool.Count() != 0 {
		t.Fatalf("count after remove: %d", pool.Count())
	}
}

// newDiscardWS spins up a tiny httptest server that accepts a single WS
// upgrade and discards everything. It returns the server-side websocket.Conn
// so tests that need a live ws.Close() can use it without breaking.
//
// Note: the returned *websocket.Conn is the CLIENT side; we use it as a
// stand-in because we only need ws.Close() to be non-nil. The server-side
// connection is also closed when the test exits via t.Cleanup.
func newDiscardWS(t *testing.T) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		// Drain until the peer closes.
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c
}

func TestHandleUpgrade_HappyPath_ServiceToken(t *testing.T) {
	fm := &fakeManager{registerResp: &manager.RegisterResponse{
		Desired:    map[string]any{"k": "v"},
		DesiredVer: 42,
	}}
	srv, _, _ := newFakeServer(t, fm, &fakeValidator{})

	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") +
		"/?id=svc-1&type=ai-gateway&name=svc&version=1.0&address=addr&metricsUrl=u&managementUrl=u&role=r&runtimeApiUrl=r&physicalId=p"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + testServiceToken}},
	})
	if err != nil {
		t.Fatalf("Dial: %v (resp=%v)", err, resp)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer client.Close(websocket.StatusNormalClosure, "")

	// Read the initial "connected" message — proves the handshake reply
	// path was hit.
	readCtx, cancelRead := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancelRead()
	_, data, err := client.Read(readCtx)
	if err != nil {
		t.Fatalf("client.Read: %v", err)
	}
	var conn ConnectedMessage
	if err := json.Unmarshal(data, &conn); err != nil {
		t.Fatalf("unmarshal connected: %v", err)
	}
	if conn.Type != "connected" || conn.HubID != "test-hub" || conn.DesiredVer != 42 {
		t.Fatalf("connected payload mismatch: %+v", conn)
	}

	// Disconnect; cleanup branch should fire.
	client.Close(websocket.StatusNormalClosure, "bye")

	// Wait until MarkOffline fires (HandleUpgrade is running in a server goroutine).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fm.offlineCount() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fm.registerCount() != 1 {
		t.Fatalf("RegisterThing should have been called once, got %d", fm.registerCount())
	}
	if fm.offlineCount() != 1 {
		t.Fatalf("MarkOffline should have been called once, got %d", fm.offlineCount())
	}
	// Confirm the query-string fields were threaded into the register call.
	fm.mu.Lock()
	req := fm.registerCalls[0]
	fm.mu.Unlock()
	if req.ID != "svc-1" || req.Type != "ai-gateway" || req.Version != "1.0" || req.Address != "addr" || req.PhysicalID != "p" {
		t.Fatalf("register request fields not threaded: %+v", req)
	}
}

// TestHandleUpgrade_TouchLivenessFires verifies the inline onLiveness
// closure in HandleUpgrade (server.go:149-151) calls Manager.TouchLiveness.
// Shortens pingInterval so the client's ping handshake completes inside
// the test deadline.
func TestHandleUpgrade_TouchLivenessFires(t *testing.T) {
	orig := pingInterval
	pingInterval = 20 * time.Millisecond
	t.Cleanup(func() { pingInterval = orig })

	fm := &fakeManager{registerResp: &manager.RegisterResponse{Desired: map[string]any{}, DesiredVer: 0}}
	srv, _, _ := newFakeServer(t, fm, &fakeValidator{})

	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/?id=svc-1&type=ai-gateway"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + testServiceToken}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")

	// Read continuously so server-side pings get pong responses.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := client.Read(ctx); err != nil {
				return
			}
		}
	}()

	// Wait for at least one TouchLiveness call.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fm.mu.Lock()
		n := len(fm.livenessCalls)
		fm.mu.Unlock()
		if n >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("TouchLiveness was never invoked")
}

// TestHandleUpgrade_HappyPath_ClientSendsShadowReport extends the happy
// path with a client-initiated shadow_report message — exercises the
// readPump's onMsg-dispatch branch (conn.go:83-85) and the Server's full
// handleMessage path including HandleShadowReport on the fake manager.
func TestHandleUpgrade_HappyPath_ClientSendsShadowReport(t *testing.T) {
	fm := &fakeManager{registerResp: &manager.RegisterResponse{
		Desired:    map[string]any{},
		DesiredVer: 1,
	}}
	srv, _, _ := newFakeServer(t, fm, &fakeValidator{})

	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/?id=svc-1&type=ai-gateway"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + testServiceToken}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")

	// Read the connected handshake first.
	readCtx, readCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer readCancel()
	if _, _, err := client.Read(readCtx); err != nil {
		t.Fatalf("client.Read handshake: %v", err)
	}

	// Send a shadow_report — this exercises the readPump onMsg branch.
	payload := []byte(`{"type":"shadow_report","reported":{"v":1},"reportedVer":42}`)
	if err := client.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("client.Write shadow_report: %v", err)
	}

	// Wait for the fake manager to observe the shadow report.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fm.shadowCount() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fm.shadowCount() != 1 {
		t.Fatalf("expected 1 shadow report dispatched, got %d", fm.shadowCount())
	}
}

func TestHandleUpgrade_AuthFailReturns401(t *testing.T) {
	srv, _, _ := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	t.Cleanup(httpSrv.Close)
	resp, err := http.Get(httpSrv.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", resp.StatusCode)
	}
}

func TestHandleUpgrade_RegistrationFailureClosesConn(t *testing.T) {
	fm := &fakeManager{registerErr: errors.New("register failed")}
	srv, _, _ := newFakeServer(t, fm, &fakeValidator{})

	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/?id=svc-1&type=ai-gateway"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + testServiceToken}},
	})
	if err != nil {
		// Upgrade was completed before the registration error, so Dial
		// itself usually succeeds; the next Read returns a close frame.
		t.Logf("Dial returned err (acceptable): %v", err)
		return
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer client.Close(websocket.StatusInternalError, "")

	// First Read should see the server-initiated close after register failure.
	readCtx, cancelRead := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancelRead()
	_, _, readErr := client.Read(readCtx)
	if readErr == nil {
		t.Fatal("expected close on registration failure")
	}
}

func TestHandleUpgrade_AcceptFailsOnDisallowedOrigin(t *testing.T) {
	srv, _, _ := newFakeServer(t, &fakeManager{}, &fakeValidator{})
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	t.Cleanup(httpSrv.Close)

	// Forge a Sec-WebSocket-Key + Sec-WebSocket-Version + Upgrade headers
	// but set an Origin that is not in the allowlist. coder/websocket's
	// Accept returns 403.
	req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/?id=svc-1&type=ai-gateway", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Origin", "http://evil.example.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http.Do: %v", err)
	}
	defer resp.Body.Close()
	// Accept fails after authenticate succeeds — the disallowed-origin
	// branch returns without writing 200; coder/websocket writes a 403.
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("expected accept-fail status (not 401), got %d", resp.StatusCode)
	}
}

// TestHandleUpgrade_ProdRejectsLocalhostOrigin covers the case where, with devMode
// false (production), a localhost Origin must NOT be auto-allowed — the upgrade
// must fail the origin check (coder/websocket returns 403, not 101). This is
// the regression that would otherwise let a page served from the operator's own
// machine, plus a leaked bearer token, open a Hub WS.
func TestHandleUpgrade_ProdRejectsLocalhostOrigin(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	// devMode=false, no allowlist → no origin is permitted.
	srv := newServerWithDeps(pool, &fakeManager{}, &fakeValidator{}, "test-hub", testServiceToken, nil, false, nullLogger())
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	t.Cleanup(httpSrv.Close)

	req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/?id=svc-1&type=ai-gateway", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Origin", "http://localhost:3000")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("prod (devMode=false) must reject a localhost Origin, but upgrade succeeded (F-0256)")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("auth should have passed; rejection must be the origin check, got 401")
	}
}

// TestHandleUpgrade_DevAllowsLocalhostOrigin is the paired positive case: with
// devMode true, the same localhost Origin upgrade succeeds. This confirms the
// dev gate flips the behavior rather than removing localhost support entirely.
func TestHandleUpgrade_DevAllowsLocalhostOrigin(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	srv := newServerWithDeps(pool, &fakeManager{}, &fakeValidator{}, "test-hub", testServiceToken, nil, true, nullLogger())
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/?id=svc-1&type=ai-gateway"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + testServiceToken}, "Origin": {"http://localhost:3000"}},
		Subprotocols: []string{"nexus.bearer"},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dev mode should accept localhost Origin, dial failed: %v", err)
	}
	_ = client.Close(websocket.StatusNormalClosure, "done")
}

// TestNewServer_PassesThroughDeps verifies the production constructor wires
// the Manager + Store dependency through to the seam without a live DB.
// manager.New tolerates a nil *store.Store at construction; the resulting
// Server's validator is nil, which is fine as long as no caller exercises
// the device-token branch through this Server instance.
func TestNewServer_PassesThroughDeps(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	// Construct a real *manager.Manager with nil store/redis/mq — only
	// the constructor path itself is exercised; we don't dispatch any RPCs.
	mgr := manager.New(nil, nil, nil, pool, "prod-hub", nullLogger())
	srv := NewServer(pool, mgr, "prod-hub", "tok", []string{"prod.example.com"}, false, nullLogger())
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	if srv.Pool() != pool {
		t.Fatal("Pool() should be wired through")
	}
}

// TestConn_ReadPump_AbnormalCloseLogged drives the "ws read error" branch
// (conn.go:83). We close the server-side ws via CloseNow which severs the
// underlying TCP without sending a close frame — the blocked Read in
// readPump returns net.ErrClosed (not a normal close), and the else arm
// fires.
func TestConn_ReadPump_AbnormalCloseLogged(t *testing.T) {
	upgraded := make(chan *websocket.Conn, 1)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		upgraded <- ws
		<-r.Context().Done()
	}))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	client, resp, err := websocket.Dial(dialCtx, wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close(websocket.StatusInternalError, "")
	server := <-upgraded

	c := newConn(server, "agent-abort", "agent", nil, nil, nullLogger())

	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(done)
	}()

	// Give readPump time to enter Read, then CloseNow the server-side.
	// CloseNow on the server-side socket makes the in-flight server Read
	// return without a clean close frame — coder/websocket surfaces this
	// as use-of-closed-conn, which has CloseStatus == -1, hitting the
	// else arm.
	time.Sleep(20 * time.Millisecond)
	_ = server.CloseNow()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after CloseNow")
	}
}

// TestConn_WritePump_WriteErrorClosesConn drives conn.go:103-107 by pre-
// closing the underlying coder/websocket connection from outside the pumps,
// then enqueuing a write. writePump's outCh case fires, c.ws.Write returns
// "connection closed", and the close branch is taken.
func TestConn_WritePump_WriteErrorClosesConn(t *testing.T) {
	upgraded := make(chan *websocket.Conn, 1)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		upgraded <- ws
		<-r.Context().Done()
	}))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, resp, err := websocket.Dial(ctx, wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close(websocket.StatusInternalError, "")
	server := <-upgraded

	c := newConn(server, "agent-x", "agent", nil, nil, nullLogger())
	// Pre-close the underlying WS via CloseNow — subsequent server-side
	// Writes will fail immediately.
	_ = server.CloseNow()

	// Enqueue AFTER close so the writePump's first Write attempt fails.
	if err := c.Write([]byte("payload")); err != nil {
		t.Fatalf("Write enqueue: %v", err)
	}

	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after pre-closed server-side")
	}
}

func TestConn_WritePump_PingFailureClosesConn(t *testing.T) {
	// Note: this test does NOT short-circuit the global pingInterval. The
	// server-side ws is pre-closed before Run starts, so writePump's
	// select almost always picks <-c.done (closed by readPump's defer
	// Close) before the ping ticker would ever fire. The intent here is
	// to exercise the symmetric pre-closed teardown path; the ping-error
	// branch itself (conn.go:112-116) is structurally hard to hit
	// deterministically and is intentionally left uncovered.

	upgraded := make(chan *websocket.Conn, 1)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		upgraded <- ws
		<-r.Context().Done()
	}))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, resp, err := websocket.Dial(ctx, wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close(websocket.StatusInternalError, "")
	server := <-upgraded
	// Pre-close the server side so the next Ping in writePump fails.
	_ = server.CloseNow()

	c := newConn(server, "agent-pingfail", "agent", nil, func(string) {
		t.Fatal("onLiveness must NOT fire when ping fails")
	}, nullLogger())

	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ping failure")
	}
}

func TestConn_WritePump_CtxDoneReturns(t *testing.T) {
	upgraded := make(chan *websocket.Conn, 1)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		upgraded <- ws
		<-r.Context().Done()
	}))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	client, resp, err := websocket.Dial(dialCtx, wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	server := <-upgraded

	c := newConn(server, "agent-ctx", "agent", nil, nil, nullLogger())

	runCtx, runCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(runCtx)
		close(done)
	}()

	// Cancel ctx — both pumps should exit promptly.
	time.Sleep(20 * time.Millisecond)
	runCancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// dummy compile-time reference to keep atomic in the import set without
// triggering unused-import; we use sync/atomic in the liveness test pieces
// below.
var _ = atomic.LoadInt64

// TestPool_Remove_IdentityGuard_StaleConnDoesNotEvictLive is the pool-level
// reconnect-race regression. After a reconnect race installs a new connection for a
// Thing ID, the stale old connection's late Remove must be a no-op: it must NOT
// evict the live new connection, must report it did not remove (false), and
// pool.Send must still reach the live conn. A genuine Remove of the current
// connection still evicts and reports true.
func TestPool_Remove_IdentityGuard_StaleConnDoesNotEvictLive(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())

	old := newStubConn("agent-1", "agent")
	old.ws = newDiscardWS(t) // Add's replace path calls old.Close → needs a real ws
	pool.Add(old)

	live := newStubConn("agent-1", "agent")
	live.ws = newDiscardWS(t)
	pool.Add(live) // replaces + closes old, installs live

	// The stale old conn's defer eventually calls Remove(old). With the
	// identity guard this is a no-op because the registered conn is `live`.
	if pool.Remove(old) {
		t.Fatal("Remove(stale old) returned true — it would evict the live new conn (F-0199)")
	}
	if !pool.IsConnected("agent-1") {
		t.Fatal("live conn was evicted by the stale old conn's Remove (F-0199)")
	}
	if pool.Count() != 1 {
		t.Fatalf("Count = %d after stale Remove, want 1", pool.Count())
	}

	// Pushes must still reach the live conn — the whole point of the fix is that
	// config pushes are not black-holed after a reconnect race.
	if !pool.Send("agent-1", []byte("cfg")) {
		t.Fatal("Send did not reach the live conn after a stale Remove")
	}
	select {
	case <-live.outCh:
	default:
		t.Fatal("live conn did not receive the queued push")
	}

	// A genuine Remove of the current conn DOES evict and reports true.
	if !pool.Remove(live) {
		t.Fatal("Remove(current conn) returned false; expected eviction")
	}
	if pool.IsConnected("agent-1") {
		t.Fatal("current conn should be gone after its own Remove")
	}
}

// TestPool_Remove_NeverAddedReportsFalse pins the "not present" branch of the
// identity guard: removing a conn that was never added returns false.
func TestPool_Remove_NeverAddedReportsFalse(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	pool := NewPool(reg, nullLogger())
	if pool.Remove(newStubConn("ghost", "agent")) {
		t.Fatal("Remove(never-added) returned true; want false")
	}
}

// TestHandleUpgrade_ReconnectRace_StaleDisconnectDoesNotMarkOffline is the
// server-level reconnect-race regression. Two WebSocket connections arrive for the same
// Thing ID; the second replaces the first in the pool. When the stale first
// connection's read loop unblocks and runs its disconnect cleanup, pool.Remove
// returns false (the new conn is current) so MarkOffline MUST be suppressed —
// otherwise a healthy node is marked offline and every config push to it is
// dropped. Closing the current connection later fires MarkOffline exactly once.
func TestHandleUpgrade_ReconnectRace_StaleDisconnectDoesNotMarkOffline(t *testing.T) {
	fm := &fakeManager{registerResp: &manager.RegisterResponse{Desired: map[string]any{}, DesiredVer: 1}}
	srv, pool, _ := newFakeServer(t, fm, &fakeValidator{})

	httpSrv := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	t.Cleanup(httpSrv.Close)

	dial := func() *websocket.Conn {
		wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/?id=svc-1&type=ai-gateway"
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + testServiceToken}},
		})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		// Drain the "connected" handshake so the upgrade is fully established.
		rctx, rcancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer rcancel()
		if _, _, err := c.Read(rctx); err != nil {
			t.Fatalf("read handshake: %v", err)
		}
		return c
	}

	oldClient := dial()
	defer oldClient.Close(websocket.StatusInternalError, "")

	// Second connection for the SAME id — the server's pool.Add replaces and
	// closes the old conn, which makes the old conn's server-side read loop
	// unblock and run its disconnect cleanup.
	newClient := dial()
	defer newClient.Close(websocket.StatusInternalError, "")

	// Wait until exactly the new conn is registered.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pool.IsConnected("svc-1") && pool.Count() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give the stale conn's cleanup goroutine room to (wrongly) mark offline.
	time.Sleep(200 * time.Millisecond)

	if got := fm.offlineCount(); got != 0 {
		t.Fatalf("MarkOffline fired %d times on a reconnect race; want 0 (live node would be black-holed)", got)
	}
	if !pool.IsConnected("svc-1") {
		t.Fatal("new conn not registered after reconnect race")
	}
	// A push to the live node must still be deliverable.
	if !pool.Send("svc-1", []byte(`{"type":"config_changed"}`)) {
		t.Fatal("pool.Send did not reach the live new conn (drift repair would be defeated)")
	}

	// Cleanly close the current conn — its cleanup Remove returns true, so
	// MarkOffline fires exactly once now.
	newClient.Close(websocket.StatusNormalClosure, "bye")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fm.offlineCount() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := fm.offlineCount(); got != 1 {
		t.Fatalf("MarkOffline = %d after current conn closed; want exactly 1", got)
	}
}

// TestAuthenticate_ServiceToken_RevokedThingIsRejected verifies that a
// service-token WS upgrade for a Thing whose status is "revoked" is rejected
// with 401, preventing a revoked service Thing from re-promoting itself to
// online.
func TestAuthenticate_ServiceToken_RevokedThingIsRejected(t *testing.T) {
	pool := NewPool(nil, nullLogger())
	fv := &fakeValidator{
		thingStatuses: map[string]string{"svc-revoked": "revoked"},
	}
	srv := newServerWithDeps(pool, &fakeManager{}, fv, "hub-1", testServiceToken, nil, false, nullLogger())

	ts := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws?id=svc-revoked&type=ai-gateway"
	_, resp, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + testServiceToken}},
		Subprotocols: []string{"nexus.bearer"},
	})
	if err == nil {
		t.Fatal("expected dial to fail for revoked service Thing; got nil error")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		var code int
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("expected 401 for revoked service Thing; got %d", code)
	}
}

// TestAuthenticate_ServiceToken_UnknownThingIsRejected verifies that a
// service-token upgrade for an unknown thingID is rejected (GetThingStatus
// returns ErrNotFound → errUnauthorized).
func TestAuthenticate_ServiceToken_UnknownThingIsRejected(t *testing.T) {
	pool := NewPool(nil, nullLogger())
	fv := &fakeValidator{statusErr: errors.New("not found")}
	srv := newServerWithDeps(pool, &fakeManager{}, fv, "hub-1", testServiceToken, nil, false, nullLogger())

	ts := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws?id=ghost-svc&type=ai-gateway"
	_, resp, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + testServiceToken}},
		Subprotocols: []string{"nexus.bearer"},
	})
	if err == nil {
		t.Fatal("expected dial to fail for unknown service Thing")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		var code int
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("expected 401 for unknown service Thing; got %d", code)
	}
}
