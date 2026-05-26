package canonicalbridge

import "github.com/tidwall/gjson"

// hasJSONField returns true when the JSON object at body has key `key`
// at the top level. Used by [Bridge.IngressEmbeddingsToCanonical] to
// detect Gemini single (`content`) vs batch (`requests`) shape from the
// body alone — neither URL nor header context is available at the
// canonical-bridge layer.
func hasJSONField(body []byte, key string) bool {
	return gjson.GetBytes(body, key).Exists()
}

// embedDataLen returns the length of `data` in a canonical
// /v1/embeddings response, or 0 when the field is missing or not an
// array. Used as the cardinality fallback in
// [Bridge.ResponseCanonicalToIngressEmbeddings].
func embedDataLen(canonical []byte) int {
	v := gjson.GetBytes(canonical, "data")
	if !v.IsArray() {
		return 0
	}
	return int(v.Get("#").Int())
}
