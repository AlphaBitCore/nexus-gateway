package canonicalbridge

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// stubEmbedCodec is a minimal SchemaCodec used to populate Bridge.codecs
// for routing-matrix tests. It does not implement real EncodeRequest /
// DecodeResponse logic — those paths are exercised in per-provider tests.
type stubEmbedCodec struct{}

func (stubEmbedCodec) EncodeRequest(_ typology.WireShape, body []byte, _ provcore.CallTarget) (provcore.EncodeResult, error) {
	return provcore.EncodeResult{Body: body, ContentType: "application/json"}, nil
}
func (stubEmbedCodec) DecodeResponse(_ typology.WireShape, body []byte, _ string, _ provcore.DecodeContext) (provcore.DecodeResult, error) {
	return provcore.DecodeResult{CanonicalBody: body}, nil
}

func newEmbedBridge() *Bridge {
	codecs := map[provcore.Format]provcore.SchemaCodec{
		provcore.FormatOpenAI:      stubEmbedCodec{},
		provcore.FormatAzureOpenAI: stubEmbedCodec{},
		provcore.FormatCohere:      stubEmbedCodec{},
		provcore.FormatGemini:      stubEmbedCodec{},
		provcore.FormatVertex:      stubEmbedCodec{},
	}
	return New(codecs)
}

func TestEmbeddingsRoutable_Matrix(t *testing.T) {
	b := newEmbedBridge()
	in := []provcore.Format{
		provcore.FormatOpenAI, provcore.FormatAzureOpenAI,
		provcore.FormatCohere, provcore.FormatGemini, provcore.FormatVertex,
	}
	for _, i := range in {
		for _, o := range in {
			if !b.EmbeddingsRoutable(i, o) {
				t.Errorf("EmbeddingsRoutable(%v,%v) want true (every in-scope pair)", i, o)
			}
			if !b.EndpointRoutable(typology.WireShapeOpenAIEmbeddings, i, o) {
				t.Errorf("EndpointRoutable(embeddings, %v, %v) want true", i, o)
			}
		}
	}
}

func TestEmbeddingsRoutable_RejectsUnregisteredFormat(t *testing.T) {
	b := newEmbedBridge()
	// Anthropic codec not present in the test bridge → reject.
	if b.EmbeddingsRoutable(provcore.FormatAnthropic, provcore.FormatOpenAI) {
		t.Fatal("unregistered ingress should route false")
	}
	if b.EmbeddingsRoutable(provcore.FormatOpenAI, provcore.FormatAnthropic) {
		t.Fatal("unregistered target should route false")
	}
}

func TestEmbeddingsRoutable_RejectsOutOfScopeFormat(t *testing.T) {
	// Register every codec including Bedrock — but EmbeddingsRoutable
	// should still reject Bedrock since the canonical helpers do not
	// cover it (in-scope list: OpenAI / Azure / Cohere / Gemini /
	// Vertex).
	codecs := map[provcore.Format]provcore.SchemaCodec{
		provcore.FormatOpenAI:      stubEmbedCodec{},
		provcore.FormatAzureOpenAI: stubEmbedCodec{},
		provcore.FormatCohere:      stubEmbedCodec{},
		provcore.FormatGemini:      stubEmbedCodec{},
		provcore.FormatBedrock:     stubEmbedCodec{},
	}
	b := New(codecs)
	if b.EmbeddingsRoutable(provcore.FormatBedrock, provcore.FormatOpenAI) {
		t.Fatal("out-of-scope ingress should route false")
	}
	if b.EmbeddingsRoutable(provcore.FormatOpenAI, provcore.FormatBedrock) {
		t.Fatal("out-of-scope target should route false")
	}
}

func TestEmbeddingsRoutable_SameFormatAlwaysPass(t *testing.T) {
	b := newEmbedBridge()
	for _, f := range []provcore.Format{
		provcore.FormatOpenAI, provcore.FormatCohere, provcore.FormatGemini,
	} {
		if !b.EmbeddingsRoutable(f, f) {
			t.Errorf("EmbeddingsRoutable(%v,%v) self-routing must be true", f, f)
		}
	}
	// Invalid format short-circuits before codec lookup.
	if b.EmbeddingsRoutable("", provcore.FormatOpenAI) {
		t.Fatal("empty format should route false")
	}
}

