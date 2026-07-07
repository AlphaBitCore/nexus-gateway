package audit

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// batchMemProducer implements mq.Producer AND the optional batchProducer
// capability, recording dispatched batches and optionally failing specific
// per-message indices or the whole batch.
type batchMemProducer struct {
	mu      sync.Mutex
	batches [][][]byte
	failIdx map[int]bool
	topErr  error
}

func (p *batchMemProducer) Publish(context.Context, string, []byte) error { return nil }
func (p *batchMemProducer) Enqueue(context.Context, string, []byte) error { return nil }
func (p *batchMemProducer) Close() error                                  { return nil }

func (p *batchMemProducer) EnqueueBatchAsync(_ context.Context, _ string, batch [][]byte) ([]error, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([][]byte, len(batch))
	copy(cp, batch)
	p.batches = append(p.batches, cp)
	if p.topErr != nil {
		return nil, p.topErr
	}
	errs := make([]error, len(batch))
	for i := range batch {
		if p.failIdx[i] {
			errs[i] = errors.New("simulated nak")
		}
	}
	return errs, nil
}

// TestPublishBatch_PrefersAsyncBatchPath: when the producer supports
// EnqueueBatchAsync, a consumer's batch publish dispatches the whole batch as ONE
// async batch (not per-record) — the amortised-ack-barrier win.
func TestPublishBatch_PrefersAsyncBatchPath(t *testing.T) {
	bp := &batchMemProducer{}
	w := NewWriter(bp, "q", nil, slog.Default())
	w.publishBatchOn(0, []*Record{
		{RequestID: "a", Timestamp: time.Now()},
		{RequestID: "b", Timestamp: time.Now()},
	})
	if len(bp.batches) != 1 {
		t.Fatalf("publish should dispatch ONE async batch, got %d", len(bp.batches))
	}
	if len(bp.batches[0]) != 2 {
		t.Fatalf("batch should carry both records, got %d", len(bp.batches[0]))
	}
}

// TestPublishBatch_PerMessageFailureReQueues: a per-message async nak must
// re-queue exactly that record onto the bounded queue (never a silent loss);
// successes do not.
func TestPublishBatch_PerMessageFailureReQueues(t *testing.T) {
	bp := &batchMemProducer{failIdx: map[int]bool{1: true}}
	w := NewWriter(bp, "q", nil, slog.Default())
	w.recCh = make(chan *Record, 4) // sized so handlePublishFailure re-queues here
	recA := &Record{RequestID: "a", Timestamp: time.Now()}
	recB := &Record{RequestID: "b", Timestamp: time.Now()}
	w.publishBatchOn(0, []*Record{recA, recB})
	requeued := drainRecCh(w)
	if len(requeued) != 1 || requeued[0].RequestID != "b" {
		t.Fatalf("failed record b must re-queue; got=%v", requeued)
	}
}

// TestPublishBatch_TopErrorReQueuesAll: a fire-time batch error re-queues every
// record in the batch.
func TestPublishBatch_TopErrorReQueuesAll(t *testing.T) {
	bp := &batchMemProducer{topErr: errors.New("boom")}
	w := NewWriter(bp, "q", nil, slog.Default())
	w.recCh = make(chan *Record, 4)
	w.publishBatchOn(0, []*Record{
		{RequestID: "a", Timestamp: time.Now()},
		{RequestID: "b", Timestamp: time.Now()},
	})
	if requeued := drainRecCh(w); len(requeued) != 2 {
		t.Fatalf("top-level batch error must re-queue all records; got=%d", len(requeued))
	}
}

// TestConsumeLoop_CapsBatchAtMaxCount: a queue depth larger than batchMaxCount must
// be drained as multiple publishes each bounded by batchMaxCount (bounding the live
// marshaled bytes), not one giant materialized batch — the fix for the measured ~2x
// heap regression. The count-cap is the consumer's greedy-batch boundary, so this
// drives the real consumeLoop: pre-queue all records, then run one consumer that
// drains them.
func TestConsumeLoop_CapsBatchAtMaxCount(t *testing.T) {
	bp := &batchMemProducer{}
	w := NewWriter(bp, "q", nil, slog.Default())
	const n = 1000 // > batchMaxCount (512)
	// Pre-queue every record before any consumer runs, so the consumer greedily
	// absorbs exactly batchMaxCount into the first publish, then the remainder.
	w.recCh = make(chan *Record, n)
	for i := range n {
		w.recCh <- &Record{RequestID: fmt.Sprintf("r%d", i), Timestamp: time.Now()}
	}
	w.startOnce.Do(func() {}) // ensureStarted no-op; we own the consumer lifecycle
	w.wg.Add(1)
	go w.consumeLoop(0)
	// Wait until all n records have been dispatched across publishes.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		bp.mu.Lock()
		total := 0
		for _, b := range bp.batches {
			total += len(b)
		}
		bp.mu.Unlock()
		if total >= n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(w.stopCh)
	w.wg.Wait()

	bp.mu.Lock()
	defer bp.mu.Unlock()
	if len(bp.batches) < 2 {
		t.Fatalf("%d records (> cap %d) should split into >=2 publishes, got %d", n, batchMaxCount, len(bp.batches))
	}
	total := 0
	for _, b := range bp.batches {
		if len(b) > batchMaxCount {
			t.Fatalf("a publish carried %d records, exceeding the batchMaxCount cap %d", len(b), batchMaxCount)
		}
		total += len(b)
	}
	if total != n {
		t.Fatalf("count-capping lost records: dispatched %d, want %d", total, n)
	}
	if len(bp.batches[0]) != batchMaxCount {
		t.Fatalf("first chunk = %d, want %d", len(bp.batches[0]), batchMaxCount)
	}
	if len(bp.batches[1]) != n-batchMaxCount {
		t.Fatalf("second chunk = %d, want %d", len(bp.batches[1]), n-batchMaxCount)
	}
}

