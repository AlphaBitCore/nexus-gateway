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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/freshness"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/gemini"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	openairesponses "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/responses"
"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
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
	Models          ModelLookup                 // was accessed via DB.GetModel etc.
	VKAuth          VKAuthenticator             // was *vkauth.Authenticator
	RateLimiter     RateLimiter                 // was *ratelimit.Limiter
	CredManager     CredentialLookup            // was *credentials.Manager — used by models handler
	Router          RouteResolver               // typically *routingcore.Resolver
	Executor        executor.API                // upstream dispatch with retry/credential/health; production wires *executor.TargetExecutor
	HookConfigCache *pipeline.HookConfigCache // shared hook config cache
	ProviderReg     *provcore.Registry         // adapter-based provider registry
	HealthTracker   *store.HealthTracker        // stays concrete — used by background flush
	AuditWriter     *audit.Writer               // stays concrete
	Metrics         MetricsRecorder             // was *metrics.Recorder
	QuotaEngine     *quota.Engine               // hierarchical enforcement engine
	Cache           *cache.Cache                // response cache (nil = disabled)
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
	// paths to filter upstream → client headers and to stamp
	// x-nexus-aigw-allowlist-version on every response. Nil falls back
	// to the embedded defaults; production startup wires
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
	// pattern detector. When non-nil AND the matched routing rule sets
	// skip_time_sensitive=true in its response_cache_policy JSONB, the
	// pre-lookup classifier skips both L1 and L2 caches for messages that
	// match the detector's compiled rule set. Nil disables the check so
	// deployments that haven't pushed a freshness pattern config still work.
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
	// BudgetTracker enforces per-route daily embedding cost ceilings
	// (embeddingCostCeilingUsdPerDay in response_cache_policy.semantic).
	// Nil disables budget enforcement. Production wires *budget.Tracker;
	// tests may wire a stub.
	BudgetTracker BudgetTrackerAPI
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
			raw := strings.TrimSpace(r.Header.Get("x-nexus-aigw-body-format"))
			writeJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("unknown body format %q; supported: openai, anthropic, gemini, azure-openai, minimax, glm, deepseek", raw))
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

		if isStream && resolved.WireShape == typology.WireShapeOpenAIChat &&
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
			needsCanonicalization := false
			if h.deps.CanonicalBridge != nil {
				switch resolved.WireShape {
				case typology.WireShapeOpenAIChat:
					needsCanonicalization = resolved.BodyFormat != provcore.Format(primary.AdapterType)
				case typology.WireShapeOpenAIResponses:
					needsCanonicalization = !h.deps.CanonicalBridge.TargetNativelyServesResponsesAPI(provcore.Format(primary.AdapterType))
				}
			}
			if needsCanonicalization {
				canonBody, canonErr := h.deps.CanonicalBridge.IngressChatToCanonical(resolved.BodyFormat, prepReq.Body, prepReq.Target)
				if canonErr != nil {
					h.writeError(w, rec, http.StatusBadRequest, "canonicalize ingress body: "+canonErr.Error())
					return
				}
				prepReq.Body = canonBody
				prepReq.BodyFormat = provcore.FormatOpenAI
				if resolved.WireShape == typology.WireShapeOpenAIResponses {
					prepReq.WireShape = typology.WireShapeOpenAIChat
					// resolved is the per-request Ingress copy (see
					// applyHeaderOverride — value receiver). It's what
					// fetchUpstreamWithPreparedBody receives and what
					// buildProviderRequest pulls WireShape from to drive
					// the URL builder. Mutating `in` (the ServeProxy
					// parameter captured by the returned HandlerFunc
					// closure) would corrupt the next request on the
					// same handler — we mutate the per-request copy
					// only. resolved.BodyFormat stays FormatOpenAIResponses
					// so the egress reshape path still hits the
					// Responses-API encoder.
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

// authenticate validates the virtual key from the request.
func (h *Handler) authenticate(r *http.Request) (*vkauth.VKMeta, error) {
	if h.deps.VKAuth == nil {
		return nil, fmt.Errorf("VIRTUAL_KEY_MISSING: authenticator not configured")
	}
	return h.deps.VKAuth.Authenticate(r.Context(), r)
}

// writeAuthError writes an appropriate auth error response with machine-parseable codes.
func (h *Handler) writeAuthError(w http.ResponseWriter, rec *audit.Record, err error) {
	code := "AUTH_INVALID_KEY"
	hint := "Verify your virtual key is correct"
	switch {
	case errors.Is(err, vkauth.ErrMissing):
		code = "AUTH_KEY_MISSING"
		hint = "Include a virtual key via x-nexus-virtual-key header or Authorization: Bearer"
	case errors.Is(err, vkauth.ErrDisabled):
		code = "AUTH_KEY_DISABLED"
		hint = "This key has been disabled by an administrator"
	case errors.Is(err, vkauth.ErrExpired):
		code = "AUTH_KEY_EXPIRED"
		hint = "This key has expired; request a new one from your admin"
	}
	h.writeDetailedErr(w, rec, http.StatusUnauthorized, code, err.Error(), hint)
}

// checkRateLimit checks per-key rate limits. Sets Retry-After header on rejection.
//
// /v1/estimate compare requests use a dedicated per-VK bucket
// (checkCompareRateLimit, keyed by vkName + ":compare") so estimation
// traffic cannot exhaust the real-call quota and vice versa.
func (h *Handler) checkRateLimit(w http.ResponseWriter, vkMeta *vkauth.VKMeta) error {
	if vkMeta.RateLimitRpm == nil || h.deps.RateLimiter == nil {
		return nil
	}
	allowed, retryAfter := h.deps.RateLimiter.Allow(vkMeta.Name, *vkMeta.RateLimitRpm, 60_000)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		return fmt.Errorf("rate limit exceeded")
	}
	return nil
}

// compareEndpointRateLimitDefault is the per-VK fallback when
// CompareEndpointRateLimitRpm is NULL.
const compareEndpointRateLimitDefault = 30

func (h *Handler) checkCompareRateLimit(w http.ResponseWriter, vkMeta *vkauth.VKMeta) error {
	if h.deps.RateLimiter == nil {
		return nil
	}
	limit := compareEndpointRateLimitDefault
	if vkMeta.CompareEndpointRateLimitRpm != nil {
		limit = *vkMeta.CompareEndpointRateLimitRpm
	}
	if limit <= 0 {
		return nil
	}
	allowed, retryAfter := h.deps.RateLimiter.Allow(vkMeta.Name+":compare", limit, 60_000)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		return fmt.Errorf("compare-endpoint rate limit exceeded")
	}
	return nil
}

// buildRequestContext constructs the L3 request context. It performs
// exactly one normcore.Registry.Normalize call per request (skipped for
// empty bodies) and packages the canonical NormalizedPayload alongside
// identity, endpoint, headers, and raw body into an immutable
// *requestcontext.RequestContext. Downstream L4 consumers (routing,
// hooks, audit) read from this single artefact instead of re-parsing
// raw bytes.
//
// Normalize errors are swallowed: the canonical payload remains nil and
// routing/hooks fall back to their nil-Request behaviour. A malformed
// or unrecognised body must not block the request — the routing layer
// makes its own non-smart fallback.
func (h *Handler) buildRequestContext(r *http.Request, vkMeta *vkauth.VKMeta, body []byte, ingressFormat provcore.Format, modelID, endpointType string) *requestcontext.RequestContext {
	var canonical *normcore.NormalizedPayload
	if h.deps.NormalizeRegistry != nil && len(body) > 0 {
		payload, err := h.deps.NormalizeRegistry.Normalize(r.Context(), body, normcore.Meta{
			AdapterType:  string(ingressFormat),
			Model:        modelID,
			ContentType:  r.Header.Get("Content-Type"),
			Direction:    normcore.DirectionRequest,
			EndpointPath: r.URL.Path,
		})
		if err == nil {
			canonical = &payload
		}
	}
	return requestcontext.NewBuilder().
		WithIdentity(vkMeta).
		WithNormalized(canonical).
		WithEndpoint(endpointType).
		WithHeaders(r.Header).
		WithRawBody(body).
		Build()
}

