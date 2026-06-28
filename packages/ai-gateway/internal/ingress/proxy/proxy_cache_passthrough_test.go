package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/streaming"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// TestRunPassthroughStream_HappyPath_RelaysBytesNoHook — admin set
// passthrough mode, so bytes must flow client-bound without any
// invocation of hookRunner. The hookRunner is wired to a sentinel
// counter that MUST stay at zero.
func TestRunPassthroughStream_HappyPath_RelaysBytesNoHook(t *testing.T) {
	body := "data: hello\n\ndata: world\n\ndata: [DONE]\n\n"
	var teeBuf bytes.Buffer
	tee := &testWriter{Buffer: &teeBuf, header: http.Header{}}

	var hookCalls atomic.Int32
	sentinelHookRunner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		hookCalls.Add(1)
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}

	runPassthroughStream(context.Background(), runStreamDeps{
		Deps:       &Deps{},
		HookRunner: sentinelHookRunner,
		HookCtx:    &streaming.StreamHookContext{RequestID: "pass-1"},
		SSEReader:  strings.NewReader(body),
		Tee:        tee,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if hookCalls.Load() != 0 {
		t.Errorf("passthrough must NOT invoke hookRunner; got %d calls", hookCalls.Load())
	}
	if teeBuf.String() != body {
		t.Errorf("passthrough must relay bytes verbatim;\n  got:  %q\n  want: %q", teeBuf.String(), body)
	}
}

// TestRunPassthroughStream_FlusherCalled — tee implements
// http.Flusher (testWriter does). Each Write should be followed by
// Flush so SSE clients see chunks immediately rather than waiting
// for OS buffer fill.
func TestRunPassthroughStream_FlusherCalled(t *testing.T) {
	// Use a flusher-tracking writer instead of testWriter so we can
	// count flush calls precisely.
	tracker := &flushTracker{Buffer: &bytes.Buffer{}, header: http.Header{}}
	body := "data: chunk-1\n\ndata: chunk-2\n\n"

	runPassthroughStream(context.Background(), runStreamDeps{
		Deps:      &Deps{},
		HookCtx:   &streaming.StreamHookContext{RequestID: "pass-flush"},
		SSEReader: strings.NewReader(body),
		Tee:       tracker,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if tracker.flushCount.Load() == 0 {
		t.Errorf("expected Flush to be called at least once for SSE relay; got 0")
	}
	if tracker.String() != body {
		t.Errorf("flusher-wrapped writer must still relay verbatim;\n  got:  %q\n  want: %q", tracker.String(), body)
	}
}

// TestRunPassthroughStream_NilSafe — defensive: nil SSEReader / nil
// Tee must short-circuit without panic. Production wires both, but
// the helper survives degraded callers.
func TestRunPassthroughStream_NilSafe(t *testing.T) {
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
					t.Fatalf("runPassthroughStream panicked on %s: %v", tc.name, r)
				}
			}()
			runPassthroughStream(context.Background(), tc.d)
		})
	}
}

// TestRunPassthroughStream_UpstreamReadError_Logged — upstream read
// error (non-ctx-cancel) must surface via WARN log so operators see
// the upstream failure. ctx cancel is silenced (client's choice to
// disconnect).
func TestRunPassthroughStream_UpstreamReadError_Logged(t *testing.T) {
	tee := &testWriter{Buffer: &bytes.Buffer{}, header: http.Header{}}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	runPassthroughStream(context.Background(), runStreamDeps{
		SSEReader: &yieldThenErrReader{first: []byte("data: partial\n\n"), err: io.ErrUnexpectedEOF},
		Tee:       tee,
		Logger:    logger,
		HookCtx:   &streaming.StreamHookContext{RequestID: "pass-err"},
	})

	if !strings.Contains(logBuf.String(), "passthrough stream copy error") {
		t.Errorf("expected WARN log on upstream error, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "pass-err") {
		t.Errorf("WARN log should include requestID, got: %s", logBuf.String())
	}
}

// TestRunPassthroughStream_CtxCancel_NoErrorLog — ctx cancel is the
// client disconnecting mid-stream. Should NOT log an error — that
// would spam logs on every client-side abort.
func TestRunPassthroughStream_CtxCancel_NoErrorLog(t *testing.T) {
	tee := &testWriter{Buffer: &bytes.Buffer{}, header: http.Header{}}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE Copy starts

	runPassthroughStream(ctx, runStreamDeps{
		SSEReader: &yieldThenErrReader{first: []byte("x"), err: context.Canceled},
		Tee:       tee,
		Logger:    logger,
		HookCtx:   &streaming.StreamHookContext{RequestID: "pass-ctx"},
	})

	if strings.Contains(logBuf.String(), "passthrough stream copy error") {
		t.Errorf("ctx cancel should not produce error log (client-disconnect spam guard), got: %s", logBuf.String())
	}
}

// TestRunPassthroughStream_NetErrClosed_Silenced pins the behavior:
// when CloseUpstreamOnExit fires in a sibling
// pipeline (or the caller's outer defer Close races this relay),
// io.Copy returns net.ErrClosed. This sentinel must be in the silence
// list, otherwise every such teardown produces a "passthrough stream
// copy error" warning.
// net.ErrClosed silences the same way ctx.Canceled
// does. Without it, the assert-empty-log check fails.
func TestRunPassthroughStream_NetErrClosed_Silenced(t *testing.T) {
	tee := &testWriter{Buffer: &bytes.Buffer{}, header: http.Header{}}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	runPassthroughStream(context.Background(), runStreamDeps{
		SSEReader: &yieldThenErrReader{first: []byte("data: partial\n\n"), err: net.ErrClosed},
		Tee:       tee,
		Logger:    logger,
		HookCtx:   &streaming.StreamHookContext{RequestID: "pass-net-closed"},
	})

	if strings.Contains(logBuf.String(), "passthrough stream copy error") {
		t.Errorf("net.ErrClosed should not produce error log (post-close teardown noise guard); got: %s", logBuf.String())
	}
}

// --- helpers ---

// flushTracker is a writer that implements http.ResponseWriter +
// http.Flusher, counting Flush calls so tests can prove
// shared.Passthrough (which runPassthroughStream delegates to) calls
// Flush after each read against http.Flusher-bearing writers.
type flushTracker struct {
	*bytes.Buffer
	header     http.Header
	flushCount atomic.Int32
}

func (f *flushTracker) Header() http.Header { return f.header }
func (f *flushTracker) WriteHeader(_ int)   {}
func (f *flushTracker) Flush()              { f.flushCount.Add(1) }

// yieldThenErrReader lives in test_helpers_test.go.
