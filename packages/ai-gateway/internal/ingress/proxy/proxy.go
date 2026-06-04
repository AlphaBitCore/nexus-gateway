// Package handler implements the HTTP route handlers for the AI gateway.
// The proxy handler orchestrates the full pipeline: VK auth → rate limit →
// routing → request hooks → credential lookup → upstream fetch → response
// hooks (or live streaming compliance) → audit → response.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/freshness"
	geminicache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/gemini"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	streamcache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// NewUpstreamClient returns an http.Client configured for upstream provider
// requests. Uses a shared transport with connection pooling and timeouts.
// Tunables come from the live [specutil.ActiveConfig] snapshot so the
// legacy proxy path and the spec-adapter path share one set of values.
func NewUpstreamClient() *http.Client {
	cfg := specutil.ActiveConfig()
	return nexushttp.New(nexushttp.Config{
		Timeout:             cfg.Timeout + 5*time.Second,
		DialTimeout:         cfg.DialTimeout,
		KeepAlive:           cfg.KeepAlive,
		MaxIdleConns:        cfg.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.IdleConnTimeout,
		TLSHandshakeTimeout: cfg.TLSHandshakeTimeout,
		Caller:              "ai-gateway-upstream",
	})
}

// Deps holds all injected dependencies for the proxy handler.
type Deps struct {
	Models          ModelLookup               // was accessed via DB.GetModel etc.
	VKAuth          VKAuthenticator           // was *vkauth.Authenticator
	RateLimiter     RateLimiter               // was *ratelimit.Limiter
	CredManager     CredentialLookup          // was *credentials.Manager — used by models handler
	Router          RouteResolver             // typically *routingcore.Resolver
	Executor        executor.API              // upstream dispatch with retry/credential/health; production wires *executor.TargetExecutor
	HookConfigCache *pipeline.HookConfigCache // shared hook config cache
	ProviderReg     *provcore.Registry        // adapter-based provider registry
	HealthTracker   *store.HealthTracker      // stays concrete — used by background flush
	AuditWriter     *audit.Writer             // stays concrete
	Metrics         MetricsRecorder           // was *metrics.Recorder
	QuotaEngine     *quota.Engine             // hierarchical enforcement engine
	Cache           *cache.Cache              // response cache (nil = disabled)
	// BrokerRegistry fans live upstream calls out to multiple
	// concurrent subscribers per cache key. Concurrent requests with
	// the same cache key share one upstream call:
	// the first subscriber stamps audit.CacheStatusMiss and triggers
	// the leaderFn; joiners stamp audit.CacheStatusHitLive and
	// consume the same chunk timeline. On the broker's terminal
	// frame the cache layer persists the timeline so subsequent
	// cold lookups become true HITs. Nil disables broker fan-out;
	// every MISS opens its own upstream session in that case.
	BrokerRegistry *streamcache.Registry
	// CacheMetrics is the Prometheus instruments for the SSE cache +
	// broker subsystem. Nil disables instrumentation.
	CacheMetrics   *streamcache.Metrics
	UpstreamClient *http.Client
	// PayloadCapture is the atomically swappable payload-capture config
	// snapshot. Nil falls back to payloadcapture.DefaultConfig so a
	// caller that forgets to wire the store still gets the safe default
	// of "capture off, 256 KiB inline cutoff".
	PayloadCapture *payloadcapture.Store

	// StreamingPolicy is the hot-swappable streaming compliance policy
	// Store. proxy_cache.go's SSE handler reads Store.Get() per request
	// to dispatch between live (chunked_async) and buffer
	// (buffer_full_block) modes; nil falls back to passthrough so
	// admin policy never silently engages compliance the caller hasn't
	// opted into. Three-service alignment (#115).
	StreamingPolicy *streampolicy.Store

	// StreamCaptureHardCap is the in-memory hard ceiling on streaming
	// response capture buffers (sourced from cfg.Spill.PerObjectCap()).
	// Bytes past this hard cap continue to flow on the wire but the
	// audit buffer no longer grows; Truncated flips true. <= 0 falls
	// back to 256 MiB.
	StreamCaptureHardCap int64
	// TrafficAdapters is the shared-traffic adapter registry used by
	// the hook pipeline. The handler instantiates a format-specific
	// adapter per request via [Handler.trafficAdapterFor] so hooks on
	// native ingress routes (Anthropic, Gemini, …) run through the
	// right content extractor instead of the hard-coded OpenAI one.
	// Nil falls back to [Deps.TrafficAdapter] (single-adapter tests)
	// or the package default.
	TrafficAdapters *traffic.AdapterRegistry
	// TrafficAdapter is retained for tests that wire a single adapter
	// instance (the proxy_hook_rewrite_test and friends). Production
	// wiring should set [Deps.TrafficAdapters] instead.
	TrafficAdapter traffic.Adapter
	// SchemaMismatchRecorder receives `(ingressFormat, providerFormat)`
	// tuples when cross-format routing rejects a target. The production
	// wiring increments the `schema_mismatch_total` opsmetrics counter;
	// tests may leave this nil.
	SchemaMismatchRecorder SchemaMismatchRecorder
	// CanonicalBridge performs ingress ↔ OpenAI-hub ↔ provider wire
	// translation for chat completions. Nil uses the legacy executor path.
	CanonicalBridge canonicalbridge.API
	// RoutingDefaultPolicy is the YAML-configured platform default retry
	// policy (cfg.Routing.DefaultRetryPolicy). Per-rule overrides on the
	// matched RoutingRule field-merge on top before the effective policy
	// is passed to the executor. Zero value falls back at call time to
	// cfgpolicy.DefaultRetryPolicy() so a misconfigured deployment
	// still gets a usable policy.
	RoutingDefaultPolicy cfgpolicy.RetryPolicy
	// Allowlist is the YAML-resolved forward-header allowlist.
	// Read at request time on both the live and cache-hit response
	// paths to filter upstream → client headers; its Hash() also feeds
	// the cache key as the allowlist version. Nil falls back to the
	// embedded defaults; production startup wires
	// forwardheader.Resolve(...) into this field.
	Allowlist *forwardheader.Resolved
	// CachePricing resolves per-model cache cost rates from the
	// provider_pricing table. Used to compute
	// cache_write_cost_usd / cache_read_savings_usd / cache_net_savings_usd
	// on traffic_event. Nil disables cache-cost accounting.
	CachePricing CachePricingLookup
	// Normaliser is the prompt cache normalisation engine. Nil disables
	// both L0 key normalisation and L3 upstream body normalisation.
	Normaliser *wirerewrite.Engine
	// GeminiCacheMgrSet holds one Manager per Gemini/Vertex Provider,
	// each resolved against the three-tier prompt cache config. Nil
	// disables Gemini cache injection entirely; a nil Manager returned
	// from Get() means the provider is not in a Gemini family and the
	// request flows through unchanged.
	GeminiCacheMgrSet *geminicache.ManagerSet
	// NormalizeRegistry is the shared/normalize Registry that produces the
	// canonical *normcore.NormalizedPayload. The handler invokes Normalize
	// exactly once per request and threads the result through
	// *routingcore.RoutingContext.Request for the routing layer.
	// Nil falls back to no canonical payload — routing layer treats this
	// as "non-AI / unrecognised request" and smart strategies fall back
	// to default; production startup must wire this.
	NormalizeRegistry *normcore.Registry
	// PassthroughCache holds the runtime kill-switch configuration as an
	// atomic.Pointer-backed Snapshot. Hub pushes the merged 3-tier blob
	// via the gateway_passthrough_config shadow key; the handler resolves
	// the effective config for the routed target's provider and wraps the
	// result into ResolvedRequest. Nil falls back to "passthrough disabled
	// for every provider" — production startup must wire this.
	PassthroughCache *passthrough.Cache
	// LatencyDetail mirrors config.Observability.LatencyDetail — when
	// true the per-request latency_breakdown JSONB surfaces sub-ms
	// phases (floored to 1) so a perf investigation can verify every
	// expected phase actually ran. See traffic.PhaseTimer.SnapshotDetail.
	LatencyDetail bool
	// FreshnessDetector holds the atomically-updated time-sensitive
	// pattern detector. When non-nil AND the fleet-wide
	// extract_cache_config.apply_freshness_rules flag is on, the pre-lookup
	// classifier skips both L1 and L2 caches for messages that match the
	// detector's compiled rule set. Nil disables the check so deployments
	// that haven't pushed a freshness pattern config still work.
	FreshnessDetector *freshness.Detector
	// SemanticReader executes the L2 semantic cache lookup on every L1
	// miss. Nil disables L2 lookup entirely. Shared across all handler
	// instances. Production wires *semantic.Reader; tests may wire a stub.
	SemanticReader SemanticReaderAPI
	// SemanticWriter persists a fresh upstream response into the L2
	// semantic cache after a successful broker dispatch. Fired in a
	// detached goroutine with a 5-second deadline so it never delays
	// response delivery. Nil disables L2 write-back. Production wires
	// *semantic.Writer; tests may wire a stub.
	SemanticWriter SemanticWriterAPI
	// SemanticConfigCache is the fleet-wide singleton snapshot of
	// semantic_cache_config. L2 lookup/write is gated on
	// SemanticConfigCache.EffectiveEnabled() — when false (the default),
	// L2 is skipped entirely. When nil, L2 is also skipped.
	SemanticConfigCache *semantic.ConfigCache
	Logger              *slog.Logger
}

