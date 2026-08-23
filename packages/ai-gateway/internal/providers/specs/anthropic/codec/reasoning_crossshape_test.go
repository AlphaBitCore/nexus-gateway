package codec

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// encodeWithEffort runs the canonical (OpenAI-shaped) body an OpenAI-ingress
// caller produces — a reasoning_effort LEVEL — through this wire's codec, which
// takes a BUDGET.
func encodeWithEffort(t *testing.T, effort string, maxTokens, modelLimit int) (gjson.Result, []string, error) {
	t.Helper()
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":` +
		itoa(maxTokens) + `,"reasoning_effort":"` + effort + `"}`
	res, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, []byte(body),
		provcore.CallTarget{ProviderModelID: "claude-haiku-4-5", MaxOutputTokens: modelLimit})
	if err != nil {
		return gjson.Result{}, nil, err
	}
	return gjson.ParseBytes(res.Body), res.Rewrites, nil
}

// TestReasoningCrossShape_ALevelBecomesABudget.
//
// The canonical carries a LEVEL because canonical is the OpenAI shape; this
// wire takes a BUDGET. Without the translation an OpenAI-ingress caller routed
// to a Claude model has their intent silently dropped — they asked for a
// reasoned answer, got an ordinary one, and the traffic row looks exactly like
// a request that was served properly.
func TestReasoningCrossShape_ALevelBecomesABudget(t *testing.T) {
	for _, tc := range []struct {
		effort string
		want   int64
	}{
		// The floor, which is the only budget this wire is documented to
		// accept at the bottom of its range.
		{"low", 1024},
		{"minimal", 1024},
		// Midway between the floor and what the cap can house.
		{"medium", 1024 + (4095-1024)/2},
		// Everything the cap can house, one below max_tokens.
		{"high", 4095},
		{"max", 4095},
	} {
		t.Run(tc.effort, func(t *testing.T) {
			root, rewrites, err := encodeWithEffort(t, tc.effort, 4096, 8192)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got := root.Get("thinking.budget_tokens").Int(); got != tc.want {
				t.Errorf("budget = %d, want %d — the caller's level was not carried onto "+
					"this wire's own shape", got, tc.want)
			}
			if root.Get("thinking.type").String() != "enabled" {
				t.Errorf("thinking.type = %q, want enabled", root.Get("thinking.type").String())
			}
			// Every derived value is a field we own, so it self-reports.
			var said bool
			for _, r := range rewrites {
				if strings.Contains(r, "reasoning_effort→thinking.budget_tokens") {
					said = true
				}
			}
			if !said {
				t.Errorf("the translation is not in the coerced list %v — a number we chose "+
					"for the caller has to be visible to them and on the traffic row", rewrites)
			}
		})
	}
}

// "none" is the caller asking NOT to think. This wire spells that by omitting
// the block; sending a zero budget would be a different request.
func TestReasoningCrossShape_NoneSendsNoThinkingBlock(t *testing.T) {
	root, _, err := encodeWithEffort(t, "none", 4096, 8192)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if root.Get("thinking").Exists() {
		t.Errorf("a thinking block was sent for reasoning_effort=none: %s", root.Get("thinking").Raw)
	}
}

// A level this wire's vocabulary does not contain produces nothing. Guessing
// which end of the range an unknown word means is how a translation starts
// answering questions nobody asked.
func TestReasoningCrossShape_AnUnknownLevelIsNotGuessed(t *testing.T) {
	root, _, err := encodeWithEffort(t, "turbo", 4096, 8192)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if root.Get("thinking").Exists() {
		t.Errorf("an unrecognised effort level produced a budget: %s", root.Get("thinking").Raw)
	}
}

// A cap that cannot house the floor forwards WITHOUT thinking rather than
// refusing. The caller expressed a preference, not a requirement, and a
// refusal here takes away an answer they can still use — the eligibility
// filter upstream is what keeps a request that NEEDS reasoning off a model
// that cannot serve it.
func TestReasoningCrossShape_ACapTooSmallForTheFloorForwardsPlainly(t *testing.T) {
	root, _, err := encodeWithEffort(t, "high", 512, 8192)
	if err != nil {
		t.Fatalf("encode: %v — a cap too small for thinking must not refuse the request", err)
	}
	if root.Get("thinking").Exists() {
		t.Errorf("a budget was sent under a cap that cannot house the floor: %s",
			root.Get("thinking").Raw)
	}
}

// The native path wins. A caller who spoke Anthropic sent the exact block they
// wanted, and a level we translated on top of it would replace their number
// with ours.
func TestReasoningCrossShape_TheNativeBlockIsNotOverridden(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":4096,` +
		`"reasoning_effort":"high",` +
		`"nexus":{"ext":{"anthropic":{"thinking":{"type":"enabled","budget_tokens":1500}}}}}`
	res, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, []byte(body),
		provcore.CallTarget{ProviderModelID: "claude-haiku-4-5", MaxOutputTokens: 8192})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.ParseBytes(res.Body).Get("thinking.budget_tokens").Int(); got != 1500 {
		t.Errorf("budget = %d, want the caller's own 1500 — a native block must not be "+
			"replaced by a number we derived from a level they also happened to send", got)
	}
}
