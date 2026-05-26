package typology

import "testing"

// TestWireShapeConstants pins the canonical wire-format string for every
// WireShape constant. Renaming any of these is a coordinated breaking
// change for the Phase 3 schema migration (Provider.wire_shape DB column
// values).
func TestWireShapeConstants(t *testing.T) {
	cases := []struct {
		w    WireShape
		want string
	}{
		{WireShapeOpenAIChat, "openai-chat"},
		{WireShapeOpenAIResponses, "openai-responses"},
		{WireShapeOpenAICompletionsLegacy, "openai-completions-legacy"},
		{WireShapeOpenAIEmbeddings, "openai-embeddings"},
		{WireShapeOpenAIAudioSpeech, "openai-audio-speech"},
		{WireShapeOpenAIAudioTranscriptions, "openai-audio-transcriptions"},
		{WireShapeOpenAIImages, "openai-images"},
		{WireShapeOpenAIBatches, "openai-batches"},
		{WireShapeAnthropicMessages, "anthropic-messages"},
		{WireShapeGeminiGenerateContent, "gemini-generate-content"},
		{WireShapeGeminiEmbedContent, "gemini-embed-content"},
		{WireShapeVertexGenerateContent, "vertex-generate-content"},
		{WireShapeVertexEmbedContent, "vertex-embed-content"},
		{WireShapeBedrockConverse, "bedrock-converse"},
		{WireShapeBedrockInvoke, "bedrock-invoke"},
		{WireShapeCohereChat, "cohere-chat"},
		{WireShapeCohereEmbed, "cohere-embed"},
		{WireShapeVoyageEmbeddings, "voyage-embeddings"},
		{WireShapeNone, ""},
	}
	for _, c := range cases {
		if string(c.w) != c.want {
			t.Errorf("WireShape %v string = %q, want %q", c.w, string(c.w), c.want)
		}
		if c.w.String() != c.want {
			t.Errorf("(WireShape).String() = %q, want %q", c.w.String(), c.want)
		}
	}
}

// TestAllWireShapesExhaustive verifies the AllWireShapes slice tracks
// every WireShape constant except the WireShapeNone sentinel.
func TestAllWireShapesExhaustive(t *testing.T) {
	want := []WireShape{
		WireShapeOpenAIChat,
		WireShapeOpenAIResponses,
		WireShapeOpenAICompletionsLegacy,
		WireShapeOpenAIEmbeddings,
		WireShapeOpenAIAudioSpeech,
		WireShapeOpenAIAudioTranscriptions,
		WireShapeOpenAIImages,
		WireShapeOpenAIBatches,
		WireShapeAnthropicMessages,
		WireShapeGeminiGenerateContent,
		WireShapeGeminiEmbedContent,
		WireShapeVertexGenerateContent,
		WireShapeVertexEmbedContent,
		WireShapeBedrockConverse,
		WireShapeBedrockInvoke,
		WireShapeBedrockEmbeddings,
		WireShapeCohereChat,
		WireShapeCohereEmbed,
		WireShapeVoyageEmbeddings,
	}
	if len(AllWireShapes) != len(want) {
		t.Fatalf("len(AllWireShapes) = %d, want %d", len(AllWireShapes), len(want))
	}
	for i, w := range want {
		if AllWireShapes[i] != w {
			t.Errorf("AllWireShapes[%d] = %v, want %v", i, AllWireShapes[i], w)
		}
	}
}

func TestWireShape_IsValid(t *testing.T) {
	for _, w := range AllWireShapes {
		if !w.IsValid() {
			t.Errorf("IsValid(%v) = false, want true for defined constant", w)
		}
	}
	// WireShapeNone is the "no body" sentinel — explicitly excluded from
	// IsValid. Callers that accept "no body" check for it separately.
	if WireShapeNone.IsValid() {
		t.Errorf("IsValid(WireShapeNone) = true, want false (sentinel is not in AllWireShapes)")
	}
	if WireShape("bogus").IsValid() {
		t.Errorf("IsValid(\"bogus\") = true, want false")
	}
}

// TestKindFromWireShape pins the wire-shape → endpoint-kind inverse
// lookup that callers use after the typology.WireShape deletion to
// recover the legacy audit / Prometheus string.
func TestKindFromWireShape(t *testing.T) {
	cases := []struct {
		w    WireShape
		want EndpointKind
	}{
		{WireShapeOpenAIChat, EndpointKindChat},
		{WireShapeOpenAIResponses, EndpointKindChat},
		{WireShapeOpenAICompletionsLegacy, EndpointKindChat},
		{WireShapeAnthropicMessages, EndpointKindChat},
		{WireShapeGeminiGenerateContent, EndpointKindChat},
		{WireShapeVertexGenerateContent, EndpointKindChat},
		{WireShapeBedrockConverse, EndpointKindChat},
		{WireShapeBedrockInvoke, EndpointKindChat},
		{WireShapeCohereChat, EndpointKindChat},
		{WireShapeOpenAIEmbeddings, EndpointKindEmbeddings},
		{WireShapeGeminiEmbedContent, EndpointKindEmbeddings},
		{WireShapeVertexEmbedContent, EndpointKindEmbeddings},
		{WireShapeCohereEmbed, EndpointKindEmbeddings},
		{WireShapeVoyageEmbeddings, EndpointKindEmbeddings},
		{WireShapeOpenAIAudioSpeech, EndpointKindTTS},
		{WireShapeOpenAIAudioTranscriptions, EndpointKindSTT},
		{WireShapeOpenAIImages, EndpointKindImageGeneration},
		{WireShapeOpenAIBatches, EndpointKindBatch},
		{WireShapeNone, EndpointKind("")},
		{WireShape("bogus"), EndpointKind("")},
	}
	for _, c := range cases {
		if got := KindFromWireShape(c.w); got != c.want {
			t.Errorf("KindFromWireShape(%v) = %q, want %q", c.w, got, c.want)
		}
	}
}
