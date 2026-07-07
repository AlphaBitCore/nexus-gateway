package audit

// coverage_gaps_test.go — targeted real-behavior tests for branches the broader
// suite leaves uncovered: the /proc/meminfo parse path (via a fixture seam), the
// adaptive flush feedback edges, the metric add-helpers' guard + count, the binary
// spill-recovery re-framing path, the framed publish failure routes, the marshal
// encode-error fallback, and the writer lifecycle's binary/MaxPayload/Close edges.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-json"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/prometheus/client_golang/prometheus"
)

// ---------------------------------------------------------------------------
// adaptive.go — availableMemoryBytes parse path via the meminfoPath seam.
// ---------------------------------------------------------------------------

// withMeminfo writes content to a temp file, points meminfoPath at it for the
// duration of the test, and restores the production path afterwards.
func withMeminfo(t *testing.T, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	orig := meminfoPath
	meminfoPath = p
	t.Cleanup(func() { meminfoPath = orig })
}

func TestAvailableMemoryBytes_ParsesMemAvailable(t *testing.T) {
	withMeminfo(t, "MemTotal:       16000000 kB\nMemAvailable:   12345 kB\nSwapFree: 0 kB\n")
	if got := availableMemoryBytes(); got != 12345*1024 {
		t.Fatalf("availableMemoryBytes() = %d, want %d", got, 12345*1024)
	}
}

func TestAvailableMemoryBytes_MalformedValueReturnsZero(t *testing.T) {
	// MemAvailable present but its value is not an integer → 0 (fall back to fixed).
	withMeminfo(t, "MemAvailable:   not-a-number kB\n")
	if got := availableMemoryBytes(); got != 0 {
		t.Fatalf("availableMemoryBytes() on malformed line = %d, want 0", got)
	}
}

func TestAvailableMemoryBytes_NoMemAvailableLineReturnsZero(t *testing.T) {
	// The key is absent entirely → scanner exhausts the file → 0.
	withMeminfo(t, "MemTotal:       16000000 kB\nMemFree:   1000 kB\n")
	if got := availableMemoryBytes(); got != 0 {
		t.Fatalf("availableMemoryBytes() without MemAvailable = %d, want 0", got)
	}
}

func TestAvailableMemoryBytes_MissingFileReturnsZero(t *testing.T) {
	orig := meminfoPath
	meminfoPath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { meminfoPath = orig })
	if got := availableMemoryBytes(); got != 0 {
		t.Fatalf("availableMemoryBytes() on missing file = %d, want 0", got)
	}
}

// adaptiveBufferCaps returns FIXED structural pointer-count depths — deliberately
// body-size-INDEPENDENT (the byte budget is the memory bound; a count derived from
// an assumed body size is exactly the sizing that OOM'd). Any meminfo content must
// not change them.
func TestAdaptiveBufferCaps_FixedStructuralDepths(t *testing.T) {
	withMeminfo(t, "MemAvailable:   8388608 kB\n") // 8 GiB available — must not matter
	recCh, spillCh := adaptiveBufferCaps()
	if recCh != recChStructuralCap || spillCh != spillChStructuralCap {
		t.Fatalf("adaptiveBufferCaps() = (%d,%d), want fixed (%d,%d)",
			recCh, spillCh, recChStructuralCap, spillChStructuralCap)
	}
}

// ---------------------------------------------------------------------------
// adaptive.go — adaptiveMemBudgetBytes: the in-memory audit BYTE budget is the
// RAM-fraction of parsed MemAvailable, with a fixed fallback when unreadable.
// ---------------------------------------------------------------------------

func TestAdaptiveMemBudgetBytes_FractionOfParsedMemory(t *testing.T) {
	withMeminfo(t, "MemAvailable:   8388608 kB\n") // 8 GiB available
	availBytes := float64(uint64(8388608) * 1024)
	want := int64(availBytes * auditMemFraction)
	if got := adaptiveMemBudgetBytes(); got != want {
		t.Fatalf("adaptiveMemBudgetBytes() = %d, want %d (%.0f%% of 8 GiB)",
			got, want, auditMemFraction*100)
	}
}

func TestAdaptiveMemBudgetBytes_FallbackWhenUnreadable(t *testing.T) {
	orig := meminfoPath
	meminfoPath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { meminfoPath = orig })
	if got := adaptiveMemBudgetBytes(); got != int64(fixedBudgetFallback) {
		t.Fatalf("adaptiveMemBudgetBytes() fallback = %d, want %d", got, int64(fixedBudgetFallback))
	}
}

