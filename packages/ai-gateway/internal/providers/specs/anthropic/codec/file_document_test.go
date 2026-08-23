package codec

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// A canonical chat file part is Anthropic's document block. Before this
// mapping existed the part fell through to the pass-through branch and reached
// the Messages API as a chat-shaped {"type":"file"}, which it rejects — so a
// caller's PDF got as far as the wire and no further.
func TestCanonicalFileBecomesAnthropicDocument(t *testing.T) {
	for _, tc := range []struct {
		name       string
		file       string
		wantSource map[string]string
	}{
		{
			name:       "base64 data URL carries the bytes",
			file:       `{"file_data":"data:application/pdf;base64,JVBERi0x"}`,
			wantSource: map[string]string{"type": "base64", "media_type": "application/pdf", "data": "JVBERi0x"},
		},
		{
			name:       "a URL is referenced, not inlined",
			file:       `{"file_url":"https://ex.com/report.pdf"}`,
			wantSource: map[string]string{"type": "url", "url": "https://ex.com/report.pdf"},
		},
		{
			name:       "an uploaded file is referenced by id",
			file:       `{"file_id":"file-abc"}`,
			wantSource: map[string]string{"type": "file", "file_id": "file-abc"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := openAIPartsToAnthropicContent(gjson.Parse(`[{"type":"file","file":` + tc.file + `}]`))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(parts) != 1 || parts[0]["type"] != "document" {
				t.Fatalf("want one document block, got %v", parts)
			}
			src, ok := parts[0]["source"].(map[string]any)
			if !ok {
				t.Fatalf("document has no source object: %v", parts[0])
			}
			for k, want := range tc.wantSource {
				if got, _ := src[k].(string); got != want {
					t.Errorf("source.%s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

// A file part carrying no usable reference is refused, not forwarded empty.
// Forwarding it would ask the model about a document it was never given and
// return 200 — the exact shape of the defect this mapping replaced.
func TestCanonicalFileWithNoReferenceIsRefused(t *testing.T) {
	for _, body := range []string{
		`[{"type":"file","file":{}}]`,
		`[{"type":"file","file":{"filename":"report.pdf"}}]`,
		`[{"type":"file","file":{"file_data":"not-a-data-url"}}]`,
	} {
		if _, err := openAIPartsToAnthropicContent(gjson.Parse(body)); err == nil {
			t.Errorf("%s: expected a refusal, got none — an unusable file must not reach the wire silently", body)
		}
	}
}

// A malformed data: URL is a caller error worth naming, not something to guess
// past. Asserted separately from the empty case because the error identifies a
// different fault.
func TestCanonicalFileMalformedDataURLNamesTheField(t *testing.T) {
	_, err := openAIPartsToAnthropicContent(gjson.Parse(`[{"type":"file","file":{"file_data":"data:application/pdf,notbase64"}}]`))
	if err == nil {
		t.Fatal("expected a refusal for a data: URL that is not base64")
	}
	if !strings.Contains(err.Error(), "file_data") {
		t.Errorf("error should name the offending field, got %q", err)
	}
}

// TestTextualDocumentIsSentAsTextNotAsItsOwnMediaType.
//
// The Messages API reads a document from a text source, and that source takes
// text/plain and nothing else. Forwarding the caller's more specific type —
// text/markdown, application/json — is refused by the wire for a document it
// would otherwise read in full, so the coercion is deliberate and the decoded
// text must survive it intact.
func TestTextualDocumentIsSentAsTextNotAsItsOwnMediaType(t *testing.T) {
	// "# Title" base64-encoded, declared as markdown.
	parts, err := openAIPartsToAnthropicContent(gjson.Parse(
		`[{"type":"file","file":{"file_data":"data:text/markdown;base64,IyBUaXRsZQ=="}}]`))
	if err != nil {
		t.Fatalf("a markdown document was refused: %v", err)
	}
	src, ok := parts[0]["source"].(map[string]any)
	if !ok {
		t.Fatalf("document has no source object: %v", parts[0])
	}
	if src["type"] != "text" {
		t.Errorf("source.type = %v, want text — a textual document sent as base64 reaches "+
			"the wire as an unreadable blob", src["type"])
	}
	if src["media_type"] != "text/plain" {
		t.Errorf("source.media_type = %v, want text/plain — the wire refuses the caller's "+
			"more specific type for a document it would otherwise read", src["media_type"])
	}
	if src["data"] != "# Title" {
		t.Errorf("source.data = %q, want the decoded text", src["data"])
	}
}

// TestADocumentTheWireCannotReadIsRefusedWithItsMediaTypeNamed.
//
// Anthropic reads a document as a PDF or as text. Anything else has to be
// refused here, because forwarding it produces a wire error that names an
// Anthropic field the caller never wrote — and the caller's actual mistake, the
// media type they attached, appears nowhere in it.
//
// The refusal is a 400: the request is at fault and no other provider would do
// better with the same bytes, so the walk must stop rather than spend the
// budget rediscovering it on every target.
func TestADocumentTheWireCannotReadIsRefusedWithItsMediaTypeNamed(t *testing.T) {
	_, err := openAIPartsToAnthropicContent(gjson.Parse(
		`[{"type":"file","file":{"file_data":"data:application/zip;base64,UEsDBA=="}}]`))
	if err == nil {
		t.Fatal("a zip was accepted as a document; it reaches the wire and fails there, " +
			"with an error naming an Anthropic field the caller never wrote")
	}
	assertRequestIsAtFault(t, err)
	if !strings.Contains(err.Error(), "application/zip") {
		t.Errorf("the refusal does not name the media type the caller attached: %v", err)
	}
	if !strings.Contains(err.Error(), "application/pdf") {
		t.Errorf("the refusal does not say what would work, so the caller has to guess: %v", err)
	}
}

// TestADeclaredTextDocumentWhoseBytesAreNotTextIsRefused.
//
// The declared media type says text; the bytes decide. Passing non-UTF-8 into a
// text source sends the model a field of replacement characters and returns
// 200 — a confidently wrong answer about a document nobody could read, which is
// worse than a refusal because nothing in the response says so.
func TestADeclaredTextDocumentWhoseBytesAreNotTextIsRefused(t *testing.T) {
	// 0xFF 0xFE 0xFD — valid base64, not valid UTF-8.
	_, err := openAIPartsToAnthropicContent(gjson.Parse(
		`[{"type":"file","file":{"file_data":"data:text/plain;base64,//79"}}]`))
	if err == nil {
		t.Fatal("bytes that are not UTF-8 were sent as text; the model answers 200 about a " +
			"document it received as replacement characters")
	}
	assertRequestIsAtFault(t, err)
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("the refusal does not say what is wrong with the bytes: %v", err)
	}
}

// TestVideoIsRefusedWithSomethingTheCallerCanActOn.
//
// The Messages API has no video part at all. Left to the pass-through branch a
// video part reaches the wire as a chat-shaped block and comes back as a schema
// error about an unknown type — which tells the caller nothing about video and
// nothing about what to do instead.
func TestVideoIsRefusedWithSomethingTheCallerCanActOn(t *testing.T) {
	_, err := openAIPartsToAnthropicContent(gjson.Parse(
		`[{"type":"video_url","video_url":{"url":"https://ex.com/clip.mp4"}}]`))
	if err == nil {
		t.Fatal("a video part was forwarded to a wire that has no video part")
	}
	assertRequestIsAtFault(t, err)
	if !strings.Contains(err.Error(), "video") {
		t.Errorf("the refusal does not name video as the problem: %v", err)
	}
	if !strings.Contains(err.Error(), "frame") && !strings.Contains(err.Error(), "route") {
		t.Errorf("the refusal offers the caller no way forward: %v", err)
	}
}

// assertRequestIsAtFault pins the error CLASS, not only its wording.
//
// The walk reads the class, not the message. A refusal the caller must fix —
// a media type this wire cannot read, a video part on a wire with no video —
// has to arrive as an invalid-request 400 so the walk stops. Coded as an
// upstream failure instead, the same bytes are re-sent to every remaining
// target in the plan, each one refuses them for the same reason, the retry
// budget is spent, and the caller receives a 502 that names no field. Every
// assertion about the message text survives that change, which is why the
// class is asserted separately.
func assertRequestIsAtFault(t *testing.T, err error) {
	t.Helper()
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T (%v), want a *provcore.ProviderError the walk can classify", err, err)
	}
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("code = %q, want %q — the walk retries this across every target and answers "+
			"502 instead of telling the caller which field to fix", pe.Code, provcore.CodeInvalidRequest)
	}
	if pe.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — a request the caller must change is not a server fault",
			pe.Status)
	}
}
