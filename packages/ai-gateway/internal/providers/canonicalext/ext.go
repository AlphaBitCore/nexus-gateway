// Package canonicalext exposes the nexus.ext.<provider>.<key> passthrough
// helpers shared by every per-provider SchemaCodec. Lives below the codecs to
// avoid an import cycle with canonicalbridge.
package canonicalext

import (
	"bytes"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Quoted so the pre-check is not tripped by message text containing the word.
var rootKey = []byte(`"nexus"`)

// Strip removes the whole nexus namespace from body. Both directions need it:
// Cohere answered 422 when the namespace reached it, and the OpenAI-wire egress
// is the identity, so anything the decode leaves behind reaches the client.
//
// Only the ROOT key is removed. A `nexus`-named property inside a caller's tool
// schema or json_schema belongs to the caller — deleting it produced an invalid
// schema and a model answering about a contract it never received, with HTTP 200
// on every byte-level assertion.
//
// A DUPLICATE root key is deliberately NOT handled: sjson removes only one, the
// gateway cannot emit one on the response side, and RFC 8259 leaves
// duplicate-name behaviour undefined anyway.
func Strip(body []byte) []byte {
	if len(body) == 0 || !hasRootKey(body) {
		return body
	}
	// sjson reports success on a malformed document and returns nonsense —
	// `{"nexus":{"ext":{"a":1}}` (one brace short) came back as `{`. The caller
	// would then get an upstream verdict on our truncation rather than on what
	// they sent. Checked only here, on the rare rewrite path.
	if !gjson.ValidBytes(body) {
		return body
	}
	// No error branch: sjson fails a delete only on an unparseable PATH, and
	// this path is the constant "nexus".
	out, _ := sjson.DeleteBytes(body, "nexus")
	return out
}

// hasRootKey reports whether body may carry the namespace root under ANY
// spelling JSON permits. A literal scan alone is not enough:
// `{"nexus":{"ext":{"anthropic":{"topK":42}}}}` is HONOURED by [Get],
// because gjson unescapes keys while matching, and was not removed by [Strip].
//
// Three layers, cheapest first: the literal token (what [Set] writes); then no
// `\u` anywhere, since \uXXXX is the only JSON escape producing an arbitrary
// character; then gjson, which resolves escapes when matching a key.
func hasRootKey(body []byte) bool {
	if bytes.Contains(body, rootKey) {
		return true
	}
	if !bytes.Contains(body, unicodeEscape) {
		return false
	}
	return gjson.GetBytes(body, "nexus").Exists()
}

// The only JSON escape that can hide a key from a literal scan.
var unicodeEscape = []byte(`\u`)

// Get returns the unparsed value at nexus.ext.<provider>.<key>, or an empty
// [gjson.Result] when absent.
func Get(body []byte, provider, key string) gjson.Result {
	return gjson.GetBytes(body, "nexus.ext."+provider+"."+key)
}

// Set writes value under nexus.ext.<provider>.<key>; value must be
// JSON-marshalable per sjson rules.
func Set(body []byte, provider, key string, value any) ([]byte, error) {
	return sjson.SetBytes(body, "nexus.ext."+provider+"."+key, value)
}
