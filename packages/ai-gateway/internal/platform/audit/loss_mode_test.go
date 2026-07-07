package audit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
	registry "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/prometheus/client_golang/prometheus"
)

// pooledFakeProducer implements mq.Producer + batchProducer + pooledBatchProducer
// so a Writer drives the pooled, framed publish path (NewWriter workers>1,
// batchPublish→EnqueueBatchAsyncOn, publishBatchOn framed, publishFramed).
type pooledFakeProducer struct {
	mu        sync.Mutex
	published [][]byte
	pool      int
}

func (p *pooledFakeProducer) Publish(context.Context, string, []byte) error { return nil }
func (p *pooledFakeProducer) Enqueue(context.Context, string, []byte) error { return nil }
func (p *pooledFakeProducer) Close() error                                  { return nil }
func (p *pooledFakeProducer) EnqueueBatchAsync(_ context.Context, _ string, b [][]byte) ([]error, error) {
	return make([]error, len(b)), nil
}
func (p *pooledFakeProducer) PoolSize() int { return p.pool }
func (p *pooledFakeProducer) EnqueueBatchAsyncOn(_ context.Context, _ string, b [][]byte, _ int) ([]error, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, fr := range b {
		p.published = append(p.published, append([]byte(nil), fr...))
	}
	return make([]error, len(b)), nil
}
func (p *pooledFakeProducer) recordCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, fr := range p.published {
		n += countFrameRecords(fr)
	}
	return n
}

