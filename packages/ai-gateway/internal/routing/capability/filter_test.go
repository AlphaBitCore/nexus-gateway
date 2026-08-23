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

// TestCompatible_EncodingFormatBothAlwaysAvailable — a model that omits
// SupportedEncodingFormats still accepts BOTH float and base64.
//
// This inverts the previous expectation (base64 rejected unless explicitly
// declared). That gate was meant to stop a base64 request being silently
// downgraded by a codec that ignores the field, but it never fired on the
// explicit-model passthrough path, which skips this filter entirely — so a
// base64 request to a non-declaring model was downgraded anyway, and the OpenAI
// SDKs decoded the resulting float array into a quarter-length garbage vector
// (AP-3, observed on staging 2026-07-27). base64 is now guaranteed on the
// response path instead, so there is nothing left to gate.
func TestCompatible_EncodingFormatBothAlwaysAvailable(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{}) // no SupportedEncodingFormats set

	for _, format := range []string{"float", "base64"} {
		ok, reason, proj := Compatible(&EmbeddingRequest{BatchSize: 1, EncodingFormat: format}, cap)
		if !ok {
			t.Errorf("%s must pass against the implicit default; rejected with %q", format, reason)
		}
		if !containsStr(proj.SupportedEncodingFormats, format) {
			t.Errorf("projection %v should advertise %s", proj.SupportedEncodingFormats, format)
		}
	}

	// A format the gateway cannot produce is still rejected.
	ok, reason, _ := Compatible(&EmbeddingRequest{BatchSize: 1, EncodingFormat: "int8"}, cap)
	if ok {
		t.Error("an undeclared, non-guaranteed format must still be rejected")
	}
	if reason == "" {
		t.Error("expected a non-empty reason on rejection")
	}
}

// TestEffectiveEncodingFormats_Default — the gateway guarantees float and
// base64 for every embeddings model, whatever the descriptor says.
func TestEffectiveEncodingFormats_Default(t *testing.T) {
	got := effectiveEncodingFormats(&EmbeddingsCapability{})
	for _, want := range []string{"float", "base64"} {
		if !containsStr(got, want) {
			t.Fatalf("default effectiveEncodingFormats = %v, must include %q", got, want)
		}
	}
	if len(got) != 2 {
		t.Errorf("default should be exactly [float base64], got %v", got)
	}
}

// TestEffectiveEncodingFormats_DescriptorWidensOnly — a descriptor can add a
// provider-specific encoding but cannot narrow the set below the two the
// gateway itself guarantees.
func TestEffectiveEncodingFormats_DescriptorWidensOnly(t *testing.T) {
	got := effectiveEncodingFormats(&EmbeddingsCapability{SupportedEncodingFormats: []string{"float", "int8"}})
	for _, want := range []string{"float", "base64", "int8"} {
		if !containsStr(got, want) {
			t.Errorf("effectiveEncodingFormats = %v, must include %q", got, want)
		}
	}
}

// TestEffectiveEncodingFormats_ExplicitOptIn — an explicit
// ["float","base64"] descriptor is honored verbatim (OpenAI-style opt-in).
func TestEffectiveEncodingFormats_ExplicitOptIn(t *testing.T) {
	emb := &EmbeddingsCapability{SupportedEncodingFormats: []string{"float", "base64"}}
	got := effectiveEncodingFormats(emb)
	if len(got) != 2 || got[0] != "float" || got[1] != "base64" {
		t.Fatalf("explicit effectiveEncodingFormats = %v, want [\"float\",\"base64\"]", got)
	}
	// And a base64 request against an explicit opt-in model passes.
	if ok, _, _ := Compatible(&EmbeddingRequest{BatchSize: 1, EncodingFormat: "base64"}, makeEmbCap(emb)); !ok {
		t.Error("expected base64 to pass against an explicit float+base64 model")
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

// --- Matryoshka range dimensions -------------------------------------------
//
// The failures these cover are not hypothetical. text-embedding-3-large
// truncates to ANY dimension up to its maximum, so no enumeration describes
// it honestly; a catalog that listed [256,512,1024,3072] turned every
// `dimensions: 1536` caller into a 400 MODEL_CAPABILITY_MISMATCH, sustained
// for days on staging. The fix is to let a model declare the range it really
// accepts and let anything inside it reach the provider, which is the only
// party that can say no for a reason we did not invent.

// TestCompatible_RangeDimension_InsideRangePasses — the exact production
// rejection: 1536 requested, enumeration omits it, but the model declares a
// range that covers it. Range wins; the request goes upstream.
func TestCompatible_RangeDimension_InsideRangePasses(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedDimensions: []int{256, 512, 1024, 3072}, // the stale enumeration
		MinDimension:        1,
		MaxDimension:        3072,
		MaxBatchSize:        2048,
	})
	req := &EmbeddingRequest{BatchSize: 1, Dimensions: intPtr(1536)}
	ok, reason, _ := Compatible(req, cap)
	if !ok {
		t.Fatalf("1536 is inside the declared range [1,3072] and must pass; got reject %q", reason)
	}
}

