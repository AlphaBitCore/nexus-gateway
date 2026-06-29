package tlsbump

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
)

// These tests pin the forward handler's observable per-phase contract:
// attestation passthrough, domain path-policy (BLOCK / PASSTHROUGH),
// request-hook decisions (REJECT_HARD / BLOCK_SOFT), upstream-failure
// handling, the non-AI fast path, response-hook hard reject, and
// correlation-ID propagation. Each asserts the externally visible
// behavior — status code, refusal body, marker headers, audit rows,
// and whether upstream was contacted — so the handler's internal
// structure can change without changing any of these outcomes.

// capturingRoundTripper records a snapshot of each outbound upstream
// request's headers so tests can assert what the upstream actually saw
// (auth stripping, correlation-ID stamping, attestation-header removal).
type capturingRoundTripper struct {
	mu       sync.Mutex
	headers  []http.Header
	makeResp func() *http.Response
	err      error
}

func (rt *capturingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.headers = append(rt.headers, r.Header.Clone())
	rt.mu.Unlock()
	if rt.err != nil {
		return nil, rt.err
	}
	return rt.makeResp(), nil
}

func (rt *capturingRoundTripper) calls() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.headers)
}

func (rt *capturingRoundTripper) header(i int) http.Header {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.headers[i]
}

// cannedHook returns a fixed pre-built result for every execution,
// letting tests drive a specific pipeline decision through the real
// resolver + pipeline machinery.
type cannedHook struct {
	result core.HookResult
}

func (h cannedHook) Execute(context.Context, *core.HookInput) (*core.HookResult, error) {
	r := h.result
	return &r, nil
}

func (h cannedHook) SupportsEndpoint(core.EndpointType) bool { return true }

func (h cannedHook) SupportsModality(core.Modality) bool { return true }

// decidingResolver builds a resolver whose single hook at the given stage
// always returns the supplied decision/reason/reasonCode.
func decidingResolver(t *testing.T, stage string, decision core.Decision, reason, reasonCode string) *compliance.PolicyResolver {
	t.Helper()
	reg := core.NewHookRegistry()
	reg.Register("canned", func(_ *core.HookConfig) (core.Hook, error) {
		return cannedHook{result: core.HookResult{
			HookID:     "h-canned",
			HookName:   "canned-hook",
			Decision:   decision,
			Reason:     reason,
			ReasonCode: reasonCode,
		}}, nil
	})
	return compliance.NewPolicyResolver([]core.HookConfig{
		{
			ID:                "h-canned",
			ImplementationID:  "canned",
			Name:              "canned-hook",
			Stage:             stage,
			Enabled:           true,
			FailBehavior:      "fail-open",
			ApplicableIngress: []string{"ALL"},
		},
	}, reg, discardSlog())
}

// emptyResolver builds a resolver with no hooks at all — BuildPipeline
// returns a nil pipeline for every stage, which keeps compliance enabled
// (audit emission still runs) without any hook influencing decisions.
func emptyResolver(t *testing.T) *compliance.PolicyResolver {
	t.Helper()
	return compliance.NewPolicyResolver(nil, core.NewHookRegistry(), discardSlog())
}

// singleDomainEngine returns a domain engine matching api.example.com
// exactly with the given default path action.
func singleDomainEngine(t *testing.T, action domain.PathAction) *domain.Engine {
	t.Helper()
	eng := domain.NewEngine()
	if err := eng.Swap([]domain.InterceptionDomain{
		{
			ID:                "dom-1",
			Name:              "example",
			HostPattern:       "api.example.com",
			HostMatchType:     domain.HostMatchExact,
			DefaultPathAction: action,
			Enabled:           true,
			Priority:          10,
		},
	}); err != nil {
		t.Fatalf("engine swap: %v", err)
	}
	return eng
}

