package proxy

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// A bridge whose every method panics: the embedded interface is nil, so any
// call at all is a nil dereference. That is stronger than counting calls —
// it proves the rerank path consults the bridge for nothing.
type untouchableBridge struct{ canonicalbridge.API }

// Rerank reached production unable to answer a single request, and it broke
// twice for one reason: an endpoint was added to the route table and the
// layers beneath it were never taught the shape. The transport could not build
// a URL; once it could, the egress reshape handed a results[] body to the chat
// encoder and returned 502.
//
// Rerank's canonical shape IS its wire shape — OpenAI ships no rerank API, so
// Cohere's was adopted as canonical — which makes the correct reshape the
// identity, and makes any bridge call evidence of the bug.
func TestEgressReshapeNonStream_RerankIsIdentityAndTouchesNoBridge(t *testing.T) {
	h := &Handler{deps: &Deps{CanonicalBridge: untouchableBridge{}}}

	body := []byte(`{"results":[{"index":0,"relevance_score":0.98}],"meta":{"billed_units":{"search_units":1}}}`)
	ingress := Ingress{WireShape: typology.WireShapeCohereRerank, BodyFormat: provcore.FormatCohere}

	got, err := h.egressReshapeNonStream(ingress, routingcore.RoutingTarget{}, body)
	if err != nil {
		t.Fatalf("reshape returned %v; a rerank body must pass through untouched", err)
	}
	if string(got) != string(body) {
		t.Errorf("body was rewritten:\n got %s\nwant %s", got, body)
	}
}

// The premise the branch rests on. If this mapping ever changes, the branch
// above stops matching and rerank silently returns to the chat encoder.
func TestRerankWireShapeMapsToRerankKind(t *testing.T) {
	if got := typology.KindFromWireShape(typology.WireShapeCohereRerank); got != typology.EndpointKindRerank {
		t.Errorf("KindFromWireShape(cohere-rerank) = %q, want %q", got, typology.EndpointKindRerank)
	}
}
