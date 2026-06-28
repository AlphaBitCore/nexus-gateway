// traffic_flush.go — the TrafficEventWriter's message-to-batch decode dispatch
// (handleMessage) and the two-tier flush path: the batched fast path
// (flush/flushBatch) and the per-item isolation fallback (flushItem +
// resolveItemInsertErr) that keeps one poison row from dropping a whole batch.
// Split from traffic.go, which owns the writer struct, construction, and the
// consume loop wiring.
package consumer

import (
	"context"
	"fmt"

	json "github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// handleMessage is the per-message handler passed to mq.Consumer.Consume.
// Returns nil if the message is a poison pill (already acked inline); returns
// mq.ErrDeferAck if the message is buffered and will be acked after the batch
// flush; returns a non-sentinel error to trigger auto-nak by the MQ driver.
func (w *TrafficEventWriter) handleMessage(queue string, batch *BatchAccumulator[pendingTrafficMessage], msg *mq.Message) error {
	if w.consumedTotal != nil {
		w.consumedTotal.With(queue).Inc()
	}

	// A message is an NDJSON frame of one or more records (the gateway batches
	// records into one publish to cut NATS op-count). A legacy single-record
	// message has no interior newline → exactly one line, so both shapes flow
	// through the same path. The frame owns the single ack/nak of the underlying
	// message, settled only after every record it carried is durably resolved.
	// Dual-read: a binary frame (binwire magic) is split into length-prefixed
	// records decoded via the binary codec; anything else is the legacy NDJSON-of-
	// JSON form. Both produce the same TrafficEventMessage, so the batch/flush path
	// below is identical — and an in-flight JSON message drains cleanly after a gw
	// flips to the binary wire.
	binaryFrame := isBinaryFrame(msg.Data)
	var lines [][]byte
	if binaryFrame {
		lines = splitBinaryFrame(msg.Data)
	} else {
		lines = splitFrame(msg.Data)
	}
	if len(lines) == 0 {
		return msg.Ack()
	}
	frame := newFrameAck(msg, len(lines))

	deferred := 0
	for _, line := range lines {
		var evt TrafficEventMessage
		var derr error
		if binaryFrame {
			evt, derr = decodeBinaryRecordSafe(line)
		} else {
			derr = json.Unmarshal(line, &evt)
		}
		if derr != nil {
			w.logger.Error("deserialize failed, dropping record",
				"queue", queue, "error", derr)
			if w.errorsTotal != nil {
				w.errorsTotal.With("deserialize").Inc()
			}
			// Poison record: permanently skipped, but it still counts toward the
			// frame so the message can ack once its siblings are done.
			frame.resolve(false, 0)
			continue
		}

		if err := batch.Add(pendingTrafficMessage{event: evt, msg: msg, raw: line, frame: frame}); err != nil {
			// Synchronous flush failure (batch hit maxSize and flush errored).
			// flush already resolved the items it held; the records of THIS frame
			// not yet handed to the batch would otherwise never settle, so force
			// the frame to nak — the whole frame redelivers and dedup (by request
			// id) drops the records that did commit. forceNak is idempotent with
			// the in-flight resolves via the frame's done guard.
			frame.forceNak()
			return err
		}
		deferred++
	}
	if deferred == 0 {
		// Every record was a poison pill (or the frame was empty): the frame has
		// already settled (acked) inline, so report it handled inline — matching
		// the legacy single-record poison-pill contract — rather than deferring.
		return nil
	}
	// Hand the single ack/nak off to the batch flush path via the frame.
	return mq.ErrDeferAck
}

// flush attempts the whole batch in one tx (the fast path); if that tx fails as
// a unit it falls back to per-item reprocessing so a single bad row cannot drop
// up to 99 healthy events. flush itself returns nil because every item
// is fully resolved (acked / nak'd / dead-lettered) by one of the two paths —
// returning a non-nil error would make the MQ driver redundantly nak a message
// the per-item path already handled.
func (w *TrafficEventWriter) flush(ctx context.Context, items []pendingTrafficMessage) error {
	if w.batchSizeHist != nil {
		w.batchSizeHist.With().Observe(float64(len(items)))
	}

	if err := w.flushBatch(ctx, items); err != nil {
		// One poison/oversize row aborts the whole pgx.Batch tx, so the batch is
		// rolled back un-acked. Re-run each item in its own tx: healthy rows
		// commit + ack, the offending row is isolated (poison → ack-to-skip;
		// transient → nak/DLQ) instead of taking the batch down with it.
		w.logger.Warn("flush: batch insert failed, isolating per-item",
			"error", err, "count", len(items))
		for i := range items {
			w.flushItem(ctx, items[i])
		}
	}
	return nil
}

// flushBatch runs the batched fast path in a single transaction. On any fatal
// failure it returns the wrapped error WITHOUT acking or naking — the caller
// (flush) falls back to per-item reprocessing which owns the ack/nak decision.
// On success it acks the whole batch.
func (w *TrafficEventWriter) flushBatch(ctx context.Context, items []pendingTrafficMessage) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.countFlushErr("db_begin")
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// COPY fast path (NEXUS_HUB_TRAFFIC_COPY=1): bulk-load via a per-tx staging
	// temp table + INSERT…SELECT…ON CONFLICT. On any error this returns and flush
	// falls back to per-item reprocessing on the pgx.Batch path (flushItem), so the
	// poison-isolation / no-strand guarantee is preserved. Default off → the proven
	// pgx.Batch path. The method-value indirection keeps both paths one branch.
	insertEvents := w.insertTrafficEvents
	insertPayloads := w.insertPayloads
	if trafficCopyEnabled {
		insertEvents = w.insertTrafficEventsCopy
		insertPayloads = w.insertPayloadsCopy
	}

	if err := insertEvents(ctx, tx, items); err != nil {
		w.countFlushErr("db_insert")
		return fmt.Errorf("insert traffic_event: %w", err)
	}

	if err := insertPayloads(ctx, tx, items); err != nil {
		w.countFlushErr("db_insert_payload")
		return fmt.Errorf("insert traffic_event_payload: %w", err)
	}

	// Normalized payloads are an independent sidecar. Each sidecar row runs
	// inside its OWN savepoint (see insertNormalizedPayloads), so a failure
	// here — including a jsonb encoding error (22P05) — rolls back only that
	// savepoint and leaves the outer tx committable. The raw traffic_event +
	// traffic_event_payload rows therefore survive even when normalization
	// fails. A returned error is a non-poison sidecar failure: it is logged +
	// counted (the normalize-backfill job heals the gap) but never rolls the
	// raw batch.
	if err := w.insertNormalizedPayloads(ctx, tx, items); err != nil {
		w.logger.Warn("flush: insert traffic_event_normalized failed (raw rows still committed)",
			"error", err, "count", len(items))
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_insert_normalized").Inc()
		}
	}

	if err := tx.Commit(ctx); err != nil {
		w.countFlushErr("db_commit")
		return fmt.Errorf("commit tx: %w", err)
	}

	w.ackAll(items)
	if w.flushTotal != nil {
		w.flushTotal.With("success").Inc()
	}

	w.logger.Debug("flushed traffic events", "count", len(items))
	return nil
}

