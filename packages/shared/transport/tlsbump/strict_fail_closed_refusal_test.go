package tlsbump

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// These tests pin the strict fail-closed contract at the three tlsbump
// pipeline BUILD sites beyond the connection stage (which is covered by the
// compliance-proxy listener test): request stage, non-SSE response stage, and
// the SSE live/buffer stages. The compliance-proxy appliance sets
// WithStrictFailClosed, so an UNBUILDABLE fail-closed hook must REFUSE the
// traffic (502 / aborted relay), never forward or relay it uninspected. The
// agent NE host-packet path leaves strict unset and must keep relaying
// (fail-open) so a hook-config error can never take down host networking —
// each strict case below carries a fail-open contrast subtest proving the
// same setup relays when strict is off.

func discardSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordingAuditWriter captures every enqueued audit event so tests can
// assert which BumpStatus the refusal path emitted.
type recordingAuditWriter struct {
	mu     sync.Mutex
	events []audit.AuditEvent
}

func (w *recordingAuditWriter) Enqueue(e audit.AuditEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, e)
}

func (w *recordingAuditWriter) Flush(context.Context) error { return nil }

func (w *recordingAuditWriter) Close(context.Context) error { return nil }

func (w *recordingAuditWriter) snapshot() []audit.AuditEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]audit.AuditEvent, len(w.events))
	copy(out, w.events)
	return out
}

// countingRoundTripper counts upstream forward attempts and serves a canned
// response, standing in for the real uTLS upstream dial.
type countingRoundTripper struct {
	mu       sync.Mutex
	calls    int
	makeResp func() *http.Response
}

func (rt *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.calls++
	rt.mu.Unlock()
	if rt.makeResp == nil {
		return nil, errors.New("no upstream response configured")
	}
	return rt.makeResp(), nil
}

func (rt *countingRoundTripper) count() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.calls
}

// unbuildableFailClosedResolver returns a resolver whose ONLY hook at the
// given stage is FailBehavior=fail-closed AND backed by a factory that
// errors, so BuildPipeline(stage, …, strict=true, …) returns an error — the
// precondition for every refusal branch under test.
func unbuildableFailClosedResolver(t *testing.T, stage string) *compliance.PolicyResolver {
	t.Helper()
	reg := core.NewHookRegistry()
	reg.Register("erroring-fc", func(_ *core.HookConfig) (core.Hook, error) {
		return nil, errors.New("factory boom")
	})
	return compliance.NewPolicyResolver([]core.HookConfig{
		{
			ID:                "h-err-fc",
			ImplementationID:  "erroring-fc",
			Name:              "erroring-fail-closed-hook",
			Stage:             stage,
			Enabled:           true,
			FailBehavior:      "fail-closed",
			ApplicableIngress: []string{"ALL"},
		},
	}, reg, discardSlog())
}

