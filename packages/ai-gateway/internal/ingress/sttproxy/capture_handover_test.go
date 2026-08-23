package sttproxy

import (
	"bytes"
	"mime/multipart"
	"strings"
	"testing"
)

// Capture hands the audio over instead of copying it. That is only safe if
// handing it over changes nothing about what goes upstream — storage must
// not pollute transmission, and a shared slice is exactly where that rule
// would break if anyone ever wrote through it.

func buildSTT(t *testing.T, audio []byte) (body []byte, boundary string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("model", "whisper-1"); err != nil {
		t.Fatal(err)
	}
	part, err := w.CreateFormFile("file", "a.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(audio); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.Boundary()
}

func TestAudioHandoverDoesNotDisturbTheForward(t *testing.T) {
	audio := append([]byte{0xFF, 0xFB, 0x90, 0x00}, bytes.Repeat([]byte{0xAB}, 4096)...)
	body, boundary := buildSTT(t, audio)

	req, err := ParseSTTMultipart(bytes.NewReader(body), boundary)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// What capture takes.
	got, mime := req.Audio()
	if !bytes.Equal(got, audio) {
		t.Fatalf("capture would store %d bytes, the caller sent %d", len(got), len(audio))
	}
	if mime == "" {
		t.Fatal("capture needs the type to serve the artifact with")
	}

	// What the forward carries, AFTER capture has taken its reference.
	forward, fwdCT, err := req.ReEmit("whisper-1-provider")
	if err != nil {
		t.Fatalf("re-emit: %v", err)
	}
	if !bytes.Contains(forward, audio) {
		t.Fatal("the forwarded body no longer carries the caller's audio verbatim")
	}
	if !strings.HasPrefix(fwdCT, "multipart/form-data") {
		t.Fatalf("forward content type = %q", fwdCT)
	}

	// And the captured slice still reads as the original after the forward
	// was built — a re-emit that wrote through the shared buffer would show
	// up here, and nowhere else.
	if !bytes.Equal(got, audio) {
		t.Fatal("building the forward mutated the bytes capture is holding")
	}
}

// No file part means nothing to capture, rather than an empty artifact that
// would render as a zero-byte download.
func TestAudioIsNilWhenNoFilePartWasSent(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "whisper-1")
	_ = w.Close()

	if _, err := ParseSTTMultipart(bytes.NewReader(buf.Bytes()), w.Boundary()); err == nil {
		t.Fatal("a multipart with no file part must be rejected, not captured as empty")
	}
}
