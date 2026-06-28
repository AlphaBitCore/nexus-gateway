package codecs

import "github.com/goccy/go-json"

// spliceDeltasIntoTerminal appends the accumulated output_text deltas to the
// terminal response object as an assistant message, but ONLY when the terminal
// carries no assistant output_text of its own (ok=false otherwise, so a
// standard OpenAI terminal that already holds the full text stays authoritative
// and unchanged). usage/status/model on the terminal are preserved.
func spliceDeltasIntoTerminal(terminal json.RawMessage, deltas string) ([]byte, bool) {
	var obj map[string]any
	if json.Unmarshal(terminal, &obj) != nil {
		return nil, false
	}
	if responseObjectHasOutputText(obj) {
		return nil, false
	}
	msg := map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []any{map[string]any{
			"type": "output_text",
			"text": deltas,
		}},
	}
	if out, ok := obj["output"].([]any); ok {
		obj["output"] = append(out, msg)
	} else {
		obj["output"] = []any{msg}
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return b, true
}

// responseObjectHasOutputText reports whether the response object already
// carries a non-empty assistant output_text content block.
func responseObjectHasOutputText(obj map[string]any) bool {
	out, ok := obj["output"].([]any)
	if !ok {
		return false
	}
	for _, it := range out {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		content, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, c := range content {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := cm["type"].(string); t == "output_text" || t == "" {
				if s, _ := cm["text"].(string); s != "" {
					return true
				}
			}
		}
	}
	return false
}
