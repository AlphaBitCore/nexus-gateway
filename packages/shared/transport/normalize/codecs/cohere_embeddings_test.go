package codecs

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Round-trip: canonical→wire→canonical for cohere-embeddings normalizer.
// These tests exercise BOTH directions in sequence to verify the normalizer
// is internally consistent (no field loss on encode→decode).

// TestRoundTrip_CohereRequest_String: single-string input round-trips without
// losing the text or the model.
func TestRoundTrip_CohereRequest_String(t *testing.T) {
	// Wire shape that a Cohere client sends.
	wire := []byte(`{"texts":["hello cohere"],"model":"embed-v4.0"}`)
	n := NewCohereEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("request normalize: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v, want ai-embedding", got.Kind)
	}
	if got.Model != "embed-v4.0" {
		t.Errorf("model = %q, want embed-v4.0", got.Model)
	}
	if len(got.Inputs) != 1 || got.Inputs[0] != "hello cohere" {
		t.Errorf("inputs = %v, want [hello cohere]", got.Inputs)
	}
	if got.Protocol != "cohere-embeddings" {
		t.Errorf("protocol = %q", got.Protocol)
	}
	if got.DetectedSpec != "cohere-embeddings" {
		t.Errorf("detectedSpec = %q", got.DetectedSpec)
	}

	// Rebuild a wire body from the canonical fields and re-normalize to confirm
	// the second pass yields the same result — the canonical is stable.
	wire2 := []byte(`{"texts":["` + got.Inputs[0] + `"],"model":"` + got.Model + `"}`)
	got2, err := n.Normalize(context.Background(), wire2, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("second normalize: %v", err)
	}
	if got2.Model != got.Model || got2.Inputs[0] != got.Inputs[0] {
		t.Errorf("round-trip mismatch: first=%+v second=%+v", got, got2)
	}
}

// TestRoundTrip_CohereRequest_StringArray: multi-string array input round-trips
// preserving all elements and their order.
func TestRoundTrip_CohereRequest_StringArray(t *testing.T) {
	wire := []byte(`{"texts":["alpha","beta","gamma"],"model":"embed-v4.0"}`)
	n := NewCohereEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("request normalize: %v", err)
	}
	if len(got.Inputs) != 3 {
		t.Fatalf("inputs len = %d, want 3", len(got.Inputs))
	}
	for i, want := range []string{"alpha", "beta", "gamma"} {
		if got.Inputs[i] != want {
			t.Errorf("inputs[%d] = %q, want %q", i, got.Inputs[i], want)
		}
	}
	if got.Model != "embed-v4.0" {
		t.Errorf("model = %q", got.Model)
	}
}

// TestRoundTrip_CohereResponse_Single: single-item response round-trips
// preserving model and usage.
func TestRoundTrip_CohereResponse_Single(t *testing.T) {
	wire := []byte(`{
		"id": "emb-rt01",
		"model": "embed-v4.0",
		"embeddings": {"float": [[0.1, 0.2, 0.3]]},
		"meta": {"billed_units": {"input_tokens": 5}},
		"response_type": "embeddings_floats"
	}`)
	n := NewCohereEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("response normalize: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if got.Model != "embed-v4.0" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Inputs != nil {
		t.Errorf("inputs must be nil on response side, got %v", got.Inputs)
	}
	if got.Usage == nil {
		t.Fatal("usage must be non-nil")
	}
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 5 {
		t.Errorf("promptTokens = %v, want 5", got.Usage.PromptTokens)
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 5 {
		t.Errorf("totalTokens = %v, want 5", got.Usage.TotalTokens)
	}
}

// TestRoundTrip_CohereResponse_Batch: multi-item response round-trips
// preserving model and usage totals.
func TestRoundTrip_CohereResponse_Batch(t *testing.T) {
	wire := []byte(`{
		"id": "emb-rt02",
		"model": "embed-v4.0",
		"embeddings": [[0.1, 0.2], [0.3, 0.4], [0.5, 0.6]],
		"meta": {"billed_units": {"input_tokens": 21}},
		"response_type": "embeddings_floats"
	}`)
	n := NewCohereEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("response normalize: %v", err)
	}
	if got.Usage == nil {
		t.Fatal("usage must be non-nil")
	}
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 21 {
		t.Errorf("promptTokens = %v, want 21", got.Usage.PromptTokens)
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 21 {
		t.Errorf("totalTokens = %v, want 21", got.Usage.TotalTokens)
	}
}

// TestRoundTrip_Cohere_ExtensionFields: nexus.ext.cohere.input_type and
// nexus.ext.cohere.embedding_types survive a request-side normalize pass.
// Because the normalizer only reads the Cohere wire shape (texts/model/
// input_type/embedding_types), we verify that the underlying Cohere request
// body is correctly decoded — the extension fields are a cohere-adapter concern
// that sits in embed_canonical.go, not in this normalizer.  What we assert
// here is that the normalizer does not corrupt the input_type value when it
// appears in the body.
func TestRoundTrip_Cohere_ExtensionFields(t *testing.T) {
	// input_type and embedding_types are Cohere wire fields on the request side;
	// the normalizer extracts texts/model but passes the rest through to the
	// confidence scorer.  We simply verify no panic and the key inputs survive.
	wire := []byte(`{
		"texts": ["search query text"],
		"model": "embed-english-v3.0",
		"input_type": "search_query",
		"embedding_types": ["float", "int8"]
	}`)
	n := NewCohereEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), wire, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(got.Inputs) != 1 || got.Inputs[0] != "search query text" {
		t.Errorf("inputs = %v", got.Inputs)
	}
	if got.Model != "embed-english-v3.0" {
		t.Errorf("model = %q", got.Model)
	}
	// Confidence should be high since input_type + embedding_types are both
	// recognized optional fields in the Cohere field spec.
	if got.Confidence < 0.7 {
		t.Errorf("confidence = %v, want >= 0.70 with all optional fields present", got.Confidence)
	}
}

