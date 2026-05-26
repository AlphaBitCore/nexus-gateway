package codecs

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestOpenAIEmbeddings_ID(t *testing.T) {
	n := NewOpenAIEmbeddingsNormalizer()
	if n.ID() != "openai-embeddings" {
		t.Fatalf("ID() = %q, want openai-embeddings", n.ID())
	}
}

func TestOpenAIEmbeddings_Request_StringInput(t *testing.T) {
	body := `{"model":"text-embedding-3-small","input":"hello world"}`
	n := NewOpenAIEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v, want ai-embedding", got.Kind)
	}
	if got.Model != "text-embedding-3-small" {
		t.Errorf("model = %q", got.Model)
	}
	if len(got.Inputs) != 1 || got.Inputs[0] != "hello world" {
		t.Errorf("inputs = %v, want [hello world]", got.Inputs)
	}
	if got.Protocol != "openai-embeddings" {
		t.Errorf("protocol = %q", got.Protocol)
	}
	if got.Confidence == 0 {
		t.Errorf("confidence should be non-zero")
	}
	if got.DetectedSpec != "openai-embeddings" {
		t.Errorf("detectedSpec = %q", got.DetectedSpec)
	}
}

func TestOpenAIEmbeddings_Request_SliceStringInput(t *testing.T) {
	body := `{"model":"text-embedding-3-small","input":["foo","bar","baz"]}`
	n := NewOpenAIEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Inputs) != 3 || got.Inputs[0] != "foo" || got.Inputs[1] != "bar" || got.Inputs[2] != "baz" {
		t.Errorf("inputs = %v", got.Inputs)
	}
}

func TestOpenAIEmbeddings_Request_TokenArrayInput(t *testing.T) {
	// []int — token array; Inputs must be nil, RuleIDs must contain the marker.
	body := `{"model":"text-embedding-3-small","input":[1234,5678,91011]}`
	n := NewOpenAIEmbeddingsNormalizer()
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

func TestOpenAIEmbeddings_Request_BatchTokenArrayInput(t *testing.T) {
	// [][]int — batch token arrays; Inputs must be nil, RuleIDs set.
	body := `{"model":"text-embedding-3-small","input":[[1,2,3],[4,5,6]]}`
	n := NewOpenAIEmbeddingsNormalizer()
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

func TestOpenAIEmbeddings_Request_EmptyBody(t *testing.T) {
	n := NewOpenAIEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestOpenAIEmbeddings_Request_MalformedJSON(t *testing.T) {
	n := NewOpenAIEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{bad json`), core.Meta{Direction: core.DirectionRequest})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestOpenAIEmbeddings_Request_MissingInput(t *testing.T) {
	body := `{"model":"text-embedding-3-small"}`
	n := NewOpenAIEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for missing input, got %v", err)
	}
}

func TestOpenAIEmbeddings_Response_HappyPath(t *testing.T) {
	body := `{
		"object": "list",
		"data": [{"object": "embedding", "index": 0, "embedding": [0.1, 0.2, 0.3]}],
		"model": "text-embedding-3-small",
		"usage": {"prompt_tokens": 5, "total_tokens": 5}
	}`
	n := NewOpenAIEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if got.Model != "text-embedding-3-small" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Inputs != nil {
		t.Errorf("inputs should be nil on response side, got %v", got.Inputs)
	}
	if got.Usage == nil {
		t.Fatal("usage should be non-nil")
	}
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 5 {
		t.Errorf("promptTokens = %v", got.Usage.PromptTokens)
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 5 {
		t.Errorf("totalTokens = %v", got.Usage.TotalTokens)
	}
}

func TestOpenAIEmbeddings_Response_EmptyBody(t *testing.T) {
	n := NewOpenAIEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: core.DirectionResponse})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestOpenAIEmbeddings_Response_MalformedJSON(t *testing.T) {
	n := NewOpenAIEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`not json`), core.Meta{Direction: core.DirectionResponse})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestOpenAIEmbeddings_Response_MissingRequiredFields(t *testing.T) {
	// A body that parses as JSON but has no recognized response fields.
	body := `{"something_else": true}`
	n := NewOpenAIEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestOpenAIEmbeddings_UnsupportedDirection(t *testing.T) {
	n := NewOpenAIEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{}`), core.Meta{Direction: "invalid"})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for unknown direction, got %v", err)
	}
}

func TestOpenAIEmbeddings_Request_ModelFromMeta(t *testing.T) {
	// Model field absent from body — should fall back to meta.Model.
	body := `{"input":"test"}`
	n := NewOpenAIEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest, Model: "text-embedding-ada-002"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "text-embedding-ada-002" {
		t.Errorf("model = %q, want text-embedding-ada-002", got.Model)
	}
}

func TestOpenAIEmbeddings_Confidence_ReturnsNonZero(t *testing.T) {
	// Well-formed request should produce a confidence above the 0.70 threshold.
	body := `{"model":"text-embedding-3-small","input":"ping"}`
	n := NewOpenAIEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Confidence < 0.7 {
		t.Errorf("confidence = %v, expected >= 0.70", got.Confidence)
	}
}

// Round-trip: canonical→wire→canonical for openai-embeddings normalizer.
// These tests exercise BOTH request and response directions in sequence.

// TestRoundTrip_OpenAIRequest_String: single-string input round-trips without
// losing the text or the model.
func TestRoundTrip_OpenAIRequest_String(t *testing.T) {
	wire := []byte(`{"model":"text-embedding-3-small","input":"round-trip text"}`)
	n := NewOpenAIEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("request normalize: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v, want ai-embedding", got.Kind)
	}
	if got.Model != "text-embedding-3-small" {
		t.Errorf("model = %q", got.Model)
	}
	if len(got.Inputs) != 1 || got.Inputs[0] != "round-trip text" {
		t.Errorf("inputs = %v, want [round-trip text]", got.Inputs)
	}
	if got.Protocol != "openai-embeddings" {
		t.Errorf("protocol = %q", got.Protocol)
	}
	if got.DetectedSpec != "openai-embeddings" {
		t.Errorf("detectedSpec = %q", got.DetectedSpec)
	}

	// Rebuild wire from canonical fields and re-normalize — second pass must
	// yield the same result (stable canonical).
	wire2 := []byte(`{"model":"` + got.Model + `","input":"` + got.Inputs[0] + `"}`)
	got2, err := n.Normalize(context.Background(), wire2, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("second normalize: %v", err)
	}
	if got2.Model != got.Model || got2.Inputs[0] != got.Inputs[0] {
		t.Errorf("round-trip mismatch: first=%+v second=%+v", got, got2)
	}
}