// resolveRoute runs the routing engine via Router.ResolveTargets, returning a
// flat RouteResult with targets already health-ranked. The router input is
// built from the RequestContext; the canonical Normalized payload flows
// through rctx.Request so smart routing can inspect the user prompt.
//
// For embeddings requests, the raw canonical body is also parsed into an
// EmbeddingRequest so the capability pre-filter can apply before target
// dispatch.
func (h *Handler) resolveRoute(ctx context.Context, rctxFull *requestcontext.RequestContext, modelID string, endpointKind typology.EndpointKind) (*routingcore.RouteResult, error) {
	var vkCtx *routingcore.VKContext
	if vkMeta := rctxFull.Identity(); vkMeta != nil {
		orgPath := buildOrgPath(vkMeta.OrganizationID, h.orgParents())
		vkCtx = &routingcore.VKContext{
			ID:               vkMeta.ID,
			Name:             vkMeta.Name,
			OrganizationID:   vkMeta.OrganizationID,
			OrganizationPath: orgPath,
			ProjectID:        vkMeta.ProjectID,
			SourceApp:        vkMeta.SourceApp,
			AllowedModels:    vkMeta.AllowedModels,
		}
	}
	rctx := &routingcore.RoutingContext{
		RequestedModel: routingcore.RequestedModel{ID: modelID},
		EndpointType:   endpointKind,
		VirtualKey:     vkCtx,
		Headers:        routingcore.NewSafeHeaders(rctxFull.Headers()),
		Request:        rctxFull.Normalized(),
	}

	// Embeddings capability pre-filter: parse the embedding request
	// parameters from the canonical body so the router can apply model
	// compatibility rules.
	if rctx.EndpointType == typology.EndpointKindEmbeddings {
		body := rctxFull.RawBody()
		rctx.EmbeddingRequest = parseEmbeddingRequest(body)
	}

	return h.deps.Router.ResolveTargets(ctx, rctx)
}

// parseEmbeddingRequest extracts the embedding request parameters from
// the canonical body (OpenAI-compatible shape). All fields are optional;
// absent fields are left at zero values (nil pointers / empty strings).
func parseEmbeddingRequest(body []byte) *routingcore.EmbeddingRequestParams {
	if len(body) == 0 {
		return &routingcore.EmbeddingRequestParams{BatchSize: 1}
	}
	req := &routingcore.EmbeddingRequestParams{}
	if d := gjson.GetBytes(body, "dimensions"); d.Exists() {
		v := int(d.Int())
		req.Dimensions = &v
	}
	if e := gjson.GetBytes(body, "encoding_format").String(); e != "" {
		req.EncodingFormat = e
	}
	req.InputType = canonicalext.Get(body, "cohere", "input_type").String()
	req.TaskType = canonicalext.Get(body, "gemini", "taskType").String()
	// BatchSize: input is either a string (single = 1) or array (len).
	if in := gjson.GetBytes(body, "input"); in.IsArray() {
		req.BatchSize = int(in.Get("#").Int())
		if req.BatchSize == 0 {
			req.BatchSize = 1
		}
	} else {
		req.BatchSize = 1
	}
	return req
}

func (h *Handler) orgParents() map[string]string {
	if h.deps == nil || h.deps.QuotaEngine == nil {
		return nil
	}
	return h.deps.QuotaEngine.OrgParents()
}

func buildOrgPath(orgID string, parents map[string]string) []string {
	if orgID == "" || len(parents) == 0 {
		return nil
	}
	path := make([]string, 0, 4)
	current := orgID
	for current != "" {
		parent := parents[current]
		if parent == "" {
			break
		}
		path = append(path, parent)
		current = parent
	}
	return path
}

// estimateTokens estimates the number of tokens in a request body using
// rune-based counting. CJK characters are single runes but often correspond
// to more tokens than ASCII bytes/4 would suggest; rune/3 gives a better
// cross-language approximation.
func estimateTokens(body []byte) int64 {
	runeCount := int64(utf8.RuneCount(body))
	est := runeCount / 3
	if est < 1 {
		est = 1
	}
	return est
}

// checkQuota performs quota enforcement and downgrade logic via the Engine.
// Returns pricing info and optional Decision.
// Sets rec.StatusCode and writes a response if quota is rejected (caller must
// check rec.StatusCode != 0).
func (h *Handler) checkQuota(r *http.Request, w http.ResponseWriter, rec *audit.Record, vkMeta *vkauth.VKMeta, result *routingcore.RouteResult, body []byte, requestedModel string) (float64, float64, *quota.Decision) {
	if vkMeta == nil {
		return 0, 0, nil
	}
	if h.deps.QuotaEngine == nil {
		return 0, 0, nil
	}

	firstTarget := result.Targets[0]
	var quotaInPrice, quotaOutPrice float64
	if h.deps.Models != nil {
		qModel, _ := h.deps.Models.GetModel(r.Context(), firstTarget.ModelID)
		if qModel != nil {
			if qModel.InputPricePM != nil {
				quotaInPrice = *qModel.InputPricePM
			}
			if qModel.OutputPricePM != nil {
				quotaOutPrice = *qModel.OutputPricePM
			}
		}
	}

	parsed := gjson.ParseBytes(body)
	maxTokens := parsed.Get("max_tokens").Int()
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	estimate := quota.CostEstimate{
		EstimatedInputTokens: estimateTokens(body),
		MaxOutputTokens:      maxTokens,
		InputPricePM:         quotaInPrice,
		OutputPricePM:        quotaOutPrice,
	}

	chain := quota.BuildCheckChain(vkMeta, h.deps.QuotaEngine.OrgParents())
	decision := h.deps.QuotaEngine.Check(r.Context(), chain, estimate, vkMeta)

	if !decision.Allowed {
		if decision.Action == "reject" {
			h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "QUOTA_EXCEEDED",
				decision.Message, "Check usage or request a quota increase")
			return quotaInPrice, quotaOutPrice, decision
		}
		if decision.Action == "downgrade" {
			modelIDs := make([]string, len(result.Targets))
			for i, t := range result.Targets {
				modelIDs[i] = t.ModelID
			}
			storePricing, pErr := h.deps.Models.FetchModelPricing(r.Context(), modelIDs)
			if pErr == nil {
				pricing := quota.TargetPricingFromStore(storePricing)
				// Use a budget based on remaining estimated cost.
				idx := quota.SelectCheapestIndex(pricing, estimate, estimate.EstimatedCost()*0.5)
				if idx >= 0 && idx < len(result.Targets) {
					selected := result.Targets[idx]
					result.Targets = []routingcore.RoutingTarget{selected}
					w.Header().Set("X-Nexus-Quota-Downgrade", "true")
					w.Header().Set("X-Nexus-Quota-Original-Model", requestedModel)
					decision.Allowed = true // Allow with downgraded model.
				} else {
					h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "QUOTA_EXCEEDED",
						"quota exceeded, no affordable model available",
						"All models exceed remaining budget; request a quota increase")
					return quotaInPrice, quotaOutPrice, decision
				}
			} else {
				h.writeError(w, rec, http.StatusTooManyRequests, decision.Message)
				return quotaInPrice, quotaOutPrice, decision
			}
		}
	} else if decision.Action == "notify-and-proceed" {
		w.Header().Set("X-Nexus-Quota-Warning", decision.Message)
	}

	// Emit VK-level quota visibility headers from the chain entry the
	// engine stamped during Check. Skip when no VK-level policy/override
	// matched so clients don't see misleading zeros.
	for _, lvl := range decision.Levels {
		if lvl.TargetType == "virtual_key" && lvl.HasLimit {
			w.Header().Set("X-Nexus-Quota-Used", fmt.Sprintf("%.2f", float64(lvl.CurrentCents)/100))
			w.Header().Set("X-Nexus-Quota-Limit", fmt.Sprintf("%.2f", float64(lvl.LimitCents)/100))
			break
		}
	}

	return quotaInPrice, quotaOutPrice, decision
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

