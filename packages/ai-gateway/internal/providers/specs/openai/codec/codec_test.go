// Package codec_test validates the OpenAI identity SchemaCodec behavior
// with the real OpenAI wire contract. Named failure modes per
// provider-adapter-architecture.md §3a:
//   - Rule 1: no structural translation (canonical OpenAI shape is the bus)
//   - Rule 3: per-model wire rules ride the contract into both entry points
//   - Rule 8: DecodeResponse delegates Usage extraction via provcore.ExtractUsage
package codec_test

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/rewrites"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// openaiCodec is the production wiring: the identity codec carrying the
// real OpenAI contract, exactly as specs/openai/spec.go constructs it.
func openaiCodec() provcore.SchemaCodec { return codec.New(rewrites.OpenAIContract()) }

// TestIdentityCodec_EncodeRequest_responses_rewritesModel: the codec owns
// the /v1/responses model stamp (encodeResponsesNative behind both entry
// points), so an aliased catalog model must ship the resolved upstream
// ProviderModelID, never the alias.
func TestIdentityCodec_EncodeRequest_responses_rewritesModel(t *testing.T) {
	c := openaiCodec()
	input := []byte(`{"model":"my-catalog-alias","input":"hi","max_output_tokens":16}`)
	target := provcore.CallTarget{ProviderModelID: "gpt-4o"}
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIResponses, input, target)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if got := gjson.GetBytes(encRes.Body, "model").String(); got != "gpt-4o" {
		t.Errorf("responses model must be rewritten to ProviderModelID; got %q body=%s", got, encRes.Body)
	}
	if !gjson.GetBytes(encRes.Body, "input").Exists() {
		t.Errorf("responses body must stay responses-shape (keep input); got %s", encRes.Body)
	}
}

// TestIdentityCodec_EncodeRequest_responses_stripsReasoningSamplingParams is
// the /v1/responses half of the reasoning-model sampling quirk: both wires
// draw the strip from the one contract, applied by the codec that talks to
// this wire (provider-adapter-architecture.md §3a Rule 3).
//
// Observed (2026-07, full --all-ingress smoke against api.openai.com):
// gpt-5.6-luna / -sol / -terra on /v1/responses answer 400 "Unsupported
// parameter: 'temperature' is not supported with this model", while the
// identical body and model on /v1/chat/completions answer 200.
func TestIdentityCodec_EncodeRequest_responses_stripsReasoningSamplingParams(t *testing.T) {
	c := openaiCodec()
	input := []byte(`{"model":"gpt-5.6-luna","input":"hi","temperature":0.7,"top_p":0.9,"max_output_tokens":16}`)
	target := provcore.CallTarget{ProviderModelID: "gpt-5.6-luna"}
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIResponses, input, target)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "temperature").Exists() {
		t.Errorf("reasoning model on /v1/responses must not carry temperature (upstream 400s); got %s", encRes.Body)
	}
	if gjson.GetBytes(encRes.Body, "top_p").Exists() {
		t.Errorf("reasoning model on /v1/responses must not carry top_p (upstream 400s); got %s", encRes.Body)
	}
	// The strip must be reported so x-nexus-coerced tells the caller what the
	// upstream actually saw — a silent strip is indistinguishable from the
	// caller's temperature having been honoured.
	if len(encRes.Rewrites) != 2 {
		t.Errorf("both strips must be reported as rewrites; got %v", encRes.Rewrites)
	}
	// Responses-shape fields must survive untouched.
	if got := gjson.GetBytes(encRes.Body, "max_output_tokens").Int(); got != 16 {
		t.Errorf("max_output_tokens must survive; got %d body=%s", got, encRes.Body)
	}
	if !gjson.GetBytes(encRes.Body, "input").Exists() {
		t.Errorf("responses body must stay responses-shape (keep input); got %s", encRes.Body)
	}
}

