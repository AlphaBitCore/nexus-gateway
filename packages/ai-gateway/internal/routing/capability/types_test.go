package capability

import (
	"github.com/goccy/go-json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

func TestParseModelCapability_NilModel(t *testing.T) {
	got := ParseModelCapability(nil)
	if got != nil {
		t.Errorf("ParseModelCapability(nil) = %v, want nil", got)
	}
}

func TestParseModelCapability_NoCapabilityJson(t *testing.T) {
	m := &store.Model{
		InputModalities:  []string{"text"},
		OutputModalities: []string{"embedding"},
		Lifecycle:        "ga",
		CapabilityJson:   nil,
	}
	got := ParseModelCapability(m)
	if got == nil {
		t.Fatal("ParseModelCapability with nil CapabilityJson should return non-nil")
	}
	if got.Embeddings != nil {
		t.Errorf("Embeddings should be nil when CapabilityJson is nil, got %v", got.Embeddings)
	}
	if got.Lifecycle != "ga" {
		t.Errorf("Lifecycle = %q, want %q", got.Lifecycle, "ga")
	}
}

func TestParseModelCapability_EmptyCapabilityJson(t *testing.T) {
	m := &store.Model{
		InputModalities:  []string{"text"},
		OutputModalities: []string{"embedding"},
		Lifecycle:        "ga",
		CapabilityJson:   []byte{},
	}
	got := ParseModelCapability(m)
	if got == nil {
		t.Fatal("ParseModelCapability with empty CapabilityJson should return non-nil")
	}
	if got.Embeddings != nil {
		t.Errorf("Embeddings should be nil when CapabilityJson is empty, got %v", got.Embeddings)
	}
}

func TestParseModelCapability_FullCapabilityJson(t *testing.T) {
	capJSON := `{
		"embeddings": {
			"max_input_tokens": 8192,
			"supported_dimensions": [256, 512, 1024, 1536],
			"default_dimension": 1536,
			"max_batch_size": 100,
			"supported_encoding_formats": ["float", "base64"],
			"supported_input_types": ["search_document", "search_query"],
			"supported_task_types": ["RETRIEVAL_DOCUMENT", "RETRIEVAL_QUERY"]
		}
	}`
	m := &store.Model{
		InputModalities:  []string{"text"},
		OutputModalities: []string{"embedding"},
		Lifecycle:        "ga",
		CapabilityJson:   []byte(capJSON),
	}
	got := ParseModelCapability(m)
	if got == nil {
		t.Fatal("ParseModelCapability should return non-nil")
	}
	if got.Embeddings == nil {
		t.Fatal("Embeddings should be non-nil when capabilityJson has embeddings block")
	}
	if got.Embeddings.MaxInputTokens != 8192 {
		t.Errorf("MaxInputTokens = %d, want 8192", got.Embeddings.MaxInputTokens)
	}
	if len(got.Embeddings.SupportedDimensions) != 4 {
		t.Errorf("SupportedDimensions len = %d, want 4", len(got.Embeddings.SupportedDimensions))
	}
	if got.Embeddings.DefaultDimension != 1536 {
		t.Errorf("DefaultDimension = %d, want 1536", got.Embeddings.DefaultDimension)
	}
	if got.Embeddings.MaxBatchSize != 100 {
		t.Errorf("MaxBatchSize = %d, want 100", got.Embeddings.MaxBatchSize)
	}
	if len(got.Embeddings.SupportedEncodingFormats) != 2 {
		t.Errorf("SupportedEncodingFormats len = %d, want 2", len(got.Embeddings.SupportedEncodingFormats))
	}
	if len(got.Embeddings.SupportedInputTypes) != 2 {
		t.Errorf("SupportedInputTypes len = %d, want 2", len(got.Embeddings.SupportedInputTypes))
	}
	if len(got.Embeddings.SupportedTaskTypes) != 2 {
		t.Errorf("SupportedTaskTypes len = %d, want 2", len(got.Embeddings.SupportedTaskTypes))
	}
	if got.Lifecycle != "ga" {
		t.Errorf("Lifecycle = %q, want %q", got.Lifecycle, "ga")
	}
}

func TestParseModelCapability_MalformedJson(t *testing.T) {
	m := &store.Model{
		InputModalities:  []string{"text"},
		OutputModalities: []string{"embedding"},
		Lifecycle:        "preview",
		CapabilityJson:   []byte(`{invalid json`),
	}
	got := ParseModelCapability(m)
	if got == nil {
		t.Fatal("ParseModelCapability should return non-nil on JSON error")
	}
	// Embeddings should be nil on parse error (fail-restrictive for embeddings pre-filter)
	if got.Embeddings != nil {
		t.Errorf("Embeddings should be nil on JSON parse error, got %v", got.Embeddings)
	}
	// Lifecycle and modalities should still be populated
	if got.Lifecycle != "preview" {
		t.Errorf("Lifecycle = %q, want %q", got.Lifecycle, "preview")
	}
}

func TestParseModelCapability_EmbeddingsKeyAbsent(t *testing.T) {
	// Valid JSON but no "embeddings" key — model has no embeddings capability
	capJSON := `{"image": {"formats": ["png"]}}`
	m := &store.Model{
		CapabilityJson: []byte(capJSON),
	}
	got := ParseModelCapability(m)
	if got == nil {
		t.Fatal("ParseModelCapability should return non-nil")
	}
	if got.Embeddings != nil {
		t.Errorf("Embeddings should be nil when JSON has no embeddings key, got %v", got.Embeddings)
	}
}

func TestParseModelCapability_ModalitiesPreserved(t *testing.T) {
	m := &store.Model{
		InputModalities:  []string{"text", "image"},
		OutputModalities: []string{"text"},
		Lifecycle:        "deprecated",
		CapabilityJson:   nil,
	}
	got := ParseModelCapability(m)
	if len(got.InputModalities) != 2 {
		t.Errorf("InputModalities len = %d, want 2", len(got.InputModalities))
	}
	if got.OutputModalities[0] != "text" {
		t.Errorf("OutputModalities[0] = %q, want %q", got.OutputModalities[0], "text")
	}
}

// TestEmbeddingsCapability_JSONRoundtrip verifies the JSON tags are correct.
func TestEmbeddingsCapability_JSONRoundtrip(t *testing.T) {
	orig := EmbeddingsCapability{
		MaxInputTokens:           4096,
		SupportedDimensions:      []int{512, 1024},
		DefaultDimension:         1024,
		MaxBatchSize:             50,
		SupportedEncodingFormats: []string{"float"},
		SupportedInputTypes:      []string{"search_query"},
		SupportedTaskTypes:       []string{"RETRIEVAL_QUERY"},
	}
	b, err := json.Marshal(&orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got EmbeddingsCapability
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MaxInputTokens != orig.MaxInputTokens {
		t.Errorf("MaxInputTokens: got %d, want %d", got.MaxInputTokens, orig.MaxInputTokens)
	}
	if got.DefaultDimension != orig.DefaultDimension {
		t.Errorf("DefaultDimension: got %d, want %d", got.DefaultDimension, orig.DefaultDimension)
	}
}
