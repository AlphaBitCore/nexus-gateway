package codecs

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
// Routing by Content-Type:
//
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
func (n *GenericHTTPNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
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
		// disagree. Try the shapes that real-world producers most
		// often mis-stamp as JSON before giving up:
		//
		//   1. SSE — Content-Type forwarded from the request side
		//      onto a streaming response container (chatgpt-web,
		//      claude-web). Tested above in Normalize() but a
		//      future caller might invoke normalizeJSON directly.
		//   2. NDJSON — one JSON object per line, common for
		//      bulk-export endpoints and some Gemini streaming
		//      formats.
		//   3. Plain UTF-8 text — server actually returned HTML,
		//      a stack trace, or other prose under a JSON CT.
		//
		// Each fallback produces a coherent Kind/BodyView pair so
		// the UI's per-kind renderer can show readable content;
		// the previous code left Kind=http-json with only Text
		// populated, which the UI rendered as "(empty)".
		if looksLikeSSE(raw) {
			return n.normalizeSSE(raw)
		}
		if looksLikeNDJSON(raw) {
			return n.normalizeNDJSON(raw)
		}
		if looksLikeUTF8Text(raw) {
			return n.normalizeText(raw)
		}
		// Genuinely unparseable bytes claiming to be JSON — keep
		// the original partial behaviour: surface the error,
		// preserve raw bytes as binary ref so the row is not lossy.
		ref := n.binaryRef(raw, "application/json")
		return ref, fmt.Errorf("generic-http: json decode: %w", err)
	}
	out.HTTP = &core.HTTPPayload{BodyView: &core.HTTPBodyView{JSON: decoded}}
	return out, nil
}

// normalizeSSE projects a Server-Sent Events stream into a verbatim
// text view. Per [[feedback_compliance_proxy_text_first]] consumer-LLM
// surfaces (chatgpt.com, claude.ai web, cursor) lack a stable wire
// schema, so we deliberately do NOT extract assistant text / tool
// calls / usage from the event stream here — that's the AI-protocol
// normalizers' job (openai_chat.go, anthropic_messages.go, …). For
// the generic case the raw SSE dump is the most-readable, lowest-
// brittleness projection: the operator scrolls the events and sees
// what the user said + what the model replied, no schema gymnastics.
func (n *GenericHTTPNormalizer) normalizeSSE(raw []byte) (core.NormalizedPayload, error) {
	return core.NormalizedPayload{
		Kind:             core.KindHTTPText,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "generic-http",
		HTTP: &core.HTTPPayload{
			BodyView: &core.HTTPBodyView{Text: string(raw)},
		},
	}, nil
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
	if len(items) < 2 {
		// Single-line valid JSON is not NDJSON — it's just JSON.
		// Re-route through the JSON path so the body is presented
		// as a single decoded object instead of a one-element array.
		if len(items) == 1 {
			out.HTTP = &core.HTTPPayload{BodyView: &core.HTTPBodyView{JSON: items[0]}}
			return out, nil
		}
		return n.normalizeText(raw)
	}
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

// looksLikeSSE reports whether the first non-whitespace bytes match a
// Server-Sent Events frame header (`event:` or `data:`). 64 bytes is
// plenty — every real-world SSE stream begins with one of those two
// tokens on the first line. Whitespace tolerance covers producers
// that emit a leading blank line before the first event.
func looksLikeSSE(raw []byte) bool {
	probe := raw
	if len(probe) > 64 {
		probe = probe[:64]
	}
	s := strings.TrimLeft(string(probe), " \r\n\t")
	return strings.HasPrefix(s, "event:") || strings.HasPrefix(s, "data:")
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
