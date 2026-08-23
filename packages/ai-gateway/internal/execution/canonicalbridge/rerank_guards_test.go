package canonicalbridge

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// ValidateRerankIngressGuards runs on the passthrough leg, where the ingress
// body goes upstream verbatim. It must enforce what protects THIS gateway —
// chiefly the documents ceiling, since rerank bills a search unit per 100
// documents — while leaving the provider's own shape latitude alone.
func TestValidateRerankIngressGuards(t *testing.T) {
	b := New(nil)
	ct := provcore.CallTarget{Format: provcore.FormatCohere, ProviderModelID: "rerank-v3.5"}
	docs := func(n int, tmpl string) string {
		out := make([]string, n)
		for i := range out {
			out[i] = tmpl
		}
		return "[" + strings.Join(out, ",") + "]"
	}

	cases := []struct {
		name    string
		ingress provcore.Format
		body    string
		wantErr string // "" = must pass
	}{
		{"strings within the ceiling", provcore.FormatCohere,
			`{"model":"m","query":"q","documents":["a","b"]}`, ""},
		// Cohere serves these; rejecting them here would 400 a body the
		// provider would have answered.
		{"object documents are the provider's business", provcore.FormatCohere,
			`{"model":"m","query":"q","documents":[{"text":"a"},{"text":"b"}]}`, ""},
		{"top_n present and valid", provcore.FormatCohere,
			`{"model":"m","query":"q","documents":["a"],"top_n":1}`, ""},

		{"over the billing ceiling", provcore.FormatCohere,
			`{"model":"m","query":"q","documents":` + docs(1001, `"d"`) + `}`, "1..1000"},
		{"over the ceiling with object documents", provcore.FormatCohere,
			`{"model":"m","query":"q","documents":` + docs(1001, `{"text":"d"}`) + `}`, "1..1000"},
		{"empty documents", provcore.FormatCohere,
			`{"model":"m","query":"q","documents":[]}`, "1..1000"},
		{"documents not an array", provcore.FormatCohere,
			`{"model":"m","query":"q","documents":"a"}`, "must be an array"},
		{"missing model", provcore.FormatCohere,
			`{"query":"q","documents":["a"]}`, `"model"`},
		{"missing query", provcore.FormatCohere,
			`{"model":"m","documents":["a"]}`, `"query"`},
		{"top_n not a positive integer", provcore.FormatCohere,
			`{"model":"m","query":"q","documents":["a"],"top_n":0}`, `"top_n"`},
		{"body not an object", provcore.FormatCohere, `[]`, "must be a JSON object"},
		{"wrong ingress format", provcore.FormatOpenAI,
			`{"model":"m","query":"q","documents":["a"]}`, "Cohere-shaped only"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := b.ValidateRerankIngressGuards(tc.ingress, []byte(tc.body), ct)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("want accepted, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("want rejected naming %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q does not name %q — the caller cannot tell what to change", err, tc.wantErr)
			}
		})
	}

}
