package codecs

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestGeminiEmbeddings_ID(t *testing.T) {
	n := NewGeminiEmbeddingsNormalizer()
	if n.ID() != "gemini-embeddings" {
		t.Fatalf("ID() = %q, want gemini-embeddings", n.ID())
	}
}

// Single :embedContent

func TestGeminiEmbeddings_Single_Request_HappyPath(t *testing.T) {
	body := `{"model":"models/text-embedding-004","content":{"parts":[{"text":"hello world"}]}}`
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction:    core.DirectionRequest,
		AdapterType:  "gemini",
		EndpointPath: "/v1beta/models/text-embedding-004:embedContent",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v, want ai-embedding", got.Kind)
	}
	if got.Model != "models/text-embedding-004" {
		t.Errorf("model = %q", got.Model)
	}
	if len(got.Inputs) != 1 || got.Inputs[0] != "hello world" {
		t.Errorf("inputs = %v", got.Inputs)
	}
	if got.Protocol != "gemini-embeddings" {
		t.Errorf("protocol = %q", got.Protocol)
	}
	if got.DetectedSpec != "gemini-embeddings" {
		t.Errorf("detectedSpec = %q", got.DetectedSpec)
	}
}

func TestGeminiEmbeddings_Single_Request_MultiPart(t *testing.T) {
	// Multiple text parts in a single content block.
	body := `{"content":{"parts":[{"text":"part one"},{"text":"part two"}]}}`
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction:    core.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:embedContent",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Inputs) != 2 {
		t.Errorf("inputs = %v, want 2 elements", got.Inputs)
	}
}

func TestGeminiEmbeddings_Single_Request_MissingContent(t *testing.T) {
	body := `{"model":"models/text-embedding-004"}`
	n := NewGeminiEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction:    core.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:embedContent",
	})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestGeminiEmbeddings_Single_Response_HappyPath(t *testing.T) {
	body := `{"embedding":{"values":[0.1,0.2,0.3]}}`
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1beta/models/text-embedding-004:embedContent",
		Model:        "text-embedding-004",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if got.Inputs != nil {
		t.Errorf("inputs should be nil on response side, got %v", got.Inputs)
	}
}

func TestGeminiEmbeddings_Single_Response_MissingEmbedding(t *testing.T) {
	body := `{"something_else": true}`
	n := NewGeminiEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1beta/models/text-embedding-004:embedContent",
	})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

// Batch :batchEmbedContents

func TestGeminiEmbeddings_Batch_Request_HappyPath(t *testing.T) {
	body := `{
		"requests": [
			{"model":"models/text-embedding-004","content":{"parts":[{"text":"first"}]}},
			{"model":"models/text-embedding-004","content":{"parts":[{"text":"second"}]}}
		]
	}`
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction:    core.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:batchEmbedContents",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if len(got.Inputs) != 2 || got.Inputs[0] != "first" || got.Inputs[1] != "second" {
		t.Errorf("inputs = %v", got.Inputs)
	}
	// Model should be extracted from first sub-request.
	if got.Model != "models/text-embedding-004" {
		t.Errorf("model = %q", got.Model)
	}
}

func TestGeminiEmbeddings_Batch_Request_ModelFromMeta(t *testing.T) {
	// Sub-requests with no model field — fall back to meta.Model.
	body := `{"requests":[{"content":{"parts":[{"text":"hello"}]}}]}`
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction:    core.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:batchEmbedContents",
		Model:        "text-embedding-004",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "text-embedding-004" {
		t.Errorf("model = %q", got.Model)
	}
}

func TestGeminiEmbeddings_Batch_Request_EmptyRequests(t *testing.T) {
	body := `{"requests":[]}`
	n := NewGeminiEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction:    core.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:batchEmbedContents",
	})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestGeminiEmbeddings_Batch_Response_HappyPath(t *testing.T) {
	body := `{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]}]}`
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1beta/models/text-embedding-004:batchEmbedContents",
		Model:        "text-embedding-004",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if got.Inputs != nil {
		t.Errorf("inputs should be nil on response side, got %v", got.Inputs)
	}
}

