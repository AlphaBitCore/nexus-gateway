// Tests for the cmdRun wiring seams, boot half: logger, crash-loop guard,
// enrollment, thingclient construction/start, and SSO auth — the
// constructors the run command calls before its runtime services come up.
// Runtime-half seams are in runwiring_runtime_seams_test.go. Each test
// asserts the seam's observable contract (resolved values, registered
// sources, fallback-to-nil semantics) without OS resources or a live Hub.
package wiring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/bootstrap"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
)

// bootstrap.go — InitLogger

func TestInitLogger_SuccessInstallsDefault(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	logger, err := InitLogger("info", "text", "", false)
	if err != nil {
		t.Fatalf("InitLogger: unexpected error: %v", err)
	}
	if logger == nil {
		t.Fatal("InitLogger: want non-nil logger")
	}
	if slog.Default() != logger {
		t.Error("InitLogger must install the returned logger as the process default")
	}
}

func TestInitLogger_FileIsDirectoryError(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	// A directory path cannot be opened as a log file — the daemon must
	// refuse to start silently logging nowhere.
	logger, err := InitLogger("info", "text", t.TempDir(), false)
	if err == nil {
		t.Fatal("InitLogger with a directory as log file: want error")
	}
	if logger != nil {
		t.Error("InitLogger error path: want nil logger")
	}
}

// bootstrap.go — InitCrashLoopGuard

func TestInitCrashLoopGuard_WritesStatusFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "audit.db")
	InitCrashLoopGuard(os.Args[0], dbPath)
	if _, err := os.Stat(dbPath + ".status"); err != nil {
		t.Errorf("InitCrashLoopGuard must record the start in %s.status: %v", dbPath, err)
	}
}

