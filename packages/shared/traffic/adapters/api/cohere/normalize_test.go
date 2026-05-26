package cohere

import (
	"context"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestNormalize_CohereEmbeddings_RequestV1(t *testing.T) {
	body := []byte(`{"model":"embed-english-v3.0","texts":["hello","world"],"input_type":"search_query"}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "cohere",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v1/embed",
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
	if payload.Inputs[0] != "hello" || payload.Inputs[1] != "world" {
		t.Errorf("Inputs = %v, want [hello world]", payload.Inputs)
	}
	if payload.Model != "embed-english-v3.0" {
		t.Errorf("Model = %q, want embed-english-v3.0", payload.Model)
	}
}

func TestNormalize_CohereEmbeddings_RequestV2(t *testing.T) {
	body := []byte(`{"model":"embed-v4.0","texts":["test text"],"input_type":"classification"}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "cohere",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v2/embed",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIEmbedding {
		t.Errorf("Kind = %q, want ai-embedding", payload.Kind)
	}
	if len(payload.Inputs) != 1 || payload.Inputs[0] != "test text" {
		t.Errorf("Inputs = %v, want [test text]", payload.Inputs)
	}
}

func TestNormalize_CohereEmbeddings_Response(t *testing.T) {
	body := []byte(`{"id":"abc","model":"embed-english-v3.0","embeddings":{"float":[[0.1,0.2]]},"meta":{"billed_units":{"input_tokens":5}},"response_type":"embeddings_floats"}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "cohere",
		Direction:    normalize.DirectionResponse,
		EndpointPath: "/v1/embed",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIEmbedding {
		t.Errorf("Kind = %q, want ai-embedding", payload.Kind)
	}
	if len(payload.Inputs) != 0 {
		t.Error("Inputs must be nil on response side")
	}
	if payload.Usage == nil || payload.Usage.PromptTokens == nil {
		t.Error("Usage.PromptTokens should be populated from billed_units")
	}
}

func TestNormalize_CohereChat_UnaffectedByEmbeddingDispatch(t *testing.T) {
	body := []byte(`{"model":"command-r-plus","messages":[{"role":"user","content":"hi"}]}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "cohere",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v2/chat",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	// Chat traffic should still be handled as ai-chat.
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind = %q, want ai-chat", payload.Kind)
	}
}

func TestIsCohereEmbeddingPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/embed", true},
		{"/v2/embed", true},
		{"/v2/chat", false},
		{"/v1/embeddings", false},
		{"", false},
	}
	for _, c := range cases {
		got := isCohereEmbeddingPath(c.path)
		if got != c.want {
			t.Errorf("isCohereEmbeddingPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
