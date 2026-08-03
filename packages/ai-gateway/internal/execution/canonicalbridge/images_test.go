package canonicalbridge

import (
	"strings"
	"testing"

	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

var imgCT = provcore.CallTarget{Format: provcore.FormatGemini, ProviderModelID: "gemini-2.5-flash-image"}

func TestImagesRoutable(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	cases := []struct {
		name            string
		ingress, target provcore.Format
		want            bool
	}{
		{"native openai passthrough", provcore.FormatOpenAI, provcore.FormatOpenAI, true},
		// OpenAI-family SIBLINGS are their own demand-gated legs: the
		// identity codec has no images encode and the Azure transport no
		// images path — advertising them would create dead failover legs.
		{"azure sibling leg not opened in v1", provcore.FormatOpenAI, provcore.FormatAzureOpenAI, false},
		{"openai to gemini (the codec leg)", provcore.FormatOpenAI, provcore.FormatGemini, true},
		{"vertex leg is separately demand-gated", provcore.FormatOpenAI, provcore.FormatVertex, false},
		{"no anthropic image leg", provcore.FormatOpenAI, provcore.FormatAnthropic, false},
		{"only the OpenAI-shaped image ingress exists", provcore.FormatGemini, provcore.FormatGemini, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := b.ImagesRoutable(c.ingress, c.target); got != c.want {
				t.Fatalf("ImagesRoutable(%s, %s) = %v, want %v", c.ingress, c.target, got, c.want)
			}
		})
	}
	// The Gemini leg requires its codec to be registered — an empty
	// registry must not claim routability it cannot serve.
	empty := New(map[provcore.Format]provcore.SchemaCodec{})
	if empty.ImagesRoutable(provcore.FormatOpenAI, provcore.FormatGemini) {
		t.Fatal("Gemini image leg routable without a registered codec")
	}
}

func TestEndpointRoutable_ImagesKind(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	if !b.EndpointRoutable(typology.WireShapeOpenAIImages, provcore.FormatOpenAI, provcore.FormatGemini) {
		t.Fatal("images kind must route openai→gemini through ImagesRoutable")
	}
	if b.EndpointRoutable(typology.WireShapeOpenAIImages, provcore.FormatOpenAI, provcore.FormatAnthropic) {
		t.Fatal("images kind must not route to a format with no image leg")
	}
}

func TestImagesWireShapeForTarget(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	if got := b.ImagesWireShapeForTarget(provcore.FormatOpenAI); got != typology.WireShapeOpenAIImages {
		t.Fatalf("openai → %v", got)
	}
	if got := b.ImagesWireShapeForTarget(provcore.FormatAzureOpenAI); got != typology.WireShapeNone {
		t.Fatalf("azure (sibling leg not opened) → %v, want the None sentinel", got)
	}
	if got := b.ImagesWireShapeForTarget(provcore.FormatGemini); got != typology.WireShapeGeminiImagesGenerateContent {
		t.Fatalf("gemini → %v", got)
	}
	if got := b.ImagesWireShapeForTarget(provcore.FormatAnthropic); got != typology.WireShapeNone {
		t.Fatalf("anthropic (no image leg) → %v, want the None sentinel", got)
	}
}

func TestIngressImagesToCanonical_ValidationContract(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	cases := []struct {
		name    string
		body    string
		wantErr string // substring; "" = valid
	}{
		{"valid minimal is identity", `{"model":"m","prompt":"a fox"}`, ""},
		{"valid n in range", `{"prompt":"a fox","n":4}`, ""},
		{"prompt missing", `{"n":1}`, `"prompt"`},
		{"prompt empty", `{"prompt":""}`, `"prompt"`},
		{"prompt array (scanner scans elements; codec must not guess a join)", `{"prompt":["a","b"]}`, `"prompt"`},
		{"n above the fan-out cap", `{"prompt":"p","n":5}`, `"n"`},
		{"n fractional", `{"prompt":"p","n":1.5}`, `"n"`},
		{"n string", `{"prompt":"p","n":"3"}`, `"n"`},
		{"nexus channel rejected (smuggling path into the closed set)", `{"prompt":"p","nexus":{"ext":{}}}`, `"nexus"`},
		{"non-object body", `[1,2]`, "JSON object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := b.IngressImagesToCanonical(provcore.FormatOpenAI, []byte(c.body), imgCT)
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
			// Field-level rejections name the resolved provider/model
			// (routing can rewrite the model mid-flight); body-shape
			// errors have no field to attribute.
			if strings.HasPrefix(c.wantErr, `"`) && !strings.Contains(err.Error(), "gemini-2.5-flash-image") {
				t.Fatalf("error %q does not name the resolved model", err)
			}
		})
	}
	if _, err := b.IngressImagesToCanonical(provcore.FormatAnthropic, []byte(`{"prompt":"p"}`), imgCT); err == nil {
		t.Fatal("non-OpenAI image ingress must be rejected (no such ingress exists)")
	}
}

func TestIngressImagesToWire_GeminiLeg(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	body := []byte(`{"model":"img","prompt":"a fox","size":"1792x1024","quality":"hd"}`)
	wire, rewrites, err := b.IngressImagesToWire(provcore.FormatOpenAI, provcore.FormatGemini, body, imgCT)
	if err != nil {
		t.Fatalf("to wire: %v", err)
	}
	w := gjson.ParseBytes(wire)
	if got := w.Get("contents.0.parts.0.text").String(); got != "a fox" {
		t.Fatalf("wire prompt = %q", got)
	}
	if got := w.Get("generationConfig.imageConfig.aspectRatio").String(); got != "16:9" {
		t.Fatalf("aspectRatio = %q", got)
	}
	// The rewrites side-channel is what keeps x-nexus-coerced alive on
	// failover legs — it must surface the codec's coercions.
	joined := strings.Join(rewrites, ",")
	for _, want := range []string{"size:1792x1024→aspectRatio 16:9", "quality→dropped", "response_format:default→b64_json"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rewrites %v missing %q", rewrites, want)
		}
	}
}

// TestIngressImagesToWire_ValidatesOnFailoverLane pins the failover-lane
// guarantee: IngressImagesToWire validates via IngressImagesToCanonical
// BEFORE encoding, so a body that never met the prepare-stage validation
// (primary was native OpenAI) still cannot reach the Gemini wire with an
// out-of-bounds n / nexus key / non-string prompt.
func TestIngressImagesToWire_ValidatesOnFailoverLane(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	for name, body := range map[string]string{
		"n over cap":   `{"prompt":"p","n":50}`,
		"nexus key":    `{"prompt":"p","nexus":{}}`,
		"array prompt": `{"prompt":["a","b"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := b.IngressImagesToWire(provcore.FormatOpenAI, provcore.FormatGemini, []byte(body), imgCT); err == nil {
				t.Fatal("invalid body must not reach the Gemini wire via the failover lane")
			}
		})
	}
}

func TestIngressImagesToWire_SameFormatPassthroughAndMissingCodec(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	body := []byte(`{"prompt":"p"}`)
	out, rewrites, err := b.IngressImagesToWire(provcore.FormatOpenAI, provcore.FormatOpenAI, body, imgCT)
	if err != nil || string(out) != string(body) || rewrites != nil {
		t.Fatalf("same-format must be identity passthrough, got %s %v %v", out, rewrites, err)
	}
	empty := New(map[provcore.Format]provcore.SchemaCodec{})
	if _, _, err := empty.IngressImagesToWire(provcore.FormatOpenAI, provcore.FormatGemini, body, imgCT); err == nil {
		t.Fatal("missing target codec must error")
	}
}