func TestInitCrashLoopGuard_RollsBackOnCrashLoop(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "audit.db")
	binPath := filepath.Join(dir, "agent-bin")
	if err := os.WriteFile(binPath, []byte("crashed-build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath+".rollback", []byte("previous-good-build"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A start recorded moments ago + a rollback binary on disk = crash loop.
	if err := os.WriteFile(dbPath+".status", []byte(time.Now().Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}

	InitCrashLoopGuard(binPath, dbPath)

	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "previous-good-build" {
		t.Errorf("crash loop must roll the binary back to the previous build, got %q", got)
	}
	if _, err := os.Stat(binPath + ".rollback"); !os.IsNotExist(err) {
		t.Error("rollback artifact must be consumed by the rename")
	}
}

// sync.go — InitEnrollment (renewer threading + CA error path)

func TestInitEnrollment_BadCAFileError(t *testing.T) {
	// Pass a CA file path that does not exist — NewHubEnrollClient must fail.
	_, _, err := InitEnrollment("https://hub.example.test", "/nonexistent/ca.pem", t.TempDir(), keystore.NewMemoryStore(), nil)
	if err == nil {
		t.Error("InitEnrollment with missing CA file should return error")
	}
}

func TestInitEnrollment_EmptyCAFileSucceeds(t *testing.T) {
	// Empty CA file means system roots — the manager and enroller must both
	// construct so a default deployment needs no pinned CA on disk.
	mgr, enroller, err := InitEnrollment("https://hub.example.test", "", t.TempDir(), keystore.NewMemoryStore(), nil)
	if err != nil {
		t.Fatalf("InitEnrollment with empty CA file: unexpected error: %v", err)
	}
	if mgr == nil || enroller == nil {
		t.Error("InitEnrollment: want non-nil manager and enroller")
	}
}

// sync.go — InitThingClientFromStore fallback semantics

func TestInitThingClientFromStore_EmptyHubURL(t *testing.T) {
	tc := InitThingClientFromStore(ThingClientConfig{
		HubURL: "",
		Logger: discardLogger2(),
	}, t.TempDir())
	if tc != nil {
		t.Error("empty HubURL: want nil client (HTTP fallback)")
	}
}

func TestInitThingClientFromStore_MissingDeviceToken(t *testing.T) {
	// HubURL set but no device-token file in certDir — the daemon must fall
	// back to HTTP audit upload instead of failing boot.
	tc := InitThingClientFromStore(ThingClientConfig{
		HubURL: "wss://hub.fake.test",
		Logger: discardLogger2(),
	}, t.TempDir())
	if tc != nil {
		t.Error("missing device token: want nil client (HTTP fallback)")
	}
}

// sync.go — WireThingClientCallbacks

func TestWireThingClientCallbacks_NilTC(t *testing.T) {
	// Should not panic when tc is nil.
	WireThingClientCallbacks(nil, registry.StaticInfo{}, nil, DiagBundle{}, nil)
}

func TestWireThingClientCallbacks_NonNilTC(t *testing.T) {
	tc := minimalThingClient(t)
	// Build a real status.Collector we can call from the callbacks.
	bc := bootstrap.New("", nil, "")
	mgr := enrollment.NewManager(t.TempDir())
	qAllowed := true
	collector := InitPendingStatusCollector(PendingStatusCollectorConfig{
		Version:         "1.0.0",
		EnrollMgr:       mgr,
		BootstrapClient: bc,
		QuitAllowed:     &qAllowed,
	})

	// Must not panic; callbacks are stored but not fired here.
	WireThingClientCallbacks(tc, registry.StaticInfo{}, collector, DiagBundle{}, discardLogger2())
}

// sync.go — WireConfigChanged (nil tc is no-op)

func TestWireConfigChanged_NilTC(t *testing.T) {
	// HTTP-fallback mode has no config push channel — must be a no-op.
	WireConfigChanged(nil, nil, discardLogger2())
}

// sync.go — StartThingClient (nil tc keeps HTTP fallback)

func TestStartThingClient_NilTC(t *testing.T) {
	tc, closer := StartThingClient(context.Background(), nil, "thing-1",
		registry.StaticInfo{}, nil, shareddiag.RecoveryConfig{}, discardLogger2())
	if tc != nil {
		t.Error("nil input client: want nil output client")
	}
	if closer != nil {
		t.Error("nil input client: want nil closer (nothing to close)")
	}
}

func TestStartThingClient_StartedClientReturnsCloser(t *testing.T) {
	tc := minimalThingClient(t)
	loader := cfgloader.New(discardLogger2(), nil, "thing-1", "agent")
	got, closer := StartThingClient(context.Background(), tc, "thing-1",
		registry.StaticInfo{}, loader, shareddiag.RecoveryConfig{}, discardLogger2())
	if got != tc {
		t.Error("started client must be returned for downstream wiring")
	}
	if closer == nil {
		t.Fatal("started client must come with a closer for the shutdown defer")
	}
	closer()
}

// identity.go — InitSSOAuth + BuildResolveCpURL

func TestInitSSOAuth_PopulatesFlowAndOnSuccess(t *testing.T) {
	mgr := enrollment.NewManager(t.TempDir())
	bc := bootstrap.New("https://hub.fake.test", bootstrap.DefaultHTTPClient(), "")
	called := false
	s := InitSSOAuth(SSOAuthConfig{
		Manager:      mgr,
		Bootstrap:    bc,
		OSVersion:    "14.5",
		AgentVersion: "1.2.3",
		OnSuccess:    func() { called = true },
	})
	if s == nil || s.Flow == nil {
		t.Fatal("InitSSOAuth: want non-nil state with a Flow")
	}
	if s.Flow.OS != runtime.GOOS || s.Flow.OSVersion != "14.5" || s.Flow.AgentVersion != "1.2.3" {
		t.Errorf("Flow identity fields wrong: os=%q osVersion=%q agentVersion=%q",
			s.Flow.OS, s.Flow.OSVersion, s.Flow.AgentVersion)
	}
	if s.Flow.Manager != mgr || s.Mgr != mgr {
		t.Error("InitSSOAuth must thread the enrollment manager into Flow and state")
	}
	if s.Flow.ResolveCpURL == nil {
		t.Error("InitSSOAuth must install a lazy Control Plane URL resolver")
	}
	if s.OnSuccess == nil {
		t.Fatal("InitSSOAuth must keep the OnSuccess hook")
	}
	s.OnSuccess()
	if !called {
		t.Error("OnSuccess hook not threaded through")
	}
}

func TestBuildResolveCpURL_EmptyControlPlaneURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"controlPlaneURL":"","deviceAuthMode":"enterprise-login"}`))
	}))
	defer srv.Close()
	resolve := BuildResolveCpURL(bootstrap.New(srv.URL, srv.Client(), ""))
	_, err := resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "empty controlPlaneURL") {
		t.Errorf("want 'empty controlPlaneURL' error when Hub publishes no CP URL, got %v", err)
	}
}

func TestBuildResolveCpURL_ReturnsPublishedURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"controlPlaneURL":"https://cp.example.test","deviceAuthMode":"enterprise-login"}`))
	}))
	defer srv.Close()
	resolve := BuildResolveCpURL(bootstrap.New(srv.URL, srv.Client(), ""))
	got, err := resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "https://cp.example.test" {
		t.Errorf("resolved CP URL = %q, want the Hub-published one", got)
	}
}