// TestIdentityCodec_EncodeRequest_responses_nonReasoningKeepsSamplingParams pins
// the other side of the gate: a non-reasoning model on /v1/responses honours the
// caller's temperature. Stripping unconditionally would silently change output
// for every gpt-4o caller on this wire.
func TestIdentityCodec_EncodeRequest_responses_nonReasoningKeepsSamplingParams(t *testing.T) {
	c := openaiCodec()
	input := []byte(`{"model":"gpt-4o","input":"hi","temperature":0.7,"top_p":0.9}`)
	target := provcore.CallTarget{ProviderModelID: "gpt-4o"}
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIResponses, input, target)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "temperature").Float() != 0.7 {
		t.Errorf("non-reasoning model must keep the caller's temperature; got %s", encRes.Body)
	}
	if gjson.GetBytes(encRes.Body, "top_p").Float() != 0.9 {
		t.Errorf("non-reasoning model must keep the caller's top_p; got %s", encRes.Body)
	}
	if len(encRes.Rewrites) != 0 {
		t.Errorf("no rewrite must be reported when nothing was stripped; got %v", encRes.Rewrites)
	}
}

// TestIdentityCodec_Gpt54Boundary pins the probed gpt-5.4 exemption on
// every wire that carries sampling params: gpt-5.4 accepts temperature
// (probed 2026-07-16, 200 on both chat and /v1/responses) but still
// requires the max_tokens rename (probed the same day, 400 "Use
// 'max_completion_tokens' instead"). Its siblings gpt-5.5 / gpt-5.6 keep
// the full strip. Stripping gpt-5.4's temperature was the over-strip this
// boundary split removes: no 400 risk, but the caller silently lost
// sampling control.
func TestIdentityCodec_Gpt54Boundary(t *testing.T) {
	c := openaiCodec()

	t.Run("chat: temperature kept, max_tokens renamed", func(t *testing.T) {
		input := []byte(`{"model":"gpt-5.4","messages":[],"temperature":0,"max_tokens":64}`)
		res, err := c.EncodeRequest(typology.WireShapeOpenAIChat, input, provcore.CallTarget{ProviderModelID: "gpt-5.4"})
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "temperature").Raw != "0" {
			t.Errorf("gpt-5.4 accepts temperature (probed 200); it must survive: %s", res.Body)
		}
		if gjson.GetBytes(res.Body, "max_tokens").Exists() || gjson.GetBytes(res.Body, "max_completion_tokens").Int() != 64 {
			t.Errorf("the max_tokens rename still applies to gpt-5.4: %s", res.Body)
		}
		if len(res.Rewrites) != 1 || res.Rewrites[0] != "max_tokens→max_completion_tokens" {
			t.Errorf("only the rename is reported: %v", res.Rewrites)
		}
	})

	t.Run("responses: temperature kept", func(t *testing.T) {
		input := []byte(`{"model":"gpt-5.4","input":"hi","temperature":0}`)
		res, err := c.EncodeRequest(typology.WireShapeOpenAIResponses, input, provcore.CallTarget{ProviderModelID: "gpt-5.4"})
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "temperature").Raw != "0" {
			t.Errorf("gpt-5.4 accepts temperature on /v1/responses too: %s", res.Body)
		}
		if len(res.Rewrites) != 0 {
			t.Errorf("nothing to report: %v", res.Rewrites)
		}
	})

	t.Run("siblings gpt-5.5 and o3 keep the full strip", func(t *testing.T) {
		for _, model := range []string{"gpt-5.5", "o3"} {
			input := []byte(`{"model":"` + model + `","messages":[],"temperature":0,"top_p":0.9,"max_tokens":64}`)
			res, err := c.EncodeRequest(typology.WireShapeOpenAIChat, input, provcore.CallTarget{ProviderModelID: model})
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(res.Body, "temperature").Exists() || gjson.GetBytes(res.Body, "top_p").Exists() {
				t.Errorf("%s: sampling params must be stripped (probed 400): %s", model, res.Body)
			}
			if gjson.GetBytes(res.Body, "max_completion_tokens").Int() != 64 {
				t.Errorf("%s: rename must land: %s", model, res.Body)
			}
		}
	})

	t.Run("unprobed future gpt-5.x fails safe (stripped)", func(t *testing.T) {
		input := []byte(`{"model":"gpt-5.9","messages":[],"temperature":0}`)
		res, err := c.EncodeRequest(typology.WireShapeOpenAIChat, input, provcore.CallTarget{ProviderModelID: "gpt-5.9"})
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "temperature").Exists() {
			t.Errorf("an unprobed gpt-5.x family must default to the strip (fail safe): %s", res.Body)
		}
	})
}

