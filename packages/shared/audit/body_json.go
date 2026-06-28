// Package audit: body_json.go — the wire (NATS message) JSON codec for Body.
// MarshalJSON / UnmarshalJSON make non-JSON inline bytes round-trip losslessly;
// the persisted-column (BYTEA) codec lives in body.go alongside the constructors.
package audit

import (
	"bytes"
	"encoding/base64"
	stdjson "encoding/json"
	"errors"
	"fmt"

	"github.com/goccy/go-json"
)

// MarshalJSON implements custom serialization so non-JSON inline bytes ride the
// wire losslessly. The shape is:
//
//	{"kind":"absent"}
//	{"kind":"inline","encoding":"raw","inlineBytes":<json>, ...}
//	{"kind":"inline","encoding":"text","inlineBytes":"<escaped utf-8>", ...}
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
		// Splice path: emit the tiny marker as inlineBytes (the producer splices the
		// real bytes back post-encode). The marker is a valid JSON string, so the
		// envelope stays valid JSON; the true Encoding above tells the consumer how
		// to read the spliced-in bytes. Skips the body-sized escape/base64 re-encode.
		if b.spliceMarker != nil {
			envelope.InlineBytes = json.RawMessage(b.spliceMarker)
			return json.Marshal(envelope)
		}
		switch b.Encoding {
		case EncodingZstd:
			// Compress the original bytes and emit base64 of the zstd frame as a JSON
			// string. This runs on the async marshal worker, not the request path.
			b64 := compressInlineToBase64(nil, b.InlineBytes)
			quoted, _ := json.Marshal(string(b64))
			envelope.InlineBytes = quoted
		case EncodingS2:
			// Same shape as EncodingZstd but the frame is S2 (far cheaper encode).
			b64 := compressInlineS2ToBase64(nil, b.InlineBytes)
			quoted, _ := json.Marshal(string(b64))
			envelope.InlineBytes = quoted
		case EncodingRaw:
			// rawValidated bodies came through NewInlineBody, which already ran
			// json.Valid to pick encoding=raw — trust that and skip the O(n)
			// re-scan (the dominant audit-marshal CPU cost). Only direct struct
			// literals (no constructor) re-validate here, keeping the contract.
			if !b.rawValidated && !stdjson.Valid(b.InlineBytes) {
				return nil, fmt.Errorf("audit.Body: inline encoding=raw but bytes are not valid JSON")
			}
			envelope.InlineBytes = json.RawMessage(b.InlineBytes)
		case EncodingText:
			// Valid UTF-8 but not JSON (SSE, plain text): emit as an escaped JSON
			// string. The envelope stays valid JSON and the wire is ~14% smaller
			// + lower-alloc than base64 (benchmarked on a real SSE corpus).
			quoted, err := json.Marshal(string(b.InlineBytes))
			if err != nil {
				return nil, fmt.Errorf("audit.Body: text encode: %w", err)
			}
			envelope.InlineBytes = quoted
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
		case EncodingZstd:
			// Keep the base64-of-zstd payload VERBATIM — the Hub persists it into
			// the column unchanged (no decompress on ingest); only the view layer
			// decompresses. probe.InlineBytes is a JSON string token of base64; recover
			// the raw base64 bytes by stripping the quotes WITHOUT the per-byte unescape
			// json.Unmarshal does — base64 can never contain a JSON escape, so that scan
			// over the ~18 KB body was pure Hub-ingest CPU waste (top goccy decodeByte).
			s, err := unquoteInlineString(probe.InlineBytes)
			if err != nil {
				return fmt.Errorf("audit.Body: inline zstd payload is not a JSON string: %w", err)
			}
			b.InlineBytes = s
		case EncodingS2:
			// Same verbatim handling as zstd — Hub copies base64-of-s2 into the
			// column unchanged; the view layer s2-decompresses on read.
			s, err := unquoteInlineString(probe.InlineBytes)
			if err != nil {
				return fmt.Errorf("audit.Body: inline s2 payload is not a JSON string: %w", err)
			}
			b.InlineBytes = s
		case EncodingRaw:
			b.InlineBytes = []byte(probe.InlineBytes)
		case EncodingText:
			// Text may carry genuine JSON escapes (SSE/plain text), so the full
			// unescape is required here.
			var s string
			if err := json.Unmarshal(probe.InlineBytes, &s); err != nil {
				return fmt.Errorf("audit.Body: inline text payload is not a JSON string: %w", err)
			}
			b.InlineBytes = []byte(s)
		case EncodingBase64, "":
			// base64 alphabet → no JSON escapes; unquote then decode (skip unescape).
			s, err := unquoteInlineString(probe.InlineBytes)
			if err != nil {
				return fmt.Errorf("audit.Body: inline base64 payload is not a JSON string: %w", err)
			}
			raw, err := base64.StdEncoding.DecodeString(string(s))
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

// unquoteInlineString returns the inner bytes of a JSON string token whose content
// is known to be escape-free — the base64 / compressed-frame inline wire forms
// (base64 alphabet, or a base64-of-frame) can never contain a JSON escape. It strips
// the surrounding quotes and copies the inner bytes, skipping the per-byte unescape
// scan json.Unmarshal runs. On the Hub ingest path that unescape over an ~18 KB
// base64 body was a top decode cost (goccy decodeByte) for zero benefit. If the
// token is not a plain quoted string, or (defensively) carries a backslash escape,
// it falls back to the full json.Unmarshal so correctness is never traded away.
func unquoteInlineString(raw []byte) ([]byte, error) {
	t := bytes.TrimSpace(raw)
	if len(t) >= 2 && t[0] == '"' && t[len(t)-1] == '"' {
		inner := t[1 : len(t)-1]
		if bytes.IndexByte(inner, '\\') < 0 {
			// Copy off the message buffer so the body outlives the decode frame.
			return append([]byte(nil), inner...), nil
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return []byte(s), nil
}
