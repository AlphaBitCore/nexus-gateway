// Package codec_test — embedding codec tests for the OpenAI IdentityCodec.
// Named failure modes per provider-adapter-architecture.md §3a:
//   - Rule 3: per-model wire quirks owned by the adapter
//   - Rule 7: source comments with empirical 400 citations
package codec_test

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// EncodeRequest embeddings

func TestIdentityCodec_EncodeRequest_embeddings_ada002_stripsFields(t *testing.T) {
	// ada-002 rejects dimensions and encoding_format — observed 400
	// "Unrecognized request argument supplied: dimensions" (OpenAI API, observed behavior).
	c := codec.IdentityCodec()
	body := []byte(`{"model":"text-embedding-ada-002","input":"hello","dimensions":256,"encoding_format":"float"}`)
	target := provcore.CallTarget{ProviderModelID: "text-embedding-ada-002"}
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, target)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	out := encRes.Body
	if gjson.GetBytes(out, "dimensions").Exists() {
		t.Errorf("ada-002: dimensions must be stripped: %s", out)
	}
	if gjson.GetBytes(out, "encoding_format").Exists() {
		t.Errorf("ada-002: encoding_format must be stripped: %s", out)
	}
	if gjson.GetBytes(out, "input").Str != "hello" {
		t.Errorf("input must be preserved: %s", out)
	}
	if gjson.GetBytes(out, "model").Str != "text-embedding-ada-002" {
		t.Errorf("model must be preserved: %s", out)
	}
	// Rewrites should mention both stripped fields.
	if len(encRes.Rewrites) != 2 {
		t.Errorf("expected 2 rewrites (dimensions, encoding_format), got %d: %v", len(encRes.Rewrites), encRes.Rewrites)
	}
}

func TestIdentityCodec_EncodeRequest_embeddings_ada002_noDimensions_noRewrites(t *testing.T) {
	// ada-002 without dimensions/encoding_format: no-op, no rewrites.
	c := codec.IdentityCodec()
	body := []byte(`{"model":"text-embedding-ada-002","input":"hello"}`)
	target := provcore.CallTarget{ProviderModelID: "text-embedding-ada-002"}
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, target)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("no-op: body changed unexpectedly: %s", encRes.Body)
	}
	if len(encRes.Rewrites) != 0 {
		t.Errorf("no rewrites expected: %v", encRes.Rewrites)
	}
}

func TestIdentityCodec_EncodeRequest_embeddings_ada002_via_target(t *testing.T) {
	// Target.ProviderModelID drives the model detection (not body model field).
	c := codec.IdentityCodec()
	body := []byte(`{"model":"alias-name","input":"hi","dimensions":128}`)
	target := provcore.CallTarget{ProviderModelID: "text-embedding-ada-002-v2"}
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, target)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "dimensions").Exists() {
		t.Errorf("ada-002 via target: dimensions should be stripped: %s", encRes.Body)
	}
}

func TestIdentityCodec_EncodeRequest_embeddings_textEmbedding3_passthrough(t *testing.T) {
	// text-embedding-3-* honours dimensions as-is.
	c := codec.IdentityCodec()
	body := []byte(`{"model":"text-embedding-3-small","input":"hi","dimensions":512,"encoding_format":"float"}`)
	target := provcore.CallTarget{ProviderModelID: "text-embedding-3-small"}
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, target)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "dimensions").Int() != 512 {
		t.Errorf("text-embedding-3: dimensions must be passed through: %s", encRes.Body)
	}
	if gjson.GetBytes(encRes.Body, "encoding_format").Str != "float" {
		t.Errorf("text-embedding-3: encoding_format must be passed through: %s", encRes.Body)
	}
	if len(encRes.Rewrites) != 0 {
		t.Errorf("text-embedding-3: no rewrites expected: %v", encRes.Rewrites)
	}
}

func TestIdentityCodec_EncodeRequest_embeddings_safetyNet_negativeDimensions(t *testing.T) {
	// Safety-net: dimensions must be a positive integer.
	c := codec.IdentityCodec()
	body := []byte(`{"model":"text-embedding-3-large","input":"hi","dimensions":-1}`)
	target := provcore.CallTarget{ProviderModelID: "text-embedding-3-large"}
	_, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, target)
	if err == nil {
		t.Fatal("expected error for negative dimensions")
	}
	if !strings.Contains(err.Error(), "dimensions") {
		t.Errorf("error should mention dimensions: %v", err)
	}
}

