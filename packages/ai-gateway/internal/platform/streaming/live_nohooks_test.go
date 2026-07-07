package streaming

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// TestLivePipeline_NilHookRun_SkipsCheckpoints verifies that when no response
// hooks are bound (nil runner), the pipeline skips EVERY audit checkpoint
// (intermediate + final) yet still delivers the full body + [DONE]. The
// FirstInspect/Reinspect chars are set to 1 so a checkpoint would fire after
// every chunk if the scan ran — proving the skip.
func TestLivePipeline_NilHookRun_SkipsCheckpoints(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"Hello "}}]}`,
		`{"choices":[{"delta":{"content":"world"}}]}`,
	)
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars:  1,
		ReinspectStepChars: 1,
		EmitOpenAIDone:     true,
	}, nil, nil, slog.Default())
	rec := httptest.NewRecorder()
	calls := 0
	hookCtx := &StreamHookContext{
		IngressType:  "AI_GATEWAY",
		Path:         "/v1/chat/completions",
		OnCheckpoint: func(*goHooks.CompliancePipelineResult) { calls++ },
	}
	if lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx) {
		t.Fatal("nil-hook stream must not block")
	}
	if calls != 0 {
		t.Errorf("nil hookRun must skip ALL checkpoints (intermediate + final); OnCheckpoint fired %d times", calls)
	}
	body := rec.Body.String()
	for _, want := range []string{"Hello ", "world", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("body must contain %q (full delivery); got %q", want, body)
		}
	}
}

// TestLivePipeline_WithHookRun_StillCheckpoints is the counterpart: a bound
// (non-nil) runner must STILL run checkpoints — the skip is gated strictly on
// "no response hooks", never on the mode.
func TestLivePipeline_WithHookRun_StillCheckpoints(t *testing.T) {
	input := makeSSEStream(
		`{"choices":[{"delta":{"content":"aaaaa"}}]}`,
		`{"choices":[{"delta":{"content":"bbbbb"}}]}`,
	)
	lp := NewLivePipeline(LiveConfig{FirstInspectChars: 1, ReinspectStepChars: 1}, approveStreamHook, nil, slog.Default())
	rec := httptest.NewRecorder()
	calls := 0
	hookCtx := &StreamHookContext{
		IngressType:  "AI_GATEWAY",
		Path:         "/v1/chat/completions",
		OnCheckpoint: func(*goHooks.CompliancePipelineResult) { calls++ },
	}
	lp.Process(context.Background(), strings.NewReader(input), rec, hookCtx)
	if calls == 0 {
		t.Error("a bound hook runner must still fire checkpoints")
	}
}

// BenchmarkLivePipeline_Process is the before/after: no-response-hooks (nil
// runner, the skip path) vs a bound runner, over a 50-token stream. The nil arm
// should drop the per-checkpoint HookInput + PayloadFromTextSegments allocs and
// the per-chunk ExtractDeltaText JSON parse.
func BenchmarkLivePipeline_Process(b *testing.B) {
	frames := make([]string, 50)
	for i := range frames {
		frames[i] = `{"choices":[{"delta":{"content":"token "}}]}`
	}
	input := makeSSEStream(frames...)
	newCtx := func() *StreamHookContext {
		return &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"}
	}
	b.Run("no_response_hooks_nil", func(b *testing.B) {
		lp := NewLivePipeline(LiveConfig{EmitOpenAIDone: true}, nil, nil, slog.Default())
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			lp.Process(context.Background(), strings.NewReader(input), httptest.NewRecorder(), newCtx())
		}
	})
	b.Run("with_hooks", func(b *testing.B) {
		lp := NewLivePipeline(LiveConfig{EmitOpenAIDone: true}, approveStreamHook, nil, slog.Default())
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			lp.Process(context.Background(), strings.NewReader(input), httptest.NewRecorder(), newCtx())
		}
	})
}