// fetchUpstreamWithPreparedBody dispatches the request to upstream
// providers via TargetExecutor. The body for the primary target's
// first attempt has already been produced by Adapter.PrepareBody —
// used by the broker leader path so the adapter does not run
// PrepareBody twice (once for the cache key, once inside Execute).
//
// preparedBody MUST be the bytes Adapter.PrepareBody would return for
// routeResult.Targets[0]; preparedRewrites MUST be the matching
// rewrites slice. Pass nil/nil to fall back to plain Execute behaviour
// (which re-runs PrepareBody internally — idempotent, behaviour-
// equivalent, just one redundant µs-scale encode on the success path).
//
// Returns the execution result, the winning target, the retry count,
// and an error. On failure the error response is already written to w.
// The ingress descriptor's Endpoint and BodyFormat drive the adapter's
// passthrough/translate decision.
func (h *Handler) fetchUpstreamWithPreparedBody(r *http.Request, w http.ResponseWriter, rec *audit.Record, routeResult *routingcore.RouteResult, body []byte, isStream bool, in Ingress, preparedBody []byte, preparedRewrites []string, start time.Time, logger *slog.Logger) (*executor.ExecutionResult, routingcore.RoutingTarget, int, error) {
	pcCfg := h.payloadCaptureConfig()
	maxResp := pcCfg.MaxResponseBytes
	if maxResp <= 0 {
		maxResp = payloadcapture.DefaultMaxResponseBytes
	}
	policy := h.effectiveRetryPolicy(routeResult.RuleRetryPolicyJSON, logger)
	req := buildProviderRequest(r, in, body, isStream, maxResp)
	req.StickyKey = stickyKeyFromCtx(r.Context())
	var execResult *executor.ExecutionResult
	if preparedBody != nil {
		execResult = h.deps.Executor.ExecuteWithPreparedBody(r.Context(), routeResult.Targets, req, policy, preparedBody, preparedRewrites)
	} else {
		execResult = h.deps.Executor.Execute(r.Context(), routeResult.Targets, req, policy)
	}

	// Total attempt count for the response header. 1 means first-try success;
	// 2+ means at least one L2 retry or L3 failover happened. Defensive floor
	// at 1 — if the executor returned a result, at least one attempt ran.
	attempts := len(execResult.Attempts)
	if attempts < 1 {
		attempts = 1
	}

	// Set credential ID and name from the successful attempt for audit tracking.
	// rec.ModelName (requested side) was stamped right after readBody with the
	// literal client model string; only Routed* fields get set here.
	if n := len(execResult.Attempts); n > 0 {
		last := execResult.Attempts[n-1]
		rec.CredentialID = last.CredentialID
		rec.CredentialName = last.CredentialName
		rec.RoutedProviderID = last.Target.ProviderID
		rec.RoutedProviderName = last.Target.ProviderName
		rec.RoutedModelID = last.Target.ModelID
		rec.RoutedModelName = last.Target.ModelCode
		rec.TargetHost = upstreamHost(last.Target)
	}

	if execResult.Error != nil {
		// If the last attempt was rate-limited, propagate 429 so clients can
		// back off rather than receiving an opaque 502.
		if n := len(execResult.Attempts); n > 0 && execResult.Attempts[n-1].StatusCode == http.StatusTooManyRequests {
			h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "PROVIDER_RATE_LIMITED", "upstream rate limit exceeded", "")
			return nil, routingcore.RoutingTarget{}, attempts, execResult.Error
		}
		for i, a := range execResult.Attempts {
			if a.Error != "" {
				logger.Error("executor attempt failed", "attempt", i+1, "provider", a.Target.ProviderName, "model", a.Target.ModelCode, "reason", a.Error)
			}
		}
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "PROVIDER_UNAVAILABLE", "all upstream providers failed", "")
		return nil, routingcore.RoutingTarget{}, attempts, execResult.Error
	}

	target := execResult.Target

	// 4xx from upstream — write the envelope in the ingress format the
	// caller expects, not the upstream's native shape, so cross-format
	// clients (OpenAI SDK calling /v1/chat/completions that the gateway
	// routed to an Anthropic upstream) can parse the body. Also forward
	// the upstream's allowlisted response headers so debugging metadata
	// like x-request-id / retry-after reaches the client on the error
	// path, matching the success-path forwarding at writeForwardedResponseHeaders.
	if execResult.StatusCode >= 400 {
		rec.StatusCode = execResult.StatusCode
		rec.ErrorCode = "PROVIDER_ERROR"
		rec.ErrorReason = extractProviderErrorMessage(execResult.Body, execResult.StatusCode)
		rec.RoutedProviderID = target.ProviderID
		rec.RoutedProviderName = target.ProviderName
		rec.RoutedModelID = target.ModelID
		rec.RoutedModelName = target.ModelCode
		rec.TargetHost = upstreamHost(target)
		// #41: stamp upstream URL on the error path too — same source as
		// the success path below. ToMessage's firstNonEmptyStr fallback
		// covers synthetic transport failures that never reached the
		// network (empty TargetPath → falls back to rec.Path).
		rec.TargetMethod = execResult.TargetMethod
		rec.TargetPath = execResult.TargetPath
		rec.LatencyMs = int(time.Since(start).Milliseconds())

		ingress, _ := IngressFromContext(r.Context())
		upstreamFormat := provcore.Format(target.AdapterType)

		// Stale Gemini cachedContent invalidation. Gemini returns 403
		// "CachedContent not found (or permission denied)" when our
		// Redis-cached name points to content the upstream has already
		// evicted. Fire the per-request invalidate hook (set by
		// ServeProxy on cache HIT) so the next request regenerates
		// instead of looping on the dead ref. The TTL fix in
		// geminicache/manager.go shrinks the stale window but cannot
		// fully eliminate it — Gemini's eviction is best-effort.
		if execResult.StatusCode == http.StatusForbidden &&
			upstreamFormat == provcore.FormatGemini &&
			geminicacheStaleRefError(execResult.Body) {
			if invalidate := GeminiCacheInvalidateFromContext(r.Context()); invalidate != nil {
				invalidate()
				logger.Warn("geminicache: upstream reported stale cachedContent — invalidated Redis entry",
					"provider", target.ProviderName)
			}
		}

		errBody := execResult.Body
		if execResult.ProviderError != nil {
			errBody = envelope.EncodeErrorEnvelopeForIngress(ingress.BodyFormat, upstreamFormat, execResult.ProviderError)
		}

		// Bug #40: stamp the upstream error body to the audit Record so
		// it lands in traffic_event.payloads.response_body. Previously
		// only ErrorReason (extracted message string) was captured —
		// the full body (with provider stack trace, request ID, etc.)
		// was discarded, making 4xx/5xx triage from the Traffic drawer
		// impossible. Mirrors the success-path stamp at line 2176
		// (subject to the same StoreResponseBody payload-capture
		// config so administrators can opt out for compliance reasons).
		pcCfgErr := h.payloadCaptureConfig()
		if len(errBody) > 0 && pcCfgErr.StoreResponseBody {
			rec.ResponseBody = errBody
			rec.ResponseContentType = "application/json"
		}

		writeForwardedResponseHeaders(w, h.deps.Allowlist, upstreamFormat, execResult.Headers, false)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(execResult.StatusCode)
		_, _ = w.Write(errBody)
		return nil, target, attempts, fmt.Errorf("upstream %dxx", execResult.StatusCode/100)
	}

	rec.RoutedProviderID = target.ProviderID
	rec.RoutedProviderName = target.ProviderName
	rec.RoutedModelID = target.ModelID
	rec.RoutedModelName = target.ModelCode
	rec.TargetHost = upstreamHost(target)
	// #41: stamp the actual upstream URL the adapter dispatched to
	// (e.g. "/v1/messages" for the Anthropic side of an OpenAI →
	// Anthropic cross-format call). On synthetic transport errors that
	// never reached the network, ExecutionResult.TargetPath is empty
	// and ToMessage's firstNonEmptyStr fallback substitutes rec.Path.
	rec.TargetMethod = execResult.TargetMethod
	rec.TargetPath = execResult.TargetPath

	return execResult, target, attempts, nil
}

