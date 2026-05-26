package vertex

import (
	"context"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestNormalize_VertexEmbeddings_Request(t *testing.T) {
	body := []byte(`{"content":{"parts":[{"text":"hello vertex"}]},"model":"text-embedding-005"}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "vertex",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v1/projects/my-proj/locations/us-central1/publishers/google/models/text-embedding-005:embedContent",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIEmbedding {
		t.Errorf("Kind = %q, want ai-embedding", payload.Kind)
	}
	if len(payload.Inputs) == 0 || payload.Inputs[0] != "hello vertex" {
		t.Errorf("Inputs = %v, want [hello vertex]", payload.Inputs)
	}
}

func TestNormalize_VertexEmbeddings_BatchRequest(t *testing.T) {
	body := []byte(`{"requests":[{"content":{"parts":[{"text":"a"}]}},{"content":{"parts":[{"text":"b"}]}}]}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "vertex",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v1/projects/p/locations/l/publishers/google/models/text-embedding-005:batchEmbedContents",
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

func TestNormalize_VertexEmbeddings_Response(t *testing.T) {
	body := []byte(`{"embedding":{"values":[0.1,0.2]}}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "vertex",
		Direction:    normalize.DirectionResponse,
		EndpointPath: "/v1/projects/p/locations/l/publishers/google/models/text-embedding-005:embedContent",
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
}

func TestNormalize_VertexChat_UnaffectedByEmbeddingDispatch(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "vertex",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/v1/projects/p/locations/l/publishers/google/models/gemini-1.5-pro:generateContent",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind = %q, want ai-chat for generateContent", payload.Kind)
	}
}

func TestIsVertexEmbeddingPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/projects/p/locations/l/publishers/google/models/text-embedding-005:embedContent", true},
		{"/v1/projects/p/locations/l/publishers/google/models/text-embedding-005:batchEmbedContents", true},
		{"/v1/projects/p/locations/l/publishers/google/models/gemini-pro:generateContent", false},
		{"", false},
	}
	for _, c := range cases {
		got := isVertexEmbeddingPath(c.path)
		if got != c.want {
			t.Errorf("isVertexEmbeddingPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
