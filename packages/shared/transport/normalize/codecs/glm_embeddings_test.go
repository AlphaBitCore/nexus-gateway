package codecs

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestGLMEmbeddings_ID(t *testing.T) {
	n := NewGLMEmbeddingsNormalizer()
	if n.ID() != "glm-embeddings" {
		t.Fatalf("ID() = %q, want glm-embeddings", n.ID())
	}
}

func TestGLMEmbeddings_Request_StringInput(t *testing.T) {
	body := `{"model":"embedding-3","input":"hello world"}`
	n := NewGLMEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v, want ai-embedding", got.Kind)
	}
	if got.Model != "embedding-3" {
		t.Errorf("model = %q, want embedding-3", got.Model)
	}
	if len(got.Inputs) != 1 || got.Inputs[0] != "hello world" {
		t.Errorf("inputs = %v, want [hello world]", got.Inputs)
	}
	if got.Protocol != "glm-embeddings" {
		t.Errorf("protocol = %q, want glm-embeddings", got.Protocol)
	}
	if got.DetectedSpec != "glm-embeddings" {
		t.Errorf("detectedSpec = %q, want glm-embeddings", got.DetectedSpec)
	}
	if got.Confidence == 0 {
		t.Errorf("confidence should be non-zero for well-formed request")
	}
}

func TestGLMEmbeddings_Request_StringArrayInput(t *testing.T) {
	body := `{"model":"embedding-2","input":["foo","bar","baz"]}`
	n := NewGLMEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Inputs) != 3 || got.Inputs[0] != "foo" || got.Inputs[1] != "bar" || got.Inputs[2] != "baz" {
		t.Errorf("inputs = %v, want [foo bar baz]", got.Inputs)
	}
}

func TestGLMEmbeddings_Request_TokenArrayInput(t *testing.T) {
	// Token arrays are not supported by GLM; the normalizer records a
	// binary_input_token_array marker so audit consumers understand the omission.
	body := `{"model":"embedding-3","input":[1234,5678,91011]}`
	n := NewGLMEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Inputs) != 0 {
		t.Errorf("inputs should be nil for token array, got %v", got.Inputs)
	}
	if len(got.RuleIDs) == 0 || got.RuleIDs[0] != "binary_input_token_array" {
		t.Errorf("ruleIDs = %v, want [binary_input_token_array]", got.RuleIDs)
	}
}

func TestGLMEmbeddings_Request_BatchTokenArrayInput(t *testing.T) {
	body := `{"model":"embedding-3","input":[[1,2,3],[4,5,6]]}`
	n := NewGLMEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Inputs) != 0 {
		t.Errorf("inputs should be nil for batch token array, got %v", got.Inputs)
	}
	if len(got.RuleIDs) == 0 || got.RuleIDs[0] != "binary_input_token_array" {
		t.Errorf("ruleIDs = %v, want [binary_input_token_array]", got.RuleIDs)
	}
}

func TestGLMEmbeddings_Request_EmptyBody(t *testing.T) {
	n := NewGLMEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestGLMEmbeddings_Request_MalformedJSON(t *testing.T) {
	n := NewGLMEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{bad json`), core.Meta{Direction: core.DirectionRequest})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestGLMEmbeddings_Request_MissingInput(t *testing.T) {
	body := `{"model":"embedding-3"}`
	n := NewGLMEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for missing input, got %v", err)
	}
}

func TestGLMEmbeddings_Request_ModelFromMeta(t *testing.T) {
	// Model absent from body — should fall back to meta.Model.
	body := `{"input":"test"}`
	n := NewGLMEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest, Model: "embedding-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "embedding-2" {
		t.Errorf("model = %q, want embedding-2", got.Model)
	}
}

func TestGLMEmbeddings_Response_HappyPath(t *testing.T) {
	body := `{
		"object": "list",
		"data": [{"object": "embedding", "index": 0, "embedding": [0.1, 0.2, 0.3]}],
		"model": "embedding-3",
		"usage": {"prompt_tokens": 5, "total_tokens": 5}
	}`
	n := NewGLMEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v, want ai-embedding", got.Kind)
	}
	if got.Model != "embedding-3" {
		t.Errorf("model = %q, want embedding-3", got.Model)
	}
	if got.Inputs != nil {
		t.Errorf("inputs should be nil on response side, got %v", got.Inputs)
	}
	if got.Usage == nil {
		t.Fatal("usage should be non-nil")
	}
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 5 {
		t.Errorf("promptTokens = %v, want 5", got.Usage.PromptTokens)
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 5 {
		t.Errorf("totalTokens = %v, want 5", got.Usage.TotalTokens)
	}
}

func TestGLMEmbeddings_Response_EmptyBody(t *testing.T) {
	n := NewGLMEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: core.DirectionResponse})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestGLMEmbeddings_Response_MalformedJSON(t *testing.T) {
	n := NewGLMEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`not json`), core.Meta{Direction: core.DirectionResponse})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestGLMEmbeddings_Response_MissingRequiredFields(t *testing.T) {
	body := `{"something_else": true}`
	n := NewGLMEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestGLMEmbeddings_UnsupportedDirection(t *testing.T) {
	n := NewGLMEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{}`), core.Meta{Direction: "invalid"})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for unknown direction, got %v", err)
	}
}

