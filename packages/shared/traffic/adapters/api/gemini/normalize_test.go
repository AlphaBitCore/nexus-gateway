package gemini

import (
	"context"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestNormalize_GeminiEmbeddings_SingleRequest(t *testing.T) {
	body := []byte(`{"content":{"parts":[{"text":"hello world"}]},"model":"models/text-embedding-004"}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "gemini",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:embedContent",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIEmbedding {
		t.Errorf("Kind = %q, want ai-embedding", payload.Kind)
	}
	if len(payload.Inputs) == 0 {
		t.Fatal("Inputs empty, want [hello world]")
	}
	if payload.Inputs[0] != "hello world" {
		t.Errorf("Inputs[0] = %q, want hello world", payload.Inputs[0])
	}
}

func TestNormalize_GeminiEmbeddings_BatchRequest(t *testing.T) {
	body := []byte(`{"requests":[{"content":{"parts":[{"text":"foo"}]}},{"content":{"parts":[{"text":"bar"}]}}]}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "gemini",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:batchEmbedContents",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIEmbedding {
		t.Errorf("Kind = %q, want ai-embedding", payload.Kind)
	}
	if len(payload.Inputs) != 2 {
		t.Errorf("Inputs len = %d, want 2", len(payload.Inputs))
	}
}

func TestNormalize_GeminiEmbeddings_SingleResponse(t *testing.T) {
	body := []byte(`{"embedding":{"values":[0.1,0.2,0.3]}}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "gemini",
		Direction:    normalize.DirectionResponse,
		EndpointPath: "/v1beta/models/text-embedding-004:embedContent",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIEmbedding {
		t.Errorf("Kind = %q, want ai-embedding", payload.Kind)
	}
	// Vectors must NOT be stored.
	if len(payload.Inputs) != 0 {
		t.Error("Inputs must be nil on response side")
	}
}

func TestNormalize_GeminiChat_UnaffectedByEmbeddingDispatch(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"model":"gemini-1.5-pro"}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "gemini",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v1beta/models/gemini-1.5-pro:generateContent",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind = %q, want ai-chat for generateContent", payload.Kind)
	}
}

func TestIsGeminiEmbeddingPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1beta/models/text-embedding-004:embedContent", true},
		{"/v1/models/text-embedding-004:embedContent", true},
		{"/v1beta/models/text-embedding-004:batchEmbedContents", true},
		{"/v1beta/models/gemini-pro:generateContent", false},
		{"/v1/chat/completions", false},
		{"", false},
	}
	for _, c := range cases {
		got := isGeminiEmbeddingPath(c.path)
		if got != c.want {
			t.Errorf("isGeminiEmbeddingPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
