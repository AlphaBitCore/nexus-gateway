package canonicalbridge

import (
	"fmt"
	"strings"
	"testing"

	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// The rerank canonical is Cohere-shaped; the resolved target used for the
// field-level 400 attribution is the Voyage translate leg.
var rrCT = provcore.CallTarget{Format: provcore.FormatVoyage, ProviderModelID: "rerank-2"}

func TestRerankRoutable(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	cases := []struct {
		name            string
		ingress, target provcore.Format
		want            bool
	}{
		// Cohere is the ONLY rerank ingress (canonical == Cohere wire).
		{"native cohere passthrough", provcore.FormatCohere, provcore.FormatCohere, true},
		{"cohere to voyage (the translate leg)", provcore.FormatCohere, provcore.FormatVoyage, true},
		// Non-Cohere ingress has no rerank canonical — reject regardless of target.
		{"openai ingress has no rerank canonical", provcore.FormatOpenAI, provcore.FormatCohere, false},
		{"gemini ingress has no rerank canonical", provcore.FormatGemini, provcore.FormatVoyage, false},
		// Targets with no opened rerank leg are dead failover legs — never advertise.
		{"openai target has no rerank leg", provcore.FormatCohere, provcore.FormatOpenAI, false},
		{"anthropic target has no rerank leg", provcore.FormatCohere, provcore.FormatAnthropic, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := b.RerankRoutable(c.ingress, c.target); got != c.want {
				t.Fatalf("RerankRoutable(%s, %s) = %v, want %v", c.ingress, c.target, got, c.want)
			}
		})
	}
	// The Voyage leg requires its codec to be registered — an empty registry
	// must not claim a routability it cannot serve.
	empty := New(map[provcore.Format]provcore.SchemaCodec{})
	if empty.RerankRoutable(provcore.FormatCohere, provcore.FormatVoyage) {
		t.Fatal("Voyage rerank leg routable without a registered codec")
	}
	// Cohere→Cohere is native passthrough and needs no registered codec.
	if !empty.RerankRoutable(provcore.FormatCohere, provcore.FormatCohere) {
		t.Fatal("Cohere native passthrough must stay routable without a codec")
	}
}

func TestEndpointRoutable_RerankKind(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	if !b.EndpointRoutable(typology.WireShapeCohereRerank, provcore.FormatCohere, provcore.FormatVoyage) {
		t.Fatal("rerank kind must route cohere→voyage through RerankRoutable")
	}
	if b.EndpointRoutable(typology.WireShapeCohereRerank, provcore.FormatCohere, provcore.FormatOpenAI) {
		t.Fatal("rerank kind must not route to a format with no rerank leg")
	}
}

func TestRerankWireShapeForTarget(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	if got := b.RerankWireShapeForTarget(provcore.FormatCohere); got != typology.WireShapeCohereRerank {
		t.Fatalf("cohere → %v, want cohere-rerank", got)
	}
	if got := b.RerankWireShapeForTarget(provcore.FormatVoyage); got != typology.WireShapeVoyageRerank {
		t.Fatalf("voyage → %v, want voyage-rerank", got)
	}
	if got := b.RerankWireShapeForTarget(provcore.FormatOpenAI); got != typology.WireShapeNone {
		t.Fatalf("openai (no rerank leg) → %v, want the None sentinel", got)
	}
}

