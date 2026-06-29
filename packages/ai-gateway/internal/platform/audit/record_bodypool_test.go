package audit

import (
	"bytes"
	"context"
	"github.com/goccy/go-json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// drainRecCh non-blockingly drains every record currently queued on the writer's
// bounded queue (recCh), returning them in queue order. Used by white-box tests to
// inspect the records handlePublishFailure re-queued for retry — the producer/
// multi-consumer successor to the old buf re-buffer.
func drainRecCh(w *Writer) []*Record {
	var out []*Record
	for {
		select {
		case rec := <-w.recCh:
			out = append(out, rec)
		default:
			return out
		}
	}
}

// failingBatchProducer fails every async publish, forcing the re-buffer path.
type failingBatchProducer struct{}

func (failingBatchProducer) Publish(context.Context, string, []byte) error { return nil }
func (failingBatchProducer) Enqueue(context.Context, string, []byte) error { return nil }
func (failingBatchProducer) Close() error                                  { return nil }
func (failingBatchProducer) EnqueueBatchAsync(_ context.Context, _ string, batch [][]byte) ([]error, error) {
	errs := make([]error, len(batch))
	for i := range errs {
		errs[i] = context.DeadlineExceeded
	}
	return errs, nil
}

// On a successful publish, the pooled request-body buffer is reclaimed (handle
// nil'd) AND the published bytes carry the intact body — no corruption.
func TestRequestBodyPool_ReclaimedAfterPublish_NoCorruption(t *testing.T) {
	prod := &frameCapProducer{}
	w := NewWriter(prod, "q", nil, slog.Default())
	src := []byte(`{"model":"m1","pad":"` + strings.Repeat("a", 2048) + `"}`)
	body, h := AcquireRequestBody(src)
	if !bytes.Equal(body, src) {
		t.Fatal("AcquireRequestBody returned wrong bytes")
	}
	rec := &Record{RequestID: "r1", Timestamp: time.Unix(1700000000, 0).UTC(), RequestBody: body, RequestAction: decision.ActionApprove, ModelName: "m1", StatusCode: 200, Path: "/v1/x"}
	rec.AttachPooledRequestBody(h)

	w.publishBatchOn(0, []*Record{rec})

	if rec.reqBodyHandle != nil {
		t.Fatal("pooled body not reclaimed after successful publish")
	}
	prod.mu.Lock()
	calls := prod.published
	prod.mu.Unlock()
	found := false
	for _, call := range calls {
		for _, p := range call {
			for _, line := range splitLines(p) {
				var msg mq.TrafficEventMessage
				if json.Unmarshal(line, &msg) == nil && msg.ID == "r1" {
					found = true
					if !bytes.Equal(msg.RequestBody.InlineBytes, src) {
						t.Fatalf("published body corrupted by pool: got %d bytes", len(msg.RequestBody.InlineBytes))
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("record r1 was not published")
	}
}

// Part C: on a publish FAILURE the pooled request body is reclaimed at marshal time
// (the marshaled bytes hold the copy) and the record carries those bytes for retry,
// so the body pool is decoupled from the retry-queue depth. The retry re-publishes
// the ORIGINAL body byte-for-byte even after the reclaimed pool buffer is reused by
// another request — proving the retry reads rec.marshaled, never the reused buffer.
func TestRequestBodyPool_ReclaimedOnFailure_RetryByteLossless(t *testing.T) {
	w := NewWriter(failingBatchProducer{}, "q", nil, slog.Default())
	origBody := []byte(`{"model":"mf","pad":"` + strings.Repeat("b", 512) + `"}`)
	body, h := AcquireRequestBody(origBody)
	rec := &Record{RequestID: "rf", Timestamp: time.Unix(1700000000, 0).UTC(), RequestBody: body, RequestAction: decision.ActionApprove, ModelName: "mf", StatusCode: 200, Path: "/v1/x"}
	rec.AttachPooledRequestBody(h)

	// Size the bounded queue so handlePublishFailure re-queues onto recCh rather
	// than spilling (no consumers started, so the re-queued record stays put for
	// the test to drain and retry).
	w.recCh = make(chan *Record, 4)

	// First publish fails → the failed record is re-queued onto recCh for retry.
	w.publishBatchOn(0, []*Record{rec})

	if rec.reqBodyHandle != nil {
		t.Fatal("Part C: pooled body must be reclaimed at marshal, not pinned across the retry")
	}
	if rec.marshaled == nil {
		t.Fatal("Part C: a failed record must carry its marshaled bytes for retry")
	}
	batch := drainRecCh(w)
	if len(batch) != 1 {
		t.Fatalf("expected exactly 1 re-queued record, got %d", len(batch))
	}

	// NO-BLEED: scribble different bytes through the reclaimed pool buffer. A buggy
	// retry that re-read the (reused) request body would now publish this scribble.
	scribble, _ := AcquireRequestBody(bytes.Repeat([]byte("Z"), 512))
	_ = scribble

	// Retry against a capturing success producer: the published body must equal the
	// ORIGINAL bytes, sourced from rec.marshaled — not the scribbled pool buffer.
	prod := &frameCapProducer{}
	w.producer = prod
	w.publishBatchOn(0, batch)

	prod.mu.Lock()
	calls := prod.published
	prod.mu.Unlock()
	found := false
	for _, call := range calls {
		for _, p := range call {
			for _, line := range splitLines(p) {
				var msg mq.TrafficEventMessage
				if json.Unmarshal(line, &msg) == nil && msg.ID == "rf" {
					found = true
					if !bytes.Equal(msg.RequestBody.InlineBytes, origBody) {
						t.Fatalf("retry published wrong body (bleed/loss): got %q", msg.RequestBody.InlineBytes)
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("re-buffered record rf was not re-published on retry")
	}
}

// A reclaimed buffer is reused by the next Acquire without leaking stale bytes.
func TestRequestBodyPool_ReuseNoStaleBytes(t *testing.T) {
	b1, h1 := AcquireRequestBody(bytes.Repeat([]byte("X"), 4096))
	_ = b1
	releaseRequestBody(h1) // simulate terminal reclaim
	b2, _ := AcquireRequestBody([]byte("short"))
	if string(b2) != "short" {
		t.Fatalf("reused buffer leaked stale bytes: %q", b2)
	}
}

// The pooled streaming-capture (response) buffer is reclaimed on a successful
// publish (handle nil'd) AND the published bytes carry the intact response body.
func TestResponseBodyPool_ReclaimedAfterPublish_NoCorruption(t *testing.T) {
	prod := &frameCapProducer{}
	w := NewWriter(prod, "q", nil, slog.Default())
	rh := AcquireResponseBuffer()
	respSrc := []byte("data: " + strings.Repeat("r", 2048) + "\n\n")
	*rh = append(*rh, respSrc...)
	rec := &Record{RequestID: "rr1", Timestamp: time.Unix(1700000000, 0).UTC(), ResponseBody: *rh, ResponseAction: decision.ActionApprove, ModelName: "m1", StatusCode: 200, Path: "/v1/x"}
	rec.AttachPooledResponseBody(rh)

	w.publishBatchOn(0, []*Record{rec})

	if rec.respBodyHandle != nil {
		t.Fatal("pooled response body not reclaimed after successful publish")
	}
	prod.mu.Lock()
	calls := prod.published
	prod.mu.Unlock()
	found := false
	for _, call := range calls {
		for _, p := range call {
			for _, line := range splitLines(p) {
				var msg mq.TrafficEventMessage
				if json.Unmarshal(line, &msg) == nil && msg.ID == "rr1" {
					found = true
					if !bytes.Equal(msg.ResponseBody.InlineBytes, respSrc) {
						t.Fatalf("published response body corrupted by pool: got %d bytes", len(msg.ResponseBody.InlineBytes))
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("record rr1 was not published")
	}
}

// Part C: on a publish FAILURE the pooled response (tee) body is reclaimed at
// marshal time and the record carries its marshaled bytes for retry; the retry
// re-publishes the ORIGINAL response body byte-for-byte even after the reclaimed
// tee buffer is reused — proving no bleed from the recycled streaming buffer.
func TestResponseBodyPool_ReclaimedOnFailure_RetryByteLossless(t *testing.T) {
	w := NewWriter(failingBatchProducer{}, "q", nil, slog.Default())
	rh := AcquireResponseBuffer()
	origResp := []byte("data: " + strings.Repeat("k", 512) + "\n\n")
	*rh = append(*rh, origResp...)
	rec := &Record{RequestID: "rrf", Timestamp: time.Unix(1700000000, 0).UTC(), ResponseBody: *rh, ResponseAction: decision.ActionApprove, ModelName: "mf", StatusCode: 200, Path: "/v1/x"}
	rec.AttachPooledResponseBody(rh)

	// Size the bounded queue so the failed record re-queues onto recCh.
	w.recCh = make(chan *Record, 4)
	w.publishBatchOn(0, []*Record{rec})

	if rec.respBodyHandle != nil {
		t.Fatal("Part C: pooled response body must be reclaimed at marshal, not pinned across the retry")
	}
	if rec.marshaled == nil {
		t.Fatal("Part C: a failed record must carry its marshaled bytes for retry")
	}
	batch := drainRecCh(w)

	// NO-BLEED: reuse the reclaimed tee buffer with different bytes.
	scribble := AcquireResponseBuffer()
	*scribble = append(*scribble, bytes.Repeat([]byte("Z"), 512)...)

	prod := &frameCapProducer{}
	w.producer = prod
	w.publishBatchOn(0, batch)

	prod.mu.Lock()
	calls := prod.published
	prod.mu.Unlock()
	found := false
	for _, call := range calls {
		for _, p := range call {
			for _, line := range splitLines(p) {
				var msg mq.TrafficEventMessage
				if json.Unmarshal(line, &msg) == nil && msg.ID == "rrf" {
					found = true
					if !bytes.Equal(msg.ResponseBody.InlineBytes, origResp) {
						t.Fatalf("retry published wrong response body (bleed/loss): got %q", msg.ResponseBody.InlineBytes)
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("re-buffered record rrf was not re-published on retry")
	}
}

// AcquireResponseBuffer always returns a zero-length buffer, so a reused pooled
// array never leaks the previous stream's bytes; ReleaseResponseBuffer recycles it.
func TestResponseBodyPool_ReuseNoStaleBytes(t *testing.T) {
	h1 := AcquireResponseBuffer()
	*h1 = append(*h1, bytes.Repeat([]byte("Y"), 4096)...)
	ReleaseResponseBuffer(h1) // simulate terminal reclaim
	h2 := AcquireResponseBuffer()
	if len(*h2) != 0 {
		t.Fatalf("AcquireResponseBuffer must return zero-length; got %d bytes", len(*h2))
	}
	*h2 = append(*h2, []byte("short")...)
	if string(*h2) != "short" {
		t.Fatalf("reused response buffer leaked stale bytes: %q", *h2)
	}
}