func TestGLMEmbeddings_Confidence_ReturnsNonZero(t *testing.T) {
	body := `{"model":"embedding-3","input":"ping"}`
	n := NewGLMEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Confidence < 0.7 {
		t.Errorf("confidence = %v, expected >= 0.70 for well-formed request", got.Confidence)
	}
}

// TestRoundTrip_GLMEmbeddings_StringInput: single-string request then
// matching response produces consistent canonical fields.
func TestRoundTrip_GLMEmbeddings_StringInput(t *testing.T) {
	reqWire := []byte(`{"model":"embedding-3","input":"round-trip text"}`)
	n := NewGLMEmbeddingsNormalizer()

	// Normalize request.
	reqGot, err := n.Normalize(context.Background(), reqWire, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("request normalize: %v", err)
	}
	if reqGot.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v, want ai-embedding", reqGot.Kind)
	}
	if reqGot.Model != "embedding-3" {
		t.Errorf("model = %q, want embedding-3", reqGot.Model)
	}
	if len(reqGot.Inputs) != 1 || reqGot.Inputs[0] != "round-trip text" {
		t.Errorf("inputs = %v, want [round-trip text]", reqGot.Inputs)
	}

	// Rebuild wire from canonical fields and re-normalize — stable canonical.
	reqWire2 := []byte(`{"model":"` + reqGot.Model + `","input":"` + reqGot.Inputs[0] + `"}`)
	reqGot2, err := n.Normalize(context.Background(), reqWire2, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("second request normalize: %v", err)
	}
	if reqGot2.Model != reqGot.Model || reqGot2.Inputs[0] != reqGot.Inputs[0] {
		t.Errorf("round-trip mismatch: first=%+v second=%+v", reqGot, reqGot2)
	}

	// Normalize response.
	respWire := []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.11,0.22]}],"model":"embedding-3","usage":{"prompt_tokens":3,"total_tokens":3}}`)
	respGot, err := n.Normalize(context.Background(), respWire, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("response normalize: %v", err)
	}
	if respGot.Inputs != nil {
		t.Errorf("inputs must be nil on response side, got %v", respGot.Inputs)
	}
	if respGot.Usage == nil {
		t.Fatal("usage must be non-nil")
	}
	if respGot.Usage.PromptTokens == nil || *respGot.Usage.PromptTokens != 3 {
		t.Errorf("promptTokens = %v, want 3", respGot.Usage.PromptTokens)
	}
}

// TestRoundTrip_GLMEmbeddings_StringArrayInput: array-of-strings request
// round-trips preserving all elements and their order.
func TestRoundTrip_GLMEmbeddings_StringArrayInput(t *testing.T) {
	wire := []byte(`{"model":"embedding-2","input":["x","y","z"]}`)
	n := NewGLMEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("request normalize: %v", err)
	}
	if len(got.Inputs) != 3 {
		t.Fatalf("inputs len = %d, want 3", len(got.Inputs))
	}
	for i, want := range []string{"x", "y", "z"} {
		if got.Inputs[i] != want {
			t.Errorf("inputs[%d] = %q, want %q", i, got.Inputs[i], want)
		}
	}
}

// TestRoundTrip_GLMEmbeddings_Response_Batch: multi-item response round-trips
// preserving model and usage totals.
func TestRoundTrip_GLMEmbeddings_Response_Batch(t *testing.T) {
	wire := []byte(`{
		"object": "list",
		"data": [
			{"object":"embedding","index":0,"embedding":[0.1,0.2]},
			{"object":"embedding","index":1,"embedding":[0.3,0.4]},
			{"object":"embedding","index":2,"embedding":[0.5,0.6]}
		],
		"model": "embedding-2",
		"usage": {"prompt_tokens": 15, "total_tokens": 15}
	}`)
	n := NewGLMEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("response normalize: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v, want ai-embedding", got.Kind)
	}
	if got.Model != "embedding-2" {
		t.Errorf("model = %q, want embedding-2", got.Model)
	}
	if got.Usage == nil {
		t.Fatal("usage must be non-nil")
	}
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 15 {
		t.Errorf("promptTokens = %v, want 15", got.Usage.PromptTokens)
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 15 {
		t.Errorf("totalTokens = %v, want 15", got.Usage.TotalTokens)
	}
}
