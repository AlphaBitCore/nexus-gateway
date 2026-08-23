// Named failure modes for the per-generation thinking contract:
//   - adaptive-contract model × canonical effort → adaptive + output_config,
//     and NEVER a budget (probed: adaptive + budget_tokens is a 400)
//   - enabled-contract model × canonical effort → enabled + budget, unchanged
//   - native enabled+budget → coerced on adaptive models only, with the
//     budget surviving as a level; verbatim on enabled models
//   - an unknown future family gets the adaptive shape, not the 400
package codec

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func encodeEffort(t *testing.T, model, effort string) []byte {
	t.Helper()
	body := `{"model":"` + model + `","max_tokens":4096,"reasoning_effort":"` + effort + `",` +
		`"messages":[{"role":"user","content":"count"}]}`
	res, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, []byte(body),
		provcore.CallTarget{ProviderModelID: model})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return res.Body
}

// The five models the reasoning sweep saw 400 on all sit past the contract
// boundary; each must now leave on the adaptive shape with no budget.
func TestThinkingContract_AdaptiveModelsGetAdaptiveShape(t *testing.T) {
	for _, model := range []string{
		"claude-opus-5", "claude-opus-4-8", "claude-opus-4-7",
		"claude-sonnet-5", "claude-fable-5",
	} {
		out := encodeEffort(t, model, "low")
		if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
			t.Errorf("%s: thinking.type = %q, want adaptive", model, got)
		}
		if gjson.GetBytes(out, "thinking.budget_tokens").Exists() {
			t.Errorf("%s: budget_tokens rode along with adaptive — the probed 400", model)
		}
		if got := gjson.GetBytes(out, "output_config.effort").String(); got != "low" {
			t.Errorf("%s: output_config.effort = %q, want low", model, got)
		}
	}
}

// The generations the sweep saw REASON on the enabled shape keep it — the
// probe showed an older model silently IGNORES the adaptive shape, so moving
// them would turn their reasoning off.
func TestThinkingContract_EnabledModelsKeepEnabledShape(t *testing.T) {
	for _, model := range []string{"claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5"} {
		out := encodeEffort(t, model, "low")
		if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
			t.Errorf("%s: thinking.type = %q, want enabled", model, got)
		}
		if !gjson.GetBytes(out, "thinking.budget_tokens").Exists() {
			t.Errorf("%s: enabled shape lost its budget", model)
		}
		if gjson.GetBytes(out, "output_config").Exists() {
			t.Errorf("%s: output_config leaked onto the enabled contract", model)
		}
	}
}

// An unlisted family — which every future Claude release starts out as —
// gets the shape the vendor is moving toward instead of a 400 in its first
// hour of traffic. Same fail-safe direction as the sampling allowlist.
func TestThinkingContract_UnknownFutureFamilyIsAdaptive(t *testing.T) {
	out := encodeEffort(t, "claude-nova-1", "medium")
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
		t.Errorf("unknown family: thinking.type = %q, want adaptive", got)
	}
	if got := gjson.GetBytes(out, "output_config.effort").String(); got != "medium" {
		t.Errorf("unknown family: effort = %q, want medium", got)
	}
}

// minimal is not in the wire's vocabulary; it maps to the nearest level
// rather than being forwarded for the wire to reject.
func TestThinkingContract_MinimalMapsToLow(t *testing.T) {
	out := encodeEffort(t, "claude-opus-5", "minimal")
	if got := gjson.GetBytes(out, "output_config.effort").String(); got != "low" {
		t.Errorf("effort = %q, want low", got)
	}
}

// The native door: an SDK still speaking enabled+budget to an adaptive model
// is coerced — the budget survives as a level — and the rewrite is visible.
func TestThinkingContract_NativeEnabledCoercedOnAdaptiveModel(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","max_tokens":9000,` +
		`"thinking":{"type":"enabled","budget_tokens":5000},` +
		`"messages":[{"role":"user","content":"count"}]}`)
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-opus-5"}, false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := gjson.GetBytes(res.Body, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive\n%s", got, res.Body)
	}
	if gjson.GetBytes(res.Body, "thinking.budget_tokens").Exists() {
		t.Error("budget_tokens rode along with adaptive — the probed 400")
	}
	if got := gjson.GetBytes(res.Body, "output_config.effort").String(); got != "medium" {
		t.Errorf("effort = %q, want medium (5000 tokens)", got)
	}
	if joined := strings.Join(res.Rewrites, ","); !strings.Contains(joined, "thinking.enabled→adaptive") {
		t.Errorf("coercion not visible in rewrites: %v", res.Rewrites)
	}
}

// The same native body to an enabled-contract model rides through verbatim —
// the differential is not a license to rewrite what already matches the wire.
func TestThinkingContract_NativeEnabledVerbatimOnEnabledModel(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-6","max_tokens":9000,` +
		`"thinking":{"type":"enabled","budget_tokens":5000},` +
		`"messages":[{"role":"user","content":"count"}]}`)
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-opus-4-6"}, false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := gjson.GetBytes(res.Body, "thinking.type").String(); got != "enabled" {
		t.Errorf("thinking.type = %q, want enabled untouched", got)
	}
	if got := gjson.GetBytes(res.Body, "thinking.budget_tokens").Int(); got != 5000 {
		t.Errorf("budget = %d, want the caller's 5000", got)
	}
}

