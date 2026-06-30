package tlsbump

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// TestOverrideStreamingModeForScope exhaustively pins the scope-routing decision
// table (mirror ai-gateway stream_shape.go §B2).
func TestOverrideStreamingModeForScope(t *testing.T) {
	cases := []struct {
		name                         string
		mode                         string
		mayBlock, mayRedact, adapter bool
		want                         string
	}{
		{"block overrides live", "live", true, false, true, "buffer"},
		{"block overrides passthrough", "passthrough", true, false, true, "buffer"},
		{"block beats redact", "live", true, true, true, "buffer"},
		{"redact live + adapter → modela", "live", false, true, true, "modela"},
		{"redact live no adapter → buffer", "live", false, true, false, "buffer"},
		{"redact passthrough → buffer", "passthrough", false, true, true, "buffer"},
		{"redact already buffer stays buffer", "buffer", false, true, true, "buffer"},
		{"non-enforcing live unchanged", "live", false, false, true, "live"},
		{"non-enforcing passthrough unchanged", "passthrough", false, false, true, "passthrough"},
		{"non-enforcing buffer unchanged", "buffer", false, false, true, "buffer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := overrideStreamingModeForScope(c.mode, c.mayBlock, c.mayRedact, c.adapter); got != c.want {
				t.Fatalf("override(%q, block=%v, redact=%v, adapter=%v) = %q, want %q",
					c.mode, c.mayBlock, c.mayRedact, c.adapter, got, c.want)
			}
		})
	}
}

// TestModelaResultEnforcing pins the storage gate: only block/redact outcomes are
// enforcing (their best-effort wire capture must NOT be persisted — it may hold a raw
// over-window prefix); approve / abstain / nil are non-enforcing (the benign original
// is safe to persist).
func TestModelaResultEnforcing(t *testing.T) {
	cases := []struct {
		name string
		res  *core.CompliancePipelineResult
		want bool
	}{
		{"nil → not enforcing", nil, false},
		{"approve → not enforcing", &core.CompliancePipelineResult{Decision: core.Approve}, false},
		{"abstain → not enforcing", &core.CompliancePipelineResult{Decision: core.Abstain}, false},
		{"reject-hard → enforcing", &core.CompliancePipelineResult{Decision: core.RejectHard}, true},
		{"modify → enforcing", &core.CompliancePipelineResult{Decision: core.Modify}, true},
		{"block-soft → enforcing", &core.CompliancePipelineResult{Decision: core.BlockSoft}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := modelaResultEnforcing(c.res); got != c.want {
				t.Fatalf("modelaResultEnforcing(%+v) = %v, want %v", c.res, got, c.want)
			}
		})
	}
}

// lastResponseBodyPayload returns the persisted response-body column payload of the
// most recent audit event (empty ⇒ stored NULL).
func lastResponseBodyPayload(t *testing.T, w *recordingAuditWriter) []byte {
	t.Helper()
	evs := w.snapshot()
	if len(evs) == 0 {
		t.Fatal("no audit event emitted")
	}
	payload, _ := evs[len(evs)-1].ResponseBody.ColumnPayload()
	return payload
}

// TestSSE_ModelA_Storage_CleanPersists_EnforcingNull is the T3 storage-parity
// end-to-end guard (asserts the PERSISTED body, not just the wire): an enforcing
// (redact) modela stream stores NULL — never the raw best-effort prefix — while a
// clean (prescan-miss, approve-at-EOF) stream persists the delivered original.
func TestSSE_ModelA_Storage_CleanPersists_EnforcingNull(t *testing.T) {
	// Enforcing redact → NULL.
	redact := modelATestHook{
		decision: core.Modify,
		modified: []core.ContentBlock{{Type: "text", Text: "card [CARD]"}},
		spans:    []normalize.TransformSpan{tspan("messages.0.content.0", 5, 21, "[CARD]")},
		matchRaw: true,
	}
	bo, w := modelABumpOptions(modelARedactResolver(redact))
	respInput, auditInfo, audCtx := modelAAudCtx()
	handleSSEResponse(context.Background(), httptest.NewRecorder(), sseFrameUpstream(oaContentBody("card ", "4111111111111111")), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())
	if body := lastResponseBodyPayload(t, w); len(body) != 0 {
		t.Fatalf("enforcing modela stream must persist a NULL response body, got %d bytes: %q", len(body), body)
	}

	// Clean prescan-miss → approve at EOF → the delivered original is persisted.
	clean := modelATestHook{decision: core.Modify, matchRaw: false}
	bo2, w2 := modelABumpOptions(modelARedactResolver(clean))
	respInput2, auditInfo2, audCtx2 := modelAAudCtx()
	handleSSEResponse(context.Background(), httptest.NewRecorder(), sseFrameUpstream(oaContentBody("hello ", "world")), audCtx2, respInput2, auditInfo2, bo2, discardSlog(), time.Now())
	if body := lastResponseBodyPayload(t, w2); len(body) == 0 {
		t.Fatal("clean modela stream must persist the delivered original, got NULL")
	}
}

