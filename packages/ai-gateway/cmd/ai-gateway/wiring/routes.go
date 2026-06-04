// routes.go — HTTP route mounting for ai-gateway.
package wiring

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	geminicache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/gemini"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	streamcache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/debug"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/models"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/proxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	epMetrics "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/ratelimit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// RouteDeps carries every subsystem the HTTP route layer needs.
type RouteDeps struct {
	Config            *config.Config
	CacheLayer        *cachelayer.Layer
	DB                *store.DB
	VKAuth            *vkauth.Authenticator
	RateLimiter       *ratelimit.Limiter
	CredManager       *credmanager.Manager
	RouterResolver    *routing.Resolver
	Executor          *executor.TargetExecutor
	HookConfigCache   *pipeline.HookConfigCache
	GWHookRegistry    *hookcore.HookRegistry
	ProviderReg       *provcore.Registry
	HealthTracker     *store.HealthTracker
	AuditWriter       *audit.Writer
	NormalizeReg      *normcore.Registry
	Metrics           *epMetrics.Recorder
	QuotaEngine       *quota.Engine
	ResponseCache     *cache.Cache
	UpstreamClient    *http.Client
	PayloadCapture    *payloadcapture.Store
	StreamingPolicy   *streampolicy.Store
	FormatBridge      *canonicalbridge.Bridge
	Allowlist         *forwardheader.Resolved
	NormEngine        *wirerewrite.Engine
	GeminiCacheMgrSet *geminicache.ManagerSet
	PassthroughCache  *passthrough.Cache
	Logger            *slog.Logger
	// Semantic holds the L2 semantic cache subsystem (all fields nil-safe → degraded mode when absent).
	Semantic SemanticDeps
}

