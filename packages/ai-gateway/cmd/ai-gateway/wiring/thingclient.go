// tc_wiring.go — Thing Client registration, diag reconnect, credential manager,
// MQ producer, AI Guard route mount, and other main-package helpers extracted
// from the original monolithic main.go to keep main.go ≤ 150 LOC.
package wiring

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	geminicache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/gemini"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	creddecrypt "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/decrypt"
	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/proxy/classify"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/rstokenauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// --- Credential Manager ---

func InitCredManager(cfg *config.Config, db *store.DB, cacheLayer *cachelayer.Layer, logger *slog.Logger) (*credmanager.Manager, error) {
	if keyMap := cfg.Auth.CredentialKeyMap; keyMap != "" {
		md, err := creddecrypt.NewMultiDecryptor(keyMap)
		if err != nil {
			return nil, err
		}
		slog.Info("credential decryptor initialized (multi-key)")
		return credmanager.NewMultiKeyManager(cacheLayer, md), nil
	}
	var decryptor *creddecrypt.Decryptor
	if key := cfg.Auth.CredentialMasterKey; key != "" {
		var err error
		decryptor, err = creddecrypt.NewDecryptor(key)
		if err != nil {
			return nil, err
		}
	}
	return credmanager.NewManager(cacheLayer, decryptor), nil
}

// --- MQ Producer ---

func InitMQProducer(cfg *config.Config, logger *slog.Logger) (mq.Producer, error) {
	if cfg.MQ.Driver == "" {
		return nil, nil
	}
	mqCfg := mq.Config{
		Driver:    cfg.MQ.Driver,
		Namespace: "nexus",
		NATS:      mq.NATSConfig{URL: cfg.MQ.NATS.URL},
	}
	p, err := mq.NewProducer(mqCfg, logger)
	if err != nil {
		return nil, err
	}
	slog.Info("MQ producer initialized", "driver", cfg.MQ.Driver)
	return p, nil
}

// --- Thing Client ---

type TCInitDeps struct {
	Cfg             *config.Config
	DB              *store.DB
	CacheLayer      *cachelayer.Layer
	CredManager     *credmanager.Manager
	GeminiMgrSet    *geminicache.ManagerSet
	HookConfigCache interface {
		Reload(ctx context.Context) error
	}
	Tp                *telemetry.SwappableTracerProvider
	ObsState          *atomic.Pointer[telemetry.Config]
	PayloadCapture    *payloadcapture.Store
	StreamingPolicy   *streampolicy.Store
	Reliability       ReliabilityReloader
	PolicyCache       *quota.PolicyCache
	AiguardGetter     func() *aiguard.ConfigCache
	NormEngine        *wirerewrite.Engine
	PassthroughCache  *passthrough.Cache
	AuditWriter       *audit.Writer
	ConfigKeyRecorder *runtimeintrospect.KeyStateRecorder
	OpsReg            *registry.Registry
	ProcessStartTime  time.Time
	MqProducer        mq.Producer
	Logger            *slog.Logger
	BuildVersion      string
	// CfgLoaderBuilder constructs the config loader once the thingclient agID and
	// OutcomeTracker are known. Built by main.go via configdispatch.BuildConfigLoader
	// so that wiring stays free of configdispatch imports (circular: configdispatch
	// imports wiring for helper functions like InitOtelConfig).
	CfgLoaderBuilder func(agID string, outcomes *thingclient.OutcomeTracker) *cfgloader.Loader
}

type TCInitResult struct {
	AgID              string
	Client            *thingclient.Client
	StaticInfo        registry.StaticInfo
	StaticInfoSet     bool
	ReconnectComposed bool
}

