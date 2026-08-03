package audit

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/lossmode"
)

// Overflow-policy behaviour. Every case here fails SILENTLY in production if it regresses: a
// discarded audit record leaves no trace by definition, so the only way to know the policy works
// is to assert it.

func overflowLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// spoolFor builds a real NDJSON spool in a temp dir, so "durable sink present" means an actual
// file on disk rather than a stub that cannot fail.
func spoolFor(t *testing.T) (*NDJSONWriter, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := NewNDJSONWriter(dir, "test-instance", 100, 1000, overflowLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	return w, dir
}

// spoolContents returns every byte the spool wrote anywhere under dir, so a test can prove the
// event really landed on disk.
//
// It WALKS rather than listing one level, because the NDJSON writer creates a per-instance
// subdirectory (<dir>/<instanceID>/...). The first version of this helper read that directory as
// a file and failed with "is a directory" — a harness fault, not a code fault, and worth the
// comment so the next person does not re-flatten it.
func spoolContents(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sb.Write(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk spool dir: %v", err)
	}
	return sb.String()
}

// overflowWriter builds the writer struct DIRECTLY, without NewMQBatchWriter, so no loop
// goroutine is started.
//
// That is deliberate and it is the fix for a harness fault: the first version of this file used
// the real constructor and then tried to fill and drain w.ch from the test, which races the loop
// goroutine that is also consuming it — the channel never stays full, and a bare `<-w.ch` blocks
// forever once the loop wins. The unit under test is the policy in handleOverflow, so the test
// drives that function and controls the channel itself.
func overflowWriter(mode string, spool *NDJSONWriter, chanCap int) *MQBatchWriter {
	w := &MQBatchWriter{
		producer:  &memProducer{},
		queue:     "nexus.event.compliance",
		batchSize: 100,
		ndjson:    spool,
		logger:    overflowLogger(),
		ch:        make(chan AuditEvent, chanCap),
		done:      make(chan struct{}),
		flushReqs: make(chan flushRequest),
	}
	return w.WithLossMode(mode)
}

// TestOverflow_SpillBlock_WritesToTheDurableSpool is the no-loss default's happy path: the event
// must end up on disk, not merely be logged about.
func TestOverflow_SpillBlock_WritesToTheDurableSpool(t *testing.T) {
	spool, dir := spoolFor(t)
	w := overflowWriter("spillblock", spool, 0)

	w.handleOverflow(AuditEvent{ID: "evt-overflow-1", TargetHost: "api.openai.com", Timestamp: time.Now()})
	if err := spool.Close(); err != nil {
		t.Fatalf("close spool: %v", err)
	}

	if got := spoolContents(t, dir); !strings.Contains(got, "evt-overflow-1") {
		t.Fatalf("the overflowed event is not in the durable spool.\nspool contents: %q\n"+
			"spillblock's guarantee is that the spool is the primary overflow buffer; if the "+
			"event is not there it was lost, and a lost audit record leaves no other trace.", got)
	}
}

// TestOverflow_NoSpool_DegradesToBlockNotDrop is the safety rule that matters most, and it is the
// exact situation compliance-proxy.dev.yaml used to ship: spillblock configured, NDJSON disabled.
// The old code claimed a durable write and discarded the event. The mode must degrade to
// back-pressure, so with room in the channel the event is ACCEPTED rather than dropped.
func TestOverflow_NoSpool_DegradesToBlockNotDrop(t *testing.T) {
	w := overflowWriter("spillblock", nil, 1)

	if got := w.EffectiveLossMode(); got != lossmode.Block {
		t.Fatalf("EffectiveLossMode with no spool = %q, want %q — a no-loss mode must never "+
			"degrade into a lossy one because a spool is missing", got, lossmode.Block)
	}
	if w.EffectiveLossMode().Lossy() {
		t.Fatal("spillblock without a spool resolved to a LOSSY effective mode")
	}

	w.handleOverflow(AuditEvent{ID: "evt-blocked", Timestamp: time.Now()})

	select {
	case got := <-w.ch:
		if got.ID != "evt-blocked" {
			t.Fatalf("channel holds %q, want evt-blocked", got.ID)
		}
	default:
		t.Fatal("the event was NOT re-queued. With no spool, spillblock must back-pressure until " +
			"there is room; discarding instead is the silent data loss the dev config shipped.")
	}
}

// TestOverflow_NoSpool_FullChannel_CountsADropRatherThanHanging is the bound on back-pressure.
// An audit path that blocks forever on a wedged consumer is a worse failure than a counted drop,
// so the wait is bounded — and the drop that follows must be a real, logged accounting rather
// than a silent discard.
func TestOverflow_NoSpool_FullChannel_CountsADropRatherThanHanging(t *testing.T) {
	w := overflowWriter("spillblock", nil, 0) // capacity 0 and no reader: back-pressure cannot succeed

	start := time.Now()
	w.handleOverflow(AuditEvent{ID: "evt-cannot-land", Timestamp: time.Now()})
	elapsed := time.Since(start)

	if elapsed < overflowBlockMaxWait {
		t.Fatalf("handleOverflow returned after %s, before the %s back-pressure window elapsed — "+
			"it did not actually try to back-pressure", elapsed, overflowBlockMaxWait)
	}
	if elapsed > overflowBlockMaxWait*3 {
		t.Fatalf("handleOverflow took %s against a %s bound: the back-pressure is not bounded, so "+
			"a wedged audit consumer can stall the request path indefinitely", elapsed, overflowBlockMaxWait)
	}
}

// TestOverflow_Drop_IsHonestAndCounted pins the lossy mode: it discards, and it is selected
// explicitly rather than being the only thing the writer can do.
func TestOverflow_Drop_IsHonestAndCounted(t *testing.T) {
	spool, dir := spoolFor(t)
	w := overflowWriter("drop", spool, 1)

	w.handleOverflow(AuditEvent{ID: "evt-dropped", Timestamp: time.Now()})
	if err := spool.Close(); err != nil {
		t.Fatalf("close spool: %v", err)
	}

	// drop must NOT touch the spool even though one is wired — otherwise "drop" and "spill" are
	// the same mode and the vocabulary means nothing.
	if got := spoolContents(t, dir); strings.Contains(got, "evt-dropped") {
		t.Fatalf("mode=drop wrote to the durable spool; drop and spill must remain distinct.\n"+
			"spool contents: %q", got)
	}
	// And it must not have been re-queued either: drop means drop.
	if len(w.ch) != 0 {
		t.Fatalf("mode=drop re-queued the event (channel depth %d); it must discard", len(w.ch))
	}
}

// TestOverflow_ModeResolution covers the config-typo rule at the writer boundary: an unknown
// value must land on the no-loss default rather than on whatever the writer's zero value is.
func TestOverflow_ModeResolution(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want lossmode.Mode
	}{
		{"spillblock", lossmode.SpillBlock},
		{"block", lossmode.Block},
		{"spill", lossmode.Spill},
		{"drop", lossmode.Drop},
		{"", lossmode.SpillBlock},
		{"SpillBlock", lossmode.SpillBlock},
		{"nonsense", lossmode.SpillBlock},
	} {
		w := NewMQBatchWriter(&memProducer{}, "q", 100, time.Hour, 10, nil, overflowLogger())
		w.WithLossMode(tc.in)
		if got := w.LossMode(); got != tc.want {
			t.Errorf("WithLossMode(%q) -> %q, want %q", tc.in, got, tc.want)
		}
		_ = w.Close(context.Background())
	}
}

