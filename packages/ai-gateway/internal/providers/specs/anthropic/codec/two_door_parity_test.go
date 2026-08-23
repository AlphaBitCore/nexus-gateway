package codec

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// TestTwoDoorParity_OurClampNeverStrandsTheBudgetOnEitherDoor.
//
// Both doors clamp max_tokens with the same policy, so both can strand a
// caller's thinking budget above the cap they just lowered — and Anthropic then
// answers 400 "max_tokens must be greater than thinking.budget_tokens",
// describing a request the caller never sent.
//
// The cross-format door reconciled it from the day the rule was written. The
// NATIVE door did not, and nothing noticed because each door was tested against
// itself. A rule that holds on one door and not the other is the incident class
// this parity requirement exists for; the historical one was a Responses wire
// with no strip while the identical body answered 200 on chat.
//
// Asserted as an equality between the doors rather than as two separate
// expectations, so neither can be "fixed" by moving the number it produces.
func TestTwoDoorParity_OurClampNeverStrandsTheBudgetOnEitherDoor(t *testing.T) {
	const modelCeiling = 2048

	// The caller's pair is consistent: 4096 < 8192. Our clamp is what breaks it.
	native := []byte(`{"model":"claude-haiku-4-5","max_tokens":8192,` +
		`"thinking":{"type":"enabled","budget_tokens":4096},` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	canonical := []byte(`{"model":"claude-haiku-4-5","max_tokens":8192,` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"nexus":{"ext":{"anthropic":{"thinking":{"type":"enabled","budget_tokens":4096}}}}}`)

	target := provcore.CallTarget{ProviderModelID: "claude-haiku-4-5", MaxOutputTokens: modelCeiling}

	nativeRes, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages, native, target, false)
	if err != nil {
		t.Fatalf("native door: %v", err)
	}
	crossRes, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canonical, target)
	if err != nil {
		t.Fatalf("cross-format door: %v", err)
	}

	nb := gjson.ParseBytes(nativeRes.Body)
	cb := gjson.ParseBytes(crossRes.Body)

	if nb.Get("max_tokens").Int() != cb.Get("max_tokens").Int() {
		t.Fatalf("the doors clamped differently: native %d, cross-format %d",
			nb.Get("max_tokens").Int(), cb.Get("max_tokens").Int())
	}
	nativeBudget := nb.Get("thinking.budget_tokens").Int()
	crossBudget := cb.Get("thinking.budget_tokens").Int()
	if nativeBudget != crossBudget {
		t.Errorf("the doors disagree on the budget after OUR clamp: native %d, cross-format %d\n"+
			"  whichever is above max_tokens (%d) sends Anthropic a pair the caller never wrote",
			nativeBudget, crossBudget, nb.Get("max_tokens").Int())
	}
	if nativeBudget >= nb.Get("max_tokens").Int() {
		t.Errorf("the native door left budget %d at or above max_tokens %d — the 400 that "+
			"follows describes a request the caller never sent",
			nativeBudget, nb.Get("max_tokens").Int())
	}
}

// The other half of the contract, and the reason the repair is scoped to OUR
// clamp: a pair the CALLER sent inconsistent rides through untouched. This
// door's contract is that native features are forwarded verbatim, and
// repairing an inconsistency we did not cause would be rewriting a native
// request beyond the evidence-cited quirks — with Anthropic's own answer being
// the one the caller should see.
func TestTwoDoorParity_ACallersOwnInconsistentPairIsTheirsToSee(t *testing.T) {
	// No model ceiling, so nothing of ours fires; 1024 is not below 16.
	body := []byte(`{"model":"m","max_tokens":16,` +
		`"thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`)
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-opus-4-6"}, false)
	if err != nil {
		t.Fatalf("a pair the caller sent must not be refused by us: %v", err)
	}
	root := gjson.ParseBytes(res.Body)
	if root.Get("thinking.budget_tokens").Int() != 1024 || root.Get("max_tokens").Int() != 16 {
		t.Errorf("we rewrote a pair we did not break: max_tokens=%d budget=%d",
			root.Get("max_tokens").Int(), root.Get("thinking.budget_tokens").Int())
	}
}

// TestTwoDoorParity_TheReconcileTouchesOnlyWhatItMustOnTheNativeDoor.
//
// The repair runs on a door whose contract is "native features ride through
// verbatim", so every shape it declines to touch is part of that contract
// rather than an oversight. Asserted through the public door and on the
// observable that matters: no thinking rewrite is emitted and the caller's own
// numbers come out unchanged.
func TestTwoDoorParity_TheReconcileTouchesOnlyWhatItMustOnTheNativeDoor(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		why  string
	}{
		{
			name: "no thinking block",
			body: `{"model":"m","max_tokens":8192,"messages":[]}`,
			why:  "a request that never asked to think has nothing to reconcile",
		},
		{
			name: "thinking explicitly disabled",
			body: `{"model":"m","max_tokens":8192,"thinking":{"type":"disabled"},"messages":[]}`,
			why:  "a disabled block carries no budget to fit, and clamping one in would turn thinking ON",
		},
		{
			name: "thinking enabled with no budget stated",
			body: `{"model":"m","max_tokens":8192,"thinking":{"type":"enabled"},"messages":[]}`,
			why:  "there is no number of the caller's to keep consistent; inventing one is not a repair",
		},
		{
			name: "thinking is not an object",
			body: `{"model":"m","max_tokens":8192,"thinking":true,"messages":[]}`,
			why:  "a malformed block is the caller's to see, as the verbatim contract says",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A ceiling well below max_tokens, so OUR clamp definitely fires and
			// the reconcile is reached rather than skipped by the guard above it.
			res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages, []byte(tc.body),
				provcore.CallTarget{ProviderModelID: "claude-haiku-4-5", MaxOutputTokens: 2048}, false)
			if err != nil {
				t.Fatalf("%s: %v — %s", tc.name, err, tc.why)
			}
			for _, r := range res.Rewrites {
				if len(r) >= 8 && r[:8] == "thinking" {
					t.Errorf("a thinking rewrite was emitted for %q: %v — %s", tc.name, res.Rewrites, tc.why)
				}
			}
			before := gjson.Get(tc.body, "thinking").Raw
			after := gjson.ParseBytes(res.Body).Get("thinking").Raw
			if before != after {
				t.Errorf("the thinking block changed from %q to %q — %s", before, after, tc.why)
			}
		})
	}
}

