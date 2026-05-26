package wirerewrite

import (
	"encoding/json"
)

// applyFieldOrderRule re-serializes body with Go's map key sorting (which
// produces alphabetically ordered JSON keys) so requests with identical
// content but different field orderings produce the same bytes for the
// cache key. Returns (sorted body, 0, 0) — this rule never "strips" bytes
// in the audit sense so it returns zero counts.
// Fail-open: returns original body on any parse error.
func applyFieldOrderRule(body []byte) (out []byte, count int, removed int) {
	if len(body) == 0 {
		return body, 0, 0
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body, 0, 0
	}
	sorted, err := json.Marshal(v)
	if err != nil {
		return body, 0, 0
	}
	return sorted, 0, 0
}
