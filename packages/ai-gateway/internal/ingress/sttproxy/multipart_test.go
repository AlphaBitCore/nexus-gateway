package sttproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"mime"
	"mime/multipart"
	"strings"
	"testing"
)

// buildMultipart assembles a multipart body from ordered parts. A part with
// fileName != "" is a file part carrying content as bytes.
type mpPart struct {
	name        string
	value       string
	fileName    string // non-empty ⇒ file part
	fileContent []byte
	contentType string // optional explicit Content-Type for a file part
}

func buildMultipart(t *testing.T, parts []mpPart) (body []byte, boundary string) {
	t.Helper()
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	for _, p := range parts {
		if p.fileName != "" {
			if p.contentType != "" {
				h := make(map[string][]string)
				h["Content-Disposition"] = []string{`form-data; name="` + p.name + `"; filename="` + p.fileName + `"`}
				h["Content-Type"] = []string{p.contentType}
				fw, err := w.CreatePart(h)
				if err != nil {
					t.Fatal(err)
				}
				fw.Write(p.fileContent)
				continue
			}
			fw, err := w.CreateFormFile(p.name, p.fileName)
			if err != nil {
				t.Fatal(err)
			}
			fw.Write(p.fileContent)
			continue
		}
		if err := w.WriteField(p.name, p.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.Boundary()
}

func parse(t *testing.T, parts []mpPart) (*STTRequest, error) {
	t.Helper()
	body, boundary := buildMultipart(t, parts)
	return ParseSTTMultipart(bytes.NewReader(body), boundary)
}

func TestParse_Valid(t *testing.T) {
	audio := []byte("RIFF....fake wav bytes....")
	req, err := parse(t, []mpPart{
		{name: "model", value: "whisper-alias"},
		{name: "response_format", value: "verbose_json"},
		{name: "language", value: "en"},
		{name: "file", fileName: "a.wav", fileContent: audio, contentType: "audio/wav"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Model != "whisper-alias" {
		t.Errorf("model = %q", req.Model)
	}
	if req.ResponseFormat != "verbose_json" {
		t.Errorf("response_format = %q", req.ResponseFormat)
	}
	// Fingerprint must be the sha256 + size of the true audio bytes.
	sum := sha256.Sum256(audio)
	if req.AudioRef.Sha256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 mismatch")
	}
	if req.AudioRef.SizeBytes != int64(len(audio)) {
		t.Errorf("size = %d want %d", req.AudioRef.SizeBytes, len(audio))
	}
	if req.AudioRef.Mime != "audio/wav" {
		t.Errorf("mime = %q want audio/wav", req.AudioRef.Mime)
	}
	// language is a non-governance field, forwarded verbatim.
	if len(req.extraFields) != 1 || req.extraFields[0].name != "language" || req.extraFields[0].value != "en" {
		t.Errorf("extraFields = %+v", req.extraFields)
	}
}

func TestParse_StreamCapturedNotForwarded(t *testing.T) {
	req, err := parse(t, []mpPart{
		{name: "model", value: "m"},
		{name: "stream", value: "true"},
		{name: "file", fileName: "a.mp3", fileContent: []byte("x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Stream != "true" {
		t.Errorf("Stream = %q, want true", req.Stream)
	}
	// stream must NOT land in extraFields — a v1a-deferred stream=true must never
	// be re-emitted to the upstream.
	for _, f := range req.extraFields {
		if f.name == "stream" {
			t.Errorf("stream leaked into extraFields (would be forwarded): %+v", req.extraFields)
		}
	}
}

func TestParse_DefaultsResponseFormatEmpty(t *testing.T) {
	req, err := parse(t, []mpPart{
		{name: "model", value: "m"},
		{name: "file", fileName: "a.mp3", fileContent: []byte("x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.ResponseFormat != "" {
		t.Errorf("response_format should default empty, got %q", req.ResponseFormat)
	}
}

func TestParse_MissingFile(t *testing.T) {
	_, err := parse(t, []mpPart{{name: "model", value: "m"}})
	if !errors.Is(err, ErrMissingFile) {
		t.Fatalf("err = %v want ErrMissingFile", err)
	}
}

func TestParse_MissingModel(t *testing.T) {
	_, err := parse(t, []mpPart{{name: "file", fileName: "a.wav", fileContent: []byte("x")}})
	if !errors.Is(err, ErrMissingModel) {
		t.Fatalf("err = %v want ErrMissingModel", err)
	}
}

func TestParse_MultipleFiles(t *testing.T) {
	_, err := parse(t, []mpPart{
		{name: "model", value: "m"},
		{name: "file", fileName: "a.wav", fileContent: []byte("x")},
		{name: "file2", fileName: "b.wav", fileContent: []byte("y")},
	})
	if !errors.Is(err, ErrMultipleFiles) {
		t.Fatalf("err = %v want ErrMultipleFiles", err)
	}
}

func TestParse_DuplicateModelRejected(t *testing.T) {
	_, err := parse(t, []mpPart{
		{name: "model", value: "m1"},
		{name: "model", value: "m2"},
		{name: "file", fileName: "a.wav", fileContent: []byte("x")},
	})
	if !errors.Is(err, ErrDuplicateField) {
		t.Fatalf("err = %v want ErrDuplicateField", err)
	}
}

func TestParse_DuplicateResponseFormatRejected(t *testing.T) {
	_, err := parse(t, []mpPart{
		{name: "model", value: "m"},
		{name: "response_format", value: "json"},
		{name: "response_format", value: "text"},
		{name: "file", fileName: "a.wav", fileContent: []byte("x")},
	})
	if !errors.Is(err, ErrDuplicateField) {
		t.Fatalf("err = %v want ErrDuplicateField", err)
	}
}

func TestParse_FieldTooLarge(t *testing.T) {
	_, err := parse(t, []mpPart{
		{name: "model", value: strings.Repeat("A", maxFieldBytes+1)},
		{name: "file", fileName: "a.wav", fileContent: []byte("x")},
	})
	if !errors.Is(err, ErrFieldTooLarge) {
		t.Fatalf("err = %v want ErrFieldTooLarge", err)
	}
}

func TestParse_TooManyParts(t *testing.T) {
	parts := []mpPart{{name: "model", value: "m"}}
	for range maxParts {
		parts = append(parts, mpPart{name: "x", value: "v"})
	}
	parts = append(parts, mpPart{name: "file", fileName: "a.wav", fileContent: []byte("x")})
	_, err := parse(t, parts)
	if !errors.Is(err, ErrTooManyParts) {
		t.Fatalf("err = %v want ErrTooManyParts", err)
	}
}

func TestSniffMime_FallsBackToDetect(t *testing.T) {
	// No declared Content-Type on the file part → http.DetectContentType. A PNG
	// magic prefix is detected as image/png (proves the sniff path runs).
	png := []byte("\x89PNG\r\n\x1a\n and more bytes to sniff")
	req, err := parse(t, []mpPart{
		{name: "model", value: "m"},
		{name: "file", fileName: "a.bin", fileContent: png},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.AudioRef.Mime != "image/png" {
		t.Errorf("sniffed mime = %q want image/png", req.AudioRef.Mime)
	}
}

func TestReEmit_RewritesModelPreservesRest(t *testing.T) {
	audio := []byte("audio-bytes-1234")
	req, err := parse(t, []mpPart{
		{name: "model", value: "gateway-alias"},
		{name: "response_format", value: "text"},
		{name: "temperature", value: "0.2"},
		{name: "file", fileName: "clip.mp3", fileContent: audio},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, ct, err := req.ReEmit("whisper-1")
	if err != nil {
		t.Fatal(err)
	}
	// Parse the re-emitted body back and assert the rewrite + preservation.
	_, params, _ := mime.ParseMediaType(ct)
	reparsed, err := ParseSTTMultipart(bytes.NewReader(body), params["boundary"])
	if err != nil {
		t.Fatalf("re-emitted body did not re-parse: %v", err)
	}
	if reparsed.Model != "whisper-1" {
		t.Errorf("model not rewritten: %q", reparsed.Model)
	}
	if reparsed.ResponseFormat != "text" {
		t.Errorf("response_format lost: %q", reparsed.ResponseFormat)
	}
	if !bytes.Equal(reparsed.audio, audio) {
		t.Errorf("audio bytes altered")
	}
	var sawTemp bool
	for _, f := range reparsed.extraFields {
		if f.name == "temperature" && f.value == "0.2" {
			sawTemp = true
		}
	}
	if !sawTemp {
		t.Errorf("temperature field lost: %+v", reparsed.extraFields)
	}
}

func TestArtifactRefsJSON(t *testing.T) {
	req := &STTRequest{AudioRef: ArtifactRef{Sha256: "abc", SizeBytes: 42, Mime: "audio/mpeg"}}
	got := req.ArtifactRefsJSON()
	want := `[{"sha256":"abc","sizeBytes":42,"mime":"audio/mpeg"}]`
	if got != want {
		t.Errorf("ArtifactRefsJSON = %s want %s", got, want)
	}
}

func TestParse_MalformedBoundary(t *testing.T) {
	_, err := ParseSTTMultipart(strings.NewReader("not multipart"), "nonexistent-boundary")
	if err == nil {
		t.Fatal("expected an error for a malformed multipart body")
	}
}

func TestSniffMime_DeclaredWithParamsStripped(t *testing.T) {
	req, err := parse(t, []mpPart{
		{name: "model", value: "m"},
		{name: "file", fileName: "a.mp3", fileContent: []byte("x"), contentType: "audio/mpeg; rate=44100"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.AudioRef.Mime != "audio/mpeg" {
		t.Errorf("mime = %q want audio/mpeg (params stripped)", req.AudioRef.Mime)
	}
}

// errAfterReader yields n bytes from the wrapped reader then returns an error,
// so an io.Copy / io.ReadAll over a mid-part cut fails.
type errAfterReader struct {
	r    *bytes.Reader
	left int
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if e.left <= 0 {
		return 0, errors.New("simulated mid-stream read failure")
	}
	if len(p) > e.left {
		p = p[:e.left]
	}
	n, _ := e.r.Read(p)
	e.left -= n
	return n, nil
}

func TestParse_FileReadError(t *testing.T) {
	// A valid body cut mid-file: the file part header is read, then io.Copy of
	// the audio hits the injected error.
	body, boundary := buildMultipart(t, []mpPart{
		{name: "model", value: "m"},
		{name: "file", fileName: "a.wav", fileContent: bytes.Repeat([]byte("A"), 2000)},
	})
	cut := len(body) - 500 // partway through the file content
	_, err := ParseSTTMultipart(&errAfterReader{r: bytes.NewReader(body), left: cut}, boundary)
	if err == nil {
		t.Fatal("expected a read error to propagate from the file copy")
	}
}

// Write-error propagation for the rebuild is covered by the shared walker's
// tests (internal/ingress/multipartbound — writeForm over a failing writer);
// ReEmit here is a thin field-assembly layer over multipartbound.Emit.
