package audit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/bytebudget"
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

// TestWriter_SpillOverflow_DropsWithoutSpool: in the LOSSY spill mode with no
// sink, an overflow record degrades to a counted drop — which is the honest
// answer, because an async spill with nowhere to spill IS a drop
// (lossmode.WithoutDurableSink maps Spill -> Drop).
//
// The previous version of this test called spillOverflow and asserted NOTHING
// beyond "no panic", so it passed whether the record was dropped, spilled, or
// silently discarded. Note this arm is the ONLY one that reaches spillOverflow
// with a nil sink: spillblock is downgraded to block at Start, so it never gets
// here.
func TestWriter_SpillOverflow_DropsWithoutSpool(t *testing.T) {
	prom := prometheus.NewRegistry()
	w := quietWriter(registry.NewRegistry(prom))
	w.WithLossMode(lossModeSpill)

	w.spillOverflow(&Record{RequestID: "r"})

	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 1 {
		t.Fatalf("dropped counter = %v, want 1 — a lossy-mode drop that is not counted is invisible data loss", got)
	}
}

// spillblock with no durable spool must not lose records. The guard is at Start,
// not at overflow: ensureStarted downgrades the mode to block when no spool is
// wired, so Enqueue never routes to spillOverflow at all.
//
// This is deliberately driven through Enqueue rather than by calling
// spillOverflow directly. A direct call bypasses ensureStarted and therefore
// tests a branch production never reaches — which is exactly the mistake that
// produced an earlier version of this test, and with it a "fix" for a defect
// that did not exist.
func TestWriter_SpillBlockWithoutSpool_DowngradesAtStartAndDoesNotDrop(t *testing.T) {
	prom := prometheus.NewRegistry()
	w := quietWriter(registry.NewRegistry(prom))
	w.WithLossMode(lossModeSpillBlock)
	if w.ndjsonSpill != nil {
		t.Fatal("precondition: this test needs NO spool wired")
	}

	w.ensureStarted()

	if got := w.LossMode(); got != lossModeBlock {
		t.Fatalf("lossMode after Start = %q, want %q: spillblock without a spool must be downgraded to the "+
			"no-loss mode that needs no spool, or overflow would reach the lossy spill path", got, lossModeBlock)
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 0 {
		t.Fatalf("dropped counter = %v after a downgrade that discarded nothing", got)
	}
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

// TestWriter_SpillRecord_NoSpoolIsLoudNotJustCounted guards finding L-7.
//
// spillRecord's nil-spool arm incremented the drop counter and returned, with no
// log of any kind — while the arm right below it (a spool that exists but fails
// to write) has always logged a throttled WARN. So the ONE place a no-loss mode
// still loses a record was the quietest path in the writer: an operator saw a
// counter move and had nothing anywhere telling them why or what to change. The
// callers reaching this arm even describe it as "never a drop".
//
// The remedy string is asserted, not just the message: a drop line that does not
// say "configure a spool" leaves the operator with a symptom and no action, which
// is most of the way back to silence.
func TestWriter_SpillRecord_NoSpoolIsLoudNotJustCounted(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := NewWriter(nil, "nexus.event.ai-traffic", registry.NewRegistry(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, nil)))

	if w.spillRecord(&Record{RequestID: "r-no-spool"}) {
		t.Fatal("spillRecord with no spool must report failure")
	}

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	if out == "" {
		t.Fatal("the no-spool drop logged NOTHING: a counted-but-silent drop on the audit path is " +
			"the exact failure this program exists to remove — the counter moves and no line says why")
	}
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("the drop was not logged at ERROR: losing an audit record is not a warning-level "+
			"event on a compliance product. got: %s", out)
	}
	if !strings.Contains(out, "no durable spool") {
		t.Errorf("the log line does not name the CAUSE; got: %s", out)
	}
	if !strings.Contains(out, "audit.ndjson.enabled") {
		t.Errorf("the log line carries no remedy: an operator is left with a symptom and no action. "+
			"got: %s", out)
	}
	if !strings.Contains(out, "r-no-spool") {
		t.Errorf("the log line does not identify WHICH record was lost; got: %s", out)
	}
}

// A spool-write failure and a missing spool are different faults with different
// remedies, so they must not share a throttle: with one counter, a storm of either
// suppresses the FIRST occurrence of the other for dropLogEvery records.
func TestWriter_NoSpoolThrottleIsIndependentOfSpillFailureThrottle(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := NewWriter(nil, "nexus.event.ai-traffic", registry.NewRegistry(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, nil)))

	// Simulate a spill-failure storm having already consumed its own throttle, at an
	// offset that is NOT one short of a throttle boundary. dropLogEvery*3 exactly
	// looks like a valid setup and is not: sharing the counter would then take it to
	// 3*dropLogEvery+1, which still satisfies n%dropLogEvery == 1 and logs anyway, so
	// the test passed against the very mutation it exists to catch. Found by running
	// that mutation rather than by reading the test.
	w.spillLogCount.Store(dropLogEvery*3 + 5)

	if w.spillRecord(&Record{RequestID: "r-after-storm"}) {
		t.Fatal("spillRecord with no spool must report failure")
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "r-after-storm") {
		t.Errorf("the first no-spool drop was suppressed by the spill-failure throttle: the two "+
			"causes share a counter, so either one can hide the other's first occurrence. got: %s", out)
	}
}

