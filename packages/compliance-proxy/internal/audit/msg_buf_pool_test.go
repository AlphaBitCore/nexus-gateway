package audit

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// These tests aim at what pooling the marshal buffer actually risks: bytes that
// belong to one audit event being served for another, and one oversized captured
// body permanently inflating the pool.

// capturingProducer records the payload it is handed, copying it as the Enqueue
// contract requires. failOn makes the Nth (0-based) call fail so the error path
// can be driven without wedging the writer.
type capturingProducer struct {
	got    [][]byte
	calls  int
	failOn int
	err    error
}

func (p *capturingProducer) Publish(context.Context, string, []byte) error { return nil }

func (p *capturingProducer) Enqueue(_ context.Context, _ string, data []byte) error {
	call := p.calls
	p.calls++
	if p.err != nil && call == p.failOn {
		return p.err
	}
	p.got = append(p.got, append([]byte(nil), data...))
	return nil
}
func (p *capturingProducer) Close() error { return nil }

// bodyEvent is an AuditEvent carrying an inline body of the given text, which is
// what makes the encoded message length vary between events in a batch.
func bodyEvent(id, text string) AuditEvent {
	raw := `{"prompt":"` + text + `"}`
	return AuditEvent{
		ID:                  id,
		TransactionID:       "txn-" + id,
		TrafficSource:       "COMPLIANCE_PROXY",
		IngressType:         "CONNECT",
		BumpStatus:          "BUMP_SUCCESS",
		SourceIP:            "203.0.113.7",
		TargetHost:          "api.openai.com",
		Method:              "POST",
		Path:                "/v1/chat/completions",
		RequestHookDecision: "APPROVE",
		RequestBody: sharedaudit.NewInlineBody(
			[]byte(raw), int64(len(raw)), false, "application/json"),
	}
}

// TestFlushBatchPublishesEachEventsOwnBytes is the use-after-reuse guard. The
// batch runs a long event, then a short one, then a long one again through the
// SAME pooled buffer. A buffer that were not reset, or reclaimed before Enqueue
// took the bytes, would leave the short event carrying the long event's tail —
// which is silent, because the leading object still parses.
func TestFlushBatchPublishesEachEventsOwnBytes(t *testing.T) {
	prod := &capturingProducer{}
	w := &MQBatchWriter{producer: prod, queue: "q", logger: quietLogger()}

	events := []AuditEvent{
		bodyEvent("evt-long-1", strings.Repeat("a", 4096)),
		bodyEvent("evt-short", "b"),
		bodyEvent("evt-long-2", strings.Repeat("c", 8192)),
	}
	if err := w.flushBatch(context.Background(), events); err != nil {
		t.Fatalf("flushBatch: %v", err)
	}
	if len(prod.got) != len(events) {
		t.Fatalf("published %d messages, want %d", len(prod.got), len(events))
	}

	for i, raw := range prod.got {
		dec := json.NewDecoder(bytes.NewReader(raw))
		var got map[string]any
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("message %d is not valid JSON: %v", i, err)
		}
		// Anything after the object means the buffer carried a previous event's
		// tail past the end of this one.
		if dec.More() {
			t.Fatalf("message %d has trailing bytes after the JSON object: %q",
				i, raw[len(raw)-32:])
		}
		if got["id"] != events[i].ID {
			t.Fatalf("message %d id = %v, want %s", i, got["id"], events[i].ID)
		}
	}

	// The short event's payload must be short: a reused-but-unreset buffer would
	// make it at least as long as the 4 KiB event before it.
	if len(prod.got[1]) >= len(prod.got[0]) {
		t.Fatalf("short event serialized to %d bytes, not smaller than the preceding "+
			"long event's %d — the pooled buffer was not reset",
			len(prod.got[1]), len(prod.got[0]))
	}
}

// TestFlushBatchWireBytesUnchangedByPooling pins functional equivalence: encoding
// into the pooled buffer must put exactly the bytes on the queue that a plain
// json.Marshal of the same message would. The Hub db-writer consumes these bytes,
// so a drift here is a wire change, not an optimization.
func TestFlushBatchWireBytesUnchangedByPooling(t *testing.T) {
	event := bodyEvent("evt-parity", strings.Repeat("x", 300))
	prod := &capturingProducer{}
	w := (&MQBatchWriter{producer: prod, queue: "q", logger: quietLogger()}).
		WithThingIdentity("thing-1", "cp-a")

	if err := w.flushBatch(context.Background(), []AuditEvent{event}); err != nil {
		t.Fatalf("flushBatch: %v", err)
	}
	want, err := json.Marshal(toMessage(event, "thing-1", "cp-a"))
	if err != nil {
		t.Fatalf("reference marshal: %v", err)
	}
	if !bytes.Equal(prod.got[0], want) {
		t.Fatalf("published bytes differ from json.Marshal output\n got: %s\nwant: %s",
			prod.got[0], want)
	}
}

