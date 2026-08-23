package core

import "testing"

// TestReasoning_AskedIsNotANilCheck.
//
// The routing constraint narrows a candidate pool only for a request that ASKED
// to reason, and it reads this. An allocated struct with nothing in it is not an
// ask — a decoder that builds one before finding anything to put in it would
// otherwise make every request look like it asked, and the pool would be
// narrowed to reasoning models for callers who never mentioned it.
//
// Asserted on the type rather than through a decoder, because today's three
// decoders happen to allocate only when they have a value: the property would
// hold there for a reason that has nothing to do with this function, and the
// next decoder need not be so careful.
func TestReasoning_AskedIsNotANilCheck(t *testing.T) {
	budget := 1024
	include := true

	for _, tc := range []struct {
		name string
		r    *Reasoning
		want bool
	}{
		{"nil", nil, false},
		{"allocated but empty", &Reasoning{}, false},
		{"a level", &Reasoning{Effort: "high"}, true},
		{"a budget", &Reasoning{BudgetTokens: &budget}, true},
		{"a dynamic budget", &Reasoning{BudgetTokens: intPtrReasoning(-1)}, true},
		{"visibility only", &Reasoning{IncludeThoughts: &include}, true},
		{"an explicit refusal to reason", &Reasoning{Effort: "none"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.Asked(); got != tc.want {
				t.Fatalf("Asked() = %v, want %v — %s", got, tc.want, map[bool]string{
					true:  "this is an expressed intent and a codec has to carry it",
					false: "this is silence, and treating it as an ask narrows the pool for callers who said nothing",
				}[tc.want])
			}
		})
	}
}

func intPtrReasoning(v int) *int { return &v }