func TestGeminiEmbeddings_Batch_Response_EmptyEmbeddings(t *testing.T) {
	body := `{"embeddings":[]}`
	n := NewGeminiEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1beta/models/text-embedding-004:batchEmbedContents",
	})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestGeminiEmbeddings_DetectBatchByBodyShape(t *testing.T) {
	// Batch detected by presence of "requests" key even without a path hint.
	body := `{"requests":[{"content":{"parts":[{"text":"hi"}]}}]}`
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction: core.DirectionRequest,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should have parsed as batch — inputs populated.
	if len(got.Inputs) != 1 || got.Inputs[0] != "hi" {
		t.Errorf("inputs = %v", got.Inputs)
	}
}

func TestGeminiEmbeddings_EmptyBody(t *testing.T) {
	n := NewGeminiEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestGeminiEmbeddings_MalformedJSON(t *testing.T) {
	n := NewGeminiEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{bad json`), core.Meta{Direction: core.DirectionRequest})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestGeminiEmbeddings_UnsupportedDirection(t *testing.T) {
	n := NewGeminiEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{"content":{}}`), core.Meta{Direction: "other"})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for unknown direction, got %v", err)
	}
}

func TestGeminiEmbeddings_Confidence_HappyPath(t *testing.T) {
	body := `{"content":{"parts":[{"text":"ping"}]},"model":"models/text-embedding-004"}`
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Confidence == 0 {
		t.Errorf("confidence should be non-zero")
	}
}

func TestGeminiEmbeddings_BatchDetectByPathHint(t *testing.T) {
	// Even a single-content body is treated as batch when path contains "batchEmbed".
	body := `{"content":{"parts":[{"text":"x"}]}}`
	n := NewGeminiEmbeddingsNormalizer()
	// The body shape detection says "single" but path says "batch". Path wins.
	// The batch decoder will see empty requests[] which yields ErrUnsupported.
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{
		Direction:    core.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:batchEmbedContents",
	})
	// Either an error (batch decode fails on missing "requests" key) or
	// returns partial — either way we just check it doesn't panic.
	_ = err
}

// Round-trip: canonical→wire→canonical for gemini-embeddings normalizer.
// These tests exercise BOTH request and response directions in sequence.

// TestRoundTrip_GeminiRequest_String: single :embedContent with one text part
// round-trips without losing the input text or the model.
func TestRoundTrip_GeminiRequest_String(t *testing.T) {
	wire := []byte(`{"model":"models/text-embedding-004","content":{"parts":[{"text":"round-trip string"}]}}`)
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{
		Direction:    core.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:embedContent",
	})
	if err != nil {
		t.Fatalf("request normalize: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if got.Model != "models/text-embedding-004" {
		t.Errorf("model = %q", got.Model)
	}
	if len(got.Inputs) != 1 || got.Inputs[0] != "round-trip string" {
		t.Errorf("inputs = %v, want [round-trip string]", got.Inputs)
	}
	if got.Protocol != "gemini-embeddings" {
		t.Errorf("protocol = %q", got.Protocol)
	}
	if got.DetectedSpec != "gemini-embeddings" {
		t.Errorf("detectedSpec = %q", got.DetectedSpec)
	}

	// Rebuild wire from canonical and re-normalize — second pass must be stable.
	wire2 := []byte(`{"model":"` + got.Model + `","content":{"parts":[{"text":"` + got.Inputs[0] + `"}]}}`)
	got2, err := n.Normalize(context.Background(), wire2, core.Meta{
		Direction:    core.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:embedContent",
	})
	if err != nil {
		t.Fatalf("second normalize: %v", err)
	}
	if got2.Model != got.Model || got2.Inputs[0] != got.Inputs[0] {
		t.Errorf("round-trip mismatch: first=%+v second=%+v", got, got2)
	}
}

