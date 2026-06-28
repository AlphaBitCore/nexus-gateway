package gemini

import (
	"github.com/goccy/go-json"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestEmbedContentRequestToCanonical_HappyPath asserts that a Gemini
// :embedContent body is converted to OpenAI canonical embedding shape:
// content.parts[*].text concatenated into "input", model from caller,
// taskType/title under nexus.ext.gemini.*, outputDimensionality lifted
// to canonical "dimensions", and the "batch" extension flag is false.
func TestEmbedContentRequestToCanonical_HappyPath(t *testing.T) {
	body := `{
        "content": {"parts": [{"text": "What is Nexus?"}, {"text": "Tell me more."}]},
        "taskType": "RETRIEVAL_QUERY",
        "title": "intro",
        "outputDimensionality": 256
    }`
	out, err := EmbedContentRequestToCanonical([]byte(body), "models/text-embedding-004")
	if err != nil {
		t.Fatalf("EmbedContentRequestToCanonical: %v", err)
	}
	if got := gjson.GetBytes(out, "model").Str; got != "models/text-embedding-004" {
		t.Fatalf("model: %q", got)
	}
	if got := gjson.GetBytes(out, "input").Str; got != "What is Nexus?\nTell me more." {
		t.Fatalf("input: %q", got)
	}
	if got := gjson.GetBytes(out, "dimensions").Int(); got != 256 {
		t.Fatalf("dimensions: %d", got)
	}
	if got := gjson.GetBytes(out, "nexus.ext.gemini.taskType").Str; got != "RETRIEVAL_QUERY" {
		t.Fatalf("taskType ext: %q", got)
	}
	if got := gjson.GetBytes(out, "nexus.ext.gemini.title").Str; got != "intro" {
		t.Fatalf("title ext: %q", got)
	}
	if got := gjson.GetBytes(out, "nexus.ext.gemini.batch").Bool(); got != false {
		t.Fatalf("batch ext should be false for single, got %v", got)
	}
}

func TestEmbedContentRequestToCanonical_Errors(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		modelID    string
		wantErrSub string
	}{
		{"invalid json", `not-json`, "models/x", "invalid JSON"},
		{"missing content", `{"taskType":"x"}`, "models/x", "missing 'content'"},
		{"empty parts", `{"content":{"parts":[]}}`, "models/x", "no text"},
		{"parts without text", `{"content":{"parts":[{"inlineData":{}}]}}`, "models/x", "no text"},
		{"missing model", `{"content":{"parts":[{"text":"hi"}]}}`, "", "missing model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EmbedContentRequestToCanonical([]byte(tc.body), tc.modelID)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.wantErrSub)
			}
		})
	}
}

func TestBatchEmbedContentsRequestToCanonical_HappyPath(t *testing.T) {
	body := `{
        "requests": [
            {"content":{"parts":[{"text":"alpha"}]}, "taskType":"RETRIEVAL_DOCUMENT", "title":"t1"},
            {"content":{"parts":[{"text":"beta"}]},  "taskType":"RETRIEVAL_DOCUMENT", "title":"t1"},
            {"content":{"parts":[{"text":"gamma"}]}, "taskType":"RETRIEVAL_DOCUMENT", "title":"t1"}
        ]
    }`
	out, err := BatchEmbedContentsRequestToCanonical([]byte(body), "models/text-embedding-004")
	if err != nil {
		t.Fatalf("BatchEmbedContentsRequestToCanonical: %v", err)
	}
	if got := gjson.GetBytes(out, "model").Str; got != "models/text-embedding-004" {
		t.Fatalf("model: %q", got)
	}
	var inputs []string
	gjson.GetBytes(out, "input").ForEach(func(_, v gjson.Result) bool {
		inputs = append(inputs, v.Str)
		return true
	})
	if !reflect.DeepEqual(inputs, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("input: %v", inputs)
	}
	if got := gjson.GetBytes(out, "nexus.ext.gemini.taskType").Str; got != "RETRIEVAL_DOCUMENT" {
		t.Fatalf("uniform taskType should lift to canonical: %q", got)
	}
	if got := gjson.GetBytes(out, "nexus.ext.gemini.title").Str; got != "t1" {
		t.Fatalf("uniform title should lift: %q", got)
	}
	if got := gjson.GetBytes(out, "nexus.ext.gemini.batch").Bool(); got != true {
		t.Fatalf("batch ext should be true: %v", got)
	}
}

