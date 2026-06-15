package codecs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strings"
)

// GenericHTTPNormalizer is the catch-all normalizer registered under the
// "*:*:*" key. It handles traffic that no AI adapter claimed (cp/agent
// intercepting plain non-LLM HTTP, ai-gateway audit rows that ended up
// without a routed adapter type, etc.) by projecting the captured bytes
// into one of the http-* NormalizedPayload kinds.
//
// Routing by byte sniff first, then Content-Type:
//
//   - SSE framing (leading `event:` / `data:`) → core.KindHTTPSSE,
//     HTTPBodyView.SSEFrames structured per frame.
//   - NDJSON (two+ independently complete JSON lines) → core.KindHTTPJSON,
//     HTTPBodyView.JSON as an array of the decoded lines.
//   - valid JSON document (first non-ws byte `{` or `[`) → core.KindHTTPJSON,
//     HTTPBodyView.JSON populated — regardless of declared Content-Type.
//   - application/json* → core.KindHTTPJSON, HTTPBodyView.JSON populated.
//   - application/x-www-form-urlencoded → core.KindHTTPForm, HTTPBodyView.Form.
//   - multipart/* → core.KindHTTPMultipart, HTTPBodyView.Form (field-by-field
//     text values; file parts decay to a single placeholder marker so the
//     audit row never inlines blobs).
//   - text/* or empty content-type with parseable UTF-8 → core.KindHTTPText.
//   - everything else → core.KindHTTPBinary, HTTPBodyView.BinaryRef with
//     size + sha256 metadata only (no inline bytes).
//
// The normalizer is deterministic — the same wire bytes produce the same
// payload across services.
type GenericHTTPNormalizer struct {
	// MaxInlineTextBytes is the upper bound on text/JSON projections.
	// Above this, the body is treated as binary and only metadata is
	// stamped. Zero defaults to 1 MiB.
	MaxInlineTextBytes int
}

const defaultMaxInlineTextBytes = 1 << 20 // 1 MiB

// NewGenericHTTPNormalizer returns a normalizer with default limits.
func NewGenericHTTPNormalizer() *GenericHTTPNormalizer {
	return &GenericHTTPNormalizer{MaxInlineTextBytes: defaultMaxInlineTextBytes}
}

// ID is the metric / log label.
func (n *GenericHTTPNormalizer) ID() string { return "generic-http" }

// Normalize routes by content-type, with a byte-level pre-sniff for SSE
// and NDJSON shapes that the audit envelope routinely mis-stamps as
// `application/json`. Direction does not change the shape (HTTP traffic
// is symmetric in this projection — caller decides which side they
// captured).
//
// Audit producers (compliance-proxy, agent) sometimes hand us bodies
// with the request-side Content-Type stamped onto a streaming response
// container, or with no Content-Type at all. Trusting the header alone
// would route an SSE event stream into json.Unmarshal and fail on the
// first `event:` byte; the byte-sniff guards against that.
//
// Provenance: every payload this normalizer emits — including partial
// payloads on decode-error paths — is stamped DetectedSpec="generic-http"
// and Confidence=1.0. The 1.0 means full confidence in the PROJECTION:
// a structural projection (JSON tree / SSE frames / form map / text /
// binary digest) is always a faithful rendering of what it claims to
// be. It makes zero claim about AI semantics — "no AI spec identified"
// is carried by the DetectedSpec value, not by a lowered confidence.
func (n *GenericHTTPNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	payload, err := n.normalize(raw, meta)
	payload.DetectedSpec = "generic-http"
	payload.Confidence = 1.0
	return payload, err
}