// syncWriter serialises writes from the logger so a test reading the buffer
// cannot race the handler.
type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// The two no-loss modes promise that a record is never discarded. These tests
// pin the two escapes that would otherwise break that promise when the
// deployment has no writable spool — the state a container or unit without the
// audit-spool path lands in, and the state spillblock is downgraded into.

// A blocking enqueue with a full queue and NO durable spool must wait for a
// consumer rather than take a timed escape, because that escape would have to
// discard the record. Waiting is the contract; discarding is what the lossy
// modes are for.
func TestWriter_BlockEnqueue_WithoutSpool_WaitsInsteadOfDropping(t *testing.T) {
	prom := prometheus.NewRegistry()
	w := quietWriter(registry.NewRegistry(prom))
	w.WithLossMode(lossModeBlock)
	if w.ndjsonSpill != nil {
		t.Fatal("precondition: this test needs NO spool wired")
	}
	w.recCh = make(chan *Record, 1)
	w.recCh <- &Record{RequestID: "filler"} // queue now full

	// Shrink the bounded wait so the test can outlive it: the point is that with
	// no durable sink the wait must NOT be bounded at all, because its expiry
	// would have to discard the record.
	restore := backpressureMaxWait
	backpressureMaxWait = 20 * time.Millisecond
	defer func() { backpressureMaxWait = restore }()

	done := make(chan struct{})
	go func() { w.blockEnqueue(&Record{RequestID: "blocked"}); close(done) }()

	select {
	case <-done:
		t.Fatal("blockEnqueue returned on a full queue with no spool — a no-loss mode discarded a record")
	case <-time.After(20 * backpressureMaxWait):
	}

	<-w.recCh // a consumer frees a slot
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blockEnqueue did not unblock after a slot freed")
	}
	if got := <-w.recCh; got.RequestID != "blocked" {
		t.Fatalf("queue got %q, want the back-pressured record", got.RequestID)
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 0 {
		t.Fatalf("dropped counter = %v: a no-loss mode must not discard while waiting", got)
	}
}

// With the byte budget exhausted and a durable spool wired, a no-loss mode
// hands the record to the spool instead of parking the request goroutine. The
// budget bounds heap; the spool absorbs overflow. Parking while a sink sits
// idle costs the caller its admission slot for nothing.
func TestWriter_Enqueue_MemBudgetFull_WithSpool_SpillsWithoutParking(t *testing.T) {
	prom := prometheus.NewRegistry()
	w := quietWriter(registry.NewRegistry(prom))
	w.WithLossMode(lossModeSpillBlock)
	dir := t.TempDir()
	spool, err := sharedndjson.New(dir, "gw", 64, 4096, nil)
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	w.WithNDJSONSpill(spool)
	// The budget must be exhausted AND already holding something: an empty
	// pipeline lets TryAcquire through regardless of size, so that an oversized
	// record is never wedged forever.
	budget := bytebudget.New(16, w.stopCh)
	w.memBudget = budget
	if !budget.TryAcquire(16) {
		t.Fatal("precondition: could not exhaust the budget")
	}
	w.startOnce.Do(func() {}) // consume the start latch: this test drives Enqueue directly

	done := make(chan struct{})
	go func() {
		w.Enqueue(&Record{RequestID: "over-budget", ResponseBody: []byte("body-bytes")})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue parked on the byte budget while a durable spool was available")
	}

	if err := spool.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	sealed, _ := spool.SealedFiles()
	if len(sealed) != 1 {
		t.Fatalf("want 1 sealed spool file, got %v", sealed)
	}
	b, _ := os.ReadFile(sealed[0])
	if !spoolDecodedContains(b, "over-budget") {
		t.Fatalf("record not spilled durably: %q", b)
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 0 {
		t.Fatalf("dropped counter = %v: spilling is not dropping", got)
	}
}

// Same exhausted budget, but no spool: there is nowhere durable to put the
// record, so the mode's promise leaves only one option — wait. It must not
// discard, and it must complete once the budget frees.
func TestWriter_Enqueue_MemBudgetFull_WithoutSpool_WaitsInsteadOfDropping(t *testing.T) {
	prom := prometheus.NewRegistry()
	w := quietWriter(registry.NewRegistry(prom))
	w.WithLossMode(lossModeBlock)
	if w.ndjsonSpill != nil {
		t.Fatal("precondition: this test needs NO spool wired")
	}
	w.recCh = make(chan *Record, 4)
	budget := bytebudget.New(16, w.stopCh)
	w.memBudget = budget
	w.startOnce.Do(func() {}) // consume the start latch; Enqueue is driven directly
	if !budget.TryAcquire(16) {
		t.Fatal("precondition: could not exhaust the budget")
	}

	done := make(chan struct{})
	go func() {
		w.Enqueue(&Record{RequestID: "waiting", ResponseBody: []byte("ten-bytes!")})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Enqueue returned with the budget exhausted and no spool — a no-loss mode discarded a record")
	case <-time.After(100 * time.Millisecond):
	}

	budget.Release(16) // the drain returns budget
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue did not resume after the byte budget freed")
	}
	if got := <-w.recCh; got.RequestID != "waiting" {
		t.Fatalf("queued %q, want the back-pressured record", got.RequestID)
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 0 {
		t.Fatalf("dropped counter = %v: a no-loss mode must not discard while waiting", got)
	}
}
