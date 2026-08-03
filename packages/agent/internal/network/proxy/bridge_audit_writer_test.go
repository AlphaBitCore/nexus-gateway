package proxy

import (
	"context"
	"errors"
	"net"
	"runtime"
	"testing"
	"time"
)

// TestBumpFlow_DoesNotSpawnPerFlowAuditGoroutines is the regression guard for a
// measured leak: BumpFlow used to build a whole audit QueueWriter — a 4096-slot event
// channel, a 256-slot overflow channel and TWO goroutines — for every
// intercepted TLS connection, and never close it. On a live containerized agent
// that measured as exactly +2 goroutines and +3.8 MB of heap per flow, held for
// the life of the daemon, on the end user's own laptop.
//
// The fix is structural: the writer is built once at wiring time and handed to
// BumpFlow as BridgeDeps.AuditWriter, so BumpFlow has nothing to construct. This
// test pins the OBSERVABLE consequence rather than the structure, because the
// structure can be undone by one line: drive the same deps through many flows
// and require that the goroutine count does not grow with the flow count.
//
// The bound is deliberately loose (< flows, i.e. < 1 per flow, against the old
// behaviour's 2 per flow). Go's runtime carries transient goroutines — a settling
// GC, a timer, the mocked dial's teardown — so a tight equality would be flaky in
// the direction that wastes time, while any per-flow writer trips this bound at
// double it.
func TestBumpFlow_DoesNotSpawnPerFlowAuditGoroutines(t *testing.T) {
	const flows = 8

	eng := newTestEngine(t)
	up := newTestUpstream(t)
	writer := newTestAuditWriter(t)
	installMockUpstream(t, nil, errors.New("mock upstream refused"))

	deps := BridgeDeps{
		TLSEngine:   eng,
		Upstream:    up,
		AuditWriter: writer,
	}

	// One warm-up flow so lazily-initialized runtime machinery (the TLS engine's
	// cert cache, the mock transport) is not counted as per-flow growth.
	runFlow(t, deps, "fl-warmup")

	before := settledGoroutines()
	for i := range flows {
		runFlow(t, deps, "fl-"+string(rune('a'+i)))
	}
	after := settledGoroutines()

	if grew := after - before; grew >= flows {
		t.Errorf("goroutines grew by %d across %d flows (before=%d after=%d); "+
			"a per-flow audit writer is back — see BridgeDeps.AuditWriter",
			grew, flows, before, after)
	}
}

// runFlow drives one BumpFlow to completion on a TLS port. The peer never speaks
// TLS and the fallback dial is mocked to fail, so the call returns an error after
// traversing the whole option-building block — which is where the per-flow writer
// used to be constructed.
func runFlow(t *testing.T, deps BridgeDeps, flowID string) {
	t.Helper()
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	_ = client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := BumpFlow(ctx, server, []byte("PEEKED"), "127.0.0.1", 443, flowID, FlowProcess{}, deps); err == nil {
		t.Fatalf("%s: expected an error when both bump and fallback dial fail", flowID)
	}
}

// settledGoroutines gives finished flows a moment to unwind before counting, so
// the number reflects what is retained rather than what is still exiting.
func settledGoroutines() int {
	for range 5 {
		runtime.Gosched()
		time.Sleep(20 * time.Millisecond)
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	return runtime.NumGoroutine()
}
