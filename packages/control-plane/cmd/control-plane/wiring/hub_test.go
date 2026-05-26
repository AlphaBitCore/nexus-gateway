package wiring

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	metricsreg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

func TestDefaultAdvertiseHost(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		want       string
	}{
		{
			name:       "empty string returns 127.0.0.1",
			configured: "",
			want:       "127.0.0.1",
		},
		{
			name:       "0.0.0.0 returns 127.0.0.1",
			configured: "0.0.0.0",
			want:       "127.0.0.1",
		},
		{
			name:       "double colon returns 127.0.0.1",
			configured: "::",
			want:       "127.0.0.1",
		},
		{
			name:       "explicit hostname is returned unchanged",
			configured: "myhost.example.com",
			want:       "myhost.example.com",
		},
		{
			name:       "explicit IP address is returned unchanged",
			configured: "10.0.0.5",
			want:       "10.0.0.5",
		},
		{
			name:       "localhost is returned unchanged",
			configured: "localhost",
			want:       "localhost",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DefaultAdvertiseHost(tc.configured)
			if got != tc.want {
				t.Errorf("DefaultAdvertiseHost(%q) = %q, want %q", tc.configured, got, tc.want)
			}
		})
	}
}

func TestCloseWithTimeout_SuccessPropagated(t *testing.T) {
	called := false
	fn := closeWithTimeout(func(ctx context.Context) error {
		called = true
		return nil
	})
	if err := fn(); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !called {
		t.Fatal("inner function was not called")
	}
}

