package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// modelACountingRunner returns a per-checkpoint hookRunner backed by the supplied
// response-hook cache (mirroring the relay's stream_hooks runner) plus a counter
// of how many full confirms ran — the observable that distinguishes a prescan
// MISS (zero confirms, fully real-time) from a HIT (≥1 confirm).
func modelACountingRunner(t *testing.T, cache *compliance.HookConfigCache, ingress Ingress) (func(context.Context, *goHooks.HookInput) *goHooks.CompliancePipelineResult, *int) {
	t.Helper()
	epType := typology.KindFromWireShape(ingress.WireShape)
	modalities := []goHooks.Modality{goHooks.ModalityText}
	count := new(int)
	runner := func(ctx context.Context, input *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		*count++
		input.EndpointType = epType
		input.OutputModality = modalities
		pipeline, err := cache.Resolver(ctx).BuildPipeline(
			"response", "AI_GATEWAY", epType, modalities,
			5*time.Second, 15*time.Second, false, true, noopLogger(),
		)
		if err != nil {
			return &goHooks.CompliancePipelineResult{Decision: goHooks.RejectHard, Reason: "build error"}
		}
		if pipeline == nil {
			return &goHooks.CompliancePipelineResult{Decision: goHooks.Approve}
		}
		pipeline.SetAllowModify(true)
		pipeline.SetClearSoftOnApprove(true)
		return pipeline.Execute(ctx, input)
	}
	return runner, count
}

// containsPrescan returns a prescan that HITs when the marker is present — a
// deterministic stand-in for the real union prefilter so a test can drive the
// MISS / false-positive / confirmed paths exactly.
func containsPrescan(marker string) func([]byte) bool {
	return func(b []byte) bool { return bytes.Contains(b, []byte(marker)) }
}

// TestModelAStream_PrescanMiss_RealTime_ZeroConfirm pins the common case: a
// prescan that never matches streams the full body in real time and pays ZERO
// full confirms.
func TestModelAStream_PrescanMiss_RealTime_ZeroConfirm(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{
		{Delta: "hello "},
		{Delta: "world", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, confirms := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("NEVER"))

	out := w.String()
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("prescan MISS must deliver the full body, got %q", out)
	}
	if *confirms != 0 {
		t.Errorf("prescan MISS must run zero full confirms, ran %d", *confirms)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("OpenAI ingress must terminate with [DONE], got tail %q", lastN(out, 40))
	}
	if s.rec.ResponseHookRewritten {
		t.Error("a clean stream is not a rewrite")
	}
}