func TestIdentityCodec_EncodeRequest_embeddings_safetyNet_zeroDimensions(t *testing.T) {
	// Safety-net: dimensions=0 is invalid.
	c := codec.IdentityCodec()
	body := []byte(`{"model":"text-embedding-3-small","input":"hi","dimensions":0}`)
	target := provcore.CallTarget{ProviderModelID: "text-embedding-3-small"}
	_, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, target)
	if err == nil {
		t.Fatal("expected error for zero dimensions")
	}
}

func TestIdentityCodec_EncodeRequest_embeddings_invalidJSON(t *testing.T) {
	c := codec.IdentityCodec()
	_, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, []byte(`{not json`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestIdentityCodec_EncodeRequest_embeddings_emptyBody_ok(t *testing.T) {
	// Empty body returns empty result (nil body, no error).
	c := codec.IdentityCodec()
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, nil, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("empty body: %v", err)
	}
	if len(encRes.Body) != 0 {
		t.Errorf("expected empty body result: %s", encRes.Body)
	}
}

func TestIdentityCodec_EncodeRequest_embeddings_modelFallbackFromBody(t *testing.T) {
	// When ProviderModelID is empty, model comes from the body field.
	c := codec.IdentityCodec()
	body := []byte(`{"model":"text-embedding-ada-002","input":"hi","dimensions":256}`)
	// No ProviderModelID set — should still detect ada-002 from body.
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(encRes.Body, "dimensions").Exists() {
		t.Errorf("dimensions should be stripped when model detected from body: %s", encRes.Body)
	}
}

func TestIdentityCodec_EncodeRequest_embeddings_unsupportedEndpoint_returnsError(t *testing.T) {
	c := codec.IdentityCodec()
	_, err := c.EncodeRequest(typology.WireShape("unknown_endpoint"), []byte(`{}`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for unsupported endpoint")
	}
	if !strings.Contains(err.Error(), "unsupported endpoint") {
		t.Errorf("expected unsupported endpoint message: %v", err)
	}
}

func TestIdentityCodec_EncodeRequest_embeddings_contentType(t *testing.T) {
	c := codec.IdentityCodec()
	body := []byte(`{"model":"text-embedding-3-small","input":"hi"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if encRes.ContentType != "application/json" {
		t.Errorf("ContentType: got %q, want application/json", encRes.ContentType)
	}
}

// DecodeResponse embeddings

func TestIdentityCodec_DecodeResponse_embeddings_isIdentity(t *testing.T) {
	// OpenAI embedding response IS the canonical shape — decode is identity.
	c := codec.IdentityCodec()
	body := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.1,0.2,0.3],"index":0}],
		"model":"text-embedding-3-small",
		"usage":{"prompt_tokens":5,"total_tokens":5}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIEmbeddings, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if string(decRes.CanonicalBody) != string(body) {
		t.Errorf("DecodeResponse: embedding response must be identity (no transform)")
	}
}

func TestIdentityCodec_DecodeResponse_embeddings_usageExtracted(t *testing.T) {
	// Usage must be extracted even for embedding responses.
	c := codec.IdentityCodec()
	body := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],
		"model":"text-embedding-3-small",
		"usage":{"prompt_tokens":10,"total_tokens":10}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIEmbeddings, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if decRes.Usage.PromptTokens == nil || *decRes.Usage.PromptTokens != 10 {
		t.Errorf("PromptTokens: got %v, want 10", decRes.Usage.PromptTokens)
	}
	if decRes.Usage.TotalTokens == nil || *decRes.Usage.TotalTokens != 10 {
		t.Errorf("TotalTokens: got %v, want 10", decRes.Usage.TotalTokens)
	}
}

// Endpoint dispatch coverage

func TestIdentityCodec_EncodeRequest_chatCompletions_isNoop(t *testing.T) {
	c := codec.IdentityCodec()
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIChat, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("chat completions must be identity")
	}
}

func TestIdentityCodec_EncodeRequest_responsesAPI_isNoop(t *testing.T) {
	c := codec.IdentityCodec()
	body := []byte(`{"model":"gpt-4o","input":"hi"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIResponses, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("responses api: %v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("responses api must be identity")
	}
}

func TestIdentityCodec_EncodeRequest_models_isNoop(t *testing.T) {
	c := codec.IdentityCodec()
	body := []byte(`{}`)
	encRes, err := c.EncodeRequest(typology.WireShapeNone, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("models must be identity")
	}
}

func TestIdentityCodec_EncodeRequest_completionsLegacy_isNoop(t *testing.T) {
	c := codec.IdentityCodec()
	body := []byte(`{"model":"gpt-3.5-turbo-instruct","prompt":"hi"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAICompletionsLegacy, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("completions legacy: %v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("completions legacy must be identity")
	}
}