// normalize holds the routing switch; Normalize wraps it so the
// provenance stamp covers every return path exactly once.
func (n *GenericHTTPNormalizer) normalize(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	maxText := n.MaxInlineTextBytes
	if maxText <= 0 {
		maxText = defaultMaxInlineTextBytes
	}

	mediaType, params := splitMediaTypeAndParams(meta.ContentType)
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))

	switch {
	case len(raw) == 0:
		// No body. Emit a metadata-only http-text payload so the row
		// still records direction/protocol; the body view is omitted.
		return core.NormalizedPayload{
			Kind:             core.KindHTTPText,
			NormalizeVersion: core.SchemaVersion,
			Protocol:         "generic-http",
			HTTP: &core.HTTPPayload{
				BodyView: &core.HTTPBodyView{},
			},
		}, nil

	case len(raw) > maxText && !looksLikeText(mediaType):
		// Oversize non-text: refuse to inline, emit binary metadata only.
		return n.binaryRef(raw, mediaType), nil

	// Byte-level sniff BEFORE the Content-Type switch: SSE and NDJSON
	// look unmistakable in the first few bytes and frequently arrive
	// with the wrong Content-Type. mediaType=="text/event-stream"
	// would also reach the text/* branch below, but sniffing first
	// keeps the routing decision in one place.
	case looksLikeSSE(raw):
		return n.normalizeSSE(raw)

	case looksLikeNDJSON(raw):
		return n.normalizeNDJSON(raw)

	// JSON byte-sniff, ahead of the declared-Content-Type switch: a
	// body whose first non-ws byte opens a JSON document AND that
	// validates as JSON gets the structured tree regardless of what the
	// producer declared. Capture-side traffic routinely arrives with no
	// Content-Type at all (or a text/* mis-stamp), and trusting the
	// header alone would dump a perfectly parseable JSON body as a flat
	// text blob. Invalid JSON never enters here — it falls to the
	// declared-CT switch, where a declared-JSON body keeps the explicit
	// decode-error path (text or binary projection + surfaced error).
	case looksLikeJSONDocument(raw):
		return n.normalizeJSON(raw)

	// Declared SSE outranks the remaining CT arms: a producer that says
	// text/event-stream gets the frame projection even when the leading
	// bytes defeated the sniff (a comment-heavy preamble longer than the
	// probe window) — landing a declared event stream in the flat text
	// projection would hide its structure from the operator and from
	// content-scanning hooks alike.
	case mediaType == "text/event-stream":
		return n.normalizeSSE(raw)

	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		return n.normalizeJSON(raw)

	case mediaType == "application/x-www-form-urlencoded":
		return n.normalizeForm(raw)

	case strings.HasPrefix(mediaType, "multipart/"):
		boundary := params["boundary"]
		return n.normalizeMultipart(raw, boundary)

	case strings.HasPrefix(mediaType, "text/") || (mediaType == "" && looksLikeUTF8Text(raw)):
		return n.normalizeText(raw)

	default:
		return n.binaryRef(raw, mediaType), nil
	}
}

// splitMediaTypeAndParams strips parameters from a Content-Type value
// and returns the bare media type plus a map of params. Unparseable
// values pass through with no params; callers fall back to the raw
// mediaType in the switch above.
func splitMediaTypeAndParams(ct string) (string, map[string]string) {
	if ct == "" {
		return "", nil
	}
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		// mime.ParseMediaType rejects things like "application/json"
		// followed by trailing whitespace. Best-effort: just split on
		// ";" and trim.
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			return strings.TrimSpace(ct[:i]), nil
		}
		return strings.TrimSpace(ct), nil
	}
	return mt, params
}

func (n *GenericHTTPNormalizer) normalizeJSON(raw []byte) (core.NormalizedPayload, error) {
	out := core.NormalizedPayload{
		Kind:             core.KindHTTPJSON,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "generic-http",
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// The audit envelope said "application/json" but the bytes
		// disagree. Only the declared-CT route can land here: the JSON
		// byte-sniff route (looksLikeJSONDocument) already validated the
		// whole body, and SSE / NDJSON mis-stamps were byte-sniffed away
		// before the Content-Type switch. What remains:
		//
		//   - Plain UTF-8 text — server actually returned HTML, a
		//     stack trace, or other prose under a JSON CT → readable
		//     text projection so the UI never renders "(empty)".
		//   - Genuinely unparseable bytes claiming to be JSON — keep
		//     the original partial behaviour: surface the error,
		//     preserve raw bytes as binary ref so the row is not lossy.
		if looksLikeUTF8Text(raw) {
			return n.normalizeText(raw)
		}
		ref := n.binaryRef(raw, "application/json")
		return ref, fmt.Errorf("generic-http: json decode: %w", err)
	}
	out.HTTP = &core.HTTPPayload{BodyView: &core.HTTPBodyView{JSON: decoded}}
	return out, nil
}

