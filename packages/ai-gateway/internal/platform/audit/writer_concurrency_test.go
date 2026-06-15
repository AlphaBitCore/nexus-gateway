package audit

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// waitForMsgCount polls the producer until it has at least n messages or
// the deadline trips. Returns the final count.
func waitForMsgCount(prod *memProducer, n int, within time.Duration) int {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if len(prod.msgs()) >= n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return len(prod.msgs())
}

// TestWriter_SizeTriggeredFlush proves a burst that reaches flushHighWater
// is flushed promptly by the size trigger — NOT held until the 5s ticker.
// Without the trigger this test would have to wait the full interval (or
// the burst would drop above maxQueueSize); a fast completion proves the
// signal path is wired.
func TestWriter_SizeTriggeredFlush(t *testing.T) {
	prod := &memProducer{}
	w := NewWriter(prod, "q", nil, slog.Default())
	defer w.Close()

	for i := range flushHighWater {
		w.Enqueue(&Record{RequestID: fmt.Sprintf("r%d", i)})
	}
	// The default ticker is 5s; the size trigger must flush far sooner.
	if got := waitForMsgCount(prod, flushHighWater, 2*time.Second); got != flushHighWater {
		t.Fatalf("size-triggered flush published %d within 2s, want %d (signal path not wired?)", got, flushHighWater)
	}
}

// TestWriter_ConcurrentEnqueue_NoLossNoDup hammers Enqueue from many
// goroutines while the flush worker pool drains concurrently, then Close
// drains the rest. Every record must be published exactly once — no loss
// (lost under a broken buffer swap) and no duplicate or corrupted record
// (cross-record bleed in the concurrent publish path). Run under -race.
func TestWriter_ConcurrentEnqueue_NoLossNoDup(t *testing.T) {
	prod := &memProducer{}
	w := NewWriter(prod, "q", nil, slog.Default())

	const goroutines, perG = 8, 500
	want := goroutines * perG
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range perG {
				w.Enqueue(&Record{RequestID: fmt.Sprintf("g%d-r%d", g, i)})
			}
		}(g)
	}
	wg.Wait()
	w.Close() // flush loop stopped, remaining buffer drained synchronously

	msgs := prod.msgs()
	if len(msgs) != want {
		t.Fatalf("published %d records, want %d (loss or duplication under concurrency)", len(msgs), want)
	}
	seen := make(map[string]int, want)
	for _, m := range msgs {
		var tm struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(m.data, &tm); err != nil {
			t.Fatalf("unmarshal wire message: %v", err)
		}
		seen[tm.ID]++
	}
	if len(seen) != want {
		t.Fatalf("distinct request ids = %d, want %d — records bled or duplicated across the concurrent publish pool", len(seen), want)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("request id %s published %d times, want exactly 1", id, c)
		}
	}
}

// TestWriter_PublishRecord_DropsWhenBufferFull covers the data-loss
// accounting branch: when a publish fails and the buffer is already at
// maxQueueSize, the record is dropped (counted), not appended past the
// cap. White-box: the flush loop is stopped and the buffer pre-filled so
// the failed re-buffer deterministically hits the cap.
func TestWriter_PublishRecord_DropsWhenBufferFull(t *testing.T) {
	prod := &memProducer{alwaysFail: true}
	w := NewWriter(prod, "q", nil, slog.Default())
	w.Close() // stop the flush loop; drainBuffer leaves buf empty

	w.mu.Lock()
	w.buf = make([]*Record, maxQueueSize) // saturate
	w.mu.Unlock()

	w.publishRecord(&Record{RequestID: "overflow"}) // fails → rebuffer → cap → drop

	w.mu.Lock()
	got := len(w.buf)
	w.mu.Unlock()
	if got != maxQueueSize {
		t.Fatalf("buffer grew past cap on drop branch: %d, want %d", got, maxQueueSize)
	}
}

// TestWriter_RebufferOnTransientFailure proves a record that fails to
// publish is re-buffered and retried on the next flush rather than lost,
// even when the failure happens inside the concurrent publish pool.
func TestWriter_RebufferOnTransientFailure(t *testing.T) {
	prod := &memProducer{failCount: 3} // first 3 publishes fail, then recover
	w := NewWriter(prod, "q", nil, slog.Default())
	for i := range 5 {
		w.Enqueue(&Record{RequestID: fmt.Sprintf("r%d", i)})
	}
	w.Close() // drainBuffer retries the re-buffered records until they land
	if got := len(prod.msgs()); got != 5 {
		t.Fatalf("after transient failures + retry, published %d, want 5 (records lost instead of re-buffered)", got)
	}
}
