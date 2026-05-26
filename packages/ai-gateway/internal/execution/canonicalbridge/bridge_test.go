package canonicalbridge

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestChatWireShapeForFormat exhaustively covers every provcore.Format value,
// asserting the projection to the adapter's native chat wire shape. New chat
// adapter Formats MUST be added to the case list — this test is the lockstep
// gate that fails if the helper drifts.
func TestChatWireShapeForFormat(t *testing.T) {
	cases := []struct {
		f    provcore.Format
		want typology.WireShape
	}{
		// Explicit non-default mappings.
		{provcore.FormatAnthropic, typology.WireShapeAnthropicMessages},
		{provcore.FormatGemini, typology.WireShapeGeminiGenerateContent},
		{provcore.FormatVertex, typology.WireShapeVertexGenerateContent},
		{provcore.FormatBedrock, typology.WireShapeBedrockConverse},
		{provcore.FormatCohere, typology.WireShapeCohereChat},
		// OpenAI-family default.
		{provcore.FormatOpenAI, typology.WireShapeOpenAIChat},
		{provcore.FormatDeepSeek, typology.WireShapeOpenAIChat},
		{provcore.FormatGLM, typology.WireShapeOpenAIChat},
		{provcore.FormatAzureOpenAI, typology.WireShapeOpenAIChat},
		{provcore.FormatMistral, typology.WireShapeOpenAIChat},
		{provcore.FormatXai, typology.WireShapeOpenAIChat},
		{provcore.FormatGroq, typology.WireShapeOpenAIChat},
		{provcore.FormatPerplexity, typology.WireShapeOpenAIChat},
		{provcore.FormatTogether, typology.WireShapeOpenAIChat},
		{provcore.FormatFireworks, typology.WireShapeOpenAIChat},
		{provcore.FormatMoonshot, typology.WireShapeOpenAIChat},
		{provcore.FormatMiniMax, typology.WireShapeOpenAIChat},
		{provcore.FormatHuggingFace, typology.WireShapeOpenAIChat},
		{provcore.FormatReplicate, typology.WireShapeOpenAIChat},
		// Embeddings-only / unknown families still fall through to OpenAI-chat
		// default (these Formats don't serve chat — adapter would reject).
		{provcore.FormatVoyage, typology.WireShapeOpenAIChat},
		{provcore.FormatOpenAIResponses, typology.WireShapeOpenAIChat},
	}
	for _, c := range cases {
		t.Run(string(c.f), func(t *testing.T) {
			if got := chatWireShapeForFormat(c.f); got != c.want {
				t.Errorf("chatWireShapeForFormat(%q) = %q, want %q", c.f, got, c.want)
			}
		})
	}
}

// TestEmbeddingsWireShapeForFormat exhaustively covers every provcore.Format
// value for the embeddings projection. Lockstep gate for the helper.
func TestEmbeddingsWireShapeForFormat(t *testing.T) {
	cases := []struct {
		f    provcore.Format
		want typology.WireShape
	}{
		// Explicit non-default mappings.
		{provcore.FormatGemini, typology.WireShapeGeminiEmbedContent},
		{provcore.FormatVertex, typology.WireShapeVertexEmbedContent},
		{provcore.FormatBedrock, typology.WireShapeBedrockEmbeddings},
		{provcore.FormatCohere, typology.WireShapeCohereEmbed},
		{provcore.FormatVoyage, typology.WireShapeVoyageEmbeddings},
		// OpenAI-family default for everything else.
		{provcore.FormatOpenAI, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatDeepSeek, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatGLM, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatAzureOpenAI, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatMistral, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatXai, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatGroq, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatPerplexity, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatTogether, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatFireworks, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatMoonshot, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatMiniMax, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatHuggingFace, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatReplicate, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatAnthropic, typology.WireShapeOpenAIEmbeddings},
		{provcore.FormatOpenAIResponses, typology.WireShapeOpenAIEmbeddings},
	}
	for _, c := range cases {
		t.Run(string(c.f), func(t *testing.T) {
			if got := embeddingsWireShapeForFormat(c.f); got != c.want {
				t.Errorf("embeddingsWireShapeForFormat(%q) = %q, want %q", c.f, got, c.want)
			}
		})
	}
}

