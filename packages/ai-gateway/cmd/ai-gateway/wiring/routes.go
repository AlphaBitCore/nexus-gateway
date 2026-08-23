// routes.go — HTTP route mounting for ai-gateway.
package wiring

import (
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	streamcache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/debug"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/models"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/proxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store/asyncjob"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"strings"
)

// RouteDeps carries every subsystem the HTTP route layer needs.
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
		Models:                    deps.CacheLayer,
		VKAuth:                    deps.VKAuth,
		RateLimiter:               deps.RateLimiter,
		CredManager:               deps.CredManager,
		Router:                    deps.RouterResolver,
		Executor:                  deps.Executor,
		Resolver:                  deps.Resolver,
		AsyncJobs:                 asyncJobs,
		HookConfigCache:           deps.HookConfigCache,
		ProviderReg:               deps.ProviderReg,
		HealthTracker:             deps.HealthTracker,
		AuditWriter:               deps.AuditWriter,
		NormalizeRegistry:         deps.NormalizeReg,
		Metrics:                   deps.Metrics,
		QuotaEngine:               deps.QuotaEngine,
		Cache:                     deps.ResponseCache,
		BrokerRegistry:            brokerRegistry,
		CacheMetrics:              cacheMetrics,
		UpstreamClient:            deps.UpstreamClient,
		PayloadCapture:            deps.PayloadCapture,
		StreamingPolicy:           deps.StreamingPolicy,
		StreamCaptureHardCap:      deps.Config.Spill.PerObjectCap(),
		TrafficAdapters:           trafficReg,
		SchemaMismatchRecorder:    deps.Metrics,
		CanonicalBridge:           deps.FormatBridge,
		RoutingDefaultPolicy:      deps.Config.Routing.DefaultRetryPolicy,
		EnforceNamedModelModality: deps.Config.Routing.EnforceNamedModelModality,
		Allowlist:                 deps.Allowlist,
		CachePricing:              deps.CacheLayer,
		Normaliser:                deps.NormEngine,
		GeminiCacheMgrSet:         deps.GeminiCacheMgrSet,
		PassthroughCache:          deps.PassthroughCache,
		LatencyDetail:             deps.Config.Observability.LatencyDetail,
		Logger:                    deps.Logger,
		// L2 semantic cache fields are nil-safe; the proxy skips L2 gracefully
		// when SemanticReader/SemanticWriter are nil.
		FreshnessDetector:   deps.Semantic.Detector,
		CapCache:            deps.CapCache,
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
	// STT (speech-to-text): the multipart /v1/audio/transcriptions route goes
	// through the PARALLEL streaming-proxy handler (ServeSTT), NOT the
	// small-JSON ServeProxy pipeline — its body is a large one-shot binary
	// stream. ServeSTT preserves the ingress path verbatim to the upstream.
	//
	// /v1/audio/translations is deliberately NOT mounted. It was, and it never
	// served anything: only whisper-1 implements the endpoint upstream, and
	// measurement showed every other speech model answering the provider's own
	// "Invalid URL (POST /v1/audio/translations)" 404 — a confusing envelope to
	// hand a caller for a route this deployment has no use for. Unmounted, the
	// path falls to the gateway's own ENDPOINT_NOT_SUPPORTED, which at least
	// says whose refusal it is. The handler still preserves the ingress path,
	// so remounting it is one line if a deployment ever wants it.
	mux.HandleFunc("POST /v1/audio/transcriptions", proxyHandler.ServeSTT(proxy.Ingress{
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

	// Vendor-native wire-shape ingress (Gemini, Azure OpenAI, GLM) — see
	// routes_native.go.
	mountNativeProviderRoutes(mux, proxyHandler)

	// Authenticated read-only endpoints (DB-backed), wrapped in the per-VK RPM
	// limiter so one key can't drive unbounded catalog/usage queries. Interface-
	// typed nils below let the wrapper's nil checks degrade to pass-through.
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
	mux.HandleFunc("GET /v1/usage", readRL(envelope.UsageSummaryHandler(selectUsageStore(deps), deps.VKAuth, deps.QuotaEngine, deps.Logger)))
	mux.HandleFunc("GET /v1/usage/daily", readRL(envelope.UsageDailyHandler(selectUsageStore(deps), deps.VKAuth, deps.Logger)))

	// Public model catalog (no VK): full enabled catalog in the enriched "catalog"
	// shape, limit/offset paginated, 5-minute Redis cache. readRL passes keyless
	// callers through and throttles keyed ones.
	mux.HandleFunc("GET /api/v1/open/models", readRL(models.OpenModelsHandler(selectModelCatalog(deps), deps.Rdb, deps.Logger)))

	// Public single-model detail (no VK): full catalogDetail shape (pricing
	// detail, capability matrix, parameter constraints, family) for one model.
	// The trailing wildcard ({model_id...}, not {model_id}) is load-bearing: the
	// list endpoint emits provider-prefixed ids ("openai/gpt-4o"), which span two
	// path segments and would never bind to a single-segment wildcard.
	mux.HandleFunc("GET /api/v1/open/models/{model_id...}", readRL(models.OpenModelDetailHandler(selectModelCatalog(deps), deps.Logger)))

	// Fallback for any /v1 path this gateway does not serve. Go's ServeMux
	// answers an unmatched pattern with `404 page not found` as text/plain, and
	// both OpenAI SDKs JSON-parse error bodies — so an unmounted endpoint used to
	// surface as an APIStatusError with no message at all. Every registered
	// pattern above is more specific than "/v1/", so this only catches the gaps
	// (/v1/completions, /v1/moderations, /v1/images/edits, …).
	// The same fallback at the root. Registering it only under /v1/ left every
	// other prefix on the text/plain default — /v1beta typos reached the Gemini
	// client as a status with no message, and so did a mistyped Azure-compat
	// deployment path. Go's ServeMux prefers the most specific pattern, so every
	// route registered above still wins over this one.
	notSupported := func(w http.ResponseWriter, r *http.Request) {
		// A catch-all matches everything, which puts ServeMux's own 405/Allow
		// branch out of reach — that branch runs only when NO pattern matched.
		// Without this, every wrong-method request became a 404 whose body
		// claimed the gateway does not serve a path it does serve. Asking the
		// mux which methods would have matched keeps the answer accurate, and
		// upgrades the /v1 prefix too, which had the same hole before the
		// catch-all was widened.
		if allow := servedUnderOtherMethods(mux, r); len(allow) > 0 {
			w.Header().Set("Allow", strings.Join(allow, ", "))
			envelope.WriteGatewayError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
				r.Method+" is not allowed on "+r.URL.Path, "Allowed: "+strings.Join(allow, ", "))
			return
		}
		envelope.WriteEndpointNotSupported(w, r.URL.Path)
	}
	mux.HandleFunc("/v1/", notSupported)
	mux.HandleFunc("/", notSupported)

	// Wrap the mounted mux with the ai-gateway middleware chain.
	return applyMiddleware(mux, deps)
}
