package openai

import (
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/tidwall/gjson"
)

// The Responses API streams a different grammar from chat/completions. A chat
// chunk is a `choices[].delta` object; a Responses event is a self-describing
// envelope — `{"type":"response.output_text.delta","delta":"…"}` — with no
// `choices` anywhere in it. Reading a Responses stream with the chat extractor
// therefore yields nothing at all, which is indistinguishable from a stream that
// carried no text: measured on a captured 206-frame reasoning stream, the chat
// extractor pulled 0 bytes out of 674 bytes of real assistant and reasoning text.
//
// That mattered beyond audit fidelity. The streaming compliance substrate grows
// its scan buffer from exactly this extraction, so a Responses stream was being
// delivered to the client with nothing scanned.

// responsesEventPrefix identifies the envelope. Chat chunks carry
// `object: "chat.completion.chunk"` and no top-level `type`, so the two grammars
// cannot be confused.
const responsesEventPrefix = "response."

// looksLikeResponsesEvent reports whether this frame is a Responses API event.
func looksLikeResponsesEvent(chunk []byte) bool {
	t := gjson.GetBytes(chunk, "type")
	return t.Type == gjson.String && strings.HasPrefix(t.Str, responsesEventPrefix)
}

// extractResponsesStreamEvent pulls the scannable text out of one Responses
// event.
//
// The text comes from the `delta` field and only from it. Every incremental
// event is mirrored by a `.done` that repeats the ACCUMULATED value under its
// own name — `response.output_text.done` carries the whole `text` — so reading
// both would count the same characters twice and, on the redaction path, ask the
// splice to mask one span in two different frames. `delta` is the wire's name
// for "the fragment that just arrived", and no `.done` event carries one:
// measured across the captured Responses streams, a non-empty string `delta`
// appears on `output_text.delta`, `reasoning_summary_text.delta` and
// `function_call_arguments.delta`, and on nothing else. Reading the field rather
// than matching event names is also what lets a newly shipped incremental event
// arrive already handled.
//
// The channel is derived from the event name for the same reason. Strip the
// `response.` prefix and what remains names the item the fragment belongs to. A
// name that mentions reasoning is reasoning; one that names function-call
// arguments is a tool call; anything else is assistant-visible text. So a text
// item this code has never seen — `response.output_audio_transcript.delta`, say
// — lands on the scanned path by default rather than silently unscanned, which
// is the direction a compliance seam should fail in.
func extractResponsesStreamEvent(chunk []byte) (traffic.NormalizedContent, error) {
	meta := map[string]string{}
	// The terminal event carries the finished response object; surface its status
	// the way the chat extractor surfaces finish_reason.
	if st := gjson.GetBytes(chunk, "response.status"); st.Type == gjson.String && st.Str != "" {
		meta["finish_reason"] = st.Str
	}

	delta := gjson.GetBytes(chunk, "delta")
	if delta.Type != gjson.String || delta.Str == "" {
		return traffic.NormalizedContent{Metadata: meta}, nil
	}
	kind := strings.TrimPrefix(gjson.GetBytes(chunk, "type").Str, responsesEventPrefix)

	out := traffic.NormalizedContent{Metadata: meta}
	switch {
	case strings.Contains(kind, "reasoning"):
		out.ReasoningSegments = []string{delta.Str}
	case strings.Contains(kind, "function_call_arguments"):
		out.ToolCallSegments = []string{delta.Str}
	default:
		out.Segments = []string{delta.Str}
	}
	return out, nil
}