func upstreamHost(target routingcore.RoutingTarget) string {
	if target.BaseURL == "" {
		return target.ProviderName
	}
	u, err := url.Parse(target.BaseURL)
	if err != nil || u.Host == "" {
		return target.ProviderName
	}
	return u.Host
}

// appendHookTrace converts a pipeline.Execute result's per-hook records into
// audit.HookExecRecord entries and grows the rec slice. Stage names line up
// with the pipeline registration ("request" / "response" / "connection") so
// dashboards can group / filter without re-deriving the bucket.
func appendHookTrace(existing []audit.HookExecRecord, stage string, results []hookcore.HookResult) []audit.HookExecRecord {
	if len(results) == 0 {
		return existing
	}
	out := existing
	for _, r := range results {
		out = append(out, audit.HookExecRecord{
			Stage:      stage,
			Order:      r.Order,
			HookID:     r.HookID,
			Name:       r.HookName,
			Decision:   string(r.Decision),
			Reason:     r.Reason,
			ReasonCode: r.ReasonCode,
			LatencyMs:  r.LatencyMs,
			Error:      r.Error,
		})
	}
	return out
}

// extractRequestContentForHooks pulls the canonical request content
// blocks out of the ingress body via the format-aware traffic
// adapter. Failures here are non-fatal — hook input is best-effort
// and the pipeline is allowed to run with partial or empty data.
//
// The returned blocks are all text segments in the adapter's
// extraction order. Role is left empty because NormalizedContent does
// not carry role information; the hook layer treats role-less blocks
// as caller input, which is the correct behaviour for request-stage
// hooks across all 9 formats.
func (h *Handler) extractRequestContentForHooks(ctx context.Context, adapter traffic.Adapter, ingressFormat string, body []byte, path string, logger *slog.Logger) *normcore.NormalizedPayload {
	if adapter == nil || len(body) == 0 {
		if h != nil && h.deps != nil && h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "request", "skipped")
		}
		return nil
	}
	extracted, err := adapter.ExtractRequest(ctx, body, path)
	if err != nil {
		logger.Debug("request extract for hooks failed",
			slog.String("adapter", adapter.ID()),
			slog.String("path", path),
			slog.String("error", err.Error()),
		)
		if h != nil && h.deps != nil && h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "request", "error")
		}
		return nil
	}
	if h != nil && h.deps != nil && h.deps.Metrics != nil {
		h.deps.Metrics.RecordTrafficExtract(ingressFormat, "request", "success")
	}
	return hookcore.PayloadFromTextSegments(extracted.Segments)
}

// extractResponseForHooks pulls the canonical content blocks, model
// name, and finish reason out of a non-streaming response body via the
// active traffic adapter. Failures here are non-fatal — hook input is
// best-effort and the pipeline is allowed to run with partial data.
func (h *Handler) extractResponseForHooks(ctx context.Context, adapter traffic.Adapter, ingressFormat string, body []byte, path string, logger *slog.Logger) (*normcore.NormalizedPayload, string, string) {
	if adapter == nil || len(body) == 0 {
		if h != nil && h.deps != nil && h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "response", "skipped")
		}
		return nil, "", ""
	}
	extracted, err := adapter.ExtractResponse(ctx, body, path)
	if err != nil {
		logger.Debug("response extract for hooks failed",
			slog.String("adapter", adapter.ID()),
			slog.String("path", path),
			slog.String("error", err.Error()),
		)
		if h != nil && h.deps != nil && h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "response", "error")
		}
		return nil, "", ""
	}
	if h != nil && h.deps != nil && h.deps.Metrics != nil {
		h.deps.Metrics.RecordTrafficExtract(ingressFormat, "response", "success")
	}
	model := ""
	if extracted.Metadata != nil {
		model = extracted.Metadata["model"]
	}
	return hookcore.PayloadFromTextSegments(extracted.Segments), model, ""
}

// usageInt returns the pointer's dereferenced value, or 0 when nil.
func usageInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// aigwHookOutcomeFromResult converts a request-side CompliancePipelineResult
// into a HookOutcomeInput suitable for traffic.FormatHookOutcome. The mapping
// follows spec §4.5:
//   - RejectHard / BlockSoft → Rejected = hookName, RejectReason = reasonCode (or reason)
//   - Modify → appended to Passed + Transformed = true
//   - Approve / Abstain → appended to Passed
//   - Any reject halts iteration (later hooks are not reported).
//
// Returns an empty HookOutcomeInput (→ "none") when r is nil or has no hook
// results.
func aigwHookOutcomeFromResult(r *hookcore.CompliancePipelineResult) traffic.HookOutcomeInput {
	if r == nil || len(r.HookResults) == 0 {
		return traffic.HookOutcomeInput{}
	}
	in := traffic.HookOutcomeInput{}
	for _, hr := range r.HookResults {
		switch hr.Decision {
		case hookcore.RejectHard, hookcore.BlockSoft:
			// Reject halts the pipeline: discard any previously-accumulated
			// Passed hooks and return only the reject attribution (spec §4.5).
			reason := hr.ReasonCode
			if reason == "" {
				reason = hr.Reason
			}
			return traffic.HookOutcomeInput{
				Rejected:     hr.HookName,
				RejectReason: reason,
			}
		case hookcore.Modify:
			in.Passed = append(in.Passed, hr.HookName)
			in.Transformed = true
		default:
			in.Passed = append(in.Passed, hr.HookName)
		}
	}
	return in
}

// allowlistVersionFromDeps returns the active forward-header
// allowlist hash for cache-key derivation, or "" when no allowlist
// is wired (test paths). Folding it into the key invalidates entries
// whose UpstreamHeaders captured under a different YAML config.
func allowlistVersionFromDeps(d *Deps) string {
	if d == nil || d.Allowlist == nil {
		return ""
	}
	return d.Allowlist.Hash()
}

