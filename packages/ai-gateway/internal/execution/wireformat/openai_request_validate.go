package wireformat

import (
	"errors"
	"fmt"

	"github.com/tidwall/gjson"
)

// ErrOpenAIRequestShape is returned when a chat.completions request body does
// not match the minimal documented envelope.
var ErrOpenAIRequestShape = errors.New("wireformat: invalid OpenAI chat completions request shape")

// ValidateOpenAIChatCompletionRequest checks model plus messages (and optional
// stream, tools, response_format blocks when present).
func ValidateOpenAIChatCompletionRequest(data []byte) error {
	root := gjson.ParseBytes(data)
	if !root.Exists() {
		return fmt.Errorf("%w: empty json", ErrOpenAIRequestShape)
	}
	if root.Get("model").String() == "" {
		return fmt.Errorf("%w: missing model", ErrOpenAIRequestShape)
	}
	msgs := root.Get("messages")
	if !msgs.Exists() || !msgs.IsArray() || len(msgs.Array()) == 0 {
		return fmt.Errorf("%w: messages must be a non-empty array", ErrOpenAIRequestShape)
	}
	if tools := root.Get("tools"); tools.Exists() && !tools.IsArray() {
		return fmt.Errorf("%w: tools must be an array when present", ErrOpenAIRequestShape)
	}
	if rf := root.Get("response_format"); rf.Exists() && !rf.IsObject() {
		return fmt.Errorf("%w: response_format must be an object when present", ErrOpenAIRequestShape)
	}
	return nil
}
