package consumer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// dlqTestLogger discards output so test runs don't spam stderr.
func dlqTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newDlqTestWriter wires a TrafficEventWriter against the mock pool with no
// MQ consumer and no opsmetrics. The on-disk DLQ is rooted at a per-test temp
// dir so the disk-fallback path is exercised in isolation.
func newDlqTestWriter(t *testing.T, mock pgxmock.PgxPoolIface) *TrafficEventWriter {
	t.Helper()
	w := newTrafficEventWriter(mock, nil, TrafficEventWriterConfig{}, dlqTestLogger(), nil)
	w.diskDLQ = newDiskDLQ(t.TempDir())
	return w
}

// delayCapture records the NakWithDelay backoff a message was nak'd with.
type delayCapture struct {
	calls int32
	last  atomic.Int64 // nanoseconds of the last delay
}

func (d *delayCapture) fn() func(time.Duration) error {
	return func(delay time.Duration) error {
		atomic.AddInt32(&d.calls, 1)
		d.last.Store(int64(delay))
		return nil
	}
}

// TestNakOrDLQ_BelowThresholdNaksWithDelay pins the canonical retry path: a
// message below the redelivery threshold is nak'd with a positive backoff
// delay (NakWithDelay), NOT a bare Nak and NOT dead-lettered.
func TestNakOrDLQ_BelowThresholdNaksWithDelay(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// NO INSERT expected — below threshold means the DLQ path is not taken.

	var bareNak int32
	var cap delayCapture
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{ID: "evt-1"},
			msg: &mq.Message{
				NumDelivered: redeliveryThresholdAttempts - 1,
				Subject:      "nexus.event.gateway",
				Nak:          func() error { atomic.AddInt32(&bareNak, 1); return nil },
				NakWithDelay: cap.fn(),
			},
		},
	}

	w := newDlqTestWriter(t, mock)
	w.nakOrDLQ(context.Background(), items, errors.New("transient"))

	if got := atomic.LoadInt32(&cap.calls); got != 1 {
		t.Errorf("NakWithDelay called %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&bareNak); got != 0 {
		t.Errorf("bare Nak called %d times, want 0 (must use NakWithDelay)", got)
	}
	if got := time.Duration(cap.last.Load()); got <= 0 {
		t.Errorf("NakWithDelay delay = %v, want > 0 (must back off, not re-deliver instantly)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNakOrDLQ_AtThresholdInsertsDLQAndAcks pins the dead-letter path: a
// message that has reached the redelivery threshold (which is strictly below
// MaxDeliver, so this is a NON-final delivery) is INSERTed into
// traffic_event_dlq and ACKed.
func TestNakOrDLQ_AtThresholdInsertsDLQAndAcks(t *testing.T) {
	if redeliveryThresholdAttempts >= 5 {
		t.Fatalf("redeliveryThresholdAttempts=%d must be < MaxDeliver(5) so the DLQ path runs while budget remains",
			redeliveryThresholdAttempts)
	}

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
				NakWithDelay: func(time.Duration) error { atomic.AddInt32(&nakCalls, 1); return nil },
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

// TestNakOrDLQ_StampsFirstSeenFromMsgTimestamp pins the DLQ-timestamp fix:
// when the JetStream message carries its original publish/first-delivery time
// (msg.Timestamp), insertDLQ passes it as first_seen_at (the 6-arg INSERT) so
// the (dlq_inserted_at - first_seen_at) gap measures real redelivery latency
// rather than collapsing to zero (both DEFAULT NOW()).
func TestNakOrDLQ_StampsFirstSeenFromMsgTimestamp(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	firstSeen := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	mock.ExpectExec(`INSERT INTO traffic_event_dlq`).
		WithArgs(
			"evt-ts",
			"nexus.event.gateway",
			pgxmock.AnyArg(), // raw bytes
			redeliveryThresholdAttempts,
			pgxmock.AnyArg(), // last_error ptr
			firstSeen,        // first_seen_at = original publish time
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	var ackCalls int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{ID: "evt-ts"},
			msg: &mq.Message{
				NumDelivered: redeliveryThresholdAttempts,
				Subject:      "nexus.event.gateway",
				Data:         []byte(`{"id":"evt-ts"}`),
				Timestamp:    firstSeen,
				Ack:          func() error { atomic.AddInt32(&ackCalls, 1); return nil },
				Nak:          func() error { return nil },
				NakWithDelay: func(time.Duration) error { return nil },
			},
		},
	}

	w := newDlqTestWriter(t, mock)
	w.nakOrDLQ(context.Background(), items, errors.New("permanent"))

	if got := atomic.LoadInt32(&ackCalls); got != 1 {
		t.Errorf("Ack called %d times, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNakOrDLQ_DBDLQFailsDiskDLQCaptures pins the durability guarantee for a
// DB outage: when the DB-backed insertDLQ ITSELF fails, the raw bytes are
// persisted to the on-disk DLQ and the message is ACKed (not dropped, not
// looped). The on-disk file must contain the exact payload so it can be
// replayed once the DB recovers.
func TestNakOrDLQ_DBDLQFailsDiskDLQCaptures(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`INSERT INTO traffic_event_dlq`).
		WithArgs("evt-3", "nexus.event.gateway", pgxmock.AnyArg(), redeliveryThresholdAttempts, pgxmock.AnyArg()).
		WillReturnError(errors.New("conn refused")) // DB is down

	dir := t.TempDir()
	payload := []byte(`{"id":"evt-3","cost":1.23}`)

	var ackCalls, nakCalls int32
	var capDelay delayCapture
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{ID: "evt-3"},
			msg: &mq.Message{
				NumDelivered: redeliveryThresholdAttempts,
				Subject:      "nexus.event.gateway",
				Data:         payload,
				Ack:          func() error { atomic.AddInt32(&ackCalls, 1); return nil },
				Nak:          func() error { atomic.AddInt32(&nakCalls, 1); return nil },
				NakWithDelay: capDelay.fn(),
			},
		},
	}

	w := newDlqTestWriter(t, mock)
	w.diskDLQ = newDiskDLQ(dir)
	w.nakOrDLQ(context.Background(), items, errors.New("permanent"))

	if got := atomic.LoadInt32(&ackCalls); got != 1 {
		t.Errorf("Ack called %d times, want 1 (on-disk capture must ack)", got)
	}
	if got := atomic.LoadInt32(&nakCalls) + atomic.LoadInt32(&capDelay.calls); got != 0 {
		t.Errorf("nak/nakWithDelay called %d times, want 0 (disk capture must not re-deliver)", got)
	}

	// The on-disk file must carry the exact payload for replay.
	recs := readDiskDLQ(t, filepath.Join(dir, diskDLQFileName))
	if len(recs) != 1 {
		t.Fatalf("on-disk dlq has %d records, want 1", len(recs))
	}
	if recs[0].MsgID != "evt-3" || recs[0].Subject != "nexus.event.gateway" {
		t.Errorf("disk record msgId/subject = %q/%q, want evt-3/nexus.event.gateway", recs[0].MsgID, recs[0].Subject)
	}
	if string(recs[0].Payload) != string(payload) {
		t.Errorf("disk record payload = %q, want %q (must round-trip for replay)", recs[0].Payload, payload)
	}
	if recs[0].DeliveryCount != redeliveryThresholdAttempts {
		t.Errorf("disk record deliveryCount = %d, want %d", recs[0].DeliveryCount, redeliveryThresholdAttempts)
	}
}

// TestNakOrDLQ_BothSinksFailNaksWithDelay pins the last-resort path: when BOTH
// the DB-backed DLQ and the on-disk DLQ fail, the message is NOT acked (we have
// no durable record of it) — it is nak'd with a backoff so the broker retries
// and the on-disk write gets another chance on redelivery.
func TestNakOrDLQ_BothSinksFailNaksWithDelay(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`INSERT INTO traffic_event_dlq`).
		WithArgs("evt-4", "nexus.event.gateway", pgxmock.AnyArg(), redeliveryThresholdAttempts, pgxmock.AnyArg()).
		WillReturnError(errors.New("conn refused"))

	// Point the on-disk DLQ at an un-creatable directory (a path UNDER a
	// regular file) so MkdirAll fails and the disk append errors.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}

	var ackCalls int32
	var capDelay delayCapture
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{ID: "evt-4"},
			msg: &mq.Message{
				NumDelivered: redeliveryThresholdAttempts,
				Subject:      "nexus.event.gateway",
				Data:         []byte(`{"id":"evt-4"}`),
				Ack:          func() error { atomic.AddInt32(&ackCalls, 1); return nil },
				Nak:          func() error { return nil },
				NakWithDelay: capDelay.fn(),
			},
		},
	}

	w := newDlqTestWriter(t, mock)
	w.diskDLQ = newDiskDLQ(filepath.Join(blocker, "sub")) // parent is a file → MkdirAll fails
	w.nakOrDLQ(context.Background(), items, errors.New("permanent"))

	if got := atomic.LoadInt32(&ackCalls); got != 0 {
		t.Errorf("Ack called %d times, want 0 (no durable record → must not ack)", got)
	}
	if got := atomic.LoadInt32(&capDelay.calls); got != 1 {
		t.Errorf("NakWithDelay called %d times, want 1 (last-resort retry)", got)
	}
}

