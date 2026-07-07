package audit

import (
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"sync"
	"testing"
	"time"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/prometheus/client_golang/prometheus"
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

// TestWriter_BurstDrainsPromptly proves a burst of records is published promptly
// by the consumer workers — NOT held until any timer. The consumeLoop greedily
// batches whatever is queued and publishes immediately once a batch fills (or after
// consumerLinger for a partial), so a full burst drains well inside 2s. A regression
// that stalled the drain (e.g. a worker that waited for a long timer before
// publishing) would miss the deadline.
func TestWriter_BurstDrainsPromptly(t *testing.T) {
	prod := &memProducer{}
	w := NewWriter(prod, "q", nil, slog.Default())
	defer w.Close()

	const burst = 1000
	for i := range burst {
		w.Enqueue(&Record{RequestID: fmt.Sprintf("r%d", i)})
	}
	if got := waitForMsgCount(prod, burst, 2*time.Second); got != burst {
		t.Fatalf("consumer drain published %d within 2s, want %d (drain path not wired?)", got, burst)
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

// TestWriter_PublishRecord_DropsWhenQueueFullNoSpill covers the data-loss
// accounting branch: when a publish fails, the re-queue onto recCh is full, and no
// durable spill is wired, the record is a counted drop (handlePublishFailure →
// spillData(false) → incDropped) — never grown past the cap, never silently lost.
// White-box: a full bounded queue + no ndjsonSpill forces the drop branch
// deterministically.
func TestWriter_PublishRecord_DropsWhenQueueFullNoSpill(t *testing.T) {
	prom := prometheus.NewRegistry()
	prod := &memProducer{alwaysFail: true}
	w := NewWriter(prod, "q", opsmetrics.NewRegistry(prom), slog.Default())
	// A full bounded queue so handlePublishFailure's non-blocking re-queue fails,
	// and no ndjsonSpill wired so spillData returns false → the drop branch.
	w.recCh = make(chan *Record, 1)
	w.recCh <- &Record{RequestID: "filler"} // saturate (cap 1)

	w.publishRecord(&Record{RequestID: "overflow"}) // fails → re-queue full → no spill → drop

	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 1 {
		t.Fatalf("dropped_total = %v, want 1 (publish-failure overflow with no spill must count a drop)", got)
	}
	// The queue did not grow past its cap.
	if got := len(w.recCh); got != 1 {
		t.Fatalf("recCh grew past cap on drop branch: %d, want 1", got)
	}
}

// counterValue gathers the named counter's summed value from a Prometheus
// registry (across all label sets). Returns 0 when the series is absent.
func counterValue(t *testing.T, prom *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := prom.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var sum float64
		for _, m := range mf.GetMetric() {
			sum += m.GetCounter().GetValue()
		}
		return sum
	}
	return 0
}

// TestWriter_RequeueOnTransientFailure proves a record that fails to publish is
// re-queued onto recCh and retried by a consumer worker rather than lost, even when
// the failure happens inside the concurrent publish pool.
func TestWriter_RequeueOnTransientFailure(t *testing.T) {
	prod := &memProducer{failCount: 3} // first 3 publishes fail, then recover
	w := NewWriter(prod, "q", nil, slog.Default())
	defer w.Close()
	for i := range 5 {
		w.Enqueue(&Record{RequestID: fmt.Sprintf("r%d", i)})
	}
	// The consumers re-queue + retry the failed records until they all land.
	if got := waitForMsgCount(prod, 5, 2*time.Second); got != 5 {
		t.Fatalf("after transient failures + retry, published %d, want 5 (records lost instead of re-queued)", got)
	}
}

// WithMaxQueuedRecords overrides the in-heap buffer cap: a Writer configured with
// a small N back-pressures/spills at N, not the maxQueueSize default — the knob
// that bounds the audit body pool's working set. n<=0 keeps the default.
func TestWriter_WithMaxQueuedRecords_BoundsBuffer(t *testing.T) {
	// Default: no override → the FIXED structural pointer-count depth (the byte
	// budget bounds memory; the count cap is body-size-independent by design).
	wDef := NewWriter(&memProducer{}, "q", nil, slog.Default())
	def := wDef.effectiveMaxQueue()
	if def != recChStructuralCap {
		t.Fatalf("default effectiveMaxQueue = %d, want structural cap %d", def, recChStructuralCap)
	}
	wDef.Close()

	// n<=0 is ignored (keeps the structural default).
	if got := NewWriter(&memProducer{}, "q", nil, slog.Default()).WithMaxQueuedRecords(0).effectiveMaxQueue(); got != def {
		t.Fatalf("WithMaxQueuedRecords(0) changed the cap to %d, want structural default %d", got, def)
	}

	// A small override sizes the bounded queue at N: Start() sizes recCh from the
	// final maxQueued, and a saturated queue rejects the next non-blocking send
	// (the would-be drop/spill admission boundary).
	const n = 16
	w := NewWriter(&memProducer{}, "q", nil, slog.Default()).WithMaxQueuedRecords(n)
	if got := w.effectiveMaxQueue(); got != n {
		t.Fatalf("effectiveMaxQueue = %d, want override %d", got, n)
	}
	// Size recCh from the override WITHOUT starting consumers, so it cannot drain
	// under us, then saturate it to the cap.
	w.recCh = make(chan *Record, w.effectiveMaxQueue())
	for range n {
		w.recCh <- &Record{RequestID: "fill"}
	}
	if cap(w.recCh) != n {
		t.Fatalf("recCh cap = %d, want override %d", cap(w.recCh), n)
	}
	// A non-blocking send past the cap must be rejected (the overflow admission point).
	select {
	case w.recCh <- &Record{RequestID: "over"}:
		t.Fatal("non-blocking send succeeded past the WithMaxQueuedRecords cap")
	default:
	}
	if got := len(w.recCh); got != n {
		t.Fatalf("queue grew past override cap: %d, want %d", got, n)
	}
}
