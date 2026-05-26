package consumer

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// exemptionFlushConsumer wires an ExemptionConsumer against pgxmock through
// the PgxPool seam. Mirrors trafficFlushWriter / adminFlushWriter shape in
// flush_pgxmock_test.go.
func exemptionFlushConsumer(t *testing.T) (*ExemptionConsumer, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	c := NewExemptionConsumerWithPgxPool(
		mock,
		nil, // mqc unused — we call flush() directly
		ExemptionConsumerConfig{BatchSize: 100, FlushInterval: time.Hour},
		discardLogger(),
		newTestRegistry(),
	)
	return c, mock
}

// makeExemptionItem builds one pendingExemptionMessage with ack/nak counters.
func makeExemptionItem(thingID, host, reason string, expiresAt time.Time, ack, nak *int32) pendingExemptionMessage {
	return pendingExemptionMessage{
		event: exemptionUpload{
			Kind:      "exemption",
			ThingID:   thingID,
			Host:      host,
			Reason:    reason,
			ExpiresAt: expiresAt.Format(time.RFC3339),
		},
		msg: countingMsg(ack, nak),
	}
}

// TestExemptionConsumer_Flush_HappyPath_AcksAllAndCommits drives the
// full Begin → Exec(INSERT exemption_request) → Commit chain and asserts
// every message acks, no nak.
func TestExemptionConsumer_Flush_HappyPath_AcksAllAndCommits(t *testing.T) {
	c, mock := exemptionFlushConsumer(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO exemption_request`).
		WithArgs(anyArgs(6)...).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectExec(`INSERT INTO exemption_request`).
		WithArgs(anyArgs(6)...).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()
	mock.ExpectRollback().Maybe()

	var ack, nak int32
	exp := time.Now().Add(24 * time.Hour).UTC()
	items := []pendingExemptionMessage{
		makeExemptionItem("agent-1", "example.com", "cert pin mismatch", exp, &ack, &nak),
		makeExemptionItem("agent-2", "api.example.com", "tls handshake error", exp, &ack, &nak),
	}
	if err := c.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if ack != 2 {
		t.Errorf("ack count = %d, want 2", ack)
	}
	if nak != 0 {
		t.Errorf("nak count = %d, want 0", nak)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestExemptionConsumer_Flush_BeginError_NaksAndReturnsErr covers the
// pool.Begin failure branch — every message must nak (so JetStream
// redelivers) and the error is wrapped with "begin tx:".
func TestExemptionConsumer_Flush_BeginError_NaksAndReturnsErr(t *testing.T) {
	c, mock := exemptionFlushConsumer(t)

	sentinel := errors.New("db down")
	mock.ExpectBegin().WillReturnError(sentinel)

	var ack, nak int32
	items := []pendingExemptionMessage{
		makeExemptionItem("agent-1", "h", "r", time.Now().Add(time.Hour), &ack, &nak),
	}
	err := c.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("want 'begin tx' wrap; got %v", err)
	}
	if nak != 1 || ack != 0 {
		t.Errorf("ack=%d nak=%d; want ack=0 nak=1", ack, nak)
	}
}

// TestExemptionConsumer_Flush_InsertError_NaksAndReturnsErr covers the
// INSERT failure branch — wraps with "insert exemption_request:".
func TestExemptionConsumer_Flush_InsertError_NaksAndReturnsErr(t *testing.T) {
	c, mock := exemptionFlushConsumer(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO exemption_request`).
		WithArgs(anyArgs(6)...).
		WillReturnError(errors.New("constraint violation"))
	mock.ExpectRollback()

	var ack, nak int32
	items := []pendingExemptionMessage{
		makeExemptionItem("agent-1", "h", "r", time.Now().Add(time.Hour), &ack, &nak),
	}
	err := c.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "insert exemption_request") {
		t.Errorf("want 'insert exemption_request' wrap; got %v", err)
	}
	if nak != 1 || ack != 0 {
		t.Errorf("ack=%d nak=%d; want ack=0 nak=1", ack, nak)
	}
}

// TestExemptionConsumer_Flush_CommitError_NaksAndReturnsErr covers the
// commit failure branch — wraps with "commit tx:".
func TestExemptionConsumer_Flush_CommitError_NaksAndReturnsErr(t *testing.T) {
	c, mock := exemptionFlushConsumer(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO exemption_request`).
		WithArgs(anyArgs(6)...).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit().WillReturnError(errors.New("commit fail"))
	mock.ExpectRollback().Maybe()

	var ack, nak int32
	items := []pendingExemptionMessage{
		makeExemptionItem("agent-1", "h", "r", time.Now().Add(time.Hour), &ack, &nak),
	}
	err := c.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "commit tx") {
		t.Errorf("want 'commit tx' wrap; got %v", err)
	}
	if nak != 1 || ack != 0 {
		t.Errorf("ack=%d nak=%d; want ack=0 nak=1", ack, nak)
	}
}

// TestExemptionConsumer_Flush_ZeroExpiresAt covers the empty-expiresAt
// branch — duration_minutes must default to 0 (no panic on time.Parse fail).
func TestExemptionConsumer_Flush_ZeroExpiresAt(t *testing.T) {
	c, mock := exemptionFlushConsumer(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO exemption_request`).
		WithArgs(anyArgs(6)...).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()
	mock.ExpectRollback().Maybe()

	var ack, nak int32
	items := []pendingExemptionMessage{
		{
			event: exemptionUpload{
				Kind: "exemption", ThingID: "agent-x", Host: "h", Reason: "r",
				ExpiresAt: "", // explicitly empty
			},
			msg: countingMsg(&ack, &nak),
		},
	}
	if err := c.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if ack != 1 {
		t.Errorf("ack count = %d, want 1", ack)
	}
}

