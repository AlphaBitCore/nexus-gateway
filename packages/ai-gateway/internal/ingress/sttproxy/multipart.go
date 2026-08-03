// Package sttproxy implements the parallel streaming-proxy handler for the STT
// (speech-to-text) modality — multipart /v1/audio/transcriptions and
// /v1/audio/translations — which does NOT go through the small-JSON ServeProxy
// pipeline (see e88-s5 SDD "Architecture decision"). This file owns the STT
// semantic layer over the shared bounded multipart walker
// (internal/ingress/multipartbound): governance-field policy, required-field
// validation, and the model-rewrite re-emit. The forward, admission, and audit
// tail live alongside it.
package sttproxy

import (
	"errors"
	"fmt"
	"io"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/multipartbound"
)

const (
	// maxParts bounds the number of multipart sections (multipart-bomb guard).
	// A real STT request has file + model + a handful of options — 16 is generous.
	maxParts = 16

	// maxFieldBytes bounds each NON-file form field. model / response_format /
	// language / temperature / prompt are all tiny; a multi-MB "model" field is
	// an OOM vector otherwise. 8 KiB is >100× any legitimate value.
	maxFieldBytes = 8 << 10
)

// ArtifactRef is the input-audio fingerprint recorded on the audit row
// (reference only — the audio bytes never enter the audit body pool, R-7). It
// is the shared walker's fingerprint type; the alias keeps this package's API
// unchanged, and its JSON shape matches the proxy package's ArtifactRef so the
// two serialize identically into traffic_event.artifact_refs.
type ArtifactRef = multipartbound.ArtifactRef

// formField is a non-governance form field forwarded verbatim to the upstream.
type formField struct {
	name  string
	value string
}

// STTRequest is the parsed, bounds-validated multipart request.
type STTRequest struct {
	// Model is the client-supplied model string (a gateway alias); the forward
	// rewrites it to the resolved provider model ID via ReEmit.
	Model string
	// ResponseFormat is the requested response_format ("" = json default).
	ResponseFormat string
	// Stream is the raw `stream` form field value ("" when absent). The OpenAI
	// transcribe models trigger SSE streaming via this field (NOT via a
	// response_format value); the handler gates it (streaming deferred in v1a).
	Stream string
	// AudioRef is the input-audio fingerprint (sha256/size/mime) for the audit
	// row — reference only, never the bytes (R-7).
	AudioRef ArtifactRef

	fileFieldName string
	fileFileName  string
	audio         []byte // the audio bytes, for ReEmit; NEVER handed to the audit pool
	extraFields   []formField
}

// Errors distinguishable by the handler for the right HTTP status.
var (
	// ErrMissingFile / ErrMissingModel / ErrDuplicateField / ErrTooManyParts /
	// ErrMultipleFiles / ErrFieldTooLarge map to 400 (client multipart is
	// malformed or violates a governance bound). ErrUploadTooLarge maps to 413
	// (the caller detects http.MaxBytesReader's limit error separately).
	ErrMissingFile    = errors.New("stt: multipart is missing the required 'file' part")
	ErrMissingModel   = errors.New("stt: multipart is missing the required 'model' field")
	ErrMultipleFiles  = errors.New("stt: multipart carries more than one file part")
	ErrTooManyParts   = errors.New("stt: multipart exceeds the part-count bound")
	ErrFieldTooLarge  = errors.New("stt: a non-file form field exceeds the size bound")
	ErrDuplicateField = errors.New("stt: a governance field (model/response_format) is duplicated")
)

// governanceFields are the fields the gateway routes/reshapes on; a duplicate of
// any of them splits the scanner-vs-provider view (R-5) and is rejected. STT
// matching is exact-lowercase (the OpenAI STT wire's field names); the video
// parser layers the stricter case-folded policy for its scanned prompt leaf.
var governanceFields = map[string]struct{}{
	"model":           {},
	"response_format": {},
}

