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
		Deps:         deps,
		AdapterType:  "openai",
		Path:         "/v1/chat/completions",
		AcceptHeader: "text/event-stream",
		HookRunner:   runner,
		HookCtx:      hookCtx,
		SSEReader:    body,
		Tee:          tee,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmitDone:     true,
	})

	if teeBuf.Len() == 0 {
		t.Errorf("expected LivePipeline to forward bytes to tee, got 0")
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