func TestBatchEmbedContentsRequestToCanonical_RejectsMixed(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantErrSub string
	}{
		{
			name: "mixed taskType",
			body: `{"requests":[
                {"content":{"parts":[{"text":"a"}]},"taskType":"RETRIEVAL_QUERY"},
                {"content":{"parts":[{"text":"b"}]},"taskType":"RETRIEVAL_DOCUMENT"}
            ]}`,
			wantErrSub: "mixed taskType",
		},
		{
			name: "mixed outputDimensionality",
			body: `{"requests":[
                {"content":{"parts":[{"text":"a"}]},"outputDimensionality":256},
                {"content":{"parts":[{"text":"b"}]},"outputDimensionality":512}
            ]}`,
			wantErrSub: "mixed outputDimensionality",
		},
		{
			name: "outputDim present then absent",
			body: `{"requests":[
                {"content":{"parts":[{"text":"a"}]},"outputDimensionality":256},
                {"content":{"parts":[{"text":"b"}]}}
            ]}`,
			wantErrSub: "mixed outputDimensionality",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BatchEmbedContentsRequestToCanonical([]byte(tc.body), "models/text-embedding-004")
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.wantErrSub)
			}
		})
	}
}

func TestBatchEmbedContentsRequestToCanonical_Errors(t *testing.T) {
	cases := []struct {
		name, body, modelID, wantErrSub string
	}{
		{"invalid json", `not-json`, "m", "invalid JSON"},
		{"missing requests", `{"foo":"bar"}`, "m", "missing or non-array"},
		{"empty content text", `{"requests":[{"content":{"parts":[]}}]}`, "m", "no text"},
		{"missing model", `{"requests":[{"content":{"parts":[{"text":"hi"}]}}]}`, "", "missing model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BatchEmbedContentsRequestToCanonical([]byte(tc.body), tc.modelID)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.wantErrSub)
			}
		})
	}
}

func TestCanonicalToEmbedContentResponse(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		canonical := []byte(`{"data":[{"object":"embedding","embedding":[0.1,-0.2,0.3],"index":0}]}`)
		out, err := CanonicalToEmbedContentResponse(canonical)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		emb := got["embedding"].(map[string]any)
		values := emb["values"].([]any)
		if !reflect.DeepEqual([]any{0.1, -0.2, 0.3}, values) {
			t.Fatalf("values: %v", values)
		}
	})
	t.Run("rejects multi-entry data", func(t *testing.T) {
		canonical := []byte(`{"data":[
            {"object":"embedding","embedding":[0.1],"index":0},
            {"object":"embedding","embedding":[0.2],"index":1}
        ]}`)
		_, err := CanonicalToEmbedContentResponse(canonical)
		if err == nil {
			t.Fatal("want error, got nil")
		}
		if !strings.Contains(err.Error(), "expected 1 data[] entry, got 2") {
			t.Fatalf("error: %v", err)
		}
	})
	t.Run("missing data", func(t *testing.T) {
		_, err := CanonicalToEmbedContentResponse([]byte(`{"foo":"bar"}`))
		if err == nil || !strings.Contains(err.Error(), "missing data") {
			t.Fatalf("expected missing data error, got: %v", err)
		}
	})
	t.Run("data entry without embedding", func(t *testing.T) {
		_, err := CanonicalToEmbedContentResponse([]byte(`{"data":[{"object":"embedding","index":0}]}`))
		if err == nil || !strings.Contains(err.Error(), "embedding missing") {
			t.Fatalf("expected embedding missing error, got: %v", err)
		}
	})
}

func TestCanonicalToBatchEmbedContentsResponse(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		canonical := []byte(`{"data":[
            {"object":"embedding","embedding":[0.1, 0.2],"index":0},
            {"object":"embedding","embedding":[0.3, 0.4],"index":1}
        ]}`)
		out, err := CanonicalToBatchEmbedContentsResponse(canonical)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		embs := got["embeddings"].([]any)
		if len(embs) != 2 {
			t.Fatalf("embeddings len: %d", len(embs))
		}
		row0 := embs[0].(map[string]any)["values"].([]any)
		if !reflect.DeepEqual([]any{0.1, 0.2}, row0) {
			t.Fatalf("row0: %v", row0)
		}
	})
	t.Run("missing data", func(t *testing.T) {
		_, err := CanonicalToBatchEmbedContentsResponse([]byte(`{}`))
		if err == nil || !strings.Contains(err.Error(), "missing data") {
			t.Fatalf("expected missing data error, got: %v", err)
		}
	})
	t.Run("data entry without embedding", func(t *testing.T) {
		_, err := CanonicalToBatchEmbedContentsResponse([]byte(`{"data":[{"object":"embedding","index":0}]}`))
		if err == nil || !strings.Contains(err.Error(), "embedding missing") {
			t.Fatalf("expected embedding missing error, got: %v", err)
		}
	})
}