// TestModelAStream_FalsePositive_OneConfirm_FullBody pins the false-positive
// path: the prescan HITs but the confirm Approves, so exactly ONE confirm runs
// and the full (benign) body is still delivered.
func TestModelAStream_FalsePositive_OneConfirm_FullBody(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{
		{Delta: "totally "},
		{Delta: "benign TRIGGER text", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, confirms := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	// Prescan HITs on "TRIGGER" (a false positive: no email → the hook Approves).
	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("TRIGGER"))

	out := w.String()
	if !strings.Contains(out, "benign TRIGGER text") || !strings.Contains(out, "totally") {
		t.Fatalf("false positive must resume and deliver the full body, got %q", out)
	}
	if *confirms != 1 {
		t.Errorf("false positive must run exactly one confirm, ran %d", *confirms)
	}
	if s.rec.ResponseHookRewritten {
		t.Error("a false positive is not a rewrite")
	}
}

// TestModelAStream_ConfirmedHit_Escalates_Redacts pins the load-bearing path: a
// confirmed hit escalates to canonical-buffer redaction and the complete PII
// value never appears verbatim in the delivered stream.
func TestModelAStream_ConfirmedHit_Escalates_Redacts(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{
		{Delta: "reach me at "},
		{Delta: "alice@example.com", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, confirms := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	out := w.String()
	if strings.Contains(out, "alice@example.com") {
		t.Fatalf("confirmed hit leaked the complete PII value verbatim: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_EMAIL]") {
		t.Fatalf("expected the masked value on the wire after escalation, got %q", out)
	}
	if *confirms < 1 {
		t.Errorf("a confirmed hit must run at least one confirm, ran %d", *confirms)
	}
	if !s.rec.ResponseHookRewritten {
		t.Error("escalation Modify must stamp ResponseHookRewritten")
	}
	if s.rec.ResponseAction != goHooks.ActionRedact {
		t.Errorf("rec.ResponseAction = %q, want redact", s.rec.ResponseAction)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("OpenAI ingress must terminate with [DONE], got tail %q", lastN(out, 40))
	}
}

// TestModelAStream_NoHit_StampsApprove pins FIX-5: a clean stream that never
// confirms still stamps an APPROVE response-hook outcome on the audit record, so
// a SIEM distinguishes "response hooks evaluated, approved" from "no response hook
// configured" (parity with the wire/buffer paths).
func TestModelAStream_NoHit_StampsApprove(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "all clean"}, {Delta: " here", Done: true}}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, confirms := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("NEVER"))

	if *confirms != 0 {
		t.Fatalf("clean stream must not confirm, ran %d", *confirms)
	}
	if s.rec.ResponseHookDecision != string(goHooks.Approve) {
		t.Errorf("no-hit stream must stamp ResponseHookDecision=%q, got %q", string(goHooks.Approve), s.rec.ResponseHookDecision)
	}
	if s.rec.ResponseAction != goHooks.ActionApprove {
		t.Errorf("rec.ResponseAction = %q, want approve", s.rec.ResponseAction)
	}
}

// TestModelAStream_FalsePositive_StampsDecision pins FIX-5 on the false-positive
// path: the confirm's Approve outcome is stamped onto the audit record (not left
// empty), so the row reflects that hooks ran.
func TestModelAStream_FalsePositive_StampsDecision(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "benign TRIGGER", Done: true}}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("TRIGGER"))

	if s.rec.ResponseHookDecision != string(goHooks.Approve) {
		t.Errorf("false-positive confirm must stamp ResponseHookDecision=%q, got %q", string(goHooks.Approve), s.rec.ResponseHookDecision)
	}
}

