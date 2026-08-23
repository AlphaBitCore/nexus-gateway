package codec

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// A content part this codec has no case for must be REFUSED, never dropped.
//
// This is the class, not one instance of it. The content policy admits a fixed
// set of part kinds before a body reaches the codec, and the codec's switch
// handles a set of its own; nothing keeps the two in step. They drifted once
// already — the policy admitted input_audio while the switch had no case for
// it — and the symptom was silence: the part vanished, the request succeeded,
// and the model answered that it could not hear any audio. That is
// indistinguishable from a model ignoring an attachment, which is the most
// expensive thing to diagnose in this whole area and the reason the default
// branch refuses.
//
// Driven against the bare codec rather than the gated spec on purpose: the gate
// turns these kinds away first, so through the spec this branch is unreachable
// and the test would prove nothing about the codec's own contract.
func TestUnknownContentPart_IsRefusedNotSilentlyDropped(t *testing.T) {
	for _, kind := range []string{"input_video", "refusal", "some_future_part"} {
		t.Run(kind, func(t *testing.T) {
			res, err := NewCodec().EncodeRequest(typology.WireShapeGeminiGenerateContent,
				[]byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":[`+
					`{"type":"`+kind+`","data":"QUJD"},`+
					`{"type":"text","text":"what is attached?"}]}]}`),
				provcore.CallTarget{ProviderModelID: "gemini-2.5-flash"})
			if err == nil {
				t.Fatalf("a %q part was accepted and silently discarded; the model would be "+
					"asked about an attachment it never received:\n  %s", kind, res.Body)
			}
			if !strings.Contains(err.Error(), kind) {
				t.Errorf("message %q does not name the part that could not be carried", err.Error())
			}
		})
	}
}

// TestAudioPart_RidesTheSameInlineDataPartAsImages.
//
// Measured against generativelanguage.googleapis.com: every Gemini chat model
// in the catalogue transcribed the fixture WAV. Before this the audio part was
// silently discarded — the request succeeded, the model answered about the text
// alone, and nothing in the response or the traffic row said an attachment had
// been dropped.
//
// The canonical part carries raw base64 and a format NAME, not a data: URL, so
// the media type is assembled from the format rather than parsed.
func TestAudioPart_RidesTheSameInlineDataPartAsImages(t *testing.T) {
	res, err := NewCodec().EncodeRequest(typology.WireShapeGeminiGenerateContent,
		[]byte(`{"messages":[{"role":"user","content":[
			{"type":"text","text":"transcribe"},
			{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`),
		provcore.CallTarget{})
	if err != nil {
		t.Fatalf("a wav attachment was refused: %v", err)
	}
	inline := gjson.GetBytes(res.Body, "contents.0.parts.1.inlineData")
	if !inline.Exists() {
		t.Fatalf("the audio part did not reach the wire: %s — the model answers about the "+
			"text alone and nothing says an attachment was dropped", res.Body)
	}
	if got := inline.Get("mimeType").String(); got != "audio/wav" {
		t.Errorf("mimeType = %q, want audio/wav — assembled from the format name, since the "+
			"canonical part carries no data: URL to parse", got)
	}
	if got := inline.Get("data").String(); got != "AAAA" {
		t.Errorf("data = %q, want the caller's bytes verbatim", got)
	}
}

// TestAudioPart_RefusesWhatItCannotCarryInsteadOfDroppingIt.
//
// Both halves of an audio part are required to build the inline part: bytes,
// and a format that maps to a media type. Missing either one used to mean the
// part vanished and the model answered about the rest of the message.
func TestAudioPart_RefusesWhatItCannotCarryInsteadOfDroppingIt(t *testing.T) {
	for _, tc := range []struct{ name, audio, want string }{
		{"no bytes", `{"format":"wav"}`, "input_audio.data"},
		{"a format this wire has no media type for", `{"data":"AAAA","format":"aiff"}`, "input_audio.format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCodec().EncodeRequest(typology.WireShapeGeminiGenerateContent,
				[]byte(`{"messages":[{"role":"user","content":[
					{"type":"input_audio","input_audio":`+tc.audio+`}]}]}`),
				provcore.CallTarget{})
			if err == nil {
				t.Fatal("the attachment was dropped and the request went upstream without " +
					"it; the answer looks correct and is about a recording nobody sent")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name the field at fault: %v", err)
			}
		})
	}
}

// A file part addressed by id is an OpenAI-side handle. Gemini cannot resolve
// it, and forwarding an empty part would ask the model about a document it
// never received — a confident answer about nothing.
func TestFilePart_ABareFileIDIsRefusedNotEmptied(t *testing.T) {
	_, err := NewCodec().EncodeRequest(typology.WireShapeGeminiGenerateContent,
		[]byte(`{"messages":[{"role":"user","content":[
			{"type":"file","file":{"file_id":"file-abc"}}]}]}`),
		provcore.CallTarget{})
	if err == nil {
		t.Fatal("a file id was forwarded to a wire that cannot resolve it")
	}
	if !strings.Contains(err.Error(), "file_id") {
		t.Errorf("the refusal does not name what cannot be resolved: %v", err)
	}
}
