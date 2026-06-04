package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestExtractIngressModel_OpenAI_FromBody(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI}
	body := []byte(`{"model":"gpt-4o","stream":true,"messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	model, stream, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", model)
	}
	if !stream {
		t.Errorf("stream = false, want true")
	}
}

func TestExtractIngressModel_Anthropic_FromBody(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeAnthropicMessages, BodyFormat: provcore.FormatAnthropic}
	body := []byte(`{"model":"claude-3-5-sonnet","messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	model, stream, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if model != "claude-3-5-sonnet" {
		t.Errorf("model = %q, want claude-3-5-sonnet", model)
	}
	if stream {
		t.Errorf("stream = true, want false")
	}
}

func TestExtractIngressModel_Gemini_FromPath_NonStreaming(t *testing.T) {
	in := Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatGemini,
	}
	body := []byte(`{"contents":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:generateContent", bytes.NewReader(body))
	req.SetPathValue("model", "gemini-1.5-pro:generateContent")

	model, stream, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if model != "gemini-1.5-pro" {
		t.Errorf("model = %q, want gemini-1.5-pro", model)
	}
	if stream {
		t.Errorf("stream = true, want false (non-streaming path)")
	}
}

func TestExtractIngressModel_Gemini_FromPath_Streaming(t *testing.T) {
	in := Ingress{
		WireShape:      typology.WireShapeOpenAIChat,
		BodyFormat:     provcore.FormatGemini,
		Stream:         true,
		StreamFromPath: true,
	}
	body := []byte(`{"contents":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:streamGenerateContent", bytes.NewReader(body))
	req.SetPathValue("model", "gemini-1.5-pro:streamGenerateContent")

	_, stream, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !stream {
		t.Errorf("stream = false, want true (streaming path)")
	}
}

func TestExtractIngressModel_Gemini_MissingPathValue(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeGeminiGenerateContent, BodyFormat: provcore.FormatGemini}
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/:generateContent", nil)

	if _, _, err := ExtractIngressModel(in, req, nil); err == nil {
		t.Fatalf("expected error for missing {model}, got nil")
	}
}

func TestExtractIngressModel_Gemini_InvalidPathSuffix(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeGeminiGenerateContent, BodyFormat: provcore.FormatGemini}
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:unknown", nil)
	req.SetPathValue("model", "gemini-1.5-pro:unknown")

	if _, _, err := ExtractIngressModel(in, req, nil); err == nil {
		t.Fatalf("expected error for invalid path suffix, got nil")
	}
}

func TestExtractIngressModel_Azure_FromPath(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatAzureOpenAI}
	body := []byte(`{"messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/openai/deployments/gpt-4-turbo/chat/completions", bytes.NewReader(body))
	req.SetPathValue("deployment", "gpt-4-turbo")

	model, _, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if model != "gpt-4-turbo" {
		t.Errorf("model = %q, want gpt-4-turbo", model)
	}
}

func TestExtractIngressModel_BedrockVertex_Rejected(t *testing.T) {
	for _, f := range []provcore.Format{provcore.FormatBedrock, provcore.FormatVertex} {
		in := Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: f}
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if _, _, err := ExtractIngressModel(in, req, nil); err == nil {
			t.Errorf("format %q: expected error, got nil", f)
		}
	}
}

// TestExtractIngressModel_Responses_FromBody pins the /v1/responses ingress
// model extraction. /v1/responses uses the same top-level `model` and
// `stream` fields as chat-completions (just with `input` instead of
// `messages` for the prompt — which we don't extract here). The pre-fix
// bug returned `unsupported ingress format "openai-responses"` in prod.
func TestExtractIngressModel_Responses_FromBody(t *testing.T) {
	in := Ingress{WireShape: typology.WireShapeOpenAIResponses, BodyFormat: provcore.FormatOpenAIResponses}
	body := []byte(`{"model":"gpt-5.2","input":"hi","stream":true,"max_output_tokens":10}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	model, stream, err := ExtractIngressModel(in, req, body)
	if err != nil {
		t.Fatalf("Responses ingress unexpectedly errored: %v", err)
	}
	if model != "gpt-5.2" {
		t.Errorf("model = %q, want gpt-5.2", model)
	}
	if !stream {
		t.Errorf("stream = false, want true (body said stream:true)")
	}

	// Non-stream variant.
	body2 := []byte(`{"model":"gpt-4o","input":"hello","max_output_tokens":5}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body2))
	model2, stream2, err := ExtractIngressModel(in, req2, body2)
	if err != nil {
		t.Fatalf("Responses non-stream errored: %v", err)
	}
	if model2 != "gpt-4o" || stream2 {
		t.Errorf("non-stream Responses: model=%q stream=%v (want gpt-4o, false)", model2, stream2)
	}
}
