// ChatGPT.com web client SSE extractor. The wire shape is markedly
// different from the public OpenAI API: chatgpt.com `/backend-api/f/conversation`
// streams JSON-Patch-style deltas plus a handful of routing / telemetry
// frames that should be filtered out of compliance audit content.
//
// Frame types and disposition (verified against a captured
// `/backend-api/f/conversation` response):
//
//	data: "v1"
//	  → encoding marker; skip.
//	data: {"type":"resume_conversation_token", ...}
//	  → routing JWT; skip (privacy: do not store).
//	data: {"type":"input_message", "input_message":{"content":{"parts":[…]}}}
//	  → user prompt echo; canonical Prompt.
//	event: delta
//	data: {"p":"/message/content/parts/0", "o":"append", "v":"<delta text>"}
//	  → assistant completion delta; canonical Completion.
//	event: delta
//	data: {"o":"patch", "v":[{"p":"/.../parts/0","o":"append","v":"…"}, …]}
//	  → JSON-Patch list; iterate and treat each `append`/`replace` on
//	    `/message/content/parts/<n>` as Completion delta.
//	data: {"type":"message_marker", ...}
//	data: {"type":"server_ste_metadata", ...}
//	data: {"type":"conversation_detail_metadata", ...}
//	data: {"type":"message_stream_complete", ...}
//	data: [DONE]
//	  → all skipped.
//
// The extractor is defensive — chatgpt.com's protocol is not public API
// and may change. Unknown frames produce empty deltas rather than panics.
package extract

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

const chatgptWebID = "chatgpt-web"

type chatgptWebExtractor struct{}

// NewChatGPTWebExtractor returns the chatgpt.com web SSE extractor.
func NewChatGPTWebExtractor() ContentExtractor { return chatgptWebExtractor{} }

func (chatgptWebExtractor) ID() string { return chatgptWebID }

// ExtractRequest parses the buffered request body. ChatGPT web posts a
// JSON object whose `messages[*].content.parts[]` carries the user's
// prompt; older shapes expose `messages[*].content.text` directly.
func (chatgptWebExtractor) ExtractRequest(body []byte) ExtractedContent {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ExtractedContent{}
	}
	var b strings.Builder
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		// Newer shape: content.parts[] of strings.
		msg.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
			if t := part.String(); t != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t)
			}
			return true
		})
		// Older shape: content.text directly.
		if t := msg.Get("content.text").String(); t != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t)
		}
		return true
	})
	return ExtractedContent{Prompt: b.String()}
}

func (chatgptWebExtractor) NewAccumulator() Accumulator {
	return &chatgptWebAccumulator{}
}

type chatgptWebAccumulator struct {
	prompt     strings.Builder
	completion strings.Builder
	truncated  bool
}

// Feed parses one chatgpt-web frame payload. The streaming pipeline strips
// the `data: ` prefix and the `event:` line before calling Feed.
func (a *chatgptWebAccumulator) Feed(frame []byte) ExtractedDelta {
	frame = bytes.TrimSpace(frame)
	if len(frame) == 0 {
		return ExtractedDelta{}
	}
	// String marker like `"v1"` or `[DONE]`.
	if bytes.Equal(frame, []byte("[DONE]")) || frame[0] == '"' {
		return ExtractedDelta{}
	}
	if !gjson.ValidBytes(frame) {
		return ExtractedDelta{}
	}

	delta := ExtractedDelta{}

	// Filtered-out frame types (do not contribute to canonical content).
	switch gjson.GetBytes(frame, "type").String() {
	case "resume_conversation_token", "message_marker", "server_ste_metadata",
		"conversation_detail_metadata", "message_stream_complete":
		return ExtractedDelta{}
	case "input_message":
		// Capture the user's prompt echo; subsequent frames carry
		// completion deltas. Newer shapes nest under `content.parts`.
		gjson.GetBytes(frame, "input_message.content.parts").ForEach(func(_, p gjson.Result) bool {
			if t := p.String(); t != "" {
				delta.Prompt += t
			}
			return true
		})
		if delta.Prompt != "" {
			a.prompt.WriteString(delta.Prompt)
		}
		return delta
	}

	// Single JSON-Patch op: {"p":"/...","o":"append","v":"<text>"}.
	if op := gjson.GetBytes(frame, "o").String(); op != "" {
		applyPatch(frame, op, &delta)
	}
	if delta.Completion != "" {
		a.completion.WriteString(delta.Completion)
	}
	return delta
}

// applyPatch handles both the singleton {p,o,v} shape and the wrapping
// {o:"patch", v:[...]} shape. Only the `append` op on a content/parts
// path contributes to the canonical Completion.
func applyPatch(frame []byte, op string, delta *ExtractedDelta) {
	if op == "patch" {
		gjson.GetBytes(frame, "v").ForEach(func(_, sub gjson.Result) bool {
			subOp := sub.Get("o").String()
			subPath := sub.Get("p").String()
			subValue := sub.Get("v")
			collect(subPath, subOp, subValue, delta)
			return true
		})
		return
	}
	path := gjson.GetBytes(frame, "p").String()
	val := gjson.GetBytes(frame, "v")
	collect(path, op, val, delta)
}

// collect accepts one (path, op, value) tuple and accumulates Completion
// when the path lands on a content part text and the op is append/replace.
// `path == ""` with op=add and a structured `value` is the initial frame
// announcing the assistant message — its `parts` may already carry a
// non-empty seed.
func collect(path, op string, value gjson.Result, delta *ExtractedDelta) {
	switch op {
	case "append":
		if isContentPartPath(path) && value.Type == gjson.String {
			delta.Completion += value.String()
		}
	case "replace":
		// `replace` on content/parts is rarer (used at end of stream
		// to set a status). Treat string values as completion content.
		if isContentPartPath(path) && value.Type == gjson.String {
			delta.Completion += value.String()
		}
	case "add":
		// Initial assistant message — `value` may carry seed parts.
		if path == "" {
			value.Get("message.content.parts").ForEach(func(_, p gjson.Result) bool {
				if t := p.String(); t != "" {
					delta.Completion += t
				}
				return true
			})
		}
	}
}

func isContentPartPath(p string) bool {
	// "/message/content/parts/0", "/message/content/parts/1", etc.
	return strings.HasPrefix(p, "/message/content/parts/")
}

func (a *chatgptWebAccumulator) Snapshot() ExtractedContent {
	return ExtractedContent{
		Prompt:     a.prompt.String(),
		Completion: a.completion.String(),
		Truncated:  a.truncated,
	}
}

func (a *chatgptWebAccumulator) Truncate() { a.truncated = true }