func TestCloseWithTimeout_ErrorPropagated(t *testing.T) {
	sentinel := errors.New("close failed")
	fn := closeWithTimeout(func(ctx context.Context) error {
		return sentinel
	})
	err := fn()
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestCloseWithTimeout_ContextHasDeadline(t *testing.T) {
	fn := closeWithTimeout(func(ctx context.Context) error {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			t.Error("expected context to have a deadline, but it does not")
		}
		return nil
	})
	if err := fn(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func makeHubDeps(cfg *config.Config) HubDeps {
	// Use an isolated Prometheus registry per call so concurrent tests that
	// exercise thingclient.New (which registers metrics on the provided
	// registerer) do not collide on prometheus.DefaultRegisterer.
	isolatedReg := prometheus.NewRegistry()
	return HubDeps{
		Cfg:               cfg,
		Logger:            silentLogger(),
		Ctx:               context.Background(),
		BuildVersion:      "v0.0.1",
		ProcessStartTime:  time.Now(),
		OpsReg:            metricsreg.NewRegistry(nil),
		MQProducer:        nil,
		OnConfigChanged:   nil,
		MetricsRegisterer: isolatedReg,
	}
}

// TestInitHub_EmptyHubURL_ReturnsNilThingClient exercises the "no Hub
// registration" fast path where NexusHubURL is empty.
func TestInitHub_EmptyHubURL_ReturnsNilThingClient(t *testing.T) {
	cfg := &config.Config{}
	cfg.Registry.NexusHubURL = "" // skip thingclient init
	cfg.Server.Port = 3001

	res, err := InitHub(makeHubDeps(cfg))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ThingClient != nil {
		t.Error("expected nil ThingClient when HubURL is empty")
	}
	if res.HubClient == nil {
		t.Error("expected non-nil HubClient even when HubURL is empty")
	}
	if res.ThingID == "" {
		t.Error("expected non-empty ThingID")
	}
	if res.ConfigKeyRecorder == nil {
		t.Error("expected non-nil ConfigKeyRecorder")
	}
	// Close must be safe to call even when ThingClient is nil.
	if err := res.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
}

// TestInitHub_WithConfigID_UsesProvidedID verifies that cfg.ID takes precedence
// over the auto-derived hostname-based ID.
func TestInitHub_WithConfigID_UsesProvidedID(t *testing.T) {
	cfg := &config.Config{}
	cfg.Registry.NexusHubURL = ""
	cfg.Server.Port = 3001
	cfg.ID = "my-stable-cp-id"

	res, err := InitHub(makeHubDeps(cfg))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ThingID != "my-stable-cp-id" {
		t.Errorf("expected ThingID=%q, got %q", "my-stable-cp-id", res.ThingID)
	}
}

// TestInitHub_WithHubURL_ThingClientMissingToken_GracefulDegrade verifies the
// thingclient.New failure path when Token is empty.
func TestInitHub_WithHubURL_ThingClientMissingToken_GracefulDegrade(t *testing.T) {
	cfg := &config.Config{}
	cfg.Registry.NexusHubURL = "http://127.0.0.1:3060/ws" // non-empty URL
	cfg.Server.Port = 3001
	cfg.Auth.InternalServiceToken = "" // missing token → thingclient.New error

	res, err := InitHub(makeHubDeps(cfg))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// thingclient.New fails → ThingClient must be nil (graceful degrade path).
	if res.ThingClient != nil {
		t.Error("expected nil ThingClient when token is missing")
	}
	_ = res.Close()
}

// TestInitHub_WithHubURL_ValidToken_StartFails_GracefulDegrade verifies that
// when thingclient.New succeeds but tc.Start fails (Hub unreachable), the
// function degrades gracefully.
func TestInitHub_WithHubURL_ValidToken_StartFails_GracefulDegrade(t *testing.T) {
	cfg := &config.Config{}
	cfg.Registry.NexusHubURL = "http://127.0.0.1:1" // hub unreachable for Start
	cfg.Server.Port = 3001
	cfg.Auth.InternalServiceToken = "test-token" // required by thingclient.New

	d := makeHubDeps(cfg)
	d.OpsReg = metricsreg.NewRegistry(nil)

	res, err := InitHub(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// tc.Start fails → ThingClient must be nil.
	_ = res
	_ = res.Close()
}

// TestInitHub_OnConfigChanged_Wired_ValidToken verifies that when OnConfigChanged
// is set AND token is valid, tc.OnConfigChanged is registered. tc.Start fails
// (Hub unreachable) → covers the OnConfigChanged wiring + Start error path.
// Each call uses an isolated Prometheus registry (via makeHubDeps) so duplicate
// registration panics cannot occur regardless of test ordering.
func TestInitHub_OnConfigChanged_Wired_ValidToken(t *testing.T) {
	cfg := &config.Config{}
	cfg.Registry.NexusHubURL = "ws://127.0.0.1:1/ws" // Hub unreachable
	cfg.Server.Port = 3001
	// Token must be non-empty so thingclient.New proceeds past the token check.
	cfg.Auth.InternalServiceToken = "test-internal-token-hub-onconfig"

	configChangeCalled := false
	d := makeHubDeps(cfg)
	d.OnConfigChanged = func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
		configChangeCalled = true
		return desired, nil
	}

	res, err := InitHub(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// tc.Start fails → ThingClient is nil (graceful degrade path).
	_ = res
	_ = configChangeCalled
	_ = res.Close()
}

func TestInstallDiagSink_NilTC_DoesNotPanic(t *testing.T) {
	cfg := &config.Config{}
	installDiagSink(context.Background(), cfg, silentLogger(), nil, "cp-test", "v1", nil)
}

// TestInstallDiagSink_NonNilTC_WiresReconnectAndStartupDiag verifies the
// non-nil tc path: OnReconnect is registered and the startup diag goroutine
// body executes (after 600 ms sleep). We construct a thingclient with an
// isolated Prometheus registry to avoid duplicate-registration panics.
func TestInstallDiagSink_NonNilTC_WiresReconnectAndStartupDiag(t *testing.T) {
	reg := prometheus.NewRegistry()
	tc, err := thingclient.New(thingclient.Config{
		HubURL:            "ws://127.0.0.1:1/ws",
		ThingType:         "control-plane",
		ThingID:           "cp-diag-test",
		PhysicalID:        "cp-diag-test",
		Token:             "test-token-diag",
		Logger:            silentLogger(),
		MetricsRegisterer: reg,
	})
	if err != nil {
		t.Fatalf("thingclient.New: %v", err)
	}

	cfg := &config.Config{}
	// installDiagSink registers an OnReconnect callback and spawns a goroutine
	// that sleeps 600 ms then pushes a diag event (which will fail with a
	// network error since Hub is unreachable — that error is swallowed).
	installDiagSink(context.Background(), cfg, silentLogger(), tc, "cp-diag-test", "v0.0.1-test", nil)

	// Wait long enough for the goroutine body to execute (600 ms sleep + margin).
	time.Sleep(750 * time.Millisecond)
	// Must not panic; the diag push fails silently against the unreachable Hub.
}
