// Tests for the cmdRun wiring seams, runtime half: the subsystems the run
// command constructs after enrollment succeeds (introspection, spill
// transport, lifecycle emitter, shutdown drain, platform interception,
// status API, bridge deps, audit queue, normalize registry, updater).
// Boot-phase seams (logger, crash-loop guard, enrollment, thingclient,
// SSO auth) are in runwiring_seams_test.go. Each test asserts the seam's
// observable contract without OS resources or a live Hub.
package wiring

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/bootstrap"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/killswitch"
	lifecycle "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/state"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	config "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/schema"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// observability.go — RegisterConfigIntrospection

func TestRegisterConfigIntrospection_SnapshotServesBothSources(t *testing.T) {
	reg := InitIntrospect("thing-1", "1.0.0")
	ks := killswitch.New(discardLogger2())
	pc := payloadcapture.NewStore(payloadcapture.Config{StoreRequestBody: true})
	RegisterConfigIntrospection(reg, ks, pc)

	snap := reg.Snapshot(context.Background())
	ksRes, ok := snap.Sources["config.killswitch"]
	if !ok || !ksRes.OK {
		t.Errorf("config.killswitch source missing or failed: %+v", ksRes)
	}
	pcRes, ok := snap.Sources["config.payload_capture"]
	if !ok || !pcRes.OK {
		t.Fatalf("config.payload_capture source missing or failed: %+v", pcRes)
	}
	cfg, ok := pcRes.Value.(payloadcapture.Config)
	if !ok || !cfg.StoreRequestBody {
		t.Errorf("payload_capture snapshot must reflect the live store value, got %+v", pcRes.Value)
	}
}

// spill.go — InitSpillTransport

func TestInitSpillTransport_UploaderAlwaysBuilt(t *testing.T) {
	uploader, _ := InitSpillTransport(nil, keystore.NewMemoryStore(), discardLogger2())
	if uploader == nil {
		t.Error("InitSpillTransport: uploader must be non-nil even without a Hub client")
	}
	// The local spill reader is OS-keystore-backed; its nil/non-nil outcome
	// is environment-dependent and exercised by the spill store tests.
}

// lifecycle.go — EmitShutdownGracefully

func TestEmitShutdownGracefully_NilEmitterNoOp(t *testing.T) {
	start := time.Now()
	EmitShutdownGracefully(nil, "test")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("nil emitter must return immediately (no flush sleep), took %v", elapsed)
	}
}

func TestEmitShutdownGracefully_RecordsEventWithFlushWindow(t *testing.T) {
	q := newTestQueue2(t)
	defer q.Close() //nolint:errcheck
	e := InitLifecycleEmitter(nil, q, LifecycleEmitterConfig{
		ThingID: "thing-1", AgentVersion: "1.0.0", Logger: discardLogger2(),
	})
	start := time.Now()
	EmitShutdownGracefully(e, "test_reason")
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("flush window must hold at least 200ms for the WS outbox, got %v", elapsed)
	}
	events, _, err := q.QueryLifecycle(0, 10)
	if err != nil {
		t.Fatalf("QueryLifecycle: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == lifecycle.ActionShutdown {
			found = true
		}
	}
	if !found {
		t.Errorf("agent.shutdown lifecycle event not recorded, got %+v", events)
	}
}

func TestInitLifecycleEmitter_NonNilClientEmitsThroughPusher(t *testing.T) {
	q := newTestQueue2(t)
	defer q.Close() //nolint:errcheck
	tc := minimalThingClient(t)
	e := InitLifecycleEmitter(tc, q, LifecycleEmitterConfig{
		ThingID: "thing-1", AgentVersion: "1.0.0", Logger: discardLogger2(),
	})
	// With a real client wired as Pusher, an emit must both record locally
	// and enqueue the Hub push without crashing.
	e.Paused(5)
	events, _, err := q.QueryLifecycle(0, 10)
	if err != nil {
		t.Fatalf("QueryLifecycle: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == lifecycle.ActionPaused {
			found = true
		}
	}
	if !found {
		t.Errorf("agent.paused lifecycle event not recorded, got %+v", events)
	}
}

