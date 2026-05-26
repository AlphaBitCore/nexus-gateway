// Package audit: body.go — wire-format-safe container for captured request /
// response bodies on traffic_event_payload.
//
// The Hub-bound audit message used to type the body field as
// `json.RawMessage`. That worked for ai-gateway's JSON request/response shape
// but exploded on compliance-proxy and agent SSE traffic, multipart uploads,
// and any byte sequence that wasn't already valid JSON: `json.Marshal` of the
// envelope would call `RawMessage.MarshalJSON`, which validates the bytes and
// errors out the moment it saw a `\x1b` (ANSI escape), an unescaped CR, or
// any other non-JSON byte. The MQ writer's `continue`-on-error path then
// silently dropped the entire audit row.
//
// `Body` makes the captured payload a structured discriminator — `Kind`
// distinguishes "absent" (capture disabled or zero-length), "inline" (body
// fits within the inline JSONB threshold and travels with the message), and
// "spill" (body was written to `shared/spillstore` and the message carries a
// reference). Inline bytes are base64-encoded on the wire when the bytes are
// not themselves valid JSON, so any byte sequence round-trips losslessly.
package audit

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// BodyKind discriminates the storage form of a captured body.
type BodyKind string

const (
	BodyAbsent BodyKind = "absent"
	BodyInline BodyKind = "inline"
	BodySpill  BodyKind = "spill"
)

// BodyEncoding records how `InlineBytes` is laid out on the wire.
//   - "raw"    — bytes are valid UTF-8 JSON and embedded as a JSON value.
//   - "base64" — bytes are not valid JSON; base64-encoded for transport.
type BodyEncoding string

const (
	EncodingRaw    BodyEncoding = "raw"
	EncodingBase64 BodyEncoding = "base64"
)

// SpillRef points to a body that was stored out-of-band (large payloads).
// Backend / Key tuples are opaque to the audit pipeline; resolution happens
// via `shared/spillstore.SpillStore.Get`.
//
// Truncated is set when the backend hit its per-object cap before exhausting
// the upstream reader; the audit pipeline then knows the persisted bytes
// are a prefix of the original payload, not the whole thing. The outer
// Body still carries its own Truncated for inline payloads — the SpillRef
// flag covers the spill-backend-specific case.
type SpillRef struct {
	Backend     string `json:"backend"`               // "localfs" | "s3" | …
	Key         string `json:"key"`                   // backend-specific key
	Size        int64  `json:"size"`                  // bytes
	SHA256      string `json:"sha256,omitempty"`      // hex-encoded
	ContentType string `json:"contentType,omitempty"` // hint for renderers
	Truncated   bool   `json:"truncated,omitempty"`
}

// Body is the discriminated container persisted on `traffic_event_payload`.
// Producers call `NewInlineBody` / `NewSpillBody` / `EmptyBody`; consumers
// read `Kind` and dispatch on it.
type Body struct {
	Kind        BodyKind     `json:"kind"`
	Encoding    BodyEncoding `json:"encoding,omitempty"` // only meaningful for Inline
	InlineBytes []byte       `json:"-"`                  // not serialized directly — see MarshalJSON
	SpillRef    *SpillRef    `json:"spillRef,omitempty"`
	SizeBytes   int64        `json:"sizeBytes,omitempty"` // pre-truncation size
	Truncated   bool         `json:"truncated,omitempty"`
	ContentType string       `json:"contentType,omitempty"`
}

// EmptyBody returns the zero-value body that means "no payload captured".
func EmptyBody() Body {
	return Body{Kind: BodyAbsent}
}

// NewInlineBody returns a body whose bytes travel with the audit message.
// The encoding is auto-detected: if `b` is a valid JSON document (object,
// array, string, number, bool, null), the wire form is "raw"; otherwise it
// is base64. `contentType` is a free-form hint stored on the row for UI
// rendering ("application/json", "text/event-stream", "multipart/form-data",
// "application/octet-stream", …).
func NewInlineBody(b []byte, sizeBytes int64, truncated bool, contentType string) Body {
	if len(b) == 0 {
		return EmptyBody()
	}
	enc := EncodingRaw
	if !json.Valid(b) {
		enc = EncodingBase64
	}
	return Body{
		Kind:        BodyInline,
		Encoding:    enc,
		InlineBytes: b,
		SizeBytes:   sizeBytes,
		Truncated:   truncated,
		ContentType: contentType,
	}
}

// NewSpillBody returns a body that lives in a `SpillStore` backend.
// `originalSize` is the pre-spill size of the captured stream (always the
// full size — there is no truncation in the spill path; oversized streams
// are capped at the per-backend hard limit and `Truncated` reflects that).
func NewSpillBody(ref *SpillRef, originalSize int64, truncated bool, contentType string) Body {
	if ref == nil {
		return EmptyBody()
	}
	return Body{
		Kind:        BodySpill,
		SpillRef:    ref,
		SizeBytes:   originalSize,
		Truncated:   truncated,
		ContentType: contentType,
	}
}

