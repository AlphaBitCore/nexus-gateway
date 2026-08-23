package core

import (
	"slices"
	"testing"
)

// TestProviderIDs_IsTheOnePlaceThatAnswers.
//
// Two conditions ask "which providers serve what the caller named": the
// structured `providers` list and an expression on `requestedModel.providerId`.
// They must read one answer. The singular field is populated only when exactly
// one provider serves the code, so a caller consulting it directly sees nothing
// for the ordinary case of one open-weights model on two hosts — which is how
// the two conditions came to disagree.
func TestProviderIDs_IsTheOnePlaceThatAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		rm   RequestedModel
		want []string
		why  string
	}{
		{
			name: "several hosts",
			rm: RequestedModel{
				ID: "llama-3.3-70b", CandidateProviderIDs: []string{"groq", "together"},
			},
			want: []string{"groq", "together"},
			why:  "both hosts serve the code, so a rule scoped to either one applies",
		},
		{
			name: "one host, only the singular field set",
			rm:   RequestedModel{ID: "gpt-4o", ProviderID: "openai"},
			want: []string{"openai"},
			why: "a caller that set the singular field by hand still gets an answer; " +
				"otherwise the condition silently becomes inapplicable",
		},
		{
			name: "nothing named",
			rm:   RequestedModel{ID: "auto"},
			want: nil,
			why:  "no answer, which conditions read as INAPPLICABLE rather than false",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rm.ProviderIDs(); !slices.Equal(got, tc.want) {
				t.Fatalf("ProviderIDs() = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestProviderIsUnknowable_SeparatesNoAnswerFromNoLookup.
//
// Empty candidate fields mean two different things and only one of them makes a
// provider condition inapplicable. "Named nothing" is an ANSWER — an `auto`
// request is what a provider-scoped rule is written for. "Named something we
// could not look up" is not, and reading it as one meant a catalogue read
// failure widened every provider-scoped rule to every provider.
func TestProviderIsUnknowable_SeparatesNoAnswerFromNoLookup(t *testing.T) {
	for _, tc := range []struct {
		name string
		rm   RequestedModel
		want bool
		why  string
	}{
		{
			name: "named, lookup failed",
			rm:   RequestedModel{ID: "gpt-4o", HydrationFailed: true},
			want: true,
			why:  "the condition cannot be evaluated, so it has not passed",
		},
		{
			name: "named nothing",
			rm:   RequestedModel{ID: "auto"},
			want: false,
			why: "`auto` names no model, which is an answer; calling it unknowable makes " +
				"provider-scoped rules refuse exactly the requests they exist for",
		},
		{
			name: "lookup failed but candidates arrived anyway",
			rm: RequestedModel{
				ID: "gpt-4o", HydrationFailed: true, CandidateProviderIDs: []string{"openai"},
			},
			want: false,
			why:  "we know the answer; how we came by it does not matter to the condition",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rm.ProviderIsUnknowable(); got != tc.want {
				t.Fatalf("ProviderIsUnknowable() = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}
