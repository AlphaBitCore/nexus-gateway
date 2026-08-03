package queue

import (
	"context"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/lossmode"
)

// Overflow-policy behaviour for the agent's queue writer.
//
// The writers here are built STRUCT-DIRECTLY rather than through NewQueueWriterWithOptions, so no
// flush loop and no overflow worker are running. That is deliberate: those goroutines consume the
// very channels a test needs to hold full, and racing them is how the equivalent
// compliance-proxy test first hung for ten minutes. The unit under test is the policy in
// handleOverflow, so the test drives that and owns the channels.
func overflowWriter(t *testing.T, mode string, q *Queue, chCap, overflowCap int) *QueueWriter {
	t.Helper()
	w := &QueueWriter{
		queue:      q,
		ch:         make(chan event.Event, chCap),
		done:       make(chan struct{}),
		flushBatch: 100,
		flushReq:   make(chan chan struct{}, 4),
		overflowCh: make(chan event.Event, overflowCap),
	}
	return w.WithLossMode(mode)
}

func overflowEvent(id string) event.Event {
	return event.Event{ID: id, Timestamp: time.Now().UTC(), TargetHost: "test.example.com"}
}

// TestOverflow_Spill_HandsToTheDurablePath is the shipped default's happy path: the event goes to
// the durable writer, NOT to a drop.
func TestOverflow_Spill_HandsToTheDurablePath(t *testing.T) {
	w := overflowWriter(t, "spill", newWriterAdapterTestQueue(t), 0, 1)

	w.handleOverflow(overflowEvent("evt-spill"))

	if got := w.Drops(); got != 0 {
		t.Fatalf("Drops() = %d, want 0 — spill must hand the event to the durable path, not lose it", got)
	}
	select {
	case got := <-w.overflowCh:
		if got.ID != "evt-spill" {
			t.Fatalf("durable buffer holds %q, want evt-spill", got.ID)
		}
	default:
		t.Fatal("the event never reached the durable overflow buffer")
	}
}

// TestOverflow_SpillBlock_BothBuffersSaturated_IsBoundedAndCounted is the one place the agent's
// host rule OVERRIDES a no-loss mode, and it must be bounded and counted rather than freezing the
// caller. CLAUDE.md forbids stalling the host path: the macOS network extension sits in the
// machine's outbound packet path, so audit bookkeeping may never hang it.
func TestOverflow_SpillBlock_BothBuffersSaturated_IsBoundedAndCounted(t *testing.T) {
	w := overflowWriter(t, "spillblock", newWriterAdapterTestQueue(t), 0, 0) // nothing can accept

	start := time.Now()
	w.handleOverflow(overflowEvent("evt-nowhere"))
	elapsed := time.Since(start)

	if elapsed < overflowBlockMaxWait {
		t.Fatalf("returned after %s, before the %s window — spillblock did not attempt to "+
			"back-pressure at all", elapsed, overflowBlockMaxWait)
	}
	if elapsed > overflowBlockMaxWait*4 {
		t.Fatalf("waited %s against a %s bound. Every arm of the agent's overflow policy must be "+
			"bounded: an unbounded wait here is a hung host network, not a slow request.",
			elapsed, overflowBlockMaxWait)
	}
	if got := w.Drops(); got != 1 {
		t.Fatalf("Drops() = %d, want 1 — when the host rule forces a no-loss mode to give up, the "+
			"loss must be counted rather than silently absorbed", got)
	}
}

// TestOverflow_Block_BoundedThenCounted covers the pure back-pressure mode with nowhere to land.
func TestOverflow_Block_BoundedThenCounted(t *testing.T) {
	w := overflowWriter(t, "block", newWriterAdapterTestQueue(t), 0, 0)

	start := time.Now()
	w.handleOverflow(overflowEvent("evt-block"))
	elapsed := time.Since(start)

	if elapsed < overflowBlockMaxWait || elapsed > overflowBlockMaxWait*4 {
		t.Fatalf("block waited %s, want roughly the %s bound", elapsed, overflowBlockMaxWait)
	}
	if got := w.Drops(); got != 1 {
		t.Fatalf("Drops() = %d, want 1", got)
	}
	if got := w.OverflowWrites(); got != 0 {
		t.Fatalf("OverflowWrites() = %d, want 0 — block must never touch the durable path", got)
	}
}

