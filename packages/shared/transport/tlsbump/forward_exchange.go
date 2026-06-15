package tlsbump

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// bumpedExchange holds the per-REQUEST state of one request/response pair
// on a bumped tunnel. Distinct from bumpedFlow (per-tunnel): a keep-alive
// session reuses the flow but gets a fresh exchange per request, so
// nothing here can leak across requests.
type bumpedExchange struct {
	flow *bumpedFlow
	w    http.ResponseWriter
	// r is the live request pointer. Phase methods that re-derive the
	// request via WithContext MUST write the result back here so later
	// phases (upstream forward, marker stamping, SSE dispatch) observe
	// the accumulated context values.
	r            *http.Request
	requestStart time.Time

	// phaseSink + phaseBreakdown + connSetupStart feed the audit row's
	// latency breakdown; populated in prepare(), consumed when the
	// request-hook phase builds AuditInfo.
	phaseSink      *traffic.PhaseSink
	phaseBreakdown map[string]int
	connSetupStart time.Time

	// pcCfg is the payload-capture config snapshotted once per request so
	// the read cap and the capture gates agree across all downstream emit
	// call-sites, even if the admin invalidates mid-request.
	pcCfg payloadcapture.Config

	// txID / traceID — the canonical X-Nexus-Request-Id correlation value.
	txID    string
	traceID string

	// complianceEnabled is the request-local shadow of the tunnel-level
	// gate; a PASSTHROUGH path action switches it off for this request only.
	complianceEnabled bool

	// Domain/path policy resolution outputs, stamped onto every audit row.
	matchedDomain      *domain.InterceptionDomain
	resolvedPathAction domain.PathAction
	domainRuleID       string

	// endpointType classified from (method, path) so both hook pipelines
	// apply the same endpoint-aware filtering.
	endpointType typology.EndpointKind

	// reqHookResult is populated when a domain rule matches and the
	// request hook pipeline runs. The CPMarker built before upstream
	// converts it into a HookOutcomeInput for downstream response writers.
	reqHookResult *core.CompliancePipelineResult
}

// newExchange begins a request on this tunnel: records the start time and
// threads the captured ClientHello bytes into the request context so
// UpstreamTransport.DialTLSContext can replay the client's fingerprint.
func (f *bumpedFlow) newExchange(w http.ResponseWriter, r *http.Request) *bumpedExchange {
	requestStart := time.Now()

	if f.bo != nil && len(f.bo.clientHelloRaw) > 0 {
		r = r.WithContext(context.WithValue(r.Context(), clientHelloKey{}, f.bo.clientHelloRaw))
	}

	return &bumpedExchange{
		flow:              f,
		w:                 w,
		r:                 r,
		requestStart:      requestStart,
		complianceEnabled: f.complianceEnabled,
	}
}

// attestationPeek checks the X-Nexus-Attestation header BEFORE any
// compliance machinery spins up. When the agent signs the inner request
// and the verifier accepts, this CONNECT is pure passthrough — skip the
// entire compliance pipeline AND audit emission AND payload capture, just
// forward the request upstream and stream the response back. CP becomes
// transparent on agent-attested traffic; the agent's own audit row is the
// system-of-record.
//
// The verifier is fail-open — invalid / missing / replayed all return
// false, and we fall through to the normal MITM path. The verifier
// increments nexus_attestation_verify_total{outcome=...} per call so the
// operational metric stays consistent regardless of which branch fires.
//
// Returns true when the request was handled as attested passthrough.
func (x *bumpedExchange) attestationPeek() bool {
	bo, logger := x.flow.bo, x.flow.logger
	if bo != nil && bo.attestationVerifier != nil {
		if valid, agentID := bo.attestationVerifier(x.r.Context(), x.r.Header.Get(AttestationHeaderName)); valid {
			logger.Info("attestation verified — passthrough (no hooks, no audit)",
				"target", x.flow.targetHost,
				"agent_id", agentID,
				"method", x.r.Method,
				"path", x.r.URL.Path,
			)
			attestationPassthrough(x.w, x.r, x.flow.upstream, logger)
			return true
		}
	}
	return false
}