// TestModelAStream_ToolCallName_Scanned pins FIX-2: a value present only in a
// tool-call name (not arguments) is scanned by the gate — the enforcer extracts
// the whole tool_calls payload (name + id + args), so the gate must too, or a
// redact rule targeting a tool identifier would be missed. The email in the tool
// name triggers a confirm (escalation), not a silent flush.
func TestModelAStream_ToolCallName_Scanned(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{
		{ToolCallDeltas: []provcore.ToolCallDelta{{Index: 0, ID: "call_1", Name: "notify_alice@example.com", Arguments: "{}"}}, Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, confirms := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	if *confirms < 1 {
		t.Fatalf("a value in a tool-call name must be scanned and confirmed (FIX-2), ran %d confirms", *confirms)
	}
}

// TestModelAStream_EscalationPreservesWireToolIndex pins FIX-4: when a tool call
// at a NON-zero wire index escalates, the redacted continuation frame carries the
// original wire index, not the positional one — so the client (which received the
// tool-call prefix under the wire index) can reassemble it instead of seeing a
// split/orphaned tool_call.
func TestModelAStream_EscalationPreservesWireToolIndex(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{
		{ToolCallDeltas: []provcore.ToolCallDelta{{Index: 1, ID: "call_x", Name: "send", Arguments: `{"to":"alice@example.com"}`}}, Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	out := w.String()
	if !strings.Contains(out, `"index":1`) {
		t.Fatalf("escalated tool frame must carry the wire index 1, not positional 0; out=%q", out)
	}
	if strings.Contains(out, "alice@example.com") {
		t.Fatalf("tool-arg PII leaked verbatim after escalation: %q", out)
	}
}

// TestModelAStream_BoundedFragment_CompleteValueNeverDelivered pins the
// bounded-fragment contract: with a benign prefix larger than the tail window the
// prefix is delivered in real time, but the COMPLETE PII value (which arrives
// after the window) is held when its pattern completes and is redacted — never
// delivered verbatim. Not zero bytes: the benign prefix DOES reach the client.
func TestModelAStream_BoundedFragment_CompleteValueNeverDelivered(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	prefix := strings.Repeat("benign filler ", 800) // > modelATailWindowBytes
	chunks := []provcore.Chunk{
		{Delta: prefix},
		{Delta: "alice@example.com", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	out := w.String()
	if !strings.Contains(out, "benign filler") {
		t.Fatalf("the benign prefix must be delivered in real time (bounded fragment, not zero bytes), got %q", lastN(out, 60))
	}
	if strings.Contains(out, "alice@example.com") {
		t.Fatalf("the complete PII value must never be delivered verbatim, got it in %q", out)
	}
	if !strings.Contains(out, "[REDACTED_EMAIL]") {
		t.Fatalf("expected the masked value after escalation, got %q", lastN(out, 80))
	}
}

// TestModelAStream_StorageCopyRedactedWithinTailWindow pins BUG-storage for the
// Model-A path. The captured wire transcript IS the audit storage copy — the relay
// persists tee.captured() as rec.ResponseBody and (on a redact rewrite) as
// rec.ResponseBodyRedacted. Any PII value that completes within the tail window is
// entirely held when its last byte arrives, so escalation redacts it in the
// canonical buffer and the delivered prefix never carries a complete un-redacted
// value — ALL realistic PII (email/SSN/card are far shorter than the 8KB window).
// Here a benign prefix LARGER than the window is best-effort delivered, yet the
// complete email lands in the still-held tail and the persisted copy is fully
// masked. (Residual, disclosed: a redaction match whose SPAN exceeds the tail
// window has its leading bytes best-effort-delivered AND in storage — the same
// best-effort-wire limitation, consistent across wire and storage.)
func TestModelAStream_StorageCopyRedactedWithinTailWindow(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	prefix := strings.Repeat("benign filler ", 800) // > modelATailWindowBytes
	chunks := []provcore.Chunk{
		{Delta: prefix},
		{Delta: "reach me at alice@example.com today", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	// w.String() == tee.captured() == the bytes the relay persists as the audit
	// storage copy (rec.ResponseBody / rec.ResponseBodyRedacted on a redact rewrite).
	stored := w.String()
	if strings.Contains(stored, "alice@example.com") {
		t.Fatalf("BUG-storage: the persisted storage copy leaked the complete PII value: %q", lastN(stored, 90))
	}
	if !strings.Contains(stored, "[REDACTED_EMAIL]") {
		t.Fatalf("BUG-storage: the persisted storage copy must carry the masked value, got %q", lastN(stored, 90))
	}
	if !strings.Contains(stored, "benign filler") {
		t.Fatalf("the benign over-window prefix must be present in the stored copy (carries no PII), got %q", lastN(stored, 90))
	}
}

// TestModelAStream_HardBlock_ZeroContentAfterEscalation pins the confirmed
// RejectHard path: escalation fails closed via redactCanonicalBuffer — an in-band
// error frame, the original content never delivered, ResponseBodyRedacted nil.
func TestModelAStream_HardBlock_ZeroContentAfterEscalation(t *testing.T) {
	cache := newRejectingResponseHookCache(t)
	chunks := []provcore.Chunk{
		{Delta: "the secret is hunter2", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, confirms := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("secret"))

	out := w.String()
	if strings.Contains(out, "hunter2") {
		t.Fatalf("hard-blocked stream leaked content: %q", out)
	}
	if !strings.Contains(out, `"error"`) {
		t.Errorf("expected an SSE error frame on hard block, got %q", out)
	}
	if *confirms < 1 {
		t.Errorf("hard block must run a confirm, ran %d", *confirms)
	}
	if s.rec.ResponseHookRewritten {
		t.Error("a hard block is not a rewrite")
	}
	if s.rec.ResponseBodyRedacted != nil {
		t.Error("ResponseBodyRedacted must stay nil on a hard block")
	}
}

// TestModelAStream_EscalationBufferCap_FailsClosed pins the escalation overflow
// bound: when the drained remainder exceeds the resolved buffer cap the stream
// fails closed with an in-band error frame (bounded accumulation, no OOM).
func TestModelAStream_EscalationBufferCap_FailsClosed(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	big := strings.Repeat("y", 400)
	chunks := []provcore.Chunk{
		{Delta: "alice@example.com "}, // triggers the confirmed hit immediately
		{Delta: big},
		{Delta: big, Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner
	s.streamMaxBufferBytes = 256 // smaller than the drained remainder

	term := s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	out := w.String()
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "maximum buffer size") {
		t.Fatalf("expected a buffer-cap error frame on escalation, got %q", out)
	}
	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q classification on cap exceedance, got %+v", streamErrCodeUpstream, te)
	}
}

// TestModelAStream_UsagePreserved pins that usage observed on the canonical
// chunks lands on the usageHolder and is re-emitted on the wire (real-time path).
func TestModelAStream_UsagePreserved(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	pt, ct, tt := 5, 7, 12
	chunks := []provcore.Chunk{
		{Delta: "hi", Done: true, Usage: &provcore.Usage{PromptTokens: &pt, CompletionTokens: &ct, TotalTokens: &tt}},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("NEVER"))

	snap := usage.snapshot()
	if snap.TotalTokens == nil || *snap.TotalTokens != 12 {
		t.Fatalf("usageHolder total = %v, want 12", snap.TotalTokens)
	}
	if !strings.Contains(w.String(), "total_tokens") {
		t.Errorf("expected usage frame re-emitted on the wire, got %q", w.String())
	}
}

// TestModelAStream_ProviderError_SynthesizesFrame pins a subscription fault in
// the real-time loop: an ingress-shaped SSE error frame + upstream classification.
func TestModelAStream_ProviderError_SynthesizesFrame(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, nil)
	s.sub = &fakeChunkSub{err: &provcore.ProviderError{Status: http.StatusBadGateway, Code: provcore.CodeUpstreamError, Message: "boom"}}
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	term := s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	if !strings.Contains(w.String(), `"error"`) {
		t.Errorf("expected SSE error frame on provider fault, got %q", w.String())
	}
	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q classification, got %+v", streamErrCodeUpstream, te)
	}
}

// TestModelAStream_ClientAbort_NoFrame pins that a context-cancelled subscription
// is classified as a client abort and writes nothing to the gone peer.
func TestModelAStream_ClientAbort_NoFrame(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, nil)
	s.sub = &fakeChunkSub{err: context.Canceled}
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	term := s.h.runModelAStream(ctx, s, w, usage, containsPrescan("@"))

	if w.Len() != 0 {
		t.Errorf("client abort must not write to the gone peer, got %q", w.String())
	}
	if te := term.terminalError(); te == nil || te.code != streamErrCodeClientAbort {
		t.Errorf("expected %q classification, got %+v", streamErrCodeClientAbort, te)
	}
}

// TestModelAStream_NilSubOrTee_NoOp pins the defensive guard.
func TestModelAStream_NilSubOrTee_NoOp(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, nil)
	s.sub = nil
	if term := s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@")); term == nil {
		t.Fatal("expected a non-nil terminal carrier even on nil sub")
	}
	if w.Len() != 0 {
		t.Errorf("nil sub must not write to the client, got %q", w.String())
	}
}

// TestModelAConfirm_NilHookRunner_FailsClosed pins that a missing hook runner
// (defensive) fails the confirm closed to RejectHard rather than Approving.
func TestModelAConfirm_NilHookRunner_FailsClosed(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	s, _, usage := bufferTestState(t, cache, openAIChatIngress, nil, nil)
	s.hookRunner = nil

	res := s.h.modelAConfirm(context.Background(), s, "any content", usage)
	if res == nil || res.Decision != goHooks.RejectHard {
		t.Fatalf("nil hook runner must fail closed to RejectHard, got %+v", res)
	}
}

// TestModelAStream_NilPrescan_FailsSafeToConfirm pins that a nil prescan defaults
// to "always confirm" (sound) — a benign stream still Approves and delivers fully.
func TestModelAStream_NilPrescan_FailsSafeToConfirm(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "plain text", Done: true}}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, confirms := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, nil)

	if *confirms == 0 {
		t.Error("nil prescan must fail safe to always-confirm (≥1 confirm)")
	}
	if !strings.Contains(w.String(), "plain text") {
		t.Errorf("benign Approve must deliver the body, got %q", w.String())
	}
}

// TestBuildResponsePrescan_NoCache_AlwaysConfirms pins the fail-safe: with no
// hook cache the prescan returns "always confirm" (never silently skips a hook).
func TestBuildResponsePrescan_NoCache_AlwaysConfirms(t *testing.T) {
	h := &Handler{deps: &Deps{Logger: noopLogger()}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIngress(req.Context(), openAIChatIngress))
	s := &streamState{h: h, r: req, logger: noopLogger()}

	prescan := h.buildResponsePrescan(context.Background(), s)
	if !prescan([]byte("anything")) {
		t.Error("no-cache prescan must fail safe to always-confirm")
	}
}

// TestBuildResponsePrescan_RealCache_GatesOnContent pins that the real probe's
// union prefilter discriminates: content with an email is flagged for a confirm,
// pure non-PII content is not (zero wasted confirms in the common case).
func TestBuildResponsePrescan_RealCache_GatesOnContent(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	h := &Handler{deps: &Deps{HookConfigCache: cache, Logger: noopLogger()}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIngress(req.Context(), openAIChatIngress))
	s := &streamState{h: h, r: req, logger: noopLogger()}

	prescan := h.buildResponsePrescan(context.Background(), s)
	if !prescan([]byte("write to alice@example.com")) {
		t.Error("email content must trip the union prefilter")
	}
	if prescan([]byte("the quick brown fox")) {
		t.Error("non-PII content must not trip the union prefilter")
	}
}

// TestStreamRelayStage_ModelAArmed_RedactsViaCanonical pins the dispatch: a
// MayRedact chunked_async stream (modelAArmed) is routed to runModelAStream — the
// canonical Model-A path — so the delivered stream is redacted, distinct from the
// wire passthrough/live dispatch which would not redact a Modify.
func TestStreamRelayStage_ModelAArmed_RedactsViaCanonical(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "reach alice@example.com", Done: true}}
	s, _, _ := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	s.h.deps.PayloadCapture = payloadcapture.NewStore(payloadcapture.Config{StoreResponseBody: true})
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner
	rr := httptest.NewRecorder()
	s.w = rr
	s.streamMode = streampolicy.ModeChunkedAsync
	s.modelAArmed = true

	if ok := (streamRelayStage{s: s}).run(); !ok {
		t.Fatal("relay stage returned false")
	}

	body := rr.Body.String()
	if strings.Contains(body, "alice@example.com") {
		t.Fatalf("relay Model-A path leaked PII: %q", body)
	}
	if !strings.Contains(body, "[REDACTED_EMAIL]") {
		t.Fatalf("expected masked content via relay Model-A path, got %q", body)
	}
	if !s.rec.ResponseHookRewritten {
		t.Error("ResponseHookRewritten should be true after the escalation redact")
	}
	if strings.Contains(string(s.rec.ResponseBodyRedacted), "alice@example.com") {
		t.Errorf("stored redacted copy leaked PII: %q", s.rec.ResponseBodyRedacted)
	}
}