// Handler orchestrates the proxy request pipeline.
type Handler struct {
	deps *Deps
}

type routingFallbackError struct {
	status  int
	code    string
	message string
	hint    string
}

func (e *routingFallbackError) Error() string {
	return e.message
}

// NewHandler creates a Handler with the given dependencies.
func NewHandler(deps *Deps) *Handler {
	return &Handler{deps: deps}
}

// payloadCaptureConfig returns the active payload-capture snapshot for
// this request. A missing Store degrades to DefaultConfig so the hot
// path never needs to nil-check.
func (h *Handler) payloadCaptureConfig() payloadcapture.Config {
	if h.deps == nil || h.deps.PayloadCapture == nil {
		return payloadcapture.DefaultConfig()
	}
	return h.deps.PayloadCapture.Get()
}

// streamCaptureHardCap returns the byte ceiling for the streaming
// capture tee. Sourced from spill.perObjectCap; <= 0 defaults to 256 MiB.
func (h *Handler) streamCaptureHardCap() int64 {
	const fallback int64 = 256 * 1024 * 1024
	if h.deps == nil || h.deps.StreamCaptureHardCap <= 0 {
		return fallback
	}
	return h.deps.StreamCaptureHardCap
}

// ServeProxy returns an http.HandlerFunc for a given [Ingress]. The
// ingress descriptor declares the canonical endpoint kind, the wire
// body format a client will send, and whether the route path is
// streaming. The caller (main.go route table) passes a populated
// Ingress; downstream pipeline stages read it from the request
// context via [IngressFromContext].
//
// `x-nexus-aigw-body-format` is honoured as an explicit override only when
// the route's ingress is the OpenAI-compat family; on native routes
// the path is authoritative.
func (h *Handler) ServeProxy(in Ingress) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// All persisted timestamps are UTC instants — see docs/developers/workflow/timezone.md.
		// Latency math is also fine off UTC since time.Time carries a
		// monotonic clock reading independent of location.
		start := time.Now().UTC()
		requestID := r.Header.Get("X-Nexus-Request-Id")
		clientRequestID := r.Header.Get("x-request-id")
		// X-Nexus-Request-Id is the single canonical correlation header;
		// it carries both the request id and the cross-service trace id.
		traceID := requestID

		// Detect the effective ingress body format, honouring the
		// `x-nexus-aigw-body-format` override on the OpenAI-compat family.
		resolved, ok := in.applyHeaderOverride(r)
		if !ok {
			// Pre-resolution validation (invalid x-nexus-aigw-body-format header):
			// no rec/ingress yet, so emit the OpenAI proxy-error shape directly.
			raw := strings.TrimSpace(r.Header.Get("x-nexus-aigw-body-format"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(openAIProxyErrorBody(http.StatusBadRequest, "",
				fmt.Sprintf("unknown body format %q; supported: openai, anthropic, gemini, azure-openai, minimax, glm, deepseek", raw), ""))
			return
		}
		endpointType := string(typology.KindFromWireShape(resolved.WireShape))

		// Stamp the effective ingress on the request context so the
		// VK extractor (vkauth) and format-aware model extractor
		// (ExtractIngressModel) can read the detected format.
		ctx := WithIngress(r.Context(), resolved)
		ctx = vkauth.WithIngressFormat(ctx, resolved.BodyFormat)
		// Attach an upstream PhaseSink so the singleton tracing transport
		// (specutil/http.go buildUpstreamTransport) populates upstream
		// TTFB + upstream-total during the provider roundtrip. The sink
		// is read into rec inside finalize so both streaming and
		// non-streaming paths benefit without per-callsite wiring.
		phaseSink := traffic.NewPhaseSink()
		ctx = traffic.WithPhaseSink(ctx, phaseSink)
		// Per-request PhaseTimer captures long-tail phase durations
		// (auth, routing, quota). .Mark(name) records elapsed since the
		// previous mark, so calls are placed at phase boundaries in
		// sequence. Snapshot is written to rec.LatencyBreakdown in the
		// finalize defer below.
		phaseTimer := traffic.NewPhaseTimer()
		r = r.WithContext(ctx)

		// Stamp the canonical correlation key onto the request-scoped logger.
		// Every downstream slog line through this scope picks it up; the
		// shared SlogSink auto-lifts the same key into DiagEvent.TraceID so
		// the resulting thing_diag_event rows carry the trace in their
		// typed column without per-callsite stamping. Key is the one
		// defined in shared/core/diag (TraceIDAttrKey = "trace_id").
		logger := h.deps.Logger.With(
			"requestId", requestID,
			"trace_id", traceID,
			"endpoint", endpointType,
			"ingressFormat", string(resolved.BodyFormat),
		)

		rec := &audit.Record{
			RequestID:       requestID,
			ClientRequestID: clientRequestID,
			TraceID:         traceID,
			Timestamp:       start,
			Method:          r.Method,
			Path:            r.URL.Path,
			SourceIP:        middleware.ClientIP(r),
			// IngressFormat is the wire shape on the captured bytes —
			// ai-gateway re-encodes both request and response through
			// the codec, so the audit's RequestBody / ResponseBody
			// always match the ingress side, NOT the upstream adapter.
			// This is the routing key shared/normalize uses.
			IngressFormat: string(resolved.BodyFormat),
			// endpoint_type discriminator — canonical
			// typology.EndpointKind string ("chat", "embeddings", "stt",
			// "tts", "image_generation", "batch"). Stamped from the route
			// table's EndpointType so cost/cache stamp sites downstream
			// can dispatch the correct cost formula.
			EndpointType: endpointType,
		}

		// Centralized audit + latency via defer. The closure reads the
		// upstream PhaseSink populated by the singleton tracing transport
		// and snapshots the PhaseTimer's long-tail keys into rec before
		// finalize enqueues the audit message.
		defer func() {
			deferStart := time.Now()
			rec.UpstreamTtfbMs = phaseSink.TtfbMs()
			rec.UpstreamTotalMs = phaseSink.TotalMs()
			// Latency detail toggle: yaml-only operator flag. When true
			// (typically during a perf-investigation window) we surface
			// sub-ms phases as 1 so the row carries evidence of every
			// phase that ran. Default false keeps prod rows compact.
			detail := h.deps != nil && h.deps.LatencyDetail
			snap := phaseTimer.SnapshotDetail(detail)
			// Merge codec-layer stamps from the sink (resp_adapter_ms)
			// into the timer snapshot before persisting.
			for k, v := range phaseSink.Breakdown() {
				if snap == nil {
					snap = map[string]int{}
				}
				snap[k] += v
			}
			// upstream_body_ms: gap between TTFB and last-byte received
			// from upstream. Non-streaming: JSON body read window after
			// the first byte. Streaming: TTFB → last SSE chunk arrival
			// (matches phaseTrackedBody.Read stamping in shared/traffic).
			// Lets analytics distinguish "upstream slow to first byte"
			// (TTFB high) from "upstream slow to stream completion"
			// (upstream_body_ms high). Skip when either source is nil
			// — derived columns must not silently zero genuine missing
			// data.
			if rec.UpstreamTtfbMs != nil && rec.UpstreamTotalMs != nil {
				bodyMs := *rec.UpstreamTotalMs - *rec.UpstreamTtfbMs
				if bodyMs > 0 || detail {
					if snap == nil {
						snap = map[string]int{}
					}
					if bodyMs <= 0 {
						bodyMs = 1
					}
					snap[string(traffic.PhaseUpstreamBody)] = bodyMs
				}
			}
			// Inline the finalize body so audit_emit_ms can capture the
			// defer-tail cost BEFORE Enqueue hands rec off to the audit
			// writer goroutine (after which mutating rec is racy).
			if rec.LatencyMs == 0 {
				us := time.Since(start).Microseconds()
				ms := int((us + 999) / 1000)
				if ms < 1 {
					ms = 1
				}
				rec.LatencyMs = ms
			}
			// audit_emit_ms: time elapsed in the audit defer up to the
			// Enqueue hand-off. Captures sink reads + snapshot build +
			// LatencyMs compute. The background audit writer's flush
			// time is NOT included (separate goroutine — invisible from
			// this site). Use this column as evidence that the inline
			// emit path isn't the slow link when total >> upstream +
			// our_overhead.
			emitMs := int(time.Since(deferStart).Milliseconds())
			if emitMs > 0 || detail {
				if snap == nil {
					snap = map[string]int{}
				}
				if emitMs <= 0 {
					emitMs = 1
				}
				snap[string(traffic.PhaseAuditEmit)] = emitMs
			}
			rec.LatencyBreakdown = snap
			h.deps.AuditWriter.Enqueue(rec)
		}()

		// Phase 1: Read body (uses ingress format to pick the right
		// model-field source: JSON body for body-carrying formats,
		// URL path for Gemini/Azure).
		bodyReadStart := time.Now()
		body, modelID, isStream, err := h.readBody(r, resolved)
		phaseTimer.MarkBetween(traffic.PhaseBodyRead, time.Since(bodyReadStart))
		if err != nil {
			if errors.Is(err, errRequestTooLarge) {
				h.writeDetailedErr(w, rec,
					http.StatusRequestEntityTooLarge,
					"PAYLOAD_TOO_LARGE",
					"request body exceeds the configured network read cap",
					"Reduce the request size or ask an admin to raise payload_capture.maxRequestBytes")
				return
			}
			h.writeError(w, rec, http.StatusBadRequest, err.Error())
			return
		}

		// Stamp the literal model string the client sent (e.g. "auto",
		// "gpt-4o") on the audit record's "requested" side immediately —
		// before routing rewrites the picked target. ProviderID/Name and
		// ModelID stay empty: OpenAI-style clients don't pin a provider,
		// and the catalog UUID is a server-side concept. Routed* gets
		// filled by the cache-HIT and fetchUpstream paths below from the
		// resolved RoutingTarget. Metrics + quota + cost math read the
		// resolved target directly and are not affected by this field.
		rec.ModelName = modelID

		// Snapshot the payload-capture config once per request so the
		// pre-hook request body and later response body decisions stay
		// consistent even if the admin invalidates mid-flight (Q2=A:
		// we store "what the caller sent", not any hook-modified bytes).
		// The full body is handed to the audit Writer; spillstore.EmitBody
		// decides inline (size <= MaxInlineBodyBytes) vs spill (>) at
		// flush time. The forwarded bytes are independently bounded by
		// MaxRequestBytes (already applied to `body` above).
		pcCfg := h.payloadCaptureConfig()
		if pcCfg.StoreRequestBody && len(body) > 0 {
			rec.RequestBody = body
			rec.RequestContentType = r.Header.Get("Content-Type")
		}

		// Phase 2: VK Auth.
		vkMeta, err := h.authenticate(r)
		if err != nil {
			logger.Debug("auth failed", "error", err)
			h.writeAuthError(w, rec, err)
			return
		}
		logger.Debug("auth ok", "vkName", vkMeta.Name, "orgId", vkMeta.OrganizationID)
		// Stamp VK ID on context for credential pool sticky routingcore.
		r = r.WithContext(withStickyKey(r.Context(), vkMeta.ID))
		rec.ApplyVKMeta(vkMeta)
		// Per-VK fingerprint for cost attribution without storing the
		// raw key. Class is empty for opaque slug tokens.
		rec.APIKeyClass = vkMeta.Class
		rec.APIKeyFingerprint = vkMeta.Fingerprint
		// Override UserID with VK owner's NexusUser ID for cross-path identity correlation.
		if vkMeta.OwnerID != "" {
			rec.UserID = vkMeta.OwnerID
			// UserDisplayName already set from VKMeta
		}
		phaseTimer.Mark(traffic.PhaseAuth)

		// Phase 3: Rate limit.
		if err := h.checkRateLimit(w, vkMeta); err != nil {
			h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "RATE_LIMITED",
				err.Error(), "Reduce request frequency or contact admin to increase limits")
			return
		}
		// Set rate limit visibility headers.
		if vkMeta.RateLimitRpm != nil {
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(*vkMeta.RateLimitRpm))
		}

		// Phase 3.5: Build the canonical request context. One
		// normcore.Registry.Normalize call per request produces the
		// canonical *normcore.NormalizedPayload that L4 consumers
		// (routing first; hooks + audit follow in subsequent stories)
		// read instead of re-parsing raw bytes. The S1 RequestContext
		// type is the L3 immutable carrier; routing reads its Normalized()
		// via *routingcore.RoutingContext.Request.
		rctxFull := h.buildRequestContext(r, vkMeta, body, in.BodyFormat, modelID, endpointType)

		// Phase 4: Routing.
		routeResult, err := h.resolveRoute(r.Context(), rctxFull, modelID, typology.EndpointKind(endpointType))
		if err != nil {
			// Capability pre-filter: all candidates were rejected for this
			// embedding request. Emit a structured 400 with
			// available_capabilities so the client knows what each model
			// supports.
			//
			// Edge case: when zero routing rules are enabled, resolver.go
			// short-circuits on the embeddings endpoint and returns an
			// empty NoCompatibleProviderError (Available=[]). Chat falls
			// through to the passthrough fallback in this case; embeddings
			// should too. An empty Available list means no candidate was
			// ever evaluated by the capability filter, so the "no
			// compatible capability" error message is misleading — try the
			// passthrough fallback instead.
			var ncpErr *routingcore.NoCompatibleProviderError
			if errors.As(err, &ncpErr) {
				if len(ncpErr.Available) > 0 {
					h.writeNoCompatibleCapability(w, rec, ncpErr)
					return
				}
				logger.Debug("empty NoCompatibleProviderError; trying passthrough fallback", "model", modelID)
				// fall through to the no-targets passthrough path below
			} else {
				logger.Error("routing failed", "error", err)
				h.writeDetailedErr(w, rec, http.StatusInternalServerError, "ROUTING_NO_MATCH",
					"routing failed", "Check that a routing rule exists for this model")
				return
			}
		}
		if routeResult == nil || len(routeResult.Targets) == 0 {
			logger.Debug("no routing targets resolved; trying passthrough fallback", "model", modelID)
			fallbackResult, fallbackErr := h.resolveNoMatchPassthrough(r.Context(), modelID, vkMeta, resolved)
			if fallbackErr != nil {
				var routingErr *routingFallbackError
				if errors.As(fallbackErr, &routingErr) {
					h.writeDetailedErr(w, rec, routingErr.status, routingErr.code, routingErr.message, routingErr.hint)
					return
				}
				logger.Error("passthrough fallback failed", "model", modelID, "error", fallbackErr)
				h.writeDetailedErr(w, rec, http.StatusInternalServerError, "ROUTING_NO_MATCH",
					"routing fallback failed", "Check gateway model catalog and provider configuration")
				return
			}
			routeResult = fallbackResult
		}
		logger.Debug("route resolved",
			"model", modelID,
			"targets", len(routeResult.Targets),
			"ruleId", routeResult.RuleID,
			"provider", routeResult.Targets[0].ProviderName,
		)
		rec.RoutingRuleID = routeResult.RuleID
		rec.RoutingRuleName = routeResult.RuleName
		if t := buildRoutingAuditTrace(routeResult); t != nil {
			rec.RoutingTrace = t
		}
		// Populate provider from the resolved primary target so the
		// traffic event carries meaningful context even for OpenAI-style
		// requests where the client only specifies a model code.
		//
		// ModelID + ModelName are intentionally NOT overwritten here:
		// rec.ModelName was stamped at readBody with the literal client
		// model string ("claude-opus-4-7") and represents the REQUESTED
		// side — the audit table's distinct routed_model_id /
		// routed_model_name columns are filled by fetchUpstream /
		// cache-HIT later from the actually-served RoutingTarget.
		// Overwriting rec.ModelName here would replace the user's
		// requested model with the routed pick and make the "REQUESTED
		// MODEL" column in the UI lie about what the client asked for.
		if len(routeResult.Targets) > 0 {
			primary := routeResult.Targets[0]
			rec.ProviderID = primary.ProviderID
			rec.ProviderName = primary.ProviderName
		}

		// Phase 4.5: resolve effective passthrough config for the primary
		// target's provider and wrap the L3 RequestContext + post-routing
		// decisions into an immutable ResolvedRequest. Stashed on
		// r.Context() so downstream consumers (hooks pipeline, audit,
		// executor) can read passthrough state without re-resolving.
		//
		// The cache is empty cold-start (fail-closed); Effective returns
		// nil until Hub pushes a real snapshot, and Resolve preserves nil.
		// Nil-receiver methods (AnyBypassActive, Flags) treat nil as
		// "no bypass".
		var primaryTarget routingcore.RoutingTarget
		if len(routeResult.Targets) > 0 {
			primaryTarget = routeResult.Targets[0]
		}
		var passthroughCfg *passthrough.Config
		if h.deps.PassthroughCache != nil {
			passthroughCfg = h.deps.PassthroughCache.Effective(primaryTarget.ProviderID, primaryTarget.AdapterType)
		}
		resolvedReq := requestcontext.Resolve(rctxFull, routeResult, passthroughCfg)
		r = r.WithContext(requestcontext.WithResolved(r.Context(), resolvedReq))

		// Stamp the bypass flags + operator reason on the audit record
		// so every downstream branch (hooks skip, cache skip, response
		// normalize skip) writes a row whose passthrough_flags column
		// reflects which layers were bypassed. PassthroughFlags is the
		// canonical-order slice from passthrough.Config.Flags() —
		// operators grep / SQL-filter on these literals.
		if pt := resolvedReq.Passthrough(); pt.AnyBypassActive() {
			rec.PassthroughFlags = pt.Flags()
			rec.PassthroughReason = pt.Reason
		}
		phaseTimer.Mark(traffic.PhaseRouting)

		// Phase 4.1: Cross-format routing filter.
		// When CanonicalBridge is wired, chat completions use the OpenAI
		// hub matrix ([canonicalbridge.Bridge.EndpointRoutable]); otherwise
		// tests fall back to the legacy rule (same format or OpenAI ingress).
		compat, incompatible := filterCompatibleTargets(resolved.BodyFormat, routeResult.Targets, resolved.WireShape, h.deps.CanonicalBridge)
		if h.deps.SchemaMismatchRecorder != nil {
			for _, rt := range incompatible {
				h.deps.SchemaMismatchRecorder.RecordSchemaMismatch(string(resolved.BodyFormat), string(rt.ProviderFormat))
			}
		}
		if len(compat) == 0 {
			providerFormat := ""
			if len(incompatible) > 0 {
				providerFormat = string(incompatible[0].ProviderFormat)
			}
			h.writeNoCompatibleProvider(w, rec, resolved.BodyFormat, providerFormat)
			return
		}
		routeResult.Targets = compat

		// Phase 4.2: Responses-API cross-format guard.
		// When ingress is /v1/responses and the resolved primary target's
		// adapter does NOT natively serve responses-api, stateful fields +
		// OpenAI-native built-in tools cannot be honoured: reject the
		// request with a Responses-shape 400 envelope BEFORE the request
		// hits hooks / quota / executor.
		if resolved.BodyFormat == provcore.FormatOpenAIResponses &&
			len(routeResult.Targets) > 0 &&
			h.deps.CanonicalBridge != nil {
			targetFormat := provcore.Format(routeResult.Targets[0].AdapterType)
			if !h.deps.CanonicalBridge.TargetNativelyServesResponsesAPI(targetFormat) {
				if rej := validateResponsesIngressForCrossFormat(body); rej != nil {
					h.writeResponsesFeatureRejection(w, rec, rej)
					return
				}
			}
		}

		// Cross-format streaming compatibility pre-check for EVERY chat-kind
		// ingress (openai-chat, anthropic /v1/messages, gemini, responses), not
		// just openai-chat — the per-ingress SSE transcoder
		// (NewStreamTranscoder, keyed on ingress.BodyFormat) handles the
		// response re-encode, but pairs StreamShapeCompatible rejects (e.g.
		// anything involving Bedrock) must fail fast with a clear 4xx rather
		// than a messy mid-stream error.
		if isStream && typology.KindFromWireShape(resolved.WireShape) == typology.EndpointKindChat &&
			len(routeResult.Targets) > 0 &&
			!canonicalbridge.StreamShapeCompatible(resolved.BodyFormat, provcore.Format(routeResult.Targets[0].AdapterType)) {
			h.writeCrossFormatStreamUnsupported(w, rec, string(resolved.BodyFormat), routeResult.Targets[0].AdapterType)
			return
		}

		// Phase 4.5: Quota check.
		quotaInPrice, quotaOutPrice, quotaDecision := h.checkQuota(r, w, rec, vkMeta, routeResult, body, modelID)
		if rec.StatusCode != 0 {
			return // quota rejected, response already written
		}
		phaseTimer.Mark(traffic.PhaseQuota)

		// Pre-stamp the request-side embedding metadata so all downstream
		// paths (live, stream HIT, non-stream HIT, broker stream HIT_LIVE,
		// broker non-stream HIT_LIVE) inherit it without needing the
		// original request body. The response-side dimension field is
		// updated in each path when the response arrives (live:
		// handleNonStream; HIT paths: their response
		// replay code). crossFormatRouting detects ingress ≠ target.
		if endpointType == "embeddings" && len(routeResult.Targets) > 0 {
			crossFormatRouting := provcore.Format(routeResult.Targets[0].AdapterType) != resolved.BodyFormat
			rec.Metadata = preStampEmbeddingRequestMeta(rec.Metadata, body, crossFormatRouting)
		}

		// Phase 5: Request hooks.
		// Pass the (post-quota) primary target so hook inputs carry
		// ProviderRegion for data-residency evaluation. Quota downgrade
		// ran above, so routeResult.Targets[0] already reflects the
		// real upstream that will be dispatched.
		var requestHookTarget routingcore.RoutingTarget
		if len(routeResult.Targets) > 0 {
			requestHookTarget = routeResult.Targets[0]
		}
		// bypassHooks: skip the request-stage hooks pipeline entirely
		// when emergency passthrough is active for the routed provider.
		// rec.HookDecision is stamped "BYPASSED" so audit consumers can
		// SQL-filter for requests that ran without hook evaluation.
		// Variables are declared in the outer scope so downstream code
		// (cache key build, audit population) sees the zero-value
		// reqHookResult on the bypass path without further branching.
		var (
			rewrittenBody []byte
			reqHookResult *hookcore.CompliancePipelineResult
			rejected      bool
		)
		if pt := resolvedReq.Passthrough(); pt.AnyBypassActive() && pt.BypassHooks {
			rec.HookDecision = "BYPASSED"
		} else {
			rewrittenBody, reqHookResult, rejected = h.runRequestHooks(r, w, rec, requestID, body, requestHookTarget, resolved, logger)
			if rejected {
				return
			}
			if rewrittenBody != nil {
				body = rewrittenBody
			}
		}

		// Phase 5.5: Cache lookup. Every non-rejected request
		// takes exactly one of these paths:
		//   - DISABLED / SKIP_NO_CACHE → fall through to live upstream;
		//     no cache key, no broker, no Redis touch.
		//   - HIT (Redis): replay the cached chunk timeline (stream) or
		//     re-encode the cached canonical response (non-stream)
		//     through the same downstream pipeline used for MISS;
		//     hooks always run (D2).
		//   - MISS (broker): subscribe to streamcache.Registry. The
		//     first subscriber stamps MISS and triggers leaderFn;
		//     joiners stamp HIT_LIVE and consume the in-flight stream.
		//     On the broker's terminal frame the cache layer persists
		//     the timeline so subsequent cold lookups become true HITs.
		//
		// The cache key uses the bytes that WILL be sent upstream
		// (output of adapter.PrepareBody) so equivalent requests
		// (different client model aliases, different SDK JSON key
		// orderings) hash to the same key.
		// passthroughBypassCache short-circuits the cache lookup entirely
		// (and therefore also any cache-write later, since writes only
		// happen on misses that ran a lookup). The bypass takes precedence
		// over the client header so an operator forcing passthrough cannot
		// be overridden by an end-user header.
		passthroughBypassCache := false
		if pt := resolvedReq.Passthrough(); pt.AnyBypassActive() && pt.BypassCache {
			passthroughBypassCache = true
		}
		// Project canonical NormalizedPayload messages → freshness.ChatMessage
		// for the time-sensitivity detector. Nil canonical payload or empty
		// messages list = nil slice → detector returns false (fail-open).
		var canonicalMsgs []freshness.ChatMessage
		if np := rctxFull.Normalized(); np != nil {
			canonicalMsgs = normMessagesToFreshness(np.Messages)
		}
		// cacheEnabled reads the runtime enabled flag set by Hub pushes
		// (response_cache.extract_config), not just "is *Cache wired".
		// skipTimeSensitivePolicy reads the apply_freshness_rules gate
		// so freshness-rule matches actually skip cache.
		preLookupStatus, preLookupSkipReason := classifyCachePreLookup(
			h.deps.Cache != nil && h.deps.Cache.IsEnabled(),
			r.Header.Get("x-nexus-aigw-no-cache") != "",
			len(routeResult.Targets) > 0,
			passthroughBypassCache,
			h.deps.FreshnessDetector,
			canonicalMsgs,
			h.deps.Cache.ApplyFreshnessRules(),
		)
		var (
			cacheKey               string
			gatewayCacheStatus     audit.GatewayCacheStatus
			gatewayCacheSkipReason audit.GatewayCacheSkipReason
			cachePreparedBody      []byte   // PrepareBody output, reused on MISS to skip a duplicate encode in the executor
			cachePreparedRewrites  []string // matching rewrites slice; goes into Response.Coerced
		)
		switch preLookupStatus {
		case audit.GatewayCacheSkipped:
			gatewayCacheStatus = preLookupStatus
			gatewayCacheSkipReason = preLookupSkipReason
			switch preLookupSkipReason {
			case audit.GatewayCacheSkipReasonDisabled:
				h.deps.CacheMetrics.RecordLookup("disabled")
			case audit.GatewayCacheSkipReasonNoCache:
				h.deps.CacheMetrics.RecordLookup("skip_no_cache")
			case audit.GatewayCacheSkipReasonPassthrough:
				h.deps.CacheMetrics.RecordLookup("passthrough_skip")
			}
		default:
			primary := routeResult.Targets[0]
			adapter, ok := h.deps.ProviderReg.Get(provcore.Format(primary.AdapterType))
			if !ok {
				// Phase 4.1 already gated on adapter availability; defensive fallback
				// — skip cache, proceed to live upstream.
				gatewayCacheStatus = audit.GatewayCacheSkipped
				gatewayCacheSkipReason = audit.GatewayCacheSkipReasonDisabled
				h.deps.CacheMetrics.RecordLookup("disabled")
				break
			}

			// PrepareBody runs the model-alias rewrite + codec
			// translation that the executor would otherwise do
			// internally. Only ProviderModelID and Format on the
			// CallTarget matter for body preparation; the executor
			// resolves the full target (BaseURL, APIKey, Extras)
			// on the wire path. PrepareBody is idempotent so the
			// executor running it again on the MISS path produces
			// the same bytes.
			//
			// G3 (provider-adapter-architecture.md §11): PrepareBody's
			// codec contract requires canonical OpenAI input. When the
			// caller's ingress format differs from the target format,
			// canonicalize via the bridge first. Without this step a
			// cross-format route (e.g. Anthropic ingress → OpenAI
			// target) would hand the Anthropic-shape body to
			// openairesponses.identityCodec (identity), which forwards it
			// verbatim and the upstream 400s.
			prepReq := buildProviderRequest(r, resolved, body, isStream, h.payloadCaptureConfig().MaxResponseBytes)
			prepReq.Target = provcore.CallTarget{
				ProviderID:      primary.ProviderID,
				ProviderName:    primary.ProviderName,
				Format:          provcore.Format(primary.AdapterType),
				ProviderModelID: primary.ProviderModelID,
				BaseURL:         primary.BaseURL,
			}
			// Cross-format canonicalization: "cross-format" depends on
			// the endpoint shape, not just the wire format string:
			//   - chat-completions ingress → canonicalize iff target wire
			//     format is not OpenAI (canonical = OpenAI chat-completions).
			//   - /v1/responses ingress    → canonicalize iff target wire
			//     format does NOT natively serve the Responses API.
			//     A naive `BodyFormat != AdapterType` check would
			//     misfire here because FormatOpenAIResponses !=
			//     FormatOpenAI even when the target IS OpenAI — that
			//     turned a native passthrough into a canonicalize, and
			//     OpenAI returned 400 "Unsupported parameter: 'messages'.
			//     In the Responses API…".
			//
			// When we canonicalize a /v1/responses request, both
			// prepReq.WireShape AND resolved.WireShape must be downgraded
			// to WireShapeOpenAIChat. prepReq.WireShape drives the
			// codec (spec_anthropic / spec_gemini only know
			// "chat-completions" — without the downgrade they return
			// `<provider>: unsupported endpoint "responses" for codec`).
			// resolved.WireShape is what fetchUpstreamWithPreparedBody later
			// hands to buildProviderRequest, which drives the URL
			// builder — without the downgrade the URL builder returns
			// `build url: <provider>: unsupported endpoint "responses"`.
			// The egress reshape path keys off resolved.BodyFormat (still
			// FormatOpenAIResponses), so the client still sees a
			// Responses-shape body.
			// Per-endpoint canonicalization decision:
			//   chat-completions: canonicalize whenever ingress ≠ target
			//     wire format. The downstream codec dispatch in
			//     specAdapter.PrepareBody handles OpenAI-wire-shape
			//     passthrough (Moonshot/Mistral/Groq/...) by matching on
			//     IsOpenAIFamily() AFTER canonicalization. So
			//     Anthropic→OpenAI / Gemini→Mistral / etc. all flow
			//     through the bridge; OpenAI→OpenAI doesn't because
			//     formats already match.
			//   /v1/responses: canonicalize only when the target adapter
			//     does NOT natively serve responses-api. The naive
			//     `BodyFormat != AdapterType` check misfires here because
			//     FormatOpenAIResponses != FormatOpenAI even when the
			//     target IS OpenAI — that turned native passthrough
			//     into canonicalize and broke the Responses-shape body.
			// Cross-format canonicalization is driven by the ingress
			// EndpointKind, not a hardcoded openai-chat/responses list, so
			// EVERY chat-kind ingress (openai-chat, anthropic /v1/messages,
			// gemini generateContent, Azure, GLM) gets the same canonical →
			// target-wire translation. "ingress shape in = ingress shape out"
			// is preserved end-to-end: resolved.WireShape (the caller's shape)
			// is left intact, and the executor derives the call-time wire
			// shape from the target while egress reshapes via the immutable
			// context ingress.
			targetFmt := provcore.Format(primary.AdapterType)
			ingressKind := typology.KindFromWireShape(resolved.WireShape)
			isEmbeddingsIngress := ingressKind == typology.EndpointKindEmbeddings
			needsCanonicalization := false
			if h.deps.CanonicalBridge != nil {
				switch {
				case resolved.WireShape == typology.WireShapeOpenAIResponses:
					// Responses is chat-kind but has its own native-passthrough
					// rule (only targets that natively serve /v1/responses).
					needsCanonicalization = !h.deps.CanonicalBridge.TargetNativelyServesResponsesAPI(targetFmt)
				case ingressKind == typology.EndpointKindChat, isEmbeddingsIngress:
					needsCanonicalization = resolved.BodyFormat != targetFmt
				}
			}
			if needsCanonicalization {
				var canonBody []byte
				var canonErr error
				if isEmbeddingsIngress {
					canonBody, canonErr = h.deps.CanonicalBridge.IngressEmbeddingsToCanonical(resolved.BodyFormat, prepReq.Body, prepReq.Target)
				} else {
					canonBody, canonErr = h.deps.CanonicalBridge.IngressChatToCanonical(resolved.BodyFormat, prepReq.Body, prepReq.Target)
					// Stamp the streaming intent onto the canonical body. Gemini
					// ingress signals streaming via the :streamGenerateContent URL,
					// not a body field, so the canonical chat body carries no
					// `stream` — without this the target codec (e.g. Anthropic, which
					// propagates `stream` from canonical input) sends a non-streaming
					// upstream request and the client's SSE loses all text. Chat-kind
					// only; embeddings never stream.
					if canonErr == nil && isStream {
						canonBody = canonicalbridge.EnsureCanonicalStream(canonBody)
					}
				}
				if canonErr != nil {
					h.writeError(w, rec, http.StatusBadRequest, "canonicalize ingress body: "+canonErr.Error())
					return
				}
				prepReq.Body = canonBody
				prepReq.BodyFormat = provcore.FormatOpenAI
				// The cache-prep codec must encode to the TARGET adapter's
				// native wire shape (e.g. anthropic-messages, gemini embedContent),
				// not the caller's ingress shape — otherwise the target codec
				// rejects "openai-chat"/"openai-embeddings". This matches the bytes
				// the executor produces (cache-key + MISS-reuse parity).
				if isEmbeddingsIngress {
					prepReq.WireShape = h.deps.CanonicalBridge.EmbeddingsWireShapeForTarget(targetFmt)
				} else {
					prepReq.WireShape = h.deps.CanonicalBridge.ChatWireShapeForTarget(targetFmt)
				}
				if resolved.WireShape == typology.WireShapeOpenAIResponses {
					// /v1/responses canonicalizes to chat-completions. Downgrade
					// the per-request Ingress copy (not `in`, the shared closure
					// param) so the executor treats it as chat-kind on the
					// failover path. resolved.BodyFormat stays
					// FormatOpenAIResponses so egress still hits the Responses
					// encoder (egress reads the immutable context ingress).
					resolved.WireShape = typology.WireShapeOpenAIChat
				}
			}
			prepStart := time.Now()
			finalBody, finalRewrites, err := adapter.PrepareBody(prepReq)
			if err != nil {
				h.writeError(w, rec, http.StatusBadRequest, "prepare body: "+err.Error())
				return
			}
			phaseTimer.MarkBetween(traffic.PhaseReqAdapter, time.Since(prepStart))
			cachePreparedBody = finalBody
			cachePreparedRewrites = finalRewrites

			// L0 (E38): key normalisation — strip volatile fields (e.g. cch=
			// billing nonce) from the body ONLY for cache key computation.
			// Never mutates cachePreparedBody; fail-open.
			keyBody := finalBody
			if h.deps.Normaliser != nil {
				keyBody = h.deps.Normaliser.NormalizeKey(primary.AdapterType, finalBody)
			}
			cacheKey = h.deps.Cache.BuildKey(primary.ProviderName, primary.ProviderModelID, keyBody, allowlistVersionFromDeps(h.deps))
			rec.CacheKey = cacheKey

			if isStream {
				if entry := h.deps.Cache.LookupStream(r.Context(), cacheKey); entry != nil {
					rec.GatewayCacheStatus = audit.GatewayCacheHit
					rec.GatewayCacheKind = audit.GatewayCacheKindExtract
					rec.ProviderCacheStatus = audit.ProviderCacheNA
					rec.UpstreamAdapterType = primary.AdapterType
					h.deps.Cache.RecordHit(r.Context())
					h.deps.CacheMetrics.RecordLookup("hit")
					h.handleStreamHit(r, w, rec, primary, routeResult, reqHookResult, entry, quotaInPrice, quotaOutPrice, quotaDecision, endpointType, requestID, start, logger)
					return
				}
			} else {
				if entry := h.deps.Cache.LookupResponse(r.Context(), cacheKey); entry != nil {
					rec.GatewayCacheStatus = audit.GatewayCacheHit
					rec.GatewayCacheKind = audit.GatewayCacheKindExtract
					rec.ProviderCacheStatus = audit.ProviderCacheNA
					rec.UpstreamAdapterType = primary.AdapterType
					h.deps.Cache.RecordHit(r.Context())
					h.deps.CacheMetrics.RecordLookup("hit")
					h.handleNonStreamHit(r, w, rec, primary, routeResult, reqHookResult, entry, quotaInPrice, quotaOutPrice, quotaDecision, endpointType, requestID, start, logger)
					return
				}
			}
			h.deps.Cache.RecordMiss(r.Context())
			h.deps.CacheMetrics.RecordLookup("miss")
			gatewayCacheStatus = audit.GatewayCacheMiss

			// L2 semantic cache lookup on L1 miss.
			// tryL2Lookup is a no-op (returns false) when SemanticReader is nil
			// or the per-route policy has semantic.enabled=false, so it is safe
			// to call unconditionally on every L1 miss.
			if h.tryL2Lookup(l2ReadParams{
				r:             r,
				w:             w,
				rec:           rec,
				routeResult:   routeResult,
				primary:       primary,
				isStream:      isStream,
				resolved:      resolved,
				reqHookResult: reqHookResult,
				quotaInPrice:  quotaInPrice,
				quotaOutPrice: quotaOutPrice,
				quotaDecision: quotaDecision,
				endpointType:  endpointType,
				requestID:     requestID,
				start:         start,
				logger:        logger,
				canonicalMsgs: func() []normcore.Message {
					if np := rctxFull.Normalized(); np != nil {
						return np.Messages
					}
					return nil
				}(),
			}) {
				return // L2 HIT — response already written
			}
		}
		// Stamp gateway-side detail fields on the record. Unified
		// rec.CacheStatus is derived at audit-write time from these +
		// ProviderCacheStatus (which the response-usage parser stamps
		// later when the upstream returns).
		rec.GatewayCacheStatus = gatewayCacheStatus
		rec.GatewayCacheSkipReason = gatewayCacheSkipReason
		// Header value: "HIT" was already emitted on the direct-HIT branches
		// above (which return); here the request is going to upstream, so
		// emit the unified MISS.
		w.Header().Set("X-Nexus-Cache", string(audit.CacheStatusMiss))
		phaseTimer.Mark(traffic.PhaseCacheLookup)

		// Phase 6+7+8: live upstream + downstream pipeline.
		//
		// Body normalisation — strip volatile bytes and inject
		// cache_control markers (Anthropic/Bedrock) and Gemini cachedContent
		// references before upstream dispatch. Runs on every MISS regardless
		// of broker wiring so that provider-side caching works even when the
		// response-cache dedup broker is not configured. No-op when
		// cachePreparedBody is empty (DISABLED or SKIP_NO_CACHE paths never
		// set it).
		if h.deps.Normaliser != nil && len(cachePreparedBody) > 0 {
			normStart := time.Now()
			primary := routeResult.Targets[0]
			normBody, normResult := h.deps.Normaliser.NormalizeUpstream(
				primary.AdapterType, primary.ProviderID, cachePreparedBody)
			if !normResult.DryRun {
				cachePreparedBody = normBody
			}
			rec.NormalizedStripCount = normResult.StripCount
			rec.NormalizedStripBytes = normResult.StripBytes
			rec.CacheMarkerInjected = normResult.MarkersInjected
			phaseTimer.MarkBetween(traffic.PhaseNormUpstream, time.Since(normStart))
		}
		// Gemini cachedContent injection: rewrite the prepared body to
		// reference a cached systemInstruction object. Runs after body
		// normalisation. Fail-open: errors are logged and the original body
		// is forwarded unchanged. Manager is per-provider (resolved against
		// the 3-tier cache_config blob) — ManagerSet.Get returns nil for
		// non-Gemini providers, which short-circuits this block.
		// geminicacheInvalidate is the per-request hook to drop the Redis
		// entry that fed this request's cachedContent injection. Set on
		// HIT below, called from the response path when the upstream
		// reports the cache has been evicted (403 / "CachedContent not
		// found"). Nil on miss so the call site can `if … != nil` cheaply.
		var geminicacheInvalidate func()
		if h.deps.GeminiCacheMgrSet != nil && len(cachePreparedBody) > 0 {
			primary := routeResult.Targets[0]
			if provcore.Format(primary.AdapterType) == provcore.FormatGemini {
				if mgr := h.deps.GeminiCacheMgrSet.Get(primary.ProviderID); mgr != nil {
					injected, injectResult, injectErr := mgr.Inject(
						r.Context(), primary.ProviderID, primary.ProviderModelID, cachePreparedBody)
					if injectErr != nil {
						logger.Warn("geminicache inject error, pass-through", "error", injectErr)
					} else {
						cachePreparedBody = injected
						geminicacheInvalidate = injectResult.Invalidate
					}
				}
			}
		}
		// Stash the invalidate hook on the context so handleNonStream /
		// stream paths can fire it without threading another parameter.
		if geminicacheInvalidate != nil {
			r = r.WithContext(withGeminiCacheInvalidate(r.Context(), geminicacheInvalidate))
		}

		// When cacheStatus == MISS and BrokerRegistry is wired, fan the
		// upstream out through the broker so concurrent requests with the
		// same key share one call. Joiners stamp HIT_LIVE.
		// On any other status (DISABLED / SKIP_NO_CACHE) we go direct.
		if gatewayCacheStatus == audit.GatewayCacheMiss && h.deps.BrokerRegistry != nil {
			// canonicalMsgs feeds the broker-path L2 write-back —
			// without this thread-through the broker leg silently
			// skipped scheduleL2Write and L2 stayed empty.
			var brokerCanonMsgs []normcore.Message
			if np := rctxFull.Normalized(); np != nil {
				brokerCanonMsgs = np.Messages
			}
			h.runViaBroker(r, w, rec, routeResult, body, isStream, resolved, reqHookResult, cacheKey, cachePreparedBody, cachePreparedRewrites, quotaInPrice, quotaOutPrice, quotaDecision, endpointType, requestID, start, logger, brokerCanonMsgs)
			return
		}

		// Direct path (cache disabled, no broker, or SKIP_NO_CACHE).
		// Pass the prepared+normalised body when available so the executor
		// skips its internal PrepareBody call (idempotent, saves a µs-scale
		// encode; nil body falls back to plain Execute behaviour).
		result, target, attempts, err := h.fetchUpstreamWithPreparedBody(r, w, rec, routeResult, body, isStream, resolved, cachePreparedBody, cachePreparedRewrites, start, logger)
		if err != nil {
			return // error response already written
		}

		// Stamp the routed upstream adapter type so the audit-side
		// normalizer can pick the correct response normalizer for
		// cross-format requests (e.g. /v1/responses → Anthropic
		// /v1/messages). Without this, normalizeAdapterType falls back
		// to IngressFormat and feeds the wrong unmarshaler an SSE
		// body, producing `traffic_event_normalized.response_status=
		// partial` with the misleading `openai-responses: response
		// unmarshal: invalid character 'e'` error.
		rec.UpstreamAdapterType = target.AdapterType

		// Forward allowlisted upstream response headers BEFORE the Nexus
		// stamps so any conflict (e.g. an upstream emitting `via` or
		// `server`) is overwritten by Nexus on the same key — see
		// docs/developers/specs/e36/e36-s2-forward-header-yaml-response.md "Nexus wins"
		// invariant. isCacheHit=false on this direct (live) path.
		writeForwardedResponseHeaders(w, h.deps.Allowlist, provcore.Format(target.AdapterType), result.Headers, false)

		if isStream {
			h.setResponseHeadersStream(w, rec, target, routeResult, attempts)
			w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(reqHookResult)))
			if len(result.Coerced) > 0 {
				w.Header().Set("X-Nexus-Coerced", strings.Join(result.Coerced, ","))
			}
			// Wrap result.Stream into a ChunkSubscription so the
			// downstream pipeline shares one shape with the broker
			// path. There is no cache write on the direct path
			// (cache is disabled or off for this request).
			sub := newDirectStreamSubscription(result.Stream)
			h.handleStreamWithSubscription(r, w, rec, sub, target, result.Coerced, quotaInPrice, quotaOutPrice, quotaDecision, endpointType, requestID, start, logger)
		} else {
			// Stamp the PhaseSink values onto rec NOW so
			// setResponseHeaders can emit x-nexus-aigw-upstream-*
			// headers. The finalize defer redundantly does the same
			// at request end — idempotent.
			rec.UpstreamTtfbMs = phaseSink.TtfbMs()
			rec.UpstreamTotalMs = phaseSink.TotalMs()
			h.setResponseHeaders(w, rec, target, routeResult, start, attempts)
			w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(reqHookResult)))
			if len(result.Coerced) > 0 {
				w.Header().Set("X-Nexus-Coerced", strings.Join(result.Coerced, ","))
			}
			// Fire L2 semantic write-back in a background goroutine.
			// Non-streaming only; streaming is persisted by the broker.
			if gatewayCacheStatus == audit.GatewayCacheMiss {
				var l2CanonMsgs []normcore.Message
				if np := rctxFull.Normalized(); np != nil {
					l2CanonMsgs = np.Messages
				}
				h.scheduleL2Write(
					routeResult,
					routeResult.Targets[0],
					l2CanonMsgs,
					result.Body,
					provcoreUsageToMap(&result.Usage),
					resolveL2VKScope(rec, ""), // empty varyBy → VK scope
					false,
					in,
					logger,
				)
			}
			h.handleNonStream(r, w, rec, result, target, quotaInPrice, quotaOutPrice, quotaDecision, endpointType, requestID, start, logger)
		}
	}
}