func InitThingClient(ctx context.Context, d TCInitDeps) TCInitResult {
	if d.Cfg.Registry.NexusHubURL == "" {
		return TCInitResult{}
	}

	hubURL := d.Cfg.Registry.NexusHubURL
	hostname, _ := os.Hostname()
	agID := d.Cfg.ID
	if agID == "" {
		agID = fmt.Sprintf("gw-%s-%d", hostname, d.Cfg.Server.Port)
	}
	if d.AuditWriter != nil {
		d.AuditWriter.WithThingIdentity(agID, hostname)
	}

	listenAddr := fmt.Sprintf(":%d", d.Cfg.Server.Port)
	advertiseHost := DefaultAdvertiseHost(d.Cfg.Server.AdvertiseHost)
	metricsURL := fmt.Sprintf("http://%s:%d/metrics", advertiseHost, d.Cfg.Server.Port)
	opsSampler := platform.NewSampler(agID, d.ProcessStartTime, d.OpsReg)

	tc, err := thingclient.New(thingclient.Config{
		HubURL: hubURL + "/ws", HubHTTPURL: hubURL,
		ThingType: "ai-gateway", ThingID: agID, PhysicalID: agID,
		ThingVersion: d.BuildVersion, ListenAddress: listenAddr,
		MetricsURL: metricsURL, Role: "default",
		Token: d.Cfg.Auth.InternalServiceToken, Logger: slog.Default(),
		MQProducer: d.MqProducer, OpsMetricsSampler: opsSampler,
	})
	if err != nil {
		slog.Warn("thingclient init failed", "error", err)
		return TCInitResult{AgID: agID}
	}

	var cfgLoader *cfgloader.Loader
	if d.CfgLoaderBuilder != nil {
		cfgLoader = d.CfgLoaderBuilder(agID, tc.Outcomes())
	}
	tc.OnConfigChanged(func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
		slog.Info("thing config change received",
			"event", "config_changed", "thing_id", agID, "thing_type", "ai-gateway",
			"config_keys", len(desired),
		)
		for k, cs := range desired {
			d.ConfigKeyRecorder.Record(k, cs.State)
		}
		if cfgLoader == nil {
			return desired, nil
		}
		reported, applyErr := cfgLoader.Apply(context.Background(), desired)
		for k, cs := range desired {
			if !cfgLoader.Has(k) {
				reported[k] = cs
			}
		}
		slog.Info("config apply finished",
			"event", "config_apply_done", "thing_id", agID, "thing_type", "ai-gateway",
			"reported_keys", len(reported),
		)
		return reported, applyErr
	})

	if err := tc.Start(ctx); err != nil {
		slog.Warn("thingclient start failed", "error", err)
		return TCInitResult{AgID: agID}
	}
	slog.Info("registered with Hub as Thing", "thingID", agID)

	staticInfo := platform.CaptureStaticInfo(platform.BuildInfo{
		ServiceVersion: "ai-gateway/0.1.0",
		StartTime:      d.ProcessStartTime.Format(time.RFC3339),
		PublicURL:      d.Cfg.PublicURL,
	})
	go func() {
		time.Sleep(500 * time.Millisecond)
		ctxP, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tc.UpdateStaticInfo(ctxP, staticInfo); err != nil {
			slog.Warn("static_info push failed at startup", "error", err)
		}
	}()
	return TCInitResult{AgID: agID, Client: tc, StaticInfo: staticInfo, StaticInfoSet: true}
}

// wireDiagReconnect installs the OnReconnect callback to drain buffered diag
// events and emits a lifecycle start event.
func WireDiagReconnect(
	tc *thingclient.Client,
	buf *shareddiag.ReconnectBuffer,
	reconnectComposed bool,
	agID, buildVersion string,
) {
	if tc == nil {
		return
	}
	if !reconnectComposed {
		tcForDrain := tc
		tc.OnReconnect(func() {
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, evt := range buf.Drain() {
				_ = tcForDrain.PushDiagEvent(drainCtx, evt)
			}
		})
	}
	go func() {
		time.Sleep(600 * time.Millisecond)
		diagCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tc.PushDiagEvent(diagCtx, registry.DiagEvent{
			ThingID:    agID,
			OccurredAt: time.Now().UTC(),
			EventType:  registry.EventTypeLifecycle,
			Level:      registry.LevelInfo,
			Source:     "ai-gateway",
			Message:    "ai-gateway started",
			Attrs:      map[string]any{"version": buildVersion},
		})
	}()
}

// wireStaticInfoReconnect installs a reconnect callback to re-push static_info.
func WireStaticInfoReconnect(tc *thingclient.Client, res TCInitResult) {
	if tc == nil || !res.StaticInfoSet || res.ReconnectComposed {
		return
	}
	tcLocal, siLocal := tc, res.StaticInfo
	tc.OnReconnect(func() {
		ctxP, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tcLocal.UpdateStaticInfo(ctxP, siLocal); err != nil {
			slog.Warn("static_info push failed on reconnect", "error", err)
		}
	})
}

// defaultAdvertiseHost returns the host to advertise externally.
// Unspecified or wildcard bind addresses are replaced with loopback.
func DefaultAdvertiseHost(configured string) string {
	switch configured {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return configured
}

// runTicker calls fn on interval until ctx is done.
func RunTicker(ctx context.Context, interval time.Duration, fn func()) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			fn()
		case <-ctx.Done():
			return nil
		}
	}
}

// initDiagSink builds a diag sink attached to the thingclient and wires reconnect
// callbacks for both diag draining and static info re-push. Returns the updated
// logger with the diag handler attached.
func InitDiagSink(
	ctx context.Context,
	tcClient *thingclient.Client,
	tcRes TCInitResult,
	agID, buildVersion string,
	baseLogger *slog.Logger,
	opsReg *registry.Registry,
) *slog.Logger {
	diagReconnectBuf := shareddiag.NewReconnectBuffer(shareddiag.ReconnectBufferConfig{})
	diagSink := shareddiag.NewSlogSink(shareddiag.SlogSinkConfig{
		ThingClient: tcClient, ReconnectBuffer: diagReconnectBuf,
		IsWSConnected: func() bool {
			return tcClient != nil && tcClient.Mode() == thingclient.ModeWSConnected
		},
		ThingID: agID, Source: "ai-gateway",
		OpsReg: opsReg,
	})
	combined := slog.New(shareddiag.NewMultiHandler(baseLogger.Handler(), diagSink))
	slog.SetDefault(combined)
	WireDiagReconnect(tcClient, diagReconnectBuf, tcRes.ReconnectComposed, agID, buildVersion)
	WireStaticInfoReconnect(tcClient, tcRes)
	_ = ctx // kept for symmetry; context used indirectly via tcClient lifecycle
	return combined
}

