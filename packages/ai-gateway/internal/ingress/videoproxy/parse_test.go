package videoproxy

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

type mpPart struct {
	name        string
	value       string
	fileName    string // non-empty ⇒ file part
	fileContent []byte
	contentType string
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

func parse(t *testing.T, parts []mpPart) (*SubmitRequest, error) {
	t.Helper()
	body, boundary := buildMultipart(t, parts)
	return ParseSubmit(bytes.NewReader(body), boundary)
}

func TestParseSubmit_Valid(t *testing.T) {
	img := []byte("\x89PNG\r\n\x1a\nfake png reference bytes")
	req, err := parse(t, []mpPart{
		{name: "prompt", value: "a calico cat sailing a paper boat"},
		{name: "model", value: "sora-alias"},
		{name: "seconds", value: "8"},
		{name: "size", value: "1280x720"},
		{name: "input_reference", fileName: "ref.png", fileContent: img, contentType: "image/png"},
		{name: "quality", value: "high"}, // unknown extra — forwarded verbatim
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Prompt != "a calico cat sailing a paper boat" {
		t.Errorf("prompt = %q", req.Prompt)
	}
	if req.Model != "sora-alias" {
		t.Errorf("model = %q", req.Model)
	}
	if req.Seconds != "8" || req.SecondsInt != 8 {
		t.Errorf("seconds = %q/%d", req.Seconds, req.SecondsInt)
	}
	if req.Size != "1280x720" {
		t.Errorf("size = %q", req.Size)
	}
	if !req.HasInputRef {
		t.Fatal("input_reference not captured")
	}
	sum := sha256.Sum256(img)
	if req.InputRef.Sha256 != hex.EncodeToString(sum[:]) {
		t.Error("input_reference sha256 mismatch")
	}
	if req.InputRef.SizeBytes != int64(len(img)) {
		t.Errorf("input_reference size = %d want %d", req.InputRef.SizeBytes, len(img))
	}
	if req.InputRef.Mime != "image/png" {
		t.Errorf("input_reference mime = %q", req.InputRef.Mime)
	}
	if len(req.extraFields) != 1 || req.extraFields[0].Name != "quality" || req.extraFields[0].Value != "high" {
		t.Errorf("extraFields = %+v", req.extraFields)
	}
}

func TestParseSubmit_MinimalNoOptionals(t *testing.T) {
	req, err := parse(t, []mpPart{
		{name: "prompt", value: "p"},
		{name: "model", value: "m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.SecondsInt != 0 || req.Seconds != "" {
		t.Errorf("absent seconds must stay zero: %q/%d", req.Seconds, req.SecondsInt)
	}
	if req.HasInputRef {
		t.Error("HasInputRef must be false with no file part")
	}
	if req.ArtifactRefsJSON() != "" {
		t.Errorf("ArtifactRefsJSON must be empty with no file: %q", req.ArtifactRefsJSON())
	}
}

func TestParseSubmit_MissingPrompt(t *testing.T) {
	_, err := parse(t, []mpPart{{name: "model", value: "m"}})
	if !errors.Is(err, ErrMissingPrompt) {
		t.Fatalf("err = %v want ErrMissingPrompt", err)
	}
	// Present-but-empty prompt is equally missing (required non-empty).
	_, err = parse(t, []mpPart{{name: "prompt", value: ""}, {name: "model", value: "m"}})
	if !errors.Is(err, ErrMissingPrompt) {
		t.Fatalf("err = %v want ErrMissingPrompt for empty prompt", err)
	}
}

func TestParseSubmit_MissingModel(t *testing.T) {
	_, err := parse(t, []mpPart{{name: "prompt", value: "p"}})
	if !errors.Is(err, ErrMissingModel) {
		t.Fatalf("err = %v want ErrMissingModel", err)
	}
}

func TestParseSubmit_PromptTooLong(t *testing.T) {
	// 32000 runes is legal; 32001 is not. Use a multi-byte rune to prove the
	// bound is rune-count, not byte-length (32001 × 3 bytes ≫ 32000 bytes).
	ok := strings.Repeat("猫", 32000)
	if _, err := parse(t, []mpPart{{name: "prompt", value: ok}, {name: "model", value: "m"}}); err != nil {
		t.Fatalf("32000-rune prompt must be accepted: %v", err)
	}
	over := strings.Repeat("猫", 32001)
	_, err := parse(t, []mpPart{{name: "prompt", value: over}, {name: "model", value: "m"}})
	if !errors.Is(err, ErrPromptTooLong) {
		t.Fatalf("err = %v want ErrPromptTooLong", err)
	}
}

func TestParseSubmit_SecondsValidation(t *testing.T) {
	for _, bad := range []string{"0", "-4", "4.5", "four", "9999", "301", " 8"} {
		_, err := parse(t, []mpPart{
			{name: "prompt", value: "p"},
			{name: "model", value: "m"},
			{name: "seconds", value: bad},
		})
		if !errors.Is(err, ErrInvalidSeconds) {
			t.Errorf("seconds=%q: err = %v want ErrInvalidSeconds", bad, err)
		}
	}
	// The sanity ceiling itself is accepted (the provider owns the real enum).
	req, err := parse(t, []mpPart{
		{name: "prompt", value: "p"},
		{name: "model", value: "m"},
		{name: "seconds", value: "300"},
	})
	if err != nil || req.SecondsInt != 300 {
		t.Fatalf("seconds=300 must be accepted: %v / %d", err, req.SecondsInt)
	}
}

// The scan-bypass lane (e88-s6 SDD §3): a case-variant of a governance field
// must be rejected, not forwarded — otherwise `Prompt` escapes the prompt
// scanner yet reaches a case-folding provider as the effective prompt.
func TestParseSubmit_CaseSmuggledGovernanceRejected(t *testing.T) {
	for _, variant := range []string{"Prompt", "PROMPT", "pRoMpT", "Model", "SECONDS", "Size", "Input_Reference", "INPUT_REFERENCE"} {
		_, err := parse(t, []mpPart{
			{name: "prompt", value: "benign"},
			{name: "model", value: "m"},
			{name: variant, value: "smuggled"},
		})
		if !errors.Is(err, ErrCaseSmuggledField) {
			t.Errorf("variant %q: err = %v want ErrCaseSmuggledField", variant, err)
		}
	}
}

func TestParseSubmit_DuplicateGovernanceRejected(t *testing.T) {
	for _, dup := range []string{"prompt", "model", "seconds", "size"} {
		parts := []mpPart{
			{name: "prompt", value: "p"},
			{name: "model", value: "m"},
			{name: "seconds", value: "8"},
			{name: "size", value: "1280x720"},
			{name: dup, value: "again"},
		}
		_, err := parse(t, parts)
		if !errors.Is(err, ErrDuplicateField) {
			t.Errorf("dup %q: err = %v want ErrDuplicateField", dup, err)
		}
	}
}

func TestParseSubmit_WalkerBoundsTranslated(t *testing.T) {
	// Part-count bound.
	parts := []mpPart{{name: "prompt", value: "p"}, {name: "model", value: "m"}}
	for range maxParts {
		parts = append(parts, mpPart{name: "x", value: "v"})
	}
	if _, err := parse(t, parts); !errors.Is(err, ErrTooManyParts) {
		t.Errorf("err = %v want ErrTooManyParts", err)
	}
	// Two file parts.
	_, err := parse(t, []mpPart{
		{name: "prompt", value: "p"},
		{name: "model", value: "m"},
		{name: "input_reference", fileName: "a.png", fileContent: []byte("x")},
		{name: "other_file", fileName: "b.png", fileContent: []byte("y")},
	})
	if !errors.Is(err, ErrMultipleFiles) {
		t.Errorf("err = %v want ErrMultipleFiles", err)
	}
	// Oversized non-file field (beyond the 128 KiB byte bound, single-byte runes).
	_, err = parse(t, []mpPart{
		{name: "prompt", value: "p"},
		{name: "model", value: "m"},
		{name: "extra", value: strings.Repeat("A", maxFieldBytes+1)},
	})
	if !errors.Is(err, ErrFieldTooLarge) {
		t.Errorf("err = %v want ErrFieldTooLarge", err)
	}
}

func TestParseSubmit_MalformedBody(t *testing.T) {
	_, err := ParseSubmit(strings.NewReader("not multipart"), "nonexistent-boundary")
	if err == nil {
		t.Fatal("expected an error for a malformed multipart body")
	}
}

func TestReEmit_RewritesModelPreservesRest(t *testing.T) {
	img := []byte("reference-image-bytes")
	req, err := parse(t, []mpPart{
		{name: "prompt", value: "a red panda"},
		{name: "model", value: "gateway-alias"},
		{name: "seconds", value: "12"},
		{name: "size", value: "720x1280"},
		{name: "input_reference", fileName: "ref.jpg", fileContent: img, contentType: "image/jpeg"},
		{name: "custom_knob", value: "42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, ct, err := req.ReEmit("sora-2")
	if err != nil {
		t.Fatal(err)
	}
	_, params, _ := mime.ParseMediaType(ct)
	re, err := ParseSubmit(bytes.NewReader(body), params["boundary"])
	if err != nil {
		t.Fatalf("re-emitted body did not re-parse: %v", err)
	}
	if re.Model != "sora-2" {
		t.Errorf("model not rewritten: %q", re.Model)
	}
	if re.Prompt != "a red panda" || re.Seconds != "12" || re.Size != "720x1280" {
		t.Errorf("governance fields lost: %q/%q/%q", re.Prompt, re.Seconds, re.Size)
	}
	if !re.HasInputRef || !bytes.Equal(re.file.Bytes, img) || re.file.FileName != "ref.jpg" {
		t.Error("input_reference bytes/name altered")
	}
	var sawKnob bool
	for _, f := range re.extraFields {
		if f.Name == "custom_knob" && f.Value == "42" {
			sawKnob = true
		}
	}
	if !sawKnob {
		t.Errorf("extra field lost: %+v", re.extraFields)
	}
}

func TestReEmit_OmitsAbsentOptionals(t *testing.T) {
	req, err := parse(t, []mpPart{
		{name: "prompt", value: "p"},
		{name: "model", value: "alias"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, ct, err := req.ReEmit("prov")
	if err != nil {
		t.Fatal(err)
	}
	_, params, _ := mime.ParseMediaType(ct)
	re, err := ParseSubmit(bytes.NewReader(body), params["boundary"])
	if err != nil {
		t.Fatal(err)
	}
	// Absent seconds/size/file must not materialize as empty fields on the wire
	// (an empty `seconds` would 400 at the provider).
	if re.Seconds != "" || re.Size != "" || re.HasInputRef {
		t.Errorf("absent optionals materialized: %q/%q/%v", re.Seconds, re.Size, re.HasInputRef)
	}
}

func TestArtifactRefsJSON(t *testing.T) {
	req := &SubmitRequest{
		HasInputRef: true,
		InputRef:    ArtifactRef{Sha256: "abc", SizeBytes: 42, Mime: "image/png"},
	}
	want := `[{"sha256":"abc","sizeBytes":42,"mime":"image/png"}]`
	if got := req.ArtifactRefsJSON(); got != want {
		t.Errorf("ArtifactRefsJSON = %s want %s", got, want)
	}
}

// A file part named with a governance-field case variant must be rejected the
// same as a text part — the file lane must not bypass the case-fold policy.
func TestParseSubmit_FilePartCaseVariantRejected(t *testing.T) {
	_, err := parse(t, []mpPart{
		{name: "prompt", value: "p"},
		{name: "model", value: "m"},
		{name: "Input_Reference", fileName: "ref.png", fileContent: []byte("x")},
	})
	if !errors.Is(err, ErrCaseSmuggledField) {
		t.Fatalf("err = %v want ErrCaseSmuggledField", err)
	}
}

// A TEXT-valued input_reference (e.g. a URL) must be rejected, never forwarded
// as an un-fingerprinted extra: the provider might honor a reference the
// gateway never recorded in artifact_refs.
func TestParseSubmit_TextInputReferenceRejected(t *testing.T) {
	_, err := parse(t, []mpPart{
		{name: "prompt", value: "p"},
		{name: "model", value: "m"},
		{name: "input_reference", value: "https://example.com/ref.png"},
	})
	if !errors.Is(err, ErrInputReferenceNotFile) {
		t.Fatalf("err = %v want ErrInputReferenceNotFile", err)
	}
}

// Duplicate input_reference detection is order-independent across the
// file/text lanes: file-then-text and text-then-file both abort. (The text
// occurrence alone would already be ErrInputReferenceNotFile; ordered second
// it must still never displace or shadow the fingerprinted file.)
func TestParseSubmit_DuplicateInputReferenceEitherOrder(t *testing.T) {
	fileFirst := []mpPart{
		{name: "prompt", value: "p"},
		{name: "model", value: "m"},
		{name: "input_reference", fileName: "a.png", fileContent: []byte("x")},
		{name: "input_reference", value: "https://example.com/b.png"},
	}
	if _, err := parse(t, fileFirst); !errors.Is(err, ErrDuplicateField) {
		t.Errorf("file-then-text: err = %v want ErrDuplicateField", err)
	}
	textFirst := []mpPart{
		{name: "prompt", value: "p"},
		{name: "model", value: "m"},
		{name: "input_reference", value: "https://example.com/b.png"},
		{name: "input_reference", fileName: "a.png", fileContent: []byte("x")},
	}
	if _, err := parse(t, textFirst); !errors.Is(err, ErrInputReferenceNotFile) {
		t.Errorf("text-then-file: err = %v want ErrInputReferenceNotFile (text form rejected at first occurrence)", err)
	}
	twoFiles := []mpPart{
		{name: "prompt", value: "p"},
		{name: "model", value: "m"},
		{name: "input_reference", fileName: "a.png", fileContent: []byte("x")},
		{name: "input_reference", fileName: "b.png", fileContent: []byte("y")},
	}
	if _, err := parse(t, twoFiles); !errors.Is(err, ErrDuplicateField) {
		t.Errorf("file-then-file: err = %v want ErrDuplicateField", err)
	}
}

// The Veo-leg accessors: ExtraFieldNames surfaces the passthrough extras (the
// allow-list-only leg refuses on any), and InputFileBytes exposes the
// input_reference bytes + sniffed mime for JSON inlining.
func TestSubmitRequest_VeoLegAccessors(t *testing.T) {
	req, err := parse(t, []mpPart{
		{name: "prompt", value: "a cat"},
		{name: "model", value: "veo-3.1"},
		{name: "style", value: "anime"},
		{name: "watermark", value: "off"},
		{name: "input_reference", fileName: "ref.png", fileContent: []byte("\x89PNG\r\n\x1a\nxxxx")},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	names := req.ExtraFieldNames()
	if len(names) != 2 || names[0] != "style" || names[1] != "watermark" {
		t.Errorf("ExtraFieldNames = %v, want [style watermark]", names)
	}
	data, mimeType, fieldName, ok := req.InputFileBytes()
	if !ok || string(data[:4]) != "\x89PNG" || mimeType != "image/png" || fieldName != "input_reference" {
		t.Errorf("InputFileBytes = (%d bytes, %q, %q, %v), want the PNG bytes with sniffed mime and field name", len(data), mimeType, fieldName, ok)
	}

	// A file part under a non-canonical name still parses (the OpenAI leg
	// re-emits it); the FieldName is surfaced so the allow-list-only Veo leg
	// can reject it.
	reqAlt, err := parse(t, []mpPart{
		{name: "prompt", value: "x"}, {name: "model", value: "m"},
		{name: "style_reference", fileName: "s.png", fileContent: []byte("\x89PNGyyyy")},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, _, fn, ok := reqAlt.InputFileBytes(); !ok || fn != "style_reference" {
		t.Errorf("InputFileBytes field name = (%q, %v), want (style_reference, true)", fn, ok)
	}

	// No file part → ok=false; no extras → empty list.
	req2, err := parse(t, []mpPart{{name: "prompt", value: "x"}, {name: "model", value: "m"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, _, _, ok := req2.InputFileBytes(); ok {
		t.Error("InputFileBytes ok=true without a file part")
	}
	if got := req2.ExtraFieldNames(); len(got) != 0 {
		t.Errorf("ExtraFieldNames = %v, want empty", got)
	}
}
