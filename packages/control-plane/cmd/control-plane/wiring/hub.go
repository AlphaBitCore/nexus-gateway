package wiring

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	metricsplatform "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	metricsreg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// DeriveThingID returns the Control Plane's ThingID using the same
// convention InitHub registers under: the yaml `id` field when set,
// otherwise `cp-<hostname>-<port>`. Exposed so other wiring functions
// (e.g. InitJWT, which builds a per-instance JetStream durable name
// before InitHub has a chance to compute this) get the same value.
func DeriveThingID(cfg *config.Config) string {
	if cfg.ID != "" {
		return cfg.ID
	}
	hostname, _ := os.Hostname()
	return fmt.Sprintf("cp-%s-%d", hostname, cfg.Server.Port)
}

// HubResult holds every handle produced by InitHub.
type HubResult struct {
	// HubClient is the plain HTTP client used for admin API calls to Hub.
	HubClient *hub.Client
	// ThingClient is the WebSocket-primary registration client; may be nil
	// when cfg.Registry.NexusHubURL is empty or startup failed.
	ThingClient *thingclient.Client
	// ThingID is the stable identifier used for Hub registration.
	ThingID string
	// ConfigKeyRecorder captures every desired config_key acknowledged from Hub.
	ConfigKeyRecorder *runtimeintrospect.KeyStateRecorder
	// Close shuts down the thingclient gracefully with a 5-second timeout.
	// Safe to call even when ThingClient is nil.
	Close func() error
}

// closeWithTimeout wraps a context-aware close with a 5-second timeout.
func closeWithTimeout(fn func(context.Context) error) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return fn(ctx)
	}
}

// HubDeps carries inputs that InitHub cannot derive itself.
type HubDeps struct {
	Cfg              *config.Config
	Logger           *slog.Logger
	Ctx              context.Context
	BuildVersion     string
	ProcessStartTime time.Time
	OpsReg           *metricsreg.Registry
	MQProducer       mq.Producer
	// OnConfigChanged is called by thingclient on every Hub-pushed config delta.
	// Signature matches thingclient.Client.OnConfigChanged callback contract.
	OnConfigChanged func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error)
	// MetricsRegisterer is the Prometheus registerer for thingclient metrics.
	// If nil, prometheus.DefaultRegisterer is used. Tests should pass an
	// isolated registry to avoid duplicate-registration panics across parallel runs.
	MetricsRegisterer prometheus.Registerer
}