// MountCoreRoutes registers health, metrics, internal admin, and all /v1/* API
// routes. Returns the fully wrapped handler with middleware applied.
// AI Guard classify routes are mounted separately via MountAIGuardRoutes.
func MountCoreRoutes(mux *http.ServeMux, deps RouteDeps) http.Handler {
	// Health + metrics.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","service":"ai-gateway"}`)
	})
	hookcore.RegisterRegexCacheMetrics(prometheus.DefaultRegisterer)
	mux.Handle("GET /metrics", promhttp.Handler())

	trafficReg := traffic.NewAdapterRegistry("nexus")
	adapters.RegisterBuiltins(trafficReg)
	trafficReg.Freeze()

	// `cache.broker` yaml flag gates same-key in-flight dedupe. When false,
	// BrokerRegistry stays nil and every MISS goes direct to upstream
	// (no joiner wait, no cache fill).
	cacheMetrics := streamcache.NewMetrics(prometheus.DefaultRegisterer)
	var brokerRegistry *streamcache.Registry
	if deps.Config.Cache.Broker {
		brokerRegistry = streamcache.NewRegistry(deps.ResponseCache, deps.Logger, cacheMetrics)
	}

	// Rulepack lister for hooks-test endpoint.
	var rulePackLister rulepack.InstallLister
	if deps.DB != nil {
		rulePackLister = rulepack.NewStore(deps.DB.Pool)
	}

	handlerDeps := &proxy.Deps{
		Models:                 deps.CacheLayer,
		VKAuth:                 deps.VKAuth,
		RateLimiter:            deps.RateLimiter,
		CredManager:            deps.CredManager,
		Router:                 deps.RouterResolver,
		Executor:               deps.Executor,
		HookConfigCache:        deps.HookConfigCache,
		ProviderReg:            deps.ProviderReg,
		HealthTracker:          deps.HealthTracker,
		AuditWriter:            deps.AuditWriter,
		NormalizeRegistry:      deps.NormalizeReg,
		Metrics:                deps.Metrics,
		QuotaEngine:            deps.QuotaEngine,
		Cache:                  deps.ResponseCache,
		BrokerRegistry:         brokerRegistry,
		CacheMetrics:           cacheMetrics,
		UpstreamClient:         deps.UpstreamClient,
		PayloadCapture:         deps.PayloadCapture,
		StreamingPolicy:        deps.StreamingPolicy,
		StreamCaptureHardCap:   deps.Config.Spill.PerObjectCap(),
		TrafficAdapters:        trafficReg,
		SchemaMismatchRecorder: deps.Metrics,
		CanonicalBridge:        deps.FormatBridge,
		RoutingDefaultPolicy:   deps.Config.Routing.DefaultRetryPolicy,
		Allowlist:              deps.Allowlist,
		CachePricing:           deps.CacheLayer,
		Normaliser:             deps.NormEngine,
		GeminiCacheMgrSet:      deps.GeminiCacheMgrSet,
		PassthroughCache:       deps.PassthroughCache,
		LatencyDetail:          deps.Config.Observability.LatencyDetail,
		Logger:                 deps.Logger,
		// L2 semantic cache fields are nil-safe; the proxy skips L2 gracefully
		// when SemanticReader/SemanticWriter are nil.
		FreshnessDetector:   deps.Semantic.Detector,
		SemanticReader:      deps.Semantic.Reader,
		SemanticWriter:      deps.Semantic.Writer,
		SemanticConfigCache: deps.Semantic.ConfigCache,
	}

	// Wire metric emitters.
	executor.SetMetricsRecorder(deps.Metrics.RecordRouterRetry)
	provdispatch.SetForwardHeaderDropFn(deps.Metrics.RecordForwardHeaderDropped)
	provdispatch.SetReasoningPassthroughFn(deps.Metrics.RecordReasoningPassthrough)

	proxyHandler := proxy.NewHandler(handlerDeps)

	// Internal admin endpoints.
	mux.HandleFunc("POST /internal/provider-test",
		debug.ProviderTestHandler(deps.ProviderReg, deps.Logger))
	mux.HandleFunc("POST /internal/routing-simulate",
		debug.RoutingSimulateHandler(deps.RouterResolver, deps.FormatBridge, deps.Logger))
	mux.HandleFunc("POST /internal/v1/credentials/{id}/probe",
		debug.CredentialProbeHandler(deps.CacheLayer, deps.ProviderReg, deps.CredManager, deps.Logger))
	mux.HandleFunc("POST /internal/hooks-test",
		debug.HooksTestHandler(deps.GWHookRegistry, rulePackLister, deps.Logger))
	// Embedding probe: called by CP BFF when admin clicks "Test Embedding" on
	// the Cache Settings page. Embeddings are request/response only (no stream).
	mux.HandleFunc("POST /internal/embedding-probe",
		debug.EmbeddingProbeHandler(deps.UpstreamClient, deps.Logger))
	// FAQ pre-warm: called by CP admin API (POST /api/admin/semantic-cache/prewarm).
	// Delegates embedding + Valkey HSET to the live semantic.Writer. The handler
	// resolves the embedding provider URL + decrypted API key from ConfigCache +
	// CredManager (mirrors proxy_l2.go resolution), so CP never forwards credentials.
	// writer is nil when Redis is unavailable → 503.
	mux.HandleFunc("POST /internal/semantic-prewarm",
		debug.SemanticPrewarmHandler(
			deps.Semantic.Writer,
			deps.Semantic.ConfigCache,
			deps.CredManager,
			deps.Logger,
		))

	// V1 API routes.
	mux.HandleFunc("POST /v1/chat/completions", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI,
	}))
	mux.HandleFunc("POST /v1/embeddings", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIEmbeddings, BodyFormat: provcore.FormatOpenAI,
	}))
	mux.HandleFunc("POST /v1/responses", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIResponses, BodyFormat: provcore.FormatOpenAIResponses,
	}))
	mux.HandleFunc("POST /v1/messages", proxyHandler.ServeProxy(proxy.Ingress{
		// /v1/messages serves Anthropic Messages wire shape.
		// EndpointKind = chat (derived via typology.KindFromWireShape).
		WireShape: typology.WireShapeAnthropicMessages, BodyFormat: provcore.FormatAnthropic,
	}))
	mux.HandleFunc("POST /v1/estimate", proxyHandler.ServeEstimate)

	// Gemini native ingress.
	mux.HandleFunc("POST /v1beta/models/{model}", func(w http.ResponseWriter, r *http.Request) {
		full := r.PathValue("model")
		switch {
		case strings.HasSuffix(full, ":streamGenerateContent"):
			proxyHandler.ServeProxy(proxy.Ingress{
				WireShape:      typology.WireShapeGeminiGenerateContent,
				BodyFormat:     provcore.FormatGemini,
				Stream:         true,
				StreamFromPath: true,
			})(w, r)
		case strings.HasSuffix(full, ":generateContent"):
			proxyHandler.ServeProxy(proxy.Ingress{
				WireShape:  typology.WireShapeGeminiGenerateContent,
				BodyFormat: provcore.FormatGemini,
			})(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	// Azure OpenAI native ingress.
	mux.HandleFunc("POST /openai/deployments/{deployment}/chat/completions", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatAzureOpenAI,
	}))
	mux.HandleFunc("POST /openai/deployments/{deployment}/embeddings", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIEmbeddings, BodyFormat: provcore.FormatAzureOpenAI,
	}))

	// GLM (ZhipuAI) native ingress.
	mux.HandleFunc("POST /api/paas/v4/chat/completions", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatGLM,
	}))
	mux.HandleFunc("POST /api/paas/v4/embeddings", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIEmbeddings, BodyFormat: provcore.FormatGLM,
	}))

	// Model catalog + usage.
	mux.HandleFunc("GET /v1/models", models.ModelsHandler(deps.DB, deps.VKAuth, deps.Logger))
	mux.HandleFunc("GET /v1/models/{model}", models.ModelDetailHandler(deps.DB, deps.VKAuth, deps.Logger))
	mux.HandleFunc("GET /v1/usage", envelope.UsageSummaryHandler(deps.DB, deps.VKAuth, deps.QuotaEngine, deps.Logger))
	mux.HandleFunc("GET /v1/usage/daily", envelope.UsageDailyHandler(deps.DB, deps.VKAuth, deps.Logger))

	// Middleware chain.
	var h http.Handler = mux
	h = middleware.ConnectionStage(
		func(ctx context.Context) (*pipeline.PolicyResolver, error) {
			return deps.HookConfigCache.Resolver(ctx), nil
		},
		5*time.Second, 30*time.Second, "AI_GATEWAY", deps.Logger,
	)(h)
	h = middleware.Logger(deps.Logger)(h)
	h = middleware.Recovery(deps.Logger)(h)
	if deps.Config.CORS.Enabled {
		allowedMethods := deps.Config.CORS.AllowedMethods
		if len(allowedMethods) == 0 {
			allowedMethods = []string{"GET", "POST", "OPTIONS"}
		}
		allowedHeaders := deps.Config.CORS.AllowedHeaders
		if len(allowedHeaders) == 0 {
			allowedHeaders = []string{
				"Content-Type", "Authorization", "x-nexus-virtual-key",
				"x-request-id", "x-nexus-aigw-body-format", "x-nexus-aigw-no-cache",
			}
		}
		h = middleware.CORS(middleware.CORSConfig{
			AllowedOrigins: deps.Config.CORS.AllowedOrigins,
			AllowedMethods: allowedMethods,
			AllowedHeaders: allowedHeaders,
			ExposeHeaders:  traffic.ExposeHeaders,
			MaxAge:         deps.Config.CORS.MaxAgeSec,
		})(h)
		slog.Info("CORS enabled", "origins", deps.Config.CORS.AllowedOrigins)
	}
	h = telemetry.HTTPTrace("nexus-ai-gateway")(h)
	// RequestID wraps outside HTTPTrace so the X-Nexus-Request-Id is on the
	// request context before the server span is created: the tracer's
	// IDGenerator derives the span's trace id from it, keeping the OTel trace
	// id and the audit trace_id one and the same value.
	h = middleware.RequestID(h)
	return h
}