// ParseSTTMultipart reads a bounded multipart/form-data STT body into memory and
// validates it. The caller MUST wrap the request body in
// http.MaxBytesReader(w, r.Body, capsMaxBytes) BEFORE calling this, so the
// cumulative read cannot exceed the STT caps ceiling (~26 MiB) even for a
// chunked / lying-Content-Length upload — the walker trusts that ceiling for
// the total size and adds the per-part / per-field structural bounds on top.
//
// v1a buffers the audio (bounded by that ceiling) rather than streaming it:
// order-independent and simple, and R-7 still holds (the audio goes only to the
// fingerprint and the re-emitted forward body via ReEmit, never the audit body
// pool). A streaming re-emit is a v1b memory optimization.
func ParseSTTMultipart(body io.Reader, boundary string) (*STTRequest, error) {
	out := &STTRequest{}
	seen := map[string]struct{}{}

	err := multipartbound.Walk(body, boundary,
		multipartbound.Bounds{MaxParts: maxParts, MaxFieldBytes: maxFieldBytes},
		multipartbound.Visitor{
			PrePart: func(name string, _ bool) error {
				if _, isGov := governanceFields[name]; isGov {
					if _, dup := seen[name]; dup {
						return ErrDuplicateField
					}
					seen[name] = struct{}{}
				}
				return nil
			},
			File: func(f multipartbound.File) error {
				out.fileFieldName = f.FieldName
				out.fileFileName = f.FileName
				out.audio = f.Bytes
				out.AudioRef = f.Ref
				return nil
			},
			Field: func(name, val string) error {
				switch name {
				case "model":
					out.Model = val
				case "response_format":
					out.ResponseFormat = val
				case "stream":
					// Captured for the handler's streaming gate; NOT forwarded via
					// extraFields — v1a defers SSE streaming, so a forwarded stream=true
					// must never reach the upstream.
					out.Stream = val
				default:
					out.extraFields = append(out.extraFields, formField{name: name, value: val})
				}
				return nil
			},
		})
	if err != nil {
		return nil, sttError(err)
	}

	if out.audio == nil {
		return nil, ErrMissingFile
	}
	if out.Model == "" {
		return nil, ErrMissingModel
	}
	return out, nil
}

// sttError translates walker-origin bound errors to this package's exported
// error values so handler status mapping and errors.Is identity are unchanged
// by the shared-walker extraction. Visitor-origin errors (ErrDuplicateField)
// and transport/multipart errors pass through verbatim.
func sttError(err error) error {
	switch {
	case errors.Is(err, multipartbound.ErrTooManyParts):
		return ErrTooManyParts
	case errors.Is(err, multipartbound.ErrMultipleFiles):
		return ErrMultipleFiles
	case errors.Is(err, multipartbound.ErrFieldTooLarge):
		return ErrFieldTooLarge
	}
	return err
}

// Prompt returns the caller-supplied `prompt` form field ("" when absent).
// The prompt is the one free-text leaf of the STT request — the request-stage
// compliance scan reads it through this accessor.
func (s *STTRequest) Prompt() string {
	for _, f := range s.extraFields {
		if f.name == "prompt" {
			return f.value
		}
	}
	return ""
}

// SetPrompt replaces the `prompt` form field value so a redacted prompt is
// what ReEmit forwards upstream (the redact-re-emit arm of the request-stage
// scan). No-op when the request carried no prompt field.
func (s *STTRequest) SetPrompt(v string) {
	for i := range s.extraFields {
		if s.extraFields[i].name == "prompt" {
			s.extraFields[i].value = v
			return
		}
	}
}

// ReEmit rebuilds the multipart body for the upstream forward with the model
// field rewritten to providerModelID and every other field/the audio preserved.
// Returns the new body plus its Content-Type (carrying the fresh boundary).
func (s *STTRequest) ReEmit(providerModelID string) ([]byte, string, error) {
	fields := []multipartbound.Field{{Name: "model", Value: providerModelID}}
	if s.ResponseFormat != "" {
		fields = append(fields, multipartbound.Field{Name: "response_format", Value: s.ResponseFormat})
	}
	for _, f := range s.extraFields {
		fields = append(fields, multipartbound.Field{Name: f.name, Value: f.value})
	}
	return multipartbound.Emit(fields, &multipartbound.File{
		FieldName: s.fileFieldName,
		FileName:  s.fileFileName,
		Bytes:     s.audio,
	})
}

// ArtifactRefsJSON returns the input-audio fingerprint as the single-element
// artifact_refs JSON array for the audit row.
func (s *STTRequest) ArtifactRefsJSON() string {
	return fmt.Sprintf(`[{"sha256":%q,"sizeBytes":%d,"mime":%q}]`,
		s.AudioRef.Sha256, s.AudioRef.SizeBytes, s.AudioRef.Mime)
}