// InitHub creates the Hub HTTP client, registers the CP as a Thing via
// thingclient (WebSocket primary, HTTP fallback), installs the diag sink, and
// wires the static_info push.  The caller must call ThingClient.Close when
// shutting down.
//
// Note: InitHub mutates the global slog default to layer the diag sink on top
// of the base logger.  The returned logger (via slog.Default()) should be used
// by all downstream subsystems.
func InitHub(d HubDeps) (HubResult, error) {
	cfg := d.Cfg
	logger := d.Logger

	hubHTTPC := nexushttp.New(nexushttp.Config{
		Timeout:        time.Duration(cfg.HTTPClients.Hub.TimeoutSec) * time.Second,
		Caller:         "cp-hub-main",
		PropagateReqID: true,
	})
	hubClient := hub.New(cfg.Registry.NexusHubURL, cfg.Auth.InternalServiceToken, hubHTTPC, logger)

	cpID := DeriveThingID(cfg)

	configKeyRecorder := runtimeintrospect.NewKeyStateRecorder()

	res := HubResult{
		HubClient:         hubClient,
		ThingID:           cpID,
		ConfigKeyRecorder: configKeyRecorder,
		Close:             func() error { return nil },
	}

	if cfg.Registry.NexusHubURL == "" {
		installDiagSink(d.Ctx, cfg, logger, nil, cpID, d.BuildVersion, d.OpsReg)
		return res, nil
	}

	opsSampler := metricsplatform.NewSampler(cpID, d.ProcessStartTime, d.OpsReg)
	advertiseHost := DefaultAdvertiseHost(cfg.Server.AdvertiseHost)
	tc, err := thingclient.New(thingclient.Config{
		HubURL:            cfg.Registry.NexusHubURL + "/ws",
		ThingType:         "control-plane",
		ThingID:           cpID,
		PhysicalID:        cpID,
		ThingVersion:      d.BuildVersion,
		ListenAddress:     fmt.Sprintf(":%d", cfg.Server.Port),
		MetricsURL:        fmt.Sprintf("http://%s:%d/metrics", advertiseHost, cfg.Server.Port),
		Role:              "api",
		Token:             cfg.Auth.InternalServiceToken,
		Logger:            logger,
		MQProducer:        d.MQProducer,
		OpsMetricsSampler: opsSampler,
		MetricsRegisterer: d.MetricsRegisterer,
	})
	if err != nil {
		logger.Warn("thingclient init failed, running without Hub registration", "error", err)
		installDiagSink(d.Ctx, cfg, logger, nil, cpID, d.BuildVersion, d.OpsReg)
		return res, nil
	}

	if d.OnConfigChanged != nil {
		tc.OnConfigChanged(d.OnConfigChanged)
	}

	if err := tc.Start(d.Ctx); err != nil {
		logger.Warn("thingclient start failed", "error", err)
		installDiagSink(d.Ctx, cfg, logger, nil, cpID, d.BuildVersion, d.OpsReg)
		return res, nil
	}

	res.ThingClient = tc
	res.Close = closeWithTimeout(tc.Close)
	logger.Info("registered with Hub as Thing", "thingID", cpID)

	// Push static_info at startup and on every reconnect.
	staticInfo := metricsplatform.CaptureStaticInfo(metricsplatform.BuildInfo{
		ServiceVersion: "control-plane/0.1.0",
		BuildSHA:       "",
		BuildTime:      "",
		StartTime:      d.ProcessStartTime.Format(time.RFC3339),
		PublicURL:      cfg.PublicURL,
	})
	go func() {
		time.Sleep(500 * time.Millisecond)
		ctxPush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tc.UpdateStaticInfo(ctxPush, staticInfo); err != nil {
			logger.Warn("static_info push failed at startup", "error", err)
		}
	}()
	tc.OnReconnect(func() {
		ctxPush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tc.UpdateStaticInfo(ctxPush, staticInfo); err != nil {
			logger.Warn("static_info push failed on reconnect", "error", err)
		}
	})

	installDiagSink(d.Ctx, cfg, logger, tc, cpID, d.BuildVersion, d.OpsReg)
	return res, nil
}

// installDiagSink layers the shared diag sink on top of the current slog
// default and wires reconnect-buffer draining when tc is non-nil.
func installDiagSink(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	tc *thingclient.Client,
	cpID string,
	buildVersion string,
	opsReg *metricsreg.Registry,
) {
	diagReconnectBuf := shareddiag.NewReconnectBuffer(shareddiag.ReconnectBufferConfig{})
	diagSink := shareddiag.NewSlogSink(shareddiag.SlogSinkConfig{
		ThingClient:     tc,
		ReconnectBuffer: diagReconnectBuf,
		IsWSConnected: func() bool {
			return tc != nil && tc.Mode() == thingclient.ModeWSConnected
		},
		ThingID: cpID,
		Source:  "control-plane",
		OpsReg:  opsReg,
	})
	slog.SetDefault(slog.New(shareddiag.NewMultiHandler(logger.Handler(), diagSink)))

	if tc == nil {
		return
	}

	tc.OnReconnect(func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, evt := range diagReconnectBuf.Drain() {
			_ = tc.PushDiagEvent(drainCtx, evt)
		}
	})

	go func() {
		time.Sleep(600 * time.Millisecond)
		diagCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tc.PushDiagEvent(diagCtx, metricsreg.DiagEvent{
			ThingID:    cpID,
			OccurredAt: time.Now().UTC(),
			EventType:  metricsreg.EventTypeLifecycle,
			Level:      metricsreg.LevelInfo,
			Source:     "control-plane",
			Message:    "control-plane started",
			Attrs:      map[string]any{"version": buildVersion},
		})
	}()
}

// DefaultAdvertiseHost returns the host to use for the metricsUrl advertised
// to Hub. Empty or wildcard values fall back to 127.0.0.1.
func DefaultAdvertiseHost(configured string) string {
	switch configured {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return configured
}
