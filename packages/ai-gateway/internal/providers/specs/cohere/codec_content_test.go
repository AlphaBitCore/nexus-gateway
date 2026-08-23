package cohere

import (
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func dataURL(mediaType, body string) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString([]byte(body))
}

func encodeChatOrErr(t *testing.T, canonical string) ([]byte, error) {
	t.Helper()
	res, err := codec{}.EncodeRequest(typology.WireShapeCohereChat, []byte(canonical),
		provcore.CallTarget{ProviderModelID: "command-a-03-2025"})
	return res.Body, err
}

// The defect: a caller's document reached Cohere as a content part it does not
// recognise and came back 422, while the same document in `documents` answers
// out of its own text. What this asserts is the shape that was measured to
// work — the text carried, the part gone from the message, and the question
// still there, because a document with no question is a different request.
func TestFilePart_BecomesADocumentTheWireCarries(t *testing.T) {
	const runbook = "# Runbook\n\nThe reference number is 52903.\n"
	body, err := encodeChatOrErr(t, `{
	  "model":"command-a-03-2025",
	  "messages":[{"role":"user","content":[
	    {"type":"file","file":{"filename":"doc.md","file_data":"`+dataURL("text/markdown", runbook)+`"}},
	    {"type":"text","text":"What is the reference number?"}
	  ]}]}`)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	root := gjson.ParseBytes(body)

	docs := root.Get("documents")
	if !docs.IsArray() || len(docs.Array()) != 1 {
		t.Fatalf("documents = %s, want exactly one entry carrying the file", docs.Raw)
	}
	if got := docs.Get("0.data.text").String(); got != runbook {
		t.Errorf("document text = %q, want the file's own bytes %q", got, runbook)
	}
	if got := docs.Get("0.data.title").String(); got != "doc.md" {
		t.Errorf("document title = %q, want the filename so the model can refer to it", got)
	}

	content := root.Get("messages.0.content")
	if n := len(content.Array()); n != 1 {
		t.Fatalf("message content has %d part(s): %s — the file part must be gone and the "+
			"question must stay", n, content.Raw)
	}
	if got := content.Get("0.text").String(); got != "What is the reference number?" {
		t.Errorf("surviving part = %q, want the caller's question", got)
	}
	if strings.Contains(content.Raw, `"file"`) {
		t.Errorf("a file part still rides in the message: %s — this is the 422", content.Raw)
	}
}

// A binary document has no text to carry, and the gateway does not extract one.
// The refusal has to name the media type, because the alternative a caller gets
// today is Cohere's "unrecognized content type 'file'", which says nothing
// about the actual constraint.
func TestFilePart_BinaryIsRefusedByNameRatherThanForwardedToA422(t *testing.T) {
	_, err := encodeChatOrErr(t, `{
	  "messages":[{"role":"user","content":[
	    {"type":"file","file":{"filename":"doc.pdf","file_data":"`+dataURL("application/pdf", "%PDF-1.4 binary")+`"}},
	    {"type":"text","text":"summarise"}
	  ]}]}`)
	if err == nil {
		t.Fatal("a PDF was encoded for a wire that carries documents as text")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is %T, want *provcore.ProviderError so the caller sees a 400 and not a 500", err)
	}
	if pe.Status != 400 {
		t.Errorf("status = %d, want 400 — the request is the problem, not the upstream", pe.Status)
	}
	if !strings.Contains(pe.Message, "application/pdf") {
		t.Errorf("message %q does not name the media type that could not be carried", pe.Message)
	}
}

