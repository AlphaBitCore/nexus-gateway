package canonicalbridge

import (
	"log/slog"
	"testing"

	"github.com/tidwall/gjson"

	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// What a caller asked for has to reach whichever wire serves them.
//
// Routing is free to send an Anthropic-ingress request to an OpenAI model and a
// Gemini-ingress request to an Anthropic one; the caller neither chose nor sees
// that. So "I want extended thinking" — however they spelled it — must arrive,
// and "I do NOT want it" must arrive too, because that one costs money when it
// goes missing.
//
// Asserted on the WIRE body, which is what the upstream actually receives.
// Asserting on the canonical body would pass while a codec dropped the field on
// the last leg.
func TestReasoningIntentReachesEveryWire(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(slog.Default()))

	const anthropicBudget = `{"model":"claude-x","max_tokens":4096,` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"enabled","budget_tokens":8000}}`
	const anthropicOff = `{"model":"claude-x","max_tokens":4096,` +
		`"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`
	const geminiBudget = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"thinkingConfig":{"thinkingBudget":8000}}}`

	for _, tc := range []struct {
		name    string
		ingress provcore.Format
		target  provcore.Format
		body    string
		// check receives the wire body and says what is wrong, or "".
		check func(wire []byte) string
	}{
		{
			name: "anthropic budget → openai wire", ingress: provcore.FormatAnthropic,
			target: provcore.FormatOpenAI, body: anthropicBudget,
			check: func(w []byte) string {
				if e := gjson.GetBytes(w, "reasoning_effort").String(); e != "high" {
					return "reasoning_effort = " + e + ", want high"
				}
				return ""
			},
		},
		{
			name: "anthropic budget → gemini wire", ingress: provcore.FormatAnthropic,
			target: provcore.FormatGemini, body: anthropicBudget,
			check: func(w []byte) string {
				if !gjson.GetBytes(w, "generationConfig.thinkingConfig").Exists() {
					return "no thinkingConfig on the Gemini wire"
				}
				return ""
			},
		},
		{
			name: "anthropic OFF → openai wire", ingress: provcore.FormatAnthropic,
			target: provcore.FormatOpenAI, body: anthropicOff,
			check: func(w []byte) string {
				if e := gjson.GetBytes(w, "reasoning_effort").String(); e != "none" {
					return "reasoning_effort = " + e + `, want "none" — the caller turned ` +
						"reasoning off and would otherwise pay for a model that reasons anyway"
				}
				return ""
			},
		},
		{
			name: "gemini budget → openai wire", ingress: provcore.FormatGemini,
			target: provcore.FormatOpenAI, body: geminiBudget,
			check: func(w []byte) string {
				if e := gjson.GetBytes(w, "reasoning_effort").String(); e != "high" {
					return "reasoning_effort = " + e + ", want high"
				}
				return ""
			},
		},
		{
			name: "gemini budget → anthropic wire", ingress: provcore.FormatGemini,
			target: provcore.FormatAnthropic, body: geminiBudget,
			check: func(w []byte) string {
				if !gjson.GetBytes(w, "thinking").Exists() {
					return "no thinking block on the Anthropic wire"
				}
				return ""
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, _, err := b.IngressChatToWire(tc.ingress, tc.target, []byte(tc.body),
				provcore.CallTarget{ProviderModelID: "m", MaxOutputTokens: 4096}, false)
			if err != nil {
				t.Fatalf("to wire: %v", err)
			}
			if msg := tc.check(wire); msg != "" {
				t.Fatalf("%s\nwire body: %s", msg, wire)
			}
		})
	}
}

// The same-wire leg is the control: it must keep the caller's own figure, so a
// failure above is read as "the translation lost it" rather than "the request
// never carried it".
func TestReasoningIntentIsUntouchedOnItsOwnWire(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(slog.Default()))
	const body = `{"model":"claude-x","max_tokens":4096,` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"enabled","budget_tokens":5000}}`

	wire, _, err := b.IngressChatToWire(provcore.FormatAnthropic, provcore.FormatAnthropic,
		[]byte(body), provcore.CallTarget{ProviderModelID: "m", MaxOutputTokens: 4096}, false)
	if err != nil {
		t.Fatalf("to wire: %v", err)
	}
	if got := gjson.GetBytes(wire, "thinking.budget_tokens").Int(); got != 5000 {
		t.Fatalf("budget_tokens = %d, want the caller's own 5000", got)
	}
}
