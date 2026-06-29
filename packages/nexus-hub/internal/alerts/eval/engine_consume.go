package alerteval

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/consumer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// handleMQMessage processes one MQ event. Returning nil signals the
// shared/mq consumer to ack the message; returning ErrDeferAck would
// hand ack ownership back to us. Pre-fix the handler called
// `defer msg.Ack()` which double-acked every message — the natsmq
// consumer still autoacks on nil, then JetStream rejected our second
// ack with "nats: message was already acknowledged" and that warning
// flooded the Hub log under load. Now we simply return nil and let
// the consumer ack once.
func (e *Engine) handleMQMessage(subject string, msg *mq.Message) error {
	source, ok := subjectToSource(subject)
	if !ok {
		return nil
	}

	// Admin audit is a distinct, low-volume message type (JSON only). Decode it
	// the simple way; a per-record failure skips THAT record, the frame still
	// acks once (return nil).
	if source == SourceAdminAudit {
		for _, line := range splitFrameLines(msg.Data) {
			evt, err := decodeEvent(source, line)
			if err != nil {
				e.warnDecodeFail(subject, err)
				continue
			}
			e.dispatchEvent(source, evt)
		}
		return nil
	}

	// Traffic events: the gateway publishes a batched frame in the binary TLV
	// wire (default) or legacy NDJSON-of-JSON. DecodeAlertFrame dual-reads both,
	// decodes each record's body METADATA-only (zero-copy — alert rules never read
	// the payload), and reuses a pooled decode target so a steady-state frame
	// allocates nothing per record. evt is reused across records: dispatchEvent
	// runs the aggregators synchronously and they copy the scalars they need into
	// their windows, so neither evt nor the pooled Traffic pointer is retained.
	var evt Event
	consumer.DecodeAlertFrame(msg.Data,
		func(tm *consumer.AlertView) {
			evt = Event{Kind: EventTraffic, Source: source, Timestamp: tm.Timestamp, Traffic: tm}
			e.dispatchEvent(source, &evt)
		},
		func(err error) { e.warnDecodeFail(subject, err) },
	)
	return nil
}

// warnDecodeFail logs a decode failure at most once per decodeFailLogEvery so a
// wire/format mismatch under full traffic cannot flood the log (and the disk it
// shares with the audit drain). The drop count is exact; only the log is throttled.
func (e *Engine) warnDecodeFail(subject string, err error) {
	if n := e.decodeFails.Add(1); n%decodeFailLogEvery == 1 {
		e.logger.Warn("alerteval decode failed; dropping record (throttled)",
			"subject", subject, "error", err, "droppedSoFar", n)
	}
}

// dispatchEvent routes one decoded event to every registered Aggregator whose
// declared sources include the event's source.
func (e *Engine) dispatchEvent(source EventSource, evt *Event) {
	// Lock-free hot path: load the precomputed, source-masked dispatch table
	// (rebuilt only when Register mutates the aggregator set) and range it. No
	// per-event slice snapshot, no Sources() slice allocation, no runtimes map
	// lookup. nil table (no aggregators registered yet) → no-op.
	tbl := e.dispatchTable.Load()
	if tbl == nil {
		return
	}
	bit := sourceBit(source)
	for i := range *tbl {
		if en := &(*tbl)[i]; en.sourceMask&bit != 0 {
			en.agg.OnEvent(en.rt, evt)
		}
	}
}