// modelATestHook is a configurable response hook for the wire Model-A path: its
// declarative onMatch is "redact" (so MayRedact routes the flow to modela), it
// returns a fixed runtime decision, and it acts as a RawContentPrescanner whose
// MayMatchRaw verdict the test controls (to drive prescan HIT vs MISS).
type modelATestHook struct {
	decision core.Decision
	modified []core.ContentBlock
	spans    []normalize.TransformSpan
	matchRaw bool
}

func (h modelATestHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{
		Decision:        h.decision,
		Reason:          "redact",
		ReasonCode:      "PII",
		ModifiedContent: h.modified,
		TransformSpans:  h.spans,
	}, nil
}
func (modelATestHook) SupportsEndpoint(core.EndpointType) bool { return true }
func (modelATestHook) SupportsModality(core.Modality) bool     { return true }
func (modelATestHook) ScansContent() bool                      { return true }
func (h modelATestHook) MayMatchRaw([]byte) bool               { return h.matchRaw }

// modelARedactResolver wires a single response-stage hook whose onMatch is "redact"
// (→ MayRedact true → modela routing) and whose runtime behaviour is h.
func modelARedactResolver(h modelATestHook) *compliance.PolicyResolver {
	reg := core.NewHookRegistry()
	reg.Register("modela-test", func(_ *core.HookConfig) (core.Hook, error) { return h, nil })
	return compliance.NewPolicyResolver([]core.HookConfig{{
		ID:                "h-modela",
		ImplementationID:  "modela-test",
		Name:              "modela-test",
		Stage:             "response",
		Enabled:           true,
		FailBehavior:      "fail-open",
		ApplicableIngress: []string{"ALL"},
		Config:            map[string]any{"onMatch": map[string]any{"action": "redact"}},
	}}, reg, discardSlog())
}

// modelACoFiringResolver wires TWO response-stage hooks, BOTH with onMatch "redact" (so the
// static scope is MayRedact && !MayBlock → modela routing, NOT buffer): a redact hook that
// returns Modify + ModifiedContent/spans, and a soft-block hook that returns BlockSoft. The
// pipeline aggregator ranks BlockSoft above Modify (Decision→BlockSoft) while carrying the
// redact's ModifiedContent/spans — the genuine co-firing shape leak #2 needs. (A soft-block
// hook declaring onMatch "block" would set MayBlock and route to buffer, never reaching the
// modela wire, so both hooks MUST be redact-scoped.)
func modelACoFiringResolver(redactHook, softBlockHook modelATestHook) *compliance.PolicyResolver {
	reg := core.NewHookRegistry()
	reg.Register("modela-redact", func(_ *core.HookConfig) (core.Hook, error) { return redactHook, nil })
	reg.Register("modela-softblock", func(_ *core.HookConfig) (core.Hook, error) { return softBlockHook, nil })
	redactCfg := map[string]any{"onMatch": map[string]any{"action": "redact"}}
	return compliance.NewPolicyResolver([]core.HookConfig{
		{ID: "h-redact", ImplementationID: "modela-redact", Name: "modela-redact", Stage: "response", Enabled: true, FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}, Config: redactCfg},
		{ID: "h-softblock", ImplementationID: "modela-softblock", Name: "modela-softblock", Stage: "response", Enabled: true, FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}, Config: redactCfg},
	}, reg, discardSlog())
}

