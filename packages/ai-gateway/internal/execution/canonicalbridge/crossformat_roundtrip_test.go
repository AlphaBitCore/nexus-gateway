package canonicalbridge

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// TestWireShapeForTarget_APIMatchesHelper locks the public API getters added
// for the cross-format generalization (the proxy pre-cache prep and the
// executor resolve a target's native wire shape through these instead of
// leaking the caller's ingress shape into the target codec). If the helpers
// drift from the exported methods this fails.
func TestWireShapeForTarget_APIMatchesHelper(t *testing.T) {
	b := testBridge(t)
	for _, f := range provcore.AllFormats() {
		if got, want := b.ChatWireShapeForTarget(f), chatWireShapeForFormat(f); got != want {
			t.Errorf("ChatWireShapeForTarget(%q) = %q, want %q", f, got, want)
		}
		if got, want := b.EmbeddingsWireShapeForTarget(f), embeddingsWireShapeForFormat(f); got != want {
			t.Errorf("EmbeddingsWireShapeForTarget(%q) = %q, want %q", f, got, want)
		}
	}
}

// TestCrossFormatChatRoundTrip is the regression guard for the
// "ingress shape in = ingress shape out" contract that the proxy + executor
// cross-format fix relies on. For representative chat-kind ingress→target
// pairs it asserts:
//
//	request:  ingress body  → IngressChatToWire → non-empty target wire body
//	response: canonical resp → ResponseCanonicalToIngress → caller's ingress shape
//
// This is the unit-level companion to the previously-failing
// "unsupported endpoint \"openai-chat\" for codec" smoke cases
// (openai-chat → anthropic / gemini), plus the reverse ingresses.
func TestCrossFormatChatRoundTrip(t *testing.T) {
	b := testBridge(t)

	// Minimal canonical OpenAI chat.completion response (the hub shape every
	// adapter's DecodeResponse normalizes to before egress reshape).
	const canonicalResp = `{"id":"c","object":"chat.completion","model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

	cases := []struct {
		ingress, target provcore.Format
		respMarker      string // token expected in the reshaped ingress response
	}{
		// openai-chat ingress → non-OpenAI upstream; client still gets OpenAI shape.
		{provcore.FormatOpenAI, provcore.FormatAnthropic, "choices"},
		{provcore.FormatOpenAI, provcore.FormatGemini, "choices"},
		// /v1/messages ingress → OpenAI upstream; client gets Anthropic shape.
		{provcore.FormatAnthropic, provcore.FormatOpenAI, "content"},
		// gemini ingress → OpenAI upstream; client gets Gemini shape.
		{provcore.FormatGemini, provcore.FormatOpenAI, "candidates"},
		// same-format native (no translation) still round-trips.
		{provcore.FormatAnthropic, provcore.FormatAnthropic, "content"},
	}
	for _, c := range cases {
		name := string(c.ingress) + "_to_" + string(c.target)
		t.Run(name, func(t *testing.T) {
			body, err := MinimalNativeChatBody(c.ingress)
			if err != nil {
				t.Skipf("no native chat fixture for ingress %q: %v", c.ingress, err)
			}
			ct := provcore.CallTarget{Format: c.target, ProviderModelID: FixtureProviderModel(c.target)}

			wire, err := b.IngressChatToWire(c.ingress, c.target, body, ct, false)
			if err != nil {
				t.Fatalf("IngressChatToWire(%s): %v", name, err)
			}
			if len(wire) == 0 {
				t.Fatalf("IngressChatToWire(%s): empty target wire body", name)
			}

			shaped, err := b.ResponseCanonicalToIngress(c.ingress, []byte(canonicalResp))
			if err != nil {
				t.Fatalf("ResponseCanonicalToIngress(%s): %v", name, err)
			}
			if !strings.Contains(string(shaped), c.respMarker) {
				t.Errorf("response reshaped to ingress %q missing marker %q:\n%s", c.ingress, c.respMarker, shaped)
			}
		})
	}
}

// TestCrossFormatEmbeddingsRoundTrip locks the embeddings-kind cross-format
// path the proxy/executor now wire (previously the IngressEmbeddingsToWire /
// ResponseCanonicalToIngressEmbeddings bridge methods existed but had no
// callers). Request: OpenAI /v1/embeddings ingress → routable target wire.
// Response: canonical → caller's ingress embeddings shape.
func TestCrossFormatEmbeddingsRoundTrip(t *testing.T) {
	b := testBridge(t)

	const canonReq = `{"model":"text-embedding-3-small","input":"hello world"}`
	const canonResp = `{"object":"list","data":[{"object":"embedding","index":0,` +
		`"embedding":[0.1,0.2,0.3]}],"model":"m","usage":{"prompt_tokens":2,"total_tokens":2}}`

	// Request: OpenAI embeddings ingress → routable target's native wire.
	for _, target := range []provcore.Format{provcore.FormatGemini, provcore.FormatCohere, provcore.FormatOpenAI} {
		t.Run("req_openai_to_"+string(target), func(t *testing.T) {
			ct := provcore.CallTarget{Format: target, ProviderModelID: FixtureProviderModel(target)}
			wire, _, err := b.IngressEmbeddingsToWire(provcore.FormatOpenAI, target, []byte(canonReq), ct)
			if err != nil {
				t.Fatalf("IngressEmbeddingsToWire(openai→%s): %v", target, err)
			}
			if len(wire) == 0 {
				t.Fatalf("IngressEmbeddingsToWire(openai→%s): empty target wire body", target)
			}
		})
	}

	// Response: canonical embeddings → caller's ingress shape.
	for _, ingress := range []provcore.Format{provcore.FormatOpenAI, provcore.FormatGemini, provcore.FormatCohere} {
		t.Run("resp_to_"+string(ingress), func(t *testing.T) {
			shaped, err := b.ResponseCanonicalToIngressEmbeddings(ingress, []byte(canonResp))
			if err != nil {
				t.Fatalf("ResponseCanonicalToIngressEmbeddings(%s): %v", ingress, err)
			}
			if len(shaped) == 0 {
				t.Fatalf("ResponseCanonicalToIngressEmbeddings(%s): empty body", ingress)
			}
			if ingress == provcore.FormatOpenAI && !strings.Contains(string(shaped), "data") {
				t.Errorf("openai embeddings response should be identity (contain \"data\"): %s", shaped)
			}
		})
	}
}

// TestIngressEmbeddingsToWire_GeminiEndpointSelection is the unit-level guard
// for the Gemini embeddings codec selecting between the single
// (:embedContent) and batch (:batchEmbedContents) upstream endpoints purely via
// EncodeResult.URLOverride. IngressEmbeddingsToWire MUST surface that override
// (it was previously discarded, so a batch body — {"requests":[…]} — was POSTed
// to the single-embed URL and Gemini returned
// `Unknown name "requests": Cannot find field`). This asserts the override
// emitted by the bridge for an OpenAI /v1/embeddings ingress routed to a Gemini
// target, the only reachable embeddings→Gemini path (no Gemini embeddings
// ingress route exists, so the request is always cross-format).
func TestIngressEmbeddingsToWire_GeminiEndpointSelection(t *testing.T) {
	b := testBridge(t)
	ct := provcore.CallTarget{Format: provcore.FormatGemini, ProviderModelID: "gemini-embedding-001"}

	cases := []struct {
		name         string
		canonReq     string
		wantOverride string
		wantBodyHas  string // a marker the target wire body must contain
	}{
		{
			name:         "single_string_input",
			canonReq:     `{"model":"gemini-embedding-001","input":"hello"}`,
			wantOverride: ":embedContent",
			wantBodyHas:  `"content"`,
		},
		{
			name:         "batch_array_input",
			canonReq:     `{"model":"gemini-embedding-001","input":["a","b","c","d","e","f","g","h"]}`,
			wantOverride: ":batchEmbedContents",
			wantBodyHas:  `"requests"`,
		},
		{
			// A single-element array collapses to the single endpoint per the
			// codec's documented Gemini recommendation.
			name:         "single_element_array",
			canonReq:     `{"model":"gemini-embedding-001","input":["solo"]}`,
			wantOverride: ":embedContent",
			wantBodyHas:  `"content"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wire, override, err := b.IngressEmbeddingsToWire(provcore.FormatOpenAI, provcore.FormatGemini, []byte(c.canonReq), ct)
			if err != nil {
				t.Fatalf("IngressEmbeddingsToWire: %v", err)
			}
			if override != c.wantOverride {
				t.Fatalf("URLOverride = %q, want %q (body=%s)", override, c.wantOverride, wire)
			}
			if !strings.Contains(string(wire), c.wantBodyHas) {
				t.Fatalf("target wire body missing %q: %s", c.wantBodyHas, wire)
			}
		})
	}
}
