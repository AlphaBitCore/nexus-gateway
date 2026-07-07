package canonicalbridge

// stream_encoders_fastpath.go — the per-token content-delta fast path for
// openAIStreamEncoder, split from stream_encoders.go to keep that file under the
// size ratchet. The output is byte-identical to the struct-marshalled envelope
// (pinned by the differential + fuzz tests in stream_encoders_fastpath_test.go);
// it exists only to skip go-json's reflection over the whole envelope struct for
// the dominant streaming frame.

import (
	"fmt"

	json "github.com/goccy/go-json"
)

// contentFramePrefix is the invariant head of a content-delta frame up to (and
// including) the opening of the content value. The envelope's fields are encoded
// in alphabetical order (see stream_encoders_openai_types.go), so `choices` is
// first and nothing stream-specific precedes the content — this prefix is a true
// constant shared by every encoder.
var contentFramePrefix = []byte(`data: {"choices":[{"delta":{"content":`)

// buildContentSuffix assembles the fixed tail of a content-delta frame:
// everything after the content value — the null finish_reason, index, and the
// per-stream-constant created/id/model/object fields, plus the closing framing
// and trailing blank line. created/id/model are emitted via json.Marshal so
// their escaping and numeric formatting are byte-identical to the
// struct-marshalled envelope.
func buildContentSuffix(created int64, id, model string) []byte {
	idJSON, _ := json.Marshal(id)
	modelJSON, _ := json.Marshal(model)
	var s []byte
	s = append(s, `},"finish_reason":null,"index":0}],"created":`...)
	s = append(s, fmt.Appendf(nil, "%d", created)...)
	s = append(s, `,"id":`...)
	s = append(s, idJSON...)
	s = append(s, `,"model":`...)
	s = append(s, modelJSON...)
	s = append(s, `,"object":"chat.completion.chunk"}`...)
	s = append(s, '\n', '\n')
	return s
}

// emitContentDelta appends a content-delta frame for the per-token hot path.
// It is byte-identical to emit(oaiStreamChoice{Delta:{Content:&content}}, nil)
// but marshals ONLY the content string (go-json's cheap string fast path) plus a
// precomputed constant prefix/suffix, avoiding reflection over the whole envelope
// struct graph. The content string uses json.Marshal so its HTML-safe escaping
// matches the struct path exactly. Pinned by TestOpenAIStreamEncoder_ContentDeltaFastPath.
func (e *openAIStreamEncoder) emitContentDelta(content string) {
	// Lazily assemble the per-stream suffix on first use so the fast path is
	// robust to any construction (the constructor, a struct literal in tests, or
	// a post-construction id/created/model set) — id/created/model are immutable
	// once the stream is producing content.
	if e.contentSuffix == nil {
		e.contentSuffix = buildContentSuffix(e.created, e.id, e.model)
	}
	contentJSON, _ := json.Marshal(content)
	e.scratch = append(e.scratch, contentFramePrefix...)
	e.scratch = append(e.scratch, contentJSON...)
	e.scratch = append(e.scratch, e.contentSuffix...)
}