// TestConsumeLoop_LingerPublishesPartialBatch deterministically exercises the linger
// path: a partial batch (< batchMaxCount) that receives no more records publishes when
// the consumerLinger timer fires (the `case <-timer.C` branch + the arm branch). Without
// closing stopCh, the ONLY way the partial batch can publish is the linger timer, so a
// non-empty batch appearing proves that branch ran — no reliance on incidental timing.
func TestConsumeLoop_LingerPublishesPartialBatch(t *testing.T) {
	bp := &batchMemProducer{}
	w := NewWriter(bp, "q", nil, slog.Default())
	const n = 3 // < batchMaxCount, so it lingers rather than publishing on a full batch
	w.recCh = make(chan *Record, n)
	for i := range n {
		w.recCh <- &Record{RequestID: fmt.Sprintf("r%d", i), Timestamp: time.Now()}
	}
	w.wg.Add(1)
	go w.consumeLoop(0)

	// The partial batch can ONLY publish via the linger timer here (no full batch, no
	// stop). Wait comfortably longer than consumerLinger (100ms) for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		bp.mu.Lock()
		got := len(bp.batches)
		bp.mu.Unlock()
		if got >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(w.stopCh)
	w.wg.Wait()

	bp.mu.Lock()
	defer bp.mu.Unlock()
	if len(bp.batches) == 0 {
		t.Fatal("partial batch never published — the linger timer branch did not fire")
	}
	if len(bp.batches[0]) != n {
		t.Fatalf("linger batch = %d records, want %d", len(bp.batches[0]), n)
	}
}

// TestDrainOnStop_PublishesRemainderBoundedByMaxCount deterministically covers the
// shutdown drain (consumeLoop's stopCh case): the remainder is published, bounded at
// batchMaxCount per publish. Driving drainOnStop directly avoids the main-select race
// that made this branch flaky under CI when exercised through consumeLoop.
func TestDrainOnStop_PublishesRemainderBoundedByMaxCount(t *testing.T) {
	t.Run("partial remainder in one publish", func(t *testing.T) {
		bp := &batchMemProducer{}
		w := NewWriter(bp, "q", nil, slog.Default())
		const n = 5
		w.recCh = make(chan *Record, n)
		for i := range n {
			w.recCh <- &Record{RequestID: fmt.Sprintf("r%d", i), Timestamp: time.Now()}
		}
		w.drainOnStop(0, nil)
		bp.mu.Lock()
		defer bp.mu.Unlock()
		if len(bp.batches) != 1 || len(bp.batches[0]) != n {
			t.Fatalf("drainOnStop should publish one batch of %d, got %d batches", n, len(bp.batches))
		}
	})

	t.Run("queue over batchMaxCount splits, capped per publish", func(t *testing.T) {
		bp := &batchMemProducer{}
		w := NewWriter(bp, "q", nil, slog.Default())
		n := batchMaxCount + 88 // forces the in-drain full-batch publish + a final remainder
		w.recCh = make(chan *Record, n)
		for i := range n {
			w.recCh <- &Record{RequestID: fmt.Sprintf("r%d", i), Timestamp: time.Now()}
		}
		w.drainOnStop(0, nil)
		bp.mu.Lock()
		defer bp.mu.Unlock()
		total := 0
		for _, b := range bp.batches {
			if len(b) > batchMaxCount {
				t.Fatalf("a drain publish carried %d > cap %d", len(b), batchMaxCount)
			}
			total += len(b)
		}
		if total != n {
			t.Fatalf("drainOnStop lost records: published %d, want %d", total, n)
		}
		if len(bp.batches) < 2 || len(bp.batches[0]) != batchMaxCount {
			t.Fatalf("expected a full %d-record publish then the remainder, got batches=%d first=%d", batchMaxCount, len(bp.batches), len(bp.batches[0]))
		}
	})

	t.Run("lingering batch published even with empty queue", func(t *testing.T) {
		bp := &batchMemProducer{}
		w := NewWriter(bp, "q", nil, slog.Default())
		w.recCh = make(chan *Record, 1)
		w.drainOnStop(0, []*Record{{RequestID: "carried", Timestamp: time.Now()}})
		bp.mu.Lock()
		defer bp.mu.Unlock()
		if len(bp.batches) != 1 || len(bp.batches[0]) != 1 {
			t.Fatalf("a lingering batch must publish on stop even with an empty queue, got %d batches", len(bp.batches))
		}
	})
}