// TestOverflow_Block_SucceedsWhenRoomAppears is the other half: with room, block must land the
// event rather than counting a drop.
func TestOverflow_Block_SucceedsWhenRoomAppears(t *testing.T) {
	w := overflowWriter(t, "block", newWriterAdapterTestQueue(t), 1, 0)

	w.handleOverflow(overflowEvent("evt-lands"))

	if got := w.Drops(); got != 0 {
		t.Fatalf("Drops() = %d, want 0 — there was room, so nothing should have been lost", got)
	}
	if got := <-w.ch; got.ID != "evt-lands" {
		t.Fatalf("channel holds %q, want evt-lands", got.ID)
	}
}

// TestOverflow_DurableWriteFailure_CountsALoss covers writeThrough's error path. A closed queue is
// the realistic version of "the durable store refused": disk gone, db closed at shutdown. The
// event is genuinely lost at that point, so it must be counted — an uncounted loss here would be
// invisible, since a dropped audit record leaves no other trace.
func TestOverflow_DurableWriteFailure_CountsALoss(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	if err := q.Close(); err != nil {
		t.Fatalf("close queue: %v", err)
	}
	w := overflowWriter(t, "spill", q, 0, 1)

	w.writeThrough(overflowEvent("evt-write-fails"))

	if got := w.Drops(); got != 1 {
		t.Fatalf("Drops() = %d, want 1 when the durable write fails", got)
	}
	if got := w.OverflowWrites(); got != 0 {
		t.Fatalf("OverflowWrites() = %d, want 0 — a failed write must not be counted as a "+
			"successful durable write, which is exactly the lying-metric bug fixed in the "+
			"compliance proxy", got)
	}
}

// TestOverflow_DurableWriteSuccess_CountsAsOverflowWrite is the positive half of the same counter
// pair, so "under pressure but losing nothing" is observably distinct from "discarding records".
func TestOverflow_DurableWriteSuccess_CountsAsOverflowWrite(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := overflowWriter(t, "spill", q, 0, 1)

	w.writeThrough(overflowEvent("evt-write-ok"))

	if got := w.Drops(); got != 0 {
		t.Fatalf("Drops() = %d, want 0", got)
	}
	if got := w.OverflowWrites(); got != 1 {
		t.Fatalf("OverflowWrites() = %d, want 1", got)
	}
	_, total, err := q.QueryEvents("", "", 0, 10)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total != 1 {
		t.Fatalf("queue holds %d rows, want 1 — the counter must reflect a row that really landed", total)
	}
}

// TestOverflow_ClosingWriter_DoesNotServeOutTheWindow pins that the blocking arms observe Close.
// Without it, a shutdown with saturated buffers would wait the full window for every in-flight
// event, turning a clean stop into a slow one.
func TestOverflow_ClosingWriter_DoesNotServeOutTheWindow(t *testing.T) {
	w := overflowWriter(t, "spillblock", newWriterAdapterTestQueue(t), 0, 0)
	close(w.done)

	start := time.Now()
	w.handleOverflow(overflowEvent("evt-shutdown"))
	if elapsed := time.Since(start); elapsed >= overflowBlockMaxWait {
		t.Fatalf("waited %s during shutdown; the blocking arms must observe done and give up "+
			"immediately rather than serving out the %s window per event", elapsed, overflowBlockMaxWait)
	}
	if got := w.Drops(); got != 1 {
		t.Fatalf("Drops() = %d, want 1 — an event abandoned at shutdown is still a loss", got)
	}
}