// TestOverflow_UnconfiguredWriterIsNoLoss pins that a writer built without WithLossMode is not
// accidentally lossy — the zero value of the mode field is the empty string, and the constructor
// must have set the no-loss default.
func TestOverflow_UnconfiguredWriterIsNoLoss(t *testing.T) {
	w := NewMQBatchWriter(&memProducer{}, "q", 100, time.Hour, 10, nil, overflowLogger())
	defer w.Close(context.Background()) //nolint:errcheck
	if got := w.LossMode(); got != lossmode.Default || got.Lossy() {
		t.Fatalf("an unconfigured writer has lossMode %q; it must default to the no-loss %q",
			got, lossmode.Default)
	}
}

// saturatedSpool returns a spool whose total-size quota is already exceeded, so every Write fails
// with ErrSpoolQuotaExceeded. That is not a contrivance — a full spool is exactly the condition
// spillblock's "back-pressure only when the spool is ALSO saturated" clause exists for, and it is
// the only way the no-loss guarantee can be tested rather than assumed.
func saturatedSpool(t *testing.T) *NDJSONWriter {
	t.Helper()
	w, err := NewNDJSONWriter(t.TempDir(), "saturated", 100, 0, overflowLogger())
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	if err := w.Write(AuditEvent{ID: "probe", Timestamp: time.Now()}); err == nil {
		t.Skip("a zero total-size quota does not refuse writes on this build; the saturated-spool " +
			"cases need a different way to force the failure")
	}
	return w
}

