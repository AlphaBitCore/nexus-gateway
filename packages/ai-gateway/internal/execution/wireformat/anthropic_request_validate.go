package wireformat

import (
	"errors"
	"fmt"

	"github.com/tidwall/gjson"
)

// ErrAnthropicRequestShape is returned when a Messages request body does not
// match the minimal documented envelope.
var ErrAnthropicRequestShape = errors.New("wireformat: invalid Anthropic Messages request shape")

// ValidateAnthropicMessagesRequest checks required Messages API fields and
// common optional blocks (metadata, stop_sequences) when present.
func ValidateAnthropicMessagesRequest(data []byte) error {
	root := gjson.ParseBytes(data)
	if !root.Exists() {
		return fmt.Errorf("%w: empty json", ErrAnthropicRequestShape)
	}
	if root.Get("model").String() == "" {
		return fmt.Errorf("%w: missing model", ErrAnthropicRequestShape)
	}
	if !root.Get("max_tokens").Exists() {
		return fmt.Errorf("%w: missing max_tokens", ErrAnthropicRequestShape)
	}
	msgs := root.Get("messages")
	if !msgs.Exists() || !msgs.IsArray() || len(msgs.Array()) == 0 {
		return fmt.Errorf("%w: messages must be a non-empty array", ErrAnthropicRequestShape)
	}
	if md := root.Get("metadata"); md.Exists() && !md.IsObject() {
		return fmt.Errorf("%w: metadata must be an object when present", ErrAnthropicRequestShape)
	}
	if ss := root.Get("stop_sequences"); ss.Exists() && !ss.IsArray() {
		return fmt.Errorf("%w: stop_sequences must be an array when present", ErrAnthropicRequestShape)
	}
	return nil
}
