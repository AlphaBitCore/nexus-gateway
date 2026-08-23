package queue

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// Async writer behaviour: enqueue returns instantly, flush
// drains, close waits for drain, full channel drops with WARN, and
// concurrent enqueue from many goroutines doesn't lose events.

func TestQueueWriter_FlushBlocksUntilCommitted(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	// Long flushInterval so the batch is committed only via Flush
	// barrier (proves Flush is what guarantees persistence).
	w := NewQueueWriterWithOptions(q, 256, 100, time.Hour)
	defer func() { _ = w.Close(context.Background()) }()

	for i := range 25 {
		w.Enqueue(sharedaudit.AuditEvent{
			ID:         fmt.Sprintf("flush-%d", i),
			Timestamp:  time.Now().UTC(),
			TargetHost: "test.example.com",
		})
	}
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	_, total, err := q.QueryEvents("", "", 0, 100)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total != 25 {
		t.Errorf("post-Flush row count = %d, want 25", total)
	}
}

func TestQueueWriter_BatchSizeTrigger(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	// batch trip = 10 events; very long interval so only batch-size
	// can flush. After 10 enqueues the worker auto-commits without
	// any Flush call.
	w := NewQueueWriterWithOptions(q, 256, 10, time.Hour)
	defer func() { _ = w.Close(context.Background()) }()

	for i := range 10 {
		w.Enqueue(sharedaudit.AuditEvent{
			ID:         fmt.Sprintf("batch-%d", i),
			Timestamp:  time.Now().UTC(),
			TargetHost: "test.example.com",
		})
	}
	// Give worker a tick to consume + commit. Poll up to 1s for the
	// 10 rows to appear (no Flush — relies on batch-size trigger).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, total, _ := q.QueryEvents("", "", 0, 100)
		if total == 10 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("batch-size trigger did not commit within 1s")
}