func (h *Handler) resolveNoMatchPassthrough(ctx context.Context, requestedModel string, vkMeta *vkauth.VKMeta, in Ingress) (*routingcore.RouteResult, error) {
	if h.deps == nil || h.deps.Models == nil {
		return nil, &routingFallbackError{
			status:  http.StatusInternalServerError,
			code:    "ROUTING_NO_MATCH",
			message: "passthrough fallback is unavailable",
			hint:    "Model lookup dependency is not configured",
		}
	}

	model, err := h.deps.Models.GetModelByCode(ctx, requestedModel)
	if err != nil || model == nil {
		return nil, &routingFallbackError{
			status:  http.StatusNotFound,
			code:    "ROUTING_NO_MATCH",
			message: "no available provider for model " + requestedModel,
			hint:    "Ensure the model exists and is enabled",
		}
	}

	if vkMeta != nil && len(vkMeta.AllowedModels) > 0 &&
		!routingcore.ModelMatchesAllowedRefs(model.ID, model.ProviderModelID, model.ProviderID, vkMeta.AllowedModels) {
		return nil, &routingFallbackError{
			status:  http.StatusForbidden,
			code:    "MODEL_NOT_ALLOWED",
			message: "model " + requestedModel + " is not allowed for this virtual key",
			hint:    "Use an allowed model or request policy update",
		}
	}

	providerName := model.ProviderName
	if providerName == "" {
		providerName = model.ProviderID
	}
	// Use the provider's actual wire adapter type so the normaliser
	// (L3/L4) and cache-key preparation use the correct format.
	// Falls back to the ingress format when adapter_type is not
	// stored (legacy rows or test doubles).
	adapterType := model.ProviderAdapterType
	if adapterType == "" {
		adapterType = string(in.BodyFormat)
	}
	target := routingcore.RoutingTarget{
		ProviderID:      model.ProviderID,
		ProviderName:    providerName,
		AdapterType:     adapterType,
		ModelID:         model.ID,
		ModelCode:       model.Code,
		ModelName:       model.Name,
		ProviderModelID: model.ProviderModelID,
		BaseURL:         model.ProviderBaseURL,
		Source:          "passthrough-fallback",
	}
	return &routingcore.RouteResult{
		Targets:  []routingcore.RoutingTarget{target},
		RuleID:   "passthrough-fallback",
		RuleName: "passthrough-fallback",
	}, nil
}