// TestNakOrDLQ_MixedBatch pins per-item independence: one item below the
// threshold (delayed Nak), one at the threshold (DB DLQ + Ack).
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

	var fastAck, capAck, capNak int32
	var fastDelay delayCapture
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{ID: "evt-fast"},
			msg: &mq.Message{
				NumDelivered: 1,
				Subject:      "nexus.event.agent",
				Ack:          func() error { atomic.AddInt32(&fastAck, 1); return nil },
				Nak:          func() error { return nil },
				NakWithDelay: fastDelay.fn(),
			},
		},
		{
			event: TrafficEventMessage{ID: "evt-cap"},
			msg: &mq.Message{
				NumDelivered: redeliveryThresholdAttempts + 1,
				Subject:      "nexus.event.gateway",
				Ack:          func() error { atomic.AddInt32(&capAck, 1); return nil },
				Nak:          func() error { atomic.AddInt32(&capNak, 1); return nil },
				NakWithDelay: func(time.Duration) error { atomic.AddInt32(&capNak, 1); return nil },
			},
		},
	}

	w := newDlqTestWriter(t, mock)
	w.nakOrDLQ(context.Background(), items, errors.New("test"))

	if got := atomic.LoadInt32(&fastAck); got != 0 {
		t.Errorf("fast item Ack=%d, want 0", got)
	}
	if got := atomic.LoadInt32(&fastDelay.calls); got != 1 {
		t.Errorf("fast item NakWithDelay=%d, want 1", got)
	}
	if got := atomic.LoadInt32(&capAck); got != 1 {
		t.Errorf("cap item Ack=%d, want 1", got)
	}
	if got := atomic.LoadInt32(&capNak); got != 0 {
		t.Errorf("cap item Nak=%d, want 0", got)
	}
}

