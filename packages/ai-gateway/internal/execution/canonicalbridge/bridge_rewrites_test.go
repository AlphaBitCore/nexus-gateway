package canonicalbridge

// The bridge returns the target codec's in-place coercions alongside the
// wire body. The rules execute INSIDE this encode (the codec contract),
// and the post-bridge re-entry differential is idempotent — it finds
// nothing left to apply and reports nothing — so these rewrites are the
// only carrier of the x-nexus-coerced signal on bridge-translated
// attempts (executor failover legs).

import (
	"log/slog"
	"testing"

	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

func TestIngressChatToWire_ReturnsCodecRewrites(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(slog.Default()))
	// Anthropic ingress body → openai target o3: the contract owes the
	// max_tokens rename and the sampling strips on the bridged encode.
	body := []byte(`{"model":"o3","max_tokens":64,"temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`)
	ct := provcore.CallTarget{Format: provcore.FormatOpenAI, ProviderModelID: "o3"}
	wire, rewrites, err := b.IngressChatToWire(provcore.FormatAnthropic, provcore.FormatOpenAI, body, ct, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(wire, "temperature").Exists() {
		t.Fatalf("bridged encode must apply the strip: %s", wire)
	}
	if gjson.GetBytes(wire, "max_completion_tokens").Int() != 64 {
		t.Fatalf("bridged encode must apply the rename: %s", wire)
	}
	want := map[string]bool{"max_tokens→max_completion_tokens": true, "temperature→removed": true}
	if len(rewrites) != 2 || !want[rewrites[0]] || !want[rewrites[1]] {
		t.Fatalf("the coercions must be RETURNED, not swallowed: %v", rewrites)
	}
}

// TestIngressChatToWire_StripsNexusThinking_OpenAIWireTarget pins the F1
// leak fix: an Anthropic-ingress body replaying a signed thinking history
// canonicalizes to reasoning_content + the Anthropic-private nexus_thinking
// block carrier. Routed cross-format to an OpenAI-wire target (identity
// codec, verbatim message forward), the nexus_thinking carrier MUST be
// stripped so the foreign upstream never receives it — while the L2
// reasoning_content text stays intact for a reasoning target to consume.
func TestIngressChatToWire_StripsNexusThinking_OpenAIWireTarget(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(slog.Default()))
	body := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64,
		"messages":[
			{"role":"user","content":"solve it"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"step 1","signature":"sig-abc"},
				{"type":"text","text":"the answer is 42"}
			]}
		]
	}`)
	ct := provcore.CallTarget{Format: provcore.FormatOpenAI, ProviderModelID: "gpt-4o"}
	wire, _, err := b.IngressChatToWire(provcore.FormatAnthropic, provcore.FormatOpenAI, body, ct, false)
	if err != nil {
		t.Fatal(err)
	}
	var asst gjson.Result
	for _, m := range gjson.GetBytes(wire, "messages").Array() {
		if m.Get("role").String() == "assistant" {
			asst = m
		}
	}
	if asst.Get("nexus_thinking").Exists() {
		t.Fatalf("assistant nexus_thinking must NOT egress to an OpenAI-wire upstream: %s", wire)
	}
	if asst.Get("reasoning_content").String() != "step 1" {
		t.Fatalf("reasoning_content (L2 universal) must survive the strip: %s", wire)
	}
}

// TestStripInternalCarriersForTarget pins the shared strip both egress legs
// use. nexus_thinking (Anthropic-private) is dropped for every target whose
// codec does NOT consume it — the OpenAI identity codec (verbatim forward) AND
// the Cohere v2 codec (also a verbatim `messages` passthrough, so keying on
// "wire == OpenAIChat" would leak it) AND the field-mapping Gemini codec — and
// PRESERVED only for the Anthropic-family codecs that rebuild the signed blocks
// (Anthropic natively, Bedrock by delegation). reasoning_content (the L2
// universal text) stays in every case.
func TestStripInternalCarriersForTarget(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(slog.Default()))
	canon := []byte(`{"model":"m","messages":[{"role":"assistant","content":"hi",` +
		`"reasoning_content":"why","nexus_thinking":[{"thinking":"why","signature":"sig-1"}]}]}`)

	// Targets whose codec does not consume the carrier → strip.
	for _, target := range []provcore.Format{provcore.FormatOpenAI, provcore.FormatCohere, provcore.FormatGemini, provcore.FormatDeepSeek} {
		out := b.StripInternalCarriersForTarget(canon, target)
		if gjson.GetBytes(out, "messages.0.nexus_thinking").Exists() {
			t.Fatalf("nexus_thinking must be stripped for non-consuming target %q: %s", target, out)
		}
		if gjson.GetBytes(out, "messages.0.reasoning_content").String() != "why" {
			t.Fatalf("reasoning_content must survive for target %q: %s", target, out)
		}
	}

	// Anthropic-family targets reconstruct the signed blocks → preserve.
	for _, target := range []provcore.Format{provcore.FormatAnthropic, provcore.FormatBedrock} {
		out := b.StripInternalCarriersForTarget(canon, target)
		if !gjson.GetBytes(out, "messages.0.nexus_thinking").Exists() {
			t.Fatalf("nexus_thinking must be PRESERVED for consuming target %q (codec rebuilds signed blocks): %s", target, out)
		}
	}
}

func TestIngressChatToWire_NoQuirk_NilRewrites(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(slog.Default()))
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	ct := provcore.CallTarget{Format: provcore.FormatOpenAI, ProviderModelID: "gpt-4o"}
	_, rewrites, err := b.IngressChatToWire(provcore.FormatAnthropic, provcore.FormatOpenAI, body, ct, false)
	if err != nil {
		t.Fatal(err)
	}
	if rewrites != nil {
		t.Fatalf("nothing was coerced: %v", rewrites)
	}
}

// The prod-observed P3A gap: a /v1/messages (Anthropic-ingress) body
// routed to a kimi fixed-temp model kept its temperature — the legacy
// dispatch callback only covered the native chat leg — and the vendor
// 400'd ("invalid temperature: only 1 is allowed for this model"). With
// the rules in the moonshot contract the bridge's codec encode strips
// them and RETURNS the coercions for x-nexus-coerced.
func TestIngressChatToWire_AnthropicToKimi_StripsFixedTemp(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(slog.Default()))
	body := []byte(`{"model":"kimi-k2.5","max_tokens":64,"temperature":0.3,"top_p":0.9,"messages":[{"role":"user","content":"hi"}]}`)
	ct := provcore.CallTarget{Format: provcore.FormatMoonshot, ProviderModelID: "kimi-k2.5"}
	wire, rewrites, err := b.IngressChatToWire(provcore.FormatAnthropic, provcore.FormatMoonshot, body, ct, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(wire, "temperature").Exists() || gjson.GetBytes(wire, "top_p").Exists() {
		t.Fatalf("the fixed-temp strip must fire on the bridged leg: %s", wire)
	}
	want := []string{"temperature→removed", "top_p→removed"}
	if len(rewrites) != 2 || rewrites[0] != want[0] || rewrites[1] != want[1] {
		t.Fatalf("the strips must be returned for the coercion report: %v", rewrites)
	}
}

// The confirmed cross-format bug the thinking fix closes: an Anthropic
// /v1/messages history with a real thinking block, bridged to a DeepSeek
// thinking model, must carry the REAL reasoning text into the wire body
// (reasoning_content) — not the empty "" back-fill that masked it before
// the ingress converter mapped thinking → reasoning_content.
func TestIngressChatToWire_AnthropicThinking_ReachesDeepSeekReasoning(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(slog.Default()))
	body := []byte(`{"model":"deepseek-reasoner","max_tokens":64,"messages":[
		{"role":"user","content":"solve it"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"the real chain of thought","signature":"s"},
			{"type":"text","text":"42"}
		]}
	]}`)
	ct := provcore.CallTarget{Format: provcore.FormatDeepSeek, ProviderModelID: "deepseek-reasoner"}
	wire, _, err := b.IngressChatToWire(provcore.FormatAnthropic, provcore.FormatDeepSeek, body, ct, false)
	if err != nil {
		t.Fatal(err)
	}
	// The DeepSeek wire is OpenAI-shaped; the assistant message must carry
	// the real reasoning text, not an empty string.
	var asst gjson.Result
	for _, m := range gjson.GetBytes(wire, "messages").Array() {
		if m.Get("role").String() == "assistant" {
			asst = m
		}
	}
	if asst.Get("reasoning_content").String() != "the real chain of thought" {
		t.Fatalf("the real reasoning text must survive to DeepSeek, not be dropped and masked by \"\": %s", wire)
	}
}
