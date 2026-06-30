package streaming

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// mockFrameRedactor is the per-service FrameRedactor stand-in used to
// exercise BufferPipeline's Modify branch without pulling an ai-gateway
// / tlsbump wire reshaper into the shared package. fn receives the
// buffered events + the pipeline result and returns the events to
// replay (or ErrRewriteUnsupported to drive the fail-closed path).
type mockFrameRedactor struct {
	mu     sync.Mutex
	calls  int
	fn     func(events []*SSEEvent, result *core.CompliancePipelineResult) ([]*SSEEvent, error)
	gotLen int
}

func (m *mockFrameRedactor) RedactReplay(events []*SSEEvent, result *core.CompliancePipelineResult) ([]*SSEEvent, error) {
	m.mu.Lock()
	m.calls++
	m.gotLen = len(events)
	m.mu.Unlock()
	return m.fn(events, result)
}

// mixedSSE builds an SSE stream with a text content frame, a non-text
// (tool_call) frame, and a [DONE] terminator. The tool frame carries no
// delta.content so extractDeltaText yields "" for it — it is the
// byte-verbatim "pass non-text frames unchanged" subject.
func mixedSSE(text, toolJSON string) string {
	var sb strings.Builder
	sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"" + text + "\"}}]}\n\n")
	sb.WriteString("data: " + toolJSON + "\n\n")
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

func modifyPipeline() *mockPipeline {
	return &mockPipeline{
		decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
			return &core.CompliancePipelineResult{Decision: core.Modify, Reason: "rewrite requested"}
		},
	}
}

