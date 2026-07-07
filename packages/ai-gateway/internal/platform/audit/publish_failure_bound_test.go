package audit

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
)

// TestPublishFailure_SustainedOutage_BoundedNoCirculation pins the death-spiral
// fix. Under a SUSTAINED publish outage (every publish fails, e.g. a full NATS
// stream returning 503) with the consumer workers actively draining recCh, a naive
// re-queue always wins a queue slot, so the failed record would circulate on recCh
// FOREVER — pinning its marshaled copy and busy-spinning the workers (the observed
// 226MB-heap + CPU-thrash collapse). The fix bounds the in-memory retry to
// maxPublishRetries, then routes the record to the durable BATCHED spill (spillCh).
// This test proves the loop is bounded: every record lands durably on the spool
// within a bounded time (never lost, never circulating), and the failing producer
// accepts nothing.
func TestPublishFailure_SustainedOutage_BoundedNoCirculation(t *testing.T) {
	dir := t.TempDir()
	// Generous per-file + total quota so the outage records all fit durably.
	spill, err := sharedndjson.New(dir, "test", 4096, 64<<20, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	prod := &memProducer{alwaysFail: true}
	w := NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default()).
		WithNDJSONSpill(spill).Start()
	defer w.Close()

	const n = 50
	for i := range n {
		w.Enqueue(&Record{RequestID: fmt.Sprintf("stuck-%d", i), Timestamp: time.Now().Add(time.Duration(i))})
	}

	// Every record must land durably on the spool within a bounded time — proving the
	// bounded retry exits to the batched spill instead of circulating on recCh forever.
	deadline := time.Now().Add(5 * time.Second)
	for len(readSpool(t, dir, "test")) < n {
		if time.Now().After(deadline) {
			t.Fatalf("sustained-outage records must all spill durably (bounded circulation); "+
				"got %d, want %d", len(readSpool(t, dir, "test")), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(prod.msgs()); got != 0 {
		t.Errorf("alwaysFail producer must not record messages; got %d", got)
	}
}

// TestPublishFailure_TransientRetriesInMemory pins that a GENUINE transient failure
// (recovers within the retry cap) is still recovered IN MEMORY via the recCh
// re-queue — the fast-path preserved by the bounded-retry fix, not spilled to disk.
// A producer that fails a bounded number of times then succeeds must surface the
// record on the producer (re-published from recCh), with nothing on the spool.
func TestPublishFailure_TransientRetriesInMemory(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 4096, 64<<20, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	// Fail the first 2 publishes (< maxPublishRetries=3), then accept — a transient
	// blip that the in-memory re-queue must ride out without touching the spool.
	prod := &memProducer{failCount: 2}
	w := NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default()).
		WithNDJSONSpill(spill).Start()
	defer w.Close()

	w.Enqueue(&Record{RequestID: "transient", Timestamp: time.Now()})

	deadline := time.Now().Add(5 * time.Second)
	for len(prod.msgs()) < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("transient failure must recover in-memory and publish; got %d messages", len(prod.msgs()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A transient blip within the cap recovers in memory — it must NOT have spilled.
	if got := len(readSpool(t, dir, "test")); got != 0 {
		t.Errorf("transient (in-cap) failure must recover in-memory, not spill; spooled %d", got)
	}
}
