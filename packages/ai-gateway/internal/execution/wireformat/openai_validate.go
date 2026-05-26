package wireformat

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/tidwall/gjson"
)

var (
	// ErrOpenAIChunkShape is returned when a chat.completion.chunk payload
	// does not match the minimal streaming contract exercised by ai-gateway.
	ErrOpenAIChunkShape = errors.New("wireformat: invalid OpenAI chat.completion.chunk shape")
)

// IsOpenAIStreamDone returns true when the SSE data payload is the literal
// "[DONE]" terminator described in OpenAI streaming guides.
func IsOpenAIStreamDone(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]"))
}

// ValidateOpenAIChatCompletionChunk checks minimal fields for a streamed
// chat.completion.chunk object (see OpenAI streaming events reference).
// Empty choices with a usage object is accepted for stream_options usage chunks.
func ValidateOpenAIChatCompletionChunk(data []byte) error {
	root := gjson.ParseBytes(data)
	if !root.Exists() {
		return fmt.Errorf("%w: empty json", ErrOpenAIChunkShape)
	}
	if root.Get("object").String() != "chat.completion.chunk" {
		return fmt.Errorf("%w: object=%q want chat.completion.chunk", ErrOpenAIChunkShape, root.Get("object").String())
	}
	if !root.Get("id").Exists() || root.Get("id").String() == "" {
		return fmt.Errorf("%w: missing id", ErrOpenAIChunkShape)
	}
	if !root.Get("created").Exists() {
		return fmt.Errorf("%w: missing created", ErrOpenAIChunkShape)
	}
	if !root.Get("model").Exists() || root.Get("model").String() == "" {
		return fmt.Errorf("%w: missing model", ErrOpenAIChunkShape)
	}
	choices := root.Get("choices")
	if !choices.Exists() {
		return fmt.Errorf("%w: missing choices", ErrOpenAIChunkShape)
	}
	arr := choices.Array()
	if len(arr) == 0 {
		if root.Get("usage").Exists() {
			return nil
		}
		return fmt.Errorf("%w: empty choices without usage", ErrOpenAIChunkShape)
	}
	for i, ch := range arr {
		if !ch.Get("index").Exists() {
			return fmt.Errorf("%w: choices[%d] missing index", ErrOpenAIChunkShape, i)
		}
		if !ch.Get("delta").Exists() {
			return fmt.Errorf("%w: choices[%d] missing delta", ErrOpenAIChunkShape, i)
		}
	}
	return nil
}