func TestIngressEmbeddingsToCanonical_IdentityForOpenAILike(t *testing.T) {
	b := newEmbedBridge()
	body := []byte(`{"model":"text-embedding-3-small","input":"hi"}`)
	out, err := b.IngressEmbeddingsToCanonical(provcore.FormatOpenAI, body, provcore.CallTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatalf("OpenAI ingress should be identity: %s", out)
	}
}

func TestIngressEmbeddingsToCanonical_Cohere(t *testing.T) {
	b := newEmbedBridge()
	body := []byte(`{"texts":["x"],"model":"embed-english-v3.0","input_type":"search_query"}`)
	out, err := b.IngressEmbeddingsToCanonical(provcore.FormatCohere, body, provcore.CallTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "model").Str; got != "embed-english-v3.0" {
		t.Fatalf("model: %q", got)
	}
	if got := gjson.GetBytes(out, "input").Str; got != "x" {
		t.Fatalf("input: %q", got)
	}
	if got := gjson.GetBytes(out, "nexus.ext.cohere.input_type").Str; got != "search_query" {
		t.Fatalf("input_type ext: %q", got)
	}
}

func TestIngressEmbeddingsToCanonical_GeminiSingleVsBatch(t *testing.T) {
	b := newEmbedBridge()
	t.Run("single via :embedContent shape", func(t *testing.T) {
		body := []byte(`{"content":{"parts":[{"text":"hi"}]}}`)
		out, err := b.IngressEmbeddingsToCanonical(provcore.FormatGemini, body, provcore.CallTarget{ProviderModelID: "models/text-embedding-004"})
		if err != nil {
			t.Fatal(err)
		}
		if batch := gjson.GetBytes(out, "nexus.ext.gemini.batch").Bool(); batch != false {
			t.Fatalf("batch should be false: %v", batch)
		}
	})
	t.Run("batch via :batchEmbedContents shape", func(t *testing.T) {
		body := []byte(`{"requests":[
            {"content":{"parts":[{"text":"a"}]}},
            {"content":{"parts":[{"text":"b"}]}}
        ]}`)
		out, err := b.IngressEmbeddingsToCanonical(provcore.FormatVertex, body, provcore.CallTarget{ProviderModelID: "models/text-embedding-004"})
		if err != nil {
			t.Fatal(err)
		}
		if batch := gjson.GetBytes(out, "nexus.ext.gemini.batch").Bool(); batch != true {
			t.Fatalf("batch should be true: %v", batch)
		}
	})
}