// file_id and file_url carry a reference the wire cannot resolve. Both were
// silently reaching Cohere as an unrecognised part.
// The name of this test promises the caller learns what to send instead, so it
// asserts the MESSAGE and not only the status. Asserting the status alone left
// every arm green when a branch was removed, because the catch-all produced a
// 400 too — the caller would have been told they sent no file field at all.
func TestFilePart_ReferencesAreRefusedWithWhatToSendInstead(t *testing.T) {
	for _, tc := range []struct{ name, file, wantIn string }{
		{"file_id", `{"file_id":"file-123"}`, "file_id"},
		{"file_url", `{"file_url":"https://example.com/doc.md"}`, "URL"},
		{"neither", `{"filename":"doc.md"}`, "no file_data"},
		// A data: URL the parser cannot decode under any base64 alphabet.
		{"undecodable_data_url", `{"file_data":"data:text/plain;base64,not valid!!!"}`, "data: URL"},
		// Present but not a data: URL. This used to hit the catch-all and be
		// answered "carries no file_data, file_id or file_url" — denying a field
		// the caller had sent.
		{"file_data_is_a_bare_string", `{"file_data":"hello world"}`, "must be a data: URL"},
		{"file_data_is_an_http_url", `{"file_data":"https://example.com/doc.md"}`, "must be a data: URL"},
		{"file_data_is_bare_base64", `{"file_data":"QUFB"}`, "must be a data: URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := encodeChatOrErr(t, `{"messages":[{"role":"user","content":[
			    {"type":"file","file":`+tc.file+`},
			    {"type":"text","text":"summarise"}]}]}`)
			if err == nil {
				t.Fatalf("%s was encoded; the wire cannot resolve it", tc.name)
			}
			var pe *provcore.ProviderError
			if !errors.As(err, &pe) || pe.Status != 400 {
				t.Fatalf("err = %v, want a 400 ProviderError", err)
			}
			if !strings.Contains(pe.Message, tc.wantIn) {
				t.Errorf("message %q does not mention %q — the caller cannot tell which of "+
					"their fields was the problem", pe.Message, tc.wantIn)
			}
		})
	}
}

// A document with nothing asked about it has no shape on this wire: Cohere
// answers 400 for an empty content array and 400 for an empty string. Saying so
// beats emitting a body that cannot succeed.
func TestFilePart_ADocumentWithNoQuestionIsRefusedRatherThanEmptied(t *testing.T) {
	_, err := encodeChatOrErr(t, `{"messages":[{"role":"user","content":[
	    {"type":"file","file":{"filename":"d.md","file_data":"`+dataURL("text/plain", "hello")+`"}}]}]}`)
	if err == nil {
		t.Fatal("encoded a message whose content would be empty on the wire")
	}
	if !strings.Contains(err.Error(), "no text") {
		t.Errorf("message %q does not say what is missing", err.Error())
	}
}

// A caller may already be using `documents` for retrieval. Lifting must extend
// that array, not renumber over it.
//
// THE FIXTURE IS THE TEST. The first version used caller id "doc-0" with one
// caller document, so the count-based scheme produced "doc-1" and no collision —
// the right assertion over an input that could not reach the defect. Every case
// below is chosen so the count-based scheme WOULD collide: caller ids are the
// ones a count lands on.
func TestFilePart_CallerDocumentsSurviveAndIDsDoNotCollide(t *testing.T) {
	for _, tc := range []struct {
		name        string
		callerDocs  string
		wantSurvive int
	}{
		// count = 1 → "doc-1", which the caller already used.
		{"one caller doc already named doc-1", `[{"id":"doc-1","data":{"text":"A"}}]`, 2},
		// count = 2 → "doc-2", which the caller already used.
		{"caller ids skip ahead", `[{"id":"doc-2","data":{"text":"A"}},{"id":"doc-3","data":{"text":"B"}}]`, 3},
		// Every id a counter could produce is taken.
		{"caller occupies the whole range",
			`[{"id":"doc-0","data":{"text":"A"}},{"id":"doc-1","data":{"text":"B"}}]`, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := encodeChatOrErr(t, `{
			  "documents":`+tc.callerDocs+`,
			  "messages":[{"role":"user","content":[
			    {"type":"file","file":{"filename":"d.md","file_data":"`+dataURL("text/plain", "lifted")+`"}},
			    {"type":"text","text":"q"}]}]}`)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			docs := gjson.ParseBytes(body).Get("documents")
			if len(docs.Array()) != tc.wantSurvive {
				t.Fatalf("documents = %s, want %d entries", docs.Raw, tc.wantSurvive)
			}
			if got := docs.Get("0.data.text").String(); got != "A" {
				t.Errorf("the caller's first document was overwritten: %q", got)
			}
			ids := map[string]bool{}
			docs.ForEach(func(_, d gjson.Result) bool {
				id := d.Get("id").String()
				if ids[id] {
					t.Errorf("duplicate document id %q in %s — Cohere's citations reference "+
						"documents by id, so a cited span would be attributed to the wrong one",
						id, docs.Raw)
				}
				ids[id] = true
				return true
			})
		})
	}
}

// sjson's "documents.-1" appends to an array and writes a literal "-1" KEY
// against anything else, which destroys whatever the caller put there.
func TestFilePart_ANonArrayDocumentsIsRefusedRatherThanOverwritten(t *testing.T) {
	for _, docs := range []string{`{"a":1}`, `"caller-wrote-this"`, `42`} {
		out, err := encodeChatOrErr(t, `{
		  "documents":`+docs+`,
		  "messages":[{"role":"user","content":[
		    {"type":"file","file":{"file_data":"`+dataURL("text/plain", "x")+`"}},
		    {"type":"text","text":"q"}]}]}`)
		if err == nil {
			t.Errorf("documents=%s was encoded; the caller's value became %s",
				docs, gjson.GetBytes(out, "documents").Raw)
			continue
		}
		if !strings.Contains(err.Error(), "array") {
			t.Errorf("message %q does not say what is wrong with documents", err.Error())
		}
	}
}

// The overwhelmingly common request carries no attachment, and must not pay for
// this path. Identity, not merely equivalence: the same bytes come back.
func TestFilePart_ABodyWithNoFilePartIsUntouched(t *testing.T) {
	const canonical = `{"model":"m","messages":[{"role":"user","content":"hello"}],"temperature":0.5}`
	out, err := translateContentForCohereChat([]byte(canonical))
	if err != nil {
		t.Fatalf("lift: %v", err)
	}
	if string(out) != canonical {
		t.Errorf("body was rewritten with no file part present:\n got %s\nwant %s", out, canonical)
	}
}

// The token scan is a filter, not a decision. A body that merely mentions
// "file" — a caller asking about one, a tool named after one — must come back
// byte-identical, because the scan's whole job is to be cheap and wrong in this
// direction only.
func TestFilePart_MentioningTheWordFileIsNotCarryingOne(t *testing.T) {
	for _, canonical := range []string{
		`{"messages":[{"role":"user","content":[{"type":"text","text":"what is a \"file\"?"}]}]}`,
		// No messages array at all: a body shape this function must not assume.
		`{"model":"m","documents":[{"data":{"text":"\"file\""}}]}`,
		// content is a plain string rather than an array of parts.
		`{"messages":[{"role":"user","content":"describe a \"file\""}]}`,
	} {
		out, err := translateContentForCohereChat([]byte(canonical))
		if err != nil {
			t.Fatalf("lift(%s): %v", canonical, err)
		}
		if string(out) != canonical {
			t.Errorf("body rewritten though it carries no file part:\n got %s\nwant %s", out, canonical)
		}
	}
}

// Coverage of the media-type rule, stated as the question it answers: are these
// bytes the document's text?
func TestIsDocumentTextMediaType(t *testing.T) {
	for _, mt := range []string{
		"text/plain", "text/markdown", "TEXT/CSV", "text/plain; charset=utf-8",
		"application/json", "application/yaml", "application/x-ndjson",
		"application/vnd.api+json", "image/svg+xml",
	} {
		if !isDocumentTextMediaType(mt) {
			t.Errorf("%s should carry as document text", mt)
		}
	}
	for _, mt := range []string{
		"application/pdf", "image/png", "audio/wav", "video/mp4",
		// Undeclared: guessing would have to decide whether a printable payload
		// is a text file or the source of a binary one.
		"application/octet-stream", "",
	} {
		if isDocumentTextMediaType(mt) {
			t.Errorf("%s must not be treated as document text", mt)
		}
	}
}

// A media type that claims text over bytes that are not text would put invalid
// UTF-8 into the request JSON.
func TestFilePart_TextTypeOverNonUTF8BytesIsRefused(t *testing.T) {
	bad := "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00})
	_, err := encodeChatOrErr(t, `{"messages":[{"role":"user","content":[
	    {"type":"file","file":{"file_data":"`+bad+`"}},
	    {"type":"text","text":"q"}]}]}`)
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("err = %v, want a refusal naming the encoding", err)
	}
}

// Writes the encoded body for the live probe against api.cohere.com. Skipped
// unless asked for: an offline assertion about a wire is a belief, and this is
// how that belief gets checked against the wire itself.
func TestFilePart_DumpEncodedBodyForLiveProbe(t *testing.T) {
	path := os.Getenv("NEXUS_COHERE_DUMP")
	if path == "" {
		t.Skip("set NEXUS_COHERE_DUMP=<file> to write the encoded body for a live probe")
	}
	doc, err := os.ReadFile("../../../../../../tests/fixtures/media/doc.md")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	body, err := encodeChatOrErr(t, `{
	  "model":"command-a-03-2025","max_tokens":32,
	  "messages":[{"role":"user","content":[
	    {"type":"file","file":{"filename":"doc.md","file_data":"`+dataURL("text/markdown", string(doc))+`"}},
	    {"type":"text","text":"What is the reference number in the attached document? Digits only."}
	  ]}]}`)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// Cohere reads an image only from inline bytes. Measured against api.cohere.com
// with the same PNG: a data: URL answers 200 and describes it, a real reachable
// https URL answers 400 "Invalid url '<url>'" — categorically, not about that
// URL. Production carries 31 of those refusals, each one a caller told their
// perfectly valid URL is invalid.
func TestImageURL_AnUnfetchableURLIsRefusedWithTheRealConstraint(t *testing.T) {
	_, err := encodeChatOrErr(t, `{"messages":[{"role":"user","content":[
	    {"type":"image_url","image_url":{"url":"https://example.com/a.png"}},
	    {"type":"text","text":"what colour?"}]}]}`)
	if err == nil {
		t.Fatal("an external image URL was forwarded; Cohere answers 400 for every one")
	}
	if !strings.Contains(err.Error(), "does not fetch images by URL") {
		t.Errorf("message %q does not state the constraint — a caller reading it cannot tell "+
			"that inlining the image is the fix", err.Error())
	}
}

// The form Cohere does read must still get through.
func TestImageURL_AnInlineImageReachesTheWire(t *testing.T) {
	body, err := encodeChatOrErr(t, `{"messages":[{"role":"user","content":[
	    {"type":"image_url","image_url":{"url":"`+dataURL("image/png", "PNGBYTES")+`"}},
	    {"type":"text","text":"what colour?"}]}]}`)
	if err != nil {
		t.Fatalf("an inline image was refused: %v", err)
	}
	if !strings.Contains(string(body), "image_url") {
		t.Errorf("the image part did not survive to the wire: %s", body)
	}
}