// TestCompatible_RangeDimension_AboveMaxRejected — the range is a real bound,
// not a way of switching the check off. Above the ceiling still rejects.
func TestCompatible_RangeDimension_AboveMaxRejected(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		MinDimension: 1,
		MaxDimension: 3072,
		MaxBatchSize: 2048,
	})
	req := &EmbeddingRequest{BatchSize: 1, Dimensions: intPtr(4096)}
	ok, reason, _ := Compatible(req, cap)
	if ok {
		t.Fatal("4096 is above the declared maximum 3072 and must be rejected")
	}
	if reason == "" {
		t.Error("expected a non-empty reason")
	}
}

// TestCompatible_RangeDimension_BelowMinRejected — same on the floor.
func TestCompatible_RangeDimension_BelowMinRejected(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		MinDimension: 256,
		MaxDimension: 3072,
		MaxBatchSize: 2048,
	})
	req := &EmbeddingRequest{BatchSize: 1, Dimensions: intPtr(128)}
	if ok, _, _ := Compatible(req, cap); ok {
		t.Fatal("128 is below the declared minimum 256 and must be rejected")
	}
}

// TestCompatible_RangeDimension_MaxOnlyTreatsMinAsOne — declaring only a
// ceiling is the common case (any dimension up to the model max), and must
// not accidentally reject everything by treating an absent min as a bound.
func TestCompatible_RangeDimension_MaxOnlyTreatsMinAsOne(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{MaxDimension: 3072, MaxBatchSize: 2048})
	if ok, reason, _ := Compatible(&EmbeddingRequest{BatchSize: 1, Dimensions: intPtr(1)}, cap); !ok {
		t.Fatalf("1 must pass when only a maximum is declared; got reject %q", reason)
	}
}

// TestCompatible_EnumerationStillAppliesWithoutRange — a model with a genuinely
// fixed set (Cohere embed-english-v3.0 produces 1024 and nothing else) keeps
// the enumeration behaviour. Adding the range form must not weaken models that
// are correctly described by a list.
func TestCompatible_EnumerationStillAppliesWithoutRange(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{
		SupportedDimensions: []int{1024},
		MaxBatchSize:        96,
	})
	if ok, _, _ := Compatible(&EmbeddingRequest{BatchSize: 1, Dimensions: intPtr(1536)}, cap); ok {
		t.Fatal("1536 must still be rejected for a model whose only dimension is 1024")
	}
	if ok, reason, _ := Compatible(&EmbeddingRequest{BatchSize: 1, Dimensions: intPtr(1024)}, cap); !ok {
		t.Fatalf("1024 must pass for that model; got reject %q", reason)
	}
}

// TestCompatible_RangeProjectedForOperators — the rejection projection feeds
// the "available capabilities" block of the 400, so an operator reading it has
// to see the range that was actually applied, not just a stale list.
func TestCompatible_RangeProjectedForOperators(t *testing.T) {
	cap := makeEmbCap(&EmbeddingsCapability{MinDimension: 256, MaxDimension: 3072})
	_, _, proj := Compatible(&EmbeddingRequest{BatchSize: 1, Dimensions: intPtr(9999)}, cap)
	if proj.MinDimension != 256 || proj.MaxDimension != 3072 {
		t.Errorf("projection must carry the range: got min=%d max=%d, want 256/3072",
			proj.MinDimension, proj.MaxDimension)
	}
}