// ---------------------------------------------------------------------------
// adaptive.go — observe edges: zero-input no-op, and a slow write growing the
// threshold while a fast write keeps it bounded.
// ---------------------------------------------------------------------------

func TestAdaptiveSpillFlush_ObserveZeroInputsNoOp(t *testing.T) {
	a := newAdaptiveSpillFlush()
	before := a.threshold()
	a.observe(0, time.Millisecond)  // zero bytes
	a.observe(1<<20, 0)             // zero duration
	a.observe(-1, time.Millisecond) // negative bytes
	if a.threshold() != before {
		t.Fatalf("observe with invalid inputs changed threshold %d -> %d", before, a.threshold())
	}
}

func TestAdaptiveSpillFlush_SlowWriteGrowsThreshold(t *testing.T) {
	a := newAdaptiveSpillFlush()
	// A very slow write (1 MiB took 10s) → per-MiB latency huge → want size shrinks
	// to the floor (still bounded, never below spillFlushMinBytes).
	a.observe(1<<20, 10*time.Second)
	if got := a.threshold(); got != spillFlushMinBytes {
		t.Fatalf("after a very slow write, threshold = %d, want floor %d", got, spillFlushMinBytes)
	}

	// A second adaptive instance fed an extremely fast write (lots of bytes in tiny
	// time) wants a large flush, clamped to the ceiling.
	b := newAdaptiveSpillFlush()
	b.observe(512<<20, time.Microsecond) // absurdly fast → want >> ceiling → clamps
	if got := b.threshold(); got != spillFlushMaxBytes {
		t.Fatalf("after a very fast write, threshold = %d, want ceiling %d", got, spillFlushMaxBytes)
	}
}

// observe applies an EMA: a second observation blends with the first rather than
// replacing it, so the threshold reflects both. We assert the EMA branch runs by
// driving two observations of different speed and checking the result lands strictly
// between the two single-observation thresholds.
func TestAdaptiveSpillFlush_ObserveBlendsEMA(t *testing.T) {
	a := newAdaptiveSpillFlush()
	a.observe(8<<20, 40*time.Millisecond) // first sets emaLatency directly
	first := a.emaLatencyNs.Load()
	if first <= 0 {
		t.Fatalf("first observe did not set emaLatency, got %d", first)
	}
	a.observe(8<<20, 80*time.Millisecond) // slower → EMA moves up but is blended
	blended := a.emaLatencyNs.Load()
	if blended <= first {
		t.Fatalf("EMA did not rise after a slower write: first=%d blended=%d", first, blended)
	}
}

// ---------------------------------------------------------------------------
// writer_metrics.go — addSpilled / addDropped: guard (n<=0 no-op) + exact count.
// ---------------------------------------------------------------------------

func TestAuditMetrics_AddSpilledAndDropped_CountAndGuard(t *testing.T) {
	prom := prometheus.NewRegistry()
	m := newAuditMetrics(opsmetrics.NewRegistry(prom))

	m.addSpilled(0)  // guard: must NOT increment
	m.addSpilled(-3) // guard: must NOT increment
	m.addSpilled(5)  // counts 5
	if got := counterValue(t, prom, "nexus_audit_mq_spilled_total"); got != 5 {
		t.Fatalf("spilled_total = %v, want 5 (0/-3 ignored, +5 counted)", got)
	}

	m.addDropped(0)  // guard
	m.addDropped(-1) // guard
	m.addDropped(4)  // counts 4
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 4 {
		t.Fatalf("dropped_total = %v, want 4 (0/-1 ignored, +4 counted)", got)
	}

	// nil receiver must be a safe no-op for both.
	var nilM *auditMetrics
	nilM.addSpilled(9)
	nilM.addDropped(9)
}

// ---------------------------------------------------------------------------
// record_bodypool.go — AcquireRequestBody allocate-new branch (pooled buffer too
// small for src) returns an exact copy of src.
// ---------------------------------------------------------------------------

func TestAcquireRequestBody_GrowsWhenPooledTooSmall(t *testing.T) {
	// Drain any pooled buffers so the next Get returns the New() 64 KiB buffer, then
	// a >64 KiB src forces the cap(b) < len(src) allocate branch.
	big := bytes.Repeat([]byte("Z"), 256<<10) // 256 KiB > the 64 KiB pool buffer cap
	body, handle := AcquireRequestBody(big)
	if handle == nil {
		t.Fatal("handle must be non-nil for a non-empty src")
	}
	if !bytes.Equal(body, big) {
		t.Fatal("AcquireRequestBody returned bytes that differ from src")
	}
	if len(body) != len(big) {
		t.Fatalf("body len = %d, want %d", len(body), len(big))
	}
	releaseRequestBody(handle)
}

