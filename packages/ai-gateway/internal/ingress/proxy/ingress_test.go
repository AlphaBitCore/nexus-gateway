package proxy

import (
	"context"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestIngress_WithContext_Roundtrip(t *testing.T) {
	in := Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatAnthropic,
	}
	ctx := WithIngress(context.Background(), in)
	got, ok := IngressFromContext(ctx)
	if !ok {
		t.Fatalf("IngressFromContext ok = false")
	}
	if got != in {
		t.Fatalf("got %+v, want %+v", got, in)
	}
}

func TestIngress_FromContext_Missing(t *testing.T) {
	got, ok := IngressFromContext(context.Background())
	if ok {
		t.Fatalf("IngressFromContext ok = true on empty ctx")
	}
	if got != (Ingress{}) {
		t.Fatalf("IngressFromContext returned non-zero on empty ctx: %+v", got)
	}
}

// TestWireShapeToBodyFormat covers every mapped wire shape returning
// (Format, ok=true) and the unmapped fall-through returning (empty, false).
func TestWireShapeToBodyFormat(t *testing.T) {
	cases := []struct {
		name string
		in   typology.WireShape
		want provcore.Format
		ok   bool
	}{
		{"openai-chat", typology.WireShapeOpenAIChat, provcore.FormatOpenAI, true},
		{"openai-completions-legacy", typology.WireShapeOpenAICompletionsLegacy, provcore.FormatOpenAI, true},
		{"openai-embeddings", typology.WireShapeOpenAIEmbeddings, provcore.FormatOpenAI, true},
		{"openai-responses", typology.WireShapeOpenAIResponses, provcore.FormatOpenAIResponses, true},
		{"anthropic-messages", typology.WireShapeAnthropicMessages, provcore.FormatAnthropic, true},
		{"gemini-generate", typology.WireShapeGeminiGenerateContent, provcore.FormatGemini, true},
		{"vertex-generate", typology.WireShapeVertexGenerateContent, provcore.FormatVertex, true},
		{"cohere-chat", typology.WireShapeCohereChat, provcore.FormatCohere, true},
		{"unmapped-vertex-embed", typology.WireShapeVertexEmbedContent, provcore.Format(""), false},
		{"unmapped-bedrock", typology.WireShapeBedrockConverse, provcore.Format(""), false},
		{"unmapped-cohere-embed", typology.WireShapeCohereEmbed, provcore.Format(""), false},
		{"unmapped-voyage", typology.WireShapeVoyageEmbeddings, provcore.Format(""), false},
		{"sentinel-none", typology.WireShapeNone, provcore.Format(""), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := WireShapeToBodyFormat(c.in)
			if got != c.want {
				t.Errorf("WireShapeToBodyFormat(%q) format = %q, want %q", c.in, got, c.want)
			}
			if ok != c.ok {
				t.Errorf("WireShapeToBodyFormat(%q) ok = %v, want %v", c.in, ok, c.ok)
			}
		})
	}
}

// TestResponsesUpgradeContext pins the Responses-API ctx-flag helpers.
func TestResponsesUpgradeContext(t *testing.T) {
	ctx := context.Background()
	if ResponsesUpgradeFromContext(ctx) {
		t.Error("zero-value ctx must report upgrade=false")
	}
	upgraded := WithResponsesUpgrade(ctx)
	if !ResponsesUpgradeFromContext(upgraded) {
		t.Error("WithResponsesUpgrade ctx must report upgrade=true")
	}
	// Original ctx unchanged (immutable child contexts).
	if ResponsesUpgradeFromContext(ctx) {
		t.Error("WithResponsesUpgrade must not mutate the parent ctx")
	}
}
