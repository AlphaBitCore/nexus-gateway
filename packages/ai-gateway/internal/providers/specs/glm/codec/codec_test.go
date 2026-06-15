// Package codec_test — GLM embedding codec round-trip and error-path tests.
//
// Architecture references:
//   - docs/dev/architecture/provider-adapter-architecture.md §3a Rules 1-7
//
// Tests assert OBSERVABLE behavior and NAMED FAILURE MODES. Coverage target
// is ≥95% per the unit-test-coverage-95 binding.
package codec_test

import (
	"errors"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	glmcodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/glm/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// ── EncodeRequest — embeddings ───────────────────────────────────────────────

// TestGLMCodec_Embeddings_StringInput verifies a bare string "input" passes
// through unchanged (GLM accepts string inputs).
func TestGLMCodec_Embeddings_StringInput(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{"model":"embedding-3","input":"hello world"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, provcore.CallTarget{ProviderModelID: "embedding-3"})
	if err != nil {
		t.Fatalf("EncodeRequest err=%v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("string input: body changed:\n got: %s\nwant: %s", encRes.Body, body)
	}
	if encRes.ContentType != "application/json" {
		t.Errorf("ContentType=%q want application/json", encRes.ContentType)
	}
	if len(encRes.Rewrites) != 0 {
		t.Errorf("no rewrites expected for string input, got %v", encRes.Rewrites)
	}
}

// TestGLMCodec_Embeddings_StringArrayInput verifies an array-of-strings input
// passes through unchanged (GLM accepts arrays of strings).
func TestGLMCodec_Embeddings_StringArrayInput(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{"model":"embedding-2","input":["foo","bar","baz"]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, provcore.CallTarget{ProviderModelID: "embedding-2"})
	if err != nil {
		t.Fatalf("EncodeRequest err=%v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("string array input: body changed:\n got: %s\nwant: %s", encRes.Body, body)
	}
}

// TestGLMCodec_Embeddings_TokenArrayRejected verifies that an integer token
// array input is rejected with a 400 ProviderError.
//
// GLM /api/paas/v4/embeddings does not support integer token inputs —
// observed 400 "invalid_request_error: token input not supported"
// (open.bigmodel.cn/api/paas/v4/embeddings, embedding-3, observed behavior).
func TestGLMCodec_Embeddings_TokenArrayRejected(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{"model":"embedding-3","input":[1234,5678,91011]}`)
	_, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, provcore.CallTarget{ProviderModelID: "embedding-3"})
	if err == nil {
		t.Fatal("expected error for token array input")
	}
	pe := &provcore.ProviderError{}
	ok := errors.As(err, &pe)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T: %v", err, err)
	}
	if pe.Status != 400 {
		t.Errorf("Status=%d want 400", pe.Status)
	}
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("Code=%q want %q", pe.Code, provcore.CodeInvalidRequest)
	}
	if !strings.Contains(pe.Message, "token_array_unsupported_by_glm") {
		t.Errorf("Message=%q want contain 'token_array_unsupported_by_glm'", pe.Message)
	}
	if !strings.Contains(pe.Message, "string inputs") {
		t.Errorf("Message=%q want contain 'string inputs'", pe.Message)
	}
}

// TestGLMCodec_Embeddings_BatchTokenArrayRejected verifies that an array-of-
// integer-arrays input is rejected (batch token format, also unsupported by GLM).
func TestGLMCodec_Embeddings_BatchTokenArrayRejected(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{"model":"embedding-3","input":[[1,2,3],[4,5,6]]}`)
	_, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for batch token array input")
	}
	pe := &provcore.ProviderError{}
	ok := errors.As(err, &pe)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T: %v", err, err)
	}
	if pe.Status != 400 {
		t.Errorf("Status=%d want 400", pe.Status)
	}
	if !strings.Contains(pe.Message, "token_array_unsupported_by_glm") {
		t.Errorf("Message=%q want contain 'token_array_unsupported_by_glm'", pe.Message)
	}
}

// TestGLMCodec_Embeddings_EmptyBody returns empty result without error (mirrors
// OpenAI identity codec behavior for pre-flight / OPTIONS paths).
func TestGLMCodec_Embeddings_EmptyBody(t *testing.T) {
	c := glmcodec.New()
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, nil, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("empty body: %v", err)
	}
	if len(encRes.Body) != 0 {
		t.Errorf("expected nil body for empty input, got %s", encRes.Body)
	}
	if encRes.ContentType != "application/json" {
		t.Errorf("ContentType=%q want application/json", encRes.ContentType)
	}
}