// finalize computes latency and enqueues the audit record. Called via defer.
//
// Latency uses ceiling-millisecond rounding (µs → ms with round-up) so a
// sub-millisecond cache hit reports as 1ms instead of 0. Reporting 0 here
// was a real bug: the wire format treats 0 as "field absent" (omitempty
// upstream + *int downstream), so the Hub stored NULL and the UI's
// Latency column rendered "-" on every fast cache hit.
func (h *Handler) finalize(rec *audit.Record, start time.Time) {
	if rec.LatencyMs == 0 {
		us := time.Since(start).Microseconds()
		ms := int((us + 999) / 1000)
		if ms < 1 {
			ms = 1
		}
		rec.LatencyMs = ms
	}
	h.deps.AuditWriter.Enqueue(rec)
}

// errRequestTooLarge is returned by readBody when the inbound body
// exceeds payloadcapture.MaxRequestBytes. ServeProxy maps this to
// `413 Payload Too Large` instead of the generic 400 path so admins can
// distinguish a malformed request from one that simply outgrew the
// network read cap.
var errRequestTooLarge = errors.New("request body exceeds the configured network read cap")

// readBody reads the request body, extracts the client-requested
// model, and determines the stream flag. Model and stream sources are
// format-specific (path params for Gemini/Azure, body `model` for
// body-carrying formats) and resolved via [ExtractIngressModel].
//
// endpointType is used to reject model="auto" for non-chat endpoints.
// The network read cap is taken from the runtime payload-capture store
// (`MaxRequestBytes`, default 10 MiB) so admin edits take effect on the
// very next request without a restart. A non-positive store value
// falls back to the package default so a stale or malformed config
// never collapses the read to zero (which would otherwise 413 every
// inbound request). The inline-vs-spill cutoff (`MaxInlineBodyBytes`)
// is NOT applied here — it only governs how the captured copy is
// stored on traffic_event_payload (inline JSONB vs spill file).
//
// To detect overflow without buffering the oversized body in memory we
// read up to `maxBytes + 1`; if the returned slice exceeds `maxBytes`,
// we return errRequestTooLarge so the caller can answer 413 cleanly.
func (h *Handler) readBody(r *http.Request, in Ingress) (body []byte, modelID string, isStream bool, err error) {
	maxBytes := h.payloadCaptureConfig().MaxRequestBytes
	if maxBytes <= 0 {
		maxBytes = payloadcapture.DefaultMaxRequestBytes
	}
	body, err = io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, "", false, fmt.Errorf("failed to read request body")
	}
	if int64(len(body)) > maxBytes {
		return nil, "", false, errRequestTooLarge
	}

	modelID, isStream, err = ExtractIngressModel(in, r, body)
	if err != nil {
		return nil, "", false, err
	}

	if modelID == "" {
		return nil, "", false, fmt.Errorf("model is required")
	}

	if modelID == "auto" && typology.KindFromWireShape(in.WireShape) == typology.EndpointKindEmbeddings {
		return nil, "", false, fmt.Errorf("model \"auto\" is not supported for embeddings")
	}

	return body, modelID, isStream, nil
}