// writeForwardedResponseHeaders applies the resolved response-side
// forward-header allowlist to upstream headers and writes the
// permitted set onto w. Per-request headers (e.g. `x-request-id`,
// `x-ratelimit-*-tokens`, `openai-processing-ms`) are stripped on
// cache HIT (`isCacheHit == true`); replaying a stale per-request
// value is worse than not surfacing it.
//
// MUST be called BEFORE [Handler.setResponseHeaders] /
// [Handler.setResponseHeadersStream] so Nexus's own
// `x-nexus-aigw-*` stamps overwrite any conflicting upstream value
// (FR-FH7 "Nexus wins on conflict").
//
// Safe with a nil allowlist (falls back to embedded defaults via
// provcore.FilterResponseHeaders) and with an empty / nil src.
//
// Prefers the live atomic snapshot from forwardheader.Active (set once
// from yaml at boot) so the response writer reads the same allowlist
// every request without re-resolving. The supplied parameter stays as
// the test/early-startup fallback.
func writeForwardedResponseHeaders(w http.ResponseWriter, allowlist *forwardheader.Resolved, format provcore.Format, src http.Header, isCacheHit bool) {
	if len(src) == 0 {
		return
	}
	if live := forwardheader.Active(); live != nil {
		allowlist = live
	}
	filtered := provdispatch.FilterResponseHeaders(allowlist, format, src, isCacheHit)
	for k, vs := range filtered {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// setResponseHeaders writes standard Nexus response headers for non-streaming
// responses. It initialises the x-nexus-via chain and stamps latency.
//
// `attempts` is the total upstream attempt count for this request — 1 means
// first-try success, 2+ means at least one L2 retry or L3 failover happened.
// The header is emitted on every response (no `> 0` gate) so observers can
// always tell whether failover engaged.
func (h *Handler) setResponseHeaders(w http.ResponseWriter, rec *audit.Record, target routingcore.RoutingTarget, result *routingcore.RouteResult, start time.Time, attempts int) {
	traffic.PrependVia(w.Header(), "ai-gateway")
	// X-Nexus-Mode is reserved as an empty position so an outer hop
	// (agent, compliance-proxy) preserves 1:1 alignment with X-Nexus-Via
	// when it prepends its own mode value. AI Gateway has no mode concept.
	w.Header().Set("X-Nexus-Mode", "")
	// Customer-facing model identifier — the same string the caller
	// sent in `{"model": "..."}`. Internal UUIDs stay out of the
	// headers; correlation against the catalog uses the code.
	if attempts < 1 {
		attempts = 1 // defensive — should never be 0 if we reached this code path
	}
	w.Header().Set("X-Nexus-Attempts", strconv.Itoa(attempts))
	if result.Substituted {
		w.Header().Set("X-Nexus-Routed-Model", target.ModelCode)
		w.Header().Set("X-Nexus-Routed-Provider", target.ProviderName)
	}
	// Server-Timing (RFC 8674) exposes gateway/upstream latency
	// breakdowns. Native browser DevTools support; comma-separated tokens.
	parts := make([]string, 0, 3)
	gwTotalMs := time.Since(start).Milliseconds()
	if rec.UpstreamTotalMs != nil {
		gwOverhead := gwTotalMs - int64(*rec.UpstreamTotalMs)
		if gwOverhead < 0 {
			gwOverhead = 0
		}
		parts = append(parts, fmt.Sprintf("gw;dur=%d", gwOverhead))
		if rec.UpstreamTtfbMs != nil {
			parts = append(parts, fmt.Sprintf("upstream-ttfb;dur=%d", *rec.UpstreamTtfbMs))
		}
		parts = append(parts, fmt.Sprintf("upstream-total;dur=%d", *rec.UpstreamTotalMs))
	} else {
		parts = append(parts, fmt.Sprintf("gw;dur=%d", gwTotalMs))
	}
	w.Header().Set("Server-Timing", strings.Join(parts, ", "))
}

// setResponseHeadersStream writes standard Nexus response headers for
// streaming (SSE) responses. It initialises the x-nexus-via chain but
// omits latency (per spec §5 — latency is meaningless on a streaming
// response where the last byte arrives long after headers are sent).
//
// `attempts` is the total upstream attempt count for this request — 1 means
// first-try success, 2+ means at least one L2 retry or L3 failover happened.
// The header is emitted on every response (no `> 0` gate) so observers can
// always tell whether failover engaged.
func (h *Handler) setResponseHeadersStream(w http.ResponseWriter, rec *audit.Record, target routingcore.RoutingTarget, result *routingcore.RouteResult, attempts int) {
	traffic.PrependVia(w.Header(), "ai-gateway")
	// Reserve X-Nexus-Mode position for outer-hop 1:1 alignment with the
	// via chain — same rationale as setResponseHeaders above.
	w.Header().Set("X-Nexus-Mode", "")
	if attempts < 1 {
		attempts = 1 // defensive — should never be 0 if we reached this code path
	}
	w.Header().Set("X-Nexus-Attempts", strconv.Itoa(attempts))
	if result.Substituted {
		w.Header().Set("X-Nexus-Routed-Model", target.ModelCode)
		w.Header().Set("X-Nexus-Routed-Provider", target.ProviderName)
	}
}

// streamCaptureTee wraps an http.ResponseWriter so streamed bytes are
// mirrored into an in-memory buffer for audit. The buffer is bounded
// by hardCap (sourced from spill.perObjectCap, default 256 MiB) — an
// OOM guard, NOT the inline-vs-spill threshold. Bytes past hardCap
// continue to reach the client unchanged but the buffer no longer
// grows and Truncated flips true so traffic_event_payload.response_truncated
// reflects reality. The end-of-stream EmitBody call decides inline vs
// spill against the runtime MaxInlineBodyBytes. Flush() / Unwrap()
// are forwarded so SSE chunked encoding and http.ResponseController
// still work through the wrapper.
type streamCaptureTee struct {
	http.ResponseWriter
	hardCap int64
	written int64
	buf     []byte
	tail    bool // true once we have stopped buffering past hardCap
}

func newStreamCaptureTee(w http.ResponseWriter, hardCap int64) *streamCaptureTee {
	if hardCap < 0 {
		hardCap = 0
	}
	return &streamCaptureTee{
		ResponseWriter: w,
		hardCap:        hardCap,
		buf:            make([]byte, 0, minInt64(hardCap, 16*1024)),
	}
}

func (w *streamCaptureTee) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && !w.tail {
		writable := w.hardCap - w.written
		switch {
		case writable <= 0:
			w.tail = true
		case int64(n) > writable:
			w.buf = append(w.buf, p[:int(writable)]...)
			w.written = w.hardCap
			w.tail = true
		default:
			w.buf = append(w.buf, p[:n]...)
			w.written += int64(n)
		}
	}
	return n, err
}

func (w *streamCaptureTee) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *streamCaptureTee) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *streamCaptureTee) captured() []byte         { return w.buf }
func (w *streamCaptureTee) truncatedBeyondCap() bool { return w.tail }

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// handleNonStream handles non-streaming JSON responses from the adapter.
// The adapter's Body is in the request's BodyFormat; Usage is reported
// separately on the ExecutionResult. Story s2 populates Usage from
// per-format SchemaCodec.DecodeResponse.
//
// This is the direct (non-broker) path used when cache is disabled or
// the broker registry is not wired. The broker MISS path uses
// handleNonStreamWithSubscription instead, which shares the cache write
// with the broker leader.
func (h *Handler) handleNonStream(r *http.Request, w http.ResponseWriter, rec *audit.Record, result *executor.ExecutionResult, target routingcore.RoutingTarget, quotaInPrice, quotaOutPrice float64, quotaDecision *quota.Decision, endpointType, requestID string, start time.Time, logger *slog.Logger) {
	respBody := result.Body
	ingress, _ := IngressFromContext(r.Context())
	// Reverse-decode the upstream's Responses-shape body back into
	// canonical chat-completions JSON before the standard ingress reshape
	// path runs. This is the inverse of the request-side
	// EncodeResponsesRequest applied earlier in the pipeline. On decode
	// failure, surface a 502 since the client expected chat-completions
	// shape; falling back to the raw Responses bytes would break SDKs.
	if ResponsesUpgradeFromContext(r.Context()) && len(respBody) > 0 {
		canonicalBody, _, dErr := openairesponses.DecodeResponsesResponse(respBody)
		if dErr != nil {
			logger.Error("reverse-decode of /v1/responses body failed",
				"error", dErr.Error())
			h.writeError(w, rec, http.StatusBadGateway,
				"upgraded /v1/responses body could not be reverse-decoded to chat-completions shape: "+dErr.Error())
			return
		}
		respBody = canonicalBody
	}
	// Re-shape the upstream response into the ingress format. This fires
	// for chat-completions ingress AND for Responses ingress (the bridge's
	// FormatOpenAIResponses case calls EncodeResponsesResponse to wrap
	// canonical → output[]).
	// Without the EndpointResponsesAPI branch, cross-format Responses
	// responses (e.g. Anthropic upstream) return Anthropic-shape JSON
	// to the client instead of Responses-shape — breaks every OpenAI
	// Responses-API SDK consumer.
	//
	// CRITICAL: Native Responses passthrough (e.g. /v1/responses ingress
	// routed to an OpenAI target) MUST skip the reshape. The upstream
	// body is already in Responses-shape; passing it through
	// EncodeResponsesResponse — which expects a canonical chat.completion
	// body and synthesises an output[] from it — would double-encode and
	// strip the original `output[].content[].text` (the prod symptom
	// was `output[].text` empty across every native OpenAI Responses
	// model). Gate the reshape on cross-format only.
	if h.deps.CanonicalBridge != nil &&
		(ingress.WireShape == typology.WireShapeOpenAIChat ||
			(ingress.WireShape == typology.WireShapeOpenAIResponses &&
				!h.deps.CanonicalBridge.TargetNativelyServesResponsesAPI(provcore.Format(target.AdapterType)))) {
		shaped, rerr := h.deps.CanonicalBridge.ResponseCanonicalToIngress(ingress.BodyFormat, respBody)
		if rerr != nil {
			logger.Error("response hub reshape failed", "error", rerr)
			h.writeError(w, rec, http.StatusBadGateway, "upstream response could not be reshaped for ingress format")
			return
		}
		respBody = shaped
	}

	usage := metrics.Usage{
		PromptTokens:     int64(usageInt(result.Usage.PromptTokens)),
		CompletionTokens: int64(usageInt(result.Usage.CompletionTokens)),
		TotalTokens:      int64(usageInt(result.Usage.TotalTokens)),
	}
	rec.PromptTokens = usage.PromptTokens
	rec.CompletionTokens = usage.CompletionTokens
	rec.TotalTokens = usage.TotalTokens
	// Use the per-endpoint cost formula registry so embeddings (prompt
	// only) and other typologies are priced correctly without a switch.
	{
		units := estimator.BillableUnits{
			PromptTokens:     int(rec.PromptTokens),
			CompletionTokens: int(rec.CompletionTokens),
		}
		cost := estimator.Lookup(rec.EndpointType)(units, metrics.ModelPrices{
			InputUsdPerM:  &quotaInPrice,
			OutputUsdPerM: &quotaOutPrice,
		})
		rec.EstimatedCostUsd = cost.Total
	}
	// Stamp ProviderCacheStatus from upstream usage cache fields.
	// Skip if already set (gateway-served paths stamp NA before reaching here).
	if rec.ProviderCacheStatus == "" {
		rec.ProviderCacheStatus = audit.ClassifyProviderCache(result.Usage.CacheReadTokens, result.Usage.CacheCreationTokens)
	}
	if result.Usage.CacheReadTokens != nil {
		rec.CacheReadTokens = int64(*result.Usage.CacheReadTokens)
	}
	if result.Usage.CacheCreationTokens != nil {
		rec.CacheCreationTokens = int64(*result.Usage.CacheCreationTokens)
	}
	// reasoning_tokens. Populated by codec packages when the provider
	// reports it (Gemini thoughtsTokenCount, OpenAI-compat
	// completion_tokens_details.reasoning_tokens, Anthropic
	// thinking_tokens). Absent = 0 → NULL via omitempty / mq schema.
	if result.Usage.ReasoningTokens != nil {
		rec.ReasoningTokens = int64(*result.Usage.ReasoningTokens)
	}
	// reasoning_cost_usd — subset of EstimatedCostUsd attributable to
	// ReasoningTokens (billed at the output rate). Stamped here BEFORE
	// computeCacheCosts which may overwrite EstimatedCostUsd using
	// provider_pricing; the ratio is preserved because both numerator and
	// denominator use the same output price.
	if rec.ReasoningTokens > 0 && quotaOutPrice > 0 {
		rec.ReasoningCostUsd = float64(rec.ReasoningTokens) * quotaOutPrice / 1_000_000
	}
	h.computeCacheCosts(rec, target)
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.TotalTokens > 0 {
		rec.UsageExtractionStatus = "ok"
	} else {
		rec.UsageExtractionStatus = "parse_failed"
	}
	// Update embedding dimension from the live response body.
	// Request-side fields were pre-stamped in ServeProxy; only the
	// response dimension is available here.
	if rec.EndpointType == "embeddings" {
		rec.Metadata = updateEmbeddingDimension(rec.Metadata, respBody)
	}

	// Response hooks. Response content, model, and finish reason are
	// derived from the response body via the ingress-aware traffic
	// adapter. The body has already been reshaped to the ingress wire
	// format when CanonicalBridge is active (DecodeResponse yields
	// canonical OpenAI, then ResponseCanonicalToIngress runs above).
	//
	// bypassHooks: when the resolved passthrough has BypassHooks active,
	// skip the response-stage pipeline build + execute. The request-stage
	// skip already stamped rec.HookDecision = "BYPASSED"; the response
	// stage just leaves rec.ResponseHookDecision empty.
	bypassResponseHooks := false
	if resolved := requestcontext.ResolvedFrom(r.Context()); resolved != nil {
		if pt := resolved.Passthrough(); pt.AnyBypassActive() && pt.BypassHooks {
			bypassResponseHooks = true
		}
	}
	if !bypassResponseHooks {
		extractor := h.trafficAdapterFor(ingress.BodyFormat)
		ingressFormat := string(ingress.BodyFormat)
		respContent, respModel, respFinish := h.extractResponseForHooks(r.Context(), extractor, ingressFormat, respBody, r.URL.Path, logger)
		epType := typology.KindFromWireShape(ingress.WireShape)
		respInput := &hookcore.HookInput{
			RequestID:      requestID,
			Stage:          "response",
			Normalized:     respContent,
			IngressType:    "AI_GATEWAY",
			Path:           r.URL.Path,
			Model:          respModel,
			FinishReason:   respFinish,
			TokenCount:     int(usage.TotalTokens),
			SourceIP:       middleware.ClientIP(r),
			ProviderRegion: target.Region,
			EndpointType:   epType,
			OutputModality: []hookcore.Modality{hookcore.ModalityText},
		}

		pipeline, pErr := h.deps.HookConfigCache.Resolver(r.Context()).BuildPipeline(
			"response", "AI_GATEWAY",
			epType,
			respInput.OutputModality,
			5*time.Second, 15*time.Second, false, logger,
		)
		if pErr != nil {
			logger.Error("failed to build response hook pipeline", "error", pErr)
			h.writeError(w, rec, http.StatusInternalServerError, "hook pipeline error")
			return
		}
		if pipeline != nil {
			pipeline.SetAllowModify(true)
			pipeline.SetClearSoftOnApprove(true)

			hookResult := pipeline.Execute(r.Context(), respInput)

			rec.ResponseHookDecision = string(hookResult.Decision)
			rec.ResponseHookReason = hookResult.Reason
			rec.ResponseHookReasonCode = hookResult.ReasonCode
			rec.ComplianceTags = mergeTagSets(rec.ComplianceTags, hookResult.Tags)
			rec.HooksPipeline = appendHookTrace(rec.HooksPipeline, "response", hookResult.HookResults)
			if br := mapBlockingRule(hookResult.BlockingRule); br != nil {
				rec.BlockingRule = br
			}

			if h.deps.Metrics != nil {
				h.deps.Metrics.RecordHookRequest(ingressFormat, "response", string(hookResult.Decision))
			}

			if hookResult.Decision == hookcore.RejectHard {
				h.writeError(w, rec, http.StatusForbidden, hookResult.Reason)
				return
			}
			if hookResult.Decision == hookcore.BlockSoft {
				h.writeError(w, rec, 246, hookResult.Reason)
				return
			}
			if hookResult.Decision == hookcore.Modify && len(hookResult.ModifiedContent) > 0 {
				rewritten, n, rErr := extractor.RewriteResponseBody(r.Context(), respBody, r.URL.Path, contentBlocksToNormalized(hookResult.ModifiedContent))
				switch {
				case errors.Is(rErr, traffic.ErrRewriteUnsupported):
					logger.Warn("hook produced Modify on response but adapter does not support rewrite; returning original body",
						slog.String("adapter", extractor.ID()),
						slog.String("path", r.URL.Path),
					)
				case rErr != nil:
					logger.Error("hook response rewrite failed",
						slog.String("adapter", extractor.ID()),
						slog.String("path", r.URL.Path),
						slog.String("error", rErr.Error()),
					)
					h.writeError(w, rec, http.StatusInternalServerError, "response rewrite failed")
					return
				default:
					respBody = rewritten
					rec.ResponseHookRewriteCount = n
					rec.ResponseHookRewritten = true
				}
			}
		}
	}

	// Capture after response hooks so payload mirrors bytes returned to
	// the client (including any response-stage rewrite). The full body is
	// handed to the audit Writer, which routes it inline
	// (≤ MaxInlineBodyBytes) or to the spill backend (>) at flush time.
	// Network-side bounding for the upstream read happens independently
	// in provcore.LimitedReadAll using MaxResponseBytes.
	pcCfgPost := h.payloadCaptureConfig()
	if len(respBody) > 0 && pcCfgPost.StoreResponseBody {
		rec.ResponseBody = respBody
		// Non-stream AI Gateway responses are always JSON-shaped after
		// the canonical bridge; this hint drives the Control Plane
		// reader's inline-vs-string decoding.
		rec.ResponseContentType = "application/json"
	}

	rec.StatusCode = http.StatusOK

	// Record metrics.
	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordRequest(target.ProviderName, target.ModelID, endpointType, rec.StatusCode, time.Since(start), usage)
	}

	// Quota reconcile — new engine (fire-and-forget).
	if h.deps.QuotaEngine != nil && quotaDecision != nil && quotaDecision.Allowed && rec.StatusCode < 400 {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					h.deps.Logger.Error("quota engine reconcile panic", "panic", r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			h.deps.QuotaEngine.Reconcile(ctx, quotaDecision, quota.ActualUsage{
				PromptTokens:     usage.PromptTokens,
				CompletionTokens: usage.CompletionTokens,
				TotalTokens:      usage.TotalTokens,
				InputPricePM:     quotaInPrice,
				OutputPricePM:    quotaOutPrice,
			})
		}()
	}

	// Cache writes are owned by the broker on the MISS path
	// (streamcache.Broker.writeCache). The direct path runs when the
	// cache is disabled or no broker is wired, so there is nothing to
	// persist here.

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBody)
}

