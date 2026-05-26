package codecs

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// VoyageEmbeddingsNormalizer tests.

func TestVoyageEmbeddings_ID(t *testing.T) {
	n := NewVoyageEmbeddingsNormalizer()
	if n.ID() != "voyage-embeddings" {
		t.Fatalf("ID() = %q, want voyage-embeddings", n.ID())
	}
}

// Request direction.

func TestVoyageEmbeddings_Request_StringInput(t *testing.T) {
	body := []byte(`{"model":"voyage-3","input":"hello voyage"}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), body, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v, want ai-embedding", got.Kind)
	}
	if got.Model != "voyage-3" {
		t.Errorf("model = %q, want voyage-3", got.Model)
	}
	if len(got.Inputs) != 1 || got.Inputs[0] != "hello voyage" {
		t.Errorf("inputs = %v, want [hello voyage]", got.Inputs)
	}
	if got.Protocol != "voyage-embeddings" {
		t.Errorf("protocol = %q, want voyage-embeddings", got.Protocol)
	}
	if got.DetectedSpec != "voyage-embeddings" {
		t.Errorf("detectedSpec = %q, want voyage-embeddings", got.DetectedSpec)
	}
}

func TestVoyageEmbeddings_Request_ArrayInput(t *testing.T) {
	body := []byte(`{"model":"voyage-3","input":["alpha","beta","gamma"]}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), body, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Inputs) != 3 {
		t.Fatalf("inputs len = %d, want 3", len(got.Inputs))
	}
	for i, want := range []string{"alpha", "beta", "gamma"} {
		if got.Inputs[i] != want {
			t.Errorf("inputs[%d] = %q, want %q", i, got.Inputs[i], want)
		}
	}
}

func TestVoyageEmbeddings_Request_TokenArray(t *testing.T) {
	// Token arrays ([]int) are marked as binary_input_token_array.
	body := []byte(`{"model":"voyage-3","input":[100,200,300]}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), body, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Inputs != nil {
		t.Errorf("inputs must be nil for token array, got %v", got.Inputs)
	}
	found := false
	for _, rid := range got.RuleIDs {
		if rid == "binary_input_token_array" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("binary_input_token_array marker not found in RuleIDs: %v", got.RuleIDs)
	}
}

func TestVoyageEmbeddings_Request_WithInputType(t *testing.T) {
	body := []byte(`{"model":"voyage-3","input":"search query","input_type":"query"}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), body, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "voyage-3" {
		t.Errorf("model = %q", got.Model)
	}
	// Confidence should be higher with the optional input_type present.
	if got.Confidence < 0.7 {
		t.Errorf("confidence = %v, want >= 0.70 with optional field present", got.Confidence)
	}
}

func TestVoyageEmbeddings_Request_ModelFromMeta(t *testing.T) {
	// When the body has no model field, fall back to meta.Model.
	body := []byte(`{"input":"hello"}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), body, core.Meta{
		Direction: core.DirectionRequest,
		Model:     "voyage-3-lite",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "voyage-3-lite" {
		t.Errorf("model = %q, want voyage-3-lite (from meta)", got.Model)
	}
}

func TestVoyageEmbeddings_Request_EmptyBody(t *testing.T) {
	n := NewVoyageEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestVoyageEmbeddings_Request_MissingInput(t *testing.T) {
	body := []byte(`{"model":"voyage-3"}`)
	n := NewVoyageEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), body, core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for missing input, got %v", err)
	}
}

func TestVoyageEmbeddings_Request_MalformedJSON(t *testing.T) {
	n := NewVoyageEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{bad`), core.Meta{Direction: core.DirectionRequest})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// Response direction.

