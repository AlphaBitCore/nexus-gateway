// Package codec_test validates the OpenAI identity SchemaCodec behavior.
// Named failure modes per provider-adapter-architecture.md §3a:
//   - Rule 1: EncodeRequest is a no-op (canonical OpenAI shape is the bus)
//   - Rule 8: DecodeResponse delegates Usage extraction via provcore.ExtractUsage
package codec_test

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
)

func TestIdentityCodec_EncodeRequest_isNoop(t *testing.T) {
	c := codec.IdentityCodec()
	input := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIChat, input, provcore.CallTarget{})
	out := encRes.Body
	rewrites := encRes.Rewrites
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if string(out) != string(input) {
		t.Errorf("EncodeRequest must be identity: got %q, want %q", out, input)
	}
	if len(rewrites) != 0 {
		t.Errorf("EncodeRequest must return no rewrites: got %v", rewrites)
	}
}

func TestIdentityCodec_EncodeRequest_withTarget_stillNoop(t *testing.T) {
	// Even when a provider model ID is set on the target, the identity
	// codec does not mutate the body — model rewriting on passthrough is
	// the specAdapter's job (Rule 1: canonical stays pure).
	c := codec.IdentityCodec()
	input := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	target := provcore.CallTarget{ProviderModelID: "gpt-5.4"}
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIChat, input, target)
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if string(out) != string(input) {
		t.Errorf("identity codec must not rewrite model: got %q", out)
	}
}

func TestIdentityCodec_DecodeResponse_returnsBodyAndUsage(t *testing.T) {
	// Rule 8: DecodeResponse extracts Usage via shared/normalize path
	// and returns the body unchanged (identity).
	c := codec.IdentityCodec()
	body := []byte(`{
		"id":"chatcmpl-abc",
		"model":"gpt-4o",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIChat, body, "")
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
	c := codec.IdentityCodec()
	body := []byte(`{
		"model":"kimi-k2",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"cached_tokens":600}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIChat, body, "")
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if usage.CacheReadTokens == nil || *usage.CacheReadTokens != 600 {
		t.Errorf("CacheReadTokens via kimi alias: got %v, want 600", usage.CacheReadTokens)
	}
}

func TestIdentityCodec_DecodeResponse_emptyBody_returnsZeroUsage(t *testing.T) {
	c := codec.IdentityCodec()
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIChat, []byte{}, "")
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
