package format

import (
	"github.com/goccy/go-json"

	"github.com/tidwall/gjson"
)

// ExtractDeltaText extracts delta.content from an OpenAI SSE chunk.
// Returns empty string if not an assistant content delta.
func ExtractDeltaText(data string) string {
	return gjson.Get(data, "choices.0.delta.content").String()
}

// OpenAIStreamDeltaPayload returns the JSON body of a single OpenAI
// SSE chunk whose `delta.content` carries the supplied text. Used by
// the compliance pipeline's Modify decision to replace held-back
// assistant deltas with a hook-rewritten text block before flushing
// the buffered SSE frames.
//
// The returned string is the `data:` field value only — the caller is
// responsible for framing it via WriteEvent / WriteTypedEvent.
func OpenAIStreamDeltaPayload(content string) (string, error) {
	m := map[string]any{
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{"content": content},
		}},
	}
	out, err := json.Marshal(m)
	return string(out), err
}
