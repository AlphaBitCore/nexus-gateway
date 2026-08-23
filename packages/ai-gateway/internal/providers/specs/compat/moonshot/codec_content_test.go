package moonshot

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Drives the codec the adapter SHIPS, never one constructed by hand. A test that
// builds the wrapper itself proves the wrapper works and says nothing about
// whether NewSpec uses it — which is exactly how the first version of this file
// stayed green while production was unchanged.
func encode(t *testing.T, canonical string) ([]byte, error) {
	t.Helper()
	res, err := NewSpec(nil).SchemaCodec.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(canonical), provcore.CallTarget{ProviderModelID: "moonshot-v1-8k-vision-preview"})
	return res.Body, err
}

// Moonshot does not fetch images. Measured against api.moonshot.cn with the same
// PNG: a data: URL answers 200 and describes it ("Red square."), a real
// reachable https URL answers 400 "unsupported image url: <url>" — and so does
// an unreachable one, which is what makes the refusal categorical rather than a
// verdict on the particular URL.
//
// Forwarding it tells a caller their URL is unsupported when the URL was never
// the problem, and says nothing about inlining being the fix.
func TestImageURL_AnUnfetchableURLIsRefusedWithTheRealConstraint(t *testing.T) {
	for _, url := range []string{
		"https://example.com/a.png",
		"http://example.com/a.png",
		"https://upload.wikimedia.org/some/real/image.png",
	} {
		t.Run(url, func(t *testing.T) {
			_, err := encode(t, `{"model":"moonshot-v1-8k-vision-preview","messages":[
			    {"role":"user","content":[
			      {"type":"image_url","image_url":{"url":"`+url+`"}},
			      {"type":"text","text":"what is this?"}]}]}`)
			if err == nil {
				t.Fatal("an external image URL was forwarded; Moonshot answers 400 for every one")
			}
			var pe *provcore.ProviderError
			if !errors.As(err, &pe) || pe.Status != 400 {
				t.Fatalf("err = %v, want a 400 ProviderError", err)
			}
			if !strings.Contains(pe.Message, "does not fetch images by URL") {
				t.Errorf("message %q does not state the constraint — a caller reading it cannot "+
					"tell that inlining the image is the fix", pe.Message)
			}
		})
	}
}

// The form Moonshot does read must reach the wire untouched, and so must every
// request that carries no image at all — this codec wraps the shared identity
// codec and must not change what it does otherwise.
func TestImageURL_InlineAndPlainRequestsAreUnaffected(t *testing.T) {
	body, err := encode(t, `{"model":"moonshot-v1-8k-vision-preview","messages":[
	    {"role":"user","content":[
	      {"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}},
	      {"type":"text","text":"what colour?"}]}]}`)
	if err != nil {
		t.Fatalf("an inline image was refused: %v", err)
	}
	if !strings.Contains(string(body), "image_url") {
		t.Errorf("the image part did not survive to the wire: %s", body)
	}

	if _, err := encode(t, `{"model":"kimi-k2-thinking","messages":[
	    {"role":"user","content":"plain text"}]}`); err != nil {
		t.Errorf("a request with no image was refused: %v", err)
	}
}

// The word appearing in a caller's own text is not an image part.
func TestImageURL_MentioningTheTokenIsNotCarryingOne(t *testing.T) {
	if _, err := encode(t, `{"model":"kimi-k2-thinking","messages":[
	    {"role":"user","content":[{"type":"text","text":"what is an \"image_url\"?"}]}]}`); err != nil {
		t.Errorf("a body that merely mentions the token was refused: %v", err)
	}
}

// The gate has to be WIRED, not merely written. The cases above construct the
// wrapped codec directly, so they pass whether or not NewSpec uses it — which is
// the whole failure mode: unwiring the wrapper in spec.go left every one of them
// green. This drives the codec the adapter actually ships.
func TestImageURL_TheGateIsWiredIntoTheSpec(t *testing.T) {
	spec := NewSpec(nil)
	if spec.SchemaCodec == nil {
		t.Fatal("the spec carries no codec")
	}
	_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeOpenAIChat,
		[]byte(`{"model":"moonshot-v1-8k-vision-preview","messages":[{"role":"user","content":[
		    {"type":"image_url","image_url":{"url":"https://example.com/a.png"}},
		    {"type":"text","text":"what is this?"}]}]}`),
		provcore.CallTarget{ProviderModelID: "moonshot-v1-8k-vision-preview"})
	if err == nil {
		t.Fatal("the SHIPPED codec forwarded an external image URL — the content gate exists " +
			"but NewSpec does not use it")
	}
	if !strings.Contains(err.Error(), "does not fetch images by URL") {
		t.Errorf("err = %v, want the constraint named", err)
	}
}