// buildProviderRequest assembles the [provcore.Request] that
// fetchUpstream hands to the executor. It is split out so the wiring
// between the inbound http.Request and the provider adapter (in
// particular the per-format header allowlist driven by
// Request.Headers) is unit-testable without spinning up the full
// adapter stack — see TestBuildProviderRequest.
//
// Inbound headers are forwarded as-is; the spec adapter applies its
// own allowlist (general + per-format) on top, so security-sensitive
// headers (Authorization, Cookie, x-api-key, Nexus-internal) never
// reach the upstream — they are filtered there. Provider-specific
// betas (anthropic-beta, openai-beta, x-goog-user-project, ...) MUST
// reach the upstream for newer Anthropic / OpenAI / Google features
// (e.g. context_management, prompt caching) to be accepted.
func buildProviderRequest(r *http.Request, in Ingress, body []byte, isStream bool, maxResp int64) provcore.Request {
	var headers http.Header
	if r != nil {
		headers = r.Header
	}
	return provcore.Request{
		WireShape:        in.WireShape,
		BodyFormat:       in.BodyFormat,
		Body:             body,
		Headers:          headers,
		Stream:           isStream,
		MaxResponseBytes: maxResp,
	}
}

// runRequestHooks executes request-stage hooks. Returns:
//   - rewrittenBody: non-nil when a hook produced a Modify decision and
//     the traffic adapter successfully rewrote the request body with the
//     redacted content. The caller should forward these bytes upstream
//     instead of the original body. Nil when no rewrite was performed.
//   - pipelineResult: the CompliancePipelineResult from the pipeline, or nil
//     when no pipeline was built (no hooks configured). The caller uses this
//     to emit X-Nexus-Hook on the response. On the reject path the
//     header is written inside this function before the error response.
//   - rejected: true when the pipeline rejected the request and an
//     error response has already been written to w.
func (h *Handler) runRequestHooks(r *http.Request, w http.ResponseWriter, rec *audit.Record, requestID string, body []byte, target routingcore.RoutingTarget, in Ingress, logger *slog.Logger) (rewrittenBody []byte, pipelineResult *hookcore.CompliancePipelineResult, rejected bool) {
	// Pick the traffic adapter matching the detected ingress body
	// format so content extraction + rewrite run through the right
	// schema parser. For OpenAI-compat ingress this is the classic
	// `openai-compat`; for Anthropic ingress it is `anthropic`; etc.
	// Per SDD E28-s5 §4: hook rewrite runs on the ingress-format
	// bytes, so the adapter here MUST match the ingress format, not
	// the upstream provider format.
	trafficAdapter := h.trafficAdapterFor(in.BodyFormat)
	ingressFormat := string(in.BodyFormat)

	input := &hookcore.HookInput{
		RequestID:      requestID,
		Stage:          "request",
		Normalized:     h.extractRequestContentForHooks(r.Context(), trafficAdapter, ingressFormat, body, r.URL.Path, logger),
		IngressType:    "AI_GATEWAY",
		Method:         r.Method,
		Path:           r.URL.Path,
		ContentType:    r.Header.Get("Content-Type"),
		BodySize:       int64(len(body)),
		SourceIP:       middleware.ClientIP(r),
		ProviderRegion: target.Region,
		// Hook configs (`targetModels: [...]`) are authored by admins
		// using customer-facing codes ("gpt-4o"), not internal UUIDs.
		Model: target.ModelCode,
	}

	// Populate endpoint/modality context on the hook input so BuildPipeline
	// can gate Class-A text hooks out of non-text endpoints. At request
	// stage the endpoint type is known from the Ingress descriptor; default
	// to text modality (all current AI-gateway traffic is text-in).
	input.EndpointType = typology.KindFromWireShape(in.WireShape)
	input.InputModality = []hookcore.Modality{hookcore.ModalityText}

	resolver := h.deps.HookConfigCache.Resolver(r.Context())
	pipeline, err := resolver.BuildPipeline(
		"request", "AI_GATEWAY",
		input.EndpointType,
		input.InputModality,
		5*time.Second, 15*time.Second, false, logger,
	)
	if err != nil {
		logger.Error("failed to build request hook pipeline", "error", err)
		h.writeError(w, rec, http.StatusInternalServerError, "hook pipeline error")
		return nil, nil, true
	}
	if pipeline == nil {
		return nil, nil, false
	}
	pipeline.SetAllowModify(true)
	pipeline.SetClearSoftOnApprove(true)

	hookResult := pipeline.Execute(r.Context(), input)

	rec.HookDecision = string(hookResult.Decision)
	rec.HookReason = hookResult.Reason
	rec.HookReasonCode = hookResult.ReasonCode
	rec.ComplianceTags = mergeTagSets(rec.ComplianceTags, hookResult.Tags)
	rec.BlockingRule = mapBlockingRule(hookResult.BlockingRule)
	rec.HooksPipeline = appendHookTrace(rec.HooksPipeline, "request", hookResult.HookResults)
	// Propagate TransformSpans + storage policy from the pipeline result
	// onto the audit Record. The audit writer applies storage policy to
	// the persisted NormalizedPayload at recordToMessage.
	rec.RequestTransformSpans = hookResult.TransformSpans
	rec.RequestStorageAction = string(hookResult.StorageAction)
	rec.RequestRedactRuleIDs = collectRuleIDs(hookResult.TransformSpans)
	// Stamp the storage-policy ReasonCode when the operator chose
	// "audit-only redact" or "drop content" — i.e. the storage path
	// diverged from the inflight path. Pure inflight-rewrite or pure
	// reject paths leave the hook's own reason code in place.
	if rec.HookReasonCode == "" {
		switch hookResult.StorageAction {
		case hookcore.StorageDropContent:
			rec.HookReasonCode = hookcore.ReasonStorageDroppedByPolicy
		case hookcore.StorageRedact:
			if hookResult.Decision == hookcore.Approve && len(hookResult.TransformSpans) > 0 {
				rec.HookReasonCode = hookcore.ReasonRedactStorageOnlyByPolicy
			}
		}
	}

	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordHookRequest(ingressFormat, "request", string(hookResult.Decision))
	}

	if hookResult.Decision == hookcore.RejectHard {
		// Write X-Nexus-Hook and via before writeError commits the status
		// line, so the client sees the marker even on hook-rejected 4xx responses.
		// X-Nexus-Mode is reserved as an empty position so an outer hop's
		// PrependChain keeps 1:1 alignment with X-Nexus-Via (AI Gateway has
		// no mode concept of its own).
		traffic.PrependVia(w.Header(), "ai-gateway")
		w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(hookResult)))
		w.Header().Set("X-Nexus-Mode", "")
		traffic.SetExposeHeaders(w.Header())
		h.writeError(w, rec, http.StatusForbidden, hookResult.Reason)
		return nil, hookResult, true
	}
	// HTTP 246 is a Nexus-specific status code for "soft reject" — the request
	// was flagged by compliance hooks but not hard-blocked. The response body
	// contains the hook's reason. Clients should treat 246 as a 200-class
	// success with a compliance warning. This convention is shared across
	// ai-gateway and compliance-proxy.
	if hookResult.Decision == hookcore.BlockSoft {
		traffic.PrependVia(w.Header(), "ai-gateway")
		w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(hookResult)))
		w.Header().Set("X-Nexus-Mode", "")
		traffic.SetExposeHeaders(w.Header())
		h.writeError(w, rec, 246, hookResult.Reason)
		return nil, hookResult, true
	}

	// MODIFY: push hook-rewritten content back onto the upstream wire.
	// When the adapter cannot reverse-encode (ErrRewriteUnsupported) we
	// forward the original body plus a warn log rather than failing —
	// that matches how the rest of the hook pipeline treats "Modify was
	// requested but not actionable here". Any other error (malformed,
	// unknown schema after Extract succeeded) indicates an internal
	// inconsistency and surfaces as 500.
	if hookResult.Decision == hookcore.Modify && len(hookResult.ModifiedContent) > 0 {
		rewritten, n, rErr := trafficAdapter.RewriteRequestBody(r.Context(), body, r.URL.Path, contentBlocksToNormalized(hookResult.ModifiedContent))
		switch {
		case errors.Is(rErr, traffic.ErrRewriteUnsupported):
			logger.Warn("hook produced Modify but adapter does not support rewrite; forwarding original body",
				slog.String("adapter", trafficAdapter.ID()),
				slog.String("path", r.URL.Path),
			)
			// Record the degraded path on the audit row.
			rec.HookReasonCode = hookcore.ReasonRedactInflightUnsupported
		case rErr != nil:
			logger.Error("hook request rewrite failed",
				slog.String("adapter", trafficAdapter.ID()),
				slog.String("path", r.URL.Path),
				slog.String("error", rErr.Error()),
			)
			h.writeError(w, rec, http.StatusInternalServerError, "request rewrite failed")
			return nil, hookResult, true
		default:
			rec.HookRewriteCount = n
			rec.HookRewritten = true
			return rewritten, hookResult, false
		}
	}
	return nil, hookResult, false
}

