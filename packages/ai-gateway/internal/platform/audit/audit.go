// Package audit implements asynchronous audit log writing for the AI gateway.
// Records are enqueued in-memory and published to MQ periodically.
//
// File layout:
//   - audit.go          — package-level constants + EndpointType vocabulary
//   - enums.go          — cache/hook enum types + classification helpers
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
	// maxQueueSize bounds the in-memory record buffer. On overflow Enqueue
	// applies bounded backpressure and then spills to the durable NDJSON
	// sink (never a silent drop). Sized for a high-concurrency burst: large
	// enough to absorb a multi-second drain hiccup at thousands of req/s
	// before backpressure even engages (records are pointers, so the memory
	// cost is modest).
	maxQueueSize = 50000

	// flushHighWater triggers an immediate flush once the buffer reaches
	// this depth, instead of waiting for the next ticker. Without it a
	// burst toward maxQueueSize within one flush interval would force
	// backpressure/spill even when the pipeline has drain capacity. Set
	// well below maxQueueSize so a spike flushes early.
	flushHighWater = 1000

	// publishConcurrency bounds how many records a single flush publishes
	// to MQ in parallel. The per-record cost (normalize + spill + one
	// JetStream publish-and-ack RTT) is the drain ceiling; fanning it out
	// lifts that ceiling roughly publishConcurrency-fold. js.Publish is
	// safe for concurrent use on one connection.
	publishConcurrency = 32

	// backpressureWait bounds how long Enqueue spends slowing the producer
	// (waiting for the flush loop to free buffer space) before spilling the
	// record to disk. Short enough that a request goroutine is never held
	// long after the client already has its response; long enough to ride
	// out a brief drain hiccup. backpressurePoll is the retry granularity.
	backpressureWait = 200 * time.Millisecond
	backpressurePoll = 10 * time.Millisecond

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
// shared/normalize from the audit record. ai-gateway keys on the
// *ingress* format for BOTH directions — never the upstream adapter
// type — because every byte buffer ai-gateway captures is in the
// client's wire shape:
//
//   - Request:  captured at handler dispatch in the client's wire shape
//     (the codec translates A→canonical→B only for the bytes
//     sent upstream, which are never the captured RequestBody).
//   - Response: the proxy ALWAYS re-encodes the upstream reply back to
//     the ingress shape before it touches rec.ResponseBody.
//     Every assignment site does this:
//   - handleNonStream / handleNonStreamHit capture the body
//     AFTER egressReshapeNonStream (B→canonical→A).
//   - the streaming tee wraps the client ResponseWriter, so
//     it buffers the per-chunk-reshaped SSE the client got.
//   - both error paths capture
//     EncodeErrorEnvelopeForIngress(ingress, …) output.
//     There is no path where the captured response bytes are in
//     the upstream's wire shape.
//
// Keying on the ingress format is therefore correct and deterministic for
// every arm — a Gemini-backed model served over the OpenAI-compatible
// `/v1/chat/completions` ingress records OpenAI Chat shape (key "openai"),
// and an OpenAI model served over the Gemini `:generateContent` ingress
// records Gemini `candidates[]` shape (key "gemini"). Cross-format arms
// (/v1/responses, /v1/messages) resolve via the registry's path-keyed
// fallback (`::/v1/responses`, `::/v1/messages`) when no adapter-only key
// matches the ingress format. Streaming SSE that no Tier-1 key folds is
// caught by the Tier-2 SSE walker regardless of key.
//
// Empty when ingress format wasn't determined (early failures before
// format resolution); the registry then falls through to the path-keyed
// and generic-http tiers.
func normalizeAdapterType(rec *Record) string {
	return strings.ToLower(rec.IngressFormat)
}
