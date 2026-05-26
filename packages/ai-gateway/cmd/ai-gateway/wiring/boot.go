// boot.go — assembles every ai-gateway subsystem from config and returns a
// fully wired BootDeps ready for run() to start servers.
package wiring

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	geminicache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/gemini"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/proxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	epMetrics "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/ratelimit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	credstats "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/stats"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
	"github.com/redis/go-redis/v9"
)

// BootDeps holds every wired subsystem produced by boot().
type BootDeps struct {
	Cfg              *config.Config
	DB               *store.DB
	Rdb              redis.UniversalClient
	CacheLayer       *cachelayer.Layer
	CredManager      *credmanager.Manager
	VkAuth           *vkauth.Authenticator
	RateLimiter      *ratelimit.Limiter
	GwHookRegistry   *hookcore.HookRegistry
	HookConfigCache  *pipeline.HookConfigCache
	HealthTracker    *store.HealthTracker
	PtResolver       *provtarget.PgResolver
	RouterResolver   *routing.Resolver
	CapCache         *capability.Cache
	AdapterReg       *provcore.Registry
	FormatBridge     *canonicalbridge.Bridge
	TargetExecutor   *executor.TargetExecutor
	QuotaEngine      *quota.Engine
	PolicyCache      *quota.PolicyCache
	MqProducer       mq.Producer
	PayloadCapture   *payloadcapture.Store
	// StreamingPolicy is the hot-swappable streaming compliance policy
	// Store (#115). proxy_cache.go reads Store.Get() per-request to
	// dispatch SSE handler between live (chunked_async) and buffer
	// (buffer_full_block) modes. Hot-reloaded via configdispatch's
	// streaming_compliance shadow handler.
	StreamingPolicy  *streampolicy.Store
	AuditWriter      *audit.Writer
	NormalizeReg     *normcore.Registry // shared with proxy.Deps so L2 semantic cache sees a populated rctxFull.Normalized()
	MetricsRecorder  *epMetrics.Recorder
	ResponseCache    *cache.Cache
	NormEngine       *wirerewrite.Engine
	PassthroughCache *passthrough.Cache
	GeminiMgrSet     *geminicache.ManagerSet
	Allowlist        *forwardheader.Resolved
	AiguardConfigCache *aiguard.ConfigCache
	UpstreamClient   *http.Client
	Tp               *telemetry.SwappableTracerProvider
	ObsState         atomic.Pointer[telemetry.Config]
	Reliability      *ReliabilityConfig
	ConfigKeyRecorder *runtimeintrospect.KeyStateRecorder
	OpsReg           *registry.Registry
	ProcessStartTime time.Time
	// Semantic holds the L2 semantic cache subsystem.
	// All fields are nil when L2 is disabled (Redis not *redis.Client or config off).
	Semantic SemanticDeps
}