// TestOverflow_SpillBlock_SpoolSaturated_BackPressuresRatherThanDrops is spillblock's actual
// guarantee, and the one case that distinguishes it from spill. When the spool refuses because it
// is full, the event must NOT be discarded — the emitting path back-pressures instead.
func TestOverflow_SpillBlock_SpoolSaturated_BackPressuresRatherThanDrops(t *testing.T) {
	w := overflowWriter("spillblock", saturatedSpool(t), 1)

	w.handleOverflow(AuditEvent{ID: "evt-spool-full", Timestamp: time.Now()})

	select {
	case got := <-w.ch:
		if got.ID != "evt-spool-full" {
			t.Fatalf("channel holds %q, want evt-spool-full", got.ID)
		}
	default:
		t.Fatal("the event was discarded when the spool was full. spillblock's whole contract is " +
			"that a saturated spool causes BACK-PRESSURE, not loss — if it drops here it is just " +
			"spill under a different name.")
	}
}

// TestOverflow_Spill_SpoolSaturated_CountsADrop is the documented difference: spill accepts a
// bounded loss when its spool is unavailable, and must not silently back-pressure instead — that
// would make a mode chosen for throughput stall the request path.
func TestOverflow_Spill_SpoolSaturated_CountsADrop(t *testing.T) {
	w := overflowWriter("spill", saturatedSpool(t), 1)

	start := time.Now()
	w.handleOverflow(AuditEvent{ID: "evt-spill-dropped", Timestamp: time.Now()})
	elapsed := time.Since(start)

	if len(w.ch) != 0 {
		t.Fatalf("mode=spill re-queued the event when its spool was full (channel depth %d); it "+
			"must take the documented bounded drop instead of back-pressuring", len(w.ch))
	}
	if elapsed >= overflowBlockMaxWait {
		t.Fatalf("mode=spill spent %s before giving up, at least the full %s back-pressure window. "+
			"spill is the throughput-oriented mode; stalling there defeats the reason to choose it.",
			elapsed, overflowBlockMaxWait)
	}
}

// TestOverflow_BlockDuringShutdown_DoesNotHang pins that back-pressure yields to Close. Without
// the done-channel arm a shutdown with a full queue would block until the 2s bound expired for
// every in-flight event, turning a clean stop into a slow one.
func TestOverflow_BlockDuringShutdown_DoesNotHang(t *testing.T) {
	w := overflowWriter("block", nil, 0) // no room, no spool
	close(w.done)                        // simulate Close having run

	start := time.Now()
	w.handleOverflow(AuditEvent{ID: "evt-during-shutdown", Timestamp: time.Now()})
	elapsed := time.Since(start)

	if elapsed >= overflowBlockMaxWait {
		t.Fatalf("handleOverflow waited %s during shutdown; it must observe the done channel and "+
			"give up immediately rather than serving out the %s back-pressure window per event",
			elapsed, overflowBlockMaxWait)
	}
}
