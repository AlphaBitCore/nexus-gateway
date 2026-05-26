package traffic

import "github.com/tidwall/gjson"

// CollectExtra walks the top-level keys of `body` (which must be a JSON
// object) and returns those that are NOT in `consumed`. Each map value is
// the raw JSON of the field. Adapters call this to populate
// NormalizedContent.Extra so compliance hooks doing defence-in-depth
// scanning can see fields the adapter did not parse explicitly — the
// safety net against silent data loss when a provider ships a new spec
// field (citations, grounding metadata, reasoning summary, …) before the
// adapter recognises it.
//
// Returns nil when the body is empty, not valid JSON, not an object, or
// produces no unrecognised keys. Consumers treat nil and empty maps
// identically.
func CollectExtra(body []byte, consumed []string) map[string]string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil
	}
	consumedSet := make(map[string]struct{}, len(consumed))
	for _, k := range consumed {
		consumedSet[k] = struct{}{}
	}
	extra := map[string]string{}
	root.ForEach(func(key, value gjson.Result) bool {
		k := key.String()
		if _, ok := consumedSet[k]; ok {
			return true
		}
		extra[k] = value.Raw
		return true
	})
	if len(extra) == 0 {
		return nil
	}
	return extra
}
