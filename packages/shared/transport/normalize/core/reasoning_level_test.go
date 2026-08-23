package core

import "testing"

// The boundaries themselves, because they are the whole content of the
// function: every value on one side of a threshold must name a different level
// from the value on the other, or the mapping is not the one the callers were
// told about.
func TestEffortForBudget_EachBoundaryIsWhereItSays(t *testing.T) {
	for _, tc := range []struct {
		budget int
		want   string
	}{
		{-1, ""},   // not a small amount of reasoning; not an amount at all
		{0, ""},    // same
		{1, "low"}, // the smallest real quantity
		{1999, "low"},
		{2000, "medium"},
		{7999, "medium"},
		{8000, "high"},
		{1 << 20, "high"},
	} {
		if got := EffortForBudget(tc.budget); got != tc.want {
			t.Errorf("EffortForBudget(%d) = %q, want %q", tc.budget, got, tc.want)
		}
	}
}

// Anthropic's floor is 1024 tokens, which is the smallest budget a real caller
// on that wire can send. It has to land on a level, not on the empty string
// that means "said nothing" — otherwise the most common minimal ask would
// arrive at every other wire as silence.
func TestEffortForBudget_TheSmallestBudgetARealCallerSendsIsALevel(t *testing.T) {
	if got := EffortForBudget(1024); got != "low" {
		t.Fatalf("EffortForBudget(1024) = %q, want low", got)
	}
}
