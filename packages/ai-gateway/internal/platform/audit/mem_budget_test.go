package audit

// mem_budget_test.go — the byte-bounded audit queue's business contracts:
// reservations are taken at Enqueue from REAL body sizes and released exactly
// once at every terminal (published / durably spilled / dropped), no-loss modes
// back-pressure the producer on a full budget, lossy modes shed with a counted
// drop instead of blocking, and shutdown always unblocks a parked producer into
// a durable spill. The invariant asserted throughout: after all records reach a
// terminal, memBudget.InUse() == 0 — a leak here means permanent
// over-back-pressure; a double release means the OOM bound is fiction.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/bytebudget"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/prometheus/client_golang/prometheus"
)

// stallProducer implements mq.Producer + batchProducer; every publish parks on
// gate until the test closes it — simulating a broker that is up but not
// draining, so records HOLD their byte-budget reservations in flight.
type stallProducer struct {
	gate chan struct{}
	mu   sync.Mutex
	sent int
}

func (p *stallProducer) Publish(context.Context, string, []byte) error { return nil }
func (p *stallProducer) Enqueue(context.Context, string, []byte) error {
	<-p.gate
	p.mu.Lock()
	p.sent++
	p.mu.Unlock()
	return nil
}
func (p *stallProducer) Close() error { return nil }
func (p *stallProducer) EnqueueBatchAsync(_ context.Context, _ string, b [][]byte) ([]error, error) {
	<-p.gate
	p.mu.Lock()
	p.sent += len(b)
	p.mu.Unlock()
	return make([]error, len(b)), nil
}

// bodyRecord builds a record whose captured bodies weigh exactly n bytes — the
// value Enqueue must reserve against the byte budget.
func bodyRecord(id string, n int) *Record {
	return &Record{
		RequestID:   id,
		Timestamp:   time.Unix(1700000000, 0).UTC(),
		RequestBody: make([]byte, n),
	}
}

// tinyBudgetWriter rebinds the writer's byte budget to n bytes (tests must not
// depend on the machine's RAM); call before the first Enqueue/Start.
func tinyBudgetWriter(w *Writer, n int64) *Writer {
	w.memBudget = bytebudget.New(n, w.stopCh)
	return w
}