// TestForwardHandler_AttestationValid_PurePassthrough: a request whose
// attestation verifies must be forwarded as pure passthrough — no audit
// row, no compliance markers, attestation header stripped from the
// upstream request, upstream body relayed verbatim.
func TestForwardHandler_AttestationValid_PurePassthrough(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver: emptyResolver(t),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
		attestationVerifier: func(context.Context, string) (bool, string) {
			return true, "agent-1"
		},
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	req := newBumpedRequest()
	req.Header.Set(AttestationHeaderName, "v1:sig")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 relayed from upstream", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "secret-body") {
		t.Fatalf("upstream body missing from passthrough relay; got %q", rec.Body.String())
	}
	if got := rt.calls(); got != 1 {
		t.Fatalf("upstream forwards = %d, want 1", got)
	}
	if v := rt.header(0).Get(AttestationHeaderName); v != "" {
		t.Fatalf("attestation header leaked to upstream: %q — must be stripped before forward", v)
	}
	if events := writer.snapshot(); len(events) != 0 {
		t.Fatalf("audit events = %d, want 0 — attested traffic must skip audit entirely (the agent's row is the system-of-record)", len(events))
	}
	if via := rec.Header().Get("X-Nexus-Via"); via != "" {
		t.Fatalf("X-Nexus-Via = %q, want unset — passthrough must not claim inspection", via)
	}
}

// TestForwardHandler_AttestationInvalid_FallsThroughToInspection: a failed
// attestation verification must NOT refuse the request — it falls through
// to the normal MITM path (fail-open verifier contract): upstream
// forwarded, audit row emitted, markers stamped.
func TestForwardHandler_AttestationInvalid_FallsThroughToInspection(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver: emptyResolver(t),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
		attestationVerifier: func(context.Context, string) (bool, string) {
			return false, ""
		},
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — invalid attestation falls through to inspection, never refuses", rec.Code)
	}
	if got := rt.calls(); got != 1 {
		t.Fatalf("upstream forwards = %d, want 1", got)
	}
	if events := writer.snapshot(); len(events) != 1 {
		t.Fatalf("audit events = %d, want 1 — the MITM path must audit the request", len(events))
	}
	if via := rec.Header().Get("X-Nexus-Via"); via == "" {
		t.Fatal("X-Nexus-Via missing — the inspected path must stamp the via marker")
	}
}

// TestForwardHandler_DomainBlock_Refuses403: a matched domain whose path
// action is BLOCK must refuse with a plain 403 before any upstream
// contact and before any audit emission.
func TestForwardHandler_DomainBlock_Refuses403(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver: emptyResolver(t),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
		domainEngine:   singleDomainEngine(t, domain.PathActionBlock),
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a BLOCK path action", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Request blocked by compliance policy") {
		t.Fatalf("refusal body = %q, want the compliance-policy block message", rec.Body.String())
	}
	if got := rt.calls(); got != 0 {
		t.Fatalf("upstream forwards = %d, want 0 — BLOCK must refuse before any upstream send", got)
	}
	if events := writer.snapshot(); len(events) != 0 {
		t.Fatalf("audit events = %d, want 0 on the BLOCK fast refusal", len(events))
	}
}

// TestForwardHandler_DomainPassthrough_SkipsCompliance: a matched domain
// whose path action is PASSTHROUGH must forward without running hooks or
// emitting audit for that request, while still relaying the upstream body.
func TestForwardHandler_DomainPassthrough_SkipsCompliance(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		// A REJECT_HARD request hook that must NOT run on the passthrough path.
		policyResolver: decidingResolver(t, "request", core.RejectHard, "should never run", "NEVER"),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
		domainEngine:   singleDomainEngine(t, domain.PathActionPassthrough),
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — PASSTHROUGH must not let the reject hook run", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "secret-body") {
		t.Fatalf("upstream body missing from passthrough relay; got %q", rec.Body.String())
	}
	if got := rt.calls(); got != 1 {
		t.Fatalf("upstream forwards = %d, want 1", got)
	}
	if events := writer.snapshot(); len(events) != 0 {
		t.Fatalf("audit events = %d, want 0 — PASSTHROUGH skips compliance audit for the request", len(events))
	}
}

