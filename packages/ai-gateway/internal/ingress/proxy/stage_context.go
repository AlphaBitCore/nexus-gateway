// stage_context.go — the per-request state carrier and constructor for
// the proxy stage chain. ServeProxy (proxy.go) drives the chain:
// admission → routing → quota → request hooks → cache → execute →
// respond, with the accounting defer (stage_accounting.go) wrapping the
// whole request. Each stage is a type in its stage_<name>.go file; all
// shared per-request state lives here so no stage smuggles values
// through globals.
package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
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

	// in is the route-table Ingress the handler closure was built with
	// (pre header-override). resolved is the per-request effective copy
	// after the `x-nexus-aigw-body-format` override; the cache stage may
	// downgrade resolved.WireShape for cross-format dispatch while
	// egress reshaping still reads the immutable context ingress.
	in       Ingress
	resolved Ingress

	start        time.Time
	requestID    string
	endpointType string

	phaseSink  *traffic.PhaseSink
	phaseTimer *traffic.PhaseTimer
	logger     *slog.Logger
	rec        *audit.Record

	// Admission outputs.
	vkMeta   *vkauth.VKMeta
	body     []byte // hook Modify may replace with the rewritten bytes
	modelID  string
	isStream bool
	rctxFull *requestcontext.RequestContext
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

// newProxyState performs the pre-pipeline setup: resolve the effective
// ingress (honouring the `x-nexus-aigw-body-format` override on the
// OpenAI-compat family), stamp the request context with the ingress,
// phase sink and timer, build the request-scoped logger, and open the
// audit record. Returns ok=false after writing the 400 response when the
// override header names an unknown body format — no audit record exists
// at that point, matching the pre-resolution contract.
func (h *Handler) newProxyState(in Ingress, w http.ResponseWriter, r *http.Request) (*proxyState, bool) {
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
		return nil, false
	}
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
		// Default both directions to approve so a path that runs no hook
		// (bypass, no-hooks-configured, hooks that don't reach the dispatch)
		// persists its captured body as-is. A matching redact/block hook
		// upgrades the action at dispatch time; the writer's StorageRawBody
		// drops the raw copy only under a redact/block action, so the empty
		// zero value must never reach it (it would drop the body).
		RequestAction:  hookcore.ActionApprove,
		ResponseAction: hookcore.ActionApprove,
	}

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
		logger:       logger,
		rec:          rec,
	}, true
}