// mountRoutes wires all mux routes (runtime API, introspect, AI Guard, core) and
// returns the fully middleware-wrapped http.Handler.
func MountRoutes(
	mux *http.ServeMux,
	tcClient *thingclient.Client,
	d *BootDeps,
	agID, buildVersion string,
	cfg *config.Config,
	logger *slog.Logger,
) http.Handler {
	MountRuntimeAPI(tcClient, cfg.Auth.InternalServiceToken, mux)
	InitIntrospectRegistry(IntrospectDeps{
		AgID: agID, BuildVersion: buildVersion, CacheLayer: d.CacheLayer,
		PolicyCache:         d.PolicyCache,
		PayloadCaptureStore: d.PayloadCapture,
		HookConfigCache:     d.HookConfigCache,
		AIGuardConfigCache:  func() *aiguard.ConfigCache { return d.AiguardConfigCache },
		ObservabilityGet:    func() *telemetry.Config { return d.ObsState.Load() },
		ConfigKeyRecorder:   d.ConfigKeyRecorder, AuthToken: cfg.Auth.InternalServiceToken,
	}, mux)
	if d.AiguardConfigCache != nil {
		MountAIGuardRoutes(mux, cfg, d.Rdb, d.AdapterReg, d.PtResolver,
			d.CacheLayer, d.AuditWriter, d.AiguardConfigCache, logger)
	}
	return MountCoreRoutes(mux, RouteDeps{
		Config: cfg, CacheLayer: d.CacheLayer, DB: d.DB, VKAuth: d.VkAuth,
		RateLimiter: d.RateLimiter, CredManager: d.CredManager,
		RouterResolver: d.RouterResolver, Executor: d.TargetExecutor,
		HookConfigCache: d.HookConfigCache, GWHookRegistry: d.GwHookRegistry,
		ProviderReg: d.AdapterReg, HealthTracker: d.HealthTracker,
		AuditWriter: d.AuditWriter, NormalizeReg: d.NormalizeReg,
		Metrics:     d.MetricsRecorder,
		QuotaEngine: d.QuotaEngine, ResponseCache: d.ResponseCache,
		UpstreamClient:  d.UpstreamClient,
		PayloadCapture:  d.PayloadCapture,
		StreamingPolicy: d.StreamingPolicy,
		FormatBridge:    d.FormatBridge, Allowlist: d.Allowlist,
		NormEngine: d.NormEngine, GeminiCacheMgrSet: d.GeminiMgrSet,
		PassthroughCache: d.PassthroughCache, Logger: logger,
		Semantic: d.Semantic,
	})
}

// --- AI Guard route mounting ---

// mountAIGuardRoutes wires /v1/ai-guard/classify and /v1/ai-guard/compliance-webhook.
func MountAIGuardRoutes(
	mux *http.ServeMux,
	cfg *config.Config,
	rdb redis.UniversalClient,
	adapterReg *provcore.Registry,
	ptResolver *provtarget.PgResolver,
	cacheLayer *cachelayer.Layer,
	auditWriter *audit.Writer,
	configCache *aiguard.ConfigCache,
	logger *slog.Logger,
) {
	aiguardClient := &LiveClassifier{
		Cache:       aiguard.NewCache(rdb),
		Sink:        &WriterBackedTrafficSink{Writer: auditWriter},
		ConfigCache: configCache,
		Adapters:    adapterReg,
		Resolver:    ptResolver,
		DB:          cacheLayer,
		ExtHTTPClient: nexushttp.New(nexushttp.Config{
			Timeout: time.Duration(cfg.HTTPClients.External.TimeoutSec) * time.Second,
			Caller:  "aiguard-external",
		}),
		Logger: logger,
	}
	serviceToken := cfg.Auth.InternalServiceToken
	if serviceToken == "" {
		logger.Warn("internal service token unset; /v1/ai-guard/* endpoints will reject all calls")
	}
	classifyHandler := classify.NewClassifyHandler(aiguardClient)
	// Both AI Guard endpoints reach a billable judge-model call on caller-supplied
	// content, so both are gated by the shared internal-service token (empty token →
	// 503, missing/wrong → 401). Never mount either on the public listener unguarded.
	gate := rstokenauth.MiddlewareHTTP(serviceToken)
	mux.Handle(
		"POST /v1/ai-guard/classify",
		gate(http.HandlerFunc(classifyHandler.ServeClassifyHTTP)),
	)
	mux.Handle(
		"POST /v1/ai-guard/compliance-webhook",
		gate(http.HandlerFunc(classifyHandler.ServeComplianceWebhookHTTP)),
	)
}
