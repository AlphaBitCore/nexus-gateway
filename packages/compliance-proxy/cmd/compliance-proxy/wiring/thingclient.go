package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// ThingClientResult holds the running thingclient and associated state.
type ThingClientResult struct {
	Client          *thingclient.Client
	StaticInfo      registry.StaticInfo
	StaticInfoReady bool
}

// ThingClientDeps bundles dependencies for InitThingClient.
type ThingClientDeps struct {
	Cfg               *config.Config
	ProxyID           string
	BuildVersion      string
	Logger            *slog.Logger
	MQProducer        mq.Producer
	OpsRegistry       *registry.Registry
	ProcessStartTime  time.Time
	ConfigKeyRecorder *runtimeintrospect.KeyStateRecorder
	// OnConfigChanged is the callback invoked when Hub pushes desired config.
	// It is built by buildConfigLoader in configdispatch.go; we accept it as
	// a parameter so thingclient.go stays free of configdispatch imports.
	OnConfigChanged func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error)
}

// InitThingClient creates, configures, and starts the thingclient. Returns
// nil Client when Hub is not configured or startup fails (non-fatal). The
// caller must call tc.Close() on shutdown.
func InitThingClient(ctx context.Context, d ThingClientDeps) ThingClientResult {
	cfg := d.Cfg
	if cfg.Registry.NexusHubURL == "" {
		return ThingClientResult{}
	}

	hubURL := cfg.Registry.NexusHubURL
	opsSampler := platform.NewSampler(d.ProxyID, d.ProcessStartTime, d.OpsRegistry)
	managementBaseURL := ComposeManagementBaseURL(cfg.Metrics.AdvertiseHost, cfg.Metrics.Address)

	tc, err := thingclient.New(thingclient.Config{
		HubURL:     hubURL + "/ws",
		HubHTTPURL: hubURL,
		ThingType:  "compliance-proxy",
		ThingID:    d.ProxyID,
		// PhysicalID mirrors ThingID — yaml `id` already overrides
		// proxyID above when set, so this captures both forms uniformly.
		PhysicalID:        d.ProxyID,
		ThingVersion:      d.BuildVersion,
		ListenAddress:     cfg.Listener.Address,
		MetricsURL:        ComposeMetricsURL(cfg.Metrics.AdvertiseHost, cfg.Metrics.Address),
		ManagementURL:     managementBaseURL,
		Role:              "default",
		Token:             cfg.Auth.InternalServiceToken,
		Logger:            d.Logger,
		MQProducer:        d.MQProducer,
		OpsMetricsSampler: opsSampler,
	})
	if err != nil {
		d.Logger.Warn("thingclient init failed", "error", err)
		return ThingClientResult{}
	}

	// Wire the OnConfigChanged callback (built from configdispatch.go).
	tc.OnConfigChanged(func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
		d.Logger.Info("thing config change received",
			"event", "config_changed",
			"thing_id", d.ProxyID,
			"thing_type", "compliance-proxy",
			"config_keys", len(desired),
		)
		for k, cs := range desired {
			d.ConfigKeyRecorder.Record(k, cs.State)
		}
		reported, applyErr := d.OnConfigChanged(desired)
		d.Logger.Info("config apply finished",
			"event", "config_apply_done",
			"thing_id", d.ProxyID,
			"thing_type", "compliance-proxy",
			"reported_keys", len(reported),
		)
		return reported, applyErr
	})

	if err := tc.Start(ctx); err != nil {
		d.Logger.Warn("thingclient start failed", "error", err)
		return ThingClientResult{}
	}

	d.Logger.Info("registered with Hub as Thing", "thingID", d.ProxyID)

	// Capture L2 static identity and push it now + on every reconnect.
	staticInfo := platform.CaptureStaticInfo(platform.BuildInfo{
		ServiceVersion: "compliance-proxy/0.1.0",
		BuildSHA:       "",
		BuildTime:      "",
		StartTime:      d.ProcessStartTime.Format(time.RFC3339),
		PublicURL:      cfg.PublicURL,
	})
	go func() {
		// Give the WS pump a moment to attach before the first push.
		time.Sleep(500 * time.Millisecond)
		ctxPush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tc.UpdateStaticInfo(ctxPush, staticInfo); err != nil {
			d.Logger.Warn("static_info push failed at startup", "error", err)
		}
	}()

	return ThingClientResult{
		Client:          tc,
		StaticInfo:      staticInfo,
		StaticInfoReady: true,
	}
}

// InitThingClientSimple starts a thingclient using a pre-built OnConfigChanged
// callback (no internal wrapping is added). Returns only the client; the
// caller retrieves ThingClientResult via CaptureThingClientResult.
func InitThingClientSimple(
	ctx context.Context,
	d ThingClientDeps,
) *thingclient.Client {
	res := InitThingClient(ctx, d)
	return res.Client
}

// CaptureThingClientResult builds a ThingClientResult for an already-started
// *thingclient.Client. Used when the client was created via a factory function
// (e.g. initHubAndCfgLoader) and the caller needs static_info pushed.
func CaptureThingClientResult(
	tc *thingclient.Client,
	cfg *config.Config,
	processStartTime time.Time,
	logger *slog.Logger,
) ThingClientResult {
	if tc == nil {
		return ThingClientResult{}
	}
	staticInfo := platform.CaptureStaticInfo(platform.BuildInfo{
		ServiceVersion: "compliance-proxy/0.1.0",
		StartTime:      processStartTime.Format(time.RFC3339),
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
	return ThingClientResult{
		Client:          tc,
		StaticInfo:      staticInfo,
		StaticInfoReady: true,
	}
}