func TestIngressRerankToCanonical_ValidationContract(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	// 1001 non-empty documents to trip the rerankMaxDocuments (1000) ceiling.
	overCap := make([]string, 1001)
	for i := range overCap {
		overCap[i] = fmt.Sprintf("%q", "d")
	}
	overCapBody := `{"model":"m","query":"q","documents":[` + strings.Join(overCap, ",") + `]}`

	cases := []struct {
		name    string
		body    string
		wantErr string // substring; "" = valid
	}{
		{"valid minimal is identity", `{"model":"m","query":"q","documents":["a","b"]}`, ""},
		{"valid with positive top_n", `{"model":"m","query":"q","documents":["a"],"top_n":1}`, ""},
		{"non-object body", `[1,2]`, "JSON object"},
		{"model missing", `{"query":"q","documents":["a"]}`, `"model"`},
		{"model empty", `{"model":"","query":"q","documents":["a"]}`, `"model"`},
		{"query missing", `{"model":"m","documents":["a"]}`, `"query"`},
		{"query empty", `{"model":"m","query":"","documents":["a"]}`, `"query"`},
		{"documents not an array", `{"model":"m","query":"q","documents":"a"}`, `"documents"`},
		{"documents empty", `{"model":"m","query":"q","documents":[]}`, `"documents"`},
		{"documents over the 1000 cap", overCapBody, `"documents"`},
		{"document element non-string", `{"model":"m","query":"q","documents":[1]}`, `"documents"`},
		{"document element empty string", `{"model":"m","query":"q","documents":[""]}`, `"documents"`},
		{"top_n non-positive", `{"model":"m","query":"q","documents":["a"],"top_n":0}`, `"top_n"`},
		{"top_n fractional", `{"model":"m","query":"q","documents":["a"],"top_n":1.5}`, `"top_n"`},
		{"top_n string", `{"model":"m","query":"q","documents":["a"],"top_n":"3"}`, `"top_n"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := b.IngressRerankToCanonical(provcore.FormatCohere, []byte(c.body), rrCT)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				if string(out) != c.body {
					t.Fatalf("canonicalize must be identity, got %s", out)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %s, got %v", c.wantErr, err)
			}
			// Field-level rejections name the resolved provider/model (routing
			// can rewrite the model mid-flight); body-shape errors have no field.
			if strings.HasPrefix(c.wantErr, `"`) && !strings.Contains(err.Error(), "rerank-2") {
				t.Fatalf("error %q does not name the resolved model", err)
			}
		})
	}
	// Non-Cohere ingress has no rerank canonical at all.
	_, err := b.IngressRerankToCanonical(provcore.FormatOpenAI, []byte(`{"model":"m","query":"q","documents":["a"]}`), rrCT)
	if err == nil || !strings.Contains(err.Error(), "no rerank canonical") {
		t.Fatalf("non-Cohere rerank ingress must be rejected, got %v", err)
	}
}

func TestIngressRerankToWire_VoyageLeg(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	body := []byte(`{"model":"m","query":"what is a fox","documents":["a red fox","a blue whale"],"top_n":1}`)
	wire, rewrites, err := b.IngressRerankToWire(provcore.FormatCohere, provcore.FormatVoyage, body, rrCT)
	if err != nil {
		t.Fatalf("to wire: %v", err)
	}
	w := gjson.ParseBytes(wire)
	if got := w.Get("query").String(); got != "what is a fox" {
		t.Fatalf("wire query = %q", got)
	}
	if got := w.Get("documents.0").String(); got != "a red fox" {
		t.Fatalf("wire documents.0 = %q", got)
	}
	// The canonical `top_n` must translate to the Voyage `top_k` field name.
	if got := w.Get("top_k").Int(); got != 1 {
		t.Fatalf("wire top_k = %d, want 1 (canonical top_n → voyage top_k)", got)
	}
	if w.Get("top_n").Exists() {
		t.Fatalf("canonical top_n must not survive onto the Voyage wire: %s", wire)
	}
	// The Voyage rerank codec records no coercions.
	if rewrites != nil {
		t.Fatalf("voyage rerank leg should return nil rewrites, got %v", rewrites)
	}
}

// TestIngressRerankToWire_ValidatesOnFailoverLane pins the failover-lane
// guarantee: IngressRerankToWire runs IngressRerankToCanonical BEFORE encoding,
// so a body that never met the prepare-stage validation (primary was native
// Cohere) still cannot reach the Voyage wire unvalidated.
func TestIngressRerankToWire_ValidatesOnFailoverLane(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	for name, tc := range map[string]struct{ body, wantErr string }{
		"empty documents":    {`{"model":"m","query":"q","documents":[]}`, `"documents"`},
		"missing query":      {`{"model":"m","documents":["a"]}`, `"query"`},
		"non-string element": {`{"model":"m","query":"q","documents":[1]}`, `"documents"`},
		"bad top_n":          {`{"model":"m","query":"q","documents":["a"],"top_n":0}`, `"top_n"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := b.IngressRerankToWire(provcore.FormatCohere, provcore.FormatVoyage, []byte(tc.body), rrCT)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("invalid body must fail validation before the Voyage wire, want %s got %v", tc.wantErr, err)
			}
		})
	}
}

func TestIngressRerankToWire_SameFormatPassthroughAndMissingCodec(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	body := []byte(`{"model":"m","query":"q","documents":["a"]}`)
	// ingress == target: identity passthrough, nil rewrites, no validation.
	out, rewrites, err := b.IngressRerankToWire(provcore.FormatCohere, provcore.FormatCohere, body, rrCT)
	if err != nil || string(out) != string(body) || rewrites != nil {
		t.Fatalf("same-format must be identity passthrough, got %s %v %v", out, rewrites, err)
	}
	// Cross-format to a target with no registered codec must error with "no codec".
	empty := New(map[provcore.Format]provcore.SchemaCodec{})
	_, _, err = empty.IngressRerankToWire(provcore.FormatCohere, provcore.FormatVoyage, body, rrCT)
	if err == nil || !strings.Contains(err.Error(), "no codec") {
		t.Fatalf("missing target codec must error with 'no codec', got %v", err)
	}
}