// shutdown.go — WaitForAuditDrain

func TestWaitForAuditDrain_AlreadyDrained(t *testing.T) {
	var wg sync.WaitGroup // zero outstanding work
	start := time.Now()
	WaitForAuditDrain(&wg, shareddiag.RecoveryConfig{})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("drained queue must not hit the 10s timeout, took %v", elapsed)
	}
}

// network.go — StartPlatformInterception

type startCapturePlatform struct {
	started chan api.ConnectionHandler
}

func (f *startCapturePlatform) Start(_ context.Context, h api.ConnectionHandler) error {
	f.started <- h
	return nil
}
func (f *startCapturePlatform) Stop() error { return nil }
func (f *startCapturePlatform) ProcessInfo(int) (api.ProcessMeta, error) {
	return api.ProcessMeta{}, nil
}

func TestStartPlatformInterception_StartsPlatformWithHandler(t *testing.T) {
	plat := &startCapturePlatform{started: make(chan api.ConnectionHandler, 1)}
	handler := InitConnectionBridge(ConnectionBridgeConfig{ThingID: "thing-1"})
	StartPlatformInterception(context.Background(), plat, handler, shareddiag.RecoveryConfig{})
	select {
	case got := <-plat.started:
		if got != api.ConnectionHandler(handler) {
			t.Error("platform must be started with the wired connection bridge")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("platform Start was never invoked")
	}
}

// statusapi.go — InitStatusServer

func TestInitStatusServer_BuildsServer(t *testing.T) {
	q := newTestQueue2(t)
	defer q.Close() //nolint:errcheck
	mgr := enrollment.NewManager(t.TempDir())
	bc := bootstrap.New("", nil, "")
	auth := InitSSOAuth(SSOAuthConfig{Manager: mgr, Bootstrap: bc})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := InitStatusServer(StatusServerDeps{
		SocketPath: filepath.Join(t.TempDir(), "status.sock"),
		Ctx:        ctx,
		Cancel:     cancel,
		Version:    "1.0.0",
		AuditQueue: q,
		ConfigMgr:  config.NewManager(&config.AgentConfig{}),
		Auth:       auth,
	})
	if srv == nil {
		t.Fatal("InitStatusServer: want non-nil server")
	}
}

// statusapi.go — StartStatusAPI

func TestStartStatusAPI_ServesStatusSocket(t *testing.T) {
	q := newTestQueue2(t)
	defer q.Close() //nolint:errcheck
	bcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"controlPlaneURL":"https://cp.example.test","deviceAuthMode":"enterprise-login"}`))
	}))
	defer bcSrv.Close()
	bc := bootstrap.New(bcSrv.URL, bcSrv.Client(), "")
	mgr := enrollment.NewManager(t.TempDir())
	auth := InitSSOAuth(SSOAuthConfig{Manager: mgr, Bootstrap: bc})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Unix socket paths are capped at ~104 bytes on macOS; t.TempDir() under
	// /var/folders exceeds it, so use a short /tmp directory instead.
	sockDir, err := os.MkdirTemp("/tmp", "nx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	srv := InitStatusServer(StatusServerDeps{
		SocketPath: sock,
		Ctx:        ctx,
		Cancel:     cancel,
		Version:    "1.0.0",
		AuditQueue: q,
		ConfigMgr:  config.NewManager(&config.AgentConfig{}),
		Auth:       auth,
	})
	StartStatusAPI(srv, bc, InitIntrospect("thing-1", "1.0.0"), shareddiag.RecoveryConfig{})
	defer srv.Stop()

	// The accept loop must actually come up on the socket.
	var conn net.Conn
	var dialErr error
	for range 50 {
		conn, dialErr = net.Dial("unix", sock)
		if dialErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("status socket never came up: %v", dialErr)
	}
	_ = conn.Close()
}

// bridgedeps.go — WireInspectBridge

func TestWireInspectBridge_FailOpenWithoutPipeline(t *testing.T) {
	// darwin: no-op by design (the NE bridge wires its own deps); other OS:
	// a missing AgentPipeline must fail open with a warning, leaving the
	// platform's inspect path unwired. Either way the call must not panic.
	WireInspectBridge(&startCapturePlatform{started: make(chan api.ConnectionHandler, 1)},
		BridgeDepsArgs{Logger: discardLogger2()})
}

// bridgedeps.go — BuildBridgeDeps requires a pipeline

func TestBuildBridgeDeps_NilPipelineError(t *testing.T) {
	// Without an AgentPipeline there is no policy resolver to bump flows
	// through — the build must fail (caller leaves inspect unwired, fail-open)
	// rather than return half-wired deps.
	deps, err := BuildBridgeDeps(BridgeDepsArgs{Logger: discardLogger2()})
	if err == nil || !strings.Contains(err.Error(), "AgentPipeline is required") {
		t.Errorf("want 'AgentPipeline is required' error, got deps=%v err=%v", deps, err)
	}
}

// observability.go — InitAuditQueue (keystore-keyed SQLCipher open)

func TestInitAuditQueue_KeyedByInjectedKeystore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "audit.db")
	ks := keystore.NewMemoryStore()

	q, err := InitAuditQueue(dbPath, ks, discardLogger2())
	if err != nil {
		t.Fatalf("InitAuditQueue: %v", err)
	}
	if q == nil {
		t.Fatal("InitAuditQueue: want non-nil queue")
	}
	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen with the SAME keystore: the derived DB key must be stable so the
	// daemon can decrypt its own queue across restarts.
	q2, err := InitAuditQueue(dbPath, ks, discardLogger2())
	if err != nil {
		t.Fatalf("reopen with same keystore must succeed: %v", err)
	}
	_ = q2.Close()

	// Reopen with a DIFFERENT keystore (fresh random key): SQLCipher must
	// refuse — proving the injected keystore actually keys the encryption
	// at rest rather than being decorative.
	if q3, err := InitAuditQueue(dbPath, keystore.NewMemoryStore(), discardLogger2()); err == nil {
		_ = q3.Close()
		t.Error("reopen with a different keystore key must fail to decrypt the DB")
	}
}

// normalize.go — InitNormalizeRegistry

func TestInitNormalizeRegistry_BuildsSharedChain(t *testing.T) {
	reg := InitNormalizeRegistry()
	if reg == nil {
		t.Fatal("InitNormalizeRegistry: want non-nil registry")
	}
	// The shared Tier 1+2+3 chain must include the OpenAI chat codec — the
	// same registry Hub agent_audit / ai-gateway / compliance-proxy build.
	keys := reg.All()
	found := false
	for _, k := range keys {
		if k == "openai::/v1/chat/completions" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("registry missing openai chat codec; got %d keys", len(keys))
	}
}

// updater.go — StartUpdater (availability transition reaches the callback)

func TestStartUpdater_AvailabilityReachesCallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/internal/things/update-check") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"available":true,"version":"9.9.9"}`))
	}))
	defer srv.Close()
	hubClient, err := InitHubClient(HubClientConfig{HubHTTPURL: srv.URL})
	if err != nil {
		t.Fatalf("InitHubClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := InitUpdater(hubClient, false, 3600, "1.0.0", runtime.GOOS, os.Args[0], t.TempDir())

	got := make(chan bool, 1)
	StartUpdater(ctx, up, shareddiag.RecoveryConfig{}, func(b bool) { got <- b })

	select {
	case avail := <-got:
		if !avail {
			t.Error("Hub reports an update: the Dashboard callback must receive true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("availability callback never fired")
	}
}
