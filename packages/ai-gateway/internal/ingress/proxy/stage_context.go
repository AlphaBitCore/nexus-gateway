// stage_context.go — the per-request state carrier and constructor for
// the proxy stage chain. ServeProxy (proxy.go) drives the chain:
// admission → routing → quota → request hooks → cache → execute →
// respond, with the accounting defer (stage_accounting.go) wrapping the
// whole request. Each stage is a type in its stage_<name>.go file; all
// shared per-request state lives here so no stage smuggles values
// through globals.
package proxy

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// proxyStage is one step of the proxy request lifecycle. run returns
// false when the stage has terminated the request — a response has been
// written or the request was handed off to a sub-pipeline (cache HIT
// replay, broker leg, streaming handler) — and the driver stops the
// chain.
type proxyStage interface {
	run() bool
}

// proxyState carries the per-request state shared across the stage
// chain. Field groups are owned by the stage that produces them; later
// stages only read (the exceptions — body rewritten by hooks, resolved
// wire-shape downgraded by cache prep, r re-stamped with derived
// contexts — are documented at their write sites).
type proxyState struct {
	h *Handler
	w http.ResponseWriter
	r *http.Request

	// in is the route-table Ingress the handler closure was built with —
	// shared across every request on the route, never mutated. resolved is
	// the per-request copy: the cache stage may downgrade
	// resolved.WireShape for cross-format dispatch while egress reshaping
	// still reads the immutable context ingress.
	in       Ingress
	resolved Ingress

	start        time.Time
	requestID    string
	endpointType string

	phaseSink  *traffic.PhaseSink
	phaseTimer *traffic.PhaseTimer
	// scopedLogger memoizes the request-scoped logger. Nil until log() is
	// first called: building it costs 463.8ns/704B/11 allocs against a JSON
	// handler, and a 2xx request logs nothing (access logs were demoted to
	// Debug), so the overwhelming majority of requests must not pay it.
	// Read through log(), never directly.
	scopedLogger *slog.Logger
	rec          *audit.Record

	// Admission outputs.
	vkMeta   *vkauth.VKMeta
	body     []byte // hook Modify may replace with the rewritten bytes
	modelID  string
	isStream bool
	rctxFull *requestcontext.RequestContext
	// releaseGenerativeCap returns the per-VK generative concurrency slot
	// acquired in the admission stage. Nil for non-generative kinds (and for
	// generative kinds with no configured cap). Called once in finalizeAudit
	// — the same defer that covers every exit path (success, error, panic) —
	// so a slot is never stranded.
	releaseGenerativeCap func()

	// postHookNormalized memoizes the canonical rebuilt from the
	// hook-rewritten body for cache consumers (see cacheNormalized).
	// Set-flag distinguishes "computed nil (skip)" from "not yet computed".
	postHookNormalized    *normcore.NormalizedPayload
	postHookNormalizedSet bool
	// unattachedBodyHandle is the pooled request-body handle when payload
	// capture did NOT attach it to the record (bodies-off). finalizeAudit
	// returns it to the pool at request end; nil when captured or no body.
	unattachedBodyHandle *[]byte

	// Routing outputs.
	routeResult *routingcore.RouteResult
	resolvedReq *requestcontext.ResolvedRequest

	// Quota outputs.
	quotaInPrice  float64
	quotaOutPrice float64
	quotaDecision *quota.Decision

	// Request-hooks outputs. Nil on the passthrough bypass path so
	// downstream code (cache key build, audit population) sees the
	// zero value without further branching.
	reqHookResult *hookcore.CompliancePipelineResult

	// Cache outputs.
	cacheKey               string
	gatewayCacheStatus     audit.GatewayCacheStatus
	gatewayCacheSkipReason audit.GatewayCacheSkipReason
	// cachePreparedCacheMarked records that the codec turned on the
	// provider's prompt cache while preparing this body. Stamped onto the
	// audit row so cache_marker_injected reports what was SENT rather than
	// what was configured.
	cachePreparedCacheMarked bool
	// cachePreparedBody is the PrepareBody output, reused on MISS to
	// skip a duplicate encode in the executor; cachePreparedRewrites is
	// the matching rewrites slice (goes into Response.Coerced);
	// cachePreparedURLOverride is the matching codec URLOverride that
	// reaches the dispatched URL on MISS.
	cachePreparedBody        []byte
	cachePreparedRewrites    []string
	cachePreparedURLOverride string

	// Execute outputs (direct path only; the broker leg responds inside
	// the execute stage).
	execResult   *executor.ExecutionResult
	execTarget   routingcore.RoutingTarget
	execAttempts int
}

