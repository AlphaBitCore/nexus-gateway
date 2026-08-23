// cachedcontent_request.go — the request-side wire shape of Gemini's
// cachedContent feature. These are the pure canonical/wire operations that know
// Gemini's request-body structure: which fields participate in a cachedContent
// and how a body is rewritten to reference one. The stateful lifecycle of a
// cachedContent — creating it via the Gemini REST API, storing the reference in
// Redis, TTLs, invalidation — lives in the cache manager, which calls these.
// Keeping the wire-shape here (next to the response-side cachedContentTokenCount
// handling in codec.go) makes the codec the single source of Gemini wire truth.
package codec

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// geminiCacheableFields are the request-body fields Gemini forbids alongside a
// cachedContent reference — they must live INSIDE the cached object, so a
// request that references a cachedContent may set none of them. This one list is
// the source of truth for both directions: reading them out to fold into the
// cache (ExtractCacheableFields) and stripping them when the reference is
// injected (InjectCachedContentRef). Without stripping them Gemini returns 400
// "CachedContent can not be used with GenerateContent request setting
// system_instruction, tools or tool_config" for every tool-calling request.
var geminiCacheableFields = []string{"systemInstruction", "tools", "toolConfig"}

// ExtractCacheableFields returns the raw JSON of the Gemini request-body fields
// that participate in a cachedContent — systemInstruction, tools, toolConfig —
// each empty when absent or empty. The cachedContent manager uses these to key
// the cache and to build the create payload; centralising the field names here
// keeps every Gemini wire-shape decision in the codec.
func ExtractCacheableFields(body []byte) (systemInstruction, tools, toolConfig string) {
	return rawGeminiField(body, "systemInstruction"),
		rawGeminiField(body, "tools"),
		rawGeminiField(body, "toolConfig")
}

func rawGeminiField(body []byte, path string) string {
	if r := gjson.GetBytes(body, path); r.Exists() && r.Raw != "" {
		return r.Raw
	}
	return ""
}

// InjectCachedContentRef rewrites a Gemini request body to reference a
// cachedContent object: it strips the fields Gemini forbids alongside the
// reference (systemInstruction, tools, toolConfig — all folded into the cached
// object at create time and part of the cache key, so a hit guarantees the
// cached copies match this request) and sets cachedContent. Deleting an absent
// field is a no-op. This is the pure wire-shape half of the feature; the
// stateful lifecycle lives in the cache manager, which calls this.
func InjectCachedContentRef(body []byte, cachedContentName string) ([]byte, error) {
	out := body
	for _, field := range geminiCacheableFields {
		var err error
		out, err = sjson.DeleteBytes(out, field)
		if err != nil {
			return nil, fmt.Errorf("delete %s: %w", field, err)
		}
	}
	out, err := sjson.SetBytes(out, "cachedContent", cachedContentName)
	if err != nil {
		return nil, fmt.Errorf("set cachedContent: %w", err)
	}
	return out, nil
}
