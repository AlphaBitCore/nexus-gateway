package proxy

import (
	"net/http"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// TestWriteOverloaded_KeepsItsCodeInEveryDialect pins the refusal the envelope
// sweep missed.
//
// writeOverloaded kept the old "branch on IsOpenAIFamily" decision after every
// other writer moved off it, and it did something the others did not: on the
// non-family branch it swapped the machine code, sending provcore.CodeRateLimited
// where the OpenAI branch sends GATEWAY_OVERLOADED. /v1/rerank is mounted with
// BodyFormat cohere, so a caller shed by the concurrency cap there received
// lower_snake "rate_limited" on a surface whose contract is UPPER_SNAKE, and a
// client branching on GATEWAY_OVERLOADED never saw it.
//
// The substitution was justified by a comment claiming CodeRateLimited maps to
// each envelope's semantically-correct type. Measured, it does not buy that:
// both codes produce anthropic rate_limit_error and gemini RESOURCE_EXHAUSTED,
// because those mappers derive the type from the STATUS. So the swap changed
// nothing a caller sees except the one field that identifies the refusal.
func TestWriteOverloaded_KeepsItsCodeInEveryDialect(t *testing.T) {
	t.Run("an ingress with no dialect of its own keeps the code and the shape", func(t *testing.T) {
		for _, f := range []provcore.Format{
			provcore.FormatOpenAI, provcore.FormatCohere, provcore.FormatBedrock,
			provcore.FormatReplicate, provcore.FormatVoyage, provcore.Format(""),
		} {
			w := &testResponseWriter{}
			writeOverloaded(w, f)

			if got := gjson.GetBytes(w.body, "error.code").String(); got != "GATEWAY_OVERLOADED" {
				t.Errorf("%q ingress: code = %q, want GATEWAY_OVERLOADED — the refusal must be "+
					"identifiable by the same code on every route (%s)", f, got, w.body)
			}
			if gjson.GetBytes(w.body, "error.param").Exists() {
				t.Errorf("%q ingress took the upstream encoder: %s", f, w.body)
			}
			if got := gjson.GetBytes(w.body, "error.type").String(); got != "rate_limit_error" {
				t.Errorf("%q ingress: type = %q, want rate_limit_error", f, got)
			}
			if w.status != http.StatusTooManyRequests {
				t.Errorf("%q ingress: status = %d, want 429", f, w.status)
			}
			if ra := w.Header().Get("Retry-After"); ra == "" {
				t.Errorf("%q ingress: a 429 must say when to come back", f)
			}
		}
	})

	t.Run("a dialect of its own keeps ITS shape, and the same type", func(t *testing.T) {
		w := &testResponseWriter{}
		writeOverloaded(w, provcore.FormatAnthropic)
		if gjson.GetBytes(w.body, "type").String() != "error" {
			t.Errorf("anthropic lost its envelope: %s", w.body)
		}
		if got := gjson.GetBytes(w.body, "error.type").String(); got != "rate_limit_error" {
			t.Errorf("anthropic type = %q, want rate_limit_error (%s)", got, w.body)
		}

		w = &testResponseWriter{}
		writeOverloaded(w, provcore.FormatGemini)
		if got := gjson.GetBytes(w.body, "error.status").String(); got != "RESOURCE_EXHAUSTED" {
			t.Errorf("gemini status = %q, want RESOURCE_EXHAUSTED (%s)", got, w.body)
		}
	})
}
