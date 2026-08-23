package codec

import (
	"bytes"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// A sampling-accepting family keeps its native sampling params AND its native
// features (thinking config) verbatim through the differential — only the model
// stamp is due, no coercion.
func TestAnthropicRewriteNative_PreservesNativeFeaturesWhenAccepted(t *testing.T) {
	body := []byte(`{"model":"my-alias","max_tokens":16,"temperature":0.5,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`)
	res, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-opus-4-6"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "model").String() != "claude-opus-4-6" {
		t.Fatalf("model not stamped: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "temperature").Float() != 0.5 {
		t.Fatalf("an accepting family keeps native temperature: %s", res.Body)
	}
	if !gjson.GetBytes(res.Body, "thinking.budget_tokens").Exists() {
		t.Fatalf("thinking config lost: %s", res.Body)
	}
	if res.Rewrites != nil {
		t.Fatalf("no coercion due, got %v", res.Rewrites)
	}
}

// D3 (owner-approved): a native /v1/messages body sent to a rejects-sampling
// family has temperature / top_p / top_k stripped (not 400'd upstream), with a
// rewrite per stripped param for x-nexus-coerced; native features survive.
func TestAnthropicRewriteNative_D3_StripsSamplingForRejectingFamily(t *testing.T) {
	body := []byte(`{"model":"x","max_tokens":16,"temperature":0.5,"top_p":0.9,"top_k":40,` +
		`"thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`)
	res, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-opus-4-8"}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"temperature", "top_p", "top_k"} {
		if gjson.GetBytes(res.Body, f).Exists() {
			t.Fatalf("%s must be stripped for a rejecting family: %s", f, res.Body)
		}
	}
	// The strip must not DELETE thinking; on this adaptive-contract model the
	// block is coerced to the shape its wire accepts, budget carried as a level.
	if gjson.GetBytes(res.Body, "thinking.type").String() != "adaptive" {
		t.Fatalf("native thinking must survive the sampling strip: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "output_config.effort").String() != "low" {
		t.Fatalf("the 1024 budget must survive as a level: %s", res.Body)
	}
	want := map[string]bool{"temperature→removed": true, "top_p→removed": true, "top_k→removed": true,
		"thinking.enabled→adaptive+output_config.effort=low": true}
	if len(res.Rewrites) != 4 {
		t.Fatalf("expected 4 coercion rewrites, got %v", res.Rewrites)
	}
	for _, r := range res.Rewrites {
		if !want[r] {
			t.Fatalf("unexpected rewrite %q in %v", r, res.Rewrites)
		}
	}
}

// D3: an accepting family that rejects temperature+top_p TOGETHER keeps
// temperature and drops top_p when both are present.
func TestAnthropicRewriteNative_D3_DropsTopPWithTemperature(t *testing.T) {
	body := []byte(`{"model":"x","max_tokens":16,"temperature":0.5,"top_p":0.9,"messages":[]}`)
	res, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-opus-4-6"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "temperature").Float() != 0.5 {
		t.Fatalf("temperature must be kept: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "top_p").Exists() {
		t.Fatalf("top_p must be dropped when temperature is present: %s", res.Body)
	}
	if len(res.Rewrites) != 1 || res.Rewrites[0] != "top_p→removed_with_temperature_present" {
		t.Fatalf("expected the combo rewrite, got %v", res.Rewrites)
	}
}

// D3: an over-ceiling max_tokens is clamped to the catalog cap (not 400'd).
func TestAnthropicRewriteNative_D3_ClampsOverCeilingMaxTokens(t *testing.T) {
	body := []byte(`{"model":"x","max_tokens":999999,"messages":[]}`)
	res, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-opus-4-6", MaxOutputTokens: 8192}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "max_tokens").Int() != 8192 {
		t.Fatalf("max_tokens must clamp to the ceiling: %s", res.Body)
	}
	if len(res.Rewrites) != 1 || res.Rewrites[0] != "max_tokens→8192_model_max" {
		t.Fatalf("expected the clamp rewrite, got %v", res.Rewrites)
	}
}

// D3: an absent max_tokens (which Anthropic 400s as "Field required") is filled
// with the model ceiling, mirroring EncodeRequest's absent-fill.
func TestAnthropicRewriteNative_D3_FillsAbsentMaxTokens(t *testing.T) {
	body := []byte(`{"model":"x","messages":[]}`)
	res, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-opus-4-6", MaxOutputTokens: 4096}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "max_tokens").Int() != 4096 {
		t.Fatalf("absent max_tokens must be filled with the ceiling: %s", res.Body)
	}
	if len(res.Rewrites) != 1 || res.Rewrites[0] != "max_tokens→4096_model_default" {
		t.Fatalf("expected the fill rewrite, got %v", res.Rewrites)
	}
}

