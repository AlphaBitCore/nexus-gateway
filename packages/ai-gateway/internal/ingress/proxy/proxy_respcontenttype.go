package proxy

import "strings"

// relayedContentType decides what a non-streaming response tells the client
// and the audit record its body is.
//
// Answering "application/json" unconditionally assumes everything is
// JSON-shaped after the canonical bridge. That holds for chat, embeddings,
// rerank and the rest of the JSON-producing endpoints, and it is false for
// tts: /v1/audio/speech relays the provider's audio bytes verbatim, so the
// body is an MP3 while the header announces JSON. Clients dispatch on that
// header — an SDK writing the response to a file, a browser <audio> element —
// so the lie reaches the caller on every TTS response. The same constant
// stamped onto the audit record is why such a stored body cannot be
// normalized: the reader is told to parse MPEG frames as JSON.
//
// The upstream header is already read on this path to mime the TTS artifact
// ref, so the truth is in hand either way.
//
// A JSON (or absent) upstream keeps the exact literal, so chat / embeddings /
// rerank responses are byte-identical and the hot path is untouched. Only a
// non-JSON upstream — audio today, whatever binary endpoint lands next —
// differs, and it differs toward the truth.
func relayedContentType(upstream string) string {
	if upstream == "" || isJSONContentType(upstream) {
		return "application/json"
	}
	return upstream
}

// isJSONContentType reports whether a Content-Type names a JSON body.
// Parameters are ignored (`application/json; charset=utf-8` is JSON), and the
// +json structured-suffix convention counts, so a provider answering
// `application/vnd.foo+json` is not mistaken for a binary relay.
func isJSONContentType(ct string) bool {
	mime := ct
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	return mime == "application/json" || strings.HasSuffix(mime, "+json")
}