// TestRedeliveryDelay pins the backoff schedule: it grows with the delivery
// count and is capped at redeliveryBackoffMax, and is always positive.
func TestRedeliveryDelay(t *testing.T) {
	if d := redeliveryDelay(0); d != redeliveryBackoffBase {
		t.Errorf("redeliveryDelay(0) = %v, want %v (floors at 1 attempt)", d, redeliveryBackoffBase)
	}
	if d := redeliveryDelay(1); d != redeliveryBackoffBase {
		t.Errorf("redeliveryDelay(1) = %v, want %v", d, redeliveryBackoffBase)
	}
	if d := redeliveryDelay(2); d != 2*redeliveryBackoffBase {
		t.Errorf("redeliveryDelay(2) = %v, want %v (scales with attempts)", d, 2*redeliveryBackoffBase)
	}
	if d := redeliveryDelay(1000); d != redeliveryBackoffMax {
		t.Errorf("redeliveryDelay(1000) = %v, want cap %v", d, redeliveryBackoffMax)
	}
}

// TestIsJSONNulPoison pins the poison-pill classifier: nil and unrelated
// errors are not poison; both the jsonb (22P05) and the cast (22021)
// null-character SQLSTATEs are. Classification keys on the TYPED
// *pgconn.PgError.Code (F-0180), so a plain string error that merely
// CONTAINS the SQLSTATE text must NOT be treated as poison (it would
// false-trigger an ack-to-skip), and a real PgError wrapped via fmt.Errorf
// MUST still be recognised through errors.As.
func TestIsJSONNulPoison(t *testing.T) {
	if isJSONNulPoison(nil) {
		t.Error("nil must not be poison")
	}
	if isJSONNulPoison(&pgconn.PgError{Code: "23505", Message: "duplicate key"}) {
		t.Error("23505 must not be poison")
	}
	// A non-PgError string that merely contains "22021"/"22P05" must NOT be
	// poison — the old substring matcher false-triggered on exactly this.
	if isJSONNulPoison(errors.New("payload mentioned 22021 and 22P05 in its text")) {
		t.Error("plain string error containing the codes must NOT be poison (typed check)")
	}
	if !isJSONNulPoison(&pgconn.PgError{Code: "22P05", Message: "untranslatable character"}) {
		t.Error("22P05 must be poison")
	}
	// Wrapped real PgError must still be recognised through errors.As.
	wrapped := fmt.Errorf("insert traffic_event: %w", &pgconn.PgError{Code: "22021", Message: "invalid byte sequence"})
	if !isJSONNulPoison(wrapped) {
		t.Error("wrapped 22021 must be poison via errors.As")
	}
}