// The §2 two-doors property for coercions: the native leg (RewriteNative) and
// the cross-format leg (EncodeRequest ∘ canonicalize) make IDENTICAL coercion
// decisions — same rewrites, SAME ORDER — on the same body. Order matters: the
// rewrites become the x-nexus-coerced response-header string, which is
// order-preserving, so a divergent order is an observable two-doors asymmetry.
// A both-coercions-fire body (rejecting family + over-ceiling max_tokens)
// exercises the max_tokens-vs-sampling ordering the earlier in-ceiling body hid.
func TestAnthropicRewriteNative_CoercionParityWithEncodeRequest(t *testing.T) {
	native := []byte(`{"model":"m","max_tokens":999999,"temperature":0.5,"top_p":0.9,"top_k":40,"messages":[{"role":"user","content":"hi"}]}`)
	target := provcore.CallTarget{ProviderModelID: "claude-opus-4-8", MaxOutputTokens: 8192}

	nativeRes, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, native, target, false)
	if err != nil {
		t.Fatal(err)
	}
	// The native body's root fields + a {role,content} message are
	// canonical-OpenAI-compatible, so it doubles as the canonicalized input for
	// the cross-format door without importing the ingress converter (cycle).
	crossRes, err := NewCodec().EncodeRequest(typology.WireShapeAnthropicMessages, native, target)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"temperature", "top_p", "top_k"} {
		if gjson.GetBytes(nativeRes.Body, f).Exists() {
			t.Fatalf("native leg kept %s: %s", f, nativeRes.Body)
		}
		if gjson.GetBytes(crossRes.Body, f).Exists() {
			t.Fatalf("cross-format leg kept %s: %s", f, crossRes.Body)
		}
	}
	if gjson.GetBytes(nativeRes.Body, "max_tokens").Int() != 8192 || gjson.GetBytes(crossRes.Body, "max_tokens").Int() != 8192 {
		t.Fatalf("both doors must clamp max_tokens to 8192: native=%s cross=%s", nativeRes.Body, crossRes.Body)
	}
	// Exact-order equality — the header string must be byte-identical.
	if len(nativeRes.Rewrites) != len(crossRes.Rewrites) {
		t.Fatalf("rewrite count diverges:\n native=%v\n cross=%v", nativeRes.Rewrites, crossRes.Rewrites)
	}
	for i := range nativeRes.Rewrites {
		if nativeRes.Rewrites[i] != crossRes.Rewrites[i] {
			t.Fatalf("rewrite ORDER diverges at %d:\n native=%v\n cross=%v", i, nativeRes.Rewrites, crossRes.Rewrites)
		}
	}
}

// D3 parity edge: a body carrying the OpenAI-only max_completion_tokens (never
// present on a genuine native Anthropic body, but possible from a hybrid client)
// is resolved into max_tokens and the foreign key dropped, matching
// EncodeRequest — not mis-filled to the ceiling with the key leaked upstream.
func TestAnthropicRewriteNative_HonorsMaxCompletionTokens(t *testing.T) {
	body := []byte(`{"model":"x","max_completion_tokens":100,"messages":[]}`)
	res, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-opus-4-6", MaxOutputTokens: 8192}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "max_tokens").Int() != 100 {
		t.Fatalf("max_completion_tokens must resolve into max_tokens: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "max_completion_tokens").Exists() {
		t.Fatalf("the OpenAI-only key must be dropped, not leaked upstream: %s", res.Body)
	}
	if res.Rewrites != nil {
		t.Fatalf("an in-ceiling resolve is not a coercion: %v", res.Rewrites)
	}
}

// A known-older-accepting family (claude-2.x / claude-instant) keeps its
// sampling params — the allowlist must not conflate them with the
// unknown-future families it fails safe toward. The version-boundary matcher
// keeps that carve-out from leaking to a hypothetical future rejecting family
// (claude-20-…) that merely shares the "claude-2" text prefix.
func TestAnthropicRewriteNative_AllowlistFamilyBoundary(t *testing.T) {
	accepts := []string{"claude-2.1", "claude-2", "claude-instant-1.2", "claude-3-5-sonnet", "claude-opus-4-6-20251101"}
	for _, model := range accepts {
		body := []byte(`{"model":"x","max_tokens":16,"temperature":0,"messages":[]}`)
		res, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, body,
			provcore.CallTarget{ProviderModelID: model, MaxOutputTokens: 8192}, false)
		if err != nil {
			t.Fatal(err)
		}
		if !gjson.GetBytes(res.Body, "temperature").Exists() {
			t.Fatalf("%s is on the accepting allowlist; temperature must NOT be stripped: %s", model, res.Body)
		}
	}
	// A future family that only shares the text prefix must NOT inherit the
	// carve-out — it fails safe to "strip" (rejecting family).
	rejects := []string{"claude-20-opus", "claude-25-next", "claude-opus-4-8"}
	for _, model := range rejects {
		body := []byte(`{"model":"x","max_tokens":16,"temperature":0,"messages":[]}`)
		res, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, body,
			provcore.CallTarget{ProviderModelID: model, MaxOutputTokens: 8192}, false)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "temperature").Exists() {
			t.Fatalf("%s only shares a text prefix; it must fail safe to strip: %s", model, res.Body)
		}
	}
}

