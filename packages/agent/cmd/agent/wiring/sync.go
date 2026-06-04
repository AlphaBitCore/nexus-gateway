package wiring

import (
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/status"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// HubClientConfig groups the configuration for the Hub HTTP client.
type HubClientConfig struct {
	HubHTTPURL string
	CertFile   string
	KeyFile    string
	CACertFile string
	// DeviceTokenFn returns the current device token (lazy, so it can
	// be wired before enrollment completes).
	DeviceTokenFn func() string
	// ThingIDFn returns the current thing ID (lazy, same reason).
	ThingIDFn func() string
}

// InitHubClient creates the mTLS Hub HTTP client used for audit upload,
// exemption upload, update check, and cert renew.
func InitHubClient(cfg HubClientConfig) (*hub.Client, error) {
	return hub.NewClient(hub.Config{
		HubURL:        cfg.HubHTTPURL,
		CertFile:      cfg.CertFile,
		KeyFile:       cfg.KeyFile,
		CACertFile:    cfg.CACertFile,
		Timeout:       30 * time.Second,
		DeviceTokenFn: cfg.DeviceTokenFn,
		ThingIDFn:     cfg.ThingIDFn,
	})
}

// InitEnrollment creates the enrollment client and manager.
// Returns the manager and the Hub enroller for passing to SSO flows.
func InitEnrollment(hubHTTPURL, hubCAFile, certDir string) (*enrollment.Manager, *enrollment.HubEnrollClient, error) {
	hubEnroller, err := enrollment.NewHubEnrollClient(hubHTTPURL, hubCAFile)
	if err != nil {
		return nil, nil, err
	}
	mgr := enrollment.NewManager(
		certDir,
		enrollment.WithHubEnroller(hubEnroller),
	)
	return mgr, hubEnroller, nil
}

// ThingClientConfig groups the configuration for the Hub WebSocket thingclient.
type ThingClientConfig struct {
	HubURL       string
	HubHTTPURL   string
	ThingID      string
	Version      string
	DeviceToken  string
	Logger       *slog.Logger
	ProcessStart time.Time
	OpsReg       *registry.Registry
	// ComposeVersionFn composes a full composite version string
	// (daemon + macOS bundle inventory). Pass composeThingVersion from main package.
	ComposeVersionFn func(version string) string
}

// InitThingClient creates the Hub WebSocket thingclient. Returns nil (no error)
// when HubURL is empty or the device token is not available — the caller falls
// back to HTTP audit upload only.
func InitThingClient(cfg ThingClientConfig) (*thingclient.Client, error) {
	if cfg.HubURL == "" || cfg.DeviceToken == "" {
		return nil, nil
	}
	opsSampler := platform.NewSampler(cfg.ThingID, cfg.ProcessStart, cfg.OpsReg)
	composedVersion := cfg.Version
	if cfg.ComposeVersionFn != nil {
		composedVersion = cfg.ComposeVersionFn(cfg.Version)
	}
	tcCfg := thingclient.Config{
		HubURL:            cfg.HubURL,
		HubHTTPURL:        cfg.HubHTTPURL,
		ThingType:         "agent",
		ThingID:           cfg.ThingID,
		ThingVersion:      composedVersion,
		Token:             cfg.DeviceToken,
		Logger:            cfg.Logger,
		OpsMetricsSampler: opsSampler,
	}
	cfg.Logger.Info("thingclient ThingVersion composed", "thing_version", composedVersion)
	return thingclient.New(tcCfg)
}

// WireThingClientCallbacks attaches the disconnect/reconnect/heartbeat
// callbacks to an already-constructed thingclient.
func WireThingClientCallbacks(
	tc *thingclient.Client,
	staticInfo registry.StaticInfo,
	statusCollector *status.Collector,
	logger *slog.Logger,
) {
	if tc == nil {
		return
	}
	tc.OnDisconnect(func() {
		statusCollector.SetGatewayConnected(false)
	})
	tc.OnHeartbeatTick(func() {
		statusCollector.SetLastHeartbeat(time.Now())
	})
	tc.OnReconnect(func() {
		statusCollector.SetGatewayConnected(true)
		statusCollector.SetLastHeartbeat(time.Now())
		// Push static_info on reconnect so Hub-side StaticInfo cache is refreshed.
		// (The actual push is a goroutine in cmdRun — this hook triggers it.)
	})
}
