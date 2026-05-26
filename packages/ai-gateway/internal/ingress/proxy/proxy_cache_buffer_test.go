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

// testWriter is a minimal http.ResponseWriter shim wrapping a
// bytes.Buffer so tests can assert what runBufferStream wrote without
// needing an httptest.ResponseRecorder.
type testWriter struct {
	*bytes.Buffer
	header     http.Header
	statusCode int
}

func (w *testWriter) Header() http.Header     { return w.header }
func (w *testWriter) WriteHeader(status int)  { w.statusCode = status }

// TestRunBufferStream_HappyPath_ReplaysToTee — happy path: hookRunner
// approves, BufferPipeline reads + replays SSE body to the tee, and
// the StreamHookContext.OnCheckpoint callback fires with the result.
func TestRunBufferStream_HappyPath_ReplaysToTee(t *testing.T) {
	body := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	var tee bytes.Buffer

	var onCheckpointCalls atomic.Int32
	hookCtx := &streaming.StreamHookContext{
		RequestID:   "buf-1",
		IngressType: "AI_GATEWAY",
		OnCheckpoint: func(*hookcore.CompliancePipelineResult) {
			onCheckpointCalls.Add(1)
		},
	}
	runner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}

	runBufferStream(context.Background(), runStreamDeps{
		Deps:        &Deps{},
		AdapterType: "openai",
		Path:        "/v1/chat/completions",
		AcceptHeader: "text/event-stream",
		HookRunner:   runner,
		HookCtx:      hookCtx,
		SSEReader:    body,
		Tee:          &testWriter{Buffer: &tee, header: http.Header{}},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if tee.Len() == 0 {
		t.Errorf("expected buffer pipeline to replay events into tee, got 0 bytes")
	}
	if onCheckpointCalls.Load() != 1 {
		t.Errorf("OnCheckpoint should fire exactly once in buffer mode, got %d", onCheckpointCalls.Load())
	}
}

// TestRunBufferStream_Reject_WritesErrorEvent — RejectHard: hook
// pipeline rejects; BufferPipeline writes a single error event +
// [DONE] (writeErrorAndDone) instead of the buffered events.
func TestRunBufferStream_Reject_WritesErrorEvent(t *testing.T) {
	body := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"forbidden\"}}]}\n\ndata: [DONE]\n\n")
	var tee bytes.Buffer

	runner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		return &hookcore.CompliancePipelineResult{Decision: hookcore.RejectHard, Reason: "blocked by test"}
	}
	hookCtx := &streaming.StreamHookContext{RequestID: "buf-reject"}

	runBufferStream(context.Background(), runStreamDeps{
		Deps:        &Deps{},
		AdapterType: "openai",
		HookRunner:  runner,
		HookCtx:     hookCtx,
		SSEReader:   body,
		Tee:         &testWriter{Buffer: &tee, header: http.Header{}},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// #115/O3 — assert STRUCTURAL invariants rather than the literal
	// "blocked by policy" string (which belongs to shared.BufferPipeline's
	// writeErrorAndDone; coupling at the substring level chains
	// ai-gateway's contract to a shared-package implementation detail).
	// What ai-gateway requires: (a) the rejected stream does NOT replay
	// the upstream "forbidden" payload, (b) an SSE error event is
	// emitted as a JSON object with an "error" field, (c) the stream is
	// terminated with the standard SSE [DONE] sentinel.
	out := tee.String()
	if strings.Contains(out, "forbidden") {
		t.Errorf("rejected stream must NOT replay upstream payload; leaked %q", out)
	}
	if !strings.Contains(out, "data: {") || !strings.Contains(out, "\"error\"") {
		t.Errorf("expected SSE error event JSON object with 'error' field, got: %q", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("expected [DONE] terminator after reject error, got: %q", out)
	}
}

// (Removed) TestBufferModeExecutor_PassesUnderlyingResult was a
// table test against a 1-line type bridge — covered nothing observable
// beyond what the Go compiler already enforces. PR #24 / S3 cleanup.
// If bufferModeExecutor ever grows real logic (rate limiting, retry,
// audit-stamp), reinstate a test asserting THAT behavior, not the
// pass-through invariant.

// TestBuildBufferPreHookCallback_NilDeps_ReturnsNil — defensive nil
// guard so a caller that wires Deps incorrectly doesn't panic; falls
// back to no preHook installed.
func TestBuildBufferPreHookCallback_NilDeps_ReturnsNil(t *testing.T) {
	if cb := buildBufferPreHookCallback(context.Background(), nil, "openai", "/v1/chat/completions", "text/event-stream"); cb != nil {
		t.Errorf("nil Deps must return nil callback, got non-nil")
	}
}

// TestBuildBufferPreHookCallback_StampsNormalized — happy path: real
// Registry seeded with the openai chat normalizer, SSE body fed
// through, callback stamps ci.Normalized.
func TestBuildBufferPreHookCallback_StampsNormalized(t *testing.T) {
	reg := normcore.NewRegistry()
	codecs.RegisterDefaultAIBuiltins(reg)
	deps := &Deps{NormalizeRegistry: reg}

	cb := buildBufferPreHookCallback(context.Background(), deps, "openai", "/v1/chat/completions", "text/event-stream")
	if cb == nil {
		t.Fatal("expected non-nil callback for wired Deps")
	}

	ci := &hookcore.HookInput{}
	cb([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), ci)
	if ci.Normalized == nil {
		t.Fatal("expected ci.Normalized stamped by preHook; got nil")
	}
}

// TestRunBufferStream_ProcessError_Surfaced — when BufferPipeline.
// Process returns an error (e.g. read failure from upstream), the
// error wraps through the result chain rather than being silently
// swallowed. We feed a reader that returns an error after one chunk;
// BufferPipeline turns the error into a wrapped Process error.
func TestRunBufferStream_ProcessError_Surfaced(t *testing.T) {
	body := &yieldThenErrReader{first: []byte("data: x\n\n"), err: io.ErrUnexpectedEOF}
	var teeBuf bytes.Buffer
	tee := &testWriter{Buffer: &teeBuf, header: http.Header{}}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	runner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}
	runBufferStream(context.Background(), runStreamDeps{
		Deps:       &Deps{},
		HookRunner: runner,
		HookCtx:    &streaming.StreamHookContext{RequestID: "buf-err"},
		SSEReader:  body,
		Tee:        tee,
		Logger:     logger,
	})
	if !strings.Contains(logBuf.String(), "buffer pipeline error") {
		t.Errorf("expected 'buffer pipeline error' log line on read failure, got: %s", logBuf.String())
	}
}

// TestRunBufferStream_MaxBufferBytes_PropagatesToPipeline — #115/O6
// follow-up. runBufferStream now reads MaxBufferBytes from
// runStreamDeps (populated from streampolicy.Store at the dispatch
// site) and threads it into BufferConfig.MaxBufferSize. Before this
// fix the buffer pipeline silently capped at the 8MB default
// regardless of admin policy, while tlsbump callers honored the
// 64MB default policy cap.
//
// Test strategy: feed a body LARGER than the requested MaxBufferBytes
// and assert the underlying BufferPipeline surfaces the overflow as
// an error (the "stream exceeded maximum buffer size" path). The
// tee output stays empty because Phase 3 never reaches replay.
func TestRunBufferStream_MaxBufferBytes_PropagatesToPipeline(t *testing.T) {
	// 200 bytes per data chunk; MaxBufferBytes=100 means the first
	// chunk's data alone exceeds the cap.
	body := "data: " + strings.Repeat("x", 200) + "\n\ndata: [DONE]\n\n"
	var teeBuf bytes.Buffer
	tee := &testWriter{Buffer: &teeBuf, header: http.Header{}}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	runner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}

	runBufferStream(context.Background(), runStreamDeps{
		Deps:           &Deps{},
		HookRunner:     runner,
		HookCtx:        &streaming.StreamHookContext{RequestID: "buf-cap"},
		SSEReader:      strings.NewReader(body),
		Tee:            tee,
		Logger:         logger,
		MaxBufferBytes: 100, // tight cap, smaller than first chunk
	})

	// BufferPipeline surfaces overflow as a "buffer pipeline error" log
	// (the wrapper around the "stream exceeded maximum buffer size" err
	// from shared/transport/streaming/buffer.go). Replay must NOT have
	// fired — tee should not contain the body.
	if !strings.Contains(logBuf.String(), "buffer pipeline error") {
		t.Errorf("expected 'buffer pipeline error' on MaxBufferBytes overflow; got log: %s", logBuf.String())
	}
	if strings.Contains(teeBuf.String(), "xxxxxxxx") {
		t.Errorf("overflow body must NOT replay to tee; got %d bytes", teeBuf.Len())
	}
}

