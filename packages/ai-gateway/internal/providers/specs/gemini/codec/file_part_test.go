package codec

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Gemini carries a document with the same two part shapes it uses for an
// image, so the canonical file part maps straight onto them. Without this the
// part fell through unhandled and the caller's document never reached the
// model.
func TestCanonicalFileBecomesGeminiPart(t *testing.T) {
	t.Run("data URL inlines the bytes", func(t *testing.T) {
		parts, err := openAIMessageToGeminiParts(gjson.Parse(
			`{"role":"user","content":[{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBERi0x"}}]}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(parts) != 1 {
			t.Fatalf("want one part, got %v", parts)
		}
		inline, ok := parts[0]["inlineData"].(map[string]any)
		if !ok {
			t.Fatalf("want inlineData, got %v", parts[0])
		}
		if inline["mimeType"] != "application/pdf" || inline["data"] != "JVBERi0x" {
			t.Errorf("inlineData = %v; the media type and bytes must both survive", inline)
		}
	})

	t.Run("a URL is referenced", func(t *testing.T) {
		parts, err := openAIMessageToGeminiParts(gjson.Parse(
			`{"role":"user","content":[{"type":"file","file":{"file_url":"https://ex.com/report.pdf"}}]}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		fd, ok := parts[0]["fileData"].(map[string]any)
		if !ok {
			t.Fatalf("want fileData, got %v", parts[0])
		}
		if fd["fileUri"] != "https://ex.com/report.pdf" {
			t.Errorf("fileUri = %v", fd["fileUri"])
		}
	})
}

// A bare file_id is an OpenAI-side handle Gemini cannot resolve. Refusing names
// the problem; forwarding an empty part would ask the model about a document it
// never received and still return 200.
func TestCanonicalFileIDIsRefusedOnTheGeminiWire(t *testing.T) {
	_, err := openAIMessageToGeminiParts(gjson.Parse(
		`{"role":"user","content":[{"type":"file","file":{"file_id":"file-abc"}}]}`))
	if err == nil {
		t.Fatal("expected a refusal: an OpenAI file handle is not resolvable on the Gemini wire")
	}
	if !strings.Contains(err.Error(), "file") {
		t.Errorf("error should name the offending field, got %q", err)
	}
}

// A malformed data: URL is refused rather than guessed past.
func TestCanonicalFileMalformedDataURLIsRefused(t *testing.T) {
	if _, err := openAIMessageToGeminiParts(gjson.Parse(
		`{"role":"user","content":[{"type":"file","file":{"file_data":"data:application/pdf,notbase64"}}]}`)); err == nil {
		t.Fatal("expected a refusal for a data: URL that is not base64")
	}
}