// TestModelAStream_Concurrent_PerCallIsolation proves per-subscriber isolation:
// many Model-A streams run concurrently under -race, each escalating and masking
// only its own PII with no cross-talk (no shared accumulation, no shared encoder).
func TestModelAStream_Concurrent_PerCallIsolation(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chunks := []provcore.Chunk{
				{Delta: "x carol@example.com y", Done: true},
			}
			s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
			runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
			s.hookRunner = runner
			s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))
			out := w.String()
			if strings.Contains(out, "carol@example.com") {
				t.Errorf("per-call leak under concurrency: %q", out)
			}
			if !strings.Contains(out, "[REDACTED_EMAIL]") {
				t.Errorf("missing mask under concurrency: %q", out)
			}
		}()
	}
	wg.Wait()
}

// TestModelAStream_EscalationProviderError_Classified pins a subscription fault
// during the escalation drain: an ingress-shaped error frame + upstream class.
func TestModelAStream_EscalationProviderError_Classified(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	// The first chunk triggers the confirmed hit (no Done), then the drain hits a
	// provider fault before the terminal frame.
	sub := &fakeChunkSub{chunks: []provcore.Chunk{{Delta: "alice@example.com "}}}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, nil)
	s.sub = &drainErrSub{head: sub, errAfter: errors.New("drain read failure")}
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	term := s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	if !strings.Contains(w.String(), `"error"`) {
		t.Errorf("expected synthesized error frame on drain fault, got %q", w.String())
	}
	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q classification, got %+v", streamErrCodeUpstream, te)
	}
}

