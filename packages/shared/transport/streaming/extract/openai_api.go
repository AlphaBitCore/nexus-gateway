// OpenAI Chat Completions extractor. Recognizes the SSE shape used by
// `/v1/chat/completions` (and the OpenAI-compatible variants on DeepSeek,
// Mistral, Moonshot, GLM, MiniMax, Azure):
//
//	data: {"choices":[{"delta":{"content":"..."}}], ...}
//	data: [DONE]
//
// Only `choices[*].delta.content` is treated as canonical completion
// content. `tool_calls` and structured-output deltas are intentionally
// ignored at this layer — Phase 7 may add a structured-output channel.
package extract

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

const openaiAPIID = "openai-api"

type openaiAPIExtractor struct{}

// NewOpenAIAPIExtractor returns the OpenAI Chat Completions extractor.
func NewOpenAIAPIExtractor() ContentExtractor { return openaiAPIExtractor{} }

func (openaiAPIExtractor) ID() string { return openaiAPIID }

// ExtractRequest pulls the user's prompt out of the OpenAI request body.
// All `messages[*].content` values are concatenated; non-text content
// blocks (e.g. image_url) are skipped. Returns the empty content if the
// body is not parseable JSON or has no messages — non-LLM traffic.
func (openaiAPIExtractor) ExtractRequest(body []byte) ExtractedContent {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ExtractedContent{}
	}
	var b strings.Builder
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		// String content directly on the message.
		if c := msg.Get("content").String(); c != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c)
		}
		// Multi-modal content array — stitch the text parts together.
		msg.Get("content").ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				if t := part.Get("text").String(); t != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(t)
				}
			}
			return true
		})
		return true
	})
	return ExtractedContent{Prompt: b.String()}
}

func (openaiAPIExtractor) NewAccumulator() Accumulator {
	return &openaiAPIAccumulator{}
}

type openaiAPIAccumulator struct {
	prompt     strings.Builder
	completion strings.Builder
	truncated  bool
}

// Feed parses one `data:` payload. The framing (split by `\n\n` and the
// `data: ` prefix) is the streaming pipeline's job — we receive only the
// JSON payload here. The terminal `[DONE]` sentinel is filtered upstream.
func (a *openaiAPIAccumulator) Feed(frame []byte) ExtractedDelta {
	frame = bytes.TrimSpace(frame)
	if len(frame) == 0 || bytes.Equal(frame, []byte("[DONE]")) {
		return ExtractedDelta{}
	}
	if !gjson.ValidBytes(frame) {
		return ExtractedDelta{}
	}
	var delta ExtractedDelta
	gjson.GetBytes(frame, "choices").ForEach(func(_, ch gjson.Result) bool {
		if c := ch.Get("delta.content").String(); c != "" {
			delta.Completion += c
		}
		return true
	})
	if delta.Completion != "" {
		a.completion.WriteString(delta.Completion)
	}
	return delta
}

func (a *openaiAPIAccumulator) Snapshot() ExtractedContent {
	return ExtractedContent{
		Prompt:     a.prompt.String(),
		Completion: a.completion.String(),
		Truncated:  a.truncated,
	}
}

func (a *openaiAPIAccumulator) Truncate() { a.truncated = true }