// TestBufferPipeline_ModifyWithRedactor_ReplaysMasked is the inversion of
// the old ModifyDegradesToApprove test: with a FrameRedactor installed,
// the Modify decision REDACTS — the masked token replaces the original
// in the replayed stream, the non-text frame is byte-verbatim, and the
// degrade counter is NOT incremented for a supported rewrite.
func TestBufferPipeline_ModifyWithRedactor_ReplaysMasked(t *testing.T) {
	toolJSON := `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"x\"}"}}]}}]}`
	fr := &mockFrameRedactor{
		fn: func(events []*SSEEvent, _ *core.CompliancePipelineResult) ([]*SSEEvent, error) {
			out := make([]*SSEEvent, len(events))
			for i, e := range events {
				if strings.Contains(e.Data, "SECRET") {
					// Mask the text frame's content in place.
					masked := *e
					masked.Data = strings.ReplaceAll(e.Data, "SECRET", "[REDACTED]")
					out[i] = &masked
					continue
				}
				// Pass non-text / terminator frames unchanged (same pointer).
				out[i] = e
			}
			return out, nil
		},
	}

	mp := modifyPipeline()
	bp := NewBufferPipeline(BufferConfig{}, mp, slog.Default()).WithFrameRedactor(fr)

	modifyDegradedTotal.DeleteLabelValues("buffer_mode")
	modifyDegradedTotal.DeleteLabelValues(reasonRedactInflightUnsupported)

	var output bytes.Buffer
	result, err := bp.Process(context.Background(), strings.NewReader(mixedSSE("my SECRET token", toolJSON)),
		&output, &core.HookInput{Stage: "response", RequestID: "buf-redact-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || result.Decision != core.Modify {
		t.Fatalf("expected underlying Modify result, got %+v", result)
	}
	if fr.calls != 1 {
		t.Fatalf("expected FrameRedactor invoked exactly once, got %d", fr.calls)
	}

	out := output.String()
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected masked token in replay, got: %q", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Errorf("original sensitive content leaked into replay: %q", out)
	}
	// Non-text tool frame must be byte-verbatim (the redactor returned the
	// same pointer; the buffer must replay it unchanged).
	if !strings.Contains(out, "data: "+toolJSON) {
		t.Errorf("expected tool_call frame replayed byte-identical, got: %q", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Error("expected [DONE] terminator in replay")
	}

	// Counter NOT incremented for a supported redaction.
	if got := readCounter(t, "buffer_mode"); got != 0 {
		t.Errorf("buffer_mode counter must stay 0 on supported redaction, got %v", got)
	}
	if got := readCounter(t, reasonRedactInflightUnsupported); got != 0 {
		t.Errorf("unsupported counter must stay 0 on supported redaction, got %v", got)
	}
}

// TestBufferPipeline_ModifyRedactorUnsupported_FailsClosed asserts that
// when the FrameRedactor cannot reconstruct the wire (ErrRewriteUnsupported)
// the buffer fails CLOSED: zero original (unredacted) content reaches the
// client and the policy error frame is delivered. The buffer pipeline does NOT
// itself bump the coarse redact_inflight_unsupported counter on this path (#21):
// the FrameRedactor owns the degrade signal and records the more-informative
// ROOT-CAUSE label (the production sseFrameRedactor bumps tlsbump_splice_divergence /
// tlsbump_tool_arg_undeliverable in redactUnsupported); double-bumping the coarse
// counter here over-counted the strict-buffer fault. This test's mock redactor does
// not emit a root-cause label, so the coarse counter stays 0.
func TestBufferPipeline_ModifyRedactorUnsupported_FailsClosed(t *testing.T) {
	fr := &mockFrameRedactor{
		fn: func(_ []*SSEEvent, _ *core.CompliancePipelineResult) ([]*SSEEvent, error) {
			return nil, ErrRewriteUnsupported
		},
	}
	mp := modifyPipeline()
	bp := NewBufferPipeline(BufferConfig{}, mp, slog.Default()).WithFrameRedactor(fr)

	modifyDegradedTotal.DeleteLabelValues(reasonRedactInflightUnsupported)

	var output bytes.Buffer
	result, err := bp.Process(context.Background(),
		strings.NewReader(makeOpenAISSE("the SECRET is 42")), &output,
		&core.HookInput{Stage: "response", RequestID: "buf-redact-fc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || result.Decision != core.Modify {
		t.Fatalf("expected underlying Modify result, got %+v", result)
	}

	out := output.String()
	if strings.Contains(out, "SECRET") || strings.Contains(out, "42") {
		t.Errorf("fail-closed must NOT replay original content, leaked: %q", out)
	}
	if !strings.Contains(out, "blocked by policy") {
		t.Errorf("expected policy error frame on fail-closed, got: %q", out)
	}
	// #21: the buffer no longer double-bumps the coarse counter here — the redactor owns
	// the (root-cause) degrade signal. This mock emits none, so the coarse stays 0.
	if got := readCounter(t, reasonRedactInflightUnsupported); got != 0 {
		t.Errorf("expected coarse redact_inflight_unsupported counter == 0 (the redactor owns the degrade label), got %v", got)
	}
}

// TestBufferPipeline_ModifyNoRedactor_LegacyDegrade preserves the
// backward-compatible behavior for callers that wire no FrameRedactor:
// Modify is ignored, the body replays verbatim, and the buffer_mode
// degrade counter + WARN log surface the silent no-op.
func TestBufferPipeline_ModifyNoRedactor_LegacyDegrade(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bp := NewBufferPipeline(BufferConfig{}, modifyPipeline(), logger)

	modifyDegradedTotal.DeleteLabelValues("buffer_mode")

	var output bytes.Buffer
	result, err := bp.Process(context.Background(), strings.NewReader(makeOpenAISSE("verbatim", " bytes")),
		&output, &core.HookInput{Stage: "response", RequestID: "buf-legacy-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || result.Decision != core.Modify {
		t.Fatalf("expected underlying Modify result, got %+v", result)
	}
	out := output.String()
	if !strings.Contains(out, "verbatim") || !strings.Contains(out, "bytes") {
		t.Errorf("expected verbatim replay (Modify ignored without redactor), got: %q", out)
	}
	if !strings.Contains(logBuf.String(), "degrading to replay") {
		t.Errorf("expected WARN log about the no-redactor degrade, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "buf-legacy-1") {
		t.Errorf("expected requestId in degradation log, got: %s", logBuf.String())
	}
	if got := readCounter(t, "buffer_mode"); got != 1 {
		t.Errorf("expected buffer_mode counter == 1, got %v", got)
	}
}

// TestBufferPipeline_RedactorConcurrency runs two buffer redactions in
// parallel, each with its own FrameRedactor producing a distinct masked
// token, and asserts the outputs do not cross-contaminate (no shared
// write state). Run under -race to catch any package-global accumulator.
func TestBufferPipeline_RedactorConcurrency(t *testing.T) {
	run := func(token string) string {
		fr := &mockFrameRedactor{
			fn: func(events []*SSEEvent, _ *core.CompliancePipelineResult) ([]*SSEEvent, error) {
				out := make([]*SSEEvent, len(events))
				for i, e := range events {
					masked := *e
					masked.Data = strings.ReplaceAll(e.Data, "PLACEHOLDER", token)
					out[i] = &masked
				}
				return out, nil
			},
		}
		bp := NewBufferPipeline(BufferConfig{}, modifyPipeline(), slog.Default()).WithFrameRedactor(fr)
		var output bytes.Buffer
		_, err := bp.Process(context.Background(), strings.NewReader(makeOpenAISSE("PLACEHOLDER")),
			&output, &core.HookInput{Stage: "response"})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		return output.String()
	}

	var wg sync.WaitGroup
	var a, b string
	wg.Add(2)
	go func() { defer wg.Done(); a = run("AAAA") }()
	go func() { defer wg.Done(); b = run("BBBB") }()
	wg.Wait()

	if !strings.Contains(a, "AAAA") || strings.Contains(a, "BBBB") {
		t.Errorf("redaction A cross-contaminated: %q", a)
	}
	if !strings.Contains(b, "BBBB") || strings.Contains(b, "AAAA") {
		t.Errorf("redaction B cross-contaminated: %q", b)
	}
}

// TestBufferPipeline_CoFiringBlockSoft_RedactDelivers proves the #13 appliance-leak fix:
// a co-firing soft-block promotes the aggregate Decision to BlockSoft but carries the
// redact's content, so the buffer routes it through the FrameRedactor (gating on
// CarriesRedaction, not Decision==Modify) and delivers the MASKED stream — not the
// original, which the old Decision==Modify switch fell through to `default` and replayed
// raw (the leak this fixes).
func TestBufferPipeline_CoFiringBlockSoft_RedactDelivers(t *testing.T) {
	fr := &mockFrameRedactor{
		fn: func(events []*SSEEvent, _ *core.CompliancePipelineResult) ([]*SSEEvent, error) {
			out := make([]*SSEEvent, len(events))
			for i, e := range events {
				masked := *e
				masked.Data = strings.ReplaceAll(e.Data, "SECRET", "[REDACTED]")
				out[i] = &masked
			}
			return out, nil
		},
	}
	mp := &mockPipeline{decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
		// Models a real co-firing redact (a Modify hook's ModifiedContent) masked by a
		// soft-block: mergeResults would set RedactionApplicable; this test bypasses the
		// merge, so it sets the flag explicitly to stay faithful to the aggregate shape.
		return &core.CompliancePipelineResult{Decision: core.BlockSoft, ModifiedContent: []core.ContentBlock{{}}, RedactionApplicable: true, Reason: "soft-block masking redact"}
	}}
	bp := NewBufferPipeline(BufferConfig{}, mp, slog.Default()).WithFrameRedactor(fr)

	var output bytes.Buffer
	result, err := bp.Process(context.Background(), strings.NewReader(makeOpenAISSE("my SECRET token")),
		&output, &core.HookInput{Stage: "response", RequestID: "buf-cofire-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := output.String()
	if strings.Contains(out, "SECRET") {
		t.Fatalf("co-firing BlockSoft leaked the original content: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("co-firing BlockSoft must redact-deliver the masked stream, got %q", out)
	}
	// #13 P3: the disposition is stamped redact (the mask WAS applied), not the BlockSoft
	// ceiling — so the audit row reads action=redact for the redact-delivered stream.
	if result == nil || result.Action != core.ActionRedact {
		t.Fatalf("result.Action = %v, want redact (an applied splice is a redact disposition, not block)", result)
	}
}

// TestBufferPipeline_CoFiringBlockSoft_FailOpenDegrade_KeepsBlockAction pins the #13 P3
// guard: when the redactor CANNOT splice and fails open (agent: returns the original frames
// with a nil error after stamping ReasonRedactInflightUnsupported), the original was relayed,
// so the disposition must NOT be re-stamped redact — it stays the aggregate BlockSoft/block.
func TestBufferPipeline_CoFiringBlockSoft_FailOpenDegrade_KeepsBlockAction(t *testing.T) {
	fr := &mockFrameRedactor{
		fn: func(events []*SSEEvent, result *core.CompliancePipelineResult) ([]*SSEEvent, error) {
			// Agent fail-open degrade: relay the ORIGINAL frames unchanged, nil error,
			// and stamp the disclosed unsupported reason (mirrors sseFrameRedactor).
			result.ReasonCode = core.ReasonRedactInflightUnsupported
			return events, nil
		},
	}
	mp := &mockPipeline{decideFn: func(_ context.Context, _ *core.HookInput) *core.CompliancePipelineResult {
		return &core.CompliancePipelineResult{Decision: core.BlockSoft, Action: core.ActionBlock, ModifiedContent: []core.ContentBlock{{}}, Reason: "soft-block masking redact"}
	}}
	bp := NewBufferPipeline(BufferConfig{}, mp, slog.Default()).WithFrameRedactor(fr)

	var output bytes.Buffer
	result, err := bp.Process(context.Background(), strings.NewReader(makeOpenAISSE("token")),
		&output, &core.HookInput{Stage: "response", RequestID: "buf-cofire-failopen"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || result.Action != core.ActionBlock {
		t.Fatalf("result.Action = %v, want block — the fail-open degrade relayed the original, so it must NOT stamp redact", result)
	}
}

// TestBufferPipeline_NoRedactor_Strict_FailsClosed proves GAP B for the appliance: a
// redaction with no frame redactor wired fails CLOSED (error frame, no original) under
// strictFailClosed, instead of the agent's degrade-replay.
func TestBufferPipeline_NoRedactor_Strict_FailsClosed(t *testing.T) {
	bp := NewBufferPipeline(BufferConfig{}, modifyPipeline(), slog.Default()).WithStrictFailClosed(true)

	var output bytes.Buffer
	if _, err := bp.Process(context.Background(), strings.NewReader(makeOpenAISSE("verbatim SECRET")),
		&output, &core.HookInput{Stage: "response", RequestID: "buf-strict-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := output.String()
	if strings.Contains(out, "SECRET") {
		t.Fatalf("strict appliance must not replay the original when no redactor is wired, got %q", out)
	}
	if !strings.Contains(out, `"error"`) {
		t.Fatalf("strict appliance must fail closed with an error frame, got %q", out)
	}
}
