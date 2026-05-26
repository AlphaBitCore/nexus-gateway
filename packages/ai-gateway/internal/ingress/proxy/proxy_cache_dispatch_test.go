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
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// TestDispatchStreamMode_RoutesEachKnownMode pins which run* function
// each streampolicy.Mode dispatches to. The dispatch surface is the
// three-service alignment boundary — agent / compliance-proxy /
// ai-gateway must agree on the mode→pipeline mapping. A wiring
// regression that routes buffer→live or live→passthrough would silently
// flip admin policy semantics on prod traffic; this table-driven test
// catches it at unit-test time.
func TestDispatchStreamMode_RoutesEachKnownMode(t *testing.T) {
	body := "data: hello\n\ndata: [DONE]\n\n"

	cases := []struct {
		name           string
		mode           streampolicy.Mode
		wantHookCalls  int32  // ≥1 if pipeline runs hooks, 0 if passthrough/buffer-non-modify
		wantBodyPart   string // must appear in tee output
		wantNoHookCall bool   // when true assert hookRunner is NEVER called
	}{
		{
			name:           "buffer_full_block runs buffer pipeline",
			mode:           streampolicy.ModeBufferFullBlock,
			wantBodyPart:   "hello", // body replays after Approve
			wantNoHookCall: false,   // buffer pipeline DOES call hookRunner once
		},
		{
			name:           "passthrough runs passthrough relay",
			mode:           streampolicy.ModePassThrough,
			wantBodyPart:   "hello",
			wantNoHookCall: true, // passthrough MUST NOT invoke hookRunner
		},
		{
			name:           "chunked_async runs live pipeline",
			mode:           streampolicy.ModeChunkedAsync,
			wantBodyPart:   "hello",
			wantNoHookCall: false, // live pipeline calls hookRunner at checkpoints
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var teeBuf bytes.Buffer
			tee := &testWriter{Buffer: &teeBuf, header: http.Header{}}
			var hookCalls atomic.Int32
			runner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
				hookCalls.Add(1)
				return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
			}
			dispatchStreamMode(context.Background(), tc.mode, runStreamDeps{
				Deps:       &Deps{},
				HookRunner: runner,
				HookCtx:    &streaming.StreamHookContext{RequestID: "dispatch-test"},
				SSEReader:  strings.NewReader(body),
				Tee:        tee,
				Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			if !strings.Contains(teeBuf.String(), tc.wantBodyPart) {
				t.Errorf("expected body part %q in tee output; got %q", tc.wantBodyPart, teeBuf.String())
			}
			if tc.wantNoHookCall && hookCalls.Load() != 0 {
				t.Errorf("%s must NOT invoke hookRunner; got %d calls", tc.name, hookCalls.Load())
			}
		})
	}
}

// TestDispatchStreamMode_UnknownEnumFallsBackToPassthrough is the
// three-service alignment invariant. Both tlsbump's
// resolveStreamingMode and ai-gateway's dispatch MUST fall back to
// passthrough on an unknown enum value. The conservative default is
// "do not silently engage compliance hooks against opted-out
// traffic" — bytes flow verbatim and the misconfigured mode surfaces
// elsewhere (validation, admin UI warnings). Previously this default
// engaged live mode, which would flip a typo into a hook-running
// session on whatever traffic landed first.
func TestDispatchStreamMode_UnknownEnumFallsBackToPassthrough(t *testing.T) {
	body := "data: forbidden-content\n\ndata: [DONE]\n\n"
	var teeBuf bytes.Buffer
	tee := &testWriter{Buffer: &teeBuf, header: http.Header{}}

	// Sentinel hookRunner — if invoked, the test fails because the
	// unknown-mode arm should NOT run hooks.
	var hookCalls atomic.Int32
	runner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		hookCalls.Add(1)
		return &hookcore.CompliancePipelineResult{Decision: hookcore.RejectHard, Reason: "would have blocked"}
	}

	// Fabricated enum value that's outside the Mode constant set.
	dispatchStreamMode(context.Background(), streampolicy.Mode("future_mode_xyz"), runStreamDeps{
		Deps:       &Deps{},
		HookRunner: runner,
		HookCtx:    &streaming.StreamHookContext{RequestID: "dispatch-unknown"},
		SSEReader:  strings.NewReader(body),
		Tee:        tee,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if hookCalls.Load() != 0 {
		t.Errorf("unknown-mode arm must fall back to passthrough (no hook calls); got %d invocations", hookCalls.Load())
	}
	// Body must replay verbatim — the "forbidden-content" string would
	// have been blocked under live/buffer mode but passes through here.
	if !strings.Contains(teeBuf.String(), "forbidden-content") {
		t.Errorf("unknown-mode passthrough must relay bytes verbatim; got %q", teeBuf.String())
	}
}

// TestDispatchStreamMode_ZeroValueMode is the boundary case where the
// caller passes Mode("") — e.g. a nil-Store path that defaults to a
// zero-value Mode. Should fall through the same passthrough arm.
func TestDispatchStreamMode_ZeroValueMode(t *testing.T) {
	body := "data: x\n\ndata: [DONE]\n\n"
	var teeBuf bytes.Buffer
	tee := &testWriter{Buffer: &teeBuf, header: http.Header{}}
	var hookCalls atomic.Int32
	runner := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		hookCalls.Add(1)
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}
	dispatchStreamMode(context.Background(), streampolicy.Mode(""), runStreamDeps{
		Deps:       &Deps{},
		HookRunner: runner,
		HookCtx:    &streaming.StreamHookContext{RequestID: "dispatch-zero"},
		SSEReader:  strings.NewReader(body),
		Tee:        tee,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if hookCalls.Load() != 0 {
		t.Errorf("zero-value Mode must fall back to passthrough; got %d hook calls", hookCalls.Load())
	}
}