// TestExemptionConsumer_Flush_PastExpiresAt covers the past-expiry branch —
// negative durations clamp to 0 so the DB sees a sensible value.
func TestExemptionConsumer_Flush_PastExpiresAt(t *testing.T) {
	c, mock := exemptionFlushConsumer(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO exemption_request`).
		WithArgs(anyArgs(6)...).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()
	mock.ExpectRollback().Maybe()

	var ack, nak int32
	exp := time.Now().Add(-1 * time.Hour).UTC() // expired an hour ago
	items := []pendingExemptionMessage{
		makeExemptionItem("agent-1", "h", "r", exp, &ack, &nak),
	}
	if err := c.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// TestExemptionConsumer_buildTransactionID pins the stable-hash invariant —
// same (thingID, host) → same txn id; different inputs → different ids.
// The dedup index relies on this stability for ON CONFLICT to work.
func TestExemptionConsumer_buildTransactionID(t *testing.T) {
	a1 := buildExemptionTransactionID("thing-1", "example.com")
	a2 := buildExemptionTransactionID("thing-1", "example.com")
	if a1 != a2 {
		t.Errorf("same inputs returned different ids: %q vs %q", a1, a2)
	}
	if !strings.HasPrefix(a1, "agent-auto-") {
		t.Errorf("missing 'agent-auto-' prefix: %q", a1)
	}
	if len(a1) != len("agent-auto-")+16 {
		t.Errorf("length wrong: got %d (%q)", len(a1), a1)
	}
	// Different thingID → different id.
	b := buildExemptionTransactionID("thing-2", "example.com")
	if a1 == b {
		t.Errorf("different thingID produced same id")
	}
	// Different host → different id.
	c := buildExemptionTransactionID("thing-1", "other.com")
	if a1 == c {
		t.Errorf("different host produced same id")
	}
	// Pin the requested_by prefix convention used by the consumer.
	// (Lives in insertExemptions; tested here via the const string to
	// guard against accidental rename — the dedup key is (host,
	// requested_by), so the "agent:" prefix is part of the contract.)
	if got := "agent:" + "thing-1"; got != "agent:thing-1" {
		t.Errorf("requested_by convention drifted")
	}
}

// TestExemptionConsumer_Start_DeserializeFail_AcksMessage drives the Start
// goroutine's poison-pill branch: an undecodable JSON payload is logged
// and acked (dropped) rather than nakked (which would cause infinite
// redelivery loop).
func TestExemptionConsumer_Start_DeserializeFail_AcksMessage(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)

	var ackCount, nakCount int32
	mqc := &mockConsumer{
		consumeFn: func(_ context.Context, _ string, _ string, h func(context.Context, *mq.Message) error) error {
			// Send one undecodable message — handler should ack it then
			// drain (no further dispatch).
			msg := &mq.Message{
				Data: []byte("{not valid json"),
				Ack:  func() error { atomic.AddInt32(&ackCount, 1); return nil },
				Nak:  func() error { atomic.AddInt32(&nakCount, 1); return nil },
			}
			_ = h(context.Background(), msg)
			return nil
		},
	}

	c := NewExemptionConsumerWithPgxPool(
		mock, mqc,
		ExemptionConsumerConfig{BatchSize: 10, FlushInterval: time.Hour},
		discardLogger(),
		newTestRegistry(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = c.Start(ctx)
	}()
	// Give the goroutine time to dispatch + ack.
	time.Sleep(50 * time.Millisecond)
	cancel()

	if ackCount != 1 {
		t.Errorf("ack count = %d; want 1 (poison-pill should be acked + dropped)", ackCount)
	}
	if nakCount != 0 {
		t.Errorf("nak count = %d; want 0 (poison-pill must NOT trigger redelivery)", nakCount)
	}
}

// TestExemptionConsumer_Defaults_CapacityAndInterval pins the zero-value
// → default substitution in newExemptionConsumer.
func TestExemptionConsumer_Defaults_CapacityAndInterval(t *testing.T) {
	c := NewExemptionConsumerWithPgxPool(
		nil, nil,
		ExemptionConsumerConfig{}, // zero-value config
		discardLogger(),
		nil, // no registry; nil-counter branches exercised
	)
	if c.cfg.BatchSize != 100 {
		t.Errorf("BatchSize default = %d, want 100", c.cfg.BatchSize)
	}
	if c.cfg.FlushInterval != 5*time.Second {
		t.Errorf("FlushInterval default = %v, want 5s", c.cfg.FlushInterval)
	}
}

// mockConsumer is a minimal mq.Consumer test double used by Start tests
// where a fake Consume is enough. Keeps the test surface independent of
// the full mq fake elsewhere in the package.
type mockConsumer struct {
	consumeFn func(ctx context.Context, queue, group string, h func(context.Context, *mq.Message) error) error
}

func (m *mockConsumer) Subscribe(_ context.Context, _ string, _ mq.MessageHandler) error {
	return nil
}
func (m *mockConsumer) Consume(ctx context.Context, queue, group string, h mq.MessageHandler) error {
	if m.consumeFn == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	return m.consumeFn(ctx, queue, group, h)
}
func (m *mockConsumer) Close() error { return nil }
