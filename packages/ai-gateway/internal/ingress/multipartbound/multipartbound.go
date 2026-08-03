// Package multipartbound is the bounded multipart/form-data walker shared by
// the parallel modality handlers (stt, video). It owns the mechanical, DoS-
// bounded part iteration — part-count bound, per-field size bound, single-
// file-part bound, file fingerprinting (sha256/size/sniffed-mime) — while each
// modality package keeps its own semantic layer (governance-field policy,
// required-field validation, typed request struct, model-rewrite re-emit).
//
// The Walk callbacks fire in STREAM ORDER, and PrePart fires before a part's
// content is read: a governance violation at part 2 must surface before a
// bound violation at part 17, and an over-cap key must not force content
// reads it can reject on the part header alone. Callers layer their policy
// through the Visitor rather than post-processing a fully-parsed form, so
// extracting this walker does not reorder any modality's error precedence.
//
// The caller MUST wrap the request body in http.MaxBytesReader before
// walking, so the cumulative read cannot exceed the endpoint's caps ceiling
// even for a chunked / lying-Content-Length upload. This package trusts that
// ceiling for the total size and adds the structural bounds on top.
package multipartbound

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
)

// Bounds are the structural DoS bounds applied by Walk.
type Bounds struct {
	// MaxParts bounds the number of multipart sections (multipart-bomb guard).
	MaxParts int
	// MaxFieldBytes bounds each NON-file form field; a multi-MB text field is
	// an OOM vector otherwise. File parts are bounded by the caller's
	// http.MaxBytesReader ceiling, not this.
	MaxFieldBytes int64
}

// ArtifactRef is a binary-input fingerprint (reference only — the bytes never
// enter the audit body pool). Its JSON shape matches the proxy package's
// ArtifactRef so both serialize identically into traffic_event.artifact_refs.
type ArtifactRef struct {
	Sha256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Mime      string `json:"mime,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Field is one bounded non-file form field, in stream order.
type Field struct {
	Name  string
	Value string
}

// File is the single file part: its raw bytes (bounded by the caller's
// MaxBytesReader ceiling) plus the fingerprint computed while reading.
type File struct {
	FieldName string
	FileName  string
	Bytes     []byte
	Ref       ArtifactRef
}

// Walker-origin errors. Modality packages either surface these directly or
// translate them to their own exported error values (preserving errors.Is
// identity is the modality package's concern).
var (
	ErrTooManyParts  = errors.New("multipart exceeds the part-count bound")
	ErrMultipleFiles = errors.New("multipart carries more than one file part")
	ErrFieldTooLarge = errors.New("a non-file form field exceeds the size bound")
)

// Visitor receives parts in stream order. Any callback returning a non-nil
// error aborts the walk immediately; the error is propagated verbatim (never
// wrapped), so a modality package's own error values keep their identity.
type Visitor struct {
	// PrePart is called with each part's form name (and whether it carries a
	// filename, i.e. is a file part) BEFORE its content is read — the seam
	// for governance-field checks that must fire in stream order.
	PrePart func(name string, isFile bool) error
	// Field is called for each bounded non-file field.
	Field func(name, value string) error
	// File is called for the single file part after its bytes are read and
	// fingerprinted.
	File func(f File) error
}

// Walk iterates a bounded multipart body, invoking the visitor per part.
func Walk(body io.Reader, boundary string, b Bounds, v Visitor) error {
	mr := multipart.NewReader(body, boundary)
	fileCount := 0
	partCount := 0

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			// A MaxBytesReader trip surfaces here as a non-EOF error; callers
			// map *http.MaxBytesError to 413. Any other malformed-multipart
			// error is a 400.
			return err
		}
		partCount++
		if partCount > b.MaxParts {
			return ErrTooManyParts
		}

		name := part.FormName()
		isFile := part.FileName() != ""
		if v.PrePart != nil {
			if err := v.PrePart(name, isFile); err != nil {
				return err
			}
		}

		if isFile {
			fileCount++
			if fileCount > 1 {
				return ErrMultipleFiles
			}
			// Read the content fully (bounded by the caller's MaxBytesReader)
			// while fingerprinting it: sha256 over the true bytes, size from
			// the copy, mime from the declared header with a byte-sniff
			// fallback.
			h := sha256.New()
			buf := &bytes.Buffer{}
			n, err := io.Copy(io.MultiWriter(buf, h), part)
			if err != nil {
				return err
			}
			f := File{
				FieldName: name,
				FileName:  part.FileName(),
				Bytes:     buf.Bytes(),
				Ref: ArtifactRef{
					Sha256:    hex.EncodeToString(h.Sum(nil)),
					SizeBytes: n,
					Mime:      SniffMime(part.Header, buf.Bytes()),
				},
			}
			if v.File != nil {
				if err := v.File(f); err != nil {
					return err
				}
			}
			continue
		}

		val, err := readBoundedField(part, b.MaxFieldBytes)
		if err != nil {
			return err
		}
		if v.Field != nil {
			if err := v.Field(name, val); err != nil {
				return err
			}
		}
	}
}

// readBoundedField reads a non-file part, failing closed if it exceeds
// maxFieldBytes (the +1 read distinguishes "exactly at the bound" from
// "over").
func readBoundedField(part *multipart.Part, maxFieldBytes int64) (string, error) {
	limited := io.LimitReader(part, maxFieldBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return "", err
	}
	if int64(len(b)) > maxFieldBytes {
		return "", ErrFieldTooLarge
	}
	return string(b), nil
}

// SniffMime prefers the part's declared Content-Type, falling back to a byte
// sniff when it is absent OR the generic application/octet-stream (which
// multipart writers set by default and carries no information). Advisory only
// (recorded on the fingerprint) — never a security decision.
func SniffMime(hdr textproto.MIMEHeader, content []byte) string {
	if ct := hdr.Get("Content-Type"); ct != "" {
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = ct[:i]
		}
		ct = strings.TrimSpace(ct)
		if ct != "" && ct != "application/octet-stream" {
			return ct
		}
	}
	return http.DetectContentType(content)
}

// Emit rebuilds a multipart body from ordered fields plus an optional file
// part, returning the body and its Content-Type (carrying the fresh
// boundary). This is the upstream-forward rebuild both modality packages ride
// — the provider only ever sees gateway-emitted canonical parts, never the
// raw client bytes.
func Emit(fields []Field, file *File) ([]byte, string, error) {
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	err := writeForm(w, fields, file)
	return buf.Bytes(), w.FormDataContentType(), err
}

// writeForm writes the form to w. Factored out so a test can drive it with a
// multipart.Writer over a failing io.Writer to exercise the (in production
// unreachable — the buffer never fails) write-error paths.
func writeForm(w *multipart.Writer, fields []Field, file *File) error {
	for _, f := range fields {
		if err := w.WriteField(f.Name, f.Value); err != nil {
			return err
		}
	}
	if file != nil {
		fw, err := w.CreateFormFile(file.FieldName, file.FileName)
		if err == nil {
			_, err = fw.Write(file.Bytes)
		}
		if err != nil {
			return err
		}
	}
	return w.Close()
}