// TestModelAStream_ToolArgs_ConfirmedRedact pins that tool-call argument PII is
// scanned (the scan folds ToolCallDelta arguments) and masked through the
// escalation, with the tool name preserved.
func TestModelAStream_ToolArgs_ConfirmedRedact(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{
		{ToolCallDeltas: []provcore.ToolCallDelta{{Index: 0, ID: "call_1", Name: "send_email"}}},
		{ToolCallDeltas: []provcore.ToolCallDelta{{Index: 0, Arguments: `{"to":"alice@example.com"}`}}, Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	out := w.String()
	if strings.Contains(out, "alice@example.com") {
		t.Fatalf("tool-call argument PII leaked: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_EMAIL]") || !strings.Contains(out, "send_email") {
		t.Fatalf("expected masked tool-arg with preserved name, got %q", out)
	}
}

// TestModelAStream_FinishReason_Preserved_RealTime pins that a real-time MISS
// stream preserves the observed finish_reason on the terminal frame.
func TestModelAStream_FinishReason_Preserved_RealTime(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{
		{Delta: "hi"},
		{FinishReason: "length"},
		{Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("NEVER"))

	if !strings.Contains(w.String(), `"finish_reason":"length"`) {
		t.Errorf("real-time path must preserve finish_reason=length, got %q", w.String())
	}
}

// TestModelAStream_EOFWithoutDone_DeliversTail pins the EOF break: a subscription
// that ends without a terminal Done frame still delivers the held tail + terminal.
func TestModelAStream_EOFWithoutDone_DeliversTail(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "tail without done"}} // no Done → EOF break
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("NEVER"))

	out := w.String()
	if !strings.Contains(out, "tail without done") {
		t.Fatalf("EOF without Done must still deliver the held tail, got %q", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("expected terminal framing at EOF, got tail %q", lastN(out, 40))
	}
}

// TestModelAStream_GenericUpstreamFault_RealTime pins a non-typed subscription
// error in the real-time loop → synthesized 502 frame + upstream classification.
func TestModelAStream_GenericUpstreamFault_RealTime(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, nil)
	s.sub = &fakeChunkSub{err: errors.New("raw upstream read failure")}
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	term := s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	if !strings.Contains(w.String(), `"error"`) {
		t.Errorf("expected synthesized error frame, got %q", w.String())
	}
	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q classification, got %+v", streamErrCodeUpstream, te)
	}
}