// TestFlushBatchAfterEnqueueErrorStillPublishesCleanBytes drives the error return,
// which reclaims the buffer on a different line than the success path. If that
// reclaim handed back a buffer whose bytes were still referenced — or skipped the
// reclaim so the pool starved — the next flush would show it.
func TestFlushBatchAfterEnqueueErrorStillPublishesCleanBytes(t *testing.T) {
	prod := &capturingProducer{failOn: 0, err: context.DeadlineExceeded}
	w := &MQBatchWriter{producer: prod, queue: "q", logger: quietLogger()}

	failing := bodyEvent("evt-fail", strings.Repeat("z", 6000))
	if err := w.flushBatch(context.Background(), []AuditEvent{failing}); err == nil {
		t.Fatal("flushBatch returned nil on a producer error")
	}
	if len(prod.got) != 0 {
		t.Fatalf("failed enqueue recorded %d messages, want 0", len(prod.got))
	}

	next := bodyEvent("evt-after", "ok")
	if err := w.flushBatch(context.Background(), []AuditEvent{next}); err != nil {
		t.Fatalf("flushBatch after error: %v", err)
	}
	want, err := json.Marshal(toMessage(next, "", ""))
	if err != nil {
		t.Fatalf("reference marshal: %v", err)
	}
	if !bytes.Equal(prod.got[0], want) {
		t.Fatalf("post-error publish carried stale bytes\n got: %s\nwant: %s",
			prod.got[0], want)
	}
}

// TestMsgBufPoolableDropsOversizedBuffers pins the cap that keeps one oversized
// captured body from inflating every pooled buffer thereafter.
func TestMsgBufPoolableDropsOversizedBuffers(t *testing.T) {
	atCap := new(bytes.Buffer)
	atCap.Grow(msgBufReclaimCap)
	if !msgBufPoolable(atCap) {
		t.Fatalf("buffer at the reclaim cap (%d) was rejected", atCap.Cap())
	}

	over := new(bytes.Buffer)
	over.Grow(msgBufReclaimCap + 1)
	if msgBufPoolable(over) {
		t.Fatalf("buffer of capacity %d was pooled despite exceeding the %d cap",
			over.Cap(), msgBufReclaimCap)
	}

	if msgBufPoolable(nil) {
		t.Fatal("nil buffer reported as poolable")
	}
}

// TestFlushBatchMarshalFailureSkipsEventAndContinues covers the encode-error
// branch, which reclaims the buffer and drops just that event. A body whose
// declared raw encoding does not hold valid JSON makes Body.MarshalJSON fail, so
// this is a real marshal failure on the production shape rather than a simulated
// one.
func TestFlushBatchMarshalFailureSkipsEventAndContinues(t *testing.T) {
	prod := &capturingProducer{}
	w := &MQBatchWriter{producer: prod, queue: "q", logger: quietLogger()}

	bad := bodyEvent("evt-bad", "x")
	bad.RequestBody = sharedaudit.Body{
		Kind:        sharedaudit.BodyInline,
		Encoding:    sharedaudit.EncodingRaw,
		InlineBytes: []byte(`{"unterminated":`),
		SizeBytes:   16,
	}
	good := bodyEvent("evt-good", "y")

	if err := w.flushBatch(context.Background(), []AuditEvent{bad, good}); err != nil {
		t.Fatalf("flushBatch: %v", err)
	}
	if len(prod.got) != 1 {
		t.Fatalf("published %d messages, want 1 (the unencodable event is dropped)", len(prod.got))
	}
	var got map[string]any
	if err := json.Unmarshal(prod.got[0], &got); err != nil {
		t.Fatalf("surviving message is not valid JSON: %v", err)
	}
	if got["id"] != "evt-good" {
		t.Fatalf("surviving message id = %v, want evt-good", got["id"])
	}
}