// boot assembles all subsystems. Returns non-nil error on fatal init failure.
// Callers must call cleanup() if boot() returns nil error.
func Boot(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*BootDeps, func(), error) {
	d := &BootDeps{
		Cfg:               cfg,
		ProcessStartTime:  time.Now().UTC(),
		ConfigKeyRecorder: runtimeintrospect.NewKeyStateRecorder(),
	}
	d.OpsReg = registry.NewRegistry(prometheus.DefaultRegisterer)

	// Upstream HTTP client.
	specutil.Configure(specutil.HTTPConfig{
		Timeout:             time.Duration(cfg.Upstream.TimeoutSec) * time.Second,
		DialTimeout:         time.Duration(cfg.Upstream.DialTimeoutSec) * time.Second,
		KeepAlive:           time.Duration(cfg.Upstream.KeepAliveSec) * time.Second,
		TLSHandshakeTimeout: time.Duration(cfg.Upstream.TLSHandshakeTimeoutSec) * time.Second,
		IdleConnTimeout:     time.Duration(cfg.Upstream.IdleConnTimeoutSec) * time.Second,
		MaxIdleConns:        cfg.Upstream.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.Upstream.MaxIdleConnsPerHost,
	})
	d.UpstreamClient = proxy.NewUpstreamClient()

	// Forward-header allowlist.
	fhCfg := cfg.ForwardHeaders
	if fhCfg == nil {
		def := forwardheader.DefaultConfig()
		fhCfg = &def
	}
	var err error
	d.Allowlist, err = InitForwardHeaderAllowlist(*fhCfg)
	if err != nil {
		return nil, nil, err
	}
	logger.Info("forward-header allowlist resolved", "version", d.Allowlist.Hash())

	d.AdapterReg = InitProviderRegistry(d.Allowlist, logger)

	d.DB, err = InitDB(ctx, cfg.Database.URL)
	if err != nil {
		return nil, nil, err
	}
	d.Rdb = InitRedis(ctx, cfg)

	// OTel.
	otelCfg := InitOtelConfig(ctx, d.DB, cfg)
	d.ObsState.Store(&otelCfg)
	d.Tp, err = telemetry.Init(ctx, otelCfg, logger)
	if err != nil {
		logger.Warn("OpenTelemetry init failed", "error", err)
		d.Tp = nil
	}

	d.CacheLayer, err = InitCacheLayer(ctx, d.DB, logger, d.OpsReg)
	if err != nil {
		return nil, nil, err
	}
	d.CredManager, err = InitCredManager(cfg, d.DB, d.CacheLayer, logger)
	if err != nil {
		return nil, nil, err
	}
	d.VkAuth, err = InitVKAuth(d.CacheLayer, cfg.Auth.HMACSecret, logger)
	if err != nil {
		return nil, nil, err
	}
	d.RateLimiter = InitRateLimiter(d.Rdb, logger)
	d.GwHookRegistry, err = InitHookRegistry(cfg.HTTPClients.Webhook)
	if err != nil {
		return nil, nil, err
	}
	d.HookConfigCache = InitHookConfigCache(d.DB, d.GwHookRegistry, logger)
	d.HealthTracker = store.NewHealthTracker()
	d.PtResolver = NewResolver(d.CacheLayer, d.CredManager, d.Rdb)
	_, _, d.RouterResolver, d.CapCache = InitRouter(d.CacheLayer, d.HealthTracker, d.PtResolver, d.AdapterReg, logger)

	// Seed the capability cache from the models already loaded by
	// InitCacheLayer.Start above. Subsequent reloads are handled by the
	// configdispatch OnModelsReloaded callback.
	if d.CacheLayer != nil && d.CapCache != nil {
		d.CapCache.Replace(capability.NewSnapshot(d.CacheLayer.AllModels()))
	}

	d.Reliability = NewReliabilityConfig(d.CacheLayer, logger)
	if d.DB != nil {
		if err := d.Reliability.Reload(ctx, d.DB); err != nil {
			logger.Warn("reliability config initial load failed; using defaults", "error", err)
		}
	}

	credStatsMetrics := credstats.NewMetrics(prometheus.DefaultRegisterer)
	credStatsBuf := credstats.New(d.Rdb, logger, d.Reliability.Resolve, credStatsMetrics)
	d.FormatBridge, d.TargetExecutor = InitExecutor(d.AdapterReg, d.PtResolver, d.HealthTracker, credStatsBuf, logger)
	d.QuotaEngine, d.PolicyCache = InitQuota(ctx, d.DB, d.Rdb, logger)

	d.MqProducer, err = InitMQProducer(cfg, logger)
	if err != nil {
		return nil, nil, err
	}
	d.PayloadCapture = InitPayloadCaptureStore(ctx, d.DB)
	d.StreamingPolicy = InitStreamingPolicyStore(ctx, d.DB)
	d.AuditWriter, d.NormalizeReg, err = InitAuditWriter(d.MqProducer, cfg.Spill, d.PayloadCapture, d.OpsReg, logger)
	if err != nil {
		return nil, nil, err
	}
	d.MetricsRecorder = InitMetricsRecorder(d.OpsReg)
	d.ResponseCache = cache.New(d.Rdb, cache.Config{
		Enabled: cfg.Cache.Enabled, TTL: cfg.Cache.TTL, Prefix: cfg.Cache.Prefix,
	}, logger)
	d.NormEngine = InitNormEngine(logger)
	d.PassthroughCache = passthrough.NewCache()
	d.GeminiMgrSet = InitGeminiCacheMgrSet(d.Rdb, d.CacheLayer, d.CredManager, logger)
	d.Semantic = InitSemantic(d.Rdb, d.UpstreamClient, "nexus", logger)

	cleanup := func() {
		if d.DB != nil {
			d.DB.Close()
		}
		if d.Rdb != nil {
			_ = d.Rdb.Close()
		}
		if d.MqProducer != nil {
			_ = d.MqProducer.Close()
		}
		if d.Tp != nil {
			_ = d.Tp.Shutdown(context.Background())
		}
		d.AuditWriter.Close()
	}
	return d, cleanup, nil
}
