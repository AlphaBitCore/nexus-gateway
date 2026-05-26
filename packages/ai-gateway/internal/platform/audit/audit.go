// Package audit implements asynchronous audit log writing for the AI gateway.
// Records are enqueued in-memory and published to MQ periodically.
//
// File layout:
//   - audit.go          — package-level constants + EndpointType vocabulary
//   - enums.go          — cache/hook enum types + classification helpers
//   - storage_action.go — applyStorageAction (NormalizedPayload mutation)
//   - record.go         — Record struct + ApplyVKMeta + small helpers
//   - writer.go         — Writer lifecycle, Enqueue, flush, Close
//   - message.go        — recordToMessage (wire-format builder)
//   - coerce.go         — coerceEmbeddingRow authoritative chat-field zeroing for embedding rows
package audit

import (
	"strings"
	"time"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

const (
	defaultFlushInterval = 5 * time.Second
	defaultBatchSize     = 100
	maxQueueSize         = 10000

	// normalizeWireVersion stamps TrafficEventMessage.NormalizeVersion so
	// the Hub db-writer persists the version used to produce the payload.
	// Kept in sync with normcore.SchemaVersion.
	normalizeWireVersion = normcore.SchemaVersion
)

// EndpointType is the typed-string alias used to classify the API
// endpoint a request targets. Values are the canonical
// typology.EndpointKind strings ("chat", "embeddings", "stt", "tts",
// "image_generation", "batch"). The constants below mirror the matching
// typology kinds; downstream cost / Prometheus / audit MQ consumers all
// read these strings verbatim.
type EndpointType = string

const (
	// EndpointTypeChat covers /v1/chat/completions, /v1/messages
	// (Anthropic), /v1/responses (OpenAI Responses API), /v1/completions
	// (legacy), Gemini :generateContent, Vertex :generateContent,
	// Bedrock Converse, Cohere chat — every chat-family wire shape.
	EndpointTypeChat EndpointType = "chat"
	// EndpointTypeEmbeddings covers every embedding endpoint
	// (/v1/embeddings, Cohere /v1/embed, Gemini :embedContent, Vertex
	// :embedContent, Voyage, Bedrock Titan/Cohere embed).
	EndpointTypeEmbeddings EndpointType = "embeddings"
	// EndpointTypeSTT covers /v1/audio/transcriptions and
	// /v1/audio/translations (speech-to-text endpoints).
	EndpointTypeSTT EndpointType = "stt"
	// EndpointTypeTTS covers /v1/audio/speech (text-to-speech).
	EndpointTypeTTS EndpointType = "tts"
	// EndpointTypeImageGeneration covers /v1/images/generations,
	// /v1/images/edits, and /v1/images/variations.
	EndpointTypeImageGeneration EndpointType = "image_generation"
	// EndpointTypeBatch covers /v1/batches (async batch endpoints).
	EndpointTypeBatch EndpointType = "batch"
)

// EndpointTypeFromPath maps the path-segment string used internally by
// the AI Gateway (e.g. "chat/completions", "embeddings") to the
// canonical typology.EndpointKind string.
//
// Returns an empty string for unknown segments; the audit Record stores
// the empty string for early-failure rows (no kind classification yet).
func EndpointTypeFromPath(p string) EndpointType {
	return string(typology.KindFromPathSegment(p))
}

// normalizeAdapterType returns the wire-format key fed to
// shared/normalize from the audit record. ai-gateway uses the *ingress*
// format here — not the upstream adapter type — because the captured
// RequestBody / ResponseBody bytes are always in the client's wire
// shape (the codec re-encodes both directions). A Gemini-backed model
// served over the OpenAI-compatible `/v1/chat/completions` ingress
// records its audit bytes in OpenAI Chat shape, so the OpenAI
// normalizer is the correct match — regardless of the upstream being
// Gemini. Empty when ingress format wasn't determined (early failures
// before format resolution).
func normalizeAdapterType(rec *Record, direction string) string {
	// Pick the normalizer key by direction:
	//   - "request"  → the bytes on disk are the CLIENT's wire shape
	//                  (captured before the codec translates), so the
	//                  ingress format is the right key.
	//   - "response" → the bytes on disk are the UPSTREAM's wire shape
	//                  (captured before the codec translates the reply
	//                  back to ingress), so the routed target's adapter
	//                  type is the right key. Falls back to ingress
	//                  format when the routed target wasn't recorded
	//                  (e.g. early failures, cache HIT replay).
	//
	// Before this split, both directions used IngressFormat → cross-format
	// requests like /v1/responses → Anthropic /v1/messages produced
	// traffic_event_normalized.response_status='partial' with
	// "openai-responses: response unmarshal: invalid character 'e' …"
	// (the 'e' is the leading char of an SSE `event:` line, or anthropic
	// JSON's `id` field starting with a different shape than the
	// openai-responses normalizer expects).
	if direction == "response" && rec.UpstreamAdapterType != "" {
		return strings.ToLower(rec.UpstreamAdapterType)
	}
	return strings.ToLower(rec.IngressFormat)
}