// TestConsumeLoop_FullBatchWhileArmedStopsTimer covers the publish()'s armed-cleanup
// branch: a partial batch arms the linger timer, then enough records arrive to fill a
// full batch — publishing it must Stop the armed timer (the `if armed` branch). The
// initial single record arms the timer (sleep ensures it armed before the flood), then a
// batchMaxCount flood drives the greedy-drain to a full publish while armed.
func TestConsumeLoop_FullBatchWhileArmedStopsTimer(t *testing.T) {
	bp := &batchMemProducer{}
	w := NewWriter(bp, "q", nil, slog.Default())
	w.recCh = make(chan *Record, batchMaxCount+8)
	w.wg.Add(1)
	go w.consumeLoop(0)

	// One record: the consumer absorbs it and ARMS the linger timer (1 < batchMaxCount).
	w.recCh <- &Record{RequestID: "arm", Timestamp: time.Now()}
	time.Sleep(30 * time.Millisecond) // << consumerLinger(100ms): armed, not yet fired

	// Flood a full batch: the recCh arm's greedy drain reaches batchMaxCount and publishes
	// WHILE armed → publish() takes the `if armed { timer.Stop() }` branch.
	for i := range batchMaxCount {
		w.recCh <- &Record{RequestID: fmt.Sprintf("f%d", i), Timestamp: time.Now()}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		bp.mu.Lock()
		var full bool
		for _, b := range bp.batches {
			if len(b) == batchMaxCount {
				full = true
			}
		}
		bp.mu.Unlock()
		if full {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(w.stopCh)
	w.wg.Wait()

	bp.mu.Lock()
	defer bp.mu.Unlock()
	var sawFull bool
	for _, b := range bp.batches {
		if len(b) == batchMaxCount {
			sawFull = true
		}
	}
	if !sawFull {
		t.Fatalf("expected a full %d-record publish while the linger timer was armed", batchMaxCount)
	}
}

// TestPublishBatch_ChunksByBytes: with a low byte cap the chunk boundary is
// driven by accumulated marshaled bytes, not count.
func TestPublishBatch_ChunksByBytes(t *testing.T) {
	defer func(c, b int) { batchMaxCount, batchMaxBytes = c, b }(batchMaxCount, batchMaxBytes)
	batchMaxCount = 100000 // high so count never trips
	batchMaxBytes = 300    // low so bytes trips after a record or two

	bp := &batchMemProducer{}
	w := NewWriter(bp, "q", nil, slog.Default())
	recs := make([]*Record, 8)
	for i := range recs {
		recs[i] = &Record{RequestID: fmt.Sprintf("rec-%d", i), Timestamp: time.Now()}
	}
	w.publishBatchOn(0, recs)
	if len(bp.batches) < 2 {
		t.Fatalf("a low byte cap must force multiple chunks; got %d batch(es)", len(bp.batches))
	}
	// every record still dispatched exactly once
	total := 0
	for _, b := range bp.batches {
		total += len(b)
	}
	if total != len(recs) {
		t.Fatalf("byte-chunking lost records: dispatched %d, want %d", total, len(recs))
	}
}

// TestClose_DrainsViaBatchPath: Close() must drain every queued record through the
// consumers' async-batch path (not silently drop them).
func TestClose_DrainsViaBatchPath(t *testing.T) {
	bp := &batchMemProducer{}
	w := NewWriter(bp, "q", nil, slog.Default())
	const n = 5
	for i := range n {
		w.Enqueue(&Record{RequestID: fmt.Sprintf("c%d", i), Timestamp: time.Now()})
	}
	w.Close()
	total := 0
	bp.mu.Lock()
	for _, b := range bp.batches {
		total += len(b)
	}
	bp.mu.Unlock()
	if total != n {
		t.Fatalf("Close should drain all %d records via the batch path, dispatched %d", n, total)
	}
}

// TestMarshalRecord_ByteIdentity: the refactored marshalRecord must produce
// bytes identical to json.Marshal of the wire message (the async batch carries
// the same bytes the sync path did — no wire change).
func TestMarshalRecord_ByteIdentity(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	rec := &Record{RequestID: "bi", Timestamp: time.Now(), StatusCode: 200, ModelName: "gpt-4o"}
	got, _, ok := w.marshalRecord(rec)
	if !ok {
		t.Fatal("marshalRecord failed")
	}
	want, err := json.Marshal(w.recordToMessage(rec))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("marshalRecord bytes != json.Marshal(msg):\n got=%s\nwant=%s", got, want)
	}
}
