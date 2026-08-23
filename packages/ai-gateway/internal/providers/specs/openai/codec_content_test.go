package openai

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Drives the codec the spec SHIPS, through whichever door is named. A test that
// builds the gate itself proves the gate works and says nothing about whether
// NewSpec installs it — the mistake that let a Moonshot gate pass every test
// and change nothing in production.
func sendOpenAI(t *testing.T, door, body string) (provcore.EncodeResult, error) {
	t.Helper()
	spec := NewSpec(nil)
	tgt := provcore.CallTarget{ProviderModelID: "gpt-4o-mini"}
	if door == "encode" {
		return spec.SchemaCodec.EncodeRequest(typology.WireShapeOpenAIChat, []byte(body), tgt)
	}
	return spec.SchemaCodec.RewriteNative(typology.WireShapeOpenAIChat, []byte(body), tgt, false)
}

func docBody(mediaType, name, content string) string {
	return `{"model":"gpt-4o-mini","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"file","file":{"filename":"` + name + `","file_data":"data:` + mediaType +
		`;base64,` + base64.StdEncoding.EncodeToString([]byte(content)) + `"}},` +
		`{"type":"text","text":"what is the reference number?"}]}]}`
}

// A text document rides as TEXT rather than being refused.
//
// This wire's file part takes a PDF and nothing else — measured: a markdown
// attachment answers "Invalid file data … Expected a base64-encoded data URL
// with an application/pdf MIME type … but got unsupported MIME type
// 'text/markdown'". But the bytes are characters and this wire reads characters,
// so carrying them as characters is lossless in meaning rather than a
// capability invented for it. Thirty-four production refusals were this.
func TestTextDocument_IsCarriedAsTextOnBothDoors(t *testing.T) {
	const content = "# Runbook\n\nThe reference number is 52903.\n"
	for _, mt := range []string{"text/markdown", "text/plain", "application/json", "text/csv"} {
		for _, door := range []string{"encode", "native"} {
			t.Run(mt+"/"+door, func(t *testing.T) {
				res, err := sendOpenAI(t, door, docBody(mt, "doc.md", content))
				if err != nil {
					t.Fatalf("%s was refused: %v — this wire reads these characters", mt, err)
				}
				parts := gjson.GetBytes(res.Body, "messages.0.content")
				first := parts.Get("0")
				if got := first.Get("type").String(); got != "text" {
					t.Fatalf("part type = %q, want text — the document was not inlined", got)
				}
				if txt := first.Get("text").String(); !strings.Contains(txt, "52903") {
					t.Errorf("the inlined text lost the document's content: %q", txt)
				} else if !strings.Contains(txt, "doc.md") {
					t.Errorf("the inlined text does not name the document: %q", txt)
				}
				// No file part may survive; a leftover would reach the wire and 400.
				if strings.Contains(string(res.Body), `"file_data"`) {
					t.Error("a file part survived the conversion and will reach the wire")
				}
				// The conversion announces itself. A caller whose attachment was
				// carried in another form must be able to tell.
				if len(res.Rewrites) == 0 {
					t.Error("the conversion is silent — nothing reaches x-nexus-coerced")
				}
			})
		}
	}
}

// A PDF is not characters, so it stays a document and reaches the wire whole.
func TestPDFDocument_IsLeftAlone(t *testing.T) {
	for _, door := range []string{"encode", "native"} {
		res, err := sendOpenAI(t, door, docBody("application/pdf", "r.pdf", "%PDF-1.4 bytes"))
		if err != nil {
			t.Fatalf("%s door refused a PDF, which this wire reads: %v", door, err)
		}
		if !strings.Contains(string(res.Body), `"file_data"`) {
			t.Errorf("%s door converted a PDF; it is not characters and must stay a document", door)
		}
		if len(res.Rewrites) != 0 {
			t.Errorf("%s door reported a coercion for an untouched PDF: %v", door, res.Rewrites)
		}
	}
}

// A type that claims to be text over bytes that are not stays a document, for
// the wire to refuse. Inlining would put replacement characters in front of the
// model and earn a confident answer about nothing.
func TestTextTypeOverNonUTF8_IsNotInlined(t *testing.T) {
	res, err := sendOpenAI(t, "encode", docBody("text/plain", "b.txt", "\xff\xfe\x00not utf8"))
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if !strings.Contains(string(res.Body), `"file_data"`) {
		t.Error("bytes that are not UTF-8 were inlined as text")
	}
}