// max_completion_tokens edge parity with EncodeRequest: over-ceiling clamps, and
// when both mct and max_tokens are present mct wins (matches EncodeRequest's
// higher-precedence arm).
func TestAnthropicRewriteNative_MaxCompletionTokensEdges(t *testing.T) {
	target := provcore.CallTarget{ProviderModelID: "claude-opus-4-6", MaxOutputTokens: 8192}

	// over ceiling → clamp + rewrite
	over, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages,
		[]byte(`{"model":"x","max_completion_tokens":999999,"messages":[]}`), target, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(over.Body, "max_tokens").Int() != 8192 || gjson.GetBytes(over.Body, "max_completion_tokens").Exists() {
		t.Fatalf("over-ceiling mct must clamp into max_tokens and drop the key: %s", over.Body)
	}
	if len(over.Rewrites) != 1 || over.Rewrites[0] != "max_tokens→8192_model_max" {
		t.Fatalf("expected clamp rewrite, got %v", over.Rewrites)
	}

	// both present → mct wins
	both, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages,
		[]byte(`{"model":"x","max_completion_tokens":100,"max_tokens":7000,"messages":[]}`), target, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(both.Body, "max_tokens").Int() != 100 {
		t.Fatalf("max_completion_tokens must win over max_tokens: %s", both.Body)
	}
	if gjson.GetBytes(both.Body, "max_completion_tokens").Exists() {
		t.Fatalf("the OpenAI-only key must be dropped: %s", both.Body)
	}
}

// BenchmarkAnthropicRewriteNative_NoCoercion measures the common native-leg
// cost when nothing is due (accepting family, in-ceiling max_tokens): the added
// D3 policy is only gjson probes over an already-parsed root, no sjson edit, and
// the body forwards as the same slice.
func BenchmarkAnthropicRewriteNative_NoCoercion(b *testing.B) {
	body := []byte(`{"model":"claude-opus-4-6","max_tokens":256,"temperature":0.7,` +
		`"system":"You are a careful assistant. Follow the instructions precisely and cite sources where relevant.",` +
		`"messages":[` +
		`{"role":"user","content":"Summarize the quarterly report and list the three biggest risks with a short justification for each."},` +
		`{"role":"assistant","content":"Here is a concise summary of the quarterly report, followed by the three biggest risks and why they matter."},` +
		`{"role":"user","content":"Now expand each risk into a paragraph and add a mitigation recommendation."}]}`)
	target := provcore.CallTarget{ProviderModelID: "claude-opus-4-6", MaxOutputTokens: 8192}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, body, target, false); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAnthropicRewriteNative_Coercion measures the D3 strip path (rejecting
// family, three sampling params removed) — the branch that turns a prod 400 into
// a coerce.
func BenchmarkAnthropicRewriteNative_Coercion(b *testing.B) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":256,"temperature":0.7,"top_p":0.9,"top_k":40,"messages":[{"role":"user","content":"hi"}]}`)
	target := provcore.CallTarget{ProviderModelID: "claude-opus-4-8", MaxOutputTokens: 8192}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, body, target, false); err != nil {
			b.Fatal(err)
		}
	}
}

func TestAnthropicRewriteNative_SameModelZeroCopy(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":16,"messages":[]}`)
	res, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "claude-opus-4-8"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if &res.Body[0] != &body[0] {
		t.Fatal("matching model must return the same slice")
	}
}

func TestAnthropicRewriteNative_Idempotent(t *testing.T) {
	target := provcore.CallTarget{ProviderModelID: "claude-opus-4-8"}
	first, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages,
		[]byte(`{"model":"alias","max_tokens":16,"messages":[]}`), target, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCodec().RewriteNative(typology.WireShapeAnthropicMessages, first.Body, target, false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Body, second.Body) {
		t.Fatalf("not idempotent:\n first=%s\nsecond=%s", first.Body, second.Body)
	}
}