// TestRunBufferStream_PreHookInstalled_WhenRegistryWired — when Deps
// carries a NormalizeRegistry, runBufferStream MUST install the
// PreHookCallback (the `if cb != nil { bp.WithPreHook(cb) }` branch
// in proxy_cache_buffer.go). We prove the branch ran by observing
// that ci.Normalized lands populated for an OpenAI body — the
// callback's only side effect is stamping it.
func TestRunBufferStream_PreHookInstalled_WhenRegistryWired(t *testing.T) {
	reg := normcore.NewRegistry()
	codecs.RegisterDefaultAIBuiltins(reg)
	body := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	var tee bytes.Buffer

	var seenNormalized atomic.Bool
	runner := func(_ context.Context, in *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		// The PreHook stamps Normalized BEFORE the executor sees the
		// input — if the WithPreHook branch ran, this assertion fires.
		if in.Normalized != nil {
			seenNormalized.Store(true)
		}
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}

	runBufferStream(context.Background(), runStreamDeps{
		Deps:         &Deps{NormalizeRegistry: reg},
		AdapterType:  "openai",
		Path:         "/v1/chat/completions",
		AcceptHeader: "text/event-stream",
		HookRunner:   runner,
		HookCtx:      &streaming.StreamHookContext{RequestID: "buf-prehook"},
		SSEReader:    body,
		Tee:          &testWriter{Buffer: &tee, header: http.Header{}},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if !seenNormalized.Load() {
		t.Errorf("PreHookCallback wasn't installed (executor saw nil Normalized)")
	}
}

// TestRunBufferStream_NilSSEReaderOrTee_NoOp — PR #24 follow-up
// S4-code defensive nil-guard. Production always wires both; this
// test pins the no-op path so a future regression that omits the
// guard would either panic (caught by recover) or stamp side effects
// we then assert are absent.
func TestRunBufferStream_NilSSEReaderOrTee_NoOp(t *testing.T) {
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
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("runBufferStream panicked on %s: %v", tc.name, r)
				}
			}()
			runBufferStream(context.Background(), tc.d)
		})
	}
}

