package anthropic_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Drives the codec the spec SHIPS. A test that calls the translation function
// directly proves the translation works and says nothing about whether the
// adapter reaches it.
func encode(t *testing.T, mediaType, body string) (gjson.Result, error) {
	t.Helper()
	spec := anthropic.NewSpec(nil)
	data := base64.StdEncoding.EncodeToString([]byte(body))
	canonical := `{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":[
	    {"type":"file","file":{"filename":"doc","file_data":"data:` + mediaType + `;base64,` + data + `"}},
	    {"type":"text","text":"what is the reference number?"}]}]}`
	res, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeAnthropicMessages,
		[]byte(canonical), provcore.CallTarget{ProviderModelID: "claude-sonnet-4"})
	if err != nil {
		return gjson.Result{}, err
	}
	return gjson.GetBytes(res.Body, "messages.0.content.0"), nil
}

// A base64 document source is PDF-only on this wire. Measured against
// api.anthropic.com with the same markdown bytes: base64 answers
// "document.source.base64.media_type: Input should be 'application/pdf'" and a
// text source answers 200 having read the document.
//
// So a text document must arrive as a text source carrying the DECODED
// characters. Emitting base64 for it is what turned a document this wire reads
// into a 400 the caller could do nothing about.
func TestTextDocument_RidesAsATextSource(t *testing.T) {
	const text = "# Report\n\nThe reference number is 52903.\n"
	for _, mediaType := range []string{
		"text/markdown", "text/plain", "text/csv", "application/json", "application/vnd.api+json",
	} {
		t.Run(mediaType, func(t *testing.T) {
			block, err := encode(t, mediaType, text)
			if err != nil {
				t.Fatalf("%s was refused: %v — this wire reads it", mediaType, err)
			}
			if got := block.Get("type").String(); got != "document" {
				t.Fatalf("block type = %q, want document", got)
			}
			if got := block.Get("source.type").String(); got != "text" {
				t.Errorf("source.type = %q, want text — base64 is PDF-only and answers 400 here", got)
			}
			// The text source takes text/plain and nothing more specific.
			if got := block.Get("source.media_type").String(); got != "text/plain" {
				t.Errorf("source.media_type = %q, want text/plain", got)
			}
			// The characters themselves, not the encoding of them.
			if got := block.Get("source.data").String(); got != text {
				t.Errorf("source.data = %q, want the decoded document", got)
			}
		})
	}
}

// The one type base64 IS for must keep using it.
func TestPDF_KeepsRidingAsBase64(t *testing.T) {
	const pdf = "%PDF-1.4 binary-ish bytes"
	block, err := encode(t, "application/pdf", pdf)
	if err != nil {
		t.Fatalf("a PDF was refused: %v", err)
	}
	if got := block.Get("source.type").String(); got != "base64" {
		t.Errorf("source.type = %q, want base64", got)
	}
	if got := block.Get("source.media_type").String(); got != "application/pdf" {
		t.Errorf("source.media_type = %q, want application/pdf", got)
	}
	raw, decErr := base64.StdEncoding.DecodeString(block.Get("source.data").String())
	if decErr != nil || string(raw) != pdf {
		t.Errorf("source.data did not carry the PDF bytes (err=%v)", decErr)
	}
}

// Neither PDF nor text means this wire has no document shape for it. Refuse in
// our own words: forwarding it earns the caller a message about someone else's
// media_type enum, which names neither their attachment nor what would work.
func TestBinaryDocument_IsRefusedInOurWords(t *testing.T) {
	for _, mediaType := range []string{"application/zip", "application/octet-stream", "audio/wav"} {
		t.Run(mediaType, func(t *testing.T) {
			_, err := encode(t, mediaType, "\x00\x01binary")
			if err == nil {
				t.Fatalf("%s was forwarded; this wire has no document source for it", mediaType)
			}
			var pe *provcore.ProviderError
			if !errors.As(err, &pe) || pe.Status != 400 {
				t.Fatalf("err = %v, want a 400 ProviderError", err)
			}
			if !strings.Contains(pe.Message, mediaType) {
				t.Errorf("message %q does not name the type that was sent", pe.Message)
			}
			if !strings.Contains(pe.Message, "application/pdf") {
				t.Errorf("message %q does not say what WOULD work, which is the actionable half",
					pe.Message)
			}
		})
	}
}