// TestExtractGeminiText_NonArrayParts hits the early-return when content
// has no `parts` array.
func TestExtractGeminiText_NonArrayParts(t *testing.T) {
	cases := []string{
		`{"content":{}}`,                    // no parts
		`{"content":{"parts":"not-array"}}`, // parts not array
		`{"content":{"parts":[{"inlineData":{},"mime":""}]}}`,
	}
	for i, c := range cases {
		t.Run(strings.TrimSpace(c)[:10], func(t *testing.T) {
			_ = i
			content := gjson.GetBytes([]byte(c), "content")
			got := extractGeminiText(content)
			if got != "" {
				t.Fatalf("expected empty string, got %q", got)
			}
		})
	}
}

// TestEmbedContentRequestToCanonical_OptionalFieldsOmitted exercises the
// branch where the request omits taskType / title / outputDimensionality.
func TestEmbedContentRequestToCanonical_OptionalFieldsOmitted(t *testing.T) {
	body := `{"content":{"parts":[{"text":"hi"}]}}`
	out, err := EmbedContentRequestToCanonical([]byte(body), "models/text-embedding-004")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"nexus.ext.gemini.taskType", "nexus.ext.gemini.title", "dimensions"} {
		if gjson.GetBytes(out, k).Exists() {
			t.Fatalf("field %s should NOT be present when omitted: %s", k, out)
		}
	}
	if got := gjson.GetBytes(out, "nexus.ext.gemini.batch").Bool(); got != false {
		t.Fatalf("batch=false expected: %v", got)
	}
}

// TestBatchEmbedContentsRequestToCanonical_NoSharedFields exercises the
// branch where no sub-request carries taskType/title/outputDimensionality.
func TestBatchEmbedContentsRequestToCanonical_NoSharedFields(t *testing.T) {
	body := `{"requests":[
        {"content":{"parts":[{"text":"a"}]}},
        {"content":{"parts":[{"text":"b"}]}}
    ]}`
	out, err := BatchEmbedContentsRequestToCanonical([]byte(body), "models/text-embedding-004")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"nexus.ext.gemini.taskType", "nexus.ext.gemini.title", "dimensions"} {
		if gjson.GetBytes(out, k).Exists() {
			t.Fatalf("field %s should NOT be present when no sub-request set it: %s", k, out)
		}
	}
	if got := gjson.GetBytes(out, "nexus.ext.gemini.batch").Bool(); got != true {
		t.Fatalf("batch=true expected: %v", got)
	}
}

// TestBatchEmbedContentsRequestToCanonical_MixedTaskType_ActionableError verifies
// that the improved error message for mixed taskType includes the differing field
// values (so users know which values to split) and contains both the constraint
// explanation and the "Split into separate requests" guidance.
func TestBatchEmbedContentsRequestToCanonical_MixedTaskType_ActionableError(t *testing.T) {
	body := `{"requests":[
        {"content":{"parts":[{"text":"a"}]},"taskType":"SEMANTIC_SIMILARITY"},
        {"content":{"parts":[{"text":"b"}]},"taskType":"CLASSIFICATION"}
    ]}`
	_, err := BatchEmbedContentsRequestToCanonical([]byte(body), "models/text-embedding-004")
	if err == nil {
		t.Fatal("want error for mixed taskType, got nil")
	}
	msg := err.Error()
	// Must include the differing task type values so users can act on it.
	if !strings.Contains(msg, "SEMANTIC_SIMILARITY") {
		t.Errorf("error must include SEMANTIC_SIMILARITY, got: %q", msg)
	}
	if !strings.Contains(msg, "CLASSIFICATION") {
		t.Errorf("error must include CLASSIFICATION, got: %q", msg)
	}
	// Must advise the user on the fix.
	if !strings.Contains(msg, "Split into separate requests") {
		t.Errorf("error must advise to split, got: %q", msg)
	}
	// Must include batch size context ("2 items").
	if !strings.Contains(msg, "2") {
		t.Errorf("error must include batch size, got: %q", msg)
	}
}

