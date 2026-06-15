package tlsbump

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// authHeaderSet is a pre-built lookup set of auth-related headers (lowercase
// keys) that are stripped from the compliance pipeline but still forwarded
// to the upstream (they contain credentials). Using a map avoids a linear
// scan on every header of every request.
var authHeaderSet = map[string]bool{
	"authorization": true,
	"x-api-key":     true,
	"api-key":       true,
}

// isAuthHeader returns true if the header name is an auth-related header
// that should be stripped from the compliance pipeline.
func isAuthHeader(name string) bool {
	return authHeaderSet[strings.ToLower(name)]
}

// requestAuditKey is a context key carrying the audit context for a request.
type requestAuditKey struct{}

// requestAuditCtx bundles the per-request data needed for post-upstream audit
// emission that is not present in HookInput (transaction/connection IDs, headers).
type requestAuditCtx struct {
	input *core.HookInput
	info  compliance.AuditInfo
	// requestBody holds the client-original request bytes captured for
	// audit storage when payload_capture.storeRequestBody is true; nil
	// otherwise. Reused across the post-upstream Emit call sites so the
	// per-request snapshot decision stays stable after the initial read.
	// Set eagerly on the buffered path; nil on the streaming path (the body
	// is not buffered there — see requestCapture).
	requestBody []byte
	// requestCapture holds the bounded tee of a streaming (unknown-length)
	// request body. It fills as the upstream reads the body during RoundTrip,
	// so it is complete by the time the post-upstream emit sites read it.
	// Nil on the buffered path. requestBodyBytes() prefers requestBody and
	// falls back to this.
	requestCapture *boundedCapture
	// storeRequestBody mirrors payload_capture.storeRequestBody for the
	// streaming path, where the snapshot decision must be applied at emit
	// time (the capture is not yet filled when the audit ctx is built).
	storeRequestBody bool
	// storeResponseBody mirrors the payload-capture snapshot for this
	// request so the response pipeline path can decide whether to
	// forward response bytes to the audit emitter without re-reading
	// the Store.
	storeResponseBody bool
	// requestPipelineResult is the request-stage hook pipeline outcome
	// (HookResults + Decision + Reason + BlockingRule). Stashed here so
	// post-upstream emit sites — including the SSE path — can record the
	// request-stage executions on
	// traffic_event.request_hooks_pipeline. Nil when the request was on
	// the compliance-disabled fast path.
	requestPipelineResult *core.CompliancePipelineResult
	// matchedDomain is the interception_domain row that admitted this
	// request, carrying the per-host StreamingPolicy override columns.
	// Nil for requests on the compliance-disabled fast path. Read by
	// handleSSEResponse so per-host streaming mode + capture flags
	// resolve through shared/streaming/policy.Resolve at request time.
	matchedDomain *domain.InterceptionDomain
	// adapter is the traffic adapter resolved from matchedDomain.AdapterID
	// at request time. Reused on the response path (DetectResponseUsage,
	// ExtractResponse) so the same instance handles both halves of the
	// request/response pair. Nil when no adapter matched or complianceEnabled
	// is false.
	adapter traffic.Adapter
}

// requestBodyBytes resolves the request bytes to hand the audit emitter,
// preferring the eagerly-buffered requestBody and falling back to the streaming
// tee capture (gated on storeRequestBody, matching the buffered path's
// captureBodyIfEnabled snapshot decision). Returns nil when nothing should be
// stored.
func (a *requestAuditCtx) requestBodyBytes() []byte {
	if a.requestBody != nil {
		return a.requestBody
	}
	if a.storeRequestBody && a.requestCapture != nil {
		return a.requestCapture.Bytes()
	}
	return nil
}

// domainName returns the matched domain's name for log fields, or the
// empty string when no domain matched (defensive — host should already
// have been admitted at CONNECT).
func domainName(d *domain.InterceptionDomain) string {
	if d == nil {
		return ""
	}
	return d.Name
}

// bumpedFlow holds the per-TUNNEL state of one bumped CONNECT tunnel —
// everything the forward handler closure used to capture. One instance
// serves every keep-alive request on the same bumped TLS session; all
// per-request state lives on bumpedExchange instead.
type bumpedFlow struct {
	// ctx is the connection-level context from BumpConnection. Hook
	// pipeline executions and adapter rewrites run against this tunnel
	// context (not the per-request context) — preserved from the
	// original closure semantics.
	ctx        context.Context
	targetHost string
	upstream   *UpstreamTransport
	logger     *slog.Logger
	bo         *bumpOptions
	// complianceEnabled is the tunnel-level gate: compliance deps were
	// injected by the caller. Each exchange takes a request-local copy
	// that a PASSTHROUGH path action can switch off for that request only.
	complianceEnabled bool
}

// buildForwardHandler returns an http.Handler that rewrites each request's
// URL to target the upstream and copies the response back to the client.
// When compliance deps are provided via bumpOptions, the handler runs
// request hooks before upstream and response/SSE hooks after upstream.
func buildForwardHandler(
	ctx context.Context,
	targetHost string,
	upstream *UpstreamTransport,
	logger *slog.Logger,
	bo *bumpOptions,
) http.Handler {
	f := &bumpedFlow{
		ctx:               ctx,
		targetHost:        targetHost,
		upstream:          upstream,
		logger:            logger,
		bo:                bo,
		complianceEnabled: bo != nil && bo.policyResolver != nil,
	}
	return http.HandlerFunc(f.serveRequest)
}

// serveRequest is the per-request phase driver: attestation peek →
// request preparation → domain path policy → request hooks → upstream
// forward → response/SSE stage → relay. Each phase method returns true
// when it fully handled the request (refusal, passthrough, SSE stream,
// upstream failure) and the chain stops there.
func (f *bumpedFlow) serveRequest(w http.ResponseWriter, r *http.Request) {
	x := f.newExchange(w, r)
	if x.attestationPeek() {
		return
	}
	x.prepare()
	if x.resolveDomainPolicy() {
		return
	}
	if x.runRequestPhase() {
		return
	}
	x.stampCPMarker()
	resp, ok := x.forwardUpstream()
	if !ok {
		return
	}
	// Always close the upstream connection's original response body.
	// Downstream branches may swap resp.Body for an in-memory NopCloser
	// (buffered inspection, refusals), so capture the network reader now
	// and close THAT — a defer on resp.Body would close the substitute and
	// leak the real connection. The SSE handler also closes it; a second
	// Close on an http response body is benign, so this is safe on every
	// path.
	upstreamBody := resp.Body
	defer func() { _ = upstreamBody.Close() }()
	if x.runResponseStage(resp) {
		return
	}
	x.relayResponse(resp)
}

// captureBodyIfEnabled returns the body slice when the corresponding
// capture flag is true and the slice is non-empty; otherwise nil. The
// returned slice is a reference to the caller's bytes — callers must not
// mutate it after handing it off to the audit emitter. We intentionally
// store the pre-hook bytes ("what the caller sent") since CP's request
// pipeline runs with allowModify=false and cannot rewrite the body.
func captureBodyIfEnabled(enabled bool, body []byte) []byte {
	if !enabled || len(body) == 0 {
		return nil
	}
	return body
}