// classifyCachePreLookup decides what cache_status a request should be
// stamped with BEFORE any Redis call. Returns DISABLED when the cache
// module is nil (config off) or the routed target list is empty, or
// SKIP_NO_CACHE when the client sent x-nexus-aigw-no-cache; returns
// empty string when the caller should proceed to PrepareBody + key
// build + lookup. Streaming requests are cacheable; the legacy
// "stream skip" branch was removed alongside the broker rewrite. The
// hasTargets guard is defensive — earlier phases normally reject empty
// target lists, but if one ever slipped through we treat it as DISABLED
// rather than computing a key against a zero-value target.
// classifyCachePreLookup is the read-only decision tree that turns the
// request-time state into a (GatewayCacheStatus, GatewayCacheSkipReason)
// pair before any Redis touch. passthroughBypassCache lands as
// (Skipped, Passthrough) — distinct from (Skipped, NoCache) so operators
// can SQL-filter "requests that bypassed cache for an emergency reason"
// separately from "client-opted-out via x-nexus-aigw-no-cache".
//
// When skipTimeSensitivePolicy is true AND the detector classifies the
// last user message as time-sensitive, the function returns
// (Skipped, time_sensitive) so BOTH L1 and L2 are skipped. The detector
// parameter may be nil; a nil detector never fires.
//
// Returns ("", "") when the cache phase should proceed normally
// (cache enabled, has targets, no client opt-out, no passthrough, not time-sensitive).
func classifyCachePreLookup(
	cacheEnabled, hasNoCacheHeader, hasTargets, passthroughBypassCache bool,
	detector timeSensitiveDetector,
	canonicalMessages []freshness.ChatMessage,
	skipTimeSensitivePolicy bool,
) (audit.GatewayCacheStatus, audit.GatewayCacheSkipReason) {
	switch {
	case !cacheEnabled:
		return audit.GatewayCacheSkipped, audit.GatewayCacheSkipReasonDisabled
	case !hasTargets:
		return audit.GatewayCacheSkipped, audit.GatewayCacheSkipReasonDisabled
	case passthroughBypassCache:
		return audit.GatewayCacheSkipped, audit.GatewayCacheSkipReasonPassthrough
	case hasNoCacheHeader:
		return audit.GatewayCacheSkipped, audit.GatewayCacheSkipReasonNoCache
	case skipTimeSensitivePolicy && detector != nil && isTimeSensitive(detector, canonicalMessages):
		return audit.GatewayCacheSkipped, audit.GatewayCacheSkipReasonTimeSensitive
	default:
		return "", ""
	}
}

