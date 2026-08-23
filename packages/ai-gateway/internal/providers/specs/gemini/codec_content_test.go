package gemini

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// sendCrossFormat drives the codec the spec SHIPS, through the door an
// OpenAI-shaped request takes on its way to a Gemini model. That is the door
// this whole program is about: a caller posts to /v1/chat/completions, `auto`
// picks a Gemini model, and the body is translated here.
func sendCrossFormat(t *testing.T, body string) error {
	t.Helper()
	_, err := NewSpec(nil).SchemaCodec.EncodeRequest(typology.WireShapeGeminiGenerateContent,
		[]byte(body), provcore.CallTarget{ProviderModelID: "gemini-2.5-flash"})
	return err
}

func inlineImage(mediaType string) string {
	data := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'})
	return `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"data:` + mediaType + `;base64,` + data + `"}},` +
		`{"type":"text","text":"what number is this?"}]}]}`
}

// Measured against generativelanguage.googleapis.com in Google's own
// inline_data shape: bmp, tiff and svg+xml each answer 400 "Unsupported MIME
// type: image/<type>". True, and it names neither the attachment the caller
// chose nor anything they could do instead — and on the cross-format door the
// caller did not choose the wire at all, `auto` did.
func TestUnreadableImageFormat_IsRefusedInOurWords(t *testing.T) {
	for _, mediaType := range []string{"image/bmp", "image/tiff", "image/svg+xml"} {
		t.Run(mediaType, func(t *testing.T) {
			err := sendCrossFormat(t, inlineImage(mediaType))
			if err == nil {
				t.Fatalf("%s was forwarded; this wire answers 400 for it", mediaType)
			}
			var pe *provcore.ProviderError
			if !errors.As(err, &pe) || pe.Status != 400 {
				t.Fatalf("err = %v, want a 400 ProviderError", err)
			}
			if !strings.Contains(pe.Message, mediaType) {
				t.Errorf("message %q does not name the format that was sent", pe.Message)
			}
			if !strings.Contains(pe.Message, "image/png") {
				t.Errorf("message %q does not say what WOULD work, which is the actionable half",
					pe.Message)
			}
		})
	}
}

// The complement — and the reason the list is built from refusals rather than
// from reads. image/gif is absent from Google's published format list and this
// wire reads it; image/heic produced a 200 on a fixture with no anchor, which
// shows the wire took it and shows nothing about unreadability. Refusing either
// would take away a capability with nothing in the error to explain it.
func TestReadableImageFormats_ReachTheWire(t *testing.T) {
	for _, mediaType := range []string{
		"image/png", "image/jpeg", "image/gif", "image/webp", "image/heic", "image/heif",
	} {
		if err := sendCrossFormat(t, inlineImage(mediaType)); err != nil {
			t.Errorf("%s was refused: %v", mediaType, err)
		}
	}
}

// Audio rides as inlineData, the same part Gemini uses for images and
// documents. Measured directly against generativelanguage.googleapis.com: all
// five Gemini chat models in the catalog transcribed the fixture WAV.
//
// This is asserted on the EMITTED BODY rather than on "no error", because the
// bug it guards produced no error at all. The content policy admitted
// input_audio while the codec's part switch had no case for it, so the part
// fell through and vanished: the model was asked to transcribe an attachment it
// never received and answered that it could not hear anything. A silently
// dropped attachment is indistinguishable from a model ignoring one.
func TestAudioPart_RidesAsInlineData(t *testing.T) {
	for _, tc := range []struct{ format, mime string }{{"wav", "audio/wav"}, {"mp3", "audio/mp3"}} {
		t.Run(tc.format, func(t *testing.T) {
			res, err := NewSpec(nil).SchemaCodec.EncodeRequest(
				typology.WireShapeGeminiGenerateContent,
				[]byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":[`+
					`{"type":"input_audio","input_audio":{"data":"QUJD","format":"`+tc.format+`"}},`+
					`{"type":"text","text":"transcribe this"}]}]}`),
				provcore.CallTarget{ProviderModelID: "gemini-2.5-flash"})
			if err != nil {
				t.Fatalf("an audio part was refused: %v", err)
			}
			part := gjson.GetBytes(res.Body, "contents.0.parts.0.inlineData")
			if !part.Exists() {
				t.Fatalf("the audio part did not reach the wire — it was dropped, and the "+
					"model will be asked about an attachment it never got:\n  %s", res.Body)
			}
			if got := part.Get("mimeType").String(); got != tc.mime {
				t.Errorf("mimeType = %q, want %q", got, tc.mime)
			}
			if got := part.Get("data").String(); got != "QUJD" {
				t.Errorf("data = %q, want the audio bytes", got)
			}
		})
	}
}

// A format outside the two the canonical field defines is refused, not guessed
// at. A wrong mimeType reaches the model as an unreadable attachment and comes
// back as a confident answer about nothing — the accepted-but-not-read failure,
// manufactured by us.
func TestAudioPart_UnknownFormatIsRefusedRatherThanGuessed(t *testing.T) {
	_, err := NewSpec(nil).SchemaCodec.EncodeRequest(typology.WireShapeGeminiGenerateContent,
		[]byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":[`+
			`{"type":"input_audio","input_audio":{"data":"QUJD","format":"flac"}}]}]}`),
		provcore.CallTarget{ProviderModelID: "gemini-2.5-flash"})
	if err == nil {
		t.Fatal("an unrecognised audio format was forwarded with a guessed media type")
	}
	if !strings.Contains(err.Error(), "flac") {
		t.Errorf("message %q does not name the format that was sent", err.Error())
	}
}

