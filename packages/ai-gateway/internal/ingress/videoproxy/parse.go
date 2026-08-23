// Package videoproxy implements the request-parsing layer for the async
// video-generation modality — the multipart POST /v1/videos submit — which is
// served by a parallel handler family, NOT the small-JSON ServeProxy pipeline
// (see e88-s6 SDD D-V8). This file owns the video semantic layer over the
// shared bounded multipart walker (internal/ingress/multipartbound):
// case-folded governance-field policy, required-field and seconds validation,
// the input_reference fingerprint, and the model-rewrite re-emit. The handler
// concerns (admission, prompt scan, forward, job insert, audit tail) belong
// to the proxy package's ServeVideo* family, which consumes this parser.
package videoproxy

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/multipartbound"
)

const (
	// maxParts bounds the number of multipart sections (multipart-bomb guard).
	// A real submit has prompt + model + seconds + size + input_reference — 16
	// is generous.
	maxParts = 16

	// maxFieldBytes bounds each NON-file form field. The prompt may legally be
	// up to maxPromptChars runes, which in worst-case UTF-8 is 4 bytes per
	// rune; 128 KiB covers that with headroom while still shutting a
	// multi-MB field flood. The rune-count validation below is the tighter,
	// spec-level prompt bound.
	maxFieldBytes = 128 << 10

	// maxPromptChars is the provider-documented prompt ceiling (OpenAI
	// /v1/videos: 1–32,000 characters). Enforced as a rune count so the bound
	// matches the provider's character semantics, not byte length.
	maxPromptChars = 32000

	// maxSeconds is a sanity ceiling on the requested duration so a typo'd
	// seconds=9999 cannot distort the advisory cost estimate; the provider's
	// own (narrower, widening-over-time) enum remains the authority and its
	// 400 passes through. See e88-s6 SDD §3.
	maxSeconds = 300
)

// ArtifactRef is the input_reference fingerprint recorded on the audit row
// (reference only — the image bytes never enter the audit body pool).
type ArtifactRef = multipartbound.ArtifactRef

// SubmitRequest is the parsed, bounds-validated video submit.
type SubmitRequest struct {
	// Prompt is the extracted generation prompt — the leaf the compliance
	// pipeline scans (SDD D-V7) before any forward.
	Prompt string
	// Model is the client-supplied model string (a gateway alias); the forward
	// rewrites it to the resolved provider model ID via ReEmit.
	Model string
	// Seconds is the validated raw `seconds` string ("" when absent).
	Seconds string
	// SecondsInt is the parsed requested duration (0 when absent; the cost
	// estimate falls back to the provider default of 4 at the handler).
	SecondsInt int
	// Size is the requested frame size ("" when absent; provider validates).
	Size string
	// InputRef is the input_reference fingerprint (zero value when the
	// optional file part is absent). Recorded whether or not the bytes are
	// captured, so an event proves WHICH reference image was submitted.
	InputRef ArtifactRef
	// HasInputRef reports whether a file part was present.
	HasInputRef bool

	file        *multipartbound.File
	extraFields []multipartbound.Field
}

// Errors distinguishable by the handler for the right HTTP status. All map to
// 400 (client multipart is malformed or violates a governance bound) except
// the http.MaxBytesError 413, which the caller detects separately.
var (
	ErrMissingPrompt  = errors.New("video: multipart is missing the required non-empty 'prompt' field")
	ErrPromptTooLong  = errors.New("video: 'prompt' exceeds the 32000-character bound")
	ErrMissingModel   = errors.New("video: multipart is missing the required 'model' field")
	ErrInvalidSeconds = errors.New("video: 'seconds' must be a positive integer string no greater than 300")
	ErrTooManyParts   = errors.New("video: multipart exceeds the part-count bound")
	ErrMultipleFiles  = errors.New("video: multipart carries more than one file part")
	ErrFieldTooLarge  = errors.New("video: a non-file form field exceeds the size bound")
	ErrDuplicateField = errors.New("video: a governance field (prompt/model/seconds/size/input_reference) is duplicated")
	// ErrCaseSmuggledField rejects a part whose name case-folds equal to a
	// governance field without being its exact lowercase form ("Prompt",
	// "PROMPT"). Such a part would bypass the gateway's prompt scan yet ride
	// the forwarded extras to a case-folding provider — a scan bypass on the
	// one leaf the safety story rests on (e88-s6 SDD §3).
	ErrCaseSmuggledField = errors.New("video: a form field name case-folds to a governance field but is not its lowercase form")
	// ErrInputReferenceNotFile rejects a TEXT-valued input_reference part. The
	// field is defined as a file part; a text form (e.g. a URL) would ride the
	// forwarded extras as a reference the provider might honor but the gateway
	// never fingerprinted — a silent gap on the one recorded-reference lane. A
	// governance-named field is captured or rejected, never forwarded blind.
	ErrInputReferenceNotFile = errors.New("video: 'input_reference' must be a file part, not a text field")
)

// governanceFields are the fields the gateway scans/routes/estimates on.
// Matching is CASE-FOLDED: an exact-lowercase occurrence is consumed, any
// case-variant is rejected, and duplicates are counted under folding.
var governanceFields = [...]string{"prompt", "model", "seconds", "size", "input_reference"}

