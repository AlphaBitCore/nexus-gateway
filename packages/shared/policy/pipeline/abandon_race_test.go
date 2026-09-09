package pipeline

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// slowModifyHook overruns its deadline by exactly its deadline, then returns a
// Modify — the shape that makes the chain goroutine WRITE input.Normalized at
// the same moment the abandon path is reading it.
type slowModifyHook struct {
	approveEverythingHook
	delay time.Duration
}

func (h *slowModifyHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	time.Sleep(h.delay)
	return &core.HookResult{
		Decision: core.Modify,
		ModifiedContent: []core.ContentBlock{
			{Role: "user", Type: "text", Text: "[REDACTED]"},
		},
	}, nil
}

// The abandon path runs on the CALLER's goroutine while the abandoned chain is
// still running on its own, and the chain owns `input`. Any read of
// input.Normalized from the abandon path is therefore a data race — and the
// field it read, Kind, is a two-word string header, so a torn read is a bogus
// comparison at best and a segfault at worst.
//
// The fix is to snapshot the kind before the chain starts. This test is what
// says the snapshot is still there: it drives the exact interleaving (a hook
// whose runtime equals its budget, so the timer and the return land together)
// and must be run with -race, which the package's CI invocation does.
//
// Reverting hookAppliesToKind to read through the input reports four races here.
func TestAbandonedChainDoesNotRaceTheAbandonPath(t *testing.T) {
	reg := core.NewHookRegistry()
	reg.Register("slow-modify", func(_ *core.HookConfig) (core.Hook, error) {
		return &slowModifyHook{delay: 15 * time.Millisecond}, nil
	})

	cfg := func(id string) core.HookConfig {
		return core.HookConfig{
			ID: id, Name: id, ImplementationID: "slow-modify",
			Stage: "request", Enabled: true, ApplicableIngress: []string{"ALL"},
			FailBehavior: "fail-open",
			// The budget equals the runtime, so the abandon timer fires within a
			// scheduling quantum of the hook returning. That coincidence is the
			// whole window.
			TimeoutMs: 15,
		}
	}
	r := NewPolicyResolver([]core.HookConfig{cfg("h1"), cfg("h2")}, reg, slog.New(slog.DiscardHandler))

	for range 60 {
		p, _, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil,
			15*time.Millisecond, time.Second, false, false, slog.New(slog.DiscardHandler))
		if err != nil || p == nil {
			t.Fatalf("BuildPipeline: %v", err)
		}
		// A non-nil payload is required: with nil Normalized the predicate
		// short-circuits and never reads through the pointer, so the race would
		// not be reachable and this test would pass by testing nothing.
		in := &core.HookInput{
			Stage: "request",
			Normalized: &normalize.NormalizedPayload{
				Kind:     normalize.KindAIChat,
				Messages: []normalize.Message{{Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "hi"}}}},
			},
		}
		if res := p.Execute(context.Background(), in); res == nil {
			t.Fatal("Execute returned no result")
		}
	}
}