// jsonUpstream returns a canned non-SSE upstream response whose body marker
// lets tests assert whether upstream bytes were relayed to the client.
func jsonUpstream() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"upstream":"secret-body"}`)),
	}
}

func newBumpedRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat", strings.NewReader(`{"prompt":"hi"}`))
}

// TestForwardHandler_RequestStage_StrictUnbuildable_RefusesWithoutForward:
// with strict fail-closed, an unbuildable fail-closed REQUEST hook must 502
// the request, never forward it upstream, and audit the refusal as a
// pipeline-build failure (not an Approve row).
func TestForwardHandler_RequestStage_StrictUnbuildable_RefusesWithoutForward(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &countingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver:   unbuildableFailClosedResolver(t, "request"),
		auditEmitter:     compliance.NewAuditEmitter(writer, discardSlog()),
		strictFailClosed: true,
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — an unbuildable fail-closed request hook must refuse, not forward uninspected", rec.Code)
	}
	if got := rt.count(); got != 0 {
		t.Fatalf("upstream forwards = %d, want 0 — the request must be refused BEFORE any upstream send", got)
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want exactly 1 refusal row", len(events))
	}
	if events[0].BumpStatus != "BUMP_PIPELINE_BUILD_FAILED" {
		t.Fatalf("BumpStatus = %q, want BUMP_PIPELINE_BUILD_FAILED — the refusal must be auditable as a build failure", events[0].BumpStatus)
	}
}

// TestForwardHandler_RequestStage_NonStrictUnbuildable_ForwardsFailOpen is
// the agent-NE-path contrast: the same unbuildable fail-closed hook with
// strict unset must fall through to forward (fail-open) so a hook-config
// error never bricks host networking.
func TestForwardHandler_RequestStage_NonStrictUnbuildable_ForwardsFailOpen(t *testing.T) {
	rt := &countingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver: unbuildableFailClosedResolver(t, "request"),
		// strictFailClosed deliberately unset (agent NE host path).
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if got := rt.count(); got != 1 {
		t.Fatalf("upstream forwards = %d, want 1 — the non-strict path must stay fail-open and forward", got)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 relayed from upstream on the fail-open path", rec.Code)
	}
}

// TestForwardHandler_ResponseStage_StrictUnbuildable_RefusesWithoutRelay:
// with strict fail-closed, an unbuildable fail-closed RESPONSE hook must 502
// instead of relaying the (uninspected) upstream body, and the audit row must
// be a reject — not the Approve row the old half-built fix emitted.
func TestForwardHandler_ResponseStage_StrictUnbuildable_RefusesWithoutRelay(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &countingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver:   unbuildableFailClosedResolver(t, "response"),
		auditEmitter:     compliance.NewAuditEmitter(writer, discardSlog()),
		strictFailClosed: true,
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — an unbuildable fail-closed response hook must refuse the response", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "secret-body") {
		t.Fatalf("client received upstream body %q — the uninspected response must NOT be relayed", body)
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want exactly 1 refusal row", len(events))
	}
	if events[0].BumpStatus != "BUMP_PIPELINE_BUILD_FAILED" {
		t.Fatalf("BumpStatus = %q, want BUMP_PIPELINE_BUILD_FAILED — the refusal must be audited as a reject, not Approve", events[0].BumpStatus)
	}
}

// TestForwardHandler_ResponseStage_NonStrictUnbuildable_RelaysFailOpen is the
// agent-NE-path contrast for the response stage: strict unset must relay the
// upstream body (fail-open) and audit it as a success row.
func TestForwardHandler_ResponseStage_NonStrictUnbuildable_RelaysFailOpen(t *testing.T) {
	writer := &recordingAuditWriter{}
	rt := &countingRoundTripper{makeResp: jsonUpstream}
	bo := &bumpOptions{
		policyResolver: unbuildableFailClosedResolver(t, "response"),
		auditEmitter:   compliance.NewAuditEmitter(writer, discardSlog()),
		// strictFailClosed deliberately unset (agent NE host path).
	}
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newBumpedRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the non-strict response path must stay fail-open", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "secret-body") {
		t.Fatalf("upstream body missing from relay on the fail-open path; got %q", body)
	}
}

// sseUpstreamResponse builds a fake upstream SSE response carrying a marker
// payload so tests can assert whether stream bytes reached the client.
func sseUpstreamResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: sse-secret\n\n")),
	}
}

// sseBumpOptions wires the minimal bumpOptions handleSSEResponse needs to
// reach the live/buffer BuildPipeline call: a streaming-policy store pinned
// to the requested mode, the unbuildable fail-closed response resolver, and
// a recording audit emitter so abort/relay rows can be asserted.
func sseBumpOptions(t *testing.T, mode streampolicy.Mode, strict bool, writer *recordingAuditWriter) *bumpOptions {
	t.Helper()
	store := streampolicy.NewStore(streampolicy.Policy{
		Mode:           mode,
		ChunkBytes:     1024,
		HookTimeoutMs:  1000,
		MaxBufferBytes: 1 << 20,
		FailBehavior:   streampolicy.FailOpen,
	})
	return &bumpOptions{
		policyResolver:       unbuildableFailClosedResolver(t, "response"),
		streamingPolicyStore: store,
		strictFailClosed:     strict,
		auditEmitter:         compliance.NewAuditEmitter(writer, discardSlog()),
	}
}

func runSSE(t *testing.T, bo *bumpOptions) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	respInput := &core.HookInput{Stage: "response", TargetHost: "api.example.com", IngressType: "COMPLIANCE_PROXY"}
	auditInfo := &compliance.AuditInfo{TransactionID: "tx-sse-strict"}
	audCtx := &requestAuditCtx{input: respInput, info: *auditInfo}
	handleSSEResponse(context.Background(), rec, sseUpstreamResponse(), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())
	return rec
}

// assertStrictRefused: the SSE-entry strict guard refuses an unbuildable
// fail-closed stream with a clean 451 BEFORE any header/byte is written (the
// guard runs ahead of the header copy), so no upstream byte reaches the client
// and the refusal carries a response-stage reject audit row.
func assertStrictRefused(t *testing.T, rec *httptest.ResponseRecorder, writer *recordingAuditWriter, mode string) {
	t.Helper()
	if rec.Code != http.StatusUnavailableForLegalReasons {
		t.Fatalf("%s mode status = %d, want 451 — the strict guard must refuse before relaying", mode, rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "sse-secret") {
		t.Fatalf("client received SSE bytes %q — strict fail-closed must refuse the %s stream, not pass it uninspected", body, mode)
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want exactly 1 refusal row", len(events))
	}
	if events[0].ResponseHookDecision == nil || *events[0].ResponseHookDecision != string(core.RejectHard) {
		t.Fatalf("ResponseHookDecision = %v, want %q (the response-stage build failure refused the stream)", events[0].ResponseHookDecision, core.RejectHard)
	}
}

// TestSSE_LiveMode_StrictUnbuildable_Refuses451: in live (chunked_async)
// mode under strict fail-closed, an unbuildable fail-closed response hook
// must abort the relay and audit the abort.
func TestSSE_LiveMode_StrictUnbuildable_Refuses451(t *testing.T) {
	writer := &recordingAuditWriter{}
	rec := runSSE(t, sseBumpOptions(t, streampolicy.ModeChunkedAsync, true, writer))
	assertStrictRefused(t, rec, writer, "live")
}

// TestSSE_LiveMode_NonStrictUnbuildable_RelaysFailOpen is the agent-NE-path
// contrast: same unbuildable hook, strict unset → passthrough relay.
func TestSSE_LiveMode_NonStrictUnbuildable_RelaysFailOpen(t *testing.T) {
	writer := &recordingAuditWriter{}
	rec := runSSE(t, sseBumpOptions(t, streampolicy.ModeChunkedAsync, false, writer))
	if body := rec.Body.String(); !strings.Contains(body, "sse-secret") {
		t.Fatalf("SSE bytes missing from fail-open relay; got %q", body)
	}
}

// TestSSE_BufferMode_StrictUnbuildable_Refuses451: same contract for buffer
// (buffer_full_block) mode — strict + unbuildable fail-closed hook must abort
// the relay (and audit it) instead of falling back to passthrough.
func TestSSE_BufferMode_StrictUnbuildable_Refuses451(t *testing.T) {
	writer := &recordingAuditWriter{}
	rec := runSSE(t, sseBumpOptions(t, streampolicy.ModeBufferFullBlock, true, writer))
	assertStrictRefused(t, rec, writer, "buffer")
}

// TestSSE_BufferMode_NonStrictUnbuildable_RelaysFailOpen is the buffer-mode
// fail-open contrast for the agent NE host path.
func TestSSE_BufferMode_NonStrictUnbuildable_RelaysFailOpen(t *testing.T) {
	writer := &recordingAuditWriter{}
	rec := runSSE(t, sseBumpOptions(t, streampolicy.ModeBufferFullBlock, false, writer))
	if body := rec.Body.String(); !strings.Contains(body, "sse-secret") {
		t.Fatalf("SSE bytes missing from fail-open relay; got %q", body)
	}
}
