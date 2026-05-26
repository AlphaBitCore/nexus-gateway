package consumer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// dlqTestLogger discards output so test runs don't spam stderr.
func dlqTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newDlqTestWriter wires a TrafficEventWriter against the mock pool with no
// MQ consumer and no opsmetrics. Sufficient for nakOrDLQ unit tests.
func newDlqTestWriter(t *testing.T, mock pgxmock.PgxPoolIface) *TrafficEventWriter {
	t.Helper()
	return newTrafficEventWriter(mock, nil, TrafficEventWriterConfig{}, dlqTestLogger(), nil)
}

// TestNakOrDLQ_BelowThresholdNaks pins the canonical retry path: a message
// with NumDelivered=1 just gets Nak'd — broker retries, no DLQ.
func TestNakOrDLQ_BelowThresholdNaks(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// NO INSERT expected — below threshold means DLQ path is not taken.

	var nakCalls int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{ID: "evt-1"},
			msg: &mq.Message{
				NumDelivered: 1,
				Subject:      "nexus.event.gateway",
				Nak:          func() error { atomic.AddInt32(&nakCalls, 1); return nil },
			},
		},
	}

	w := newDlqTestWriter(t, mock)
	w.nakOrDLQ(context.Background(), items, errors.New("transient"))

	if got := atomic.LoadInt32(&nakCalls); got != 1 {
		t.Errorf("Nak called %d times, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNakOrDLQ_AtThresholdInsertsDLQAndAcks pins the dead-letter path: a
// message at the redelivery cap is INSERTed into traffic_event_dlq and
// ACKed (no further redelivery).
func TestNakOrDLQ_AtThresholdInsertsDLQAndAcks(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`INSERT INTO traffic_event_dlq`).
		WithArgs(
			"evt-2",
			"nexus.event.compliance",
			pgxmock.AnyArg(), // raw bytes
			redeliveryThresholdAttempts,
			pgxmock.AnyArg(), // last_error string ptr
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	var ackCalls, nakCalls int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{ID: "evt-2"},
			msg: &mq.Message{
				NumDelivered: redeliveryThresholdAttempts,
				Subject:      "nexus.event.compliance",
				Data:         []byte(`{"id":"evt-2"}`),
				Ack:          func() error { atomic.AddInt32(&ackCalls, 1); return nil },
				Nak:          func() error { atomic.AddInt32(&nakCalls, 1); return nil },
			},
		},
	}

	w := newDlqTestWriter(t, mock)
	w.nakOrDLQ(context.Background(), items, errors.New("permanent: 23505 duplicate key"))

	if got := atomic.LoadInt32(&ackCalls); got != 1 {
		t.Errorf("Ack called %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&nakCalls); got != 0 {
		t.Errorf("Nak called %d times, want 0 (DLQ path acks)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNakOrDLQ_DLQInsertFailureFallsBackToNak pins the fallback path: if
// the DLQ insert itself errors, the consumer falls back to Nak so the
// broker keeps trying — better to retry forever than silently drop.
func TestNakOrDLQ_DLQInsertFailureFallsBackToNak(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`INSERT INTO traffic_event_dlq`).
		WillReturnError(errors.New("conn refused"))

	var ackCalls, nakCalls int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{ID: "evt-3"},
			msg: &mq.Message{
				NumDelivered: redeliveryThresholdAttempts + 2,
				Subject:      "nexus.event.gateway",
				Ack:          func() error { atomic.AddInt32(&ackCalls, 1); return nil },
				Nak:          func() error { atomic.AddInt32(&nakCalls, 1); return nil },
			},
		},
	}

	w := newDlqTestWriter(t, mock)
	w.nakOrDLQ(context.Background(), items, errors.New("permanent"))

	if got := atomic.LoadInt32(&ackCalls); got != 0 {
		t.Errorf("Ack called %d times, want 0 (DLQ failure should not ack)", got)
	}
	if got := atomic.LoadInt32(&nakCalls); got != 1 {
		t.Errorf("Nak called %d times, want 1 (fallback)", got)
	}
}

// TestNakOrDLQ_MixedBatch pins the per-item independence: one item below
// the cap, one at the cap. Below gets Nak, at-cap gets DLQ + Ack.
func TestNakOrDLQ_MixedBatch(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`INSERT INTO traffic_event_dlq`).
		WithArgs(
			"evt-cap",
			"nexus.event.gateway",
			pgxmock.AnyArg(),
			redeliveryThresholdAttempts+1,
			pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	var fastAck, fastNak, capAck, capNak int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{ID: "evt-fast"},
			msg: &mq.Message{
				NumDelivered: 1,
				Subject:      "nexus.event.agent",
				Ack:          func() error { atomic.AddInt32(&fastAck, 1); return nil },
				Nak:          func() error { atomic.AddInt32(&fastNak, 1); return nil },
			},
		},
		{
			event: TrafficEventMessage{ID: "evt-cap"},
			msg: &mq.Message{
				NumDelivered: redeliveryThresholdAttempts + 1,
				Subject:      "nexus.event.gateway",
				Ack:          func() error { atomic.AddInt32(&capAck, 1); return nil },
				Nak:          func() error { atomic.AddInt32(&capNak, 1); return nil },
			},
		},
	}

	w := newDlqTestWriter(t, mock)
	w.nakOrDLQ(context.Background(), items, errors.New("test"))

	if got := atomic.LoadInt32(&fastAck); got != 0 {
		t.Errorf("fast item Ack=%d, want 0", got)
	}
	if got := atomic.LoadInt32(&fastNak); got != 1 {
		t.Errorf("fast item Nak=%d, want 1", got)
	}
	if got := atomic.LoadInt32(&capAck); got != 1 {
		t.Errorf("cap item Ack=%d, want 1", got)
	}
	if got := atomic.LoadInt32(&capNak); got != 0 {
		t.Errorf("cap item Nak=%d, want 0", got)
	}
}

// TestErrString covers the small helper: nil produces empty, error
// produces .Error() output. Trivial but exercises the structured-log
// helper path that nakOrDLQ uses.
func TestErrString(t *testing.T) {
	if got := errString(nil); got != "" {
		t.Errorf("errString(nil) = %q, want empty", got)
	}
	if got := errString(errors.New("boom")); got != "boom" {
		t.Errorf("errString(err) = %q, want boom", got)
	}
}