// TestForwardHandler_RequestHookRejectHard_RichReject: a request hook that
// hard-rejects on a richReject=true caller (compliance-proxy) must produce a
// 403 refusal with the attributed reject body (blocked_by_policy + the hook's
// rule-ID reason), stamp deny markers + the correlation ID, audit the
// decision, and never contact upstream.
func TestForwardHandler_RequestHookRejectHard_RichReject(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver: decidingResolver(t, "request", core.RejectHard, "pii detected", "PII_BLOCK"),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
		rejectConfig:   RejectConfig{DefaultLevel: RejectLevelDetailed},
		richReject:     true,
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	req := newBumpedRequest()
	req.Header.Set("X-Nexus-Request-Id", "tx-pin-reject")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 on REJECT_HARD", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "blocked_by_policy") || !strings.Contains(body, "pii detected") {
		t.Fatalf("reject body = %q, want blocked_by_policy with the hook reason (detailed level)", body)
	}
	if got := rt.calls(); got != 0 {
		t.Fatalf("upstream forwards = %d, want 0 — the rejected request must never reach upstream", got)
	}
	if mode := rec.Header().Get("X-Nexus-Mode"); mode != "deny" {
		t.Fatalf("X-Nexus-Mode = %q, want %q on the reject path", mode, "deny")
	}
	if rid := rec.Header().Get("X-Nexus-Request-Id"); rid != "tx-pin-reject" {
		t.Fatalf("X-Nexus-Request-Id = %q, want the client-supplied correlation id", rid)
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want exactly 1 reject row", len(events))
	}
	e := events[0]
	if e.BumpStatus != "BUMP_SUCCESS" {
		t.Fatalf("BumpStatus = %q, want BUMP_SUCCESS (the bump itself succeeded; the hook rejected)", e.BumpStatus)
	}
	if e.RequestHookDecision != string(core.RejectHard) {
		t.Fatalf("RequestHookDecision = %q, want %q", e.RequestHookDecision, core.RejectHard)
	}
	if e.StatusCode == nil || *e.StatusCode != http.StatusForbidden {
		t.Fatalf("StatusCode = %v, want 403", e.StatusCode)
	}
	if e.TransactionID != "tx-pin-reject" {
		t.Fatalf("TransactionID = %q, want the client-supplied correlation id", e.TransactionID)
	}
}

// TestForwardHandler_RequestHookRejectHard_MinimalReject: the agent-path
// contrast — the same hard-reject on a richReject=false caller (the on-host
// transparent interceptor) must produce a 403 with a MINIMAL "Forbidden" body
// that carries NO attribution (no blocked_by_policy envelope, no hook reason),
// while still stamping deny markers, auditing the decision, and never
// contacting upstream.
func TestForwardHandler_RequestHookRejectHard_MinimalReject(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver: decidingResolver(t, "request", core.RejectHard, "pii detected", "PII_BLOCK"),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
		// A Detailed reject config that MUST be ignored because richReject is
		// false — the agent never synthesizes a rich error page.
		rejectConfig: RejectConfig{DefaultLevel: RejectLevelDetailed},
		richReject:   false,
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 on REJECT_HARD", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "blocked_by_policy") {
		t.Fatalf("minimal reject body = %q, must NOT contain the blocked_by_policy attribution envelope", body)
	}
	if strings.Contains(body, "pii detected") || strings.Contains(body, "PII_BLOCK") {
		t.Fatalf("minimal reject body = %q, must NOT echo the hook reason / reason code", body)
	}
	if !strings.Contains(body, "Forbidden") {
		t.Fatalf("minimal reject body = %q, want the bare \"Forbidden\" 403 text", body)
	}
	if got := rt.calls(); got != 0 {
		t.Fatalf("upstream forwards = %d, want 0 — the rejected request must never reach upstream", got)
	}
	// The deny marker is still stamped even on the minimal-body path.
	if mode := rec.Header().Get("X-Nexus-Mode"); mode != "deny" {
		t.Fatalf("X-Nexus-Mode = %q, want %q on the reject path", mode, "deny")
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want exactly 1 reject row", len(events))
	}
	if events[0].RequestHookDecision != string(core.RejectHard) {
		t.Fatalf("RequestHookDecision = %q, want %q", events[0].RequestHookDecision, core.RejectHard)
	}
	if events[0].StatusCode == nil || *events[0].StatusCode != http.StatusForbidden {
		t.Fatalf("StatusCode = %v, want 403", events[0].StatusCode)
	}
}

