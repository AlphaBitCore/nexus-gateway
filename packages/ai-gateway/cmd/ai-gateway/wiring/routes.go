// routes.go — HTTP route mounting for ai-gateway.
package wiring

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

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
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store/asyncjob"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/ratelimit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing"
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
	Config         *config.Config
	CacheLayer     *cachelayer.Layer
	DB             *store.DB
	VKAuth         *vkauth.Authenticator
	RateLimiter    *ratelimit.Limiter
	CredManager    *credmanager.Manager
	RouterResolver *routing.Resolver
	Executor       *executor.TargetExecutor
	// Resolver is the shared (providerID, modelID) → CallTarget resolver
	// (the same *provtarget.PgResolver the executor holds). The STT
	// streaming-proxy handler resolves its single upstream target through it.
	Resolver          provtarget.Resolver
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
	// Register the compliance pipeline metric set (hook decisions /
	// durations / fail-opens, storage-redaction outcomes) under the nexus
	// namespace so the pipeline's package-level metrics export on /metrics
	// instead of recording into their isolated no-op defaults.
	pipeline.RegisterDefaultMetrics("nexus")
	// /metrics is gated behind the internal-service token. The
	// ai-gateway HTTP server binds all interfaces on the same port as the
	// public /v1/* data plane, so an unauthenticated /metrics would expose
	// request counts, model/provider ids, and cost/latency histograms to
	// anyone who can reach the port. The compliance-proxy already token-gates
	// its runtime /metrics; this mirrors that posture using the same
	// INTERNAL_SERVICE_TOKEN guard the /internal/* operator routes use.
	metricsGuard := newInternalAuth(deps.Config.Auth.InternalServiceToken)
	mux.HandleFunc("GET /metrics", metricsGuard.require(promhttp.Handler().ServeHTTP))

	trafficReg := traffic.NewAdapterRegistry("nexus")
	adapters.RegisterBuiltins(trafficReg)
	trafficReg.Freeze()

	// The broker registry is ALWAYS constructed so an enabled cache tier always
	// has a fill path (the broker pump is the sole cache writer). The
	// `cache.broker` yaml flag now controls only same-key in-flight DEDUP:
	// true → 1-leader-N-joiners (fewer upstream calls); false → every MISS its
	// own upstream call (low p99) but still cache-filling. A MISS only reaches
	// the broker when a tier is active; cache-off traffic stays on the direct
	// path with zero broker overhead.
	cacheMetrics := streamcache.NewMetrics(prometheus.DefaultRegisterer)
	brokerRegistry := streamcache.NewRegistry(deps.ResponseCache, deps.Logger, cacheMetrics,
		streamcache.WithDedup(deps.Config.Cache.Broker))

	// Rulepack lister for hooks-test endpoint.
	var rulePackLister rulepack.InstallLister
	if deps.DB != nil {
		rulePackLister = rulepack.NewStore(deps.DB.Pool)
	}

	// Async-job correlation store (the ServeVideo* family). Nil without a
	// DB — the video routes then serve 503 VIDEO_STORE_UNAVAILABLE.
	var asyncJobs asyncjob.Store
	if deps.DB != nil {
		asyncJobs = asyncjob.New(deps.DB.Pool)
	}

	handlerDeps := &proxy.Deps{
		Models:                 deps.CacheLayer,
		VKAuth:                 deps.VKAuth,
		RateLimiter:            deps.RateLimiter,
		CredManager:            deps.CredManager,
		Router:                 deps.RouterResolver,
		Executor:               deps.Executor,
		Resolver:               deps.Resolver,
		AsyncJobs:              asyncJobs,
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

	// Internal admin endpoints. These are service-to-service operator surfaces
	// called by the Control Plane BFF, NOT VK data-plane routes — they are
	// gated on the shared internal-service token (env INTERNAL_SERVICE_TOKEN)
	// via guard.require. The /v1/* routes below stay on VK auth. Reuse the
	// same guard instance that gates /metrics above.
	guard := metricsGuard
	mux.HandleFunc("POST /internal/provider-test",
		guard.require(debug.ProviderTestHandler(deps.ProviderReg, deps.Logger)))
	mux.HandleFunc("POST /internal/provider-discover-models",
		guard.require(debug.ProviderDiscoverModelsHandler(deps.ProviderReg, deps.Logger)))
	mux.HandleFunc("POST /internal/routing-simulate",
		guard.require(debug.RoutingSimulateHandler(deps.RouterResolver, deps.FormatBridge, deps.Logger)))
	mux.HandleFunc("POST /internal/v1/credentials/{id}/probe",
		guard.require(debug.CredentialProbeHandler(deps.CacheLayer, deps.ProviderReg, deps.CredManager, deps.Logger)))
	mux.HandleFunc("POST /internal/hooks-test",
		guard.require(debug.HooksTestHandler(deps.GWHookRegistry, rulePackLister, deps.Logger)))
	// Pattern perf test: called by the CP BFF when an author clicks "Test
	// performance" on a rule-pack or hook regex. Measures the pattern on the real
	// Vectorscan engine (the CP has no libhs) so a prefilter-defeating regex is
	// caught at authoring time. INTERNAL_SERVICE_TOKEN-gated like the siblings.
	mux.HandleFunc("POST /internal/pattern-perf-test",
		guard.require(debug.PatternPerfHandler()))
	// Embedding probe: called by CP BFF when admin clicks "Test Embedding" on
	// the Cache Settings page. Embeddings are request/response only (no stream).
	mux.HandleFunc("POST /internal/embedding-probe",
		guard.require(debug.EmbeddingProbeHandler(deps.UpstreamClient, deps.Logger)))
	// FAQ pre-warm: called by CP admin API (POST /api/admin/semantic-cache/prewarm).
	// Delegates embedding + Valkey HSET to the live semantic.Writer. The handler
	// resolves the embedding provider URL + decrypted API key from ConfigCache +
	// CredManager (mirrors proxy_l2.go resolution), so CP never forwards credentials.
	// writer is nil when Redis is unavailable → 503.
	mux.HandleFunc("POST /internal/semantic-prewarm",
		guard.require(debug.SemanticPrewarmHandler(
			deps.Semantic.Writer,
			deps.Semantic.ConfigCache,
			deps.CredManager,
			deps.Logger,
		)))

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
	// Multimodal JSON-body routes (native-shape passthrough): image
	// generation + TTS. Both carry `model` at the body root, so the full
	// ServeProxy pipeline works; the cache is endpoint-skipped. The sibling
	// multipart routes (images/edits|variations, audio/transcriptions|
	// translations) need multipart model extraction and ship with that work.
	mux.HandleFunc("POST /v1/images/generations", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIImages, BodyFormat: provcore.FormatOpenAI,
	}))
	mux.HandleFunc("POST /v1/audio/speech", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIAudioSpeech, BodyFormat: provcore.FormatOpenAI,
	}))
	// STT (speech-to-text): the multipart /v1/audio/transcriptions +
	// /v1/audio/translations routes go through the PARALLEL streaming-proxy
	// handler (ServeSTT), NOT the small-JSON ServeProxy pipeline — their body
	// is a large one-shot binary stream (see e88-s5). Both share one wire
	// shape; ServeSTT preserves the ingress path (transcriptions vs
	// translations) verbatim to the upstream.
	mux.HandleFunc("POST /v1/audio/transcriptions", proxyHandler.ServeSTT(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIAudioTranscriptions, BodyFormat: provcore.FormatOpenAI,
	}))
	mux.HandleFunc("POST /v1/audio/translations", proxyHandler.ServeSTT(proxy.Ingress{
		WireShape: typology.WireShapeOpenAIAudioTranscriptions, BodyFormat: provcore.FormatOpenAI,
	}))
	// Reranking: canonical = Cohere shape (no OpenAI rerank API), so FormatCohere.
	mux.HandleFunc("POST /v1/rerank", proxyHandler.ServeProxy(proxy.Ingress{
		WireShape: typology.WireShapeCohereRerank, BodyFormat: provcore.FormatCohere,
	}))
	// Video generation (async): the parallel ServeVideo* family + the
	// deliberately-unserved sub-route 404 envelopes (extracted to
	// routes_video.go for the file-size ratchet).
	mountVideoRoutes(mux, proxyHandler)
	// Realtime voice relay (WebSocket): parallel handler, routes_realtime.go.
	mountRealtimeRoutes(mux, proxyHandler)

	// Guardrail: the standalone compliance-verdict endpoint runs the SAME hook
	// pipeline the inline path runs over caller-supplied text and returns an
	// allow/block/redact verdict — no upstream relay, so no Ingress wire shape
	// (see e90-s1). Parallel handler (ServeGuardrail), not ServeProxy.
	mux.HandleFunc("POST /v1/guardrail", proxyHandler.ServeGuardrail())
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

	// Model catalog + usage. These authenticated read-only endpoints are
	// DB-backed and were previously unthrottled: a valid VK could
	// hammer /v1/models or /v1/usage* with no per-key cap. Wrap them in the
	// same per-VK RPM limiter the data-plane ServeProxy path enforces so one
	// key cannot drive unbounded catalog/usage queries.
	// Pass interface-typed nils when the concrete deps are nil so the wrapper's
	// nil checks fire correctly (a typed-nil pointer in an interface is not ==
	// nil). When either is absent the wrapper degrades to pass-through.
	var readAuth readVKAuthenticator
	if deps.VKAuth != nil {
		readAuth = deps.VKAuth
	}
	var readLimiter readRateLimiter
	if deps.RateLimiter != nil {
		readLimiter = deps.RateLimiter
	}
	readRL := vkReadRateLimit(readAuth, readLimiter, deps.Logger)
	modelCatalog := selectModelCatalog(deps)
	mux.HandleFunc("GET /v1/models", readRL(models.ModelsHandler(modelCatalog, deps.VKAuth, deps.Logger)))
	mux.HandleFunc("GET /v1/models/{model}", readRL(models.ModelDetailHandler(modelCatalog, deps.VKAuth, deps.Logger)))
	mux.HandleFunc("GET /v1/usage", readRL(envelope.UsageSummaryHandler(deps.DB, deps.VKAuth, deps.QuotaEngine, deps.Logger)))
	mux.HandleFunc("GET /v1/usage/daily", readRL(envelope.UsageDailyHandler(deps.DB, deps.VKAuth, deps.Logger)))

	// Wrap the mounted mux with the ai-gateway middleware chain.
	return applyMiddleware(mux, deps)
}
