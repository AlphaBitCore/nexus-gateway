package wireformat

import (
	"errors"
	"fmt"

	"github.com/tidwall/gjson"
)

// ErrAnthropicSSEShape is returned when an Anthropic streaming JSON payload
// does not match the documented event envelope (type field + event-specific keys).
var ErrAnthropicSSEShape = errors.New("wireformat: invalid Anthropic streaming event shape")

// ValidateAnthropicStreamingJSON checks the top-level "type" and required nested
// fields for common Anthropic SSE event kinds (see Anthropic streaming API docs).
func ValidateAnthropicStreamingJSON(data []byte) error {
	root := gjson.ParseBytes(data)
	if !root.Exists() {
		return fmt.Errorf("%w: empty json", ErrAnthropicSSEShape)
	}
	typ := root.Get("type").String()
	if typ == "" {
		return fmt.Errorf("%w: missing type", ErrAnthropicSSEShape)
	}
	switch typ {
	case "message_start":
		msg := root.Get("message")
		if !msg.Exists() {
			return fmt.Errorf("%w: message_start missing message", ErrAnthropicSSEShape)
		}
		if msg.Get("id").String() == "" {
			return fmt.Errorf("%w: message_start.message.id required", ErrAnthropicSSEShape)
		}
		if msg.Get("type").String() != "message" {
			return fmt.Errorf("%w: message_start.message.type want message got %q", ErrAnthropicSSEShape, msg.Get("type").String())
		}
		if msg.Get("role").String() != "assistant" {
			return fmt.Errorf("%w: message_start.message.role want assistant got %q", ErrAnthropicSSEShape, msg.Get("role").String())
		}
		if !msg.Get("usage").Exists() {
			return fmt.Errorf("%w: message_start.message.usage required", ErrAnthropicSSEShape)
		}
	case "content_block_start":
		if !root.Get("index").Exists() {
			return fmt.Errorf("%w: content_block_start missing index", ErrAnthropicSSEShape)
		}
		cb := root.Get("content_block")
		if !cb.Exists() {
			return fmt.Errorf("%w: content_block_start missing content_block", ErrAnthropicSSEShape)
		}
		if cb.Get("type").String() == "" {
			return fmt.Errorf("%w: content_block.type required", ErrAnthropicSSEShape)
		}
	case "content_block_delta":
		if !root.Get("index").Exists() {
			return fmt.Errorf("%w: content_block_delta missing index", ErrAnthropicSSEShape)
		}
		delta := root.Get("delta")
		if !delta.Exists() {
			return fmt.Errorf("%w: content_block_delta missing delta", ErrAnthropicSSEShape)
		}
		if delta.Get("type").String() == "" {
			return fmt.Errorf("%w: content_block_delta.delta.type required", ErrAnthropicSSEShape)
		}
	case "content_block_stop":
		if !root.Get("index").Exists() {
			return fmt.Errorf("%w: content_block_stop missing index", ErrAnthropicSSEShape)
		}
	case "message_delta":
		delta := root.Get("delta")
		if !delta.Exists() {
			return fmt.Errorf("%w: message_delta missing delta", ErrAnthropicSSEShape)
		}
	case "message_stop":
		// terminal event; no extra required fields in public docs
	case "ping":
		// keep-alive
	default:
		// Unknown event types are allowed on the wire; only validate known shapes.
		return nil
	}
	return nil
}