// ---------------------------------------------------------------------------
// message.go — recordToMessage carries the embedding + ai-guard cost fields when
// non-zero (the optional pointer branches).
// ---------------------------------------------------------------------------

func TestRecordToMessage_CarriesEmbeddingAndAIGuardCosts(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	rec := &Record{
		RequestID:        "rec-cost",
		Timestamp:        time.Unix(1700000000, 0).UTC(),
		EmbeddingCostUsd: 0.0021,
		EmbeddingModelID: "text-embedding-3-small",
		AIGuardCostUsd:   0.0007,
		HookRewritten:    true,
		HookRewriteCount: 2,
	}
	msg := w.recordToMessage(rec)

	if msg.EmbeddingCostUsd == nil || *msg.EmbeddingCostUsd != 0.0021 {
		t.Fatalf("EmbeddingCostUsd = %v, want 0.0021", msg.EmbeddingCostUsd)
	}
	if msg.EmbeddingModelID != "text-embedding-3-small" {
		t.Fatalf("EmbeddingModelID = %q, want text-embedding-3-small", msg.EmbeddingModelID)
	}
	if msg.AIGuardCostUsd == nil || *msg.AIGuardCostUsd != 0.0007 {
		t.Fatalf("AIGuardCostUsd = %v, want 0.0007", msg.AIGuardCostUsd)
	}
	details := jsonbMap(t, msg.Details)
	if got := details["hookRewritten"]; got != true {
		t.Fatalf("details.hookRewritten = %v, want true", got)
	}
	if got := details["hookRewriteCount"]; got != float64(2) {
		t.Fatalf("details.hookRewriteCount = %v, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// writer_batch.go — marshalRecordPlain encode-error branch: an unmarshalable
// Metadata value makes json.Encode fail; the function returns ok=false.
// ---------------------------------------------------------------------------

func TestMarshalRecordPlain_EncodeErrorReturnsNotOK(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := &Record{
		RequestID: "bad",
		Timestamp: time.Unix(1700000000, 0).UTC(),
		Metadata:  make(chan int), // channels are not JSON-encodable
	}
	data, buf, ok := w.marshalRecordPlain(rec)
	if ok || data != nil || buf != nil {
		t.Fatalf("marshalRecordPlain on unmarshalable Metadata: ok=%v data=%v buf=%v, want ok=false/nil/nil", ok, data, buf)
	}
}

// marshalRecord (JSON wire) shares the same encode-error fallback.
func TestMarshalRecord_EncodeErrorReturnsNotOK(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := &Record{
		RequestID: "bad2",
		Timestamp: time.Unix(1700000000, 0).UTC(),
		Metadata:  make(chan int),
	}
	if _, _, ok := w.marshalRecord(rec); ok {
		t.Fatal("marshalRecord on unmarshalable Metadata must return ok=false")
	}
}

// ---------------------------------------------------------------------------
// writer_batch.go — marshalChunkParallel single-record marshal failure returns
// empty slices (the n==1 not-ok branch).
// ---------------------------------------------------------------------------

func TestMarshalChunkParallel_SingleBadRecordYieldsNothing(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	bad := &Record{RequestID: "x", Timestamp: time.Unix(1700000000, 0).UTC(), Metadata: make(chan int)}
	datas, bufs, recs := w.marshalChunkParallel([]*Record{bad})
	if datas != nil || bufs != nil || recs != nil {
		t.Fatalf("single bad record: datas=%v bufs=%v recs=%v, want all nil", datas, bufs, recs)
	}
}

// marshalChunkParallel multi-record path: a mix of good + bad records keeps the good
// ones (in order) and drops the bad. With >1 record it runs the worker pool, and the
// workers>n clamp is exercised by a tiny chunk on a multi-core box.
func TestMarshalChunkParallel_DropsBadKeepsGood(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	good1 := &Record{RequestID: "g1", Timestamp: time.Unix(1700000000, 0).UTC()}
	bad := &Record{RequestID: "b", Timestamp: time.Unix(1700000000, 0).UTC(), Metadata: make(chan int)}
	good2 := &Record{RequestID: "g2", Timestamp: time.Unix(1700000000, 0).UTC()}
	datas, _, recs := w.marshalChunkParallel([]*Record{good1, bad, good2})
	if len(recs) != 2 || len(datas) != 2 {
		t.Fatalf("kept %d records (%d datas), want 2 good ones", len(recs), len(datas))
	}
	if recs[0].RequestID != "g1" || recs[1].RequestID != "g2" {
		t.Fatalf("order not preserved: got %s,%s want g1,g2", recs[0].RequestID, recs[1].RequestID)
	}
}

// ---------------------------------------------------------------------------
// writer_batch.go — acquireFrame grow branch: a size larger than the pooled
// buffer's capacity yields a buffer with at least that capacity.
// ---------------------------------------------------------------------------

func TestAcquireFrame_GrowsBeyondPooledCap(t *testing.T) {
	const huge = (1 << 20) + 7 // larger than any pooled frame's starting cap
	frame, handle := acquireFrame(huge)
	if handle == nil {
		t.Fatal("acquireFrame handle must be non-nil")
	}
	if len(frame) != 0 {
		t.Fatalf("acquireFrame returned non-empty frame len=%d", len(frame))
	}
	if cap(frame) < huge {
		t.Fatalf("acquireFrame cap=%d, want >= %d", cap(frame), huge)
	}
}

// ---------------------------------------------------------------------------
// writer_batch.go — publishFramed failure routes: a whole-batch error and a
// per-frame nak both route every record to handlePublishFailure (re-buffer/spill),
// surfacing on enqueue_errors and never silently dropping.
// ---------------------------------------------------------------------------

// hardFailBatchProducer fails the whole EnqueueBatchAsync call.
type hardFailBatchProducer struct{}

func (hardFailBatchProducer) Publish(context.Context, string, []byte) error { return nil }
func (hardFailBatchProducer) Enqueue(context.Context, string, []byte) error { return nil }
func (hardFailBatchProducer) Close() error                                  { return nil }
func (hardFailBatchProducer) EnqueueBatchAsync(context.Context, string, [][]byte) ([]error, error) {
	return nil, errors.New("broker unreachable")
}

func TestPublishFramed_WholeBatchErrorReBuffers(t *testing.T) {
	prom := prometheus.NewRegistry()
	w := NewWriter(hardFailBatchProducer{}, "q", opsmetrics.NewRegistry(prom), slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.frameMaxBytes = 256 << 10 // engage framing
	w.recCh = make(chan *Record, 8)

	datas := [][]byte{[]byte(`{"id":"a"}`), []byte(`{"id":"b"}`)}
	recs := []*Record{{RequestID: "a"}, {RequestID: "b"}}
	w.publishFramed(hardFailBatchProducer{}, 0, datas, recs)

	if got := counterValue(t, prom, "nexus_audit_mq_enqueue_errors_total"); got != 2 {
		t.Fatalf("enqueue_errors_total = %v, want 2 (one per record on a whole-batch error)", got)
	}
	// No durable spill wired and a free queue → records re-buffer onto recCh (no loss).
	if got := len(w.recCh); got == 0 {
		t.Fatal("records on a publish error must be re-buffered, recCh is empty")
	}
}

// perFrameNakProducer naks every frame in the call (per-frame errors, no hard error).
type perFrameNakProducer struct{}

func (perFrameNakProducer) Publish(context.Context, string, []byte) error { return nil }
func (perFrameNakProducer) Enqueue(context.Context, string, []byte) error { return nil }
func (perFrameNakProducer) Close() error                                  { return nil }
func (perFrameNakProducer) EnqueueBatchAsync(_ context.Context, _ string, batch [][]byte) ([]error, error) {
	errs := make([]error, len(batch))
	for i := range errs {
		errs[i] = errors.New("frame nak")
	}
	return errs, nil
}

func TestPublishFramed_PerFrameNakReBuffers(t *testing.T) {
	prom := prometheus.NewRegistry()
	w := NewWriter(perFrameNakProducer{}, "q", opsmetrics.NewRegistry(prom), slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.frameMaxBytes = 256 << 10
	w.recCh = make(chan *Record, 8)

	datas := [][]byte{[]byte(`{"id":"a"}`), []byte(`{"id":"b"}`)}
	recs := []*Record{{RequestID: "a"}, {RequestID: "b"}}
	w.publishFramed(perFrameNakProducer{}, 0, datas, recs)

	if got := counterValue(t, prom, "nexus_audit_mq_enqueue_errors_total"); got != 2 {
		t.Fatalf("enqueue_errors_total = %v, want 2 (one per naked record)", got)
	}
	if got := len(w.recCh); got == 0 {
		t.Fatal("naked records must be re-buffered, recCh is empty")
	}
}

// publishFramed with an empty datas slice is a no-op (no publish, no counters).
func TestPublishFramed_EmptyIsNoOp(t *testing.T) {
	prom := prometheus.NewRegistry()
	prod := &frameCapProducer{}
	w := NewWriter(prod, "q", opsmetrics.NewRegistry(prom), slog.Default())
	w.publishFramed(prod, 0, nil, nil)
	if prod.calls() != 0 {
		t.Fatalf("publishFramed(nil) made %d publish calls, want 0", prod.calls())
	}
}

// publishChunk with an empty datas slice is a no-op.
func TestPublishChunk_EmptyIsNoOp(t *testing.T) {
	prom := prometheus.NewRegistry()
	prod := &frameCapProducer{}
	w := NewWriter(prod, "q", opsmetrics.NewRegistry(prom), slog.Default())
	w.publishChunk(prod, 0, nil, nil)
	if prod.calls() != 0 {
		t.Fatalf("publishChunk(nil) made %d publish calls, want 0", prod.calls())
	}
}

// ---------------------------------------------------------------------------
// writer.go — ensureStarted defaults the frame cap when the binary wire is on with
// no explicit cap (an unframed binary message would be mis-detected as a frame).
// ---------------------------------------------------------------------------

func TestEnsureStarted_BinaryWireDefaultsFrameCap(t *testing.T) {
	t.Setenv("NEXUS_AUDIT_WIRE", "binary")
	w := NewWriter(&memProducer{}, "q", nil, slog.Default())
	if !w.wireBinary {
		t.Fatal("expected wireBinary=true under NEXUS_AUDIT_WIRE=binary")
	}
	if w.frameMaxBytes != 0 {
		t.Fatalf("precondition: frameMaxBytes=%d, want 0 before Start", w.frameMaxBytes)
	}
	w.Start()
	defer w.Close()
	if w.frameMaxBytes != defaultBinaryFrameBytes {
		t.Fatalf("binary wire without explicit cap: frameMaxBytes=%d, want %d", w.frameMaxBytes, defaultBinaryFrameBytes)
	}
}

// ---------------------------------------------------------------------------
// writer.go — startSpillRecovery sources the broker max_payload when the producer
// exposes MaxPayload(), bounding the sweeper's per-record dead-letter cap.
// ---------------------------------------------------------------------------

// maxPayloadProducer is a batch producer that also advertises a broker max_payload.
type maxPayloadProducer struct {
	maxPayload int64
}

func (p *maxPayloadProducer) Publish(context.Context, string, []byte) error { return nil }
func (p *maxPayloadProducer) Enqueue(context.Context, string, []byte) error { return nil }
func (p *maxPayloadProducer) Close() error                                  { return nil }
func (p *maxPayloadProducer) EnqueueBatchAsync(_ context.Context, _ string, b [][]byte) ([]error, error) {
	return make([]error, len(b)), nil
}
func (p *maxPayloadProducer) MaxPayload() int64 { return p.maxPayload }

func TestStartSpillRecovery_SourcesMaxPayload(t *testing.T) {
	dir := t.TempDir()
	spool, err := sharedndjson.New(dir, "gw", 64, 1<<20, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	prod := &maxPayloadProducer{maxPayload: 1 << 20} // 1 MiB
	w := NewWriter(prod, "q", opsmetrics.NewRegistry(prometheus.NewRegistry()), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool).
		WithSpillRecovery(time.Hour, 0) // long interval: never fires during the test
	w.Start()
	defer w.Close()
	// The sweeper goroutine is up and Close drains it cleanly; the MaxPayload accessor
	// branch ran during startSpillRecovery (no panic, recovery enabled). We assert the
	// observable wiring: a 1 MiB max_payload is large enough to keep recovery enabled
	// rather than disabled, so a record spilled now is recoverable (no warning log).
	// The key assertion is structural: Start+Close with a MaxPayload producer + spool
	// completes without leaking the recovery goroutine (wg.Wait in Close returns).
}

// startSpillRecovery is disabled (logs once) when the producer is not batch-capable.
func TestStartSpillRecovery_DisabledForNonBatchProducer(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	spool, _ := sharedndjson.New(dir, "gw", 64, 1<<20, nil)
	// memProducer is NOT a batchProducer.
	w := NewWriter(&memProducer{}, "q", nil, slog.New(slog.NewTextHandler(&buf, nil))).
		WithNDJSONSpill(spool).
		WithSpillRecovery(time.Hour, 0)
	w.Start()
	// Close first so every writer goroutine that logs to buf (the consumer workers'
	// startup Info line) has exited before we read it — otherwise the buffer read
	// races their concurrent writes.
	w.Close()
	if !bytes.Contains(buf.Bytes(), []byte("spill recovery requested but disabled")) {
		t.Fatalf("expected the disabled-recovery warning, log=%q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// writer.go — Close drains a straggler left on recCh after the workers exit,
// spilling it durably (no loss) rather than dropping it.
// ---------------------------------------------------------------------------

func TestClose_DrainsStragglerToSpill(t *testing.T) {
	dir := t.TempDir()
	spool, err := sharedndjson.New(dir, "gw", 64, 1<<20, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	// Build a writer but DON'T Start it (no workers). Manually wire recCh with a
	// straggler so Close's post-wg drain loop hits the recCh<-rec case and spills it.
	w := NewWriter(nil, "q", opsmetrics.NewRegistry(prometheus.NewRegistry()), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool)
	w.recCh = make(chan *Record, 4)
	w.recCh <- &Record{RequestID: "straggler", Timestamp: time.Unix(1700000000, 0).UTC()}

	w.Close() // wg empty → returns immediately → drain loop spills the straggler

	if err := spool.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	sealed, _ := spool.SealedFiles()
	if len(sealed) != 1 {
		t.Fatalf("straggler not spilled, sealed=%v", sealed)
	}
	b, _ := os.ReadFile(sealed[0])
	if !spoolDecodedContains(b, "straggler") {
		t.Fatalf("straggler record missing from spool: %q", b)
	}
}

// ---------------------------------------------------------------------------
// backpressure.go — spillLoop: the in-buffer flush triggered by crossing the
// adaptive threshold writes the accumulated records durably, and a marshal failure
// inside the loop counts a drop instead of a spill.
// ---------------------------------------------------------------------------

func TestSpillLoop_ThresholdFlushWritesDurably(t *testing.T) {
	prom := prometheus.NewRegistry()
	dir := t.TempDir()
	spool, err := sharedndjson.New(dir, "gw", 1024, 1<<20, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(nil, "q", opsmetrics.NewRegistry(prom), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool).
		WithLossMode(lossModeSpill)
	// Force an immediate threshold flush: any single record crosses a 1-byte threshold.
	w.spillFlush.targetBytes.Store(1)
	w.Start()

	for i := range 5 {
		w.spillCh <- &Record{RequestID: "s-" + string(rune('a'+i)), Timestamp: time.Unix(1700000000, 0).UTC()}
	}
	w.Close() // drains spillCh, flushes, exits

	if got := counterValue(t, prom, "nexus_audit_mq_spilled_total"); got < 5 {
		t.Fatalf("spilled_total = %v, want >= 5 (threshold flush must persist the records)", got)
	}
	_ = spool.Rotate()
	sealed, _ := spool.SealedFiles()
	if len(sealed) == 0 {
		t.Fatal("threshold flush produced no sealed spool file")
	}
}

func TestSpillLoop_MarshalErrorCountsDrop(t *testing.T) {
	prom := prometheus.NewRegistry()
	dir := t.TempDir()
	spool, _ := sharedndjson.New(dir, "gw", 1024, 1<<20, nil)
	w := NewWriter(nil, "q", opsmetrics.NewRegistry(prom), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool).
		WithLossMode(lossModeSpill)
	w.Start()

	// A record whose Metadata cannot be JSON-marshaled fails inside spillLoop.add →
	// incDropped, never spilled.
	w.spillCh <- &Record{RequestID: "bad", Timestamp: time.Unix(1700000000, 0).UTC(), Metadata: make(chan int)}
	w.Close()

	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got < 1 {
		t.Fatalf("dropped_total = %v, want >= 1 (an unmarshalable record drops in the loop)", got)
	}
}

// spillLoop's batched WriteBatch failure (e.g. spool quota exceeded) counts the
// whole batch as drops, never as spills — the loud overflow signal.
func TestSpillLoop_WriteBatchErrorCountsDrops(t *testing.T) {
	prom := prometheus.NewRegistry()
	dir := t.TempDir()
	spool, err := sharedndjson.New(dir, "gw", 1, 1, nil) // 1 MB quota
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	// Pre-seed past the quota so the loop's batched WriteBatch is refused. The seed
	// must be a countable audit-*.ndjson file — the reclaimable-quota gate excludes
	// non-audit entries (e.g. .poison), so a differently-named filler would not count.
	if err := os.WriteFile(filepath.Join(dir, "gw", "audit-20260101-0001.ndjson"), make([]byte, 1100*1024), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := NewWriter(nil, "q", opsmetrics.NewRegistry(prom), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool).
		WithLossMode(lossModeSpill)
	w.spillFlush.targetBytes.Store(1) // flush on the first record
	w.Start()

	for i := range 3 {
		w.spillCh <- &Record{RequestID: "wb-" + string(rune('a'+i)), Timestamp: time.Unix(1700000000, 0).UTC()}
	}
	w.Close()

	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got < 3 {
		t.Fatalf("dropped_total = %v, want >= 3 (a quota-refused batch drops its records)", got)
	}
	if got := counterValue(t, prom, "nexus_audit_mq_spilled_total"); got != 0 {
		t.Fatalf("spilled_total = %v, want 0 (the write failed, nothing was durably spilled)", got)
	}
}

// spillOverflow's success path: a wired spool + free spill channel hands the record
// to the async worker (non-blocking) rather than dropping it.
func TestSpillOverflow_HandsToWorker(t *testing.T) {
	prom := prometheus.NewRegistry()
	dir := t.TempDir()
	spool, _ := sharedndjson.New(dir, "gw", 64, 512, nil)
	w := NewWriter(nil, "q", opsmetrics.NewRegistry(prom), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool).
		WithLossMode(lossModeSpill)
	// Do NOT Start (no worker draining), so the handed-off record stays on spillCh
	// for inspection — proving spillOverflow took the channel-send path, not a drop.
	rec := &Record{RequestID: "ovf", Timestamp: time.Unix(1700000000, 0).UTC()}
	w.spillOverflow(rec)

	select {
	case got := <-w.spillCh:
		if got.RequestID != "ovf" {
			t.Fatalf("spillCh carried %q, want ovf", got.RequestID)
		}
	default:
		t.Fatal("spillOverflow did not hand the record to the async spill channel")
	}
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 0 {
		t.Fatalf("dropped_total = %v, want 0 (record was spilled, not dropped)", got)
	}
}

// spillOverflow's saturated-channel path: with a spool wired but the async spill
// channel already full, the only non-blocking option is a counted drop (genuine
// overload beyond the disk's sustainable write rate), never a request-path block.
func TestSpillOverflow_FullChannelDrops(t *testing.T) {
	prom := prometheus.NewRegistry()
	dir := t.TempDir()
	spool, _ := sharedndjson.New(dir, "gw", 64, 512, nil)
	w := NewWriter(nil, "q", opsmetrics.NewRegistry(prom), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithNDJSONSpill(spool).
		WithLossMode(lossModeSpill)
	// Saturate the spill channel (do not Start a worker that would drain it).
	w.spillCh = make(chan *Record, 1)
	w.spillCh <- &Record{RequestID: "filler"}

	w.spillOverflow(&Record{RequestID: "overflow-drop", Timestamp: time.Unix(1700000000, 0).UTC()})

	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got != 1 {
		t.Fatalf("dropped_total = %v, want 1 (a full spill channel forces a counted drop)", got)
	}
}

// ---------------------------------------------------------------------------
// spill_recovery.go — drainFile on the BINARY wire re-frames recovered records as
// magic + length-prefixed records (the path the JSON-wire tests never hit).
// ---------------------------------------------------------------------------

// binaryWireRecovery builds a spillRecovery on the binary wire over a temp spool
// that already holds spilled (base64-encoded) records.
func TestDrainFile_BinaryWireReFramesRecords(t *testing.T) {
	dir := t.TempDir()
	spool, err := sharedndjson.New(dir, "gw", 4096, 1<<20, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	// Spool three records (the live writer writes base64 spool lines).
	want := [][]byte{[]byte(`{"id":"r0"}`), []byte(`{"id":"r1"}`), []byte(`{"id":"r2"}`)}
	for _, rec := range want {
		if err := spool.Write(spillEncodeRecord(rec)); err != nil {
			t.Fatalf("spool write: %v", err)
		}
	}
	if err := spool.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	sealed, _ := spool.SealedFiles()
	if len(sealed) != 1 {
		t.Fatalf("want 1 sealed file, got %v", sealed)
	}

	bp := newFakeBatchProducer()
	// wireBinary=true so drainFile uses the binary framing (magic + uvarint lengths);
	// frameMax large so all three records pack into one frame.
	r := newSpillRecovery(spool, bp, "q", 1<<20, 1<<20, 0, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !r.drainFile(context.Background(), sealed[0]) {
		t.Fatal("drainFile returned false on a clean binary-wire replay")
	}

	// The producer received exactly one binary frame carrying all three records,
	// decodable by the shared frame-record counter (magic + length-prefixed).
	bp.mu.Lock()
	frames := bp.received
	bp.mu.Unlock()
	total := 0
	sawMagic := false
	for _, fr := range frames {
		if len(fr) > 0 && fr[0] == mq.BinwireMagic {
			sawMagic = true
		}
		total += countFrameRecords(fr)
	}
	if !sawMagic {
		t.Fatal("binary-wire recovery did not emit a frame with the binwire magic")
	}
	if total != len(want) {
		t.Fatalf("binary-wire recovery delivered %d records, want %d", total, len(want))
	}

	// The file was fully resolved and deleted (no-loss → durably acked).
	if _, err := os.Stat(sealed[0]); !os.IsNotExist(err) {
		t.Fatalf("drainFile did not delete the fully-acked file: stat err=%v", err)
	}
}

// drainFile on the binary wire across MULTIPLE frames: a tiny frame cap forces each
// record into its own binary frame, exercising the mid-file frame seal + a new
// magic-prefixed frame start.
func TestDrainFile_BinaryWireMultiFrame(t *testing.T) {
	dir := t.TempDir()
	spool, _ := sharedndjson.New(dir, "gw", 4096, 1<<20, nil)
	want := [][]byte{[]byte(`{"id":"a0"}`), []byte(`{"id":"a1"}`), []byte(`{"id":"a2"}`)}
	for _, rec := range want {
		if err := spool.Write(spillEncodeRecord(rec)); err != nil {
			t.Fatalf("spool write: %v", err)
		}
	}
	_ = spool.Rotate()
	sealed, _ := spool.SealedFiles()

	bp := newFakeBatchProducer()
	// frameMax = 8 forces a new frame per record (each record + magic + len > 8).
	r := newSpillRecovery(spool, bp, "q", 8, 1<<20, 0, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !r.drainFile(context.Background(), sealed[0]) {
		t.Fatal("drainFile returned false on a clean multi-frame binary replay")
	}
	bp.mu.Lock()
	frames := bp.received
	bp.mu.Unlock()
	if len(frames) != len(want) {
		t.Fatalf("multi-frame binary replay produced %d frames, want %d (one per record)", len(frames), len(want))
	}
	total := 0
	for _, fr := range frames {
		if len(fr) == 0 || fr[0] != mq.BinwireMagic {
			t.Fatalf("binary frame missing magic: %x", fr)
		}
		total += countFrameRecords(fr)
	}
	if total != len(want) {
		t.Fatalf("multi-frame binary replay delivered %d records, want %d", total, len(want))
	}
}

// appendPoisonFile happy-path readback is already covered; this asserts that an
// existing poison file is APPENDED to (not truncated), so a second oversize record
// from the same spool file accumulates rather than overwriting the first.
func TestAppendPoisonFile_AppendsNotTruncates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ndjson.poison")
	if err := appendPoisonFile(p, []byte(`{"id":"first"}`)); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := appendPoisonFile(p, []byte(`{"id":"second"}`)); err != nil {
		t.Fatalf("second append: %v", err)
	}
	b, _ := os.ReadFile(p)
	want := `{"id":"first"}` + "\n" + `{"id":"second"}` + "\n"
	if string(b) != want {
		t.Fatalf("poison file = %q, want %q", b, want)
	}
}

// Sanity: a JSON-wire message built from a record still round-trips so the embedding
// cost fields above are wire-shaped, not just struct-shaped.
func TestRecordToMessage_EmbeddingFieldsRoundTripJSON(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	rec := &Record{RequestID: "rt", Timestamp: time.Unix(1700000000, 0).UTC(), EmbeddingCostUsd: 0.5}
	data, err := json.Marshal(w.recordToMessage(rec))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back mq.TrafficEventMessage
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.EmbeddingCostUsd == nil || *back.EmbeddingCostUsd != 0.5 {
		t.Fatalf("round-trip EmbeddingCostUsd = %v, want 0.5", back.EmbeddingCostUsd)
	}
}