func modelABumpOptions(resolver *compliance.PolicyResolver) (*bumpOptions, *recordingAuditWriter) {
	w := &recordingAuditWriter{}
	store := streampolicy.NewStore(streampolicy.Policy{
		Mode: streampolicy.ModeChunkedAsync, ChunkBytes: 1024,
		HookTimeoutMs: 1000, MaxBufferBytes: 1 << 20, FailBehavior: streampolicy.FailOpen,
	})
	return &bumpOptions{
		policyResolver:       resolver,
		streamingPolicyStore: store,
		auditEmitter:         compliance.NewAuditEmitter(w, discardSlog()),
		streamingConfig:      streaming.LiveConfig{MaxBufferSize: 1 << 20},
	}, w
}

func modelAAudCtx() (*core.HookInput, *compliance.AuditInfo, *requestAuditCtx) {
	respInput := &core.HookInput{Stage: "response", TargetHost: "api.example.com", Path: "/v1/chat/completions", IngressType: "COMPLIANCE_PROXY"}
	auditInfo := &compliance.AuditInfo{TransactionID: "tx-modela"}
	audCtx := &requestAuditCtx{input: respInput, info: *auditInfo, adapter: &openai.Adapter{}, storeResponseBody: true}
	return respInput, auditInfo, audCtx
}

