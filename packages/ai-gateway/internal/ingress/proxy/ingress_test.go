package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestIngress_ApplyHeaderOverride_OpenAIOnly(t *testing.T) {
	tests := []struct {
		name    string
		ingress Ingress
		header  string
		wantOK  bool
		wantFmt provcore.Format
	}{
		{
			name:    "openai no header keeps openai",
			ingress: Ingress{BodyFormat: provcore.FormatOpenAI},
			header:  "",
			wantOK:  true,
			wantFmt: provcore.FormatOpenAI,
		},
		{
			name:    "openai with valid header overrides",
			ingress: Ingress{BodyFormat: provcore.FormatOpenAI},
			header:  "anthropic",
			wantOK:  true,
			wantFmt: provcore.FormatAnthropic,
		},
		{
			name:    "openai with invalid header rejects",
			ingress: Ingress{BodyFormat: provcore.FormatOpenAI},
			header:  "banana",
			wantOK:  false,
			wantFmt: provcore.FormatOpenAI,
		},
		{
			name:    "anthropic ignores header",
			ingress: Ingress{BodyFormat: provcore.FormatAnthropic},
			header:  "openai",
			wantOK:  true,
			wantFmt: provcore.FormatAnthropic,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.header != "" {
				req.Header.Set("x-nexus-aigw-body-format", tc.header)
			}
			got, ok := tc.ingress.applyHeaderOverride(req)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got.BodyFormat != tc.wantFmt {
				t.Fatalf("BodyFormat = %q, want %q", got.BodyFormat, tc.wantFmt)
			}
		})
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
		{"unmapped-gemini", typology.WireShapeGeminiGenerateContent, provcore.Format(""), false},
		{"unmapped-vertex", typology.WireShapeVertexEmbedContent, provcore.Format(""), false},
		{"unmapped-bedrock", typology.WireShapeBedrockConverse, provcore.Format(""), false},
		{"unmapped-cohere", typology.WireShapeCohereEmbed, provcore.Format(""), false},
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