// TestForwardHandler_UpstreamError_502WithAuditRow: an upstream dial/send
// failure must answer 502 Bad Gateway and still leave an audit row (with
// no usage, since no body was ever received).
func TestForwardHandler_UpstreamError_502WithAuditRow(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{err: errors.New("dial upstream: connection refused")}
	bo := &bumpOptions{
		policyResolver: emptyResolver(t),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 on upstream failure", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Bad Gateway") {
		t.Fatalf("body = %q, want Bad Gateway", rec.Body.String())
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1 — upstream failures must still be audited", len(events))
	}
	if events[0].StatusCode == nil || *events[0].StatusCode != http.StatusBadGateway {
		t.Fatalf("StatusCode = %v, want 502", events[0].StatusCode)
	}
	if events[0].UsageExtractionStatus != "no_body" {
		t.Fatalf("UsageExtractionStatus = %q, want no_body — upstream failed before any body", events[0].UsageExtractionStatus)
	}
}

// TestForwardHandler_NonAIFastPath_RelaysAndAuditsNonLLM: non-AI traffic
// with no response hooks stays on the stream-through fast path — body
// relayed, audited with non_llm usage status, and a generated correlation
// ID stamped on the upstream request.
func TestForwardHandler_NonAIFastPath_RelaysAndAuditsNonLLM(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver: emptyResolver(t),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest()) // no X-Nexus-Request-Id supplied

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "secret-body") {
		t.Fatalf("upstream body missing from fast-path relay; got %q", rec.Body.String())
	}
	if got := rt.calls(); got != 1 {
		t.Fatalf("upstream forwards = %d, want 1", got)
	}
	upstreamTx := rt.header(0).Get("X-Nexus-Request-Id")
	if upstreamTx == "" {
		t.Fatal("X-Nexus-Request-Id missing on the upstream request — the handler must generate one when the client omits it")
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1 fast-path row", len(events))
	}
	if events[0].UsageExtractionStatus != "non_llm" {
		t.Fatalf("UsageExtractionStatus = %q, want non_llm on the non-AI fast path", events[0].UsageExtractionStatus)
	}
	if events[0].TransactionID != upstreamTx {
		t.Fatalf("TransactionID = %q, want the generated id %q stamped upstream", events[0].TransactionID, upstreamTx)
	}
	if via := rec.Header().Get("X-Nexus-Via"); via == "" {
		t.Fatal("X-Nexus-Via missing — the inspected relay must stamp the via marker")
	}
}