// Every published record must return its reservation: after the pipeline drains,
// InUse is exactly 0 — across BOTH publish paths (per-record fallback and the
// pooled framed batch path).
func TestMemBudget_ReleasedOnPublishOK(t *testing.T) {
	t.Run("per-record fallback", func(t *testing.T) {
		prod := &memProducer{}
		w := NewWriter(prod, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		for i := range 5 {
			w.Enqueue(bodyRecord(fmt.Sprintf("pub-%d", i), 1024))
		}
		if got := waitForMsgCount(prod, 5, 2*time.Second); got != 5 {
			t.Fatalf("published %d of 5", got)
		}
		w.Close()
		if got := w.memBudget.InUse(); got != 0 {
			t.Fatalf("InUse after publish-OK drain = %d, want 0 (reservation leak)", got)
		}
	})
	t.Run("pooled framed batch", func(t *testing.T) {
		p := &pooledFakeProducer{pool: 2}
		w := NewWriter(p, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithFramePublish(1 << 20)
		w.Start()
		for i := range 20 {
			w.Enqueue(bodyRecord(fmt.Sprintf("fr-%d", i), 2048))
		}
		w.Close()
		if got := p.recordCount(); got != 20 {
			t.Fatalf("framed publish delivered %d of 20", got)
		}
		if got := w.memBudget.InUse(); got != 0 {
			t.Fatalf("InUse after framed drain = %d, want 0 (reservation leak)", got)
		}
	})
}

// No-loss (block) mode: a full byte budget must PARK the producer — the
// back-pressure that bounds the audit heap — and wake it exactly when the
// in-flight record's publish completes and releases its bytes. The distinct
// mem_backpressure signal must record the wait.
func TestMemBudget_BlockModeBackpressuresAtFullBudget(t *testing.T) {
	prom := prometheus.NewRegistry()
	prod := &stallProducer{gate: make(chan struct{})}
	w := NewWriter(prod, "q", opsmetrics.NewRegistry(prom),
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithLossMode(lossModeBlock)
	tinyBudgetWriter(w, 64)
	w.Start()

	w.Enqueue(bodyRecord("holder", 64)) // fills the budget; its publish stalls on gate

	second := make(chan struct{})
	go func() {
		w.Enqueue(bodyRecord("parked", 64)) // must park until the holder releases
		close(second)
	}()
	select {
	case <-second:
		t.Fatal("Enqueue admitted past an exhausted byte budget (no back-pressure)")
	case <-time.After(100 * time.Millisecond):
	}
	if got := counterValue(t, prom, "nexus_audit_mem_backpressure_total"); got < 1 {
		t.Fatalf("mem_backpressure_total = %v, want >= 1 while parked", got)
	}

	close(prod.gate) // the holder publishes → releases 64 bytes → parked producer wakes
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("parked Enqueue did not wake after the in-flight record released its bytes")
	}
	w.Close()
	if got := w.memBudget.InUse(); got != 0 {
		t.Fatalf("InUse after drain = %d, want 0", got)
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 0 {
		t.Fatalf("dropped_total = %v, want 0 (block mode is no-loss)", got)
	}
}

// Lossy modes must NEVER block the request path: a full byte budget is an
// immediate counted drop (handing an unaccounted record onward would re-open
// the unbounded-memory hole the budget closes).
func TestMemBudget_LossyModeShedsInsteadOfBlocking(t *testing.T) {
	prom := prometheus.NewRegistry()
	prod := &stallProducer{gate: make(chan struct{})}
	w := NewWriter(prod, "q", opsmetrics.NewRegistry(prom),
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithLossMode(lossModeDrop)
	tinyBudgetWriter(w, 64)
	w.Start()

	w.Enqueue(bodyRecord("holder", 64)) // fills the budget

	returned := make(chan struct{})
	go func() {
		w.Enqueue(bodyRecord("shed", 64)) // budget full → counted drop, no wait
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("lossy-mode Enqueue blocked on a full byte budget (must shed instead)")
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 1 {
		t.Fatalf("dropped_total = %v, want exactly 1 (the shed record)", got)
	}
	// The budget-exhausted event is counted even in lossy modes, so an operator
	// can tell a budget-full shed from a plain queue-full drop.
	if got := counterValue(t, prom, "nexus_audit_mem_backpressure_total"); got != 1 {
		t.Fatalf("mem_backpressure_total = %v, want 1 (budget-full shed must be distinguishable)", got)
	}
	close(prod.gate)
	w.Close()
	if got := w.memBudget.InUse(); got != 0 {
		t.Fatalf("InUse after drain = %d, want 0", got)
	}
}

// The spill worker batches records and discards the *Record after marshal — the
// reservation must transfer to the batch aggregate and be released at the flush
// terminal (spilled durably), never leaked and never double-released.
func TestMemBudget_SpillFlushReleasesBatchAggregate(t *testing.T) {
	prom := prometheus.NewRegistry()
	spool, err := sharedndjson.New(t.TempDir(), "gw", 64, 1, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(nil, "q", opsmetrics.NewRegistry(prom),
		slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool).WithLossMode(lossModeSpillBlock)
	tinyBudgetWriter(w, 1<<20)
	w.spillFlush.targetBytes.Store(1) // flush on the first record
	w.Start()
	defer w.Close()

	// Reserve exactly as Enqueue would, then hand the record straight to the
	// spill worker (the overflow hand-off path).
	rec := bodyRecord("sp-1", 4096)
	if !w.memBudget.Acquire(4096) {
		t.Fatal("Acquire on an empty budget must admit")
	}
	rec.reserved = 4096
	w.spillCh <- rec

	if !waitFor(2*time.Second, func() bool {
		return counterValue(t, prom, "nexus_audit_mq_spilled_total") >= 1
	}) {
		t.Fatal("spill worker did not durably write the record")
	}
	if !waitFor(2*time.Second, func() bool { return w.memBudget.InUse() == 0 }) {
		t.Fatalf("InUse after spill flush = %d, want 0 (batch aggregate not released)", w.memBudget.InUse())
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 0 {
		t.Fatalf("dropped_total = %v, want 0", got)
	}
}

// Shutdown must unblock a producer parked on the byte budget and still lose
// nothing: the record that could not be admitted spills durably instead.
func TestMemBudget_ShutdownUnblocksParkedEnqueueIntoDurableSpill(t *testing.T) {
	prom := prometheus.NewRegistry()
	spool, err := sharedndjson.New(t.TempDir(), "gw", 64, 1, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	prod := &stallProducer{gate: make(chan struct{})}
	w := NewWriter(prod, "q", opsmetrics.NewRegistry(prom),
		slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool).WithLossMode(lossModeBlock)
	tinyBudgetWriter(w, 64)
	w.Start()

	w.Enqueue(bodyRecord("holder", 64)) // fills the budget; publish stalls

	parked := make(chan struct{})
	go func() {
		w.Enqueue(bodyRecord("straggler", 64)) // parks on the full budget
		close(parked)
	}()
	select {
	case <-parked:
		t.Fatal("Enqueue admitted past an exhausted budget")
	case <-time.After(100 * time.Millisecond):
	}

	closed := make(chan struct{})
	go func() { w.Close(); close(closed) }()
	select {
	case <-parked: // stopCh fired → Acquire returns false → durable spill, Enqueue returns
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not unblock the parked Enqueue")
	}
	close(prod.gate) // let the in-flight holder publish so Close can finish draining
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not complete after the gate opened")
	}
	if got := counterValue(t, prom, "nexus_audit_mq_spilled_total"); got < 1 {
		t.Fatalf("spilled_total = %v, want >= 1 (the straggler must spill durably, not vanish)", got)
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 0 {
		t.Fatalf("dropped_total = %v, want 0 (shutdown must not lose the straggler)", got)
	}
	if got := w.memBudget.InUse(); got != 0 {
		t.Fatalf("InUse after shutdown = %d, want 0", got)
	}
}

// handlePublishFailure terminals must release the reservation exactly once —
// each of its four resolutions is driven directly on an UNSTARTED writer (nil
// recCh → the in-memory re-queue is never ready, so the routing is
// deterministic) with a pre-reserved record, then the budget must read 0.
func TestMemBudget_PublishFailureTerminalsRelease(t *testing.T) {
	newRec := func(w *Writer) *Record {
		rec := bodyRecord("pf", 128)
		if !w.memBudget.Acquire(128) {
			t.Fatal("Acquire on an empty budget must admit")
		}
		rec.reserved = 128
		rec.publishRetries = maxPublishRetries // next failure exhausts the in-memory retry
		return rec
	}

	t.Run("no spool: counted drop releases", func(t *testing.T) {
		w := NewWriter(nil, "q", nil,
			slog.New(slog.NewTextHandler(io.Discard, nil))).WithLossMode(lossModeDrop)
		tinyBudgetWriter(w, 1<<20)
		rec := newRec(w)
		w.handlePublishFailure([]byte("{}"), rec)
		if got := w.memBudget.InUse(); got != 0 {
			t.Fatalf("InUse after nil-spool drop = %d, want 0", got)
		}
	})

	t.Run("spillCh hand-off keeps the reservation", func(t *testing.T) {
		w := NewWriter(nil, "q", nil,
			slog.New(slog.NewTextHandler(io.Discard, nil))).WithLossMode(lossModeSpill)
		spool, err := sharedndjson.New(t.TempDir(), "gw", 64, 1, nil)
		if err != nil {
			t.Fatalf("ndjson.New: %v", err)
		}
		w.WithNDJSONSpill(spool)
		rec := newRec(w)
		w.handlePublishFailure([]byte("{}"), rec)
		// The record now sits in spillCh with its reservation intact — the spill
		// worker's flush is the terminal, not this hand-off.
		if got := w.memBudget.InUse(); got != 128 {
			t.Fatalf("InUse after spillCh hand-off = %d, want 128 (still pinned)", got)
		}
		if rec.reserved != 128 {
			t.Fatalf("reserved after hand-off = %d, want 128", rec.reserved)
		}
	})

	t.Run("saturated spill worker, lossy: durable spillData releases", func(t *testing.T) {
		w := NewWriter(nil, "q", nil,
			slog.New(slog.NewTextHandler(io.Discard, nil))).WithLossMode(lossModeSpill)
		spool, err := sharedndjson.New(t.TempDir(), "gw", 64, 1, nil)
		if err != nil {
			t.Fatalf("ndjson.New: %v", err)
		}
		w.WithNDJSONSpill(spool)
		w.spillCh = make(chan *Record) // unbuffered + no worker = saturated hand-off
		rec := newRec(w)
		w.handlePublishFailure([]byte("{}"), rec)
		if got := w.memBudget.InUse(); got != 0 {
			t.Fatalf("InUse after lossy spillData terminal = %d, want 0", got)
		}
	})

	t.Run("saturated spill worker, spillBlock at shutdown: one-shot spill releases", func(t *testing.T) {
		w := NewWriter(nil, "q", nil,
			slog.New(slog.NewTextHandler(io.Discard, nil))).WithLossMode(lossModeSpillBlock)
		spool, err := sharedndjson.New(t.TempDir(), "gw", 64, 1, nil)
		if err != nil {
			t.Fatalf("ndjson.New: %v", err)
		}
		w.WithNDJSONSpill(spool)
		w.spillCh = make(chan *Record) // saturated park target
		close(w.stopCh)                // shutdown escapes the park
		rec := newRec(w)
		w.handlePublishFailure([]byte("{}"), rec)
		if got := w.memBudget.InUse(); got != 0 {
			t.Fatalf("InUse after spillBlock shutdown terminal = %d, want 0", got)
		}
	})
}

// Records with no captured bodies never touch the budget: even a 1-byte budget
// admits them without blocking (the bound is on body bytes, not record count).
func TestMemBudget_BodylessRecordsBypassAccounting(t *testing.T) {
	prod := &memProducer{}
	w := NewWriter(prod, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithLossMode(lossModeBlock)
	tinyBudgetWriter(w, 1)
	for i := range 10 {
		w.Enqueue(&Record{RequestID: fmt.Sprintf("meta-%d", i)})
	}
	if got := waitForMsgCount(prod, 10, 2*time.Second); got != 10 {
		t.Fatalf("published %d of 10 bodyless records", got)
	}
	w.Close()
	if got := w.memBudget.InUse(); got != 0 {
		t.Fatalf("InUse = %d, want 0 (bodyless records must not reserve)", got)
	}
}
