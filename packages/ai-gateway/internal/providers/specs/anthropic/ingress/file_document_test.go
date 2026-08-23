package ingress_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/ingress"
	"github.com/tidwall/gjson"
)

// Anthropic ingress carrying a PDF must land on the canonical as a file, so it
// can reach a non-Anthropic target. Before this mapping the document block had
// no canonical counterpart and was dropped on the way in — the caller's
// document never left the Anthropic lane.
func TestAnthropicDocumentBecomesCanonicalFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		source  string
		wantKey string
		wantVal string
	}{
		{"base64 keeps the bytes as a data URL", `{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}`,
			"file_data", "data:application/pdf;base64,JVBERi0x"},
		{"url stays a reference", `{"type":"url","url":"https://ex.com/report.pdf"}`,
			"file_url", "https://ex.com/report.pdf"},
		{"an uploaded file keeps its id", `{"type":"file","file_id":"file-abc"}`,
			"file_id", "file-abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := []byte(`{"model":"claude-3-5-haiku-20240307","max_tokens":16,"messages":[{"role":"user","content":[` +
				`{"type":"text","text":"summarise"},{"type":"document","title":"report.pdf","source":` + tc.source + `}]}]}`)
			out, err := ingress.MessagesRequestToOpenAIChatCompletion(native, "claude-3-5-haiku-20240307")
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			var filePart gjson.Result
			gjson.GetBytes(out, "messages.0.content").ForEach(func(_, p gjson.Result) bool {
				if p.Get("type").String() == "file" {
					filePart = p
					return false
				}
				return true
			})
			if !filePart.Exists() {
				t.Fatalf("no canonical file part produced: %s", out)
			}
			if got := filePart.Get("file." + tc.wantKey).String(); got != tc.wantVal {
				t.Errorf("file.%s = %q, want %q", tc.wantKey, got, tc.wantVal)
			}
			if got := filePart.Get("file.filename").String(); got != "report.pdf" {
				t.Errorf("filename = %q; the document title is the only name Anthropic carries and must survive", got)
			}
			if txt := gjson.GetBytes(out, "messages.0.content.0.text").String(); txt != "summarise" {
				t.Errorf("the text part beside the document was lost: %s", out)
			}
		})
	}
}

// A document whose source type is one we do not recognise contributes no file
// part rather than an empty one. An empty file part would claim a document
// reached the model when nothing did.
func TestAnthropicDocumentUnknownSourceProducesNoFilePart(t *testing.T) {
	native := []byte(`{"model":"claude-3-5-haiku-20240307","max_tokens":16,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"hi"},{"type":"document","source":{"type":"telepathy"}}]}]}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(native, "claude-3-5-haiku-20240307")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	gjson.GetBytes(out, "messages.0.content").ForEach(func(_, p gjson.Result) bool {
		if p.Get("type").String() == "file" {
			t.Errorf("an unrecognised document source produced a file part with nothing in it: %s", p.Raw)
		}
		return true
	})
}