// parseRulePolicy unmarshals a routing rule's stored retryPolicy JSON
// into a *cfgpolicy.RetryPolicy. Returns nil for empty/null/invalid
// JSON; an unparseable value is logged but does not fail the request —
// the rule simply inherits the YAML default.
func (h *Handler) parseRulePolicy(raw json.RawMessage) *cfgpolicy.RetryPolicy {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var p cfgpolicy.RetryPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		if h != nil && h.deps != nil && h.deps.Logger != nil {
			h.deps.Logger.Warn("routing rule retryPolicy JSON unparseable; falling back to YAML default",
				slog.String("error", err.Error()),
				slog.String("raw", string(raw)),
			)
		}
		return nil
	}
	return &p
}

// effectiveRetryPolicy returns the policy the executor should honor for
// this request: the YAML default field-merged with the matched rule's
// per-rule override (if any). When the deps are missing a default
// (e.g. tests that did not wire RoutingDefaultPolicy), this falls back
// to cfgpolicy.DefaultRetryPolicy() so the executor never runs with a
// zero-valued policy (which would clamp MaxAttemptsPerTarget to 1 with
// nil RetryOn — "retry everything once" — instead of the documented
// platform defaults).
func (h *Handler) effectiveRetryPolicy(raw json.RawMessage, logger *slog.Logger) cfgpolicy.RetryPolicy {
	base := cfgpolicy.DefaultRetryPolicy()
	if h != nil && h.deps != nil {
		// Treat an all-zero RoutingDefaultPolicy as "not wired" — main.go
		// always populates it from cfg.Routing.DefaultRetryPolicy, which
		// the config loader merges against DefaultRetryPolicy() so a
		// real deployment always carries non-zero fields.
		dp := h.deps.RoutingDefaultPolicy
		if dp.MaxAttemptsPerTarget != 0 || dp.RetryOn != nil ||
			dp.BackoffInitial != 0 || dp.BackoffMax != 0 || dp.BackoffJitter != 0 {
			base = dp
		}
	}
	rule := h.parseRulePolicy(raw)
	policy := base.MergedWith(rule)
	policy.MaxAttemptsPerTarget = cfgpolicy.ClampMaxAttempts(policy.MaxAttemptsPerTarget)
	if logger != nil && rule != nil {
		logger.Debug("retry policy merged",
			slog.Int("maxAttemptsPerTarget", policy.MaxAttemptsPerTarget),
			slog.Any("retryOn", policy.RetryOn),
			slog.Duration("backoffInitial", policy.BackoffInitial),
			slog.Duration("backoffMax", policy.BackoffMax),
			slog.Float64("backoffJitter", policy.BackoffJitter),
		)
	}
	return policy
}
