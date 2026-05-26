package openai

import (
	"context"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestNormalize_OpenAIEmbeddings_Request(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hello world"}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "openai-compat",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v1/embeddings",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIEmbedding {
		t.Errorf("Kind = %q, want %q", payload.Kind, normalize.KindAIEmbedding)
	}
	if len(payload.Inputs) == 0 {
		t.Error("Inputs empty, want at least 1 element")
	}
	if payload.Inputs[0] != "hello world" {
		t.Errorf("Inputs[0] = %q, want %q", payload.Inputs[0], "hello world")
	}
	if payload.Model != "text-embedding-3-small" {
		t.Errorf("Model = %q, want text-embedding-3-small", payload.Model)
	}
}

func TestNormalize_OpenAIEmbeddings_Request_Batch(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":["foo","bar","baz"]}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "openai-compat",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v1/embeddings",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIEmbedding {
		t.Errorf("Kind = %q, want ai-embedding", payload.Kind)
	}
	if len(payload.Inputs) != 3 {
		t.Errorf("Inputs len = %d, want 3", len(payload.Inputs))
	}
}

func TestNormalize_OpenAIEmbeddings_Response(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"text-embedding-3-small","usage":{"prompt_tokens":2,"total_tokens":2}}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "openai-compat",
		Direction:    normalize.DirectionResponse,
		EndpointPath: "/v1/embeddings",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIEmbedding {
		t.Errorf("Kind = %q, want ai-embedding", payload.Kind)
	}
	// Vectors must NOT be stored.
	if len(payload.Inputs) != 0 {
		t.Errorf("Inputs should be nil on response side, got %v", payload.Inputs)
	}
	if payload.Usage == nil {
		t.Fatal("Usage should be populated on response side")
	}
	if payload.Usage.PromptTokens == nil || *payload.Usage.PromptTokens != 2 {
		t.Errorf("PromptTokens = %v, want 2", payload.Usage.PromptTokens)
	}
}

func TestNormalize_OpenAIChat_UnaffectedByEmbeddingDispatch(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "openai-compat",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind = %q, want ai-chat", payload.Kind)
	}
}

func TestIsOpenAIEmbeddingPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/embeddings", true},
		{"/openai/deployments/gpt-4o/embeddings", true},
		{"/v1/chat/completions", false},
		{"/v1/embeddings/extra", false},
		{"", false},
	}
	for _, c := range cases {
		got := isOpenAIEmbeddingPath(c.path)
		if got != c.want {
			t.Errorf("isOpenAIEmbeddingPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