// newProxyState performs the pre-pipeline setup: stamp the request
// context with the ingress, phase sink and timer, build the
// request-scoped logger, and open the audit record.
// log returns the request-scoped logger, building it on first use.
//
// Every attribute it carries is already on the state, so memoizing costs no
// extra field: the request id is the cross-service correlation value, endpoint
// is the audit endpoint kind, and ingressFormat comes from the per-request
// resolved Ingress.
//
// Deliberately lazy. slog.Logger.With allocates unconditionally — it boxes
// each argument, clones the Logger and clones the handler — and it does so
// whether or not anything is ever logged through the result. Access logs for
// 2xx/3xx are Debug, so a healthy request emits nothing and would have paid
// 463.8ns/704B/11 allocs for a logger it never used. The sibling call site in
// proxy_routing.go builds its logger inside the branch that logs, which is the
// shape this now matches.
//
// Not safe for concurrent first use, and does not need to be: the stage chain
// is sequential, and the streaming legs that outlive it capture the result of
// an earlier call rather than racing to build one.
func (s *proxyState) log() *slog.Logger {
	if s.scopedLogger == nil {
		s.scopedLogger = s.h.deps.Logger.With(
			"requestId", s.requestID,
			diag.ExternalRequestIDAttrKey, s.requestID,
			"endpoint", s.endpointType,
			"ingressFormat", string(s.resolved.BodyFormat),
		)
	}
	return s.scopedLogger
}

// debugEnabled reports whether anything would come of a Debug line, asked of
// the BASE logger so the question itself does not build the scoped one.
//
// It exists because slog's level check happens too late to save the cost:
// Logger.Debug(msg, args...) builds the variadic []any and boxes every
// argument AT THE CALL SITE, then discards them inside. On a path every
// successful request walks, that is a permanent tax for output nobody reads —
// 2xx access logging is Debug by deliberate choice
// (perf(gateway): demote 2xx/3xx access logs to Debug).
func (s *proxyState) debugEnabled() bool {
	return s.h.deps.Logger.Enabled(s.r.Context(), slog.LevelDebug)
}

func (h *Handler) newProxyState(in Ingress, w http.ResponseWriter, r *http.Request) *proxyState {
	// All persisted timestamps are UTC instants — see docs/developers/workflow/timezone.md.
	// Latency math is also fine off UTC since time.Time carries a
	// monotonic clock reading independent of location.
	start := time.Now().UTC()
	requestID := traffic.ResolveRequestID(r.Header)
	clientRequestID := r.Header.Get(traffic.HeaderRequestIDAlias)
	// The request id under either accepted spelling. It is the cross-service
	// correlation key; the caller's own W3C trace is a separate value read from
	// traceparent below.
	// The caller's W3C trace id, present only when they sent a parseable
	// traceparent (telemetry.HTTPTrace captures it between Extract and Start).
	// Empty means the caller runs no tracing we can join to — the column stays
	// NULL rather than receiving the id our own tracer derived from requestID.
	traceID := telemetry.InboundTraceID(r.Context())

	// Ingress detection is path-authoritative: the route table's descriptor
	// is the whole answer. resolved starts as a copy of it and only the
	// cache stage's cross-format downgrade rewrites it later.
	resolved := in
	// endpoint_type is chat-KIND for routing / cache / hook dispatch (via
	// KindFromWireShape), but the Responses API carries its own label so the
	// persisted traffic_event distinguishes /v1/responses from chat completions.
	endpointType := string(typology.KindFromWireShape(resolved.WireShape))
	if resolved.WireShape == typology.WireShapeOpenAIResponses {
		endpointType = string(typology.EndpointKindResponses)
	}

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
	// finalize defer.
	phaseTimer := traffic.NewPhaseTimer()
	r = r.WithContext(ctx)

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
		// Default both directions to approve so a path that runs no hook
		// (bypass, no-hooks-configured, hooks that don't reach the dispatch)
		// persists its captured body as-is. A matching redact/block hook
		// upgrades the action at dispatch time; the writer's StorageRawBody
		// drops the raw copy only under a redact/block action, so the empty
		// zero value must never reach it (it would drop the body).
		RequestAction:  hookcore.ActionApprove,
		ResponseAction: hookcore.ActionApprove,
	}
	stampCallerAttribution(rec, r.Header)

	return &proxyState{
		h:            h,
		w:            w,
		r:            r,
		in:           in,
		resolved:     resolved,
		start:        start,
		requestID:    requestID,
		endpointType: endpointType,
		phaseSink:    phaseSink,
		phaseTimer:   phaseTimer,
		rec:          rec,
	}
}
