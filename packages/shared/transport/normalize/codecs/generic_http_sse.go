package codecs

import (
	"github.com/goccy/go-json"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// maxSSEFrames and maxSSEFrameBytes together bound the structured frame
// projection so one runaway stream cannot inflate a
// traffic_event_normalized row without limit: the payload is persisted
// as one JSONB document per event, and the frame-count cap alone would
// leave row size unbounded (a single frame can carry megabytes of
// data). Frames beyond either cap are dropped from the projection (the
// original bytes remain available to the Raw view) and the payload is
// marked SSETruncated.
const (
	maxSSEFrames     = 2000
	maxSSEFrameBytes = 1 << 20
)

// normalizeSSE projects a Server-Sent Events stream into structured
// frames: one core.SSEFrame per `data:` line, carrying the dispatch's
// `event:` name plus either the decoded JSON tree (Data) or the
// verbatim string (DataText). We deliberately do NOT extract assistant
// text / tool calls / usage here — unknown SSE shapes have no stable
// schema, and the AI-protocol normalizers (openai_chat.go,
// anthropic_messages.go, …) own the streams that do. The frame list is
// the most-readable, lowest-brittleness projection for the rest: the
// operator scrolls structured frames instead of a wall of `data:`
// prefixes. The raw text is NOT duplicated into the payload — the Raw
// view already shows the original bytes.
func (n *GenericHTTPNormalizer) normalizeSSE(raw []byte) (core.NormalizedPayload, error) {
	frames := make([]core.SSEFrame, 0, 16)
	truncated := false
	frameBytes := 0
	_ = walkSSEFrames(raw, func(event, data string) {
		if len(frames) >= maxSSEFrames || frameBytes+len(data) > maxSSEFrameBytes {
			truncated = true
			return
		}
		frameBytes += len(data)
		frame := core.SSEFrame{Event: event}
		var decoded any
		// json.Unmarshal already validates — a preceding json.Valid was a
		// redundant second full scan (and goccy's Valid decodes into interface{},
		// allocating ~4x the frame). One Unmarshal both validates and decodes.
		if json.Unmarshal([]byte(data), &decoded) == nil && decoded != nil {
			frame.Data = decoded
		} else {
			// Non-JSON data, or the JSON literal `null` (whose decoded
			// tree is indistinguishable from "no data") — keep the
			// verbatim string so the frame stays self-describing.
			frame.DataText = data
		}
		frames = append(frames, frame)
	})
	return core.NormalizedPayload{
		Kind:             core.KindHTTPSSE,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "generic-http",
		HTTP: &core.HTTPPayload{
			BodyView: &core.HTTPBodyView{
				SSEFrames:    frames,
				SSETruncated: truncated,
			},
		},
	}, nil
}
