package dispatch

// Multimodal wire shapes carry `model` at the JSON body root exactly like
// chat/embeddings, so the passthrough alias → provider-model rewrite must
// apply on /v1/images/generations and /v1/audio/speech bodies. Without it
// a routing-resolved model (or a client-facing alias) would reach the
// upstream verbatim and 404 — the same failure the openAIWire gate exists
// to prevent for chat.

import (
	"github.com/goccy/go-json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func multimodalReqFor(shape typology.WireShape, body []byte) Request {
	return Request{
		WireShape:  shape,
		BodyFormat: FormatOpenAI,
		Body:       body,
		Target:     CallTarget{ProviderModelID: "provider-real-model"},
	}
}

func TestRewritePassthroughModel_imagesShape(t *testing.T) {
	body := []byte(`{"model":"client-alias","prompt":"a red fox","n":2,"size":"1024x1024"}`)
	out, _, err := rpm(multimodalReqFor(typology.WireShapeOpenAIImages, body))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "provider-real-model" {
		t.Errorf("images body model = %v, want provider-real-model (alias rewrite must apply)", m["model"])
	}
	if m["prompt"] != "a red fox" {
		t.Errorf("prompt mutated: %v", m["prompt"])
	}
	if m["size"] != "1024x1024" {
		t.Errorf("size mutated: %v", m["size"])
	}
}

func TestRewritePassthroughModel_audioSpeechShape(t *testing.T) {
	body := []byte(`{"model":"client-alias","input":"hello world","voice":"alloy"}`)
	out, _, err := rpm(multimodalReqFor(typology.WireShapeOpenAIAudioSpeech, body))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "provider-real-model" {
		t.Errorf("tts body model = %v, want provider-real-model (alias rewrite must apply)", m["model"])
	}
	if m["input"] != "hello world" {
		t.Errorf("input mutated: %v", m["input"])
	}
	if m["voice"] != "alloy" {
		t.Errorf("voice mutated: %v", m["voice"])
	}
}

// TestRewritePassthroughModel_transcriptionsShape_untouched locks the
// deliberate exclusion: the STT transcriptions shape is multipart (model is
// a form field, not JSON root), so it is NOT in the openAIWire rewrite set —
// the body must pass through byte-identical.
func TestRewritePassthroughModel_transcriptionsShape_untouched(t *testing.T) {
	body := []byte(`{"model":"client-alias"}`) // even a JSON-looking body must not be rewritten
	out, _, err := rpm(multimodalReqFor(typology.WireShapeOpenAIAudioTranscriptions, body))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "client-alias" {
		t.Errorf("transcriptions body must pass through untouched; model = %v", m["model"])
	}
}