// A video part reaches this wire, which reads it.
//
// This test used to assert the opposite — that we refuse video "in our words" —
// on the stated ground that a video part has no inline form here. That ground
// is false: an inline video/mp4 sent straight at the provider answers 200, and
// the provider accounts for the input under promptTokensDetails as modality
// VIDEO. It was the one refusal in this adapter's content policy with no
// measurement behind it, in a file whose every other entry carries one.
func TestVideoPart_ReachesTheWire(t *testing.T) {
	if err := sendCrossFormat(t, `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":[`+
		`{"type":"video_url","video_url":{"url":"data:video/mp4;base64,QQ=="}},`+
		`{"type":"text","text":"what happens?"}]}]}`); err != nil {
		t.Fatalf("a video part was refused: %v", err)
	}
}

// THE NATIVE DOOR, asserted rather than assumed — and asserted to do NOTHING,
// which is the deliberate half.
//
// A caller on Gemini's own ingress sends Gemini's own body: contents[].parts[]
// with inline_data.mime_type, not messages[].content[] with a type tag. The
// content gate walks the canonical OpenAI shape, so it has no opinion about
// this body and passes it through. That is correct here for the same reason it
// is correct for Anthropic's document sources: a caller who already speaks this
// wire picked their own mime_type, Google's refusal names the type they picked,
// and reinterpreting their choice would be us overriding a deliberate decision.
//
// It is asserted because the alternative — a gate that quietly no-ops on a body
// shape it was never written for — looks identical from the outside to a gate
// that was never wired, and that mistake has shipped here before.
func TestNativeDoor_PassesGeminiShapedBodiesThrough(t *testing.T) {
	spec := NewSpec(nil)
	data := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'})
	for _, mimeType := range []string{"image/png", "image/bmp"} {
		native := `{"contents":[{"role":"user","parts":[` +
			`{"inline_data":{"mime_type":"` + mimeType + `","data":"` + data + `"}},` +
			`{"text":"what number is this?"}]}]}`
		res, err := spec.SchemaCodec.RewriteNative(typology.WireShapeGeminiGenerateContent,
			[]byte(native), provcore.CallTarget{ProviderModelID: "gemini-2.5-flash"}, false)
		if err != nil {
			t.Fatalf("the native door refused a body it did not build (%s): %v", mimeType, err)
		}
		if !strings.Contains(string(res.Body), mimeType) {
			t.Errorf("the caller's mime_type %q did not survive the native door: %s",
				mimeType, res.Body)
		}
	}
}

// The gate has to be WIRED into the spec, not merely written, and it has to be
// wired to THIS policy. Unwiring it in spec.go is invisible to a test that
// constructs the wrapper itself.
//
// "err != nil" is not enough on its own, which the mutation battery caught:
// substituting an empty policy refuses this request too, because an empty
// policy has an empty Allow and turns away every structured part. That refusal
// is a different one, and a test that cannot tell the two apart reports a
// policy is wired when nothing of it survives. So the assertion is on the
// refusal being about the FORMAT — it names the type sent and a type that works.
func TestTheContentGateIsWiredIntoTheSpec(t *testing.T) {
	spec := NewSpec(nil)
	if spec.SchemaCodec == nil {
		t.Fatal("the spec carries no codec")
	}
	_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeGeminiGenerateContent,
		[]byte(inlineImage("image/bmp")), provcore.CallTarget{ProviderModelID: "gemini-2.5-flash"})
	if err == nil {
		t.Fatal("the SHIPPED codec forwarded image/bmp — the content policy exists but " +
			"NewSpec does not use it")
	}
	if !strings.Contains(err.Error(), "image/bmp") || !strings.Contains(err.Error(), "image/png") {
		t.Errorf("the shipped codec refused, but not over the image format: %v\n"+
			"  a refusal that names neither the format sent nor one that works is a "+
			"different policy than the one this file declares", err)
	}
}

// The generations decode different sets, and this is the assertion that stops a
// list measured on one from being applied to the other. Measured per model:
// 2.5.x answers 400 to bmp/tiff/svg; 3.x returns the number rendered in the
// pixels for bmp and tiff. A provider-wide list built from 2.5-flash alone
// refused both on the 3.x models — a capability loss with nothing in the error
// to explain it, and it shipped.
func TestImageFormats_AreResolvedPerModelNotPerProvider(t *testing.T) {
	send := func(model, mediaType string) error {
		_, err := NewSpec(nil).SchemaCodec.EncodeRequest(typology.WireShapeGeminiGenerateContent,
			[]byte(inlineImage(mediaType)), provcore.CallTarget{ProviderModelID: model})
		return err
	}
	for _, model := range []string{"gemini-3.1-flash-lite", "gemini-3.5-flash"} {
		for _, mt := range []string{"image/bmp", "image/tiff", "image/svg+xml"} {
			if err := send(model, mt); err != nil {
				t.Errorf("%s refused %s, which it reads: %v", model, mt, err)
			}
		}
	}
	for _, model := range []string{"gemini-2.5-flash", "gemini-2.5-pro", "gemini-2.5-flash-lite"} {
		for _, mt := range []string{"image/bmp", "image/tiff", "image/svg+xml"} {
			if err := send(model, mt); err == nil {
				t.Errorf("%s forwarded %s, which its wire answers 400 for", model, mt)
			}
		}
	}
}