// TestForwardHandler_ResponseHookRejectHard_RichReject_NoUpstreamLeak: a
// response hook that hard-rejects on a richReject=true caller must suppress
// the upstream body and answer 451 with the attributed reject body. The
// attributed body is built from the hook's rule-ID/label reason only — it must
// NEVER echo the upstream's original sensitive value. The negative assertion
// constructs an upstream whose body carries an injected sensitive marker and
// asserts that marker never appears in the synthesized reject body.
func TestForwardHandler_ResponseHookRejectHard_RichReject_NoUpstreamLeak(t *testing.T) {
	const sensitiveUpstreamMarker = "SSN-123-45-6789-LEAKED"
	leakyUpstream := func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"upstream":"secret-body","detail":"` + sensitiveUpstreamMarker + `"}`)),
		}
	}
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: leakyUpstream}
	bo := &bumpOptions{
		policyResolver: decidingResolver(t, "response", core.RejectHard, "policy text in response", "RESP_BLOCK"),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
		rejectConfig:   RejectConfig{DefaultLevel: RejectLevelDetailed},
		richReject:     true,
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if rec.Code != http.StatusUnavailableForLegalReasons {
		t.Fatalf("status = %d, want 451 on response REJECT_HARD", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "secret-body") {
		t.Fatalf("client received the blocked upstream body %q — it must be suppressed", body)
	}
	// Negative assertion: the synthesized reject body must carry the hook's
	// rule-ID/label only, never the upstream's original sensitive value.
	if strings.Contains(body, sensitiveUpstreamMarker) {
		t.Fatalf("reject body leaked the upstream sensitive value %q; body = %q", sensitiveUpstreamMarker, body)
	}
	// The rule-ID/label reason IS expected in the attributed body.
	if !strings.Contains(body, "RESP_BLOCK") && !strings.Contains(body, "policy text in response") {
		t.Fatalf("reject body = %q, want the rule-ID/label attribution (reason or reason code)", body)
	}
	if mode := rec.Header().Get("X-Nexus-Mode"); mode != "deny" {
		t.Fatalf("X-Nexus-Mode = %q, want %q on the response reject path", mode, "deny")
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want exactly 1 response-reject row", len(events))
	}
	e := events[0]
	// The response pipeline's decision must land in the RESPONSE-stage
	// columns: an admin triaging "which stage blocked this?" on the
	// Traffic page reads the per-stage decisions, and a response-hook
	// reject misfiled under the request stage points them at the wrong
	// hook config. No request hook ran here, so the request column
	// stays empty.
	if e.ResponseHookDecision == nil || *e.ResponseHookDecision != string(core.RejectHard) {
		t.Fatalf("ResponseHookDecision = %v, want %q (the response stage made this decision)", e.ResponseHookDecision, core.RejectHard)
	}
	if e.RequestHookDecision != "" {
		t.Fatalf("RequestHookDecision = %q, want empty — no request hook ran", e.RequestHookDecision)
	}
	if e.StatusCode == nil || *e.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %v, want the upstream 200 recorded on the audit row", e.StatusCode)
	}
}

// TestForwardHandler_SSEResponse_DispatchesToStreamingPath: an upstream
// text/event-stream response must route through the streaming handler
// (not the buffered relay): with no streaming-policy store wired the
// conservative passthrough mode relays the stream bytes, and the
// inspection markers are stamped before the first flush.
func TestForwardHandler_SSEResponse_DispatchesToStreamingPath(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: sseUpstreamResponse}
	bo := &bumpOptions{
		policyResolver: emptyResolver(t),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "sse-secret") {
		t.Fatalf("SSE bytes missing from the streaming relay; got %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want the upstream SSE content type relayed", ct)
	}
	if via := rec.Header().Get("X-Nexus-Via"); via == "" {
		t.Fatal("X-Nexus-Via missing — the SSE path must stamp markers before the first flush")
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1 row for the relayed SSE stream", len(events))
	}
}

// errorBody is a request body whose read fails mid-stream, simulating a
// client that disconnects while the proxy is buffering the request.
type errorBody struct{}

func (errorBody) Read([]byte) (int, error) { return 0, errors.New("client reset") }

func (errorBody) Close() error { return nil }

// TestForwardHandler_RequestBodyReadError_FailsOpenToUpstream: when the
// request body cannot be read for compliance inspection, the request must
// still be forwarded (fail-open: an inspection I/O error never blocks
// traffic) and no hook decision is recorded.
func TestForwardHandler_RequestBodyReadError_FailsOpenToUpstream(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver: decidingResolver(t, "request", core.RejectHard, "must not run", "NEVER"),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	req := newBumpedRequest()
	req.Body = errorBody{}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a body-read failure must fall through to forward, not refuse", rec.Code)
	}
	if got := rt.calls(); got != 1 {
		t.Fatalf("upstream forwards = %d, want 1 (fail-open on inspection I/O error)", got)
	}
	if events := writer.snapshot(); len(events) != 0 {
		t.Fatalf("audit events = %d, want 0 — no audit context exists when the body read failed", len(events))
	}
}

// TestForwardHandler_ResponseBodyReadError_FailsOpenEmptyBody: when a
// response hook needs the buffered body but the upstream body read fails,
// the request must complete fail-open — the audit row records the
// approve-with-no-body outcome and the client gets the (empty-bodied)
// upstream status rather than an error.
func TestForwardHandler_ResponseBodyReadError_FailsOpenEmptyBody(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       errorBody{},
		}
	}}
	bo := &bumpOptions{
		// An approve response hook forces the buffered path (needBuffer).
		policyResolver: decidingResolver(t, "response", core.Approve, "", ""),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unreadable response body lets the response through fail-open", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("client body = %q, want empty — the unreadable body is replaced, never partially relayed", body)
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1 fail-open row", len(events))
	}
	if events[0].UsageExtractionStatus != "no_body" {
		t.Fatalf("UsageExtractionStatus = %q, want no_body when the response body was unreadable", events[0].UsageExtractionStatus)
	}
}

// TestForwardHandler_CredentialsForwardedAndUAAudited: credentials must
// still reach the upstream (the proxy never strips client auth from the
// wire), and the audit row's UserAgent must be extracted from the
// sanitized header snapshot taken at request time.
func TestForwardHandler_CredentialsForwardedAndUAAudited(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &capturingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver: emptyResolver(t),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	req := newBumpedRequest()
	req.Header.Set("Authorization", "Bearer sk-secret")
	req.Header.Set("User-Agent", "pin-client/1.0")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rt.calls(); got != 1 {
		t.Fatalf("upstream forwards = %d, want 1", got)
	}
	if rt.header(0).Get("Authorization") != "Bearer sk-secret" {
		t.Fatal("Authorization header must still reach the upstream")
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	if events[0].UserAgent == nil || *events[0].UserAgent != "pin-client/1.0" {
		t.Fatalf("UserAgent = %v, want pin-client/1.0 extracted from the request headers", events[0].UserAgent)
	}
}

// TestForwardHandler_PassthroughDoesNotLatchTunnel: a per-path PASSTHROUGH
// decision must scope to exactly ONE request — on a keep-alive tunnel the
// next request through the SAME handler must still run compliance. The
// per-tunnel/per-request state split makes "request-local override latches
// tunnel state" a possible regression shape, so it gets an explicit pin:
// request 1 hits a PASSTHROUGH path (forwarded, no hook), request 2 hits a
// PROCESS path and MUST be rejected by the request hook.
func TestForwardHandler_PassthroughDoesNotLatchTunnel(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &countingRoundTripper{makeResp: jsonUpstream}
	eng := domain.NewEngine()
	if err := eng.Swap([]domain.InterceptionDomain{
		{
			ID:                "dom-latch",
			Name:              "example",
			HostPattern:       "api.example.com",
			HostMatchType:     domain.HostMatchExact,
			DefaultPathAction: domain.PathActionProcess,
			Paths: []domain.InterceptionPath{
				{
					ID:          "p-skip",
					PathPattern: []string{"/skip"},
					MatchType:   domain.PathMatchExact,
					Action:      domain.PathActionPassthrough,
				},
			},
			Enabled:  true,
			Priority: 10,
		},
	}); err != nil {
		t.Fatalf("engine swap: %v", err)
	}
	bo := &bumpOptions{
		policyResolver: decidingResolver(t, "request", core.RejectHard, "blocked by test", "TEST_BLOCK"),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
		domainEngine:   eng,
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "https://api.example.com/skip", strings.NewReader(`{"a":1}`)))
	if rec1.Code != http.StatusOK {
		t.Fatalf("request 1 (PASSTHROUGH path) status = %d, want 200 forwarded without hooks", rec1.Code)
	}
	if got := rt.count(); got != 1 {
		t.Fatalf("request 1 upstream forwards = %d, want 1", got)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat", strings.NewReader(`{"b":2}`)))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("request 2 (PROCESS path) status = %d, want 403 — a passthrough request must not disable compliance for the rest of the tunnel", rec2.Code)
	}
	if got := rt.count(); got != 1 {
		t.Fatalf("upstream forwards after rejected request 2 = %d, want still 1 (rejected request must not reach upstream)", got)
	}
}