// When OUR clamp leaves no room for thinking at all, the native door refuses
// with a message naming both numbers — the same answer the cross-format door
// gives, and the reason the refusal is ours rather than Anthropic's: their 400
// can only report whichever bound they checked first.
func TestTwoDoorParity_AClampThatMakesThinkingImpossibleIsRefusedOnBothDoors(t *testing.T) {
	native := []byte(`{"model":"m","max_tokens":8192,` +
		`"thinking":{"type":"enabled","budget_tokens":4096},"messages":[]}`)
	canonical := []byte(`{"model":"m","max_tokens":8192,"messages":[{"role":"user","content":"hi"}],` +
		`"nexus":{"ext":{"anthropic":{"thinking":{"type":"enabled","budget_tokens":4096}}}}}`)
	// A ceiling of 512 leaves 511 for thinking, below the 1024 floor.
	target := provcore.CallTarget{ProviderModelID: "claude-haiku-4-5", MaxOutputTokens: 512}

	_, nativeErr := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages, native, target, false)
	_, crossErr := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canonical, target)

	if (nativeErr == nil) != (crossErr == nil) {
		t.Fatalf("the doors disagree on whether this is servable: native err=%v, cross-format err=%v",
			nativeErr, crossErr)
	}
	if nativeErr == nil {
		t.Fatalf("our clamp left 511 tokens for a floor of 1024 and the request was forwarded " +
			"anyway — Anthropic answers 400 about a pair we created")
	}
}

// The clamp fires but the caller's budget already fits under the new cap:
// nothing to repair, and repairing anyway would rewrite a number that was
// already correct.
func TestTwoDoorParity_AClampThatLeavesTheBudgetFittingChangesNothing(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":8192,` +
		`"thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`)
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-haiku-4-5", MaxOutputTokens: 4096}, false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	root := gjson.ParseBytes(res.Body)
	if root.Get("max_tokens").Int() != 4096 {
		t.Fatalf("the clamp did not fire, so this case is not the one under test: %s", res.Body)
	}
	if got := root.Get("thinking.budget_tokens").Int(); got != 1024 {
		t.Errorf("budget = %d, want the caller's own 1024 — it already fit under the clamped "+
			"cap, and rewriting a correct number is not a repair", got)
	}
	for _, r := range res.Rewrites {
		if len(r) >= 8 && r[:8] == "thinking" {
			t.Errorf("a thinking rewrite was emitted for a budget that already fit: %v", res.Rewrites)
		}
	}
}
