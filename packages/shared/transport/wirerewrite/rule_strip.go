package wirerewrite

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyStripRule removes all regex matches from the JSON values at the
// given gjson path within body. Returns (modified body, strip count, bytes removed).
// Fail-open: returns original body on any error.
func applyStripRule(body []byte, path string, r *Rule) (out []byte, count int, removed int) {
	re := r.Regex
	if re == nil {
		return body, 0, 0
	}

	result := gjson.GetBytes(body, path)
	if !result.Exists() {
		return body, 0, 0
	}

	var err error
	current := body

	switch {
	case result.IsArray():
		// e.g. path "system.#.text": iterate each array element and
		// replace the # selector with the concrete index for sjson writes.
		result.ForEach(func(key, val gjson.Result) bool {
			if val.Type == gjson.String {
				stripped := re.ReplaceAllString(val.String(), "")
				diff := len(val.String()) - len(stripped)
				if diff > 0 {
					elemPath := resolveArrayPath(path, int(key.Int()))
					var setErr error
					current, setErr = sjson.SetBytes(current, elemPath, stripped)
					if setErr != nil {
						err = setErr
						return false
					}
					count++
					removed += diff
				}
			}
			return true
		})
	case result.Type == gjson.String:
		// Direct string value at path
		original := result.String()
		stripped := re.ReplaceAllString(original, "")
		diff := len(original) - len(stripped)
		if diff > 0 {
			current, err = sjson.SetBytes(current, path, stripped)
			if err == nil {
				count++
				removed += diff
			}
		}
	}

	if err != nil {
		return body, 0, 0
	}
	return current, count, removed
}

// resolveArrayPath converts a gjson array path like "system.#.text" with
// index i into a concrete sjson path like "system.0.text".
func resolveArrayPath(gpath string, index int) string {
	// Replace the first # with the concrete index.
	for i := range len(gpath) {
		if gpath[i] == '#' {
			return fmt.Sprintf("%s%d%s", gpath[:i], index, gpath[i+1:])
		}
	}
	return gpath
}
