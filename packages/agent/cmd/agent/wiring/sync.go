package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/auth"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/status"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// HubClientConfig groups the configuration for the Hub HTTP client.
type HubClientConfig struct {
	HubHTTPURL string
	CertFile   string
	KeyFile    string
	CACertFile string
	// DeviceCAFile is the on-disk path of the Nexus device CA cert used for
	// TLS inspection (e.g. /var/lib/nexus-agent/device-ca.pem). When set,
	// this cert is excluded from the system root pool used for Hub TLS
	// verification to prevent a compromised device CA from forging Hub certs.
	// Optional: when CACertFile is also set, it pins the Hub CA directly and
	// DeviceCAFile exclusion is redundant (but harmless if provided).
	DeviceCAFile string
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
		DeviceCAFile:  cfg.DeviceCAFile,
		Timeout:       30 * time.Second,
		DeviceTokenFn: cfg.DeviceTokenFn,
		ThingIDFn:     cfg.ThingIDFn,
	})
}

// InitEnrollment creates the enrollment client and manager.
// Returns the manager and the Hub enroller for passing to SSO flows.
// renewer installs the device-token renewer used by RenewDeviceToken
// (the Hub HTTP client in production; nil disables renewal).
func InitEnrollment(hubHTTPURL, hubCAFile, certDir string, ks keystore.Store, renewer enrollment.TokenRenewer) (*enrollment.Manager, *enrollment.HubEnrollClient, error) {
	hubEnroller, err := enrollment.NewHubEnrollClient(hubHTTPURL, hubCAFile)
	if err != nil {
		return nil, nil, err
	}
	mgr := enrollment.NewManager(
		certDir,
		enrollment.WithHubEnroller(hubEnroller),
		enrollment.WithTokenRenewer(renewer),
		enrollment.WithKeyStore(ks),
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

// InitThingClientFromStore builds the Hub WebSocket thingclient using the
// device token stored on disk in certDir. Returns nil when HubURL is unset,
// the token is unreadable, or construction fails — in every fallback case the
// agent continues with HTTP audit upload only.
func InitThingClientFromStore(cfg ThingClientConfig, certDir string) *thingclient.Client {
	if cfg.HubURL == "" {
		return nil
	}
	deviceToken, tokenErr := auth.LoadDeviceToken(certDir)
	if tokenErr != nil {
		cfg.Logger.Warn("device token not found, audit uploads will use HTTP fallback only", "error", tokenErr)
		return nil
	}
	cfg.DeviceToken = deviceToken
	tc, tcErr := InitThingClient(cfg)
	if tcErr != nil {
		cfg.Logger.Warn("thingclient init failed, audit uploads will use HTTP fallback only", "error", tcErr)
	}
	return tc
}

// WireThingClientCallbacks attaches the disconnect/reconnect/heartbeat
// callbacks to an already-constructed thingclient. On reconnect the Hub-side
// StaticInfo cache is refreshed and any diag events buffered while the
// WebSocket was down are drained.
func WireThingClientCallbacks(
	tc *thingclient.Client,
	staticInfo registry.StaticInfo,
	statusCollector *status.Collector,
	diag DiagBundle,
	logger *slog.Logger,
) {
	if tc == nil {
		return
	}
	tc.OnDisconnect(func() { statusCollector.SetGatewayConnected(false) })
	tc.OnHeartbeatTick(func() { statusCollector.SetLastHeartbeat(time.Now()) })
	tcLocal, staticInfoLocal := tc, staticInfo
	tc.OnReconnect(func() {
		statusCollector.SetGatewayConnected(true)
		statusCollector.SetLastHeartbeat(time.Now())
		ctxPush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tcLocal.UpdateStaticInfo(ctxPush, staticInfoLocal); err != nil {
			logger.Warn("static_info push failed on reconnect", "error", err)
		}
		drained := diag.ReconnectBuffer.Drain()
		for _, e := range drained {
			if pushErr := tcLocal.PushDiagEvent(ctxPush, e); pushErr != nil {
				logger.Debug("reconnect drain push failed", "error", pushErr, "messageHash", e.MessageHash)
			}
		}
	})
}

// WireConfigChanged installs the Hub config-changed callback onto the
// thingclient, dispatching desired shadow state through the config loader.
// No-op when tc is nil (HTTP-fallback mode has no config push channel).
func WireConfigChanged(tc *thingclient.Client, loader *cfgloader.Loader, logger *slog.Logger) {
	if tc == nil {
		return
	}
	tc.OnConfigChanged(func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
		logger.Info("thing config change received", "config_keys", len(desired))
		reported, applyErr := loader.Apply(context.Background(), desired)
		logger.Info("config apply finished", "reported_keys", len(reported))
		return reported, applyErr
	})
}

// StartThingClient starts the Hub WebSocket connection. On failure it logs a
// warning and returns (nil, nil) so the caller falls back to HTTP audit
// upload. On success it returns the client plus a closer the caller must
// defer, and launches the startup static-info push and the config-startup
// refresh goroutines.
func StartThingClient(
	ctx context.Context,
	tc *thingclient.Client,
	thingID string,
	staticInfo registry.StaticInfo,
	loader *cfgloader.Loader,
	recoveryCfg shareddiag.RecoveryConfig,
	logger *slog.Logger,
) (*thingclient.Client, func()) {
	if tc == nil {
		return nil, nil
	}
	if err := tc.Start(ctx); err != nil {
		logger.Warn("thingclient start failed, falling back to HTTP audit upload", "error", err)
		return nil, nil
	}
	closer := func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = tc.Close(shutdownCtx)
	}
	logger.Info("connected to Hub via thingclient", "thing_id", thingID)
	tcLocal, staticInfoLocal := tc, staticInfo
	go func() {
		rcfg := recoveryCfg
		rcfg.Source = "static-info-push"
		defer shareddiag.Recover(rcfg, nil)
		time.Sleep(500 * time.Millisecond)
		ctxPush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tcLocal.UpdateStaticInfo(ctxPush, staticInfoLocal); err != nil {
			logger.Warn("static_info push failed at startup", "error", err)
		}
	}()
	loaderLocal := loader
	go func() {
		rcfg := recoveryCfg
		rcfg.Source = "config-startup-refresh"
		defer shareddiag.Recover(rcfg, nil)
		time.Sleep(750 * time.Millisecond)
		refreshCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = loaderLocal.RefreshPullKeys(refreshCtx)
	}()
	return tc, closer
}
