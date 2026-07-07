package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/streaming"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestRunLiveStream_HappyPath_FlowsThroughLivePipeline — verifies the
// runLiveStream helper builds a LivePipeline + installs PreHook + runs
// Process. Symmetric with the runBufferStream tests; the live helper
// is the chunked_async counterpart in the streaming-relay dispatch.
func TestRunLiveStream_HappyPath_FlowsThroughLivePipeline(t *testing.T) {
	body := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	var teeBuf bytes.Buffer
	tee := &testWriter{Buffer: &teeBuf, header: http.Header{}}

	var hookCalls atomic.Int32
	hookCtx := &streaming.StreamHookContext{
		RequestID:   "live-1",
		IngressType: "AI_GATEWAY",
		OnCheckpoint: func(*hookcore.CompliancePipelineResult) {
			hookCalls.Add(1)
		},
	}
	runner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}

	// Wired Deps with a real Registry so the PreHook path exercises
	// the normalize-before-hooks branch (covering the deps != nil
	// guard inside buildStreamPreHookCallback).
	reg := normcore.NewRegistry()
	codecs.RegisterDefaultAIBuiltins(reg)
	deps := &Deps{NormalizeRegistry: reg}

	runLiveStream(context.Background(), runStreamDeps{
		Deps:             deps,
		AdapterType:      "openai",
		Path:             "/v1/chat/completions",
		AcceptHeader:     "text/event-stream",
		HookRunner:       runner,
		HookCtx:          hookCtx,
		HasResponseHooks: true, // response rules bound → PreHook installed + checkpoints run
		SSEReader:        body,
		Tee:              tee,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmitDone:         true,
	})

	if teeBuf.Len() == 0 {
		t.Errorf("expected LivePipeline to forward bytes to tee, got 0")
	}
	// With response hooks bound the checkpoint MUST fire (the runner is kept,
	// not nil'd) — without this assertion the test went blind to the
	// HasResponseHooks gate.
	if hookCalls.Load() == 0 {
		t.Error("expected OnCheckpoint to fire with HasResponseHooks=true")
	}
}

// TestRunLiveStream_ResponseHooksGate pins the HasResponseHooks gate: with a
// response rule bound the runner is kept and checkpoints fire; with none bound
// the runner is nil'd and checkpoints are skipped — but in BOTH cases the
// persisted (tee) body carries the full transcript, since skipping the audit
// scan must never drop delivered/persisted bytes.
func TestRunLiveStream_ResponseHooksGate(t *testing.T) {
	for _, tc := range []struct {
		name             string
		hasResponseHooks bool
		wantCheckpoints  bool
	}{
		{"hooks_bound_runs_checkpoints", true, true},
		{"no_hooks_skips_checkpoints", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hello world\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\" more\"}}]}\n\ndata: [DONE]\n\n")
			var teeBuf bytes.Buffer
			tee := &testWriter{Buffer: &teeBuf, header: http.Header{}}

			var hookCalls atomic.Int32
			hookCtx := &streaming.StreamHookContext{
				RequestID:    "gate-" + tc.name,
				IngressType:  "AI_GATEWAY",
				OnCheckpoint: func(*hookcore.CompliancePipelineResult) { hookCalls.Add(1) },
			}
			runner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
				return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
			}
			reg := normcore.NewRegistry()
			codecs.RegisterDefaultAIBuiltins(reg)

			runLiveStream(context.Background(), runStreamDeps{
				Deps:             &Deps{NormalizeRegistry: reg},
				AdapterType:      "openai",
				Path:             "/v1/chat/completions",
				HookRunner:       runner,
				HookCtx:          hookCtx,
				HasResponseHooks: tc.hasResponseHooks,
				SSEReader:        body,
				Tee:              tee,
				Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
				EmitDone:         true,
			})

			// (a) OnCheckpoint fires ONLY when response hooks are bound.
			if got := hookCalls.Load() > 0; got != tc.wantCheckpoints {
				t.Errorf("checkpoints fired=%v, want %v (HasResponseHooks=%v)", got, tc.wantCheckpoints, tc.hasResponseHooks)
			}
			// (b) The persisted (tee) body carries the FULL transcript in BOTH cases.
			persisted := teeBuf.String()
			for _, want := range []string{"hello world", " more", "[DONE]"} {
				if !strings.Contains(persisted, want) {
					t.Errorf("persisted tee body missing %q (HasResponseHooks=%v); got %q", want, tc.hasResponseHooks, persisted)
				}
			}
		})
	}
}

// TestRunLiveStream_NilDeps_NoPreHook_StillRuns — when Deps is nil
// (degraded wiring) buildStreamPreHookCallback short-circuits and
// LivePipeline runs without a PreHook callback. Body still forwards.
func TestRunLiveStream_NilDeps_NoPreHook_StillRuns(t *testing.T) {
	body := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n")
	var teeBuf bytes.Buffer
	tee := &testWriter{Buffer: &teeBuf, header: http.Header{}}

	runner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}
	hookCtx := &streaming.StreamHookContext{RequestID: "live-nodep"}

	runLiveStream(context.Background(), runStreamDeps{
		Deps:       nil,
		HookRunner: runner,
		HookCtx:    hookCtx,
		SSEReader:  body,
		Tee:        tee,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if teeBuf.Len() == 0 {
		t.Errorf("expected LivePipeline to forward bytes even without Deps; got 0")
	}
}

// TestRunLiveStream_NilSSEReaderOrTee_NoOp — defensive nil-guard.
// Symmetric with runBufferStream and
// runPassthroughStream guards. Production always wires both; this
// test pins the no-op fallback so a future malformed runStreamDeps
// doesn't nil-deref into a 502.
func TestRunLiveStream_NilSSEReaderOrTee_NoOp(t *testing.T) {
	tests := []struct {
		name string
		d    runStreamDeps
	}{
		{
			name: "nil_reader",
			d: runStreamDeps{
				Tee:    &testWriter{Buffer: &bytes.Buffer{}, header: http.Header{}},
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			},
		},
		{
			name: "nil_tee",
			d: runStreamDeps{
				SSEReader: strings.NewReader("data: x\n\n"),
				Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			},
		},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("runLiveStream panicked on %s: %v", tc.name, r)
				}
			}()
			runLiveStream(context.Background(), tc.d)
		})
	}
}
