package wiring

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	configcache "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/issuer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// sharedTestThingClient is created once per test binary because
// thingclient.New registers Prometheus metrics on prometheus.DefaultRegisterer
// and panics on duplicate registration.
var sharedTestThingClient = func() *thingclient.Client {
	tc, err := thingclient.New(thingclient.Config{
		HubURL:     "ws://127.0.0.1:1/ws",
		HubHTTPURL: "http://127.0.0.1:1",
		ThingType:  "compliance-proxy",
		ThingID:    "test-proxy-health",
		Token:      "test-token",
		Logger:     testLogger(),
	})
	if err != nil {
		panic("sharedTestThingClient: " + err.Error())
	}
	return tc
}()

// newTestThingClient returns the shared test client. Start() is never called,
// so getter methods work but Close() would block.
func newTestThingClient(_ *testing.T) *thingclient.Client {
	return sharedTestThingClient
}

func buildHealthDeps(t *testing.T) HealthDeps {
	t.Helper()

	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := issuer.NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	cacheManager := configcache.NewManager(5*time.Minute, testLogger())
	domainEngine := domain.NewEngine()
	ks := killswitch.NewKillSwitch(testLogger())
	exStore := exemption.NewStore(testLogger())
	captureStore := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	connMgr := conn.NewManager(0)
	keyRecorder := runtimeintrospect.NewKeyStateRecorder()

	return HealthDeps{
		ProxyID:           "test-proxy",
		BuildVersion:      "v0.0.0-test",
		Logger:            testLogger(),
		Readiness:         &atomic.Bool{},
		ThingClient:       nil, // nil — shadowProbe disabled; valid path
		KillSwitch:        ks,
		ExemptionStore:    exStore,
		PayloadCapture:    captureStore,
		CacheManager:      cacheManager,
		DomainEngine:      domainEngine,
		HookConfigCache:   nil,
		ConnManager:       connMgr,
		CertIssuer:        iss,
		ServiceToken:      "test-svc-token",
		ConfigKeyRecorder: keyRecorder,
	}
}

func TestInitHealthHandler_NilThingClient_ReturnsNonNil(t *testing.T) {
	d := buildHealthDeps(t)
	mux, introspectReg := InitHealthHandler(d)
	if mux == nil {
		t.Fatal("expected non-nil ServeMux")
	}
	if introspectReg == nil {
		t.Fatal("expected non-nil introspect registry")
	}
}

func TestInitHealthHandler_CACertEndpoint_ReturnsPEM(t *testing.T) {
	d := buildHealthDeps(t)
	mux, _ := InitHealthHandler(d)

	req := httptest.NewRequest(http.MethodGet, "/management/ca-cert", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Real issuer has a CA cert → 200 with PEM body.
	if got := w.Code; got != http.StatusOK {
		t.Errorf("status = %d; want 200", got)
	}
	body, _ := io.ReadAll(w.Body)
	if len(body) == 0 {
		t.Error("expected non-empty PEM body")
	}
}

func TestInitHealthHandler_CACertEndpoint_MethodNotAllowedForPOST(t *testing.T) {
	d := buildHealthDeps(t)
	mux, _ := InitHealthHandler(d)

	req := httptest.NewRequest(http.MethodPost, "/management/ca-cert", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if got := w.Code; got != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", got)
	}
}

func TestInitHealthHandler_WithThingClient_ShadowProbeWired(t *testing.T) {
	d := buildHealthDeps(t)
	// Use a non-nil ThingClient so the shadowProbeAdapter branch is taken.
	// Do NOT call Start() — the client is only used for getter methods.
	d.ThingClient = newTestThingClient(t)

	mux, _ := InitHealthHandler(d)
	if mux == nil {
		t.Fatal("expected non-nil mux when ThingClient is set")
	}
}

func TestInitHealthHandler_IntrospectSources_ResolveWithoutPanic(t *testing.T) {
	d := buildHealthDeps(t)
	_, introspectReg := InitHealthHandler(d)

	srv := httptest.NewServer(introspectReg.Handler(runtimeintrospect.HandlerOptions{
		Token:  "test-svc-token",
		Logger: testLogger(),
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer test-svc-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("introspect request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Errorf("introspect status = %d; want 200", resp.StatusCode)
	}
}

func TestShadowProbeAdapter_HasReportedFalseBeforeFirstReport(t *testing.T) {
	tc := newTestThingClient(t)
	// Do NOT call tc.Close() — Start() was never called so Close() blocks on <-done.

	adapter := &shadowProbeAdapter{client: tc}
	if adapter.HasReported() {
		t.Error("HasReported() should be false before any report")
	}
}

func TestShadowProbeAdapter_LastReportAgeZeroWhenNoReport(t *testing.T) {
	tc := newTestThingClient(t)
	// Do NOT call tc.Close() — Start() was never called so Close() blocks on <-done.

	adapter := &shadowProbeAdapter{client: tc}
	if age := adapter.LastReportAge(); age != 0 {
		t.Errorf("LastReportAge() = %v; want 0 when no report yet", age)
	}
}

func TestShadowProbeAdapter_StaleAfterIsPositive(t *testing.T) {
	tc := newTestThingClient(t)
	// Do NOT call tc.Close() — Start() was never called so Close() blocks on <-done.

	adapter := &shadowProbeAdapter{client: tc}
	if d := adapter.StaleAfter(); d <= 0 {
		t.Errorf("StaleAfter() = %v; want > 0", d)
	}
}
