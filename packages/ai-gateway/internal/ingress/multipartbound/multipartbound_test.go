package multipartbound

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"mime"
	"mime/multipart"
	"net/textproto"
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

// collect walks the body recording everything the visitor sees.
type collected struct {
	pre    []string // "name|isFile"
	fields []Field
	file   *File
}

func walkCollect(t *testing.T, parts []mpPart, b Bounds) (*collected, error) {
	t.Helper()
	body, boundary := buildMultipart(t, parts)
	c := &collected{}
	err := Walk(bytes.NewReader(body), boundary, b, Visitor{
		PrePart: func(name string, isFile bool) error {
			tag := name + "|field"
			if isFile {
				tag = name + "|file"
			}
			c.pre = append(c.pre, tag)
			return nil
		},
		Field: func(name, value string) error {
			c.fields = append(c.fields, Field{Name: name, Value: value})
			return nil
		},
		File: func(f File) error {
			c.file = &f
			return nil
		},
	})
	return c, err
}

var stdBounds = Bounds{MaxParts: 8, MaxFieldBytes: 64}

func TestWalk_FieldsAndFileInStreamOrder(t *testing.T) {
	content := []byte("binary-content-0123456789")
	c, err := walkCollect(t, []mpPart{
		{name: "alpha", value: "1"},
		{name: "upload", fileName: "u.bin", fileContent: content, contentType: "video/mp4"},
		{name: "beta", value: "2"},
	}, stdBounds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// PrePart saw every part in stream order, with the file flagged.
	wantPre := []string{"alpha|field", "upload|file", "beta|field"}
	if len(c.pre) != len(wantPre) {
		t.Fatalf("pre = %v want %v", c.pre, wantPre)
	}
	for i := range wantPre {
		if c.pre[i] != wantPre[i] {
			t.Errorf("pre[%d] = %q want %q", i, c.pre[i], wantPre[i])
		}
	}
	// Fields preserved in order with values.
	if len(c.fields) != 2 || c.fields[0] != (Field{"alpha", "1"}) || c.fields[1] != (Field{"beta", "2"}) {
		t.Errorf("fields = %+v", c.fields)
	}
	// File carries the true bytes + a fingerprint of them.
	if c.file == nil {
		t.Fatal("file part not delivered")
	}
	if c.file.FieldName != "upload" || c.file.FileName != "u.bin" {
		t.Errorf("file identity = %q/%q", c.file.FieldName, c.file.FileName)
	}
	if !bytes.Equal(c.file.Bytes, content) {
		t.Error("file bytes altered")
	}
	sum := sha256.Sum256(content)
	if c.file.Ref.Sha256 != hex.EncodeToString(sum[:]) {
		t.Error("sha256 mismatch")
	}
	if c.file.Ref.SizeBytes != int64(len(content)) {
		t.Errorf("size = %d want %d", c.file.Ref.SizeBytes, len(content))
	}
	if c.file.Ref.Mime != "video/mp4" {
		t.Errorf("mime = %q want declared video/mp4", c.file.Ref.Mime)
	}
}

func TestWalk_TooManyParts(t *testing.T) {
	parts := make([]mpPart, 0, stdBounds.MaxParts+1)
	for i := 0; i <= stdBounds.MaxParts; i++ {
		parts = append(parts, mpPart{name: "x", value: "v"})
	}
	_, err := walkCollect(t, parts, stdBounds)
	if !errors.Is(err, ErrTooManyParts) {
		t.Fatalf("err = %v want ErrTooManyParts", err)
	}
}

func TestWalk_MultipleFiles(t *testing.T) {
	_, err := walkCollect(t, []mpPart{
		{name: "a", fileName: "a.bin", fileContent: []byte("x")},
		{name: "b", fileName: "b.bin", fileContent: []byte("y")},
	}, stdBounds)
	if !errors.Is(err, ErrMultipleFiles) {
		t.Fatalf("err = %v want ErrMultipleFiles", err)
	}
}

func TestWalk_FieldTooLarge(t *testing.T) {
	_, err := walkCollect(t, []mpPart{
		{name: "big", value: strings.Repeat("A", int(stdBounds.MaxFieldBytes)+1)},
	}, stdBounds)
	if !errors.Is(err, ErrFieldTooLarge) {
		t.Fatalf("err = %v want ErrFieldTooLarge", err)
	}
}

func TestWalk_FieldExactlyAtBoundAccepted(t *testing.T) {
	c, err := walkCollect(t, []mpPart{
		{name: "edge", value: strings.Repeat("A", int(stdBounds.MaxFieldBytes))},
	}, stdBounds)
	if err != nil {
		t.Fatalf("a field exactly at the bound must be accepted: %v", err)
	}
	if len(c.fields) != 1 || int64(len(c.fields[0].Value)) != stdBounds.MaxFieldBytes {
		t.Errorf("fields = %d values", len(c.fields))
	}
}

// Visitor errors abort the walk and propagate VERBATIM (never wrapped) — the
// modality packages' own error values must keep errors.Is identity.
func TestWalk_VisitorErrorsPropagateVerbatim(t *testing.T) {
	sentinel := errors.New("policy violation")
	body, boundary := buildMultipart(t, []mpPart{
		{name: "a", value: "1"},
		{name: "f", fileName: "f.bin", fileContent: []byte("x")},
	})

	err := Walk(bytes.NewReader(body), boundary, stdBounds, Visitor{
		PrePart: func(name string, _ bool) error {
			if name == "a" {
				return sentinel
			}
			return nil
		},
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("PrePart error = %v, want the sentinel verbatim", err)
	}

	err = Walk(bytes.NewReader(body), boundary, stdBounds, Visitor{
		Field: func(string, string) error { return sentinel },
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Field error = %v, want the sentinel verbatim", err)
	}

	err = Walk(bytes.NewReader(body), boundary, stdBounds, Visitor{
		File: func(File) error { return sentinel },
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("File error = %v, want the sentinel verbatim", err)
	}
}

// A stream-order guarantee that matters: a PrePart policy violation at an
// early part surfaces BEFORE a structural bound violation later in the body.
func TestWalk_PrePartFiresBeforeLaterBoundViolation(t *testing.T) {
	sentinel := errors.New("early governance violation")
	parts := []mpPart{{name: "gov", value: "dup"}}
	for i := 0; i <= stdBounds.MaxParts; i++ { // would trip ErrTooManyParts if reached
		parts = append(parts, mpPart{name: "x", value: "v"})
	}
	body, boundary := buildMultipart(t, parts)
	err := Walk(bytes.NewReader(body), boundary, stdBounds, Visitor{
		PrePart: func(name string, _ bool) error {
			if name == "gov" {
				return sentinel
			}
			return nil
		},
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the early PrePart sentinel (stream order)", err)
	}
}

func TestWalk_NilCallbacksTolerated(t *testing.T) {
	body, boundary := buildMultipart(t, []mpPart{
		{name: "a", value: "1"},
		{name: "f", fileName: "f.bin", fileContent: []byte("x")},
	})
	if err := Walk(bytes.NewReader(body), boundary, stdBounds, Visitor{}); err != nil {
		t.Fatalf("nil visitor callbacks must be tolerated: %v", err)
	}
}

func TestWalk_MalformedBody(t *testing.T) {
	// A body with no boundary at all: Go's multipart reader yields zero parts
	// (io.EOF on the first NextPart), so the walk ends cleanly WITHOUT
	// visiting anything — the modality packages' required-field validation is
	// what rejects such a request (their tests pin that). The contract here is
	// that no phantom part is ever delivered.
	err := Walk(strings.NewReader("not multipart"), "nonexistent-boundary", stdBounds, Visitor{
		PrePart: func(name string, _ bool) error {
			t.Errorf("phantom part %q delivered from a boundary-less body", name)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("boundary-less body must end the walk cleanly with zero parts: %v", err)
	}

	// A body with a syntactically invalid part header (no colon) is a real
	// multipart error and must propagate. (A body merely truncated mid-header
	// reads as EOF — zero parts — same as the boundary-less case above.)
	bad := "--bnd\r\nbad header line no colon\r\n\r\nvalue\r\n--bnd--\r\n"
	if err := Walk(strings.NewReader(bad), "bnd", stdBounds, Visitor{}); err == nil {
		t.Fatal("expected an error for a malformed part header")
	}
}

// errAfterReader yields n bytes from the wrapped reader then returns an error,
// so the file-content io.Copy over a mid-part cut fails.
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

func TestWalk_FileReadErrorPropagates(t *testing.T) {
	body, boundary := buildMultipart(t, []mpPart{
		{name: "f", fileName: "f.bin", fileContent: bytes.Repeat([]byte("A"), 2000)},
	})
	cut := len(body) - 500 // partway through the file content
	err := Walk(&errAfterReader{r: bytes.NewReader(body), left: cut}, boundary, stdBounds, Visitor{})
	if err == nil {
		t.Fatal("expected a read error to propagate from the file copy")
	}
}

func TestWalk_FieldReadErrorPropagates(t *testing.T) {
	// The stream cuts partway through a TEXT field's content, so the bounded
	// field read (not the file copy) hits the error.
	body, boundary := buildMultipart(t, []mpPart{
		{name: "big", value: strings.Repeat("A", 40)},
	})
	cut := len(body) - 60 // inside the field content
	err := Walk(&errAfterReader{r: bytes.NewReader(body), left: cut}, boundary, stdBounds, Visitor{})
	if err == nil {
		t.Fatal("expected a read error to propagate from the bounded field read")
	}
}

func TestSniffMime(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n and more bytes to sniff")
	cases := []struct {
		name     string
		declared string
		content  []byte
		want     string
	}{
		{"declared wins", "video/mp4", png, "video/mp4"},
		{"declared params stripped", "audio/mpeg; rate=44100", png, "audio/mpeg"},
		{"absent falls back to sniff", "", png, "image/png"},
		{"octet-stream falls back to sniff", "application/octet-stream", png, "image/png"},
		{"whitespace-only declared falls back", "  ; x=1", png, "image/png"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hdr := textproto.MIMEHeader{}
			if c.declared != "" {
				hdr.Set("Content-Type", c.declared)
			}
			if got := SniffMime(hdr, c.content); got != c.want {
				t.Errorf("SniffMime = %q want %q", got, c.want)
			}
		})
	}
}

func TestEmit_RoundTrips(t *testing.T) {
	content := []byte("file-bytes-42")
	body, ct, err := Emit(
		[]Field{{Name: "model", Value: "prov-model"}, {Name: "size", Value: "720x1280"}},
		&File{FieldName: "input_reference", FileName: "ref.png", Bytes: content},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("Emit content-type unparsable: %v", err)
	}
	c := &collected{}
	err = Walk(bytes.NewReader(body), params["boundary"], Bounds{MaxParts: 8, MaxFieldBytes: 1 << 10}, Visitor{
		Field: func(name, value string) error {
			c.fields = append(c.fields, Field{name, value})
			return nil
		},
		File: func(f File) error { c.file = &f; return nil },
	})
	if err != nil {
		t.Fatalf("emitted body did not re-parse: %v", err)
	}
	if len(c.fields) != 2 || c.fields[0] != (Field{"model", "prov-model"}) || c.fields[1] != (Field{"size", "720x1280"}) {
		t.Errorf("fields = %+v", c.fields)
	}
	if c.file == nil || c.file.FieldName != "input_reference" || c.file.FileName != "ref.png" || !bytes.Equal(c.file.Bytes, content) {
		t.Errorf("file round-trip failed: %+v", c.file)
	}
}

func TestEmit_NoFile(t *testing.T) {
	body, ct, err := Emit([]Field{{Name: "prompt", Value: "a cat"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, params, _ := mime.ParseMediaType(ct)
	c := &collected{}
	err = Walk(bytes.NewReader(body), params["boundary"], stdBounds, Visitor{
		Field: func(name, value string) error {
			c.fields = append(c.fields, Field{name, value})
			return nil
		},
		File: func(File) error {
			t.Error("no file part was emitted; File callback must not fire")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.fields) != 1 || c.fields[0] != (Field{"prompt", "a cat"}) {
		t.Errorf("fields = %+v", c.fields)
	}
}

// failWriter fails all writes after failAfter bytes have been written.
type failWriter struct {
	written   int
	failAfter int
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.written >= f.failAfter {
		return 0, errors.New("simulated write failure")
	}
	f.written += len(p)
	return len(p), nil
}

func TestWriteForm_WriteErrorsPropagate(t *testing.T) {
	fields := []Field{{Name: "model", Value: "alias"}, {Name: "language", Value: "en"}}
	file := &File{FieldName: "file", FileName: "a.wav", Bytes: []byte("audio")}

	// failAfter=0 → the first WriteField fails.
	if err := writeForm(multipart.NewWriter(&failWriter{failAfter: 0}), fields, file); err == nil {
		t.Error("expected a WriteField error to propagate")
	}
	// A larger budget lets the fields + file-part header through, then the
	// write of the content (or Close) fails — exercising the later returns.
	if err := writeForm(multipart.NewWriter(&failWriter{failAfter: 400}), fields, file); err == nil {
		t.Error("expected a later write error to propagate")
	}
	// No file: the Close error path.
	if err := writeForm(multipart.NewWriter(&failWriter{failAfter: 100}), fields, nil); err == nil {
		t.Error("expected the Close error to propagate on the no-file path")
	}
}