// A type that claims to be text over bytes that are not would reach the model as
// replacement characters and come back as a confident answer about nothing —
// the accepted-but-not-read failure, manufactured by us.
func TestTextTypeOverNonUTF8Bytes_IsRefused(t *testing.T) {
	_, err := encode(t, "text/plain", "\xff\xfe\x00not utf8")
	if err == nil {
		t.Fatal("bytes that are not UTF-8 were sent as a text document")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("message %q does not say the bytes contradict the declared type", err.Error())
	}
}

// THE NATIVE DOOR. A caller on /v1/messages already speaks this wire and picks
// its own source shape; the differential must not reinterpret it. Asserted
// rather than assumed, because a translation applied to the native leg would
// rewrite a caller's deliberate choice — and the reverse mistake, gating only
// one door, has shipped here before.
func TestNativeDoor_LeavesTheCallersDocumentSourceAlone(t *testing.T) {
	spec := anthropic.NewSpec(nil)
	for _, source := range []string{
		`{"type":"text","media_type":"text/plain","data":"reference 52903"}`,
		`{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}`,
		`{"type":"content","content":[{"type":"text","text":"reference 52903"}]}`,
	} {
		native := `{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":[
		    {"type":"document","source":` + source + `}]}]}`
		res, err := spec.SchemaCodec.RewriteNative(typology.WireShapeAnthropicMessages,
			[]byte(native), provcore.CallTarget{ProviderModelID: "claude-sonnet-4"}, false)
		if err != nil {
			t.Fatalf("the native door refused a document it did not build: %v\n  %s", err, source)
		}
		got := gjson.GetBytes(res.Body, "messages.0.content.0.source").Raw
		if !gjson.Valid(got) || gjson.Parse(got).Get("type").String() !=
			gjson.Parse(source).Get("type").String() {
			t.Errorf("the native door rewrote the caller's source\n  sent %s\n  got  %s", source, got)
		}
	}
}

// A video part has no block on this wire, and the caller should hear that from
// us. Measured before this: "Input tag 'video_url' found using 'type' does not
// match any of the expected tags: 'bash_code_execution_tool_result', …" — a
// list of tag names, on eighteen production requests.
func TestVideoPart_IsRefusedInOurWords(t *testing.T) {
	spec := anthropic.NewSpec(nil)
	_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeAnthropicMessages,
		[]byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":[
		    {"type":"video_url","video_url":{"url":"data:video/mp4;base64,QQ=="}},
		    {"type":"text","text":"what is in this video?"}]}]}`),
		provcore.CallTarget{ProviderModelID: "claude-sonnet-4"})
	if err == nil {
		t.Fatal("a video part was forwarded; the wire answers 400 with its own tag list")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) || pe.Status != 400 {
		t.Fatalf("err = %v, want a 400 ProviderError", err)
	}
	if !strings.Contains(pe.Message, "no video part") {
		t.Errorf("message %q does not state the constraint", pe.Message)
	}
	if !strings.Contains(pe.Message, "image") {
		t.Errorf("message %q does not say what WOULD work", pe.Message)
	}
}

// A block kind this codec does not know must still ride through — the
// forward-compatibility path the video case is deliberately carved out of.
func TestUnknownBlockKind_StillRidesThrough(t *testing.T) {
	spec := anthropic.NewSpec(nil)
	res, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeAnthropicMessages,
		[]byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":[
		    {"type":"server_tool_use","id":"t1","name":"web_search","input":{}}]}]}`),
		provcore.CallTarget{ProviderModelID: "claude-sonnet-4"})
	if err != nil {
		t.Fatalf("an unknown block kind was refused: %v", err)
	}
	if got := gjson.GetBytes(res.Body, "messages.0.content.0.type").String(); got != "server_tool_use" {
		t.Errorf("the unknown block did not survive: type = %q", got)
	}
}
