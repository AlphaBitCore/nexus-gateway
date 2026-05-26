package capability

import (
	"testing"
)

// makeEmbCap is a test helper that builds a ModelCapability with an
// EmbeddingsCapability block.
func makeEmbCap(emb *EmbeddingsCapability) *ModelCapability {
	return &ModelCapability{
		InputModalities:  []string{"text"},
		OutputModalities: []string{"embedding"},
		Lifecycle:        "ga",
		Embeddings:       emb,
	}
}

func intPtr(v int) *int { return &v }

// TestCompatible_NilCap — no capability data at all → reject.
func TestCompatible_NilCap(t *testing.T) {
	ok, reason, _ := Compatible(&EmbeddingRequest{BatchSize: 1}, nil)
	if ok {
		t.Error("expected reject for nil capability")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

// TestCompatible_NilEmbeddings — cap exists but no embeddings block → reject.
func TestCompatible_NilEmbeddings(t *testing.T) {
	cap := &ModelCapability{Lifecycle: "ga"}
	ok, reason, _ := Compatible(&EmbeddingRequest{BatchSize: 1}, cap)
	if ok {
		t.Error("expected reject when Embeddings is nil")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

// TestCompatible_NilRequest — nil req is treated as "all defaults", should pass for a
// model with dimensions declared.
func TestCompatible_NilReqPasses(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedDimensions: []int{1536},
		MaxBatchSize:        100,
	})
	ok, _, _ := Compatible(nil, cap)
	if !ok {
		t.Error("expected ok for nil request (all omitted params)")
	}
}

// TestCompatible_DimensionsMatch — client asks for 1536, model supports 1536 → pass.
func TestCompatible_DimensionsMatch(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedDimensions: []int{512, 1024, 1536},
		MaxBatchSize:        100,
	})
	req := &EmbeddingRequest{BatchSize: 1, Dimensions: intPtr(1536)}
	ok, _, _ := Compatible(req, cap)
	if !ok {
		t.Error("expected ok for matching dimension")
	}
}

// TestCompatible_DimensionsMismatch — client asks for 256, model only has 512/1024 → reject.
func TestCompatible_DimensionsMismatch(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedDimensions: []int{512, 1024},
		MaxBatchSize:        100,
	})
	req := &EmbeddingRequest{BatchSize: 1, Dimensions: intPtr(256)}
	ok, reason, proj := Compatible(req, cap)
	if ok {
		t.Error("expected reject for mismatched dimension")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
	if len(proj.SupportedDimensions) != 2 {
		t.Errorf("CandidateCapability.SupportedDimensions len = %d, want 2", len(proj.SupportedDimensions))
	}
}

// TestCompatible_ModelRejectsDimensions — model has no SupportedDimensions (ada-002 style),
// client sends dimensions → reject.
func TestCompatible_ModelRejectsDimensions(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedDimensions: nil, // no dimensions parameter accepted
		MaxBatchSize:        100,
	})
	req := &EmbeddingRequest{BatchSize: 1, Dimensions: intPtr(512)}
	ok, reason, _ := Compatible(req, cap)
	if ok {
		t.Error("expected reject when model has empty SupportedDimensions")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

// TestCompatible_NoDimensions — client omits dimensions, model has them → pass.
func TestCompatible_NoDimensionsOmitted(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedDimensions: []int{512, 1024},
		MaxBatchSize:        100,
	})
	req := &EmbeddingRequest{BatchSize: 1} // no Dimensions pointer
	ok, _, _ := Compatible(req, cap)
	if !ok {
		t.Error("expected ok when client omits dimensions")
	}
}

// TestCompatible_BatchSizeOk — batch size within limit → pass.
func TestCompatible_BatchSizeOk(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{MaxBatchSize: 100})
	req := &EmbeddingRequest{BatchSize: 50}
	ok, _, _ := Compatible(req, cap)
	if !ok {
		t.Error("expected ok for batch within limit")
	}
}

// TestCompatible_BatchSizeExceeds — batch size exceeds model limit → reject.
func TestCompatible_BatchSizeExceeds(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{MaxBatchSize: 10})
	req := &EmbeddingRequest{BatchSize: 11}
	ok, reason, proj := Compatible(req, cap)
	if ok {
		t.Error("expected reject for oversized batch")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
	if proj.MaxBatchSize != 10 {
		t.Errorf("CandidateCapability.MaxBatchSize = %d, want 10", proj.MaxBatchSize)
	}
}

// TestCompatible_BatchSizeZeroMaxUnlimited — MaxBatchSize 0 = unlimited → pass.
func TestCompatible_BatchSizeZeroMaxUnlimited(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{MaxBatchSize: 0})
	req := &EmbeddingRequest{BatchSize: 9999}
	ok, _, _ := Compatible(req, cap)
	if !ok {
		t.Error("expected ok when MaxBatchSize is 0 (unlimited)")
	}
}