func TestIngressEmbeddingsToCanonical_UnsupportedFormat(t *testing.T) {
	b := newEmbedBridge()
	_, err := b.IngressEmbeddingsToCanonical(provcore.FormatAnthropic, []byte(`{}`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("want error for unsupported ingress format")
	}
	if !strings.Contains(err.Error(), "no embeddings hub codec") {
		t.Fatalf("error: %v", err)
	}
}

func TestIngressEmbeddingsToWire_SameFormatPassthrough(t *testing.T) {
	b := newEmbedBridge()
	body := []byte(`{"texts":["x"],"model":"embed-english-v3.0"}`)
	out, override, err := b.IngressEmbeddingsToWire(provcore.FormatCohere, provcore.FormatCohere, body, provcore.CallTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatalf("same-format should passthrough: %s", out)
	}
	if override != "" {
		t.Fatalf("same-format passthrough must emit no URL override, got %q", override)
	}
}

func TestIngressEmbeddingsToWire_CrossFormat(t *testing.T) {
	b := newEmbedBridge()
	body := []byte(`{"texts":["hi"],"model":"embed-english-v3.0","input_type":"search_query"}`)
	out, _, err := b.IngressEmbeddingsToWire(provcore.FormatCohere, provcore.FormatOpenAI, body, provcore.CallTarget{})
	if err != nil {
		t.Fatal(err)
	}
	// stubEmbedCodec is identity, so the wire body is the canonical body.
	if got := gjson.GetBytes(out, "model").Str; got != "embed-english-v3.0" {
		t.Fatalf("model: %q", got)
	}
	if got := gjson.GetBytes(out, "input").Str; got != "hi" {
		t.Fatalf("input: %q", got)
	}
}

func TestIngressEmbeddingsToWire_NoCodec(t *testing.T) {
	b := newEmbedBridge()
	_, _, err := b.IngressEmbeddingsToWire(provcore.FormatOpenAI, provcore.FormatAnthropic, []byte(`{}`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("want error for missing target codec")
	}
	if !strings.Contains(err.Error(), "no codec for format") {
		t.Fatalf("error: %v", err)
	}
}

func TestIngressEmbeddingsToWire_InvalidCanonicalize(t *testing.T) {
	b := newEmbedBridge()
	// Invalid Cohere body — canonicalizer errors first.
	_, _, err := b.IngressEmbeddingsToWire(provcore.FormatCohere, provcore.FormatOpenAI, []byte(`not-json`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("want error from canonicalizer")
	}
}

func TestResponseCanonicalToIngressEmbeddings_OpenAIIdentity(t *testing.T) {
	b := newEmbedBridge()
	canonical := []byte(`{"object":"list","data":[]}`)
	out, err := b.ResponseCanonicalToIngressEmbeddings(provcore.FormatOpenAI, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(canonical) {
		t.Fatalf("identity expected: %s", out)
	}
}

func TestResponseCanonicalToIngressEmbeddings_Cohere(t *testing.T) {
	b := newEmbedBridge()
	canonical := []byte(`{"data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"usage":{"prompt_tokens":3,"total_tokens":3}}`)
	out, err := b.ResponseCanonicalToIngressEmbeddings(provcore.FormatCohere, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"embeddings_floats"`) {
		t.Fatalf("Cohere response_type missing: %s", out)
	}
}

func TestResponseCanonicalToIngressEmbeddings_GeminiBatchFlagDispatch(t *testing.T) {
	b := newEmbedBridge()
	t.Run("flag present + true → batch shape", func(t *testing.T) {
		canonical := []byte(`{"nexus":{"ext":{"gemini":{"batch":true}}},"data":[
            {"object":"embedding","embedding":[0.1],"index":0}
        ]}`)
		out, err := b.ResponseCanonicalToIngressEmbeddings(provcore.FormatGemini, canonical)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"embeddings"`) {
			t.Fatalf("batch response should use 'embeddings': %s", out)
		}
	})
	t.Run("flag present + false → single shape", func(t *testing.T) {
		canonical := []byte(`{"nexus":{"ext":{"gemini":{"batch":false}}},"data":[
            {"object":"embedding","embedding":[0.1],"index":0}
        ]}`)
		out, err := b.ResponseCanonicalToIngressEmbeddings(provcore.FormatGemini, canonical)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"embedding":`) {
			t.Fatalf("single response should use 'embedding' (singular): %s", out)
		}
	})
	t.Run("flag absent + 1 entry → single fallback", func(t *testing.T) {
		canonical := []byte(`{"data":[{"object":"embedding","embedding":[0.1],"index":0}]}`)
		out, err := b.ResponseCanonicalToIngressEmbeddings(provcore.FormatVertex, canonical)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"embedding":{"values":`) {
			t.Fatalf("fallback single shape expected: %s", out)
		}
	})
	t.Run("flag absent + N entries → batch fallback", func(t *testing.T) {
		canonical := []byte(`{"data":[
            {"object":"embedding","embedding":[0.1],"index":0},
            {"object":"embedding","embedding":[0.2],"index":1}
        ]}`)
		out, err := b.ResponseCanonicalToIngressEmbeddings(provcore.FormatGemini, canonical)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"embeddings":[`) {
			t.Fatalf("fallback batch shape expected: %s", out)
		}
	})
}

func TestResponseCanonicalToIngressEmbeddings_Unsupported(t *testing.T) {
	b := newEmbedBridge()
	_, err := b.ResponseCanonicalToIngressEmbeddings(provcore.FormatAnthropic, []byte(`{}`))
	if err == nil {
		t.Fatal("want error for unsupported ingress")
	}
	if !strings.Contains(err.Error(), "no embeddings response codec") {
		t.Fatalf("error: %v", err)
	}
}

func TestEmbedDataLen(t *testing.T) {
	cases := []struct {
		name      string
		canonical string
		want      int
	}{
		{"absent", `{}`, 0},
		{"not array", `{"data":"string"}`, 0},
		{"empty", `{"data":[]}`, 0},
		{"one", `{"data":[{}]}`, 1},
		{"three", `{"data":[{},{},{}]}`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := embedDataLen([]byte(tc.canonical)); got != tc.want {
				t.Fatalf("embedDataLen(%s): got %d, want %d", tc.canonical, got, tc.want)
			}
		})
	}
}

func TestHasJSONField(t *testing.T) {
	if hasJSONField([]byte(`{"a":1}`), "a") != true {
		t.Fatal("a should exist")
	}
	if hasJSONField([]byte(`{"a":1}`), "b") != false {
		t.Fatal("b should not exist")
	}
}
