package openai

import (
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// sendAudio drives the codec the spec SHIPS, through whichever door is named.
// The native door is the one production uses for an OpenAI-shaped request to an
// OpenAI model, so gating only the cross-format door would change nothing.
func sendAudio(t *testing.T, door, body string) []byte {
	t.Helper()
	spec := NewSpec(nil)
	tgt := provcore.CallTarget{ProviderModelID: "gpt-audio-mini"}
	var res provcore.EncodeResult
	var err error
	if door == "encode" {
		res, err = spec.SchemaCodec.EncodeRequest(typology.WireShapeOpenAIChat, []byte(body), tgt)
	} else {
		res, err = spec.SchemaCodec.RewriteNative(typology.WireShapeOpenAIChat, []byte(body), tgt, false)
	}
	if err != nil {
		t.Fatalf("%s door refused an audio request: %v", door, err)
	}
	return res.Body
}

const audioPart = `{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}`
const askPart = `{"type":"text","text":"transcribe this"}`

// Measured in production, deterministic, 6 of 6 against 0 of 6 on both
// gpt-audio-mini and gpt-audio-1.5: an audio attachment is read when a text part
// precedes it and refused when it does not. The refusal arrives as HTTP 200 with
// "I'm sorry, but I can't transcribe audio directly", so it is indistinguishable
// from a model that cannot do audio — which is why the reorder is ours to do
// rather than the caller's to discover.
func TestLoneAudioAttachment_GetsATextPartInFront(t *testing.T) {
	for _, door := range []string{"encode", "native"} {
		t.Run(door, func(t *testing.T) {
			out := sendAudio(t, door, `{"model":"gpt-audio-mini","messages":[{"role":"user","content":[`+
				audioPart+`,`+askPart+`]}]}`)
			parts := gjson.GetBytes(out, "messages.0.content")
			if got := parts.Get("0.type").String(); got != "text" {
				t.Errorf("content[0].type = %q, want text — the audio part still leads, and this "+
					"wire answers 200 with a refusal for that order:\n  %s", got, out)
			}
			if got := parts.Get("1.type").String(); got != "input_audio" {
				t.Errorf("content[1].type = %q, want input_audio", got)
			}
			// The attachment must survive the move, not just change position.
			if got := parts.Get("1.input_audio.data").String(); got != "QUJD" {
				t.Errorf("the audio bytes did not survive the reorder: %q", got)
			}
		})
	}
}

// Already correct order: left exactly as the caller wrote it.
func TestTextAlreadyLeading_IsNotDisturbed(t *testing.T) {
	out := sendAudio(t, "native", `{"model":"gpt-audio-mini","messages":[{"role":"user","content":[`+
		askPart+`,`+audioPart+`]}]}`)
	parts := gjson.GetBytes(out, "messages.0.content")
	if parts.Get("0.type").String() != "text" || parts.Get("1.type").String() != "input_audio" {
		t.Errorf("a correctly ordered request was rearranged: %s", out)
	}
}

// THE LIMIT OF THE RULE, and the reason it is scoped to one attachment.
//
// With two attachments the order IS the caller's meaning — "compare the first
// recording with the second" — so moving parts would change what they asked. A
// reorder that is safe for one attachment is not safe for two, and a rule that
// did not stop here would silently rewrite the question.
func TestTwoAttachments_OrderIsTheCallersMeaningAndIsLeftAlone(t *testing.T) {
	second := `{"type":"input_audio","input_audio":{"data":"WFla","format":"wav"}}`
	out := sendAudio(t, "native", `{"model":"gpt-audio-mini","messages":[{"role":"user","content":[`+
		audioPart+`,`+second+`,{"type":"text","text":"which came first?"}]}]}`)
	parts := gjson.GetBytes(out, "messages.0.content")
	if got := parts.Get("0.input_audio.data").String(); got != "QUJD" {
		t.Errorf("the first recording is no longer first (content[0].data=%q); with two "+
			"attachments the order is the question:\n  %s", got, out)
	}
	if got := parts.Get("1.input_audio.data").String(); got != "WFla" {
		t.Errorf("the second recording moved: content[1].data=%q", got)
	}
}

// An audio attachment with no text part at all has nothing to lead with, and
// must not be turned into a malformed body.
func TestAudioWithNoTextPart_IsLeftIntact(t *testing.T) {
	out := sendAudio(t, "native", `{"model":"gpt-audio-mini","messages":[{"role":"user","content":[`+
		audioPart+`]}]}`)
	parts := gjson.GetBytes(out, "messages.0.content")
	if !parts.IsArray() || len(parts.Array()) != 1 ||
		parts.Get("0.input_audio.data").String() != "QUJD" {
		t.Errorf("a lone audio part was disturbed: %s", out)
	}
}

// An image attachment is not audio, and this rule has no measurement behind it
// for images. Reordering there would be a guess dressed as a fix.
func TestImageAttachment_OrderIsNotTouched(t *testing.T) {
	out := sendAudio(t, "native", `{"model":"gpt-4o","messages":[{"role":"user","content":[`+
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}},`+askPart+`]}]}`)
	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "image_url" {
		t.Errorf("an image request was reordered on no evidence: content[0].type=%q", got)
	}
}