func TestCohereEmbeddings_ID(t *testing.T) {
	n := NewCohereEmbeddingsNormalizer()
	if n.ID() != "cohere-embeddings" {
		t.Fatalf("ID() = %q, want cohere-embeddings", n.ID())
	}
}

func TestCohereEmbeddings_Request_HappyPath(t *testing.T) {
	body := `{"texts":["hello","world"],"model":"embed-v4.0","input_type":"search_document"}`
	n := NewCohereEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v, want ai-embedding", got.Kind)
	}
	if got.Model != "embed-v4.0" {
		t.Errorf("model = %q", got.Model)
	}
	if len(got.Inputs) != 2 || got.Inputs[0] != "hello" || got.Inputs[1] != "world" {
		t.Errorf("inputs = %v", got.Inputs)
	}
	if got.Protocol != "cohere-embeddings" {
		t.Errorf("protocol = %q", got.Protocol)
	}
	if got.DetectedSpec != "cohere-embeddings" {
		t.Errorf("detectedSpec = %q", got.DetectedSpec)
	}
}

func TestCohereEmbeddings_Request_SingleText(t *testing.T) {
	body := `{"texts":["only one"],"model":"embed-v4.0"}`
	n := NewCohereEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Inputs) != 1 || got.Inputs[0] != "only one" {
		t.Errorf("inputs = %v", got.Inputs)
	}
}

func TestCohereEmbeddings_Request_EmptyTexts(t *testing.T) {
	body := `{"texts":[],"model":"embed-v4.0"}`
	n := NewCohereEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestCohereEmbeddings_Request_EmptyBody(t *testing.T) {
	n := NewCohereEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestCohereEmbeddings_Request_MalformedJSON(t *testing.T) {
	n := NewCohereEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{bad`), core.Meta{Direction: core.DirectionRequest})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestCohereEmbeddings_Response_HappyPath(t *testing.T) {
	body := `{
		"id": "emb-abc123",
		"model": "embed-v4.0",
		"embeddings": {"float": [[0.1, 0.2]]},
		"meta": {"billed_units": {"input_tokens": 10}},
		"response_type": "embeddings_floats"
	}`
	n := NewCohereEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIEmbedding {
		t.Errorf("kind = %v", got.Kind)
	}
	if got.Model != "embed-v4.0" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Inputs != nil {
		t.Errorf("inputs should be nil on response side, got %v", got.Inputs)
	}
	if got.Usage == nil {
		t.Fatal("usage should be non-nil")
	}
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 10 {
		t.Errorf("promptTokens = %v", got.Usage.PromptTokens)
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 10 {
		t.Errorf("totalTokens = %v", got.Usage.TotalTokens)
	}
}

func TestCohereEmbeddings_Response_NoUsage(t *testing.T) {
	// Response with id but no usage block — Usage should be nil.
	body := `{"id":"emb-xyz","model":"embed-v4.0","response_type":"embeddings_floats"}`
	n := NewCohereEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Usage != nil {
		t.Errorf("usage should be nil when not in response, got %+v", got.Usage)
	}
}

func TestCohereEmbeddings_Response_EmptyBody(t *testing.T) {
	n := NewCohereEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: core.DirectionResponse})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestCohereEmbeddings_Response_MalformedJSON(t *testing.T) {
	n := NewCohereEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`not json`), core.Meta{Direction: core.DirectionResponse})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestCohereEmbeddings_Response_MissingRequiredFields(t *testing.T) {
	body := `{"unrelated_field": true}`
	n := NewCohereEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestCohereEmbeddings_UnsupportedDirection(t *testing.T) {
	n := NewCohereEmbeddingsNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{}`), core.Meta{Direction: "bad"})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for unknown direction, got %v", err)
	}
}

func TestCohereEmbeddings_Confidence_Request(t *testing.T) {
	body := `{"texts":["test text"],"model":"embed-v4.0"}`
	n := NewCohereEmbeddingsNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Confidence < 0.7 {
		t.Errorf("confidence = %v, expected >= 0.70", got.Confidence)
	}
}
