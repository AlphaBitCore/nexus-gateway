package audit

import (
	"github.com/goccy/go-json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// Writer buffers audit records and publishes them to MQ in batches.
type Writer struct {
	producer mq.Producer
	logger   *slog.Logger
	queue    string
	metrics  *auditMetrics

	// frameMaxBytes, when > 0, packs marshaled records into newline-delimited
	// NDJSON frames bounded by this many bytes and publishes one NATS message
	// per frame — instead of one PublishAsync per record. The per-record op
	// count is the measured audit-drain bottleneck (records-per-frame× fewer
	// publishes recovers ~1.5× request RPS under load); the Hub splits each
	// frame back into records. 0 keeps the legacy one-message-per-record path.
	// Bound below the deployment's NATS max_payload and the Hub frame cap. Set
	// via WithFramePublish; gated OFF by default so it only engages once the
	// Hub is known to support framed messages (the consumer is backward-
	// compatible: a legacy 1-record message is just a 1-line frame).
	frameMaxBytes int

	// wireBinary selects the binary TLV audit wire (shared/transport/mq/binwire.go)
	// over the legacy NDJSON-of-JSON form. Set from NEXUS_AUDIT_WIRE=binary at
	// construction. Binary removes the gw-side JSON marshal + body base64 and the
	// Hub-side key matching + body base64 decode, and shrinks the message ~25–50%.
	// The Hub dual-reads (peeks the frame magic), so this can flip per-process. The
	// binary frame is length-prefixed records under a 1-byte magic, so it ALSO needs
	// frameMaxBytes>0 to engage the framed publish; the per-record fallback path
	// stays JSON regardless (it predates framing and is test/transport-only).
	wireBinary bool

	// Thing identity of the emitting ai-gateway instance. Stamped onto
	// every TrafficEventMessage so traffic_event.thing_id / thing_name
	// identify which gateway processed the request. Set via
	// WithThingIdentity at startup; empty in tests that don't wire it
	// (the consumer stores SQL NULL).
	thingID   string
	thingName string

	// SpillStore is the optional out-of-band body-storage backend. When
	// non-nil, recordToMessage uses spillstore.EmitBody to choose
	// between inline and spill based on the captured body size and the
	// runtime MaxInlineBodyBytes from payloadCapture. Nil keeps an
	// inline-only behaviour. Set via WithSpillStore.
	spill spillstore.SpillStore

	// ndjsonSpill is the durable on-disk fallback for whole audit records.
	// When the in-memory buffer is full after the backpressure window (or a
	// re-buffer on MQ failure cannot fit), Enqueue/publishRecord write the
	// record here instead of dropping it. Nil disables the fallback — then a
	// genuine overflow is a loud, counted drop. Set via WithNDJSONSpill.
	// Distinct from `spill` above, which stores oversized request/response
	// BODIES out-of-band; this stores entire records on transport failure.
	ndjsonSpill *sharedndjson.Writer

	// payloadCapture is the runtime payload-capture config snapshot
	// store. recordToMessage pulls MaxInlineBodyBytes from here on each
	// flush so admin-driven shadow updates take effect without a
	// service restart. Set via WithPayloadCaptureStore. Nil falls back
	// to payloadcapture.DefaultMaxInlineBodyBytes.
	payloadCapture *payloadcapture.Store

	// normalize, when non-nil, is invoked at recordToMessage time on
	// each captured (RequestBody / ResponseBody) direction to produce
	// the NormalizedPayload persisted on traffic_event_normalized.
	// Wired by ai-gateway main via shared/normcore.Registry. Nil keeps
	// the wire message without normalized fields (test / fallback).
	normalize NormalizeFn

	// reuseMetric, when non-nil, records the request-direction normalize
	// metrics for a payload reused from the request path without
	// re-running Normalize, so the normalize_total / payload_bytes series keep
	// moving on the reuse path (which bypasses the normalize bridge). Wired by
	// ai-gateway main via normcore.BuildReuseMetricFn. Nil = no-op.
	reuseMetric ReuseMetricFn

	// recCh is the bounded producer→consumer queue (cap = maxQueued). Enqueue is
	// the producer; N publishWorkers are the consumers. This IS the standard
	// bounded producer/multi-consumer pattern: a blocking send (block mode) is the
	// no-loss back-pressure, a non-blocking send (spill/drop modes) is the lossy
	// opt-out. Each queued record pins its pooled ~50 KB body until a worker
	// marshals it, so the cap bounds the audit body-pool working set.
	recCh chan *Record

	// maxQueued is recCh's capacity (the bounded-queue depth). Defaults to
	// maxQueueSize; set per deployment via WithMaxQueuedRecords. Read once at
	// NewWriter to size recCh.
	maxQueued int

	// lossMode selects the overflow policy when recCh is full (lossModeBlock
	// default | Spill | Drop). block = no-loss back-pressure (compliance default);
	// spill/drop = lossy opt-in. Set via WithLossMode from AuditConfig.LossMode.
	lossMode string

	// workers is the number of consumer goroutines draining recCh — one per
	// producer connection-pool member, so each worker publishes on its own NATS
	// connection with an independent ack barrier (the publishes pipeline instead of
	// serialising on a single flush loop). Set at NewWriter from the producer's
	// PoolSize.
	workers int

	stopCh    chan struct{}
	wg        sync.WaitGroup
	startOnce sync.Once

	// batchPathLogOnce logs (once) that the async-batch publish path engaged,
	// so prod logs prove the optimization is active.
	batchPathLogOnce sync.Once

	// dropLogCount rate-limits the overflow-drop log (one line per dropLogEvery
	// drops; dropped_total stays exact) — a stack-trace Error per dropped record
	// was itself a top allocator under the overload it reports.
	dropLogCount atomic.Uint64

	// spillLogCount rate-limits the spill-failure log, for the same reason: under
	// a spool-quota-full burst a per-failure Error (with its stack trace) is itself
	// a top allocator + CPU cost on the request path it was meant to protect.
	spillLogCount atomic.Uint64

	// spillCh hands overflow records from the request path to the async spill
	// worker. On a full in-heap buffer, Enqueue does a NON-BLOCKING send here and
	// returns immediately — the expensive marshal + NDJSON write happens on the
	// spill worker, never on the request goroutine. Buffered + non-blocking, so a
	// saturated spill worker degrades to a bounded, counted drop (the audit
	// side-path's last resort) without ever stalling a request. Drained on Close.
	spillCh chan *Record

	// spillRecoveryInterval, when > 0 (and ndjsonSpill + a batch producer are
	// wired), starts the background sweeper that replays sealed spool files back
	// into the MQ queue — the drain half of the spill-defer architecture, so a
	// record that overflowed to disk still reaches the queryable store. 0 disables
	// recovery (the spool is then a pure durable safety net drained out-of-band).
	// spillRecoveryPace throttles the sweep between files to yield the box to the
	// gateway's core request path. Both set via WithSpillRecovery.
	spillRecoveryInterval time.Duration
	spillRecoveryPace     time.Duration

	// spillFlush adapts the spill worker's flush size to measured disk-write
	// latency (replacing a fixed flush-bytes magic number). Set in NewWriter.
	spillFlush *adaptiveSpillFlush
}

// NewWriter creates an audit writer that publishes to the given MQ producer.
// If producer is nil, records are enqueued but discarded on flush (no-op mode).
// If reg is nil, MQ-pipeline metrics are silently skipped (test-only path).
func NewWriter(producer mq.Producer, queue string, reg *opsmetrics.Registry, logger *slog.Logger) *Writer {
	workers := 1
	if pooled, ok := producer.(pooledBatchProducer); ok {
		if n := pooled.PoolSize(); n > workers {
			workers = n
		}
	}
	// Buffer capacities adapt to AVAILABLE MEMORY rather than fixed magic numbers,
	// so a bigger box automatically absorbs deeper bursts and a smaller box scales
	// down — no config, and a machine swap can't silently collapse throughput.
	// WithMaxQueuedRecords still overrides the in-heap cap for an explicit operator
	// choice.
	recChCap, spillChCap := adaptiveBufferCaps()
	w := &Writer{
		producer:   producer,
		queue:      queue,
		logger:     logger,
		metrics:    newAuditMetrics(reg),
		maxQueued:  recChCap,
		lossMode:   lossModeBlock,
		workers:    workers,
		spillCh:    make(chan *Record, spillChCap),
		spillFlush: newAdaptiveSpillFlush(),
		stopCh:     make(chan struct{}),
		// Binary TLV wire is the proven-optimal default (eliminates the Hub-side JSON
		// decode + gw base64/marshal, shrinks the NATS message 25–50%); the Hub
		// dual-reads, so this is safe to default on. NEXUS_AUDIT_WIRE=json opts back
		// out (the legacy path, kept until it is deleted).
		wireBinary: !strings.EqualFold(strings.TrimSpace(os.Getenv("NEXUS_AUDIT_WIRE")), "json"),
	}
	return w
}

// WithSpillStore wires an out-of-band body backend. Bodies whose
// captured size exceeds the runtime MaxInlineBodyBytes are written via
// spillstore.Put and the audit row keeps a SpillRef; smaller bodies
// stay inline. Returns the receiver for chaining.
func (w *Writer) WithSpillStore(store spillstore.SpillStore) *Writer {
	w.spill = store
	return w
}

// WithPayloadCaptureStore wires the runtime payload-capture config
// snapshot. The audit writer reads MaxInlineBodyBytes from this store
// on every record flush, so admin-driven shadow updates take effect
// without a restart. Returns the receiver for chaining.
func (w *Writer) WithPayloadCaptureStore(s *payloadcapture.Store) *Writer {
	w.payloadCapture = s
	return w
}

// WithThingIdentity stamps the emitting ai-gateway's Thing ID and
// human-readable name onto every TrafficEventMessage. Persisted as
// traffic_event.thing_id / thing_name. Returns the receiver for chaining.
//
// Must be called during startup before any Enqueue / flushLoop runs;
// mutates w.thingID / w.thingName without a lock, matching the
// WithSpillStore / WithPayloadCaptureStore startup-only convention.
func (w *Writer) WithThingIdentity(id, name string) *Writer {
	w.thingID = id
	w.thingName = name
	return w
}

// NormalizeFn is the closure ai-gateway main wires to invoke
// shared/normalize on captured request/response bodies. Returns the
// marshalled NormalizedPayload (or nil on protocol-mismatch), the
// status ("ok" / "partial" / "failed"), and an error reason for the
// failed/partial path. The audit Writer is intentionally agnostic
// about the normalize package — it accepts bytes in and produces wire
// bytes out, so this package keeps building when shared/normalize is
// not wired (tests, no-op deployments).
//
// adapterType is the wire-format key ("openai", "anthropic", "gemini",
// "vertex", "bedrock", ...) selected by routing; it is the routing
// signal for shared/normalize's Registry. Operator-friendly provider
// names are intentionally NOT used as the routing key.
type NormalizeFn func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (raw json.RawMessage, status, errReason string)

// WithNormalizer wires a normalize closure. The audit write path no longer
// persists a normalized projection (the control plane recomputes it at view
// time from the action-governed raw body), so the closure is retained only
// as the seam ai-gateway main uses to keep the normalize metrics series wired;
// recordToMessage does not invoke it.
func (w *Writer) WithNormalizer(fn NormalizeFn) *Writer {
	w.normalize = fn
	return w
}

// ReuseMetricFn records the request-direction normalize metrics for a payload
// reused verbatim from the request goroutine (no re-Normalize). The Writer stays
// agnostic of the normalize package; the concrete is wired from
// normcore.BuildReuseMetricFn. protocol/kind/payloadLen come from the reused
// payload; the status is always "ok" (a failed/partial normalize leaves no reuse
// bytes and falls back to re-Normalize).
type ReuseMetricFn func(protocol, kind, direction string, payloadLen int)

// WithReuseMetric wires the reuse-metric closure so recordToMessage keeps the
// normalize metric series moving on the reuse path (which bypasses the bridge).
func (w *Writer) WithReuseMetric(fn ReuseMetricFn) *Writer {
	w.reuseMetric = fn
	return w
}