// TestModelAStream_RealTimeFlushWriteError_Classified pins a write failure while
// releasing the bounded tail → upstream classification.
func TestModelAStream_RealTimeFlushWriteError_Classified(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	big := strings.Repeat("z", modelATailWindowBytes+500) // forces a tail release
	chunks := []provcore.Chunk{{Delta: big}, {Delta: "x", Done: true}}
	s, _, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner
	fw := &failingWriter{header: http.Header{}}

	term := s.h.runModelAStream(context.Background(), s, fw, usage, containsPrescan("NEVER"))

	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q on flush write error, got %+v", streamErrCodeUpstream, te)
	}
}

// TestModelAStream_EOFTailWriteError_Classified pins a write failure delivering
// the held tail at EOF → upstream classification.
func TestModelAStream_EOFTailWriteError_Classified(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "hi", Done: true}}
	s, _, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner
	fw := &failingWriter{header: http.Header{}}

	term := s.h.runModelAStream(context.Background(), s, fw, usage, containsPrescan("NEVER"))

	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q on EOF tail write error, got %+v", streamErrCodeUpstream, te)
	}
}

// TestModelAStream_EscalationSynthWriteError_Classified pins a write failure
// delivering the redacted remainder on escalation → upstream classification.
func TestModelAStream_EscalationSynthWriteError_Classified(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "alice@example.com", Done: true}}
	s, _, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner
	fw := &failingWriter{header: http.Header{}}

	term := s.h.runModelAStream(context.Background(), s, fw, usage, containsPrescan("@"))

	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q on escalation synth write error, got %+v", streamErrCodeUpstream, te)
	}
}