// normalizeNDJSON decodes newline-delimited JSON into a JSON array
// projection. Each non-empty line must independently unmarshal; a
// single failure falls through to a plain-text projection (so we
// don't half-eat a body that happened to share a `{` prefix). The
// array is rendered by the UI's http-json branch.
func (n *GenericHTTPNormalizer) normalizeNDJSON(raw []byte) (core.NormalizedPayload, error) {
	out := core.NormalizedPayload{
		Kind:             core.KindHTTPJSON,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "generic-http",
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	// NDJSON lines can be large (a serialized Gemini chunk easily exceeds
	// the default 64 KiB). Match the limit used by the OpenAI / Anthropic
	// stream decoders so the three paths handle the same upper bound.
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	items := make([]any, 0, 8)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var decoded any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			// One bad line invalidates the NDJSON assumption — drop
			// down to plain text so the operator still sees the body.
			return n.normalizeText(raw)
		}
		items = append(items, decoded)
	}
	if err := scanner.Err(); err != nil {
		return n.normalizeText(raw)
	}
	// items always holds >= 2 entries here: the only route into this
	// function is Normalize's looksLikeNDJSON sniff, which demands two
	// independently complete JSON lines (same scanner limit, same
	// blank-line skipping), and any line failing to re-parse above
	// already fell through to the text projection.
	out.HTTP = &core.HTTPPayload{BodyView: &core.HTTPBodyView{JSON: items}}
	return out, nil
}

func (n *GenericHTTPNormalizer) normalizeForm(raw []byte) (core.NormalizedPayload, error) {
	out := core.NormalizedPayload{
		Kind:             core.KindHTTPForm,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "generic-http",
	}
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		out.HTTP = &core.HTTPPayload{BodyView: &core.HTTPBodyView{Text: string(raw)}}
		return out, fmt.Errorf("generic-http: form decode: %w", err)
	}
	form := make(map[string]string, len(values))
	for k, vs := range values {
		// Multi-valued form fields are collapsed with a newline so the
		// audit reader sees every value while keeping the map shape
		// stable. Single-valued keys pass through unchanged.
		if len(vs) == 1 {
			form[k] = vs[0]
		} else {
			form[k] = strings.Join(vs, "\n")
		}
	}
	out.HTTP = &core.HTTPPayload{BodyView: &core.HTTPBodyView{Form: form}}
	return out, nil
}

func (n *GenericHTTPNormalizer) normalizeMultipart(raw []byte, boundary string) (core.NormalizedPayload, error) {
	out := core.NormalizedPayload{
		Kind:             core.KindHTTPMultipart,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "generic-http",
	}
	if boundary == "" {
		// Content-Type lacked a boundary param — treat as binary, the
		// raw bytes are not structurally parseable.
		ref := n.binaryRef(raw, "multipart/form-data")
		out.Kind = core.KindHTTPMultipart
		out.HTTP = ref.HTTP
		return out, nil
	}
	reader := multipart.NewReader(bytes.NewReader(raw), boundary)
	form := map[string]string{}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Partial parse — keep what we got, surface the error.
			out.HTTP = &core.HTTPPayload{BodyView: &core.HTTPBodyView{Form: form}}
			return out, fmt.Errorf("generic-http: multipart decode: %w", err)
		}
		fieldName := part.FormName()
		if fieldName == "" {
			fieldName = part.FileName()
			if fieldName == "" {
				fieldName = "_part"
			}
		}
		body, err := io.ReadAll(io.LimitReader(part, int64(n.MaxInlineTextBytes)))
		_ = part.Close()
		if err != nil {
			return out, fmt.Errorf("generic-http: multipart part read: %w", err)
		}
		if part.FileName() != "" {
			form[fieldName] = fmt.Sprintf("<file len=%d>", len(body))
			continue
		}
		form[fieldName] = string(body)
	}
	out.HTTP = &core.HTTPPayload{BodyView: &core.HTTPBodyView{Form: form}}
	return out, nil
}

func (n *GenericHTTPNormalizer) normalizeText(raw []byte) (core.NormalizedPayload, error) {
	return core.NormalizedPayload{
		Kind:             core.KindHTTPText,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "generic-http",
		HTTP: &core.HTTPPayload{
			BodyView: &core.HTTPBodyView{Text: string(raw)},
		},
	}, nil
}

