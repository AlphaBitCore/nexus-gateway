package ingress

import (
	"testing"

	"github.com/tidwall/gjson"
)

// Same requirement as the Anthropic ingress: the extension speaks only to a
// Gemini target, so the canonical level is what every other wire reads.
func TestGenerateContentRequestToCanonical_StatesTheReasoningAskInTheCanonicalVocabulary(t *testing.T) {
	const head = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]`

	for _, tc := range []struct {
		name       string
		cfg        string
		wantEffort string // "" = the field must be absent
	}{
		{"a zero budget turns reasoning off", `,"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}`, "none"},
		{"a small budget is a low effort", `,"generationConfig":{"thinkingConfig":{"thinkingBudget":1500}}`, "low"},
		{"a mid budget is a medium effort", `,"generationConfig":{"thinkingConfig":{"thinkingBudget":5000}}`, "medium"},
		{"a large budget is a high effort", `,"generationConfig":{"thinkingConfig":{"thinkingBudget":9000}}`, "high"},
		{"\"you decide\" is an ask with the amount left open", `,"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}`, "medium"},
		{"asking only to SEE the reasoning names no amount", `,"generationConfig":{"thinkingConfig":{"includeThoughts":true}}`, ""},
		{"no thinkingConfig asks nothing", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(head+tc.cfg+`}`), "m")
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			got := gjson.GetBytes(out, "reasoning_effort")
			if tc.wantEffort == "" {
				if got.Exists() {
					t.Fatalf("reasoning_effort = %q, want absent", got.String())
				}
				return
			}
			if got.String() != tc.wantEffort {
				t.Fatalf("reasoning_effort = %q, want %q", got.String(), tc.wantEffort)
			}
		})
	}
}

// "You decide" is the one case where a figure is chosen rather than read, so
// the caller's own -1 has to survive for a Gemini target — otherwise the
// translation would have replaced their instruction with our guess.
func TestGenerateContentRequestToCanonical_DynamicBudgetSurvivesForItsOwnWire(t *testing.T) {
	out, err := GenerateContentRequestToOpenAIChatCompletion(
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],`+
			`"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`), "m")
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if b := gjson.GetBytes(out, "nexus.ext.gemini.thinking_config.thinkingBudget").Int(); b != -1 {
		t.Fatalf("extension budget = %d, want -1 — a Gemini target must still be told to decide "+
			"for itself rather than handed the level we picked for everyone else", b)
	}
}
