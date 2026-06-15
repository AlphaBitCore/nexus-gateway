package consumer

import (
	"context"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// redeliveryThresholdAttempts is the delivery count at which the consumer
// stops Nak-ing and dead-letters the message instead. It is deliberately set
// STRICTLY BELOW the broker's MaxDeliver (5, in shared/transport/mq) so the
// dead-letter path runs on a non-final delivery while delivery budget still
// remains: if the DB-backed insertDLQ itself fails (DB outage) the message can
// fall through to the on-disk DLQ and, failing even that, be Nak'd for one more
// attempt rather than being purged. Equating the threshold with MaxDeliver
// (the previous value, 5) meant the DLQ was only ever attempted on the final
// delivery, so a DB-outage DLQ failure had no budget left and the row was lost
// — JetStream removes a message the instant MaxDeliver is exhausted; the 6h
// MaxAge does NOT grant a grace window past exhaustion.
const redeliveryThresholdAttempts = 3

// Compile-time assertion that the dead-letter threshold stays STRICTLY BELOW
// the broker's delivery cap (shared/transport/mq.MaxDeliver). If a future edit
// raises redeliveryThresholdAttempts to or above mq.MaxDeliver, the untyped
// constant below goes negative and the uint conversion fails to compile —
// turning the previously implicit "threshold < MaxDeliver" invariant (two
// unlinked magic 5s) into an enforced one. mq.MaxDeliver - threshold -
// 1 >= 0  ⟺  threshold <= MaxDeliver - 1  ⟺  threshold < MaxDeliver.
const _ = uint(mq.MaxDeliver - redeliveryThresholdAttempts - 1)

// redeliveryBackoffBase / redeliveryBackoffMax bound the NakWithDelay backoff.
// A bare Nak re-delivers as fast as the broker can, burning the whole
// MaxDeliver budget in ~25-30s (it bypasses the 30s AckWait); a graduated
// delay makes the budget span the outage so a multi-minute DB failover
// recovers normally instead of exhausting deliveries and purging the message.
const (
	redeliveryBackoffBase = 15 * time.Second
	redeliveryBackoffMax  = 2 * time.Minute
)

// redeliveryDelay returns the per-message redelivery backoff, scaling with the
// delivery count and capped at redeliveryBackoffMax.
func redeliveryDelay(numDelivered uint64) time.Duration {
	if numDelivered < 1 {
		numDelivered = 1
	}
	d := redeliveryBackoffBase * time.Duration(numDelivered)
	if d > redeliveryBackoffMax {
		d = redeliveryBackoffMax
	}
	return d
}

// nakOrDLQ routes each item to either delayed Nak (let the broker redeliver
// later) or dead-letter (when the message has hit the redelivery cap). lastErr
// is stamped on the DLQ row's last_error column so operators see why the row
// got there without grep'ing logs by event ID. ctx is the parent flush ctx.
func (w *TrafficEventWriter) nakOrDLQ(ctx context.Context, items []pendingTrafficMessage, lastErr error) {
	for _, pm := range items {
		if pm.msg.NumDelivered >= redeliveryThresholdAttempts {
			w.deadLetter(ctx, pm, lastErr)
			continue
		}
		w.nakWithBackoff(pm)
	}
}

// deadLetter persists a message that has exhausted its retry budget so it is
// never silently dropped, then Acks it. Two sinks, in order:
//
//  1. DB-backed traffic_event_dlq (preferred — surfaces in the admin DLQ UI
//     for inspect/retry).
//  2. On-disk JSON-Lines DLQ — used ONLY when (1) fails, i.e. the DB is
//     unreachable. This is the durability guarantee for a full DB outage: the
//     raw billing/audit bytes land on disk for later replay instead of being
//     Nak'd into MaxDeliver exhaustion and purged.
//
// If BOTH sinks fail, fall back to a delayed Nak so the broker re-attempts —
// the on-disk write gets another chance on redelivery rather than dropping a
// message we have no record of.
func (w *TrafficEventWriter) deadLetter(ctx context.Context, pm pendingTrafficMessage, lastErr error) {
	if err := w.insertDLQ(ctx, pm, lastErr); err == nil {
		w.ackDeadLettered(pm)
		if w.dlqInsertedTotal != nil {
			w.dlqInsertedTotal.With(pm.msg.Subject).Inc()
		}
		w.logger.Warn("message moved to traffic_event_dlq after redelivery cap",
			"msgId", pm.event.ID, "subject", pm.msg.Subject,
			"deliveries", pm.msg.NumDelivered, "lastError", errString(lastErr))
		return
	} else {
		w.logger.Error("dlq insert failed; trying on-disk dlq",
			"msgId", pm.event.ID, "subject", pm.msg.Subject, "error", err)
		if w.errorsTotal != nil {
			w.errorsTotal.With("dlq_insert").Inc()
		}
	}

	if w.diskDLQ != nil {
		rec := diskDLQRecord{
			MsgID:         pm.event.ID,
			Subject:       pm.msg.Subject,
			Payload:       pm.msg.Data,
			DeliveryCount: int(pm.msg.NumDelivered),
			LastError:     errString(lastErr),
			WrittenAt:     time.Now().UTC(),
		}
		if err := w.diskDLQ.append(rec); err == nil {
			w.ackDeadLettered(pm)
			if w.diskDLQTotal != nil {
				w.diskDLQTotal.With(pm.msg.Subject).Inc()
			}
			w.logger.Warn("message persisted to on-disk dlq after db dlq failure (replay required)",
				"msgId", pm.event.ID, "subject", pm.msg.Subject,
				"file", w.diskDLQ.path(), "lastError", errString(lastErr))
			return
		} else {
			w.logger.Error("on-disk dlq write failed; falling back to delayed nak",
				"msgId", pm.event.ID, "subject", pm.msg.Subject, "error", err)
			if w.errorsTotal != nil {
				w.errorsTotal.With("disk_dlq").Inc()
			}
		}
	}

	w.nakWithBackoff(pm)
}

// ackDeadLettered Acks a message that has been durably captured in a DLQ sink.
func (w *TrafficEventWriter) ackDeadLettered(pm pendingTrafficMessage) {
	if err := pm.msg.Ack(); err != nil {
		w.logger.Warn("dlq ack failed", "error", err)
	}
}

// nakWithBackoff rejects a message for delayed redelivery, preferring
// NakWithDelay so the broker honours the backoff. Falls back to a bare Nak on
// transports that do not expose a per-message delay.
func (w *TrafficEventWriter) nakWithBackoff(pm pendingTrafficMessage) {
	if pm.msg.NakWithDelay != nil {
		delay := redeliveryDelay(pm.msg.NumDelivered)
		if err := pm.msg.NakWithDelay(delay); err != nil {
			w.logger.Warn("nak-with-delay failed", "error", err)
		}
		return
	}
	if err := pm.msg.Nak(); err != nil {
		w.logger.Warn("nak failed", "error", err)
	}
}

// insertDLQ writes a single message to traffic_event_dlq. Best-effort: the
// caller decides what to do with the error (typically: fall back to Nak so
// the broker keeps trying instead of silently dropping the message).
func (w *TrafficEventWriter) insertDLQ(ctx context.Context, pm pendingTrafficMessage, lastErr error) error {
	const sql = `
INSERT INTO traffic_event_dlq
    (msg_id, subject, payload, delivery_count, last_error, first_seen_at)
VALUES ($1, $2, $3, $4, $5, $6)
`
	var errPtr *string
	if lastErr != nil {
		s := lastErr.Error()
		errPtr = &s
	}
	// first_seen_at = the message's original publish/first-delivery time from
	// JetStream metadata (mq.Consumer lifts meta.Timestamp onto msg.Timestamp).
	// Passing it explicitly — instead of letting it fall to DEFAULT NOW() like
	// dlq_inserted_at — makes the (dlq_inserted_at - first_seen_at) gap the real
	// time the message spent being redelivered before it was dead-lettered. The
	// core-NATS topic path carries no broker timestamp and sets a zero time;
	// fall back to NOW() (via NULL → DEFAULT) in that case so the column is never
	// the Go zero epoch.
	var firstSeen any
	if !pm.msg.Timestamp.IsZero() {
		firstSeen = pm.msg.Timestamp.UTC()
	}
	if firstSeen == nil {
		// No broker timestamp available: keep first_seen_at = dlq_inserted_at
		// (both DEFAULT NOW()) so the column stays meaningful rather than 0001.
		const sqlNoTS = `
INSERT INTO traffic_event_dlq
    (msg_id, subject, payload, delivery_count, last_error)
VALUES ($1, $2, $3, $4, $5)
`
		_, err := w.pool.Exec(ctx, sqlNoTS,
			pm.event.ID,
			pm.msg.Subject,
			pm.msg.Data,
			int(pm.msg.NumDelivered),
			errPtr,
		)
		return err
	}
	_, err := w.pool.Exec(ctx, sql,
		pm.event.ID,
		pm.msg.Subject,
		pm.msg.Data,
		int(pm.msg.NumDelivered),
		errPtr,
		firstSeen,
	)
	return err
}

// errString returns "" for nil errors so structured logs stay clean.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