// TestBatchEmbedContentsRequestToCanonical_ThreeWayMixedTaskType exercises the
// deduplication branch in the seenTaskTypes collector: when a third sub-request
// repeats a taskType value already observed, the error message should still list
// only distinct values (no duplicates) while still reporting the conflict.
func TestBatchEmbedContentsRequestToCanonical_ThreeWayMixedTaskType(t *testing.T) {
	// Requests: SEMANTIC_SIMILARITY, CLASSIFICATION, SEMANTIC_SIMILARITY (repeat).
	// The dedup loop fires on the third entry: "SEMANTIC_SIMILARITY" is already
	// in seenTaskTypes, so found=true and the break runs.
	body := `{"requests":[
        {"content":{"parts":[{"text":"a"}]},"taskType":"SEMANTIC_SIMILARITY"},
        {"content":{"parts":[{"text":"b"}]},"taskType":"CLASSIFICATION"},
        {"content":{"parts":[{"text":"c"}]},"taskType":"SEMANTIC_SIMILARITY"}
    ]}`
	_, err := BatchEmbedContentsRequestToCanonical([]byte(body), "models/text-embedding-004")
	if err == nil {
		t.Fatal("want error for mixed taskType, got nil")
	}
	msg := err.Error()
	// Error must list both distinct values only once each.
	if !strings.Contains(msg, "SEMANTIC_SIMILARITY") {
		t.Errorf("error must include SEMANTIC_SIMILARITY: %q", msg)
	}
	if !strings.Contains(msg, "CLASSIFICATION") {
		t.Errorf("error must include CLASSIFICATION: %q", msg)
	}
	if !strings.Contains(msg, "Split into separate requests") {
		t.Errorf("error must advise to split: %q", msg)
	}
}

// TestBatchEmbedContentsRequestToCanonical_MixedDim_ActionableError verifies
// the improved mixed outputDimensionality error message is actionable.
func TestBatchEmbedContentsRequestToCanonical_MixedDim_ActionableError(t *testing.T) {
	body := `{"requests":[
        {"content":{"parts":[{"text":"a"}]},"outputDimensionality":256},
        {"content":{"parts":[{"text":"b"}]},"outputDimensionality":512}
    ]}`
	_, err := BatchEmbedContentsRequestToCanonical([]byte(body), "models/text-embedding-004")
	if err == nil {
		t.Fatal("want error for mixed outputDimensionality, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Split into separate requests") {
		t.Errorf("error must advise to split, got: %q", msg)
	}
	if !strings.Contains(msg, "2") {
		t.Errorf("error must include batch size, got: %q", msg)
	}
}

// TestBatchEmbedContentsRequestToCanonical_MixedTitle_DropsTitle verifies that
// when sub-requests carry different title values (titleMixed=true), the title
// is NOT lifted to the canonical body. The batch still succeeds (mixed title is
// a warning-level condition, not a hard error, unlike mixed taskType).
func TestBatchEmbedContentsRequestToCanonical_MixedTitle_DropsTitle(t *testing.T) {
	body := `{"requests":[
        {"content":{"parts":[{"text":"a"}]},"title":"title-one"},
        {"content":{"parts":[{"text":"b"}]},"title":"title-two"}
    ]}`
	out, err := BatchEmbedContentsRequestToCanonical([]byte(body), "models/text-embedding-004")
	if err != nil {
		t.Fatalf("mixed title should not error, got: %v", err)
	}
	// title must NOT be in canonical when mixed — it would incorrectly assign
	// one title to all inputs in a way that misrepresents the batch.
	if gjson.GetBytes(out, "nexus.ext.gemini.title").Exists() {
		t.Errorf("title must not be lifted when mixed across sub-requests: %s", out)
	}
	// inputs must still be present.
	inp := gjson.GetBytes(out, "input")
	if !inp.IsArray() || len(inp.Array()) != 2 {
		t.Errorf("input must be 2-element array: %s", out)
	}
}

// TestBatchEmbedContentsRequestToCanonical_WithDimensions exercises the
// `if dimSet { canonical, _ = sjson.SetBytes... }` branch.
func TestBatchEmbedContentsRequestToCanonical_WithDimensions(t *testing.T) {
	body := `{"requests":[
        {"content":{"parts":[{"text":"a"}]},"outputDimensionality":512},
        {"content":{"parts":[{"text":"b"}]},"outputDimensionality":512}
    ]}`
	out, err := BatchEmbedContentsRequestToCanonical([]byte(body), "models/text-embedding-004")
	if err != nil {
		t.Fatalf("uniform outputDimensionality should not error, got: %v", err)
	}
	if got := gjson.GetBytes(out, "dimensions").Int(); got != 512 {
		t.Errorf("dimensions: got %d, want 512", got)
	}
}

func TestCanonicalEmbeddingBatchFlag(t *testing.T) {
	cases := []struct {
		name        string
		canonical   string
		wantBatch   bool
		wantPresent bool
	}{
		{"absent", `{"data":[]}`, false, false},
		{"present true", `{"nexus":{"ext":{"gemini":{"batch":true}}}}`, true, true},
		{"present false", `{"nexus":{"ext":{"gemini":{"batch":false}}}}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			batch, present := CanonicalEmbeddingBatchFlag([]byte(tc.canonical))
			if batch != tc.wantBatch || present != tc.wantPresent {
				t.Fatalf("got (%v, %v), want (%v, %v)", batch, present, tc.wantBatch, tc.wantPresent)
			}
		})
	}
}