// TestCompatible_EncodingFormatMatch — client asks float, model supports float+base64 → pass.
func TestCompatible_EncodingFormatMatch(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedEncodingFormats: []string{"float", "base64"},
	})
	req := &EmbeddingRequest{BatchSize: 1, EncodingFormat: "float"}
	ok, _, _ := Compatible(req, cap)
	if !ok {
		t.Error("expected ok for matching encoding_format")
	}
}

// TestCompatible_EncodingFormatMismatch — client asks int8, model only supports float → reject.
func TestCompatible_EncodingFormatMismatch(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedEncodingFormats: []string{"float"},
	})
	req := &EmbeddingRequest{BatchSize: 1, EncodingFormat: "int8"}
	ok, reason, _ := Compatible(req, cap)
	if ok {
		t.Error("expected reject for unsupported encoding_format")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

// TestCompatible_EncodingFormatDefault — model omits SupportedEncodingFormats,
// defaults to ["float","base64"]. Client asks float → pass.
func TestCompatible_EncodingFormatDefault(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{}) // no SupportedEncodingFormats set
	req := &EmbeddingRequest{BatchSize: 1, EncodingFormat: "base64"}
	ok, _, proj := Compatible(req, cap)
	if !ok {
		t.Error("expected ok for default encoding formats (float/base64)")
	}
	if len(proj.SupportedEncodingFormats) != 2 {
		t.Errorf("expected 2 default formats in projection, got %d", len(proj.SupportedEncodingFormats))
	}
}

// TestCompatible_InputTypeCohereMatch — Cohere input_type matches → pass.
func TestCompatible_InputTypeCohereMatch(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedInputTypes: []string{"search_document", "search_query", "classification"},
	})
	req := &EmbeddingRequest{BatchSize: 1, InputType: "search_query"}
	ok, _, _ := Compatible(req, cap)
	if !ok {
		t.Error("expected ok for matching Cohere input_type")
	}
}

// TestCompatible_InputTypeCohereNotInList — model doesn't support the input_type → reject.
func TestCompatible_InputTypeCohereNotInList(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedInputTypes: []string{"search_document"},
	})
	req := &EmbeddingRequest{BatchSize: 1, InputType: "clustering"}
	ok, reason, proj := Compatible(req, cap)
	if ok {
		t.Error("expected reject for unsupported Cohere input_type")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
	if len(proj.RequiredExtensions) == 0 {
		t.Error("expected RequiredExtensions to be set on rejection")
	}
}

// TestCompatible_InputTypeEmpty — client omits input_type → pass regardless of model list.
func TestCompatible_InputTypeEmpty(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedInputTypes: []string{"search_document"},
	})
	req := &EmbeddingRequest{BatchSize: 1, InputType: ""}
	ok, _, _ := Compatible(req, cap)
	if !ok {
		t.Error("expected ok when InputType is empty")
	}
}

// TestCompatible_TaskTypeGeminiMatch — Gemini taskType in list → pass.
func TestCompatible_TaskTypeGeminiMatch(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedTaskTypes: []string{"RETRIEVAL_DOCUMENT", "RETRIEVAL_QUERY", "CLASSIFICATION"},
	})
	req := &EmbeddingRequest{BatchSize: 1, TaskType: "RETRIEVAL_QUERY"}
	ok, _, _ := Compatible(req, cap)
	if !ok {
		t.Error("expected ok for matching Gemini taskType")
	}
}

// TestCompatible_TaskTypeGeminiMismatch — Gemini taskType not in model list → reject.
func TestCompatible_TaskTypeGeminiMismatch(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedTaskTypes: []string{"RETRIEVAL_DOCUMENT"},
	})
	req := &EmbeddingRequest{BatchSize: 1, TaskType: "FACT_VERIFICATION"}
	ok, reason, proj := Compatible(req, cap)
	if ok {
		t.Error("expected reject for unsupported Gemini taskType")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
	if len(proj.RequiredExtensions) == 0 {
		t.Error("expected RequiredExtensions to be set on rejection")
	}
}

// TestCompatible_AllRulesPass — all params match → pass + projection populated.
func TestCompatible_AllRulesPass(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedDimensions:      []int{512, 1024, 1536},
		MaxBatchSize:             100,
		SupportedEncodingFormats: []string{"float", "base64"},
		SupportedInputTypes:      []string{"search_document", "search_query"},
		SupportedTaskTypes:       []string{"RETRIEVAL_DOCUMENT", "RETRIEVAL_QUERY"},
	})
	req := &EmbeddingRequest{
		Dimensions:     intPtr(1024),
		BatchSize:      5,
		EncodingFormat: "float",
		InputType:      "search_query",
		TaskType:       "RETRIEVAL_QUERY",
	}
	ok, reason, _ := Compatible(req, cap)
	if !ok {
		t.Errorf("expected ok for all matching params; got reason: %q", reason)
	}
}