// timeSensitiveDetector is a narrow interface matching freshness.Detector.
// Tests may inject a stub; production wires *freshness.Detector directly.
type timeSensitiveDetector interface {
	IsTimeSensitive(messages []freshness.ChatMessage) (bool, string)
}

// isTimeSensitive is a thin adapter between the detector interface and the
// caller.  Kept as a standalone function so it can be tested in isolation.
func isTimeSensitive(d timeSensitiveDetector, msgs []freshness.ChatMessage) bool {
	if d == nil || len(msgs) == 0 {
		return false
	}
	matched, _ := d.IsTimeSensitive(msgs)
	return matched
}

// normMessagesToFreshness projects normcore.Message slice (from the canonical
// NormalizedPayload built at Phase 3.5) into the flat freshness.ChatMessage
// representation that the time-sensitivity detector accepts.
//
// Each normcore.Message may carry multiple ContentBlocks; this function
// concatenates all ContentText blocks with a single space separator to produce
// a flat string.  Non-text blocks (image refs, tool use, tool results) are
// omitted — the detector only reasons over text.  An empty result (all
// messages have non-text content only) returns nil, which the detector treats
// as "not time-sensitive" (fail-open).
func normMessagesToFreshness(msgs []normcore.Message) []freshness.ChatMessage {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]freshness.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		var textParts []string
		for _, block := range m.Content {
			if block.Type == normcore.ContentText && block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		}
		if len(textParts) == 0 {
			continue
		}
		out = append(out, freshness.ChatMessage{
			Role:    string(m.Role),
			Content: strings.Join(textParts, " "),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// writeError writes a JSON error response and sets rec.StatusCode and rec.ErrorReason.
func (h *Handler) writeError(w http.ResponseWriter, rec *audit.Record, status int, message string) {
	rec.StatusCode = status
	rec.ErrorReason = message
	writeJSONError(w, status, message)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "proxy_error",
			"code":    status,
		},
	})
	_, _ = w.Write(resp)
}