// The ext door: a canonical caller who injected the native enabled block gets
// the same coercion behind the same predicate — three doors, one contract.
func TestThinkingContract_ExtInjectedEnabledCoercedOnAdaptiveModel(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":9000,` +
		`"nexus":{"ext":{"anthropic":{"thinking":{"type":"enabled","budget_tokens":1000}}}},` +
		`"messages":[{"role":"user","content":"count"}]}`
	res, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, []byte(body),
		provcore.CallTarget{ProviderModelID: "claude-opus-5"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(res.Body, "thinking.type").String(); got != "adaptive" {
		t.Errorf("thinking.type = %q, want adaptive", got)
	}
	if got := gjson.GetBytes(res.Body, "output_config.effort").String(); got != "low" {
		t.Errorf("effort = %q, want low (1000 tokens)", got)
	}
}

// An empty native thinking block (`{}`) rides the ext door but carries nothing
// to translate: it is skipped fail-open, so the model gets NO thinking rather
// than a meaningless empty block or a 400. Distinct from a malformed (non-JSON)
// block, which skips down the unmarshal-error arm with its own WARN key.
func TestThinkingContract_ExtEmptyThinkingIsSkipped(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":9000,` +
		`"nexus":{"ext":{"anthropic":{"thinking":{}}}},` +
		`"messages":[{"role":"user","content":"count"}]}`
	res, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, []byte(body),
		provcore.CallTarget{ProviderModelID: "claude-opus-5"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if gjson.GetBytes(res.Body, "thinking").Exists() {
		t.Errorf("an empty ext thinking block was not skipped fail-open: %s", res.Body)
	}
}

// A non-Claude id behind this adapter is some other endpoint speaking the
// Messages shape; its thinking contract was never probed and is not judged.
func TestThinkingContract_NonClaudeModelNeverCoerced(t *testing.T) {
	body := []byte(`{"model":"mistral-large","max_tokens":9000,` +
		`"thinking":{"type":"enabled","budget_tokens":5000},"messages":[]}`)
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "mistral-large"}, false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := gjson.GetBytes(res.Body, "thinking.type").String(); got != "enabled" {
		t.Errorf("non-Claude thinking.type = %q, want untouched enabled", got)
	}
}

// An unknown canonical effort word produces NO thinking on the adaptive
// contract — guessing which level an unknown word means is how a translation
// starts producing answers nobody asked for.
func TestThinkingContract_UnknownEffortWordProducesNoThinking(t *testing.T) {
	out := encodeEffort(t, "claude-opus-5", "turbo")
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Errorf("unknown effort word produced a thinking block: %s", out)
	}
	if gjson.GetBytes(out, "output_config").Exists() {
		t.Errorf("unknown effort word produced output_config: %s", out)
	}
}

// none means the caller asked NOT to think — spelled by omission.
func TestThinkingContract_NoneOmitsThinkingOnAdaptive(t *testing.T) {
	out := encodeEffort(t, "claude-opus-5", "none")
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Errorf("none produced a thinking block: %s", out)
	}
}

// Native edge cases on an adaptive model: a disabled block, a body without
// thinking, and an enabled block with no usable budget all ride verbatim.
func TestThinkingContract_NativeEdgesRideVerbatim(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"disabled block", `{"model":"claude-opus-5","max_tokens":100,"thinking":{"type":"disabled"},"messages":[]}`},
		{"no thinking", `{"model":"claude-opus-5","max_tokens":100,"messages":[]}`},
		{"enabled without budget", `{"model":"claude-opus-5","max_tokens":100,"thinking":{"type":"enabled"},"messages":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages, []byte(tc.body),
				provcore.CallTarget{ProviderModelID: "claude-opus-5"}, false)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if gjson.GetBytes(res.Body, "thinking.type").String() == "adaptive" {
				t.Errorf("%s was coerced: %s", tc.name, res.Body)
			}
			if gjson.GetBytes(res.Body, "output_config").Exists() {
				t.Errorf("%s grew an output_config: %s", tc.name, res.Body)
			}
		})
	}
}

// A native body that already carries its own output_config keeps it — the
// coercion fills the effort only when the caller has not spoken.
func TestThinkingContract_NativeKeepsCallersOutputConfig(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","max_tokens":9000,` +
		`"thinking":{"type":"enabled","budget_tokens":100},` +
		`"output_config":{"effort":"xhigh"},"messages":[]}`)
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-opus-5"}, false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := gjson.GetBytes(res.Body, "output_config.effort").String(); got != "xhigh" {
		t.Errorf("caller's effort overwritten: %q", got)
	}
	if got := gjson.GetBytes(res.Body, "thinking.type").String(); got != "adaptive" {
		t.Errorf("thinking.type = %q, want adaptive", got)
	}
}