func TestQueueWriter_IntervalTrigger(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	// batch=1000 (never tripped) + short 50 ms interval. After 1 enqueue
	// the ticker should commit within ~50 ms.
	w := NewQueueWriterWithOptions(q, 256, 1000, 50*time.Millisecond)
	defer func() { _ = w.Close(context.Background()) }()

	w.Enqueue(sharedaudit.AuditEvent{
		ID:         "interval-1",
		Timestamp:  time.Now().UTC(),
		TargetHost: "test.example.com",
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, total, _ := q.QueryEvents("", "", 0, 10)
		if total == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("interval trigger did not commit within 1s")
}

// TestQueueWriter_ChannelFull_DefaultModeLosesNothing replaces a test that asserted the
// OPPOSITE, and the replacement is the point of the change: this used to be
// TestQueueWriter_ChannelFullDrops, pinning "channel size 2 plus 5 enqueues must produce
// drops > 0" — correct when dropping was the only thing a full channel could do.
//
// The shipped default is now lossmode.Spill: overflow is written durably off the caller's
// goroutine. So the contract to pin is the stronger one — a full channel loses NOTHING — and
// the old assertion is now the DROP mode's test below.
//
// It drives handleOverflow rather than racing the flush loop, for the same reason the drop-mode
// test below does: enqueueing five events into a channel of two only overflows if the producer
// outruns the consumer on that machine, that run. On an idle machine it does; on a loaded CI
// runner the flush loop drains the channel first, nothing overflows, and the guard below fires
// with "the overflow path was never taken". That guard is doing its job — the test was racy, not
// the writer. The property being pinned belongs to the OVERFLOW POLICY, not to the scheduler,
// and handleOverflow is exactly the branch Enqueue takes when the select's default arm fires,
// which is what "the channel is full" means to this writer.
func TestQueueWriter_ChannelFull_DefaultModeLosesNothing(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	// batch=1000 (never tripped), interval=1h (never): nothing commits until Close.
	w := NewQueueWriterWithOptions(q, 2, 1000, time.Hour)

	const n = 5
	for i := range n {
		w.handleOverflow(w.buildRow(sharedaudit.AuditEvent{
			ID:         fmt.Sprintf("overflow-%d", i),
			Timestamp:  time.Now().UTC(),
			TargetHost: "test.example.com",
		}))
	}
	// Close drains both the channel and the overflow buffer, so after it every accepted event
	// must be on disk.
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := w.Drops(); got != 0 {
		t.Errorf("Drops() = %d, want 0. The default mode writes overflow durably; a drop here "+
			"means the agent is losing audit records under ordinary channel pressure.", got)
	}
	if got := w.OverflowWrites(); got != n {
		t.Errorf("OverflowWrites() = %d, want %d. Every event handed to the overflow path must be "+
			"durably written under the default mode; an exact count is asserted because a "+
			"\"> 0\" bar cannot tell a working overflow path from a partly working one.", got, n)
	}
	_, total, err := q.QueryEvents("", "", 0, 100)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total != n {
		t.Errorf("queue holds %d rows, want %d. Overflow events must reach sqlite, not just be "+
			"counted — a row that was accepted and never persisted is silent audit data loss.",
			total, n)
	}
}

// TestQueueWriter_DropModeDropsWhenTheChannelIsFull keeps the old behaviour
// under test, because it is still reachable — it is now a deliberate
// configuration rather than the only option, and a deployment that selects it
// must actually get it.
//
// It drives handleOverflow rather than racing the flush loop. The previous
// version enqueued five events into a channel of two and asserted a drop; the
// loop drains that channel into its batch, so whether anything dropped came
// down to whether the producer outran the consumer on that machine, that run.
// It usually did, which is the worst kind of flake: it passes often enough to
// look like a real assertion.
//
// The property is a property of the OVERFLOW POLICY, not of the scheduler.
// This exercises exactly the branch Enqueue takes when the select's default
// arm fires, which is what "the channel is full" means to this writer.
func TestQueueWriter_DropModeDropsWhenTheChannelIsFull(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriterWithOptions(q, 2, 1000, time.Hour).WithLossMode("drop")
	defer func() { _ = w.Close(context.Background()) }()

	const n = 5
	for i := range n {
		w.handleOverflow(w.buildRow(sharedaudit.AuditEvent{
			ID:         fmt.Sprintf("drop-%d", i),
			Timestamp:  time.Now().UTC(),
			TargetHost: "test.example.com",
		}))
	}

	if got := w.Drops(); got != n {
		t.Errorf("Drops() = %d, want %d; the mode must be honoured when it is chosen "+
			"explicitly, otherwise the vocabulary is decorative", got, n)
	}
	if got := w.OverflowWrites(); got != 0 {
		t.Errorf("OverflowWrites() = %d under lossMode=drop; drop must not quietly persist "+
			"through the durable path, or drop and spill are the same mode", got)
	}
}

// And the other half, which the old test could not state because it never
// knew whether the channel had actually filled: a channel with room does NOT
// drop, whatever the loss mode says. Drop mode is a saturation policy, not a
// licence to discard.
func TestQueueWriter_DropModeKeepsEverythingWhileThereIsRoom(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriterWithOptions(q, 256, 1000, time.Hour).WithLossMode("drop")

	const n = 7
	for i := range n {
		w.Enqueue(sharedaudit.AuditEvent{
			ID:         fmt.Sprintf("keep-%d", i),
			Timestamp:  time.Now().UTC(),
			TargetHost: "test.example.com",
		})
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Drops(); got != 0 {
		t.Errorf("Drops() = %d with a channel of 256 and %d events; drop mode must only "+
			"drop under saturation", got, n)
	}
	_, total, _ := q.QueryEvents("", "", 0, 100)
	if total != n {
		t.Errorf("persisted %d of %d; drop mode must still deliver what it accepted", total, n)
	}
}

func TestQueueWriter_CloseDrainsAndIsIdempotent(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriterWithOptions(q, 256, 1000, time.Hour)

	for i := range 7 {
		w.Enqueue(sharedaudit.AuditEvent{
			ID:         fmt.Sprintf("close-%d", i),
			Timestamp:  time.Now().UTC(),
			TargetHost: "test.example.com",
		})
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent: second Close is a no-op + returns nil.
	if err := w.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
	_, total, _ := q.QueryEvents("", "", 0, 100)
	if total != 7 {
		t.Errorf("Close did not drain remaining events: got %d, want 7", total)
	}
}

func TestQueueWriter_ConcurrentEnqueueAllPersisted(t *testing.T) {
	// Mirror real production: many parallel inspect goroutines each
	// call Enqueue. With async writer there is zero contention on
	// SQLite write lock — every event makes it through.
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriterWithOptions(q, 1024, 50, 10*time.Millisecond)
	defer func() { _ = w.Close(context.Background()) }()

	const goroutines = 32
	const perGoroutine = 10
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := range perGoroutine {
				w.Enqueue(sharedaudit.AuditEvent{
					ID:         fmt.Sprintf("conc-%d-%d", gid, i),
					Timestamp:  time.Now().UTC(),
					TargetHost: "test.example.com",
				})
			}
		}(g)
	}
	wg.Wait()
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	_, total, _ := q.QueryEvents("", "", 0, 1000)
	want := goroutines * perGoroutine
	if total != want && total != want-int(w.Drops()) {
		t.Errorf("total=%d drops=%d, want %d combined", total, w.Drops(), want)
	}
}

func TestRecordBatch_RoundTrip_TraceID(t *testing.T) {
	q := newTestQueue(t)
	events := []event.Event{
		{ID: "rb1", Timestamp: time.Now().UTC(), TargetHost: "x", Action: "inspect", TraceID: "trace-A"},
		{ID: "rb2", Timestamp: time.Now().UTC(), TargetHost: "y", Action: "inspect", TraceID: ""},
		{ID: "rb3", Timestamp: time.Now().UTC(), TargetHost: "z", Action: "deny", TraceID: "trace-C"},
	}
	if err := q.RecordBatch(events); err != nil {
		t.Fatalf("RecordBatch: %v", err)
	}
	rows, total, err := q.QueryEvents("", "", 0, 100)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total != 3 {
		t.Fatalf("want 3 rows, got %d", total)
	}
	seen := map[string]string{}
	for _, r := range rows {
		seen[r.ID] = r.TraceID
	}
	if seen["rb1"] != "trace-A" || seen["rb2"] != "" || seen["rb3"] != "trace-C" {
		t.Errorf("traceId round-trip mismatch: %v", seen)
	}
}

func TestRecordBatch_EmptyNoOp(t *testing.T) {
	q := newTestQueue(t)
	if err := q.RecordBatch(nil); err != nil {
		t.Errorf("RecordBatch(nil) should be no-op: %v", err)
	}
	if err := q.RecordBatch([]event.Event{}); err != nil {
		t.Errorf("RecordBatch(empty) should be no-op: %v", err)
	}
}

func TestQueueWriter_NilDrops(t *testing.T) {
	var w *QueueWriter
	if w.Drops() != 0 {
		t.Errorf("nil writer Drops should return 0")
	}
}

func TestQueueWriter_FlushOnClosedWriter(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := NewQueueWriterWithOptions(q, 256, 100, time.Hour)
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Flush after Close: returns nil (writer already drained).
	if err := w.Flush(context.Background()); err != nil {
		t.Errorf("Flush after Close: %v", err)
	}
}

func TestQueueWriter_FlushCtxCancelled(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	// Buffer 1, big batch + long interval — flushReq capacity is 4 but
	// once 4 outstanding flushes queue up, the 5th's send blocks. Use
	// ctx with immediate cancel to exercise the ctx.Done branch.
	w := NewQueueWriterWithOptions(q, 1, 1000, time.Hour)
	defer func() { _ = w.Close(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	// Either ctx.Err() is returned, or the request is served instantly.
	// Both outcomes are valid; we just need the ctx.Done branch covered.
	_ = w.Flush(ctx)
}

func TestRecordBatch_TxBeginErr_AfterClose(t *testing.T) {
	q := newTestQueue(t)
	_ = q.Close()
	err := q.RecordBatch([]event.Event{
		{ID: "x", Timestamp: time.Now().UTC(), TargetHost: "h", Action: "inspect"},
	})
	if err == nil {
		t.Errorf("RecordBatch on closed db should error")
	}
}

// Finding A-4: the flush interval is now a timer armed per batch rather than a standing
// ticker, so an idle writer wakes zero times instead of 600 times a minute at the production
// 100 ms interval. The idle saving itself is structural — a goroutine parked in select with no
// pending timer cannot wake — but two behaviours the change could plausibly break are NOT
// structural, and both fail silently: audit rows simply sit in memory instead of reaching the
// encrypted queue, with no error logged anywhere. Hence these two.

// TestQueueWriter_IntervalTriggerRearmsAfterFlush is the re-arm contract. A standing ticker
// re-arms itself; a per-batch timer has to be re-armed explicitly, and an implementation that
// arms once would flush the FIRST batch on the interval and then never flush again on the
// interval — every later batch would wait for a batch-size trip or for Close. The existing
// interval test only ever drives one batch, so it cannot see that.
func TestQueueWriter_IntervalTriggerRearmsAfterFlush(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	// Batch trip far above what is enqueued, so ONLY the interval can flush.
	w := NewQueueWriterWithOptions(q, 256, 1000, 50*time.Millisecond)
	defer func() { _ = w.Close(context.Background()) }()

	for round := range 3 {
		w.Enqueue(sharedaudit.AuditEvent{
			ID:         fmt.Sprintf("rearm-%d", round),
			Timestamp:  time.Now().UTC(),
			TargetHost: "test.example.com",
		})
		want := round + 1
		deadline := time.Now().Add(3 * time.Second)
		var total int
		for time.Now().Before(deadline) {
			var err error
			_, total, err = q.QueryEvents("", "", 0, 100)
			if err != nil {
				t.Fatalf("QueryEvents: %v", err)
			}
			if total >= want {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if total != want {
			t.Fatalf("after round %d the queue holds %d rows, want %d. The interval flush did "+
				"not re-arm: batch %d is still sitting in memory. Nothing errors on this path — "+
				"the rows just never reach the encrypted queue until Close.",
				round, total, want, round)
		}
	}
}

// TestQueueWriter_TrickleDoesNotPostponeFlush pins the latency bound. The timer must measure
// from the FIRST event of a batch; re-arming on every event instead — the obvious way to write
// this wrong — lets a steady trickle push the deadline out indefinitely, so under continuous
// light traffic the batch would never flush on the interval at all. That is precisely the
// agent's normal shape: a slow drip of intercepted requests, not bursts.
func TestQueueWriter_TrickleDoesNotPostponeFlush(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	const flushInterval = 100 * time.Millisecond
	w := NewQueueWriterWithOptions(q, 256, 1000, flushInterval)
	defer func() { _ = w.Close(context.Background()) }()

	// One event every quarter-interval for six intervals. If the deadline were pushed out per
	// event, nothing would ever commit while the trickle continues.
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(flushInterval / 4)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tick.C:
				w.Enqueue(sharedaudit.AuditEvent{
					ID:         fmt.Sprintf("trickle-%d", i),
					Timestamp:  time.Now().UTC(),
					TargetHost: "test.example.com",
				})
			}
		}
	}()
	defer close(stop)

	// Well inside the trickle, rows must already be landing.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, total, err := q.QueryEvents("", "", 0, 100); err != nil {
			t.Fatalf("QueryEvents: %v", err)
		} else if total > 0 {
			return // a flush happened while events kept arriving: the bound holds
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no rows committed during 3s of continuous light traffic at a 100ms flush interval. " +
		"The interval timer is being re-armed on every event instead of on the first event of a " +
		"batch, so a trickle postpones the flush forever and audit rows accumulate in memory " +
		"until Close — silently, since nothing on this path errors.")
}

// TestQueueWriter_FlushDeadlineRunsFromTheFirstEvent_NotATickBoundary is the guard for A-4's
// actual claim, which neither test above can see.
//
// A-4 replaced a standing 100 ms ticker with a timer armed only while a batch is pending, so an
// idle writer parks with no pending timer and wakes zero times a minute instead of 600. That is a
// battery claim on a laptop agent, and it has no direct observable: an empty-batch flush returns
// without touching the queue, so a reinstated ticker commits exactly the same rows. Both existing
// interval tests stay green against it.
//
// What a standing ticker DOES change observably is WHERE the flush deadline is measured from.
// A ticker fires on absolute boundaries, so an event arriving mid-interval commits at the next
// boundary — sooner than flushInterval after the event. A per-batch timer starts at the event, so
// the row commits flushInterval after it, every time, regardless of when it arrived.
//
// Under testing/synctest's fake clock that difference is exact rather than statistical: idle for
// 10.5 intervals, then enqueue. A per-batch timer commits at +1.0 interval; a standing ticker
// aligned anywhere would commit at +0.5.
func TestQueueWriter_FlushDeadlineRunsFromTheFirstEvent_NotATickBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newWriterAdapterTestQueue(t)
		const interval = 100 * time.Millisecond
		// Batch trip far above what is enqueued, so ONLY the interval can flush.
		w := NewQueueWriterWithOptions(q, 256, 1000, interval)
		defer func() { _ = w.Close(context.Background()) }()

		synctest.Wait()
		// Idle for a deliberately non-integer number of intervals, so a tick boundary and an
		// event-relative deadline cannot coincide.
		time.Sleep(interval*10 + interval/2)
		synctest.Wait()

		start := time.Now()
		w.Enqueue(sharedaudit.AuditEvent{
			ID:         "deadline-origin",
			Timestamp:  time.Now().UTC(),
			TargetHost: "test.example.com",
		})

		const step = interval / 20
		var elapsed time.Duration
		for range 60 {
			time.Sleep(step)
			synctest.Wait()
			_, total, err := q.QueryEvents("", "", 0, 100)
			if err != nil {
				t.Fatalf("QueryEvents: %v", err)
			}
			if total > 0 {
				elapsed = time.Since(start)
				break
			}
		}
		if elapsed == 0 {
			t.Fatal("the row never committed within 3 intervals of the event: the interval timer " +
				"was not armed by the first event of a batch at all")
		}
		if elapsed < interval-step {
			t.Fatalf("the row committed %v after its event, short of the %v interval. The flush "+
				"deadline is being measured from an absolute tick boundary rather than from the "+
				"first event of the batch — i.e. a standing ticker, which is the resident idle "+
				"wake-up A-4 removed.", elapsed, interval)
		}
		if elapsed > interval+2*step {
			t.Fatalf("the row committed %v after its event, well past the %v interval: the "+
				"worst-case flush latency A-4 promised to leave unchanged has regressed.",
				elapsed, interval)
		}
	})
}