// The NATIVE leg is the path production actually uses: an OpenAI-shaped request
// to this OpenAI-compatible provider is a same-spec passthrough, so it reaches
// RewriteNative and never EncodeRequest. Gating only the cross-format door left
// the live path untouched — a replay against production came back with the
// vendor's "unsupported image url" while every unit test above passed.
func TestImageURL_TheNativeLegIsGatedToo(t *testing.T) {
	spec := NewSpec(nil)
	_, err := spec.SchemaCodec.RewriteNative(typology.WireShapeOpenAIChat,
		[]byte(`{"model":"moonshot-v1-8k-vision-preview","messages":[{"role":"user","content":[
		    {"type":"image_url","image_url":{"url":"https://example.com/a.png"}},
		    {"type":"text","text":"what is this?"}]}]}`),
		provcore.CallTarget{ProviderModelID: "moonshot-v1-8k-vision-preview"}, false)
	if err == nil {
		t.Fatal("the native leg forwarded an external image URL — this is the path an " +
			"OpenAI-shaped request takes, so gating only EncodeRequest changes nothing in " +
			"production")
	}
	if !strings.Contains(err.Error(), "does not fetch images by URL") {
		t.Errorf("err = %v, want the constraint named", err)
	}
	// The inline form still passes on this leg.
	if _, err := spec.SchemaCodec.RewriteNative(typology.WireShapeOpenAIChat,
		[]byte(`{"model":"moonshot-v1-8k-vision-preview","messages":[{"role":"user","content":[
		    {"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}},
		    {"type":"text","text":"what colour?"}]}]}`),
		provcore.CallTarget{ProviderModelID: "moonshot-v1-8k-vision-preview"}, false); err != nil {
		t.Errorf("an inline image was refused on the native leg: %v", err)
	}
}

// A `file` part is a variant this wire does not know: production carries
// "the message at position 0 with role 'user' contains an invalid part type:
// file". Moonshot's Files API is upload-then-reference, so there is no inline
// form to map onto — the refusal is the answer, and it has to say that rather
// than repeat the vendor's deserializer.
func TestFilePart_IsRefusedWithWhatToSendInstead(t *testing.T) {
	for _, door := range []string{"encode", "native"} {
		t.Run(door, func(t *testing.T) {
			body := `{"model":"moonshot-v1-8k-vision-preview","messages":[{"role":"user","content":[
			    {"type":"file","file":{"filename":"d.md","file_data":"data:text/plain;base64,QQ=="}},
			    {"type":"text","text":"summarise"}]}]}`
			var err error
			spec := NewSpec(nil)
			tgt := provcore.CallTarget{ProviderModelID: "moonshot-v1-8k-vision-preview"}
			if door == "encode" {
				_, err = spec.SchemaCodec.EncodeRequest(typology.WireShapeOpenAIChat, []byte(body), tgt)
			} else {
				_, err = spec.SchemaCodec.RewriteNative(typology.WireShapeOpenAIChat, []byte(body), tgt, false)
			}
			if err == nil {
				t.Fatalf("%s door forwarded a file part; Moonshot answers 400 for it", door)
			}
			if !strings.Contains(err.Error(), "no inline document part") {
				t.Errorf("message %q does not state the constraint", err.Error())
			}
		})
	}
}

// An image format this wire does not decode is refused in OUR words, on both
// doors, naming the caller's format and the ones that work.
//
// Measured before this: an SVG earned "Invalid request: unsupported image
// format: text/plain; charset=utf-8" — Moonshot content-sniffs, SVG bytes are
// text, and the caller was told their image was text/plain. Eight models, one
// message, and nothing in it a caller could act on.
// Drives the codec the adapter SHIPS, through whichever door is named.
func sendMoonshot(t *testing.T, door, body string) error {
	t.Helper()
	spec := NewSpec(nil)
	tgt := provcore.CallTarget{ProviderModelID: "kimi-k3"}
	if door == "encode" {
		_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeOpenAIChat, []byte(body), tgt)
		return err
	}
	_, err := spec.SchemaCodec.RewriteNative(typology.WireShapeOpenAIChat, []byte(body), tgt, false)
	return err
}

func TestUnsupportedImageFormat_IsRefusedInOurWordsOnBothDoors(t *testing.T) {
	svg := base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	for _, door := range []string{"encode", "native"} {
		err := sendMoonshot(t, door, `{"model":"kimi-k3","messages":[{"role":"user","content":[`+
			`{"type":"image_url","image_url":{"url":"data:image/svg+xml;base64,`+svg+`"}},`+
			`{"type":"text","text":"what is this?"}]}]}`)
		if err == nil {
			t.Fatalf("the %s door forwarded an SVG; this wire answers 400 for it", door)
		}
		var pe *provcore.ProviderError
		if !errors.As(err, &pe) || pe.Status != 400 {
			t.Fatalf("err = %v, want a 400 ProviderError", err)
		}
		if !strings.Contains(pe.Message, "image/svg+xml") {
			t.Errorf("message %q does not name the format that was sent", pe.Message)
		}
		if !strings.Contains(pe.Message, "image/png") {
			t.Errorf("message %q does not say what WOULD work", pe.Message)
		}
	}
}

// The four formats this wire does decode must still reach it, on both doors.
func TestSupportedImageFormats_ReachTheWire(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'})
	for _, mt := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		for _, door := range []string{"encode", "native"} {
			if err := sendMoonshot(t, door, `{"model":"kimi-k3","messages":[{"role":"user","content":[`+
				`{"type":"image_url","image_url":{"url":"data:`+mt+`;base64,`+data+`"}}]}]}`); err != nil {
				t.Errorf("%s door refused %s: %v", door, mt, err)
			}
		}
	}
}
