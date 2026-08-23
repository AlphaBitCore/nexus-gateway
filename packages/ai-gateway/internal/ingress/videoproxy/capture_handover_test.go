package videoproxy

import (
	"bytes"
	"mime/multipart"
	"testing"
)

// The same property the STT path is held to: capture takes a reference to
// the parsed bytes, and the forward must still carry them verbatim.
func TestInputBytesHandoverDoesNotDisturbTheForward(t *testing.T) {
	img := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0x7F}, 2048)...)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("prompt", "a cat on a roof")
	_ = w.WriteField("model", "sora-2")
	part, err := w.CreateFormFile("input_reference", "ref.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(img); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	req, err := ParseSubmit(bytes.NewReader(buf.Bytes()), w.Boundary())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !req.HasInputRef {
		t.Fatal("the file part was not seen")
	}

	got, mime := req.InputBytes()
	if !bytes.Equal(got, img) {
		t.Fatalf("capture would store %d bytes, the caller sent %d", len(got), len(img))
	}
	if mime == "" {
		t.Fatal("capture needs the type to serve the artifact with")
	}

	forward, _, err := req.ReEmit("sora-2-provider")
	if err != nil {
		t.Fatalf("re-emit: %v", err)
	}
	if !bytes.Contains(forward, img) {
		t.Fatal("the forwarded body no longer carries the caller's image verbatim")
	}
	if !bytes.Equal(got, img) {
		t.Fatal("building the forward mutated the bytes capture is holding")
	}
}

// The file part is optional here, unlike STT. No part means nothing to
// capture — not a zero-byte artifact the reader would try to open.
func TestInputBytesIsNilWithoutAFilePart(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("prompt", "a cat")
	_ = w.WriteField("model", "sora-2")
	_ = w.Close()

	req, err := ParseSubmit(bytes.NewReader(buf.Bytes()), w.Boundary())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.HasInputRef {
		t.Fatal("no file part was sent")
	}
	if got, _ := req.InputBytes(); got != nil {
		t.Fatalf("capture must take nothing, got %d bytes", len(got))
	}
}
