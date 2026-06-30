package streaming

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// TestFoldHookResults_CollapsesPerCheckpointDuplicates reproduces the observed
// chunked_async "RESPONSE PIPELINE (63)" duplication: the same hook scanned at 63
// checkpoints must fold to ONE result with summed microsecond latency and the
// latest decision — not 63 identical rows.
func TestFoldHookResults_CollapsesPerCheckpointDuplicates(t *testing.T) {
	var acc []core.HookResult
	for range 63 {
		acc = append(acc, core.HookResult{HookID: "h1", HookName: "response-quality-signals", Decision: core.Approve, LatencyUs: 100})
	}
	got := foldHookResults(acc)
	if len(got) != 1 {
		t.Fatalf("63 checkpoint scans must fold to 1 row, got %d", len(got))
	}
	if got[0].LatencyUs != 6300 {
		t.Fatalf("summed LatencyUs=%d want 6300", got[0].LatencyUs)
	}
}

// TestFoldHookResults_LatestWinsAndSums verifies summed latency + latest decision,
// and that distinct hooks stay separate in first-seen order.
func TestFoldHookResults_LatestWinsAndSums(t *testing.T) {
	in := []core.HookResult{
		{HookID: "a", Decision: core.Approve, LatencyMs: 0, LatencyUs: 200},
		{HookID: "b", Decision: core.Approve, LatencyUs: 50},
		{HookID: "a", Decision: core.RejectHard, Reason: "blocked", LatencyMs: 1, LatencyUs: 1100},
	}
	got := foldHookResults(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 hooks, got %d", len(got))
	}
	if got[0].HookID != "a" || got[1].HookID != "b" {
		t.Fatalf("first-seen order not preserved: %+v", got)
	}
	if got[0].LatencyUs != 1300 || got[0].LatencyMs != 1 {
		t.Fatalf("hook a latency not summed: %+v", got[0])
	}
	if got[0].Decision != core.RejectHard || got[0].Reason != "blocked" {
		t.Fatalf("hook a latest decision not kept: %+v", got[0])
	}
}

// TestFoldHookResults_PassThroughSmall confirms the <=1 fast path returns input
// unchanged (no allocation for the common single-hook / empty case).
func TestFoldHookResults_PassThroughSmall(t *testing.T) {
	if got := foldHookResults(nil); got != nil {
		t.Fatalf("nil → %v", got)
	}
	one := []core.HookResult{{HookID: "x", LatencyUs: 9}}
	got := foldHookResults(one)
	if len(got) != 1 || got[0].HookID != "x" {
		t.Fatalf("single passthrough altered: %+v", got)
	}
}
