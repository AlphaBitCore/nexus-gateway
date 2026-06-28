package consumer

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// dlq_malformed_test.go: the admin-audit consumer must route an
// undeserializable message to a durable on-disk DLQ and Ack it, never silently
// Ack-DROP it (which would lose an admin-audit ledger row, since the Control
// Plane is the sole producer). On an on-disk write failure the message is Nak'd
// so the broker retries instead of dropping it.

// TestAdminAudit_DeadLetterMalformed_PersistsThenAcks pins the success path:
// the malformed bytes land in the on-disk DLQ (subject + payload + a
// "deserialize:" last_error preserved) and the message is acked exactly once.
func TestAdminAudit_DeadLetterMalformed_PersistsThenAcks(t *testing.T) {
	w := NewAdminAuditWriterWithPgxPool(nil, nil, AdminAuditWriterConfig{}, discardLogger(), newTestRegistry())
	w.diskDLQ = newDiskDLQNamed(t.TempDir(), adminAuditDLQFileName)

	var ack, nak int32
	msg := &mq.Message{
		Subject:      adminAuditQueue,
		Data:         []byte("this is not valid json"),
		NumDelivered: 2,
		Ack:          func() error { atomic.AddInt32(&ack, 1); return nil },
		Nak:          func() error { atomic.AddInt32(&nak, 1); return nil },
	}

	if err := w.deadLetterMalformed(msg, errors.New("unexpected token")); err != nil {
		t.Fatalf("deadLetterMalformed: %v", err)
	}
	if ack != 1 || nak != 0 {
		t.Fatalf("ack=%d nak=%d, want ack=1 nak=0 (malformed must be DLQ'd then acked)", ack, nak)
	}

	recs := readDiskDLQ(t, w.diskDLQ.path())
	if len(recs) != 1 {
		t.Fatalf("dlq records = %d, want 1 (message must be captured, not dropped)", len(recs))
	}
	if string(recs[0].Payload) != "this is not valid json" {
		t.Errorf("dlq payload = %q, want the raw malformed bytes", recs[0].Payload)
	}
	if recs[0].Subject != adminAuditQueue {
		t.Errorf("dlq subject = %q, want %q", recs[0].Subject, adminAuditQueue)
	}
	if got := recs[0].LastError; got == "" || got[:12] != "deserialize:" {
		t.Errorf("dlq lastError = %q, want a 'deserialize:'-prefixed reason", got)
	}
}

// TestAdminAudit_DeadLetterMalformed_NaksWhenDiskFails pins the durability
// fallback: if the on-disk DLQ write fails, the message must be Nak'd (broker
// retries, disk gets another chance) — never acked, which would lose it.
func TestAdminAudit_DeadLetterMalformed_NaksWhenDiskFails(t *testing.T) {
	w := NewAdminAuditWriterWithPgxPool(nil, nil, AdminAuditWriterConfig{}, discardLogger(), newTestRegistry())
	// Point the DLQ at a path whose parent is a regular file → MkdirAll fails.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	w.diskDLQ = newDiskDLQNamed(filepath.Join(blocker, "sub"), adminAuditDLQFileName)

	var ack, nak int32
	msg := &mq.Message{
		Subject: adminAuditQueue,
		Data:    []byte("garbage"),
		Ack:     func() error { atomic.AddInt32(&ack, 1); return nil },
		Nak:     func() error { atomic.AddInt32(&nak, 1); return nil },
	}
	if err := w.deadLetterMalformed(msg, errors.New("bad")); err != nil {
		t.Fatalf("deadLetterMalformed: %v", err)
	}
	if ack != 0 || nak != 1 {
		t.Fatalf("ack=%d nak=%d, want ack=0 nak=1 (disk failure must nak, not drop)", ack, nak)
	}
}

// TestDeadLetterMalformed_NilRegistry — the metric guards must short-circuit
// when the writer is built with reg=nil (the diskDLQTotal / errorsTotal arms).
func TestDeadLetterMalformed_NilRegistry(t *testing.T) {
	w := NewAdminAuditWriterWithPgxPool(nil, nil, AdminAuditWriterConfig{}, discardLogger(), nil)
	w.diskDLQ = newDiskDLQNamed(t.TempDir(), adminAuditDLQFileName)
	var ack int32
	msg := &mq.Message{Subject: adminAuditQueue, Data: []byte("x"), Ack: func() error { atomic.AddInt32(&ack, 1); return nil }}
	if err := w.deadLetterMalformed(msg, errors.New("bad")); err != nil {
		t.Fatalf("admin nil-reg: %v", err)
	}
	if ack != 1 {
		t.Fatalf("ack=%d, want 1", ack)
	}
}
