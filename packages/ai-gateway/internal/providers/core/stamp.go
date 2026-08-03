package core

import (
	"bytes"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// SurgicalModelStamp rewrites the top-level `model` field of a JSON object
// body to model, without decoding the whole body in the common case. It is
// the shared mechanics for every codec whose native wire carries the model
// at the body root (OpenAI-family chat/embeddings, Anthropic /v1/messages).
//
//   - Body already carries model → the SAME slice is returned (zero copy):
//     rebuilding a ~50KB body to write an identical value is the dominant
//     hot-path allocation this fast path exists to avoid.
//   - Single `"model"` occurrence → surgical sjson set of just that field.
//   - Duplicate `"model"` occurrences → decode + re-marshal. This dup-key
//     check is a PRECONDITION of the sjson edit, not a policy guard: sjson
//     edits the FIRST occurrence while JSON parsers and the cache-key
//     canonicaliser take last-wins, so an sjson edit on that pathological
//     shape would silently target the wrong model. It therefore runs only
//     here, where an edit is about to happen — never as a hoisted scan on
//     paths that edit nothing.
//
// The caller is responsible for the non-object carve-out: sjson.SetBytes on
// a non-object body FABRICATES a synthetic {"model":X} instead of forwarding
// the client's bytes, so dispatch forwards non-object bodies verbatim and
// never calls into stamping code.
func SurgicalModelStamp(body []byte, model string) ([]byte, error) {
	if len(body) == 0 || model == "" {
		return body, nil
	}
	if bytes.Count(body, []byte(`"model"`)) > 1 {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		payload["model"] = model
		return json.Marshal(payload)
	}
	if gjson.GetBytes(body, "model").String() == model {
		return body, nil
	}
	return sjson.SetBytes(body, "model", model)
}
