package codec

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func encodeWithEffort(t *testing.T, effort string) (gjson.Result, []string, error) {
	t.Helper()
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + effort + `"}`
	res, err := Codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, []byte(body),
		provcore.CallTarget{ProviderModelID: "gemini-2.5-pro"})
	if err != nil {
		return gjson.Result{}, nil, err
	}
	return gjson.ParseBytes(res.Body), res.Rewrites, nil
}

// TestGeminiReasoningCrossShape_ALevelBecomesDynamic.
//
// The canonical carries a LEVEL; this wire takes a BUDGET. Unlike Anthropic,
// no probed minimum or maximum budget exists anywhere in this repo for Gemini,
// so a level-to-number mapping would be an invented range — and an invented
// range is an upstream 400 wearing a feature's clothes.
//
// -1 is this wire's own way of saying "you decide", so it carries the intent
// (reason) without asserting an amount nobody has measured.
func TestGeminiReasoningCrossShape_ALevelBecomesDynamic(t *testing.T) {
	for _, effort := range []string{"minimal", "low", "medium", "high", "max"} {
		t.Run(effort, func(t *testing.T) {
			root, rewrites, err := encodeWithEffort(t, effort)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got := root.Get("generationConfig.thinkingConfig.thinkingBudget")
			if !got.Exists() || got.Int() != -1 {
				t.Errorf("thinkingBudget = %v, want -1 — the caller asked to reason and this "+
					"wire was sent no budget at all, so the answer arrives without the "+
					"reasoning they asked for", got.Raw)
			}
			var said bool
			for _, r := range rewrites {
				if strings.Contains(r, "reasoning_effort→thinkingConfig") {
					said = true
				}
			}
			if !said {
				t.Errorf("the translation is absent from the coerced list %v — the gradation "+
					"between low and high is genuinely lost here, so the caller has to be "+
					"able to see that we chose for them", rewrites)
			}
		})
	}
}

// "none" and any unrecognised level produce nothing. This wire's spelling for
// a disable is not established in this repo — 0 is a guess, not a probed fact —
// and declining leaves the request exactly as it was rather than asserting
// something unproven.
func TestGeminiReasoningCrossShape_NoneAndUnknownAreNotGuessed(t *testing.T) {
	for _, effort := range []string{"none", "turbo", ""} {
		t.Run(effort, func(t *testing.T) {
			root, _, err := encodeWithEffort(t, effort)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got := root.Get("generationConfig.thinkingConfig"); got.Exists() {
				t.Errorf("a thinkingConfig was sent for reasoning_effort=%q: %s", effort, got.Raw)
			}
		})
	}
}

// The native config wins: a caller who spoke Gemini sent the exact budget they
// wanted, and replacing it with our -1 would throw away a number they chose.
func TestGeminiReasoningCrossShape_TheNativeConfigIsNotOverridden(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high",` +
		`"nexus":{"ext":{"gemini":{"thinking_config":{"thinkingBudget":2048,"includeThoughts":true}}}}}`
	res, err := Codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, []byte(body),
		provcore.CallTarget{ProviderModelID: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	root := gjson.ParseBytes(res.Body)
	if got := root.Get("generationConfig.thinkingConfig.thinkingBudget").Int(); got != 2048 {
		t.Errorf("thinkingBudget = %d, want the caller's own 2048", got)
	}
	if !root.Get("generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Errorf("includeThoughts was dropped: %s", root.Get("generationConfig.thinkingConfig").Raw)
	}
}