func writeDetailedError(w http.ResponseWriter, status int, code, message, hint string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	errBody := map[string]any{
		"message": message,
		"type":    "proxy_error",
		"code":    code,
	}
	if hint != "" {
		errBody["hint"] = hint
	}
	resp, _ := json.Marshal(map[string]any{"error": errBody})
	_, _ = w.Write(resp)
}

func (h *Handler) writeDetailedErr(w http.ResponseWriter, rec *audit.Record, status int, code, message, hint string) {
	rec.StatusCode = status
	rec.ErrorCode = code
	rec.ErrorReason = message
	writeDetailedError(w, status, code, message, hint)
}

// geminicacheStaleRefError reports whether a Gemini 403 response body
// carries the stale-cachedContent error signature. Gemini phrases the
// message a few ways across API versions ("CachedContent not found",
// "permission denied" with the cache name, "GenerateContentRequest:
// cachedContent not found"); we match on the substrings that are
// stable across all of them, keeping false-positives low.
func geminicacheStaleRefError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// gjson would be more rigorous but the error payload shape varies;
	// substring match on the lowercase body is robust to wrapping.
	low := strings.ToLower(string(body))
	if strings.Contains(low, "cachedcontent not found") ||
		strings.Contains(low, "cached content not found") ||
		strings.Contains(low, "cachedcontents/") && strings.Contains(low, "not found") ||
		strings.Contains(low, "cachedcontents/") && strings.Contains(low, "permission denied") {
		return true
	}
	return false
}

// extractProviderErrorMessage extracts a human-readable error message from a
// provider response body. Handles the common JSON envelope used by OpenAI,
// Anthropic, and Gemini (.error.message or top-level .message). Falls back to
// a truncated raw body, or a generic "provider returned HTTP <N>" when empty.
func extractProviderErrorMessage(body []byte, statusCode int) string {
	if len(body) == 0 {
		return fmt.Sprintf("provider returned HTTP %d", statusCode)
	}
	if msg := gjson.GetBytes(body, "error.message").String(); msg != "" {
		return msg
	}
	if msg := gjson.GetBytes(body, "message").String(); msg != "" {
		return msg
	}
	if len(body) > 300 {
		return string(body[:300]) + "..."
	}
	return string(body)
}

// mergeTagSets returns the sorted, deduplicated union of a and b. The audit
// record accumulates compliance tags across request- and response-stage hook
// pipelines, so the merger must be stable, deterministic (sorted output),
// and de-duplicating (the same tag emitted on both stages appears once).
// Callers supply the current rec.ComplianceTags as a and the freshly emitted
// hookResult.Tags as b; the result replaces rec.ComplianceTags.
func mergeTagSets(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, t := range a {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for _, t := range b {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// mapBlockingRule narrows the hook-layer BlockingRule (which carries
// category/severity/labels for logging) to the JSONB audit shape that
// gets persisted on traffic_event.blocking_rule.
func mapBlockingRule(br *hookcore.BlockingRule) *rulepack.BlockingRule {
	if br == nil {
		return nil
	}
	return &rulepack.BlockingRule{
		Pack:        br.Pack,
		PackVersion: br.PackVersion,
		RuleID:      br.RuleID,
	}
}

// estimatedCostUSD computes the per-request USD cost from token counts and
// per-million-token prices. Both prices use the same units as
// Model.inputPricePerMillion / outputPricePerMillion (USD per 1M tokens),
// so we just divide the token counts by 1e6 and multiply.
//
// Zero or missing prices yield zero cost — e.g. when the routed Model row
// has not had prices set yet, or when a request fails before any tokens
// are consumed. This matches what the analytics surfaces expect: a NULL
// estimated_cost_usd is "we could not bill this", but we now persist 0
// when we know there were no chargeable tokens (cache hits, error
// responses), keeping cost summaries internally consistent.
func estimatedCostUSD(promptTok, completionTok int64, inPricePM, outPricePM float64) float64 {
	const million = 1_000_000.0
	return float64(promptTok)*inPricePM/million + float64(completionTok)*outPricePM/million
}

// computeCacheCosts recomputes rec.EstimatedCostUsd and populates the cache
// cost/savings fields using the provider_pricing table.
//
// Token-bucket semantics differ by provider wire format:
//   - Anthropic: input_tokens (→ PromptTokens) are NON-cached only; cache-read
//     and cache-write tokens are separate billing buckets in CacheReadTokens /
//     CacheCreationTokens.
//   - OpenAI / Gemini: prompt_tokens (→ PromptTokens) is the TOTAL input
//     including any cached subset; CacheReadTokens is a sub-count of PromptTokens.
//
// EstimatedCostUsd is recomputed from scratch using provider_pricing so the
// result is internally consistent regardless of what quotaInPrice (the model
// table's configured price) was set to. This prevents negative cost values
// that occurred when the two price sources diverged and savings > base cost.
func (h *Handler) computeCacheCosts(rec *audit.Record, target routingcore.RoutingTarget) {
	if h.deps.CachePricing == nil {
		return
	}
	if rec.CacheCreationTokens == 0 && rec.CacheReadTokens == 0 {
		return
	}
	p := h.deps.CachePricing.LookupCachePricing(target.AdapterType, target.ProviderID, target.ModelCode)
	if p == nil {
		return
	}
	const million = 1_000_000.0
	if rec.CacheCreationTokens > 0 {
		rec.CacheWriteCostUsd = float64(rec.CacheCreationTokens) * p.CacheWriteUSDPerM / million
	}
	if rec.CacheReadTokens > 0 {
		// Savings = what would have been paid at standard input price minus
		// the cheaper cache-read price, using the provider's own price list.
		rec.CacheReadSavingsUsd = float64(rec.CacheReadTokens) * (p.InputUSDPerM - p.CacheReadUSDPerM) / million
	}
	rec.CacheNetSavingsUsd = rec.CacheReadSavingsUsd - rec.CacheWriteCostUsd

	// Recompute EstimatedCostUsd from scratch.
	//
	// PromptTokens is always "total input including cached" across every
	// adapter (the normalizer sums input_tokens + cache_read + cache_creation
	// into it at codec time). computeCacheCosts subtracts both cache buckets
	// to get the uncached remainder, then bills each bucket at its own rate.
	// Without this, cached tokens would be charged at full input price AND
	// again at the cache rate.
	regularInput := rec.PromptTokens - rec.CacheReadTokens - rec.CacheCreationTokens
	if regularInput < 0 {
		regularInput = 0
	}
	rec.EstimatedCostUsd = float64(regularInput)*p.InputUSDPerM/million +
		float64(rec.CacheReadTokens)*p.CacheReadUSDPerM/million +
		float64(rec.CacheCreationTokens)*p.CacheWriteUSDPerM/million +
		float64(rec.CompletionTokens)*p.OutputUSDPerM/million
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