// MarshalJSON implements custom serialization so non-JSON inline bytes are
// base64-encoded on the wire. The shape is:
//
//	{"kind":"absent"}
//	{"kind":"inline","encoding":"raw","inlineBytes":<json>, ...}
//	{"kind":"inline","encoding":"base64","inlineBytes":"<base64>", ...}
//	{"kind":"spill","spillRef":{...}, ...}
func (b Body) MarshalJSON() ([]byte, error) {
	switch b.Kind {
	case BodyAbsent, "":
		return json.Marshal(struct {
			Kind BodyKind `json:"kind"`
		}{Kind: BodyAbsent})

	case BodyInline:
		envelope := struct {
			Kind        BodyKind        `json:"kind"`
			Encoding    BodyEncoding    `json:"encoding"`
			InlineBytes json.RawMessage `json:"inlineBytes"`
			SizeBytes   int64           `json:"sizeBytes,omitempty"`
			Truncated   bool            `json:"truncated,omitempty"`
			ContentType string          `json:"contentType,omitempty"`
		}{
			Kind:        BodyInline,
			Encoding:    b.Encoding,
			SizeBytes:   b.SizeBytes,
			Truncated:   b.Truncated,
			ContentType: b.ContentType,
		}
		switch b.Encoding {
		case EncodingRaw:
			if !json.Valid(b.InlineBytes) {
				return nil, fmt.Errorf("audit.Body: inline encoding=raw but bytes are not valid JSON")
			}
			envelope.InlineBytes = json.RawMessage(b.InlineBytes)
		case EncodingBase64, "":
			s := base64.StdEncoding.EncodeToString(b.InlineBytes)
			quoted, _ := json.Marshal(s)
			envelope.InlineBytes = quoted
			if envelope.Encoding == "" {
				envelope.Encoding = EncodingBase64
			}
		default:
			return nil, fmt.Errorf("audit.Body: unknown encoding %q", b.Encoding)
		}
		return json.Marshal(envelope)

	case BodySpill:
		if b.SpillRef == nil {
			return nil, errors.New("audit.Body: kind=spill but SpillRef is nil")
		}
		return json.Marshal(struct {
			Kind        BodyKind  `json:"kind"`
			SpillRef    *SpillRef `json:"spillRef"`
			SizeBytes   int64     `json:"sizeBytes,omitempty"`
			Truncated   bool      `json:"truncated,omitempty"`
			ContentType string    `json:"contentType,omitempty"`
		}{
			Kind:        BodySpill,
			SpillRef:    b.SpillRef,
			SizeBytes:   b.SizeBytes,
			Truncated:   b.Truncated,
			ContentType: b.ContentType,
		})

	default:
		return nil, fmt.Errorf("audit.Body: unknown kind %q", b.Kind)
	}
}

// UnmarshalJSON inverts MarshalJSON. Inline bytes recover their original
// form regardless of which encoding produced the wire copy.
func (b *Body) UnmarshalJSON(data []byte) error {
	probe := struct {
		Kind        BodyKind        `json:"kind"`
		Encoding    BodyEncoding    `json:"encoding"`
		InlineBytes json.RawMessage `json:"inlineBytes"`
		SpillRef    *SpillRef       `json:"spillRef"`
		SizeBytes   int64           `json:"sizeBytes"`
		Truncated   bool            `json:"truncated"`
		ContentType string          `json:"contentType"`
	}{}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	b.Kind = probe.Kind
	b.SizeBytes = probe.SizeBytes
	b.Truncated = probe.Truncated
	b.ContentType = probe.ContentType
	switch probe.Kind {
	case BodyAbsent, "":
		*b = EmptyBody()
		return nil
	case BodyInline:
		b.Encoding = probe.Encoding
		switch probe.Encoding {
		case EncodingRaw:
			b.InlineBytes = []byte(probe.InlineBytes)
		case EncodingBase64, "":
			var s string
			if err := json.Unmarshal(probe.InlineBytes, &s); err != nil {
				return fmt.Errorf("audit.Body: inline base64 payload is not a JSON string: %w", err)
			}
			raw, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return fmt.Errorf("audit.Body: inline base64 decode: %w", err)
			}
			b.InlineBytes = raw
		default:
			return fmt.Errorf("audit.Body: unknown encoding %q", probe.Encoding)
		}
		return nil
	case BodySpill:
		if probe.SpillRef == nil {
			return errors.New("audit.Body: kind=spill but spillRef missing")
		}
		b.SpillRef = probe.SpillRef
		return nil
	default:
		return fmt.Errorf("audit.Body: unknown kind %q", probe.Kind)
	}
}

// SHA256Hex returns the lowercase hex-encoded SHA-256 of `b`. Used by spill
// callers to populate `SpillRef.SHA256` deterministically before Put.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