// TestRoundTrip_OpenAIRequest_StringArray: array-of-strings input round-trips
// preserving all elements and their order.
func TestRoundTrip_OpenAIRequest_StringArray(t *testing.T) {
	wire := []byte(`{"model":"text-embedding-3-small","input":["x","y","z"]}`)
	n := NewOpenAIEmbeddingsNormalizer()
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

// TestRoundTrip_OpenAIResponse_Single: single-item response round-trips
// preserving model, usage, and the absence of inputs.
func TestRoundTrip_OpenAIResponse_Single(t *testing.T) {
	wire := []byte(`{
		"object": "list",
		"data": [{"object":"embedding","index":0,"embedding":[0.11,0.22]}],
		"model": "text-embedding-3-small",
		"usage": {"prompt_tokens": 3, "total_tokens": 3}
	}`)
	n := NewOpenAIEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("response normalize: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if got.Model != "text-embedding-3-small" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Inputs != nil {
		t.Errorf("inputs must be nil on response side, got %v", got.Inputs)
	}
	if got.Usage == nil {
		t.Fatal("usage must be non-nil")
	}
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 3 {
		t.Errorf("promptTokens = %v, want 3", got.Usage.PromptTokens)
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 3 {
		t.Errorf("totalTokens = %v, want 3", got.Usage.TotalTokens)
	}
}

// TestRoundTrip_OpenAIResponse_Batch: multi-item response round-trips
// preserving model and usage totals.
func TestRoundTrip_OpenAIResponse_Batch(t *testing.T) {
	wire := []byte(`{
		"object": "list",
		"data": [
			{"object":"embedding","index":0,"embedding":[0.1,0.2]},
			{"object":"embedding","index":1,"embedding":[0.3,0.4]},
			{"object":"embedding","index":2,"embedding":[0.5,0.6]}
		],
		"model": "text-embedding-ada-002",
		"usage": {"prompt_tokens": 15, "total_tokens": 15}
	}`)
	n := NewOpenAIEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("response normalize: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if got.Model != "text-embedding-ada-002" {
		t.Errorf("model = %q", got.Model)
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