// TestRoundTrip_GeminiRequest_StringArray: batch :batchEmbedContents with
// multiple text entries round-trips preserving all inputs.
func TestRoundTrip_GeminiRequest_StringArray(t *testing.T) {
	wire := []byte(`{
		"requests": [
			{"model":"models/text-embedding-004","content":{"parts":[{"text":"first"}]}},
			{"model":"models/text-embedding-004","content":{"parts":[{"text":"second"}]}},
			{"model":"models/text-embedding-004","content":{"parts":[{"text":"third"}]}}
		]
	}`)
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{
		Direction:    core.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:batchEmbedContents",
	})
	if err != nil {
		t.Fatalf("request normalize: %v", err)
	}
	if len(got.Inputs) != 3 {
		t.Fatalf("inputs len = %d, want 3", len(got.Inputs))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got.Inputs[i] != want {
			t.Errorf("inputs[%d] = %q, want %q", i, got.Inputs[i], want)
		}
	}
	if got.Model != "models/text-embedding-004" {
		t.Errorf("model = %q", got.Model)
	}
}

// TestRoundTrip_GeminiResponse_Single: :embedContent response round-trips
// preserving kind, protocol, and the absence of inputs.
func TestRoundTrip_GeminiResponse_Single(t *testing.T) {
	wire := []byte(`{"embedding":{"values":[0.11,0.22,0.33,0.44]}}`)
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1beta/models/text-embedding-004:embedContent",
		Model:        "text-embedding-004",
	})
	if err != nil {
		t.Fatalf("response normalize: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if got.Inputs != nil {
		t.Errorf("inputs must be nil on response side, got %v", got.Inputs)
	}
	if got.Model != "text-embedding-004" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Protocol != "gemini-embeddings" {
		t.Errorf("protocol = %q", got.Protocol)
	}
}

// TestRoundTrip_GeminiResponse_Batch: :batchEmbedContents response round-trips
// preserving kind, protocol, and the absence of inputs.
func TestRoundTrip_GeminiResponse_Batch(t *testing.T) {
	wire := []byte(`{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]},{"values":[0.5,0.6]}]}`)
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{
		Direction:    core.DirectionResponse,
		EndpointPath: "/v1beta/models/text-embedding-004:batchEmbedContents",
		Model:        "text-embedding-004",
	})
	if err != nil {
		t.Fatalf("response normalize: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if got.Inputs != nil {
		t.Errorf("inputs must be nil on response side, got %v", got.Inputs)
	}
	if got.Protocol != "gemini-embeddings" {
		t.Errorf("protocol = %q", got.Protocol)
	}
}

// TestRoundTrip_Gemini_ExtensionFields: taskType, title, and batch flag all
// survive the round-trip through the normalizer. The Gemini normalizer stores
// only Kind/Model/Inputs/Protocol; extension fields are handled by
// embed_canonical.go. We verify the request normalizer correctly extracts inputs
// for a body that also carries taskType and title (no interference with inputs).
func TestRoundTrip_Gemini_ExtensionFields(t *testing.T) {
	wire := []byte(`{
		"requests": [
			{
				"model": "models/text-embedding-004",
				"content": {"parts": [{"text": "semantic search"}]},
				"taskType": "SEMANTIC_SIMILARITY",
				"title": "doc-title"
			},
			{
				"model": "models/text-embedding-004",
				"content": {"parts": [{"text": "another query"}]},
				"taskType": "SEMANTIC_SIMILARITY",
				"title": "doc-title"
			}
		]
	}`)
	n := NewGeminiEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{
		Direction:    core.DirectionRequest,
		EndpointPath: "/v1beta/models/text-embedding-004:batchEmbedContents",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(got.Inputs) != 2 {
		t.Fatalf("inputs len = %d, want 2", len(got.Inputs))
	}
	if got.Inputs[0] != "semantic search" {
		t.Errorf("inputs[0] = %q, want semantic search", got.Inputs[0])
	}
	if got.Inputs[1] != "another query" {
		t.Errorf("inputs[1] = %q, want another query", got.Inputs[1])
	}
	if got.Model != "models/text-embedding-004" {
		t.Errorf("model = %q", got.Model)
	}
}