// TestDeadLetter_DBDLQSucceedsButAckFails covers the ack-error branch: even
// when the post-DLQ Ack fails, the row is recorded in the DB DLQ and the
// failure is only logged (no panic, no nak).
func TestDeadLetter_DBDLQSucceedsButAckFails(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectExec(`INSERT INTO traffic_event_dlq`).
		WithArgs("evt-ackfail", "nexus.event.gateway", pgxmock.AnyArg(), redeliveryThresholdAttempts, pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	var nakCalls int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{ID: "evt-ackfail"},
			msg: &mq.Message{
				NumDelivered: redeliveryThresholdAttempts,
				Subject:      "nexus.event.gateway",
				Data:         []byte(`{}`),
				Ack:          func() error { return errors.New("ack rejected") },
				Nak:          func() error { atomic.AddInt32(&nakCalls, 1); return nil },
				NakWithDelay: func(time.Duration) error { atomic.AddInt32(&nakCalls, 1); return nil },
			},
		},
	}
	w := newDlqTestWriter(t, mock)
	w.nakOrDLQ(context.Background(), items, errors.New("permanent"))
	if got := atomic.LoadInt32(&nakCalls); got != 0 {
		t.Errorf("nak called %d times, want 0 (DLQ insert succeeded)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNakWithBackoff_FallsBackToBareNak covers the transport that does not
// expose NakWithDelay: nakWithBackoff must fall back to a bare Nak.
func TestNakWithBackoff_FallsBackToBareNak(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	var bareNak int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{ID: "evt-bare"},
			msg: &mq.Message{
				NumDelivered: 1,
				Subject:      "nexus.event.gateway",
				Nak:          func() error { atomic.AddInt32(&bareNak, 1); return nil },
				// NakWithDelay deliberately nil — no per-message delay support.
			},
		},
	}
	w := newDlqTestWriter(t, mock)
	w.nakOrDLQ(context.Background(), items, errors.New("transient"))
	if got := atomic.LoadInt32(&bareNak); got != 1 {
		t.Errorf("bare Nak called %d times, want 1 (fallback)", got)
	}
}

// TestErrString covers the small helper: nil produces empty, error produces
// .Error() output.
func TestErrString(t *testing.T) {
	if got := errString(nil); got != "" {
		t.Errorf("errString(nil) = %q, want empty", got)
	}
	if got := errString(errors.New("boom")); got != "boom" {
		t.Errorf("errString(err) = %q, want boom", got)
	}
}

// readDiskDLQ parses the JSON-Lines on-disk DLQ file into records.
func readDiskDLQ(t *testing.T, path string) []diskDLQRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open disk dlq %s: %v", path, err)
	}
	defer f.Close() //nolint:errcheck
	var out []diskDLQRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec diskDLQRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("unmarshal disk dlq line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan disk dlq: %v", err)
	}
	return out
}