// An image format this wire does not decode is refused in OUR words, naming the
// caller's format and the ones that work — not "['png', 'jpeg', 'gif', 'webp']"
// with no mention of what they actually sent.
func TestUnsupportedImageFormat_IsRefusedInOurWords(t *testing.T) {
	for _, mt := range []string{"image/heic", "image/svg+xml", "image/bmp"} {
		for _, door := range []string{"encode", "native"} {
			data := base64.StdEncoding.EncodeToString([]byte("bytes"))
			_, err := sendOpenAI(t, door, `{"model":"gpt-4o-mini","messages":[{"role":"user",`+
				`"content":[{"type":"image_url","image_url":{"url":"data:`+mt+`;base64,`+data+`"}}]}]}`)
			if err == nil {
				t.Fatalf("%s door forwarded %s; the wire answers 400 for it", door, mt)
			}
			var pe *provcore.ProviderError
			if !errors.As(err, &pe) || pe.Status != 400 {
				t.Fatalf("err = %v, want a 400 ProviderError", err)
			}
			if !strings.Contains(pe.Message, mt) {
				t.Errorf("message %q does not name the format that was sent", pe.Message)
			}
			if !strings.Contains(pe.Message, "image/png") {
				t.Errorf("message %q does not say what WOULD work", pe.Message)
			}
		}
	}
}

// The four this wire does decode must still reach it.
func TestSupportedImageFormats_ReachTheWire(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'})
	for _, mt := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		for _, door := range []string{"encode", "native"} {
			if _, err := sendOpenAI(t, door, `{"model":"gpt-4o-mini","messages":[{"role":"user",`+
				`"content":[{"type":"image_url","image_url":{"url":"data:`+mt+`;base64,`+data+
				`"}}]}]}`); err != nil {
				t.Errorf("%s door refused %s: %v", door, mt, err)
			}
		}
	}
}

// A video part has no variant on this wire, and the caller should hear that
// from us — "Invalid value: 'video_url'. Supported values are: …" names a Python
// enum, not their attachment.
func TestVideoPart_IsRefusedInOurWords(t *testing.T) {
	for _, door := range []string{"encode", "native"} {
		_, err := sendOpenAI(t, door, `{"model":"gpt-4o-mini","messages":[{"role":"user",`+
			`"content":[{"type":"video_url","video_url":{"url":"data:video/mp4;base64,QQ=="}}]}]}`)
		if err == nil {
			t.Fatalf("%s door forwarded a video part", door)
		}
		if !strings.Contains(err.Error(), "no video part") {
			t.Errorf("message %q does not state the constraint in our words", err.Error())
		}
	}
}

// Everything ordinary must be untouched — a plain text request, and an image
// request that needs no conversion.
func TestOrdinaryRequests_AreUnchanged(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"gpt-4o-mini","messages":[{"role":"assistant","tool_calls":[` +
			`{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
	} {
		for _, door := range []string{"encode", "native"} {
			res, err := sendOpenAI(t, door, body)
			if err != nil {
				t.Errorf("%s door refused an ordinary request: %v\n  %s", door, err, body)
				continue
			}
			if len(res.Rewrites) != 0 {
				t.Errorf("%s door reported a coercion on an untouched request: %v", door, res.Rewrites)
			}
		}
	}
}

// An attachment a browser labelled application/octet-stream, whose bytes are
// text, must reach THIS wire as a text part — not as a relabelled file part.
//
// The document part here is PDF-only, so a text/plain file 400s exactly as
// octet-stream did: "Invalid file data … Expected a base64-encoded data URL with
// an application/pdf MIME type". Measured on 17 models in the final sweep, after
// a relabel-only fix had already landed — the relabel is applied after the
// content walk, so the inline decision had already seen octet-stream and
// declined. Asserted on the emitted body because a relabel and an inline are
// indistinguishable from "no error".
func TestUndeclaredTextAttachment_RidesAsTextOnThisWire(t *testing.T) {
	doc := base64.StdEncoding.EncodeToString([]byte("# Report\n\nreference 52903\n"))
	for _, door := range []string{"encode", "native"} {
		t.Run(door, func(t *testing.T) {
			spec := NewSpec(nil)
			body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":[` +
				`{"type":"file","file":{"filename":"notes","file_data":"data:application/octet-stream;base64,` + doc + `"}},` +
				`{"type":"text","text":"what is the reference number?"}]}]}`)
			tgt := provcore.CallTarget{ProviderModelID: "gpt-4o"}
			var res provcore.EncodeResult
			var err error
			if door == "encode" {
				res, err = spec.SchemaCodec.EncodeRequest(typology.WireShapeOpenAIChat, body, tgt)
			} else {
				res, err = spec.SchemaCodec.RewriteNative(typology.WireShapeOpenAIChat, body, tgt, false)
			}
			if err != nil {
				t.Fatalf("%s door refused a text attachment labelled octet-stream: %v", door, err)
			}
			part := gjson.GetBytes(res.Body, "messages.0.content.0")
			if got := part.Get("type").String(); got != "text" {
				t.Fatalf("content[0].type = %q, want text — a file part carrying non-PDF bytes "+
					"400s on this wire:\n  %s", got, res.Body)
			}
			if !strings.Contains(part.Get("text").String(), "52903") {
				t.Errorf("the document's characters did not survive: %q", part.Get("text").String())
			}
		})
	}
}
