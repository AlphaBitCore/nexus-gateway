package wirerewrite

import (
	"github.com/goccy/go-json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ephemeralCC is the Anthropic cache_control marker for ephemeral (5-minute) caching.
var ephemeralCC = map[string]string{"type": "ephemeral"}

// injectCacheMarkers adds explicit cache_control markers at content-block
// boundaries in an Anthropic Messages API request body. Explicit marking
// (placing cache_control on the system prompt content block and optionally
// on a conversation history turn) is required for Anthropic to report
// cache_creation_input_tokens and cache_read_input_tokens in its response —
// enabling gateway-side cost tracking and cache ROI analytics.
//
// Strategy:
//   - system field: convert string → content block array and stamp the
//     last text block with cache_control. Already-array system prompts
//     get cache_control on the last text block that lacks one.
//   - messages: when boundary3Enabled, also stamp the last content block
//     of the most recent user message that appears ≥ 2 turns before the
//     current one (i.e. conversation history boundary).
//
// If the client has already set any cache_control (root or block level),
// the body is returned unchanged — respecting explicit caller intent.
//
// Fail-open: returns original body on any error.
func injectCacheMarkers(body []byte, cacheType string, boundary3Enabled bool) ([]byte, error) {
	if countExistingMarkers(body) > 0 {
		return body, nil
	}

	cc := map[string]string{"type": "ephemeral"}
	if cacheType != "ephemeral" {
		// Only "ephemeral" is supported by the Anthropic public API.
		// Treat any other value as ephemeral to avoid upstream 400 errors.
		cc = ephemeralCC
	}

	current := body

	// --- system prompt ---
	// Convert the system field (string or array) to a content block array
	// and add cache_control to the last text block.
	systemVal := gjson.GetBytes(current, "system")
	if systemVal.Exists() {
		var blocks []map[string]any

		switch systemVal.Type {
		case gjson.String:
			// String form — wrap in a single text content block.
			blocks = []map[string]any{
				{"type": "text", "text": systemVal.String(), "cache_control": cc},
			}
		case gjson.JSON:
			// Array form — stamp the last text block that has no cache_control.
			if err := json.Unmarshal([]byte(systemVal.Raw), &blocks); err == nil {
				stamped := false
				for i := len(blocks) - 1; i >= 0; i-- {
					b := blocks[i]
					if b["type"] == "text" {
						if _, hasMark := b["cache_control"]; !hasMark {
							b["cache_control"] = cc
							blocks[i] = b
							stamped = true
						}
						break
					}
				}
				if !stamped {
					// All text blocks already marked or no text blocks; leave unchanged.
					blocks = nil
				}
			}
		}

		if blocks != nil {
			newBody, err := sjson.SetBytes(current, "system", blocks)
			if err == nil {
				current = newBody
			}
		}
	}

	// --- conversation history boundary (boundary3) ---
	// When enabled, also stamp the most recent user message in the
	// conversation history (all but the last user message) so that
	// repeated multi-turn conversations benefit from a second cache
	// breakpoint covering the accumulated history.
	if boundary3Enabled {
		msgs := gjson.GetBytes(current, "messages")
		if msgs.IsArray() {
			arr := msgs.Array()
			// Find the second-to-last user message (skip the last one;
			// the last user message changes every turn and should not
			// be cached — it is not a stable prefix).
			userIndexes := []int{}
			for i, m := range arr {
				if m.Get("role").String() == "user" {
					userIndexes = append(userIndexes, i)
				}
			}
			if len(userIndexes) >= 2 {
				targetIdx := userIndexes[len(userIndexes)-2]
				current = stampMessageCacheControl(current, arr, targetIdx, cc)
			}
		}
	}

	return current, nil
}

// stampMessageCacheControl adds cache_control to the last text content block
// of the message at messages[idx]. Returns original body on any error.
func stampMessageCacheControl(body []byte, arr []gjson.Result, idx int, cc map[string]string) []byte {
	content := arr[idx].Get("content")
	if !content.Exists() {
		return body
	}

	var blocks []map[string]any
	switch content.Type {
	case gjson.String:
		blocks = []map[string]any{
			{"type": "text", "text": content.String(), "cache_control": cc},
		}
	case gjson.JSON:
		if err := json.Unmarshal([]byte(content.Raw), &blocks); err != nil {
			return body
		}
		for i := len(blocks) - 1; i >= 0; i-- {
			b := blocks[i]
			if b["type"] == "text" {
				if _, hasMark := b["cache_control"]; !hasMark {
					b["cache_control"] = cc
					blocks[i] = b
				}
				break
			}
		}
	default:
		return body
	}

	path := "messages." + itoa(idx) + ".content"
	newBody, err := sjson.SetBytes(body, path, blocks)
	if err != nil {
		return body
	}
	return newBody
}

// itoa converts a non-negative int to its decimal string representation
// without importing strconv (keeping the package dependency-light).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// countExistingMarkers counts cache_control blocks already present in the body.
func countExistingMarkers(body []byte) int {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return 0
	}
	return countCacheControlKeys(v)
}

func countCacheControlKeys(v any) int {
	switch x := v.(type) {
	case map[string]any:
		n := 0
		for k, child := range x {
			if k == "cache_control" {
				n++
			} else {
				n += countCacheControlKeys(child)
			}
		}
		return n
	case []any:
		n := 0
		for _, child := range x {
			n += countCacheControlKeys(child)
		}
		return n
	default:
		return 0
	}
}

// countInjectedMarkers returns the difference in cache_control count
// between the modified body and the original.
func countInjectedMarkers(original, modified []byte) int {
	before := countExistingMarkers(original)
	after := countExistingMarkers(modified)
	n := after - before
	if n < 0 {
		return 0
	}
	return n
}