func TestBridge_ChatRoutable(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))
	if !b.ChatRoutable(provcore.FormatAnthropic, provcore.FormatOpenAI) {
		t.Fatal("expected anthropic→openai routable")
	}
	if !b.ChatRoutable(provcore.FormatOpenAI, provcore.FormatAnthropic) {
		t.Fatal("expected openai→anthropic routable")
	}
	if !b.ChatRoutable(provcore.FormatGemini, provcore.FormatOpenAI) {
		t.Fatal("expected gemini→openai routable")
	}
	if !b.ChatRoutable(provcore.FormatMiniMax, provcore.FormatMiniMax) {
		t.Fatal("expected minimax passthrough")
	}
}

func TestStreamShapeCompatible(t *testing.T) {
	// Same-format pairs: always compatible.
	if !StreamShapeCompatible(provcore.FormatOpenAI, provcore.FormatOpenAI) {
		t.Fatal("same-format (openai) should be compatible")
	}
	if !StreamShapeCompatible(provcore.FormatAnthropic, provcore.FormatAnthropic) {
		t.Fatal("same-format (anthropic) should be compatible")
	}

	// OpenAI-like ingress → any non-Bedrock target: transcoder re-encodes to OpenAI SSE.
	if !StreamShapeCompatible(provcore.FormatOpenAI, provcore.FormatAnthropic) {
		t.Fatal("openai→anthropic should be compatible via SSE transcoding")
	}
	if !StreamShapeCompatible(provcore.FormatOpenAI, provcore.FormatGemini) {
		t.Fatal("openai→gemini should be compatible via SSE transcoding")
	}
	if !StreamShapeCompatible(provcore.FormatGLM, provcore.FormatOpenAI) {
		t.Fatal("glm→openai: both OpenAI-like, should be compatible")
	}

	// Non-OpenAI ingress → any non-Bedrock target: encoder re-encodes to ingress SSE.
	if !StreamShapeCompatible(provcore.FormatAnthropic, provcore.FormatOpenAI) {
		t.Fatal("anthropic→openai should be compatible via Anthropic SSE encoder")
	}
	if !StreamShapeCompatible(provcore.FormatAnthropic, provcore.FormatGemini) {
		t.Fatal("anthropic→gemini should be compatible via Anthropic SSE encoder")
	}
	if !StreamShapeCompatible(provcore.FormatGemini, provcore.FormatAnthropic) {
		t.Fatal("gemini→anthropic should be compatible via Gemini SSE encoder")
	}

	// Bedrock: always incompatible (AWS binary event-stream, not SSE).
	if StreamShapeCompatible(provcore.FormatOpenAI, provcore.FormatBedrock) {
		t.Fatal("openai→bedrock should be incompatible (AWS binary framing)")
	}
	if StreamShapeCompatible(provcore.FormatBedrock, provcore.FormatOpenAI) {
		t.Fatal("bedrock→openai should be incompatible")
	}
}

func TestNewStreamTranscoder(t *testing.T) {
	b := New(provbuiltins.SchemaCodecs(nil))

	// Passthrough pairs return nil.
	if tr := b.NewStreamTranscoder(provcore.FormatOpenAI, provcore.FormatOpenAI, ""); tr != nil {
		t.Fatal("same-format pair should return nil transcoder")
	}
	if tr := b.NewStreamTranscoder(provcore.FormatGLM, provcore.FormatDeepSeek, ""); tr != nil {
		t.Fatal("both-OpenAI-like pair should return nil transcoder")
	}

	// Cross-format pairs return a non-nil transcoder.
	pairs := [][2]provcore.Format{
		{provcore.FormatOpenAI, provcore.FormatAnthropic},
		{provcore.FormatOpenAI, provcore.FormatGemini},
		{provcore.FormatAnthropic, provcore.FormatOpenAI},
		{provcore.FormatGemini, provcore.FormatOpenAI},
		{provcore.FormatVertex, provcore.FormatAnthropic},
		{provcore.FormatCohere, provcore.FormatOpenAI},
		{provcore.FormatReplicate, provcore.FormatGemini},
	}
	for _, p := range pairs {
		tr := b.NewStreamTranscoder(p[0], p[1], "test-model")
		if tr == nil {
			t.Errorf("NewStreamTranscoder(%s, %s) = nil, want non-nil", p[0], p[1])
		}
	}
}
