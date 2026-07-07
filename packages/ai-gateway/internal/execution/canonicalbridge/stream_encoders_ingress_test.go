package canonicalbridge

import (
	"context"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// TestIngressStreamEncoder_NativeWireShape is the regression test for the
// cross-format streaming egress bug: the buffer / Model-A re-emit path
// (proxy.fallbackStreamEncoder) and the cross-format live transcoder must encode
// canonical chunks into the CALLER's ingress wire shape. A Gemini / Anthropic /
// Responses client must NEVER receive chat.completion.chunk frames — which is
// exactly what leaked when a non-OpenAI ingress streamed under an enforcing
// (redact/block) response scope.
func TestIngressStreamEncoder_NativeWireShape(t *testing.T) {
	cases := []struct {
		name        string
		ingress     provcore.Format
		mustHave    string // a marker proving the native wire shape
		mustNotHave string // the wrong wire shape that must never leak
	}{
		{"openai_chat", provcore.FormatOpenAI, "chat.completion.chunk", "response.output_text"},
		{"responses", provcore.FormatOpenAIResponses, "response.", "chat.completion.chunk"},
		{"anthropic", provcore.FormatAnthropic, "content_block", "chat.completion.chunk"},
		{"gemini", provcore.FormatGemini, "\"text\":\"hi\"", "chat.completion.chunk"},
		{"vertex", provcore.FormatVertex, "\"text\":\"hi\"", "chat.completion.chunk"},
		{"cohere", provcore.FormatCohere, "hi", "chat.completion.chunk"},
		{"replicate", provcore.FormatReplicate, "hi", "chat.completion.chunk"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enc := IngressStreamEncoder(c.ingress, "test-model")
			if enc == nil {
				t.Fatalf("IngressStreamEncoder(%s) returned nil — the buffer re-emit path must always have a concrete encoder", c.ingress)
			}
			// Drive a realistic short stream: a text delta then a terminal chunk,
			// concatenating all emitted bytes (encoders are stateful — preamble on
			// the first Write, terminal on the last).
			var out strings.Builder
			for _, chunk := range []provcore.Chunk{
				{Delta: "hi"},
				{Done: true, FinishReason: "stop"},
			} {
				b, err := enc.Write(context.Background(), chunk)
				if err != nil {
					t.Fatalf("%s: Write: %v", c.name, err)
				}
				out.Write(b)
			}
			s := out.String()
			if c.mustHave != "" && !strings.Contains(s, c.mustHave) {
				t.Fatalf("%s: emitted wire missing native marker %q; got:\n%s", c.name, c.mustHave, s)
			}
			if c.mustNotHave != "" && strings.Contains(s, c.mustNotHave) {
				t.Fatalf("%s: emitted wire LEAKED foreign shape %q to the client; got:\n%s", c.name, c.mustNotHave, s)
			}
		})
	}
}
