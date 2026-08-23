package cohere

import (
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The model this wire is told to call is the one ROUTING resolved, never the
// one the caller typed.
//
// The two are routinely different — that is what routing is — and the caller's
// word is not a name any upstream knows. Forwarding it produces a 404 from the
// provider naming a model nobody asked for, which is the shape a caller reads
// as "the gateway is broken" and an operator reads as "the model was deleted".
//
// The embeddings leg of this same adapter already reads this way; these are the
// two legs that did not.
func TestCohere_EncodeRequest_SendsTheResolvedModelNotTheCallersWord(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape typology.WireShape
		body  string
	}{
		{
			"chat", typology.WireShapeCohereChat,
			`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			"rerank", typology.WireShapeCohereRerank,
			`{"model":"auto","query":"q","documents":["a","b"]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codec{}.EncodeRequest(tc.shape, []byte(tc.body),
				provcore.CallTarget{ProviderModelID: "command-r7b-12-2024"})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if m := gjson.GetBytes(got.Body, "model").String(); m != "command-r7b-12-2024" {
				t.Fatalf("wire model = %q, want the resolved command-r7b-12-2024 — the caller's "+
					"word reached the upstream, which answers 404 for a model it has never heard of\n%s",
					m, got.Body)
			}
		})
	}
}

// A target that names no model leaves the caller's word alone: some callers
// address a provider's own model directly, and overwriting that with nothing
// would strip the only name the request had.
func TestCohere_EncodeRequest_KeepsTheCallersModelWhenTheTargetNamesNone(t *testing.T) {
	got, err := codec{}.EncodeRequest(typology.WireShapeCohereChat,
		[]byte(`{"model":"command-r-plus","messages":[{"role":"user","content":"hi"}]}`),
		provcore.CallTarget{})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if m := gjson.GetBytes(got.Body, "model").String(); m != "command-r-plus" {
		t.Fatalf("wire model = %q, want the caller's own command-r-plus", m)
	}
}

// The Cohere chat response carries no model field. The canonical response
// must name the model the call was MADE with — an auto-routed caller reading
// response.model otherwise gets "" and cannot see who answered.
func TestDecodeResponse_ModelBackfilledFromResolvedTarget(t *testing.T) {
	native := []byte(`{"id":"resp1","message":{"content":[{"type":"text","text":"Ready."}]},` +
		`"finish_reason":"COMPLETE","usage":{"tokens":{"input_tokens":3,"output_tokens":2}}}`)
	res, err := codec{}.DecodeResponse(typology.WireShapeCohereChat, native, "",
		provcore.DecodeContext{Target: provcore.CallTarget{ProviderModelID: "command-r7b-12-2024"}})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(res.CanonicalBody, "model").String(); got != "command-r7b-12-2024" {
		t.Fatalf("model = %q, want the resolved target — an empty model blinds auto callers", got)
	}
}

// A body that DOES report a model keeps its own word — the backfill is for
// absence, not an override.
func TestDecodeResponse_BodyModelStillWins(t *testing.T) {
	native := []byte(`{"id":"resp1","model":"command-futurist","message":{"content":[{"type":"text","text":"x"}]}}`)
	res, err := codec{}.DecodeResponse(typology.WireShapeCohereChat, native, "",
		provcore.DecodeContext{Target: provcore.CallTarget{ProviderModelID: "command-r7b-12-2024"}})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(res.CanonicalBody, "model").String(); got != "command-futurist" {
		t.Fatalf("model = %q, want the body's own value", got)
	}
}
