package moonshot

// The Moonshot wire contract: the kimi fixed-temp families lose
// temperature + top_p (upstream 400 "invalid temperature: only 1 is
// allowed for this model" on any caller-supplied value), everything else
// forwards untouched, and BOTH codec entry points apply the same rules —
// the historical gap was a strip that fired on the OpenAI-chat ingress
// but not on bodies bridged from /v1/messages.

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// TestIsFixedTempModel pins the prefix-list for the fixed-temperature
// family.
func TestIsFixedTempModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  bool
	}{
		{"kimi-k2.5", true},
		{"kimi-k2.5-2026-04", true},
		{"kimi-k2.6", true},
		{"kimi-k2.6-mini", true},
		// The k2.7 families shipped in the catalog while this list still
		// ended at k2.6, so every temperature-sending client got the
		// upstream's own 400 until a smoke run against production caught it.
		{"kimi-k2.7-code", true},
		{"kimi-k2.7-code-highspeed", true},
		// Probed to accept a caller temperature — the one carve-out.
		{"kimi-k2-thinking", false},
		{"kimi-k2-thinking-2026-01", false},

		// Unprobed ids INSIDE the kimi namespace fail safe: stripped, not
		// forwarded. kimi-k3 is the live case — the vendor's /v1/models
		// carried it on 2026-08-06 while our catalog did not, which is the
		// exact sequence that made k2.7 400 for every caller. Bare kimi-k2
		// is not offered by the vendor at all and has no probe behind it.
		{"kimi-k3", true},
		{"kimi-k3-turbo", true},
		{"kimi-k2", true},
		{"kimi-k4.1", true},

		// Outside the namespace: untouched, they accept temperature.
		{"moonshot-v1-8k", false},
		{"moonshot-v1-128k", false},
		{"moonshot-v1-auto", false},
		{"", false},
	}
	for _, tc := range cases {
		got := IsFixedTempModel(tc.model)
		if got != tc.want {
			t.Errorf("IsFixedTempModel(%q)=%v want %v", tc.model, got, tc.want)
		}
	}
}

// TestContract_BothDoors_StripFixedTemp drives the production codec (the
// identity codec carrying this contract) through both entry points and
// asserts the strip + its report land identically. The canonical door is
// the leg the legacy dispatch callback never covered: bodies bridged from
// /v1/messages 400'd on the same kimi models the chat ingress coerced.
func TestContract_BothDoors_StripFixedTemp(t *testing.T) {
	t.Parallel()
	c := openai.NewIdentityCodec(Contract())
	body := []byte(`{"model":"kimi-k2.5","messages":[{"role":"user","content":"hi"}],"temperature":0.3,"top_p":0.9}`)
	target := provcore.CallTarget{ProviderModelID: "kimi-k2.5"}
	wantRW := []string{"temperature→removed", "top_p→removed"}

	check := func(name string, res provcore.EncodeResult, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if gjson.GetBytes(res.Body, "temperature").Exists() || gjson.GetBytes(res.Body, "top_p").Exists() {
			t.Fatalf("%s: fixed-temp params must be stripped: %s", name, res.Body)
		}
		if len(res.Rewrites) != 2 || res.Rewrites[0] != wantRW[0] || res.Rewrites[1] != wantRW[1] {
			t.Fatalf("%s: rewrites %v, want %v (exact legacy labels)", name, res.Rewrites, wantRW)
		}
	}

	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIChat, body, target)
	check("EncodeRequest (cross-format door)", encRes, err)

	rwRes, err := c.RewriteNative(typology.WireShapeOpenAIChat, body, target, false)
	check("RewriteNative (native door)", rwRes, err)
}

// The strip is targeted: moonshot-v1-*, kimi-k2-thinking, and any future
// kimi model outside the fixed-temp family keep the caller's temperature
// (probed: those families accept arbitrary values).
func TestContract_NoQuirkModels_Untouched(t *testing.T) {
	t.Parallel()
	c := openai.NewIdentityCodec(Contract())
	for _, model := range []string{"moonshot-v1-8k", "moonshot-v1-32k", "moonshot-v1-128k", "kimi-k2-thinking"} {
		body := []byte(`{"model":"` + model + `","messages":[],"temperature":0.3,"top_p":0.9}`)
		res, err := c.RewriteNative(typology.WireShapeOpenAIChat, body, provcore.CallTarget{ProviderModelID: model}, false)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "temperature").Float() != 0.3 || gjson.GetBytes(res.Body, "top_p").Float() != 0.9 {
			t.Fatalf("%s accepts arbitrary sampling params (probed); they must survive: %s", model, res.Body)
		}
		if res.Rewrites != nil {
			t.Fatalf("%s: nothing to report: %v", model, res.Rewrites)
		}
	}
}

// The spec publishes no transitional callback: the rules ride the codec
// contract, and a re-wired callback would double-apply them.
func TestNewSpec_Drained(t *testing.T) {
	t.Parallel()
	spec := NewSpec(nil)
	// The transitional callback fields no longer exist on AdapterSpec —
	// drained-ness is compile-enforced; pin the codec wiring instead.
	if spec.SchemaCodec == nil {
		t.Fatal("codec must be wired")
	}
}