func TestIdentityCodec_EncodeRequest_responses_noProviderModelID_noop(t *testing.T) {
	c := openaiCodec()
	input := []byte(`{"model":"gpt-4o","input":"hi"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIResponses, input, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if string(encRes.Body) != string(input) {
		t.Errorf("no ProviderModelID → identity; got %s", encRes.Body)
	}
}

func TestIdentityCodec_EncodeRequest_chat_emptyTarget_isNoop(t *testing.T) {
	c := openaiCodec()
	input := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIChat, input, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if string(encRes.Body) != string(input) {
		t.Errorf("no ProviderModelID → nothing to stamp: got %q, want %q", encRes.Body, input)
	}
	if len(encRes.Rewrites) != 0 {
		t.Errorf("no rewrites expected: got %v", encRes.Rewrites)
	}
}

// TestIdentityCodec_EncodeRequest_chat_stampsResolvedModel pins the chat
// door's half of two-doors-one-body: the canonical bridge does NOT stamp
// the resolved model for OpenAI-family ingress, so this door owes the
// stamp exactly as RewriteNative does. (Before the contract migration this
// door was a bare identity and the stamp gap was masked by the dispatch
// map path.)
func TestIdentityCodec_EncodeRequest_chat_stampsResolvedModel(t *testing.T) {
	c := openaiCodec()
	input := []byte(`{"model":"my-alias","messages":[{"role":"user","content":"hi"}]}`)
	target := provcore.CallTarget{ProviderModelID: "gpt-4o"}
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIChat, input, target)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if got := gjson.GetBytes(encRes.Body, "model").String(); got != "gpt-4o" {
		t.Errorf("chat door must stamp the resolved model; got %q", got)
	}
	if len(encRes.Rewrites) != 0 {
		t.Errorf("the stamp is not a coercion: %v", encRes.Rewrites)
	}
}

func TestIdentityCodec_DecodeResponse_returnsBodyAndUsage(t *testing.T) {
	// Rule 8: DecodeResponse extracts Usage via shared/normalize path
	// and returns the body unchanged (identity).
	c := openaiCodec()
	body := []byte(`{
		"id":"chatcmpl-abc",
		"model":"gpt-4o",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("DecodeResponse body must be identity")
	}
	// Usage must be populated via shared/normalize (not zero).
	if usage.PromptTokens == nil || *usage.PromptTokens != 10 {
		t.Errorf("PromptTokens: got %v, want 10", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 5 {
		t.Errorf("CompletionTokens: got %v, want 5", usage.CompletionTokens)
	}
	if usage.TotalTokens == nil || *usage.TotalTokens != 15 {
		t.Errorf("TotalTokens: got %v, want 15", usage.TotalTokens)
	}
}

func TestIdentityCodec_DecodeResponse_cacheAliasChain(t *testing.T) {
	// Kimi K2 flat cached_tokens alias — verifies shared/normalize alias chain
	// is reachable through the identity codec.
	c := codec.New(codec.Contract{})
	body := []byte(`{
		"model":"kimi-k2",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"cached_tokens":600}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if usage.CacheReadTokens == nil || *usage.CacheReadTokens != 600 {
		t.Errorf("CacheReadTokens via kimi alias: got %v, want 600", usage.CacheReadTokens)
	}
}

func TestIdentityCodec_DecodeResponse_emptyBody_returnsZeroUsage(t *testing.T) {
	c := codec.New(codec.Contract{})
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIChat, []byte{}, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty body: expected empty out, got %q", out)
	}
	// Zero-value usage for empty body.
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		t.Errorf("expected zero Usage for empty body")
	}
}

func TestErrorNormalizerInstance_returnsNonNil(t *testing.T) {
	// Smoke: the exported factory returns a usable normalizer.
	n := codec.ErrorNormalizerInstance()
	if n == nil {
		t.Fatal("ErrorNormalizerInstance returned nil")
	}
}

// TestIdentityCodec_Gpt56ToolsEffortBoundary pins the probed
// tools×reasoning_effort quirk (2026-07-17, api.openai.com; first seen as
// a prod 400): gpt-5.6 with a non-empty tools[] rejects the request for
// ANY reasoning_effort other than the explicit "none" — including the
// field being absent — so the gateway forces "none" and reports it. The
// rule must not touch tool-less bodies, empty tools arrays, other
// families, or the /v1/responses wire (where the vendor accepts tools).
func TestIdentityCodec_Gpt56ToolsEffortBoundary(t *testing.T) {
	c := openaiCodec()
	tools := `"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object"}}}]`
	target := provcore.CallTarget{ProviderModelID: "gpt-5.6-terra"}
	wantLabel := "reasoning_effort→none (function tools on the chat wire)"

	t.Run("tools + high → forced none, reported", func(t *testing.T) {
		res, err := c.EncodeRequest(typology.WireShapeOpenAIChat,
			[]byte(`{"model":"gpt-5.6-terra","messages":[],`+tools+`,"reasoning_effort":"high"}`), target)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "reasoning_effort").String() != "none" {
			t.Fatalf("effort must be forced to the one accepted value: %s", res.Body)
		}
		found := false
		for _, r := range res.Rewrites {
			if r == wantLabel {
				found = true
			}
		}
		if !found {
			t.Fatalf("the coercion must be reported: %v", res.Rewrites)
		}
	})

	t.Run("tools + absent effort → injected (absence 400s upstream)", func(t *testing.T) {
		res, err := c.EncodeRequest(typology.WireShapeOpenAIChat,
			[]byte(`{"model":"gpt-5.6-terra","messages":[],`+tools+`}`), target)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "reasoning_effort").String() != "none" {
			t.Fatalf("the absent field must be injected — the family default is itself rejected: %s", res.Body)
		}
	})

	t.Run("native door agrees (two doors, one body)", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.6-terra","messages":[],` + tools + `,"reasoning_effort":"low"}`)
		viaRewrite, err := c.RewriteNative(typology.WireShapeOpenAIChat, body, target, false)
		if err != nil {
			t.Fatal(err)
		}
		viaEncode, err := c.EncodeRequest(typology.WireShapeOpenAIChat, body, target)
		if err != nil {
			t.Fatal(err)
		}
		if string(viaRewrite.Body) != string(viaEncode.Body) {
			t.Fatalf("doors diverge:\n rewrite=%s\n  encode=%s", viaRewrite.Body, viaEncode.Body)
		}
	})

	t.Run("streaming quirk body gets the same forcing", func(t *testing.T) {
		res, err := c.RewriteNative(typology.WireShapeOpenAIChat,
			[]byte(`{"model":"gpt-5.6-terra","stream":true,"stream_options":{"include_usage":true},`+tools+`,"messages":[]}`),
			target, true)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "reasoning_effort").String() != "none" {
			t.Fatalf("streaming parity (§3a Rule 6): %s", res.Body)
		}
	})

	t.Run("no tools → untouched", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.6-terra","messages":[],"reasoning_effort":"high"}`)
		res, err := c.RewriteNative(typology.WireShapeOpenAIChat, body, target, false)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "reasoning_effort").String() != "high" {
			t.Fatalf("tool-less traffic keeps its effort (probed 200): %s", res.Body)
		}
	})

	t.Run("empty tools array → untouched", func(t *testing.T) {
		res, err := c.RewriteNative(typology.WireShapeOpenAIChat,
			[]byte(`{"model":"gpt-5.6-terra","messages":[],"tools":[]}`), target, false)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "reasoning_effort").Exists() {
			t.Fatalf("empty tools answers 200 without the field (probed): %s", res.Body)
		}
	})

	t.Run("gpt-5.5 with tools → untouched", func(t *testing.T) {
		res, err := c.RewriteNative(typology.WireShapeOpenAIChat,
			[]byte(`{"model":"gpt-5.5","messages":[],`+tools+`}`),
			provcore.CallTarget{ProviderModelID: "gpt-5.5"}, false)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "reasoning_effort").Exists() {
			t.Fatalf("gpt-5.5 accepts tools without the field (probed 200): %s", res.Body)
		}
	})

	t.Run("responses wire never gets the rule", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.6-terra","input":"x",` + tools + `,"reasoning":{"effort":"high"}}`)
		res, err := c.RewriteNative(typology.WireShapeOpenAIResponses, body, target, false)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "reasoning_effort").Exists() {
			t.Fatalf("the vendor points tool callers AT /v1/responses; nothing to force there: %s", res.Body)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.6-terra","messages":[],` + tools + `,"reasoning_effort":"high"}`)
		first, err := c.RewriteNative(typology.WireShapeOpenAIChat, body, target, false)
		if err != nil {
			t.Fatal(err)
		}
		second, err := c.RewriteNative(typology.WireShapeOpenAIChat, first.Body, target, false)
		if err != nil {
			t.Fatal(err)
		}
		if string(first.Body) != string(second.Body) || len(second.Rewrites) != 0 {
			t.Fatalf("second pass must be a no-op: %s %v", second.Body, second.Rewrites)
		}
	})
}
