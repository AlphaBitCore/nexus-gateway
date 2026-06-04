package geminicache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const redisKeyPrefix = "gemini:cc:"

// contentHash returns the Redis key for a cached content object keyed on the
// (providerID, model, systemInstruction, tools, toolConfig) tuple.
//
// systemJSON (and, when present, toolsJSON / toolConfigJSON) are normalized
// through encoding/json before hashing so that logically identical values
// (differing only in whitespace or key ordering) produce the same Redis key.
// Without normalization, two ingresses that both target Gemini — one via the
// canonical bridge (compact JSON, `parts` before `role`) and one via
// `/v1beta` native passthrough (pretty JSON, `role` before `parts`) —
// hash to different keys and never reuse each other's cachedContent.
// That asymmetry shows up in smoke as: /v1/chat/completions and
// /v1/responses hit the Gemini prompt cache, but /v1beta misses.
//
// tools / toolConfig are part of the key because Gemini folds them INTO the
// cachedContent (a request that references a cachedContent may not also set
// systemInstruction / tools / toolConfig). Two requests that share a system
// prompt but carry different tool sets must therefore map to different cache
// objects. When both are empty the hash input is byte-identical to the
// system-only form, so existing no-tool cache entries keep hitting unchanged.
//
// Algorithm:
//
//	input = providerID + "|" + model + "|" + canonical(system)
//	        [+ "|tools|"   + canonical(tools)]       // only when non-empty
//	        [+ "|toolcfg|" + canonical(toolConfig)]  // only when non-empty
//	redis_key = "gemini:cc:" + hex(sha256(input))
func contentHash(providerID, model, systemJSON, toolsJSON, toolConfigJSON string) string {
	input := providerID + "|" + model + "|" + canonicalizeJSON(systemJSON)
	if toolsJSON != "" {
		input += "|tools|" + canonicalizeJSON(toolsJSON)
	}
	if toolConfigJSON != "" {
		input += "|toolcfg|" + canonicalizeJSON(toolConfigJSON)
	}
	h := sha256.Sum256([]byte(input))
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