// prepare sets up the request-scoped bookkeeping every later phase reads:
// the latency PhaseSink, the phase breakdown map (with the one-shot
// tls_handshake_ms stamp), the payload-capture snapshot, the correlation
// ID, and the upstream URL rewrite.
func (x *bumpedExchange) prepare() {
	bo, logger := x.flow.bo, x.flow.logger

	// A per-request PhaseSink that the shared/traffic tracing
	// transport (wrapped around UpstreamTransport in upstream.go)
	// populates with upstream TTFB and upstream-total. The same
	// pointer is stamped onto AuditInfo so buildEvent can read it
	// at emit time across every Emit / EmitDual call site.
	x.phaseSink = traffic.NewPhaseSink()
	x.r = x.r.WithContext(traffic.WithPhaseSink(x.r.Context(), x.phaseSink))
	// Stamp conn_setup_ms (cheap server-side bookkeeping) and
	// — on the FIRST request of this bumped tunnel only —
	// tls_handshake_ms (sourced from BumpConnection). Subsequent
	// keep-alive requests on the same bo skip the handshake stamp
	// via sync.Once so we don't double-count it across the row set.
	x.phaseBreakdown = map[string]int{}
	x.connSetupStart = time.Now()
	if bo != nil {
		bo.tlsHandshakeOnce.Do(func() {
			if bo.tlsHandshakeMs > 0 {
				x.phaseBreakdown["tls_handshake_ms"] = bo.tlsHandshakeMs
			}
		})
	}

	// Snapshot the payload-capture config once per request so the
	// read cap and the capture gates agree across all downstream
	// emit call-sites, even if the admin invalidates mid-request.
	x.pcCfg = payloadcapture.DefaultConfig()
	if bo != nil && bo.payloadCaptureStore != nil {
		x.pcCfg = bo.payloadCaptureStore.Get()
	}

	// Use client-supplied correlation ID if present; otherwise generate one.
	// X-Nexus-Request-Id is the single canonical correlation header — it
	// doubles as the cross-service trace id (seeded by the agent for
	// intercepted flows, generated here for direct proxy traffic) and is
	// forwarded to ai-gateway so its audit records share the same id.
	x.txID = x.r.Header.Get("X-Nexus-Request-Id")
	if x.txID == "" {
		x.txID = uuid.NewString()
	}
	// Set the header on the outgoing request so upstream services can correlate.
	x.r.Header.Set("X-Nexus-Request-Id", x.txID)
	x.traceID = x.txID

	// Rewrite the URL to point to the upstream.
	x.r.URL.Scheme = "https"
	x.r.URL.Host = x.flow.targetHost

	logger.Debug("request entry",
		"method", x.r.Method,
		"path", x.r.URL.Path,
		"host", x.r.Host,
		"target", x.flow.targetHost,
		"complianceEnabled", x.complianceEnabled,
		"contentType", x.r.Header.Get("Content-Type"),
		"txID", x.txID,
	)
}

// resolveDomainPolicy resolves the matched InterceptionDomain
// (priority-ordered) + per-path action. BLOCK rejects the request before
// any work; PASSTHROUGH skips the compliance pipeline for this request
// only by switching the request-local complianceEnabled off; PROCESS
// leaves the existing flow untouched. The matched domain's NetworkZone is
// captured for audit tagging downstream. Also classifies the endpoint
// type from (method, path) so the compliance pipeline can apply
// endpoint-aware hook filtering.
//
// Returns true when the request was refused by a BLOCK path action.
func (x *bumpedExchange) resolveDomainPolicy() bool {
	bo, logger := x.flow.bo, x.flow.logger

	var matchedZone string
	// pathAction + domainRuleID live on the exchange so the audit-
	// emitter call sites stamp them onto every audit_event row.
	// The agent UI's classify() reads these to distinguish Inspect
	// (matched + PASSTHROUGH) from Processed (matched + PROCESS +
	// hooks ran).
	if bo != nil && bo.domainEngine != nil {
		x.matchedDomain = bo.domainEngine.MatchHost(x.r.Host)
		if x.matchedDomain == nil {
			host, _, _ := net.SplitHostPort(x.flow.targetHost)
			if host == "" {
				host = x.flow.targetHost
			}
			x.matchedDomain = bo.domainEngine.MatchHost(host)
		}
		x.resolvedPathAction = bo.domainEngine.PathAction(x.matchedDomain, x.r.URL.Path)
		if x.matchedDomain != nil {
			matchedZone = string(x.matchedDomain.NetworkZone)
			// Capture domainRuleID unconditionally on match — even when
			// no adapter resolves later. Without this the audit row
			// shows DomainRuleID="" and the UI mislabels the row as
			// Untracked.
			x.domainRuleID = x.matchedDomain.ID
		}
		logger.Debug("domain/path resolved",
			"matchedDomain", domainName(x.matchedDomain),
			"pathAction", x.resolvedPathAction,
			"complianceEnabledAfter", x.complianceEnabled,
			"txID", x.txID,
		)
		switch x.resolvedPathAction {
		case domain.PathActionBlock:
			logger.Warn("request blocked by interception path policy",
				"target", x.flow.targetHost,
				"path", x.r.URL.Path,
				"transactionId", x.txID,
				"domain", domainName(x.matchedDomain),
				"networkZone", matchedZone,
				"action", "BLOCK",
			)
			http.Error(x.w, "Request blocked by compliance policy", http.StatusForbidden)
			return true
		case domain.PathActionPassthrough:
			logger.Info("request passthrough — compliance hooks skipped by path policy",
				"target", x.flow.targetHost,
				"path", x.r.URL.Path,
				"transactionId", x.txID,
				"domain", domainName(x.matchedDomain),
				"networkZone", matchedZone,
				"action", "PASSTHROUGH",
			)
			x.complianceEnabled = false
		}
	}
	_ = matchedZone // network_zone audit tagging lands in a follow-up

	// Classify the endpoint from (method, path) so the compliance
	// pipeline can apply endpoint-aware hook filtering. Falls back
	// to empty when no rule matches — all hooks run on unclassified
	// endpoints.
	x.endpointType, _, _ = typology.ClassifyPath(x.r.Method, x.r.URL.Path)

	return false
}

