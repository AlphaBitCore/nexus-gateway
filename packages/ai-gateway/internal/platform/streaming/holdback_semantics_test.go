package streaming

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// TestLivePipeline_HoldBackFalse_StillRunsHooksAndPreHook
// pins the contract that the LivePipeline default behaviour
// when HoldBack=false is NOT equivalent to passthrough.
//
// passthrough = wire bytes copied through, NO compliance pipeline,
//
//	NO audit normalize stamp, NO hooks
//
// HoldBack=false = wire bytes copied through IMMEDIATELY (no
//
//	client-side delay), but checkpoints still fire and the hook
//	executor + preHook callback still see the cumulative bytes
//
// This distinction matters because an admin who sets HoldBack=false
// hoping to "skip compliance" actually keeps the full audit + hook
// pipeline — only the client-side delay is removed. The test fails
// the day someone shortcuts HoldBack=false to a passthrough fast-path,
// silently dropping audit + hook coverage for that mode.
func TestLivePipeline_HoldBackFalse_StillRunsHooksAndPreHook(t *testing.T) {
	body := strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"plenty of text to cross the FirstInspectChars threshold and trigger the checkpoint at least once\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)

	var (
		hookRunCalls atomic.Int32
		preHookCalls atomic.Int32
	)
	hookRun := func(_ context.Context, _ *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		hookRunCalls.Add(1)
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}

	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars:  10,
		ReinspectStepChars: 10,
		HoldBack:           false, // key under test
	}, hookRun, nil, nil)

	lp.WithPreHook(func(_ []byte, _ *hookcore.HookInput) {
		preHookCalls.Add(1)
	})

	w := httptest.NewRecorder()
	hookCtx := &StreamHookContext{
		RequestID:   "r1",
		Path:        "/v1/chat/completions",
		Method:      "POST",
		Model:       "x",
		IngressType: "openai-chat",
	}
	_ = lp.Process(context.Background(), body, w, hookCtx)

	// Both must fire even with HoldBack=false — the hold-back flag
	// only controls whether deltas are buffered before the first
	// checkpoint, NOT whether the checkpoint/hook pipeline runs.
	if hookRunCalls.Load() == 0 {
		t.Errorf("HoldBack=false bypassed the hook executor; hookRun fired %d times, want >=1", hookRunCalls.Load())
	}
	if preHookCalls.Load() == 0 {
		t.Errorf("HoldBack=false bypassed the preHook callback; fired %d times, want >=1", preHookCalls.Load())
	}

	// Client SHOULD see content (the hold-back flag was off so deltas
	// flow immediately on the released branch).
	if w.Body.Len() == 0 {
		t.Errorf("HoldBack=false produced no client output — should write immediately")
	}
}