// TestWriter_PooledFramedPublish drives the pooled multi-worker framed publish
// path end-to-end: every enqueued record reaches the producer.
func TestWriter_PooledFramedPublish(t *testing.T) {
	// Binary wire (production default); counts via magic+length-prefixed framing.
	t.Setenv("NEXUS_AUDIT_WIRE", "binary")
	p := &pooledFakeProducer{pool: 2}
	w := NewWriter(p, "nexus.event.ai-traffic", registry.NewRegistry(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithFramePublish(4096)
	if w.workers != 2 {
		t.Fatalf("pooled producer should yield 2 workers, got %d", w.workers)
	}
	w.Start()
	const n = 20
	for i := range n {
		w.Enqueue(&Record{RequestID: fmt.Sprintf("req-%d", i)})
	}
	w.Close() // drains all workers

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.recordCount() < n {
		time.Sleep(5 * time.Millisecond)
	}
	if got := p.recordCount(); got != n {
		t.Fatalf("pooled framed publish delivered %d of %d records", got, n)
	}
}

func quietWriter(reg *registry.Registry) *Writer {
	return NewWriter(nil, "nexus.event.ai-traffic", reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestWriter_WithLossMode_Selection: the overflow policy resolves to the named
// mode, and an empty/unknown value falls back to no-loss spillBlock — the durable
// spool is the primary overflow buffer and the request path is back-pressured only
// when it too saturates (audit must never silently turn lossy from a config typo;
// spillBlock is no-loss whenever a spool sink is wired, which prod/rig always do).
func TestWriter_WithLossMode_Selection(t *testing.T) {
	cases := map[string]string{
		lossModeSpill:      lossModeSpill,
		lossModeDrop:       lossModeDrop,
		lossModeSpillBlock: lossModeSpillBlock,
		"block":            lossModeBlock,
		"":                 lossModeSpillBlock,
		"garbage":          lossModeSpillBlock,
	}
	for in, want := range cases {
		if got := quietWriter(nil).WithLossMode(in).LossMode(); got != want {
			t.Errorf("WithLossMode(%q).LossMode()=%q want %q", in, got, want)
		}
	}
}

// TestWriter_EffectiveMaxQueue: WithMaxQueuedRecords sets the cap; 0 keeps the
// current value (default when never set).
func TestWriter_EffectiveMaxQueue(t *testing.T) {
	w := quietWriter(nil)
	// The default cap is the FIXED structural pointer-count depth — body-size-
	// independent by design (the byte budget is the memory bound, not this count).
	if c := w.effectiveMaxQueue(); c != recChStructuralCap {
		t.Fatalf("default cap = %d, want structural cap %d", c, recChStructuralCap)
	}
	w.WithMaxQueuedRecords(42)
	if w.effectiveMaxQueue() != 42 {
		t.Fatalf("cap after set = %d want 42", w.effectiveMaxQueue())
	}
	w.WithMaxQueuedRecords(0) // no-op: keeps 42
	if w.effectiveMaxQueue() != 42 {
		t.Fatalf("cap after 0 = %d want 42 (no-op)", w.effectiveMaxQueue())
	}
	w.maxQueued = 0 // unset → falls back to the package default
	if w.effectiveMaxQueue() != maxQueueSize {
		t.Fatalf("cap when unset = %d want %d", w.effectiveMaxQueue(), maxQueueSize)
	}
}

// TestBodyPool_AcquireReclaimRoundTrip exercises the pooled request/response body
// lifecycle: acquire, attach, reclaim (idempotent + nil-safe), and release edges.
func TestBodyPool_AcquireReclaimRoundTrip(t *testing.T) {
	if b, h := AcquireRequestBody(nil); b != nil || h != nil {
		t.Fatal("empty src must yield nil body + handle")
	}
	body, h := AcquireRequestBody([]byte("hello-body"))
	if string(body) != "hello-body" || h == nil {
		t.Fatalf("acquire = %q/%v", body, h)
	}
	rec := &Record{RequestID: "x", RequestBody: body}
	rec.AttachPooledRequestBody(h)

	rh := AcquireResponseBuffer()
	*rh = append(*rh, []byte("resp-body")...)
	rec.ResponseBody = *rh
	rec.AttachPooledResponseBody(rh)

	w := quietWriter(nil)
	w.reclaimRecordBody(rec)
	if rec.reqBodyHandle != nil || rec.respBodyHandle != nil {
		t.Fatal("handles must be nil after reclaim")
	}
	if rec.RequestBody != nil || rec.ResponseBody != nil {
		t.Fatal("body refs must be cleared after reclaim")
	}
	w.reclaimRecordBody(rec) // idempotent
	w.reclaimRecordBody(nil) // nil-safe

	releaseRequestBody(nil)  // nil-safe
	releaseResponseBody(nil) // nil-safe
	// Over-cap buffers are dropped to GC, never pooled (must not panic).
	bigReq := make([]byte, requestBodyPoolCap+1)
	releaseRequestBody(&bigReq)
	bigResp := make([]byte, responseBodyPoolCap+1)
	releaseResponseBody(&bigResp)
}

// TestWriter_SpillRecord_WritesDurably: spillRecord persists the record to the
// NDJSON sink and reports success; the sealed file carries it.
func TestWriter_SpillRecord_WritesDurably(t *testing.T) {
	w := quietWriter(registry.NewRegistry(prometheus.NewRegistry()))
	dir := t.TempDir()
	spool, err := sharedndjson.New(dir, "gw", 64, 4096, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w.WithNDJSONSpill(spool)
	if !w.spillRecord(&Record{RequestID: "r-durable"}) {
		t.Fatal("spillRecord should report success")
	}
	if err := spool.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	sealed, _ := spool.SealedFiles()
	if len(sealed) != 1 {
		t.Fatalf("want 1 sealed file, got %v", sealed)
	}
	b, _ := os.ReadFile(sealed[0])
	if !spoolDecodedContains(b, "r-durable") {
		t.Fatalf("spilled record missing: %q", b)
	}
}

// TestWriter_SpillRecord_NoSpoolReportsDrop: with no spool wired, spillRecord
// cannot persist and reports failure (a counted drop).
func TestWriter_SpillRecord_NoSpoolReportsDrop(t *testing.T) {
	w := quietWriter(registry.NewRegistry(prometheus.NewRegistry()))
	if w.spillRecord(&Record{RequestID: "r"}) {
		t.Fatal("spillRecord with no spool must report failure")
	}
}

// TestWriter_SpillOverflow_DropsWithoutSpool: in spill mode with no sink, an
// overflow record degrades to a counted drop rather than a panic.
func TestWriter_SpillOverflow_DropsWithoutSpool(t *testing.T) {
	w := quietWriter(registry.NewRegistry(prometheus.NewRegistry()))
	w.spillOverflow(&Record{RequestID: "r"}) // no spool, no spillCh send → dropOverflow
}

// TestWriter_SpillOverflow_SpillBlock_BackpressuresThenDrains: in lossModeSpillBlock,
// a full spill channel parks the caller (no drop) until a slot frees, then the
// record lands on the channel — the lossless back-pressure path, not a drop.
func TestWriter_SpillOverflow_SpillBlock_BackpressuresThenDrains(t *testing.T) {
	w := quietWriter(registry.NewRegistry(prometheus.NewRegistry()))
	w.WithLossMode(lossModeSpillBlock)
	spool, _ := sharedndjson.New(t.TempDir(), "gw", 64, 4096, nil)
	w.WithNDJSONSpill(spool)
	w.spillCh = make(chan *Record, 1)
	w.spillCh <- &Record{RequestID: "filler"} // channel now full

	done := make(chan struct{})
	go func() { w.spillOverflow(&Record{RequestID: "blocked"}); close(done) }()

	// Must be parked, not dropped: spillOverflow has not returned.
	select {
	case <-done:
		t.Fatal("spillOverflow returned on a full channel — it dropped instead of back-pressuring")
	case <-time.After(50 * time.Millisecond):
	}

	<-w.spillCh // free a slot → the parked send completes
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("spillOverflow did not unblock after a slot freed")
	}
	if got := <-w.spillCh; got.RequestID != "blocked" {
		t.Fatalf("spill channel got %q, want the back-pressured record", got.RequestID)
	}
}

// TestWriter_SpillOverflow_SpillBlock_SpillsOnShutdown: in lossModeSpillBlock, a
// full spill channel during shutdown (stopCh closed) takes the escape and spills
// the record durably instead of hanging or dropping.
func TestWriter_SpillOverflow_SpillBlock_SpillsOnShutdown(t *testing.T) {
	w := quietWriter(registry.NewRegistry(prometheus.NewRegistry()))
	w.WithLossMode(lossModeSpillBlock)
	dir := t.TempDir()
	spool, _ := sharedndjson.New(dir, "gw", 64, 4096, nil)
	w.WithNDJSONSpill(spool)
	w.spillCh = make(chan *Record, 1)
	w.spillCh <- &Record{RequestID: "filler"} // full, so the send would block
	close(w.stopCh)                           // shutting down → escape via spillRecord

	w.spillOverflow(&Record{RequestID: "r-shutdown"})
	if err := spool.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	sealed, _ := spool.SealedFiles()
	if len(sealed) != 1 {
		t.Fatalf("want 1 sealed file, got %v", sealed)
	}
	b, _ := os.ReadFile(sealed[0])
	if !spoolDecodedContains(b, "r-shutdown") {
		t.Fatalf("shutdown record not spilled durably: %q", b)
	}
}

// TestWriter_BlockEnqueue_SpillsOnShutdown: a blocking enqueue during shutdown
// (stopCh closed) spills the record durably instead of hanging or dropping.
func TestWriter_BlockEnqueue_SpillsOnShutdown(t *testing.T) {
	w := quietWriter(registry.NewRegistry(prometheus.NewRegistry()))
	dir := t.TempDir()
	spool, _ := sharedndjson.New(dir, "gw", 64, 4096, nil)
	w.WithNDJSONSpill(spool)
	close(w.stopCh) // shutting down: the blocking select takes the stopCh branch
	w.blockEnqueue(&Record{RequestID: "r-shutdown"})
	_ = spool.Rotate()
	sealed, _ := spool.SealedFiles()
	if len(sealed) != 1 {
		t.Fatalf("shutdown enqueue should spill, sealed=%v", sealed)
	}
	b, _ := os.ReadFile(sealed[0])
	if !spoolDecodedContains(b, "r-shutdown") {
		t.Fatalf("shutdown record not spilled: %q", b)
	}
}

// TestAppendPoisonFile_OpenError: a dead-letter write to an unopenable path
// surfaces the error (so the caller leaves the spool file, never losing the record).
func TestAppendPoisonFile_OpenError(t *testing.T) {
	if err := appendPoisonFile("/nonexistent-dir-xyzzy/foo.poison", []byte("x")); err == nil {
		t.Fatal("expected an open error for an unwritable path")
	}
}

// TestAppendPoisonFile_WritesLine: the happy path appends a newline-terminated
// record and fsyncs.
func TestAppendPoisonFile_WritesLine(t *testing.T) {
	p := t.TempDir() + "/audit-x.ndjson.poison"
	if err := appendPoisonFile(p, []byte(`{"id":"big"}`)); err != nil {
		t.Fatalf("appendPoisonFile: %v", err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != `{"id":"big"}`+"\n" {
		t.Fatalf("poison content = %q", b)
	}
}
