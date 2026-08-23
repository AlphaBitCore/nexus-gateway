package proxy

import "testing"

// The defect this pins: /v1/audio/speech relays provider MP3 bytes, and the
// non-streaming path announced application/json over them. Measured on
// production before the fix — `file` reported "MPEG ADTS, layer III, 128 kbps"
// for a body served as application/json.
func TestRelayedContentType_AudioRelayKeepsItsOwnType(t *testing.T) {
	for _, upstream := range []string{
		"audio/mpeg",
		"audio/wav",
		"audio/mp4",
		"application/octet-stream",
		"image/png",
	} {
		if got := relayedContentType(upstream); got != upstream {
			t.Errorf("relayedContentType(%q) = %q; a relayed body must keep its own type", upstream, got)
		}
	}
}

// The hot path must not move. Chat, embeddings and rerank all produce JSON
// after the canonical bridge, and those responses have to stay byte-identical
// to what the gateway has always sent.
func TestRelayedContentType_JsonUpstreamsStillGetTheExactLiteral(t *testing.T) {
	for _, upstream := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"Application/JSON",
		"  application/json  ",
		"application/vnd.provider+json",
		"", // upstream sent no Content-Type at all
	} {
		if got := relayedContentType(upstream); got != "application/json" {
			t.Errorf("relayedContentType(%q) = %q; want the unchanged application/json literal", upstream, got)
		}
	}
}

func TestIsJSONContentType(t *testing.T) {
	json := []string{
		"application/json",
		"application/json; charset=utf-8",
		"APPLICATION/JSON",
		"application/problem+json",
		"application/vnd.api+json; charset=utf-8",
	}
	for _, ct := range json {
		if !isJSONContentType(ct) {
			t.Errorf("isJSONContentType(%q) = false; want true", ct)
		}
	}
	notJSON := []string{
		"audio/mpeg",
		"application/octet-stream",
		"text/plain",
		"",
		// A type that merely mentions json without the structured suffix is
		// not JSON — "+json" is the RFC 6839 convention, "jsonish" is not.
		"application/jsonish",
		"text/json-ld",
	}
	for _, ct := range notJSON {
		if isJSONContentType(ct) {
			t.Errorf("isJSONContentType(%q) = true; want false", ct)
		}
	}
}