// TestRunBufferStream_NilHookCtx_DoesNotPanicAndStillReplays —
// defensive: production always supplies hookCtx, but the function
// must not panic if a future wiring path passes nil, AND the buffer
// pipeline must still run + replay bytes (the OnCheckpoint side-
// effect is the only thing that should be skipped). Pre-PR #24
// follow-up review: this test only asserted no-panic via recover —
// a regression that silently dropped the stream would have passed.
// S2-test review fix: assert tee actually received the replayed body.
func TestRunBufferStream_NilHookCtx_DoesNotPanicAndStillReplays(t *testing.T) {
	body := strings.NewReader("data: hi\n\ndata: [DONE]\n\n")
	var teeBuf bytes.Buffer
	tee := &testWriter{Buffer: &teeBuf, header: http.Header{}}
	runner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("runBufferStream panicked with nil HookCtx: %v", r)
		}
	}()
	runBufferStream(context.Background(), runStreamDeps{
		Deps:       &Deps{},
		HookRunner: runner,
		HookCtx:    nil,
		SSEReader:  body,
		Tee:        tee,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// Even with nil HookCtx the pipeline MUST replay the body —
	// a silent drop on nil HookCtx would pass the recover-only check.
	if !strings.Contains(teeBuf.String(), "hi") {
		t.Errorf("expected tee to receive replayed 'hi' body with nil HookCtx; got %q", teeBuf.String())
	}
	if !strings.Contains(teeBuf.String(), "[DONE]") {
		t.Errorf("expected [DONE] terminator in tee with nil HookCtx; got %q", teeBuf.String())
	}
}