// TestOverflow_ModeResolution covers the config-typo rule at the writer boundary, and pins the
// agent's deliberate default. The two must not be the same value by accident: an explicit choice
// (spill, for the host rule) and an unrecognised typo (the shared no-loss default) land in
// different places on purpose.
func TestOverflow_ModeResolution(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	for _, tc := range []struct {
		in   string
		want lossmode.Mode
	}{
		{"spillblock", lossmode.SpillBlock},
		{"block", lossmode.Block},
		{"spill", lossmode.Spill},
		{"drop", lossmode.Drop},
		{"", lossmode.SpillBlock},
		{"nonsense", lossmode.SpillBlock},
	} {
		w := overflowWriter(t, tc.in, q, 1, 1)
		if got := w.LossMode(); got != tc.want {
			t.Errorf("WithLossMode(%q) -> %q, want %q", tc.in, got, tc.want)
		}
	}

	// The SHIPPED default, which is not the shared default and should not silently become it.
	real := NewQueueWriterWithOptions(q, 4, 100, time.Hour)
	defer func() { _ = real.Close(context.Background()) }()
	if got := real.LossMode(); got != lossmode.Spill {
		t.Fatalf("the agent's shipped default is %q, want %q. It is spill on purpose: a no-loss "+
			"mode must block until the record is durable, and a SQLite write under WAL contention "+
			"can wait out the 5s busy_timeout — on the agent that is a frozen host network caused "+
			"by audit bookkeeping. Changing this default needs that argument answered.", got, lossmode.Spill)
	}
}

// TestOverflow_SpillBlock_FallsBackToTheMainChannel covers the arm that makes spillblock's
// back-pressure actually useful: the durable buffer is full, but the flush loop has drained a slot
// on the main channel, so the event lands there instead of being lost. Without this arm spillblock
// would give up whenever its relief buffer filled, which is the moment it is most needed.
func TestOverflow_SpillBlock_FallsBackToTheMainChannel(t *testing.T) {
	w := overflowWriter(t, "spillblock", newWriterAdapterTestQueue(t), 1, 0) // room on ch, none in overflow

	w.handleOverflow(overflowEvent("evt-main-channel"))

	if got := w.Drops(); got != 0 {
		t.Fatalf("Drops() = %d, want 0 — the main channel had room, so nothing should be lost", got)
	}
	select {
	case got := <-w.ch:
		if got.ID != "evt-main-channel" {
			t.Fatalf("channel holds %q, want evt-main-channel", got.ID)
		}
	default:
		t.Fatal("the event reached neither buffer despite room on the main channel")
	}
}

// TestOverflowWorker_DrainsOnShutdown pins that events already accepted for durable writing are
// persisted rather than abandoned when Close runs. Accepting an event and then discarding it at
// shutdown would be the worst kind of loss: the caller was told it was handled.
func TestOverflowWorker_DrainsOnShutdown(t *testing.T) {
	q := newWriterAdapterTestQueue(t)
	w := overflowWriter(t, "spill", q, 0, 4)
	w.startOverflowWorker()

	const n = 3
	for i := range n {
		w.overflowCh <- overflowEvent("evt-drain-" + string(rune('a'+i)))
	}
	close(w.done)
	w.wg.Wait() // the worker's drain must finish before it returns

	if got := w.Drops(); got != 0 {
		t.Fatalf("Drops() = %d, want 0 — accepted events must be written, not abandoned at shutdown", got)
	}
	_, total, err := q.QueryEvents("", "", 0, 10)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total != n {
		t.Fatalf("queue holds %d rows, want %d. The worker must drain its buffer on shutdown; an "+
			"event accepted for durable writing and then dropped is worse than a refused one, "+
			"because the caller was told it was handled.", total, n)
	}
}

// TestOverflow_Block_DuringShutdown_GivesUpImmediately covers blockForRoom's done arm. Same reason
// as the spillblock shutdown case: a stop with saturated buffers must not wait out the window once
// per in-flight event.
func TestOverflow_Block_DuringShutdown_GivesUpImmediately(t *testing.T) {
	w := overflowWriter(t, "block", newWriterAdapterTestQueue(t), 0, 0)
	close(w.done)

	start := time.Now()
	w.handleOverflow(overflowEvent("evt-block-shutdown"))
	if elapsed := time.Since(start); elapsed >= overflowBlockMaxWait {
		t.Fatalf("block waited %s during shutdown, want an immediate give-up", elapsed)
	}
	if got := w.Drops(); got != 1 {
		t.Fatalf("Drops() = %d, want 1 — an event abandoned at shutdown is still a loss", got)
	}
}
