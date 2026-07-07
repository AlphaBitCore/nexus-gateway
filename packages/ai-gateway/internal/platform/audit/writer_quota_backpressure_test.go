package audit

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/prometheus/client_golang/prometheus"
)

// waitFor polls cond until it is true or the deadline elapses, returning cond's
// final value — used to synchronise on the async spill worker without a fixed sleep.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestSpillBlock_QuotaFull_BackPressuresThenSpills is the core no-loss proof for the
// spool-quota back-pressure fix: under lossModeSpillBlock, a spool AT its total-size
// quota must NOT drop the record (as it did before — the measured 253778-drop bug);
// the spill worker back-pressures (retries the same batch, incrementing the distinct
// spill_backpressure signal) until the recovery drain frees space, then the record
// spills durably. Zero drops throughout.
func TestSpillBlock_QuotaFull_BackPressuresThenSpills(t *testing.T) {
	prom := prometheus.NewRegistry()
	dir := t.TempDir()
	spool, err := sharedndjson.New(dir, "gw", 1, 1, nil) // 1 MB quota
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	// Seed a countable audit-*.ndjson file PAST the quota so the worker's first
	// WriteBatch is refused with ErrSpoolQuotaExceeded.
	seed := filepath.Join(dir, "gw", "audit-20260101-0001.ndjson")
	if err := os.WriteFile(seed, make([]byte, 1100*1024), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := NewWriter(nil, "q", opsmetrics.NewRegistry(prom), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool).WithLossMode(lossModeSpillBlock)
	w.spillFlush.targetBytes.Store(1) // flush on the first record
	w.Start()
	defer w.Close()

	w.spillCh <- &Record{RequestID: "bp-1", Timestamp: time.Unix(1700000000, 0).UTC()}

	// The worker hits the quota and back-pressures — a rising spill_backpressure
	// signal, and crucially NO drop.
	if !waitFor(2*time.Second, func() bool {
		return counterValue(t, prom, "nexus_audit_mq_spill_backpressure_total") >= 1
	}) {
		t.Fatal("spillBlock quota-full must raise the spill_backpressure signal")
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 0 {
		t.Fatalf("quota-full under spillBlock must back-pressure, not drop; dropped=%v", got)
	}
	if got := counterValue(t, prom, "nexus_audit_mq_spilled_total"); got != 0 {
		t.Fatalf("nothing may spill while the quota is full; spilled=%v", got)
	}

	// Free the quota — the recovery drain's real-world effect. The worker's next
	// retry now succeeds and the held record spills durably, still zero drops.
	if err := os.Remove(seed); err != nil {
		t.Fatalf("free quota: %v", err)
	}
	if !waitFor(3*time.Second, func() bool {
		return counterValue(t, prom, "nexus_audit_mq_spilled_total") >= 1
	}) {
		t.Fatal("record must spill durably once the quota frees")
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 0 {
		t.Fatalf("no record may be dropped across the whole back-pressure→spill cycle; dropped=%v", got)
	}
	if got := len(readSpool(t, dir, "gw")); got < 1 {
		t.Fatalf("the back-pressured record must be durably spooled after space frees; spooled=%d", got)
	}
}

// TestSpillBlock_QuotaFull_ShutdownDropsBounded pins the Close() escape: with the
// spool permanently at quota and no drain, the worker back-pressures — but Close()
// must still terminate in bounded time, converting the held batch to a single
// counted shutdown drop rather than hanging wg.Wait() forever.
func TestSpillBlock_QuotaFull_ShutdownDropsBounded(t *testing.T) {
	prom := prometheus.NewRegistry()
	dir := t.TempDir()
	spool, err := sharedndjson.New(dir, "gw", 1, 1, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	seed := filepath.Join(dir, "gw", "audit-20260101-0001.ndjson")
	if err := os.WriteFile(seed, make([]byte, 1100*1024), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := NewWriter(nil, "q", opsmetrics.NewRegistry(prom), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool).WithLossMode(lossModeSpillBlock)
	w.spillFlush.targetBytes.Store(1)
	w.Start()
	w.spillCh <- &Record{RequestID: "sd-1", Timestamp: time.Unix(1700000000, 0).UTC()}

	// Let the worker enter back-pressure, then Close with the quota still full.
	waitFor(2*time.Second, func() bool {
		return counterValue(t, prom, "nexus_audit_mq_spill_backpressure_total") >= 1
	})
	done := make(chan struct{})
	go func() { w.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() hung with the spool at quota — the shutdown escape is missing")
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got < 1 {
		t.Fatalf("a batch held at quota through shutdown must be a counted drop; dropped=%v", got)
	}
}

// TestSpillBlock_Close_DrainsSpillCh covers the shutdown-race sweep (N3): a record
// left in the spillCh buffer at Close (the primary spillBlock back-pressure path)
// must be drained to the durable spool, never silently lost. Deterministic: recCh is
// sized manually and the spill worker is NOT started, so wg.Wait() returns at once
// and the ONLY thing that can rescue the buffered record is Close's own spillCh
// drain — exactly the post-worker-exit race the sweep closes.
func TestSpillBlock_Close_DrainsSpillCh(t *testing.T) {
	dir := t.TempDir()
	spool, err := sharedndjson.New(dir, "gw", 64, 512, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(nil, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool).WithLossMode(lossModeSpillBlock)
	w.recCh = make(chan *Record, 1) // non-nil so Close proceeds to the drain; no worker started
	w.spillCh <- &Record{RequestID: "drain-me", Timestamp: time.Unix(1700000000, 0).UTC()}

	w.Close() // wg.Wait() returns immediately (no worker); drain(recCh)+drain(spillCh) rescue the record

	if got := len(readSpool(t, dir, "gw")); got != 1 {
		t.Fatalf("Close must drain a buffered spillCh record to the spool; spooled lines=%d, want 1", got)
	}
}