// flushItem reprocesses a single message in its own transaction, used only when
// the batched fast path failed. It guarantees the message is resolved exactly
// once: a permanent encoding poison (typed SQLSTATE 22021 / 22P05) is acked to
// skip; any other failure is nak'd / dead-lettered; success commits + acks.
func (w *TrafficEventWriter) flushItem(ctx context.Context, pm pendingTrafficMessage) {
	single := []pendingTrafficMessage{pm}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.countFlushErr("db_begin")
		w.nakOrDLQ(ctx, single, fmt.Errorf("begin tx: %w", err))
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := w.insertTrafficEvents(ctx, tx, single); err != nil {
		w.countFlushErr("db_insert")
		w.resolveItemInsertErr(ctx, single, fmt.Errorf("insert traffic_event: %w", err))
		return
	}

	if err := w.insertPayloads(ctx, tx, single); err != nil {
		w.countFlushErr("db_insert_payload")
		w.resolveItemInsertErr(ctx, single, fmt.Errorf("insert traffic_event_payload: %w", err))
		return
	}

	if err := w.insertNormalizedPayloads(ctx, tx, single); err != nil {
		w.logger.Warn("flush: insert traffic_event_normalized failed (raw row still committed)",
			"error", err, "id", pm.event.ID)
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_insert_normalized").Inc()
		}
	}

	if err := tx.Commit(ctx); err != nil {
		w.countFlushErr("db_commit")
		w.nakOrDLQ(ctx, single, fmt.Errorf("commit tx: %w", err))
		return
	}

	w.ackAll(single)
	if w.flushTotal != nil {
		w.flushTotal.With("success").Inc()
	}
}

// resolveItemInsertErr decides the fate of a single row whose insert failed in
// the per-item path. A permanent NUL/encoding error (typed 22021 / 22P05) can
// never succeed on retry, so the row is acked to skip (the error log is the
// audit trail); every other error is nak'd / dead-lettered for redelivery.
func (w *TrafficEventWriter) resolveItemInsertErr(ctx context.Context, single []pendingTrafficMessage, err error) {
	if isJSONNulPoison(err) {
		w.logger.Warn("flush: permanent encoding error, acking to skip poison row",
			"id", single[0].event.ID, "error", err)
		if w.errorsTotal != nil {
			w.errorsTotal.With("db_insert_poison").Inc()
		}
		w.ackAll(single)
		return
	}
	w.nakOrDLQ(ctx, single, err)
}

// countFlushErr increments the flush error counters for the given error_type.
func (w *TrafficEventWriter) countFlushErr(errorType string) {
	if w.flushTotal != nil {
		w.flushTotal.With("error").Inc()
	}
	if w.errorsTotal != nil {
		w.errorsTotal.With(errorType).Inc()
	}
}

func (w *TrafficEventWriter) ackAll(items []pendingTrafficMessage) {
	for _, pm := range items {
		// Resolve this record's slot in its frame; the underlying NATS message is
		// acked once, only when the last record of the frame resolves. A nil frame
		// (a directly-constructed single record) acks the message directly.
		if pm.frame != nil {
			pm.frame.resolve(false, 0)
			continue
		}
		if err := pm.msg.Ack(); err != nil {
			w.logger.Warn("ack failed", "error", err)
		}
	}
}
