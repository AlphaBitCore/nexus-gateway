// Package responses — classify.go detects the OpenAI response wire shape from
// the actual response bytes, so the egress side decides how to decode/encode a
// /v1/responses reply from what the upstream really returned rather than from
// the target provider's declared Format. A provider tagged OpenAI may serve the
// chat-completions wire; trusting the bytes (not the Format) is what keeps a
// chat-shaped reply from being forwarded verbatim to a Responses client.
package responses

import (
	"strings"

	"github.com/tidwall/gjson"
)

// WireClass is the detected OpenAI response wire shape.
type WireClass int

const (
	// ClassUnknown means the bytes match neither known shape — a keep-alive
	// comment, an empty frame, or garbage. Callers must fail closed (route to
	// the canonical/transcode or enforced lane), never to verbatim passthrough.
	ClassUnknown WireClass = iota
	// ClassResponses is the Responses API shape: a non-stream body whose
	// top-level object is "response", or a stream whose events are response.*.
	ClassResponses
	// ClassChat is the chat-completions shape: object "chat.completion" (non
	// stream) or "chat.completion.chunk" (stream).
	ClassChat
)

// ClassifyNonStreamBody classifies a non-streaming response body by its
// top-level "object" discriminator.
func ClassifyNonStreamBody(body []byte) WireClass {
	switch gjson.GetBytes(body, "object").String() {
	case "response":
		return ClassResponses
	case "chat.completion":
		return ClassChat
	default:
		return ClassUnknown
	}
}

// ClassifyFirstSSEFrame classifies a stream from its first decoded SSE event.
// eventType is the SSE "event:" line value (may be empty when the grammar
// carries the type inside the data payload); data is the "data:" payload bytes.
func ClassifyFirstSSEFrame(eventType string, data []byte) WireClass {
	if strings.HasPrefix(eventType, "response.") {
		return ClassResponses
	}
	if strings.HasPrefix(gjson.GetBytes(data, "type").String(), "response.") {
		return ClassResponses
	}
	if gjson.GetBytes(data, "object").String() == "chat.completion.chunk" {
		return ClassChat
	}
	return ClassUnknown
}