func (n *GenericHTTPNormalizer) binaryRef(raw []byte, mediaType string) core.NormalizedPayload {
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	sum := sha256.Sum256(raw)
	return core.NormalizedPayload{
		Kind:             core.KindHTTPBinary,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "generic-http",
		HTTP: &core.HTTPPayload{
			BodyView: &core.HTTPBodyView{
				BinaryRef: &core.BinaryRef{
					Size:        int64(len(raw)),
					ContentType: mediaType,
					SHA256:      hex.EncodeToString(sum[:]),
				},
			},
		},
	}
}

// looksLikeText reports whether the media type is one we are willing to
// inline as a text projection even when its bytes don't pass the UTF-8
// sniff (e.g. text/csv with embedded \r\n is fine).
func looksLikeText(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		return true
	}
	if mediaType == "application/x-www-form-urlencoded" {
		return true
	}
	return false
}

// looksLikeUTF8Text inspects up to the first 512 bytes and reports
// whether they appear to be UTF-8 text (no control bytes other than
// \t \n \r). Used when the producer didn't set a Content-Type so we can
// still differentiate "text" from "binary blob".
func looksLikeUTF8Text(raw []byte) bool {
	probe := raw
	if len(probe) > 512 {
		probe = probe[:512]
	}
	for _, b := range probe {
		switch {
		case b == '\t' || b == '\n' || b == '\r':
			// whitespace OK
		case b < 0x20:
			return false
		}
	}
	return true
}

// looksLikeSSE reports whether the leading lines match a Server-Sent
// Events stream: the first non-whitespace, non-comment line opens with
// `event:` or `data:`. SSE comment lines (leading `:`) are skipped —
// real-world streams open with keep-alive comments like `:ok`
// (stream.wikimedia.org) or `: ping`, and a probe that only looked at
// the first line dumped those streams into the text projection. The
// probe window is 256 bytes so a few comment lines cannot push the
// first frame header out of view.
func looksLikeSSE(raw []byte) bool {
	probe := raw
	if len(probe) > 256 {
		probe = probe[:256]
	}
	s := strings.TrimLeft(string(probe), " \r\n\t")
	for strings.HasPrefix(s, ":") {
		nl := strings.IndexByte(s, '\n')
		if nl < 0 {
			return false
		}
		s = strings.TrimLeft(s[nl+1:], " \r\n\t")
	}
	return strings.HasPrefix(s, "event:") || strings.HasPrefix(s, "data:")
}

// looksLikeJSONDocument reports whether the body IS one complete JSON
// document: the first non-whitespace byte opens an object or array AND
// the whole (trimmed) body validates. The full json.Valid scan — not
// just a prefix probe — is deliberate: this sniff overrides the
// declared Content-Type, so it must never claim a body that would then
// fail the JSON decode (an HTML error page starting with a brace, a
// truncated capture). The scan is a single O(n) pass over bytes the
// JSON path would parse anyway.
func looksLikeJSONDocument(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	return json.Valid(trimmed)
}

// looksLikeNDJSON reports whether the body is plausibly newline-
// delimited JSON: at least two non-empty lines, the first one
// starts with `{` or `[`, and the whole body does NOT start with
// `[` followed by a newline (which would be a real JSON array
// printed with one element per line). Conservative on purpose —
// we'd rather route a real JSON document through the JSON path
// and have it render correctly than mis-classify it as NDJSON.
func looksLikeNDJSON(raw []byte) bool {
	trimmed := bytes.TrimLeft(raw, " \r\n\t")
	if len(trimmed) < 4 {
		return false
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return false
	}
	// A real JSON array spans multiple lines but its outer shape is
	// `[ ... ]` — when we scan the bytes and find the array bracket
	// closing before EOF, treat it as JSON, not NDJSON. The cheapest
	// disambiguation: count the first line; if it ends with `,` or
	// is itself parse-incomplete (an open `{` with no closing `}`
	// before the newline), it's a JSON array printed multi-line, not
	// NDJSON. NDJSON lines are individually complete documents.
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	completeLines := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var probe any
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			return false
		}
		completeLines++
		if completeLines >= 2 {
			return true
		}
	}
	return false
}
