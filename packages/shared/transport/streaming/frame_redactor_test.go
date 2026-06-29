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
// client, the policy error frame is delivered, and the
// redact_inflight_unsupported counter is bumped once.
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
	if got := readCounter(t, reasonRedactInflightUnsupported); got != 1 {
		t.Errorf("expected redact_inflight_unsupported counter == 1, got %v", got)
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
	if !strings.Contains(logBuf.String(), "Modify decision degraded to Approve") {
		t.Errorf("expected WARN log about degradation, got: %s", logBuf.String())
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