func TestVoyageEmbeddings_Response_HappyPath(t *testing.T) {
	body := []byte(`{
		"object": "list",
		"data": [{"object": "embedding", "embedding": [0.1, 0.2, 0.3], "index": 0}],
		"model": "voyage-3",
		"usage": {"total_tokens": 5}
	}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), body, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if got.Model != "voyage-3" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Inputs != nil {
		t.Errorf("inputs must be nil on response side, got %v", got.Inputs)
	}
	if got.Usage == nil {
		t.Fatal("usage must be non-nil")
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 5 {
		t.Errorf("totalTokens = %v, want 5", got.Usage.TotalTokens)
	}
	// PromptTokens mirrors total_tokens (Voyage has no prompt/completion split).
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 5 {
		t.Errorf("promptTokens = %v, want 5 (mirrors total_tokens)", got.Usage.PromptTokens)
	}
}

func TestVoyageEmbeddings_Response_Batch(t *testing.T) {
	body := []byte(`{
		"object": "list",
		"data": [
			{"object": "embedding", "embedding": [0.1, 0.2], "index": 0},
			{"object": "embedding", "embedding": [0.3, 0.4], "index": 1}
		],
		"model": "voyage-3",
		"usage": {"total_tokens": 8}
	}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), body, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Usage == nil {
		t.Fatal("usage must be non-nil")
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 8 {
		t.Errorf("totalTokens = %v, want 8", got.Usage.TotalTokens)
	}
}

func TestVoyageEmbeddings_Response_ZeroTokens(t *testing.T) {
	// total_tokens = 0 → Usage should be nil (no meaningful usage data).
	body := []byte(`{"object":"list","data":[],"model":"voyage-3","usage":{"total_tokens":0}}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), body, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Usage != nil {
		t.Errorf("usage should be nil for zero total_tokens, got %+v", got.Usage)
	}
}

func TestVoyageEmbeddings_Response_ModelFromMeta(t *testing.T) {
	body := []byte(`{"object":"list","data":[],"usage":{"total_tokens":2}}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), body, core.Meta{
		Direction: core.DirectionResponse,
		Model:     "voyage-3-lite",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "voyage-3-lite" {
		t.Errorf("model = %q, want voyage-3-lite (from meta)", got.Model)
	}
}

func TestVoyageEmbeddings_Response_MissingRequiredFields(t *testing.T) {
	body := []byte(`{"unrelated_field": true}`)
	n := NewVoyageEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), body, core.Meta{Direction: core.DirectionResponse})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestVoyageEmbeddings_Response_EmptyBody(t *testing.T) {
	n := NewVoyageEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: core.DirectionResponse})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestVoyageEmbeddings_Response_MalformedJSON(t *testing.T) {
	n := NewVoyageEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`not json`), core.Meta{Direction: core.DirectionResponse})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// Unsupported direction.

func TestVoyageEmbeddings_UnsupportedDirection(t *testing.T) {
	n := NewVoyageEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{}`), core.Meta{Direction: "bad"})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for unknown direction, got %v", err)
	}
}

func TestRoundTrip_VoyageRequest_String(t *testing.T) {
	wire := []byte(`{"model":"voyage-3","input":"round trip test"}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("request normalize: %v", err)
	}
	if got.Model != "voyage-3" {
		t.Errorf("model = %q, want voyage-3", got.Model)
	}
	if len(got.Inputs) != 1 || got.Inputs[0] != "round trip test" {
		t.Errorf("inputs = %v, want [round trip test]", got.Inputs)
	}
	// Rebuild and re-normalize.
	wire2 := []byte(`{"model":"voyage-3","input":"round trip test"}`)
	got2, err := n.Normalize(context.Background(), wire2, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("second normalize: %v", err)
	}
	if got2.Model != got.Model || got2.Inputs[0] != got.Inputs[0] {
		t.Errorf("round-trip mismatch: first=%+v second=%+v", got, got2)
	}
}

func TestRoundTrip_VoyageRequest_Array(t *testing.T) {
	wire := []byte(`{"model":"voyage-code-3","input":["func foo()","class Bar"]}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("request normalize: %v", err)
	}
	if len(got.Inputs) != 2 {
		t.Fatalf("inputs len = %d, want 2", len(got.Inputs))
	}
	if got.Inputs[0] != "func foo()" || got.Inputs[1] != "class Bar" {
		t.Errorf("inputs = %v", got.Inputs)
	}
}

func TestRoundTrip_VoyageResponse(t *testing.T) {
	wire := []byte(`{
		"object": "list",
		"data": [{"object":"embedding","embedding":[0.5,0.6,0.7],"index":0}],
		"model": "voyage-finance-2",
		"usage": {"total_tokens": 12}
	}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("response normalize: %v", err)
	}
	if got.Model != "voyage-finance-2" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Usage == nil || *got.Usage.TotalTokens != 12 {
		t.Errorf("totalTokens = %v, want 12", got.Usage)
	}
}

// Confidence scoring.

func TestVoyageEmbeddings_Confidence_Request(t *testing.T) {
	body := []byte(`{"model":"voyage-3","input":"test"}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), body, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Confidence < 0.7 {
		t.Errorf("confidence = %v, want >= 0.70 for fully-matched request", got.Confidence)
	}
}

func TestVoyageEmbeddings_Confidence_Response(t *testing.T) {
	body := []byte(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0.1],"index":0}],
		"model":"voyage-3",
		"usage":{"total_tokens":3}
	}`)
	n := NewVoyageEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), body, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Confidence < 0.7 {
		t.Errorf("confidence = %v, want >= 0.70 for fully-matched response", got.Confidence)
	}
}