// TestModelAStream_EscalationDrain_FinishReason pins the drain loop: a confirmed
// hit on a non-terminal chunk drains the remaining chunks (capturing their
// finish_reason) before redacting the full remainder.
func TestModelAStream_EscalationDrain_FinishReason(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{
		{Delta: "alice@example.com "}, // triggers the confirmed hit (no Done)
		{Delta: "trailing"},
		{FinishReason: "stop", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	out := w.String()
	if strings.Contains(out, "alice@example.com") {
		t.Fatalf("escalation leaked PII across the drain: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_EMAIL]") || !strings.Contains(out, "trailing") {
		t.Fatalf("expected masked email + drained remainder, got %q", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("drain must preserve finish_reason, got %q", out)
	}
}

// TestModelAStream_EscalationDrain_EOFNoDone pins the drain EOF break: a confirmed
// hit whose held chunk lacks Done and whose subscription then ends (EOF) still
// redacts and delivers the remainder.
func TestModelAStream_EscalationDrain_EOFNoDone(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "alice@example.com"}} // no Done; sub then EOFs
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	out := w.String()
	if strings.Contains(out, "alice@example.com") {
		t.Fatalf("escalation EOF-drain leaked PII: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_EMAIL]") {
		t.Fatalf("expected masked value after EOF drain, got %q", out)
	}
}

// TestModelAStream_EscalationDrain_ClientAbort pins a context cancellation during
// the escalation drain → client-abort classification, nothing written after.
func TestModelAStream_EscalationDrain_ClientAbort(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	// First chunk triggers the confirmed hit; the drain then sees the cancel.
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, nil)
	s.sub = &drainErrSub{head: &fakeChunkSub{chunks: []provcore.Chunk{{Delta: "alice@example.com "}}}, errAfter: context.Canceled, onDrain: cancel}
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	term := s.h.runModelAStream(ctx, s, w, usage, containsPrescan("@"))

	if te := term.terminalError(); te == nil || te.code != streamErrCodeClientAbort {
		t.Errorf("expected %q on drain client abort, got %+v", streamErrCodeClientAbort, te)
	}
}

// kthFailWriter is an http.ResponseWriter whose Write succeeds until the kth
// call, which fails — used to fault a specific frame in a multi-write sequence
// (e.g. the trailing `data: [DONE]` after the terminal frame succeeded).
type kthFailWriter struct {
	header http.Header
	n      int
	failOn int
}

func (k *kthFailWriter) Header() http.Header { return k.header }
func (k *kthFailWriter) WriteHeader(int)     {}
func (k *kthFailWriter) Write(b []byte) (int, error) {
	k.n++
	if k.n >= k.failOn {
		return 0, io.ErrClosedPipe
	}
	return len(b), nil
}

// TestWriteEncodedChunk_WriteError pins error propagation when the client write
// of a body frame fails.
func TestWriteEncodedChunk_WriteError(t *testing.T) {
	enc := canonicalbridge.NewChatCompletionsStreamEncoder("m")
	if err := writeEncodedChunk(context.Background(), enc, &failingWriter{header: http.Header{}}, provcore.Chunk{Delta: "hi"}); err == nil {
		t.Error("expected a write error to propagate")
	}
}

// TestWriteEncodedTerminal_TerminalWriteError pins error propagation when the
// terminal frame write fails.
func TestWriteEncodedTerminal_TerminalWriteError(t *testing.T) {
	enc := canonicalbridge.NewChatCompletionsStreamEncoder("m")
	if err := writeEncodedTerminal(context.Background(), enc, &failingWriter{header: http.Header{}}, true, nil, "stop"); err == nil {
		t.Error("expected a terminal write error to propagate")
	}
}

// TestWriteEncodedTerminal_DoneSentinelWriteError pins error propagation when the
// terminal frame succeeds but the trailing `data: [DONE]` sentinel write fails.
func TestWriteEncodedTerminal_DoneSentinelWriteError(t *testing.T) {
	enc := canonicalbridge.NewChatCompletionsStreamEncoder("m")
	w := &kthFailWriter{header: http.Header{}, failOn: 2}
	if err := writeEncodedTerminal(context.Background(), enc, w, true, nil, "stop"); err == nil {
		t.Error("expected the [DONE] sentinel write error to propagate")
	}
}

// TestModelAStream_EscalationDrain_RecordsUsage pins that usage observed on a
// chunk drained DURING the escalation is recorded and re-emitted on the wire.
func TestModelAStream_EscalationDrain_RecordsUsage(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	pt, ct, tt := 3, 4, 7
	chunks := []provcore.Chunk{
		{Delta: "alice@example.com "}, // confirmed hit (no Done)
		{Delta: "rest", Done: true, Usage: &provcore.Usage{PromptTokens: &pt, CompletionTokens: &ct, TotalTokens: &tt}},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner

	s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	if snap := usage.snapshot(); snap.TotalTokens == nil || *snap.TotalTokens != 7 {
		t.Fatalf("usage drained during escalation must be recorded, got %v", usage.snapshot().TotalTokens)
	}
	if !strings.Contains(w.String(), "total_tokens") {
		t.Errorf("expected usage frame re-emitted after escalation, got %q", w.String())
	}
}

// TestModelAStream_EscalationTerminalWriteError_Classified pins error
// propagation when the redacted-remainder synth write succeeds but the terminal
// frame write fails during escalation.
func TestModelAStream_EscalationTerminalWriteError_Classified(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "alice@example.com", Done: true}}
	s, _, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	runner, _ := modelACountingRunner(t, cache, openAIChatIngress)
	s.hookRunner = runner
	w := &kthFailWriter{header: http.Header{}, failOn: 2} // synth ok, terminal fails

	term := s.h.runModelAStream(context.Background(), s, w, usage, containsPrescan("@"))

	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q on escalation terminal write error, got %+v", streamErrCodeUpstream, te)
	}
}

// drainErrSub yields head's chunks, then returns errAfter (used to fault the
// escalation drain loop specifically, after the real-time confirm has fired).
type drainErrSub struct {
	head     *fakeChunkSub
	errAfter error
	onDrain  func() // invoked once when the head is exhausted (e.g. cancel the ctx)
}

func (d *drainErrSub) Next(ctx context.Context) (provcore.Chunk, error) {
	c, err := d.head.Next(ctx)
	if errors.Is(err, io.EOF) {
		if d.onDrain != nil {
			d.onDrain()
		}
		return provcore.Chunk{}, d.errAfter
	}
	return c, err
}
func (d *drainErrSub) Close() error { return nil }
