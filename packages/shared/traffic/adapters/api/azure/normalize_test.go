package azure

import (
	"context"
	"errors"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestNormalize_RequestChatShape pins that an Azure-shaped (= OpenAI-Chat
// wire) request body claims Tier 1 via the openai-chat spec and stamps
// DetectedSpec = "azure-openai" — the per-adapter caller stamps the
// adapter ID directly (no "pattern:" prefix) so the analytics pipeline
// distinguishes Azure from generic openai-compat hits.
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "Hello Azure!"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "azure-openai",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/openai/deployments/gpt-4o/chat/completions",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind = %v, want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "azure-openai" {
		t.Errorf("DetectedSpec = %q, want azure-openai (no pattern: prefix for adapter caller)", payload.DetectedSpec)
	}
	if payload.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o", payload.Model)
	}
	if len(payload.Messages) < 1 {
		t.Fatalf("messages empty, want at least 1: %+v", payload.Messages)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence = %v, want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_ResponseNonStream pins response-side scoring against the
// openai-chat-nonstream spec listed in the AdapterSpecHint.
func TestNormalize_ResponseNonStream(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-abc",
		"object": "chat.completion",
		"model": "gpt-4o",
		"choices": [
			{"index": 0, "message": {"role": "assistant", "content": "Hi from Azure"}, "finish_reason": "stop"}
		],
		"usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "azure-openai",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind = %v, want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "azure-openai" {
		t.Errorf("DetectedSpec = %q, want azure-openai", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies that a body
// matching neither the request nor response specs returns
// ErrUnsupported so the Coordinator can fall through to Tier 2.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo": "bar", "baz": 42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "azure-openai",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

// TestNormalize_AzureEmbeddings_Request pins that an Azure embeddings
// request body correctly returns Kind=ai-embedding with Inputs populated.
func TestNormalize_AzureEmbeddings_Request(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hello azure"}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "azure-openai",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/openai/deployments/text-embedding-3-small/embeddings",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIEmbedding {
		t.Errorf("Kind = %q, want ai-embedding", payload.Kind)
	}
	if len(payload.Inputs) == 0 || payload.Inputs[0] != "hello azure" {
		t.Errorf("Inputs = %v, want [\"hello azure\"]", payload.Inputs)
	}
}

// TestNormalize_AzureEmbeddings_Response pins that an Azure embeddings
// response body correctly returns Kind=ai-embedding with Usage populated
// and Inputs nil.
func TestNormalize_AzureEmbeddings_Response(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"text-embedding-3-small","usage":{"prompt_tokens":3,"total_tokens":3}}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "azure-openai",
		Direction:    normalize.DirectionResponse,
		EndpointPath: "/openai/deployments/text-embedding-3-small/embeddings",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIEmbedding {
		t.Errorf("Kind = %q, want ai-embedding", payload.Kind)
	}
	if len(payload.Inputs) != 0 {
		t.Error("Inputs must be nil on response side (vectors not stored)")
	}
	if payload.Usage == nil || payload.Usage.PromptTokens == nil {
		t.Error("Usage.PromptTokens must be populated on response side")
	}
}

func TestIsAzureEmbeddingPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/openai/deployments/text-embedding-3-small/embeddings", true},
		{"/v1/embeddings", true},
		{"/openai/deployments/gpt-4o/chat/completions", false},
		{"", false},
	}
	for _, c := range cases {
		got := isAzureEmbeddingPath(c.path)
		if got != c.want {
			t.Errorf("isAzureEmbeddingPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
