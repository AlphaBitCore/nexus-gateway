package canonicalbridge

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

func TestIngressGeminiToVertex_IdenticalCallsPreserveCoordinatesSignaturesAndFIFO(t *testing.T) {
	b := testBridge(t)
	body := []byte(`{"contents":[` +
		`{"role":"model","parts":[` +
		`{"functionCall":{"name":"lookup","args":{"q":"same"}},"thoughtSignature":"sig-a"},` +
		`{"functionCall":{"name":"lookup","args":{"q":"same"}},"thoughtSignature":"sig-b"}]},` +
		`{"role":"user","parts":[` +
		`{"functionResponse":{"name":"lookup","response":{"r":1}}},` +
		`{"functionResponse":{"name":"lookup","response":{"r":2}}}]}]}`)
	ct := dummyCallTarget(provcore.FormatVertex)
	wire, _, err := b.IngressChatToWire(provcore.FormatGemini, provcore.FormatVertex, body, ct, false)
	if err != nil {
		t.Fatalf("Gemini→Vertex bridge: %v", err)
	}
	callA := gjson.GetBytes(wire, "contents.0.parts.0.functionCall")
	callB := gjson.GetBytes(wire, "contents.0.parts.1.functionCall")
	if callA.Get("id").String() == "" || callA.Get("id").String() == callB.Get("id").String() {
		t.Fatalf("identical calls need distinct stable IDs: %s", wire)
	}
	if got := gjson.GetBytes(wire, "contents.0.parts.0.thoughtSignature").String(); got != "sig-a" {
		t.Fatalf("first thoughtSignature=%q, want sig-a: %s", got, wire)
	}
	if got := gjson.GetBytes(wire, "contents.0.parts.1.thoughtSignature").String(); got != "sig-b" {
		t.Fatalf("second thoughtSignature=%q, want sig-b: %s", got, wire)
	}
	if got := gjson.GetBytes(wire, "contents.1.parts.0.functionResponse.id").String(); got != callA.Get("id").String() {
		t.Fatalf("first response id=%q, want %q: %s", got, callA.Get("id").String(), wire)
	}
	if got := gjson.GetBytes(wire, "contents.2.parts.0.functionResponse.id").String(); got != callB.Get("id").String() {
		t.Fatalf("second response id=%q, want %q: %s", got, callB.Get("id").String(), wire)
	}
	wire2, _, err := b.IngressChatToWire(provcore.FormatGemini, provcore.FormatVertex, body, ct, false)
	if err != nil {
		t.Fatalf("Gemini→Vertex bridge replay: %v", err)
	}
	if got, want := gjson.GetBytes(wire2, "contents.0.parts.0.functionCall.id").String(), callA.Get("id").String(); got != want {
		t.Fatalf("replay changed first id: %q vs %q", got, want)
	}
}
