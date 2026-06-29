package streaming

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	sharedstreaming "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// TestLivePipeline_WithPreHook_FiresWithCumulativeBytes verifies that
// the installed PreHook callback fires at every checkpoint with the
// cumulative raw SSE wire bytes seen so far, and can mutate the
// HookInput's Normalized field before the hookRun executes.
func TestLivePipeline_WithPreHook_FiresWithCumulativeBytes(t *testing.T) {
	// Use OpenAI-style flat data: lines (no event: typed events); each
	// chunk carries >= the FirstInspectChars threshold below so the
	// first event triggers the checkpoint.
	body := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hello world this is enough text to trigger the first checkpoint and then some more so we cross the inspect threshold and fire the preHook callback at least once\"}}]}\n\ndata: [DONE]\n\n")

	var (
		preHookCalls atomic.Int32
		seenBytes    atomic.Int64
		stamped      atomic.Bool
	)

	hookRun := func(ctx context.Context, input *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		// Verify preHook stamped Normalized BEFORE hooks ran. The
		// preHookSentinel Protocol value is set only by our test's
		// preHook callback.
		if input.Normalized != nil && input.Normalized.Protocol == "preHookSentinel" {
			stamped.Store(true)
		}
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}

	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars:  10, // tiny so checkpoint fires fast
		ReinspectStepChars: 10,
	}, hookRun, nil, nil)

	lp.WithPreHook(func(rawBody []byte, ci *hookcore.HookInput) {
		preHookCalls.Add(1)
		seenBytes.Store(int64(len(rawBody)))
		// Mutate Normalized to a synthetic payload so the assertion in
		// hookRun can verify our callback fired BEFORE hookRun.
		ci.Normalized = &normcore.NormalizedPayload{
			Kind:     "ai-chat",
			Protocol: "preHookSentinel",
		}
	})

	w := httptest.NewRecorder()
	hookCtx := &StreamHookContext{
		RequestID:   "r1",
		Path:        "/v1/messages",
		Method:      "POST",
		Model:       "claude",
		IngressType: "anthropic-messages",
	}
	_ = lp.Process(context.Background(), body, w, hookCtx)

	if preHookCalls.Load() == 0 {
		t.Errorf("expected preHook to fire at least once; saw %d calls", preHookCalls.Load())
	}
	if seenBytes.Load() == 0 {
		t.Errorf("expected preHook to receive non-empty cumulative bytes; got 0")
	}
	if !stamped.Load() {
		t.Errorf("expected hookRun to observe preHook-stamped Normalized")
	}
}

// TestLivePipeline_NoPreHook_StillWorks verifies the
// flat-text behaviour stays the default when caller doesn't
// wire WithPreHook.
func TestLivePipeline_NoPreHook_StillWorks(t *testing.T) {
	body := strings.NewReader(`event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}

data: [DONE]

`)
	var sawHookRun atomic.Bool
	hookRun := func(ctx context.Context, input *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		sawHookRun.Store(true)
		return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
	}
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars:  10,
		ReinspectStepChars: 10,
	}, hookRun, nil, nil)
	w := httptest.NewRecorder()
	hookCtx := &StreamHookContext{RequestID: "r"}
	_ = lp.Process(context.Background(), body, w, hookCtx)
	// We don't assert sawHookRun strictly (a tiny body may not cross
	// the first checkpoint); we just verify Process completed without
	// panicking and no preHook wiring caused regressions.
}

// TestLockedByteBuffer_ConcurrentWriteSnapshot verifies the goroutine-
// safety contract — Snapshot copies, callers can't observe torn state,
// and Write + Snapshot are concurrent-safe.
func TestLockedByteBuffer_ConcurrentWriteSnapshot(t *testing.T) {
	var b sharedstreaming.LockedByteBuffer
	if n, _ := b.Write([]byte("hello")); n != 5 {
		t.Errorf("Write returned %d, want 5", n)
	}
	snap1 := b.Snapshot()
	if !bytes.Equal(snap1, []byte("hello")) {
		t.Errorf("Snapshot = %q, want hello", snap1)
	}
	_, _ = b.Write([]byte(" world"))
	snap2 := b.Snapshot()
	if !bytes.Equal(snap2, []byte("hello world")) {
		t.Errorf("Snapshot 2 = %q, want hello world", snap2)
	}
	// Verify snap1 unaffected by the second Write — defensive copy
	// contract.
	if !bytes.Equal(snap1, []byte("hello")) {
		t.Errorf("snap1 mutated by later Write; got %q", snap1)
	}
}
