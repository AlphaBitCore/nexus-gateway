package geminicache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const redisKeyPrefix = "gemini:cc:"

// contentHash returns the Redis key for a (providerID, model, systemJSON) triple.
//
// systemJSON is normalized through encoding/json before hashing so that
// logically identical systemInstruction values (differing only in
// whitespace or key ordering) produce the same Redis key. Without
// normalization, two ingresses that both target Gemini — one via the
// canonical bridge (compact JSON, `parts` before `role`) and one via
// `/v1beta` native passthrough (pretty JSON, `role` before `parts`) —
// hash to different keys and never reuse each other's cachedContent.
// That asymmetry shows up in smoke as: /v1/chat/completions and
// /v1/responses hit the Gemini prompt cache, but /v1beta misses.
//
// Algorithm:
//
//	canonical = json.Marshal(json.Unmarshal(systemJSON))   // best-effort
//	hash_input = providerID + "|" + model + "|" + canonical
//	redis_key  = "gemini:cc:" + hex(sha256(hash_input))
func contentHash(providerID, model, systemJSON string) string {
	canonical := canonicalizeJSON(systemJSON)
	h := sha256.Sum256([]byte(providerID + "|" + model + "|" + canonical))
	return redisKeyPrefix + hex.EncodeToString(h[:])
}

// canonicalizeJSON re-serializes raw JSON through encoding/json so the
// output drops whitespace and sorts map keys alphabetically (Go's
// json.Marshal behaviour for map[string]any). If parsing fails — e.g.
// the input isn't valid JSON — fall back to the original string so the
// hash stays stable for that malformed case rather than collapsing
// every parse error to one key.
func canonicalizeJSON(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(out)
}