// stampCPMarker stashes the per-request marker state on the context so
// that downstream response write sites (the buffered relay in upstream.go
// and the SSE handler in sse.go) can inject x-nexus-cp-* headers without
// re-deriving these values. The marker is always set — even on the
// compliance-disabled fast path — so callers never need to handle a nil
// check for the basic request-id field.
func (x *bumpedExchange) stampCPMarker() {
	x.r = x.r.WithContext(contextWithCPMarker(x.r.Context(), &CPMarker{
		RequestID:    x.txID,
		DomainRuleID: x.domainRuleID,
		HookOutcome:  cpHookOutcomeFromResult(x.reqHookResult),
	}))
}

// forwardUpstream sends the (possibly hook-rewritten) request upstream.
// Uses r.Context() (not the connection-level ctx) so the clientHelloKey
// value flows through to UpstreamTransport.DialTLSContext. On failure it
// answers 502, emits the failure audit row when compliance is enabled,
// and returns ok=false.
func (x *bumpedExchange) forwardUpstream() (*http.Response, bool) {
	bo, logger := x.flow.bo, x.flow.logger

	resp, err := x.flow.upstream.ForwardRequest(x.r.Context(), x.r)
	if err != nil {
		logger.Error("upstream request failed",
			"target", x.flow.targetHost,
			"method", x.r.Method,
			"path", x.r.URL.Path,
			// cancel_cause distinguishes a client close (client_canceled —
			// the request context was canceled by the downstream client
			// giving up) from our own deadline. For a streaming chat that the
			// client abandoned mid-request this reads client_canceled, which
			// rules out a server/proxy timeout as the cause.
			"cancel_cause", cancelCause(x.r.Context()),
			"duration_ms", int(time.Since(x.requestStart).Milliseconds()),
			"error", err,
		)
		// Emit audit for failed upstream if compliance enabled.
		if x.complianceEnabled {
			if audCtx, ok := x.r.Context().Value(requestAuditKey{}).(*requestAuditCtx); ok && audCtx != nil {
				if bo.auditEmitter != nil {
					// Upstream failed before any body — no usage available.
					// EmitDual so the request-stage StorageAction still
					// governs the persisted request body on this failure path.
					usage := traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
					bo.auditEmitter.EmitDual(audCtx.input, audCtx.info, audCtx.requestPipelineResult, &core.CompliancePipelineResult{Decision: compliance.Approve}, "BUMP_SUCCESS", http.StatusBadGateway, int(time.Since(x.requestStart).Milliseconds()), audCtx.requestBodyBytes(), nil, usage)
				}
			}
		}
		http.Error(x.w, "Bad Gateway", http.StatusBadGateway)
		return nil, false
	}
	return resp, true
}

// relayResponse copies the upstream response to the client, injecting the
// x-nexus-* marker headers via markerHook.
func (x *bumpedExchange) relayResponse(resp *http.Response) {
	if err := copyResponse(x.w, resp, markerHook(x.r.Context(), x.flow.bo.identity)); err != nil {
		ct := resp.Header.Get("Content-Type")
		// Rich diagnostic: a failed relay on a streaming endpoint is the
		// "we lost the chat reply" case. Record WHO canceled (client vs us),
		// the Content-Type + a streaming smell (to see whether a streaming
		// reply was mis-routed to this buffered/copy relay because its CT
		// isn't in isStreamingContentType), and timing — so the mechanism is
		// verifiable from agent.log alone. The audit ROW, when an audit
		// context exists, was already emitted by runResponseStage before this
		// relay; when it does NOT exist runResponseStage logged the UNAUDITED
		// warning, so this failure leaves a paper trail either way.
		x.flow.logger.Error("failed to copy upstream response",
			"target", x.flow.targetHost,
			"method", x.r.Method,
			"path", x.r.URL.Path,
			"status_code", resp.StatusCode,
			"content_type", ct,
			"is_sse", isStreamingContentType(ct),
			"maybe_buffered_stream", looksLikeStreamingResponse(resp),
			"cancel_cause", cancelCause(x.r.Context()),
			"duration_ms", int(time.Since(x.requestStart).Milliseconds()),
			"error", err,
		)
		// Response may be partially written; nothing more we can do.
	}
}
