package proxy

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The gateway's `nexus` namespace must not reach the client, and the arm where
// that is at risk is the one that does the least work.
//
// Three of egressReshapeNonStream's arms return the body verbatim, and for an
// OpenAI-family caller the canonical body IS the client's shape — so on those
// arms whatever a codec left in the namespace is delivered. The other two arms
// are projections: they rebuild the client body out of named fields and drop
// unknown keys as a side effect of how they work, not as a rule anyone stated.
// Relying on that side effect is what let the leak exist, so the strip is
// applied to every arm's result and these cases pin each arm.
//
// The carrier that motivates it is nexus.ext.openai.responses.*, which the
// Responses decode writes and the Responses egress encoder consumes: an
// OpenAI-CHAT caller routed to a Responses-API target ends up holding a
// canonical body carrying a carrier nothing on its path will read.

const canonicalWithNamespace = `{"id":"chatcmpl-1","object":"chat.completion","model":"m",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13},` +
	`"nexus":{"ext":{"openai":{"responses":{"id":"resp_1","status":"completed"}}}}}`

func TestEgressReshapeNonStream_StripsTheNamespaceOnEveryArm(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ingress Ingress
	}{
		{
			// The identity arm — canonical is already the caller's shape, so
			// nothing else on this path would have removed it.
			name:    "openai-family identity",
			ingress: Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI},
		},
		{
			// A projection arm. It drops the namespace by construction; the
			// assertion states the requirement rather than trusting the shape
			// of the converter that happens to satisfy it today.
			name:    "anthropic projection",
			ingress: Ingress{WireShape: typology.WireShapeAnthropicMessages, BodyFormat: provcore.FormatAnthropic},
		},
		{
			name:    "gemini projection",
			ingress: Ingress{WireShape: typology.WireShapeGeminiGenerateContent, BodyFormat: provcore.FormatGemini},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{deps: &Deps{CanonicalBridge: canonicalbridge.New(nil)}}
			out, err := h.egressReshapeNonStream(tc.ingress, routingcore.RoutingTarget{}, []byte(canonicalWithNamespace))
			if err != nil {
				t.Fatalf("egressReshapeNonStream: %v", err)
			}
			if strings.Contains(string(out), `"nexus"`) {
				t.Errorf("the gateway's namespace reached the client: %s", out)
			}
			// The strip must cost the caller nothing else: the answer itself
			// still has to be there.
			if !strings.Contains(string(out), "hi") {
				t.Errorf("the assistant's content was lost with the namespace: %s", out)
			}
		})
	}
}

// A body with no namespace must come back byte-identical. The strip is a
// removal, not a re-serialisation — a rewritten body would churn allocations on
// every response and could reorder keys a client compares.
func TestEgressReshapeNonStream_CleanBodyIsUntouched(t *testing.T) {
	clean := []byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	h := &Handler{deps: &Deps{CanonicalBridge: canonicalbridge.New(nil)}}
	out, err := h.egressReshapeNonStream(
		Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI},
		routingcore.RoutingTarget{}, clean)
	if err != nil {
		t.Fatalf("egressReshapeNonStream: %v", err)
	}
	if string(out) != string(clean) {
		t.Errorf("clean body was rewritten:\n got %s\nwant %s", out, clean)
	}
}

// A `nexus`-named property inside a caller's own tool schema is the caller's
// data. An earlier version of this removal matched the reserved name at any
// depth and deleted such a property while leaving `required` pointing at it —
// an invalid schema, a model answering about a contract it never received, and
// HTTP 200 with every byte-level assertion green.
func TestEgressReshapeNonStream_LeavesCallerNexusKeysAlone(t *testing.T) {
	body := []byte(`{"id":"c","object":"chat.completion",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"{\"nexus_id\":\"a\"}"},"finish_reason":"stop"}],` +
		`"nexus":{"ext":{"openai":{"responses":{"id":"r"}}}}}`)
	h := &Handler{deps: &Deps{CanonicalBridge: canonicalbridge.New(nil)}}
	out, err := h.egressReshapeNonStream(
		Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI},
		routingcore.RoutingTarget{}, body)
	if err != nil {
		t.Fatalf("egressReshapeNonStream: %v", err)
	}
	if strings.Contains(string(out), `"ext"`) {
		t.Errorf("the gateway's namespace survived: %s", out)
	}
	if !strings.Contains(string(out), `nexus_id`) {
		t.Errorf("the model's own answer was mutated: %s", out)
	}
}
