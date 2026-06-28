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

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

const (
	// maxQueueSize is the DEFAULT in-memory record-buffer cap (overridable per
	// Writer via WithMaxQueuedRecords / AuditConfig.MaxQueuedRecords). On overflow
	// Enqueue spills to the durable NDJSON sink (never a silent drop). Each queued
	// record PINS its pooled ~50 KB request/response body until it is marshaled, so
	// the cap directly bounds the audit body pool's working set: at the old 50000 a
	// slow-publish burst pinned ~5 GB of bodies (the dominant gw retained heap);
	// 10000 holds the pool near ~1 GB with the same measured spill/drop rate and a
	// ~70% lower GC pause. The buffer of last resort is NATS (disk) + the spill
	// sink, NOT this in-heap queue — keep it bounded, raise it only where a
	// memory-rich box wants extra absorption headroom.
	maxQueueSize = 10000

	// flushHighWater triggers an immediate flush once the buffer reaches
	// this depth, instead of waiting for the next ticker. Without it a
	// burst toward maxQueueSize within one flush interval would force
	// backpressure/spill even when the pipeline has drain capacity. Set
	// well below maxQueueSize so a spike flushes early.
	flushHighWater = 1000

	// Audit loss modes (AuditConfig.LossMode). Every default is zero-loss, because
	// durable audit is a product promise + a compliance requirement — the gateway
	// must never silently drop an audit record. Two distinct "defaults" exist and
	// must not be confused: the CONFIG default (config.defaults(), what an
	// unset AI_GATEWAY_AUDIT_LOSS_MODE resolves to) is "spillblock"; the
	// WithLossMode FALLBACK for an empty/unrecognised string is "block" (the most
	// conservative no-loss mode, so a config typo can never start dropping). The
	// lossy modes are an explicit opt-out for callers that do NOT need compliance
	// audit and prefer raw throughput.
	//   - spillblock (CONFIG default): like spill, but on a full spill channel it
	//     back-pressures the request goroutine until a slot frees instead of
	//     dropping (bounded by backpressureMaxWait → durable spill). Lossless up to
	//     disk-write success, throttling ingest to the disk rate; spills to disk
	//     before it ever blocks. Identical to spill in the normal regime; differs
	//     only at the extreme where spill would drop.
	//   - block (empty/unknown fallback): when the in-heap buffer is full, Enqueue
	//     BACK-PRESSURES the request path (bounded wait for the flush to free space;
	//     durable spill only if the pipeline is genuinely wedged past the wait).
	//     Admission self-throttles to the audit persistence rate; nothing is
	//     dropped. NATS (disk) is the burst buffer; the gw slows rather than loses.
	//   - spill: no wait — overflow is handed to the async spill worker (durable
	//     NDJSON, off the request path); a bounded drop only if the spill is also
	//     saturated. Higher throughput, bounded loss only under extreme overload.
	//   - drop: no wait — overflow is a counted bounded drop. Max throughput,
	//     audit-lossy. For non-compliance callers only.
	lossModeBlock      = "block"
	lossModeSpill      = "spill"
	lossModeDrop       = "drop"
	lossModeSpillBlock = "spillblock"

	// backpressureMaxWait bounds how long Enqueue back-pressures on a full buffer
	// before falling back to a durable spill (so a genuinely wedged pipeline — e.g.
	// NATS down — cannot hang a request goroutine forever). Under healthy NATS the
	// flush frees space in well under this, so the wait is short and the request
	// path simply throttles to the drain rate. backpressureTick re-checks for space
	// between flush wakeups.
	backpressureMaxWait = 10 * time.Second

	// consumerLinger bounds how long a consumer worker waits to fill a partial
	// batch before publishing it. Under load a batch reaches batchMaxCount long
	// before this fires (so publishes stay full-sized); when traffic is light it
	// bounds the per-record audit latency. 100 ms balances batch fullness against
	// tail latency on a near-idle pipeline.
	consumerLinger = 100 * time.Millisecond

	// spillFlushBytes / spillFlushInterval shape the spill worker's batching: it
	// accumulates marshaled record bytes up to spillFlushBytes (or
	// spillFlushInterval, whichever trips first) and writes them to the durable
	// spool in ONE large sequential write (ndjson.WriteBatch). Batching by BYTES,
	// not record count — audit records vary widely in size (a 50 KB body vs a tiny
	// metadata row), so a fixed count gives uneven writes; a fixed byte budget
	// gives uniform, IOPS-efficient large writes.
	//
	// Sized LARGE (128 MiB), not a few MiB: a flush is a disk write, and writing
	// frequently is itself the slow path — the per-record write saturated disk
	// write %util ~90% (~800 writes/s) under load. A big budget collapses that to a
	// handful of very large sequential writes. The trade is RAM: the accumulation
	// buffer holds up to spillFlushBytes of pending audit bytes, but that buffer is
	// cheap relative to dropping audit, and spill is the overflow path (it only
	// fills under burst). Tunable via AI_GATEWAY_AUDIT_SPILL_FLUSH_MB.
	spillFlushBytes    = 128 << 20 // 128 MiB
	spillFlushInterval = 250 * time.Millisecond

	// dropLogEvery throttles the buffer-overflow drop log: one line per this
	// many drops (the dropped_total metric stays exact). A per-record
	// stack-trace Error was itself a top allocator under the overload it
	// reports — see Writer.Enqueue.
	dropLogEvery = 2000
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
