package cohere

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestEmbedRequestToCanonical_HappyPaths pins the wire-shape contracts
// the embedding canonical bridge depends on. Drifting any of these
// silently breaks Cohere↔OpenAI cross-format routing.
func TestEmbedRequestToCanonical_HappyPaths(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		modelID       string
		wantInput     string // JSON-encoded expected `input`
		wantModel     string
		wantExtKeys   map[string]string
		wantNoExtKeys []string
		wantNoModel   bool
	}{
		{
			name:      "single string flattens on canonical",
			body:      `{"texts":["hello"],"model":"embed-english-v3.0","input_type":"search_query"}`,
			modelID:   "",
			wantInput: `"hello"`,
			wantModel: "embed-english-v3.0",
			wantExtKeys: map[string]string{
				"cohere.input_type": "search_query",
			},
			wantNoExtKeys: []string{"cohere.embedding_types", "cohere.truncate"},
		},
		{
			name:      "batch with truncate + embedding_types",
			body:      `{"texts":["a","b","c"],"model":"embed-english-v3.0","input_type":"search_document","truncate":"END","embedding_types":["float","int8"]}`,
			modelID:   "",
			wantInput: `["a","b","c"]`,
			wantModel: "embed-english-v3.0",
			wantExtKeys: map[string]string{
				"cohere.input_type":      "search_document",
				"cohere.truncate":        "END",
				"cohere.embedding_types": `["float","int8"]`,
			},
		},
		{
			name:      "model defaults to providerModelID when omitted",
			body:      `{"texts":["x"],"input_type":"search_query"}`,
			modelID:   "embed-multilingual-v3.0",
			wantInput: `"x"`,
			wantModel: "embed-multilingual-v3.0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := EmbedRequestToCanonical([]byte(tc.body), tc.modelID)
			if err != nil {
				t.Fatalf("EmbedRequestToCanonical: %v", err)
			}
			gotInput := gjson.GetBytes(out, "input").Raw
			if gotInput != tc.wantInput {
				t.Fatalf("input mismatch: got %s, want %s\nfull canonical: %s", gotInput, tc.wantInput, out)
			}
			if got := gjson.GetBytes(out, "model").Str; got != tc.wantModel {
				t.Fatalf("model mismatch: got %q, want %q", got, tc.wantModel)
			}
			for k, want := range tc.wantExtKeys {
				got := gjson.GetBytes(out, "nexus.ext."+k)
				if !got.Exists() {
					t.Fatalf("ext key %s missing from canonical: %s", k, out)
				}
				if got.Raw != want && got.Str != want {
					t.Fatalf("ext %s: got %q (raw %s), want %q", k, got.Str, got.Raw, want)
				}
			}
			for _, k := range tc.wantNoExtKeys {
				got := gjson.GetBytes(out, "nexus.ext."+k)
				if got.Exists() {
					t.Fatalf("ext key %s should NOT be set on canonical: %s", k, out)
				}
			}
		})
	}
}

func TestEmbedRequestToCanonical_Errors(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		modelID    string
		wantErrSub string
	}{
		{
			name:       "invalid JSON",
			body:       `not-json`,
			wantErrSub: "invalid JSON",
		},
		{
			name:       "missing texts",
			body:       `{"model":"m"}`,
			wantErrSub: "missing or non-array",
		},
		{
			name:       "non-array texts",
			body:       `{"texts":"abc","model":"m"}`,
			wantErrSub: "missing or non-array",
		},
		{
			name:       "non-string element in texts",
			body:       `{"texts":["a", 42],"model":"m"}`,
			wantErrSub: "non-string element",
		},
		{
			name:       "missing model both body and target",
			body:       `{"texts":["a"]}`,
			wantErrSub: "missing 'model'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EmbedRequestToCanonical([]byte(tc.body), tc.modelID)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.wantErrSub)
			}
		})
	}
}

// TestCanonicalToEmbedResponse_HappyPath checks the float matrix is
// preserved verbatim and meta.billed_units.input_tokens comes from
// canonical usage.prompt_tokens.
func TestCanonicalToEmbedResponse_HappyPath(t *testing.T) {
	canonical := []byte(`{
        "object": "list",
        "data": [
            {"object":"embedding","embedding":[0.1, -0.2, 0.3],"index":0},
            {"object":"embedding","embedding":[0.4, 0.5],"index":1}
        ],
        "model": "embed-english-v3.0",
        "usage": {"prompt_tokens": 5, "total_tokens": 5}
    }`)
	out, err := CanonicalToEmbedResponse(canonical)
	if err != nil {
		t.Fatalf("CanonicalToEmbedResponse: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("response not valid JSON: %v\n%s", err, out)
	}
	if got["response_type"] != "embeddings_floats" {
		t.Fatalf("response_type: %v", got["response_type"])
	}
	emb, ok := got["embeddings"].([]any)
	if !ok || len(emb) != 2 {
		t.Fatalf("embeddings: %v", got["embeddings"])
	}
	row0 := emb[0].([]any)
	if !reflect.DeepEqual([]any{0.1, -0.2, 0.3}, row0) {
		t.Fatalf("row[0]: %v", row0)
	}
	meta := got["meta"].(map[string]any)
	billed := meta["billed_units"].(map[string]any)
	if got, want := billed["input_tokens"], float64(5); got != want {
		t.Fatalf("billed_units.input_tokens: got %v, want %v", got, want)
	}
}

func TestCanonicalToEmbedResponse_OptionalFields(t *testing.T) {
	t.Run("includes id when present in canonical", func(t *testing.T) {
		canonical := []byte(`{"id":"req-abc","data":[{"object":"embedding","embedding":[0.1],"index":0}]}`)
		out, err := CanonicalToEmbedResponse(canonical)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"id":"req-abc"`) {
			t.Fatalf("id should pass through: %s", out)
		}
	})
	t.Run("no meta when usage absent", func(t *testing.T) {
		canonical := []byte(`{"data":[{"object":"embedding","embedding":[0.1],"index":0}]}`)
		out, err := CanonicalToEmbedResponse(canonical)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), `"meta"`) {
			t.Fatalf("meta should be omitted when usage absent: %s", out)
		}
	})
}

func TestCanonicalToEmbedResponse_Errors(t *testing.T) {
	cases := []struct {
		name       string
		canonical  string
		wantErrSub string
	}{
		{
			name:       "invalid JSON",
			canonical:  `not-json`,
			wantErrSub: "invalid canonical body",
		},
		{
			name:       "missing data",
			canonical:  `{"model":"m"}`,
			wantErrSub: "missing data[]",
		},
		{
			name:       "data entry without embedding",
			canonical:  `{"data":[{"object":"embedding","index":0}]}`,
			wantErrSub: "data[].embedding missing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CanonicalToEmbedResponse([]byte(tc.canonical))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.wantErrSub)
			}
		})
	}
}
