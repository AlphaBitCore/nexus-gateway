package sttproxy

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"
)

func buildPromptMultipart(t *testing.T, prompt string) (io.Reader, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	_ = w.WriteField("model", "whisper-1")
	if prompt != "" {
		_ = w.WriteField("prompt", prompt)
	}
	fw, _ := w.CreateFormFile("file", "a.mp3")
	_, _ = fw.Write([]byte("RIFFdata"))
	_ = w.Close()
	return buf, w.Boundary()
}

// TestPromptAccessors pins the scan seam: Prompt reads the parsed field,
// SetPrompt's replacement is what ReEmit forwards, and both are no-ops
// when the request carried no prompt.
func TestPromptAccessors(t *testing.T) {
	t.Run("read + replace flows into ReEmit", func(t *testing.T) {
		body, boundary := buildPromptMultipart(t, "call cat@example.com")
		req, err := ParseSTTMultipart(body, boundary)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got := req.Prompt(); got != "call cat@example.com" {
			t.Fatalf("Prompt() = %q", got)
		}
		req.SetPrompt("call [REDACTED_EMAIL]")
		out, ct, err := req.ReEmit("whisper-1-provider")
		if err != nil {
			t.Fatalf("ReEmit: %v", err)
		}
		_, params, _ := mime.ParseMediaType(ct)
		mr := multipart.NewReader(bytes.NewReader(out), params["boundary"])
		var forwarded string
		for {
			p, perr := mr.NextPart()
			if perr != nil {
				break
			}
			if p.FormName() == "prompt" {
				b, _ := io.ReadAll(p)
				forwarded = string(b)
			}
		}
		if forwarded != "call [REDACTED_EMAIL]" {
			t.Fatalf("ReEmit forwarded prompt = %q; want the redacted value", forwarded)
		}
		if strings.Contains(string(out), "cat@example.com") {
			t.Fatal("original PII still present in the re-emitted body")
		}
	})

	t.Run("absent prompt: read empty, set no-op", func(t *testing.T) {
		body, boundary := buildPromptMultipart(t, "")
		req, err := ParseSTTMultipart(body, boundary)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if req.Prompt() != "" {
			t.Fatalf("Prompt() = %q; want empty", req.Prompt())
		}
		req.SetPrompt("x") // must not invent a field
		out, _, _ := req.ReEmit("m")
		if strings.Contains(string(out), `name="prompt"`) {
			t.Fatal("SetPrompt invented a prompt field on a promptless request")
		}
	})
}