func oaContentBody(parts ...string) string {
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(`data: {"choices":[{"delta":{"content":"`)
		sb.WriteString(p)
		sb.WriteString(`"}}]}` + "\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// TestSSE_ModelA_ConfirmedRedact_NoLeak is the load-bearing wire-path guard: a
// redact-scope chunked_async flow routes to Model A; a confirmed hit escalates and
// the complete card number never reaches the client — the masked value is spliced.
func TestSSE_ModelA_ConfirmedRedact_NoLeak(t *testing.T) {
	hook := modelATestHook{
		decision: core.Modify,
		modified: []core.ContentBlock{{Type: "text", Text: "card [CARD]"}},
		spans:    []normalize.TransformSpan{tspan("messages.0.content.0", 5, 21, "[CARD]")},
		matchRaw: true,
	}
	bo, _ := modelABumpOptions(modelARedactResolver(hook))
	respInput, auditInfo, audCtx := modelAAudCtx()
	body := oaContentBody("card ", "4111111111111111")
	rec := httptest.NewRecorder()

	handleSSEResponse(context.Background(), rec, sseFrameUpstream(body), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())

	out := rec.Body.String()
	if strings.Contains(out, "4111111111111111") {
		t.Fatalf("Model A leaked the complete card number raw: %q", out)
	}
	if !strings.Contains(out, "[CARD]") {
		t.Fatalf("Model A escalation did not splice the mask onto the wire: %q", out)
	}
}

// TestSSE_ModelA_BlockSoftMaskedRedact_NoLeak is leak #2's missing WIRE evidence: a redact
// hook co-firing with a soft-block hook (BOTH redact-scoped, so the flow routes to modela)
// aggregates to Decision=BlockSoft while carrying the redact's ModifiedContent/spans. Before
// #13 the merge dropped ModifiedContent under BlockSoft, so the wire splice fence
// (maskedMatchesModified) failed → redactUnsupported → the agent fail-open relayed the
// ORIGINAL held tail. Now mergeResults carries the content and the engine escalates on the
// enforcing ACTION (ActionFromDecision(BlockSoft)=block), so SpliceTextFrames masks the wire:
// the complete card number never reaches the client. (The ai-gateway RedactDelivers test
// covers the CANONICAL locus; this pins the raw-SSE splice end-to-end.)
func TestSSE_ModelA_BlockSoftMaskedRedact_NoLeak(t *testing.T) {
	redact := modelATestHook{
		decision: core.Modify,
		modified: []core.ContentBlock{{Type: "text", Text: "card [CARD]"}},
		spans:    []normalize.TransformSpan{tspan("messages.0.content.0", 5, 21, "[CARD]")},
		matchRaw: true,
	}
	softBlock := modelATestHook{decision: core.BlockSoft, matchRaw: true}
	bo, _ := modelABumpOptions(modelACoFiringResolver(redact, softBlock))
	respInput, auditInfo, audCtx := modelAAudCtx()
	body := oaContentBody("card ", "4111111111111111")
	rec := httptest.NewRecorder()

	handleSSEResponse(context.Background(), rec, sseFrameUpstream(body), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())

	out := rec.Body.String()
	if strings.Contains(out, "4111111111111111") {
		t.Fatalf("Model A co-firing BlockSoft leaked the complete card number raw: %q", out)
	}
	if !strings.Contains(out, "[CARD]") {
		t.Fatalf("Model A co-firing BlockSoft did not splice the mask onto the wire: %q", out)
	}
}

// TestSSE_ModelA_StrictAppliance_FailClosed_OnUnredactableMask pins the caller-driven
// posture on the Model A wire escalation: a tool-arg mask is undeliverable on the
// streaming wire, so the redactor reports unsupported. Under the compliance-proxy
// appliance (strictFailClosed) the escalation BLOCKS with an in-band error frame —
// it must not relay the unredacted remainder raw; the agent (non-strict) fail-open
// path relays the original (NE host-packet safety) instead.
func TestSSE_ModelA_StrictAppliance_FailClosed_OnUnredactableMask(t *testing.T) {
	mkHook := func() modelATestHook {
		return modelATestHook{
			decision: core.Modify,
			modified: []core.ContentBlock{{Type: "text", Text: "x"}},
			spans:    []normalize.TransformSpan{tspan("messages.0.content.0.toolUse.input.0", 0, 1, "x")},
			matchRaw: true,
		}
	}
	body := oaContentBody("secret ", "payload")

	// Appliance (strict) → fail-CLOSED block frame, no raw remainder.
	boStrict, _ := modelABumpOptions(modelARedactResolver(mkHook()))
	boStrict.strictFailClosed = true
	ri, ai, ac := modelAAudCtx()
	recStrict := httptest.NewRecorder()
	handleSSEResponse(context.Background(), recStrict, sseFrameUpstream(body), ac, ri, ai, boStrict, discardSlog(), time.Now())
	if out := recStrict.Body.String(); !strings.Contains(out, "blocked by policy") || strings.Contains(out, "payload") {
		t.Fatalf("strict appliance must block on an unredactable mask (no raw remainder), got %q", out)
	}

	// Agent (non-strict) → fail-OPEN relays the original (NE safety).
	boOpen, _ := modelABumpOptions(modelARedactResolver(mkHook()))
	ri2, ai2, ac2 := modelAAudCtx()
	recOpen := httptest.NewRecorder()
	handleSSEResponse(context.Background(), recOpen, sseFrameUpstream(body), ac2, ri2, ai2, boOpen, discardSlog(), time.Now())
	out := recOpen.Body.String()
	if strings.Contains(out, "blocked by policy") {
		t.Fatalf("agent fail-open must NOT block on an unredactable mask, got %q", out)
	}
	// Agent fail-open relays the original remainder (the disclosed best-effort posture).
	if !strings.Contains(out, "payload") {
		t.Fatalf("agent fail-open must relay the original remainder, got %q", out)
	}
}

// TestSSE_ModelA_FalsePositive_Approve: the prescan HITs but the confirm Approves, so
// Model A resumes real-time streaming and delivers the full (benign) body unchanged.
func TestSSE_ModelA_FalsePositive_Approve(t *testing.T) {
	hook := modelATestHook{decision: core.Approve, matchRaw: true}
	bo, _ := modelABumpOptions(modelARedactResolver(hook))
	respInput, auditInfo, audCtx := modelAAudCtx()
	body := oaContentBody("hello ", "world")
	rec := httptest.NewRecorder()

	handleSSEResponse(context.Background(), rec, sseFrameUpstream(body), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())

	out := rec.Body.String()
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("false-positive approve must deliver the full body, got %q", out)
	}
}

// TestSSE_ModelA_PrescanMiss_StreamsAndApproves: a sound prescan that never matches
// streams the whole body in real time with zero confirms and an approve-at-EOF stamp.
func TestSSE_ModelA_PrescanMiss_StreamsAndApproves(t *testing.T) {
	hook := modelATestHook{decision: core.Modify, matchRaw: false} // prescan never hits
	bo, _ := modelABumpOptions(modelARedactResolver(hook))
	respInput, auditInfo, audCtx := modelAAudCtx()
	body := oaContentBody("totally ", "benign text")
	rec := httptest.NewRecorder()

	handleSSEResponse(context.Background(), rec, sseFrameUpstream(body), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())

	out := rec.Body.String()
	if !strings.Contains(out, "totally") || !strings.Contains(out, "benign text") {
		t.Fatalf("prescan miss must stream the full body, got %q", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("stream must terminate with the [DONE] frame, got %q", out)
	}
}

// TestSSE_ModelA_HardBlock_ErrorFrame: a confirmed RejectHard escalation emits an
// in-band block error frame with zero remainder content (deliberate block, not a
// fail-open redaction fault).
func TestSSE_ModelA_HardBlock_ErrorFrame(t *testing.T) {
	hook := modelATestHook{decision: core.RejectHard, matchRaw: true}
	bo, _ := modelABumpOptions(modelARedactResolver(hook))
	respInput, auditInfo, audCtx := modelAAudCtx()
	body := oaContentBody("secret ", "payload")
	rec := httptest.NewRecorder()

	handleSSEResponse(context.Background(), rec, sseFrameUpstream(body), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())

	out := rec.Body.String()
	if !strings.Contains(out, "blocked by policy") {
		t.Fatalf("hard block must emit an in-band error frame, got %q", out)
	}
	if strings.Contains(out, "payload") {
		t.Fatalf("hard block must deliver zero remainder content, got %q", out)
	}
}

// TestSSE_ModelA_UsageAccumulated exercises the usage-accumulator seam: when the
// request was detected as AI traffic (provider set), the substrate feeds every
// parsed frame to the accumulator during Next, and the redact escalation still
// masks the value (no leak).
func TestSSE_ModelA_UsageAccumulated(t *testing.T) {
	hook := modelATestHook{
		decision: core.Modify,
		modified: []core.ContentBlock{{Type: "text", Text: "card [CARD]"}},
		spans:    []normalize.TransformSpan{tspan("messages.0.content.0", 5, 21, "[CARD]")},
		matchRaw: true,
	}
	bo, _ := modelABumpOptions(modelARedactResolver(hook))
	respInput, auditInfo, audCtx := modelAAudCtx()
	auditInfo.RequestMeta.Provider = "openai" // → handleSSEResponse builds a UsageAccumulator
	body := oaContentBody("card ", "4111111111111111")
	rec := httptest.NewRecorder()

	handleSSEResponse(context.Background(), rec, sseFrameUpstream(body), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())

	if strings.Contains(rec.Body.String(), "4111111111111111") {
		t.Fatalf("Model A leaked the card with usage accounting on: %q", rec.Body.String())
	}
}

// TestSSE_ModelA_EscalationDrainError exercises the escalation drain path hitting a
// non-EOF upstream error: the confirm fires on the first frame and escalates, then
// the drain read errors — the substrate returns the error without panicking or
// delivering the held value raw.
func TestSSE_ModelA_EscalationDrainError(t *testing.T) {
	hook := modelATestHook{
		decision: core.Modify,
		modified: []core.ContentBlock{{Type: "text", Text: "card [CARD]"}},
		spans:    []normalize.TransformSpan{tspan("messages.0.content.0", 5, 21, "[CARD]")},
		matchRaw: true,
	}
	bo, _ := modelABumpOptions(modelARedactResolver(hook))
	respInput, auditInfo, audCtx := modelAAudCtx()
	// One content frame, no [DONE], then an upstream error during the escalation drain.
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(&errAfterReader{data: []byte(`data: {"choices":[{"delta":{"content":"card 4111"}}]}` + "\n\n")}),
	}
	rec := httptest.NewRecorder()

	handleSSEResponse(context.Background(), rec, resp, audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())
	if strings.Contains(rec.Body.String(), "4111") {
		t.Fatalf("a drain error must not flush the held value raw: %q", rec.Body.String())
	}
}

// failingWriter is an http.ResponseWriter whose Write always errors — to exercise
// the frame-write error path (client disconnected mid-stream).
type failingWriter struct{ header http.Header }

func (f *failingWriter) Header() http.Header       { return f.header }
func (f *failingWriter) WriteHeader(int)           {}
func (f *failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestSSE_ModelA_ClientWriteError exercises the frame-write error path (client
// disconnected mid-stream): on the prescan-miss real-time delivery a failing writer
// makes Deliver/writeFrame return, and the handler stops without panicking.
func TestSSE_ModelA_ClientWriteError(t *testing.T) {
	hook := modelATestHook{decision: core.Modify, matchRaw: false} // miss → real-time held delivery at EOF
	bo, _ := modelABumpOptions(modelARedactResolver(hook))
	respInput, auditInfo, audCtx := modelAAudCtx()
	body := oaContentBody("hello ", "world")
	fw := &failingWriter{header: http.Header{}}

	handleSSEResponse(context.Background(), fw, sseFrameUpstream(body), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())
	// The assertion is that the failing writer did not panic the handler; failingWriter
	// discards all writes, so there is nothing to inspect on the wire.
}

// TestSSE_ModelA_EscalationWriteError exercises a frame-write error WHILE delivering
// the redacted remainder on escalation (client gone mid-escalation): writeFrames
// returns and the handler stops without leaking the raw value.
func TestSSE_ModelA_EscalationWriteError(t *testing.T) {
	hook := modelATestHook{
		decision: core.Modify,
		modified: []core.ContentBlock{{Type: "text", Text: "card [CARD]"}},
		spans:    []normalize.TransformSpan{tspan("messages.0.content.0", 5, 21, "[CARD]")},
		matchRaw: true,
	}
	bo, _ := modelABumpOptions(modelARedactResolver(hook))
	respInput, auditInfo, audCtx := modelAAudCtx()
	body := oaContentBody("card ", "4111111111111111")
	fw := &failingWriter{header: http.Header{}}

	handleSSEResponse(context.Background(), fw, sseFrameUpstream(body), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())
	// failingWriter discards all writes; the assertion is no panic and no raw value
	// could reach the (failing) wire.
}

// TestSSE_ModelA_EscalationDrainCap pins the bounded-drain guard: after a confirmed
// hit, a post-hit remainder larger than the buffer cap is BLOCKED with an in-band
// error frame (bounded memory, zero leak) rather than buffered unbounded or relayed
// raw. The cap is forced small via the streaming policy MaxBufferBytes.
func TestSSE_ModelA_EscalationDrainCap(t *testing.T) {
	hook := modelATestHook{
		decision: core.Modify,
		modified: []core.ContentBlock{{Type: "text", Text: "card [CARD]"}},
		spans:    []normalize.TransformSpan{tspan("messages.0.content.0", 5, 21, "[CARD]")},
		matchRaw: true,
	}
	bo, _ := modelABumpOptions(modelARedactResolver(hook))
	bo.streamingConfig.MaxBufferSize = 64 // tiny cap → the drained remainder overflows
	respInput, auditInfo, audCtx := modelAAudCtx()
	big := strings.Repeat("4", 200)
	body := oaContentBody("card ", big, big)
	rec := httptest.NewRecorder()

	handleSSEResponse(context.Background(), rec, sseFrameUpstream(body), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())

	out := rec.Body.String()
	if strings.Contains(out, big) {
		t.Fatalf("drain-cap overflow leaked the raw remainder: %q", out)
	}
	if !strings.Contains(out, "blocked by policy") {
		t.Fatalf("drain-cap overflow must block with an in-band error frame, got %q", out)
	}
}

// errAfterReader yields data once, then a non-EOF error — to exercise the substrate's
// upstream-error (OnError) path.
type errAfterReader struct {
	data []byte
	done bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	r.done = true
	n := copy(p, r.data)
	return n, nil
}

// TestSSE_ModelA_UpstreamError_NoPanic: a non-EOF upstream read error on the
// prescan-miss path routes through OnError without panicking or forcing a synthetic
// frame (agent fail-open).
func TestSSE_ModelA_UpstreamError_NoPanic(t *testing.T) {
	hook := modelATestHook{decision: core.Modify, matchRaw: false} // miss → main-loop Next hits the error
	bo, _ := modelABumpOptions(modelARedactResolver(hook))
	respInput, auditInfo, audCtx := modelAAudCtx()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(&errAfterReader{data: []byte(`data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n")}),
	}
	rec := httptest.NewRecorder()

	handleSSEResponse(context.Background(), rec, resp, audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())
	// No panic + no synthetic error frame forced (fail-open). The partial frame may or
	// may not have flushed depending on the window; the assertion is the call returns.
	if strings.Contains(rec.Body.String(), "blocked by policy") {
		t.Fatalf("agent fail-open must not force a block frame on an upstream error: %q", rec.Body.String())
	}
}
