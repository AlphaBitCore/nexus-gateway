package modela

import (
	"context"
	"log/slog"
	"testing"
)

// countingHandler records how many log records at each level were emitted.
type countingHandler struct {
	warns int
	last  map[string]any
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *countingHandler) WithGroup(string) slog.Handler            { return h }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.warns++
		h.last = map[string]any{}
		r.Attrs(func(a slog.Attr) bool { h.last[a.Key] = a.Value.Any(); return true })
	}
	return nil
}

func TestWarnStreamingCoverageGap(t *testing.T) {
	// Each run picks its own generation, so this test never collides with another
	// test's state and `-count=2` means what it says without reaching into the
	// dedupe. The dedupe is scoped to a rule-set generation, which is exactly the
	// lifetime a rule-set-derived warning should have.
	const gen = 4242

	h := &countingHandler{}
	logger := slog.New(h)

	// Below the window: no warning (the normal case).
	WarnStreamingCoverageGap(logger, gen, 100, DefaultTailWindowBytes)
	if h.warns != 0 {
		t.Fatalf("below-window must not warn, got %d", h.warns)
	}

	// At/above the window, first sight: exactly one warning carrying the sizes.
	const big = 987654 // unique, unlikely to collide with other tests' keys
	WarnStreamingCoverageGap(logger, gen, big, DefaultTailWindowBytes)
	if h.warns != 1 {
		t.Fatalf("at/above-window first sight must warn once, got %d", h.warns)
	}
	if h.last["maxPatternBytes"] != int64(big) {
		t.Errorf("warn maxPatternBytes = %v, want %d", h.last["maxPatternBytes"], big)
	}
	if h.last["tailWindowBytes"] != int64(DefaultTailWindowBytes) {
		t.Errorf("warn tailWindowBytes = %v, want %d", h.last["tailWindowBytes"], DefaultTailWindowBytes)
	}
	if h.last["ruleSetGeneration"] != uint64(gen) {
		t.Errorf("warn ruleSetGeneration = %v, want %d — an operator reading the log has to be "+
			"able to tell which rule set the gap belongs to", h.last["ruleSetGeneration"], gen)
	}

	// Same bound, same generation: deduped, no second warning. This is what keeps
	// a busy stream from logging on every setup.
	WarnStreamingCoverageGap(logger, gen, big, DefaultTailWindowBytes)
	if h.warns != 1 {
		t.Fatalf("repeated same-bound call within one generation must be deduped, got %d warns", h.warns)
	}

	// Nil logger must not panic.
	WarnStreamingCoverageGap(nil, gen, big+1, DefaultTailWindowBytes)
}

// TestWarnStreamingCoverageGapReopensWithANewRuleSet is the defect the old dedupe
// had: its key was the derived bound alone with no expiry, so the warning spoke
// once per bound per PROCESS. An operator who is warned about a 7362-byte bound,
// narrows the rule so the gap closes, and later re-adds the long pattern gets a
// reopened gap and permanent silence — the log says the problem was fixed while
// it is back.
//
// The bound is the same value in both halves on purpose. That is precisely the
// case the old key could not tell apart.
func TestWarnStreamingCoverageGapReopensWithANewRuleSet(t *testing.T) {
	h := &countingHandler{}
	logger := slog.New(h)

	const bound = 7362 // the bound the shipped rule pack derives

	// Generation 1: the admin's rule set leaves a gap. Warn once, dedupe after.
	WarnStreamingCoverageGap(logger, 1, bound, DefaultTailWindowBytes)
	WarnStreamingCoverageGap(logger, 1, bound, DefaultTailWindowBytes)
	if h.warns != 1 {
		t.Fatalf("generation 1 must warn exactly once, got %d", h.warns)
	}

	// Generation 2: the admin narrows the rule. Bound drops below the window, no
	// warning — nothing is wrong, and nothing should be said.
	WarnStreamingCoverageGap(logger, 2, 100, DefaultTailWindowBytes)
	if h.warns != 1 {
		t.Fatalf("a generation with no gap must stay silent, got %d warns", h.warns)
	}

	// Generation 3: the long pattern is back and so is the gap. The operator must
	// hear about it again.
	WarnStreamingCoverageGap(logger, 3, bound, DefaultTailWindowBytes)
	if h.warns != 2 {
		t.Fatalf("a reopened gap under a NEW rule set must warn again, got %d warns — this is the "+
			"process-lifetime dedupe swallowing a live compliance gap because the same bound had "+
			"been seen before", h.warns)
	}
	if h.last["ruleSetGeneration"] != uint64(3) {
		t.Errorf("the second warning must name the generation that reopened the gap, got %v",
			h.last["ruleSetGeneration"])
	}

	// And it dedupes within that new generation like any other.
	WarnStreamingCoverageGap(logger, 3, bound, DefaultTailWindowBytes)
	if h.warns != 2 {
		t.Fatalf("the new generation must dedupe too, got %d warns", h.warns)
	}
}
