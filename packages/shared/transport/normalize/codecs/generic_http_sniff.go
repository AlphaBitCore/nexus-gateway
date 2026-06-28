package codecs

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
)

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
// the whole (trimmed) body validates. The full validity scan — not
// just a prefix probe — is deliberate: this sniff overrides the
// declared Content-Type, so it must never claim a body that would then
// fail the JSON decode (an HTML error page starting with a brace, a
// truncated capture). The scan is a single O(n) pass over bytes the
// JSON path would parse anyway.
//
// Uses stdlib encoding/json.Valid (a zero-alloc stack scanner) instead of
// goccy's json.Valid (which decodes into interface{}, ~4x body alloc — 22 KB/op
// on a typical chat body). stdlib is STRICTER than the goccy decoder this sniff
// feeds, so it can only ever UNDER-claim (a few RFC-malformed bodies goccy would
// leniently decode now route to the text path, preserving raw bytes) — never
// over-claim, which is the invariant above. See TestValid_SniffSafetyInvariant.
func looksLikeJSONDocument(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	return stdjson.Valid(trimmed)
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
	// NDJSON is >=2 newline-separated complete JSON docs. A body with no
	// newline at all cannot be NDJSON — bail before any validation so the
	// common single-document case (e.g. one large chat-completion body) pays
	// nothing. This sniff runs on every request through the normalize path
	// (routing + audit), shared by all services, so it must not allocate:
	// scan line spans in-place over the byte slice and validate with
	// stdjson.Valid (a zero-alloc stack scanner; goccy's json.Valid decodes
	// into interface{}, ~4x body alloc) instead of bufio.Scanner (per-call
	// 64 KiB buffer) + json.Unmarshal-into-any (full parse tree).
	if bytes.IndexByte(trimmed, '\n') < 0 {
		return false
	}
	// ndjsonMaxLine mirrors the 8 MiB bufio token cap used by the decoder
	// (normalizeNDJSON): an oversized line stops the sniff rather than
	// validating a multi-MB span, so classification stays cheap and matches
	// the decoder's degrade-to-text behavior.
	const ndjsonMaxLine = 8 * 1024 * 1024
	completeLines := 0
	for rest := trimmed; len(rest) > 0; {
		var line []byte
		if nl := bytes.IndexByte(rest, '\n'); nl < 0 {
			line, rest = rest, nil
		} else {
			line, rest = rest[:nl], rest[nl+1:]
		}
		if len(line) > ndjsonMaxLine {
			break
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !stdjson.Valid(line) {
			return false
		}
		completeLines++
		if completeLines >= 2 {
			return true
		}
	}
	return false
}