// ParseSubmit reads a bounded multipart/form-data video submit into memory
// and validates it. The caller MUST wrap the request body in
// http.MaxBytesReader(w, r.Body, capsMaxBytes) BEFORE calling this, so the
// cumulative read (including the optional input_reference image) cannot
// exceed the video caps ceiling (16 MiB) even for a chunked /
// lying-Content-Length upload — the walker trusts that ceiling for the total
// size and adds the per-part / per-field structural bounds on top.
func ParseSubmit(body io.Reader, boundary string) (*SubmitRequest, error) {
	out := &SubmitRequest{}
	seen := map[string]struct{}{}

	err := multipartbound.Walk(body, boundary,
		multipartbound.Bounds{MaxParts: maxParts, MaxFieldBytes: maxFieldBytes},
		multipartbound.Visitor{
			PrePart: func(name string, isFile bool) error {
				for _, gov := range governanceFields {
					if !strings.EqualFold(name, gov) {
						continue
					}
					if name != gov {
						return ErrCaseSmuggledField
					}
					if _, dup := seen[gov]; dup {
						return ErrDuplicateField
					}
					if gov == "input_reference" && !isFile {
						return ErrInputReferenceNotFile
					}
					seen[gov] = struct{}{}
				}
				return nil
			},
			File: func(f multipartbound.File) error {
				out.file = &f
				out.InputRef = f.Ref
				out.HasInputRef = true
				return nil
			},
			Field: func(name, val string) error {
				switch name {
				case "prompt":
					out.Prompt = val
				case "model":
					out.Model = val
				case "seconds":
					out.Seconds = val
				case "size":
					out.Size = val
				default:
					out.extraFields = append(out.extraFields, multipartbound.Field{Name: name, Value: val})
				}
				return nil
			},
		})
	if err != nil {
		return nil, videoError(err)
	}

	if out.Prompt == "" {
		return nil, ErrMissingPrompt
	}
	if utf8.RuneCountInString(out.Prompt) > maxPromptChars {
		return nil, ErrPromptTooLong
	}
	if out.Model == "" {
		return nil, ErrMissingModel
	}
	if out.Seconds != "" {
		n, err := strconv.Atoi(out.Seconds)
		if err != nil || n <= 0 || n > maxSeconds {
			return nil, ErrInvalidSeconds
		}
		out.SecondsInt = n
	}
	return out, nil
}

// videoError translates walker-origin bound errors to this package's exported
// error values so handler status mapping reads one vocabulary. Visitor-origin
// errors and transport/multipart errors pass through verbatim.
func videoError(err error) error {
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

// ReEmit rebuilds the multipart body for the upstream forward with the model
// field rewritten to providerModelID and every other governance field, extra
// field, and the input_reference bytes preserved. The provider only ever sees
// this gateway-emitted rebuild — never the raw client bytes — so the scanned
// prompt and the forwarded prompt are the same value by construction.
func (s *SubmitRequest) ReEmit(providerModelID string) ([]byte, string, error) {
	fields := []multipartbound.Field{
		{Name: "prompt", Value: s.Prompt},
		{Name: "model", Value: providerModelID},
	}
	if s.Seconds != "" {
		fields = append(fields, multipartbound.Field{Name: "seconds", Value: s.Seconds})
	}
	if s.Size != "" {
		fields = append(fields, multipartbound.Field{Name: "size", Value: s.Size})
	}
	fields = append(fields, s.extraFields...)
	return multipartbound.Emit(fields, s.file)
}

// ExtraFieldNames lists the names of multipart fields outside the canonical
// governance set. The OpenAI leg forwards them verbatim inside ReEmit (native
// passthrough); the allow-list-only Veo leg rejects a submit that carries any
// (an untranslatable field must never forward raw).
func (s *SubmitRequest) ExtraFieldNames() []string {
	names := make([]string, 0, len(s.extraFields))
	for _, f := range s.extraFields {
		names = append(names, f.Name)
	}
	return names
}

// InputFileBytes returns the file part's bytes, sniffed mime, and FIELD NAME,
// or ok=false when no file part was present. The field name is exposed so the
// allow-list-only Veo leg can reject a file part sent under any name other
// than input_reference (the walker accepts a file part under any name; only
// the caller knows the leg's canonical field set). The Veo leg inlines the
// image into its JSON body (base64) instead of re-emitting a multipart part.
func (s *SubmitRequest) InputFileBytes() (data []byte, mime, fieldName string, ok bool) {
	if s.file == nil {
		return nil, "", "", false
	}
	return s.file.Bytes, s.InputRef.Mime, s.file.FieldName, true
}

// ArtifactRefsJSON returns the input_reference fingerprint as the
// single-element artifact_refs JSON array for the audit row, or "" when no
// file part was present (nothing to record).
func (s *SubmitRequest) ArtifactRefsJSON() string {
	if !s.HasInputRef {
		return ""
	}
	return fmt.Sprintf(`[{"sha256":%q,"sizeBytes":%d,"mime":%q}]`,
		s.InputRef.Sha256, s.InputRef.SizeBytes, s.InputRef.Mime)
}

// InputBytes returns the input_reference bytes for capture, with the mime the
// fingerprint recorded, or nil when no file part was sent.
//
// Handed over rather than copied. The parser has already materialised these
// bytes and ReEmit only reads them, so capture costs a slice assignment on
// the request path instead of a copy proportional to the upload — the same
// reason the STT audio path hands over rather than copies. The caller must
// treat the slice as read-only: capture must never change what is forwarded.
func (r *SubmitRequest) InputBytes() ([]byte, string) {
	if r.file == nil {
		return nil, ""
	}
	return r.file.Bytes, r.InputRef.Mime
}