// TestGLMCodec_Embeddings_InvalidJSON returns a 400 ProviderError for
// malformed JSON input.
func TestGLMCodec_Embeddings_InvalidJSON(t *testing.T) {
	c := glmcodec.New()
	_, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, []byte(`{not json`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	pe := &provcore.ProviderError{}
	ok := errors.As(err, &pe)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if pe.Status != 400 {
		t.Errorf("Status=%d want 400", pe.Status)
	}
	if !strings.Contains(pe.Message, "invalid canonical JSON body") {
		t.Errorf("Message=%q want contain 'invalid canonical JSON body'", pe.Message)
	}
}

// TestGLMCodec_Embeddings_MissingInput passes through when "input" is missing;
// the upstream will return its own error. We do not inject a default.
func TestGLMCodec_Embeddings_MissingInput(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{"model":"embedding-3"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("missing input: expected pass-through, got err=%v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("missing input: body changed:\n got: %s\nwant: %s", encRes.Body, body)
	}
}

// TestGLMCodec_Embeddings_EmptyArray passes through an empty input array
// (GLM returns an error; we let upstream decide rather than pre-reject).
func TestGLMCodec_Embeddings_EmptyArray(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{"model":"embedding-3","input":[]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("empty array: expected pass-through, got err=%v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("empty array: body changed:\n got: %s\nwant: %s", encRes.Body, body)
	}
}

// ── EncodeRequest — other endpoints ─────────────────────────────────────────

// TestGLMCodec_ChatCompletions_IsIdentity pins the chat-completions pass-
// through — GLM chat wire shape equals canonical OpenAI shape.
func TestGLMCodec_ChatCompletions_IsIdentity(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{"model":"glm-4-plus","messages":[{"role":"user","content":"hi"}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIChat, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("chat completions must be identity")
	}
}

// TestGLMCodec_ResponsesAPI_IsIdentity pins the responses-api pass-through.
func TestGLMCodec_ResponsesAPI_IsIdentity(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{"model":"glm-4-plus","input":"hi"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIResponses, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("responses api: %v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("responses api must be identity")
	}
}

// TestGLMCodec_Models_IsIdentity pins the models pass-through.
func TestGLMCodec_Models_IsIdentity(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{}`)
	encRes, err := c.EncodeRequest(typology.WireShapeNone, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("models must be identity")
	}
}

// TestGLMCodec_CompletionsLegacy_IsIdentity pins the legacy-completions
// pass-through.
func TestGLMCodec_CompletionsLegacy_IsIdentity(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{"model":"glm-4","prompt":"hi"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAICompletionsLegacy, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("completions legacy: %v", err)
	}
	if string(encRes.Body) != string(body) {
		t.Errorf("completions legacy must be identity")
	}
}

// TestGLMCodec_UnsupportedEndpoint verifies that an unknown endpoint returns
// an error rather than silently passing the body.
func TestGLMCodec_UnsupportedEndpoint(t *testing.T) {
	c := glmcodec.New()
	_, err := c.EncodeRequest(typology.WireShape("unknown_endpoint"), []byte(`{}`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for unsupported endpoint")
	}
	if !strings.Contains(err.Error(), "unsupported endpoint") {
		t.Errorf("err=%v want contain 'unsupported endpoint'", err)
	}
}

// ── DecodeResponse ───────────────────────────────────────────────────────────

// TestGLMCodec_DecodeResponse_EmbeddingIdentity verifies that an embedding
// response is forwarded byte-for-byte (GLM response == canonical OpenAI shape).
func TestGLMCodec_DecodeResponse_EmbeddingIdentity(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.1,0.2,0.3],"index":0}],
		"model":"embedding-3",
		"usage":{"prompt_tokens":5,"total_tokens":5}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIEmbeddings, body, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse err=%v", err)
	}
	if string(decRes.CanonicalBody) != string(body) {
		t.Errorf("embedding response must be identity:\n got: %s\nwant: %s", decRes.CanonicalBody, body)
	}
}

// TestGLMCodec_DecodeResponse_UsageExtracted verifies that token usage is
// extracted from the embedding response so the cost ledger is populated.
func TestGLMCodec_DecodeResponse_UsageExtracted(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],
		"model":"embedding-3",
		"usage":{"prompt_tokens":10,"total_tokens":10}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIEmbeddings, body, "", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse err=%v", err)
	}
	if decRes.Usage.PromptTokens == nil || *decRes.Usage.PromptTokens != 10 {
		t.Errorf("PromptTokens=%v want 10", decRes.Usage.PromptTokens)
	}
	if decRes.Usage.TotalTokens == nil || *decRes.Usage.TotalTokens != 10 {
		t.Errorf("TotalTokens=%v want 10", decRes.Usage.TotalTokens)
	}
}

// TestGLMCodec_DecodeResponse_ChatIdentity verifies that a chat response is
// forwarded byte-for-byte and usage is extracted.
func TestGLMCodec_DecodeResponse_ChatIdentity(t *testing.T) {
	c := glmcodec.New()
	body := []byte(`{
		"id":"chatcmpl-x","object":"chat.completion","model":"glm-4-plus",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse chat err=%v", err)
	}
	if string(decRes.CanonicalBody) != string(body) {
		t.Errorf("chat response must be identity")
	}
	if decRes.Usage.PromptTokens == nil || *decRes.Usage.PromptTokens != 5 {
		t.Errorf("PromptTokens=%v want 5", decRes.Usage.PromptTokens)
	}
	if decRes.Usage.CompletionTokens == nil || *decRes.Usage.CompletionTokens != 3 {
		t.Errorf("CompletionTokens=%v want 3", decRes.Usage.CompletionTokens)
	}
}

// ── Round-trip: encode → decode ──────────────────────────────────────────────

// TestGLMCodec_RoundTrip_StringInput verifies that a string-input embedding
// request encodes without mutation and the matching response decodes correctly.
func TestGLMCodec_RoundTrip_StringInput(t *testing.T) {
	c := glmcodec.New()

	// Encode: string input must pass through unchanged.
	reqBody := []byte(`{"model":"embedding-3","input":"round-trip text"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, reqBody, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest err=%v", err)
	}
	if string(encRes.Body) != string(reqBody) {
		t.Errorf("encode mutated body:\n got: %s\nwant: %s", encRes.Body, reqBody)
	}

	// Decode: embedding response must be identity with usage extracted.
	respBody := []byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.5,0.6],"index":0}],"model":"embedding-3","usage":{"prompt_tokens":4,"total_tokens":4}}`)
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIEmbeddings, respBody, "application/json", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse err=%v", err)
	}
	if string(decRes.CanonicalBody) != string(respBody) {
		t.Errorf("decode mutated body:\n got: %s\nwant: %s", decRes.CanonicalBody, respBody)
	}
	if decRes.Usage.PromptTokens == nil || *decRes.Usage.PromptTokens != 4 {
		t.Errorf("PromptTokens=%v want 4", decRes.Usage.PromptTokens)
	}

	// Sanity: the decoded canonical body carries the expected model and embedding.
	if m := gjson.GetBytes(decRes.CanonicalBody, "model").Str; m != "embedding-3" {
		t.Errorf("model=%q want embedding-3", m)
	}
	if dim := gjson.GetBytes(decRes.CanonicalBody, "data.0.embedding.#").Int(); dim != 2 {
		t.Errorf("embedding dim=%d want 2", dim)
	}
}

// TestGLMCodec_RoundTrip_StringArrayInput verifies that an array-of-strings
// request encodes without mutation and response decodes correctly.
func TestGLMCodec_RoundTrip_StringArrayInput(t *testing.T) {
	c := glmcodec.New()

	reqBody := []byte(`{"model":"embedding-2","input":["alpha","beta","gamma"]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeOpenAIEmbeddings, reqBody, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest err=%v", err)
	}
	if string(encRes.Body) != string(reqBody) {
		t.Errorf("string-array encode mutated body:\n got: %s\nwant: %s", encRes.Body, reqBody)
	}

	respBody := []byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0},{"object":"embedding","embedding":[0.2],"index":1},{"object":"embedding","embedding":[0.3],"index":2}],"model":"embedding-2","usage":{"prompt_tokens":6,"total_tokens":6}}`)
	decRes, err := c.DecodeResponse(typology.WireShapeOpenAIEmbeddings, respBody, "", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("DecodeResponse err=%v", err)
	}
	if decRes.Usage.TotalTokens == nil || *decRes.Usage.TotalTokens != 6 {
		t.Errorf("TotalTokens=%v want 6", decRes.Usage.TotalTokens)
	}
	dataLen := gjson.GetBytes(decRes.CanonicalBody, "data.#").Int()
	if dataLen != 3 {
		t.Errorf("data length=%d want 3", dataLen)
	}
}
