package canonicalbridge

import provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"

// finishTracker remembers the finish_reason from whichever chunk carried it, so
// an encoder writing its terminal frame can still name the real stop condition.
//
// It exists because the wire and the canonical stream disagree about WHERE the
// stop condition lives. OpenAI-shaped upstreams report finish_reason on a
// trailing delta-EMPTY chunk and only then send the terminator, so the chunk an
// encoder emits its terminal frame on carries no finish_reason of its own.
// Reading it off that chunk alone silently rewrote every tool-calling turn's
// "tool_calls" into "stop" — and a client that keys tool execution off
// finish_reason then does not run the tool. Cross-format decoders that DO report
// the stop condition on the terminal chunk keep working: resolve prefers the
// chunk's own value.
//
// Every stream encoder embeds this rather than keeping its own copy: the defect
// was found in one encoder and was present in all five, which is what a
// per-encoder copy of the same three lines produces.
type finishTracker struct {
	seen string
}

// observe records a chunk's finish_reason if it carries one. Call it once at the
// top of Write, before any branch can return early.
func (f *finishTracker) observe(chunk provcore.Chunk) {
	if chunk.FinishReason != "" {
		f.seen = chunk.FinishReason
	}
}

// resolve returns the finish_reason to render on a terminal frame: the chunk's
// own if it has one, else the last one observed on this stream.
func (f *finishTracker) resolve(chunk provcore.Chunk) string {
	if chunk.FinishReason != "" {
		return chunk.FinishReason
	}
	return f.seen
}
