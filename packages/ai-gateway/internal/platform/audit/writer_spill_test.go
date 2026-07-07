package audit

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
)

// readSpool returns every non-empty NDJSON line under {dir}/{instanceID}/.
func readSpool(t *testing.T, dir, instanceID string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, instanceID))
	if err != nil {
		t.Fatalf("ReadDir spool: %v", err)
	}
	var lines []string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, instanceID, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if l == "" {
				continue
			}
			// Spool lines are base64-framed (binary-safe); decode back to the original
			// marshaled record so assertions read the record content.
			if rec, ok := spillDecodeLine([]byte(l)); ok {
				lines = append(lines, string(rec))
			} else {
				lines = append(lines, l)
			}
		}
	}
	return lines
}

// spoolDecodedContains reports whether any base64-framed spool line decodes to a
// record containing sub — for tests that read a spool file directly.
func spoolDecodedContains(spool []byte, sub string) bool {
	for _, l := range strings.Split(strings.TrimRight(string(spool), "\n"), "\n") {
		if l == "" {
			continue
		}
		rec, _ := spillDecodeLine([]byte(l))
		if strings.Contains(string(rec), sub) {
			return true
		}
	}
	return false
}

// saturate prepares a Writer whose bounded queue (recCh) is full and whose
// consumer workers are NOT running, so the next Enqueue/publishRecord
// deterministically hits the overflow path (spill/drop) instead of being drained.
// It consumes startOnce with a no-op so Enqueue's lazy ensureStarted() does not
// later spin up consumers that would drain the saturated queue.
func saturate(w *Writer) {
	w.startOnce.Do(func() {}) // claim the Once so ensureStarted() stays a no-op
	w.recCh = make(chan *Record, 1)
	w.recCh <- &Record{RequestID: "fill"} // full (cap 1)
}

// TestWriter_SpillsOnOverflowToNDJSON proves an overflow record is handed to the
// async spill channel (off the request path) and the worker's write step captures
// it durably to NDJSON — not dropped. saturate() leaves the consumers unstarted, so
// the test drives the worker's write step directly for determinism.
func TestWriter_SpillsOnOverflowToNDJSON(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 64, 512, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(&memProducer{}, "q", nil, slog.Default()).WithNDJSONSpill(spill).WithLossMode(lossModeSpill)
	saturate(w)

	w.Enqueue(&Record{RequestID: "spilled-1"}) // buf full → handed to async spill channel

	select {
	case rec := <-w.spillCh:
		if !w.spillRecord(rec) { // the worker's write step
			t.Fatal("spillRecord failed to write the handed-off overflow record")
		}
	default:
		t.Fatal("overflow record was not handed to the async spill channel")
	}
	lines := readSpool(t, dir, "test")
	if len(lines) != 1 || !strings.Contains(lines[0], "spilled-1") {
		t.Fatalf("overflow record must spill to disk; got %v", lines)
	}
}

// TestSpillRecord_HonorsActiveWireFormat locks the spill/recovery wire-format
// match. spill-recovery re-frames spooled records for the broker as BINARY when the
// binary wire is active; if spillRecord stored JSON there, the Hub would read the
// leading '{' as a field-id and silently drop the record. So a record spilled with
// the binary wire active must be stored as a binary-wire record, and a JSON-wire
// record must be stored as JSON. Both the async-worker path (spillRecord) and the
// request-path overflow share this code, so this covers both.
func TestSpillRecord_HonorsActiveWireFormat(t *testing.T) {
	for _, wireBinary := range []bool{true, false} {
		name := "json"
		if wireBinary {
			name = "binary"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			spill, err := sharedndjson.New(dir, "test", 64, 512, nil)
			if err != nil {
				t.Fatalf("ndjson.New: %v", err)
			}
			w := NewWriter(&memProducer{}, "q", nil, slog.Default()).
				WithNDJSONSpill(spill).WithLossMode(lossModeSpill)
			w.wireBinary = wireBinary

			if !w.spillRecord(&Record{RequestID: "wire-1"}) {
				t.Fatal("spillRecord failed to write the record")
			}
			lines := readSpool(t, dir, "test")
			if len(lines) != 1 {
				t.Fatalf("want 1 spool line, got %d", len(lines))
			}
			isJSON := strings.HasPrefix(lines[0], "{")
			switch {
			case wireBinary && isJSON:
				t.Fatalf("binary wire spilled a JSON record (the recovery-mismatch regression): %.48q", lines[0])
			case !wireBinary && !isJSON:
				t.Fatalf("json wire spilled a non-JSON record: %.48q", lines[0])
			}
			// The record content survives in either wire (the RequestID bytes are present).
			if !strings.Contains(lines[0], "wire-1") {
				t.Fatalf("spooled record lost its RequestID: %.48q", lines[0])
			}
		})
	}
}

// TestWriter_EnqueueOnFullQueueIsNonBlocking proves the core-path contract: when
// the bounded queue is full, Enqueue (in spill mode) does NOT wait AND does NOT pay
// the marshal/spill cost on the request goroutine — it hands the record to the async
// spill channel with a non-blocking send and returns immediately. The expensive
// durable write happens on the spill worker, never on the finalizeAudit defer.
func TestWriter_EnqueueOnFullQueueIsNonBlocking(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 64, 512, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(&memProducer{}, "q", nil, slog.Default()).WithNDJSONSpill(spill).WithLossMode(lossModeSpill)
	saturate(w)

	done := make(chan struct{})
	go func() {
		w.Enqueue(&Record{RequestID: "bp-1"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Enqueue blocked on a full buffer — core path must not wait on the audit side-path")
	}

	// The bounded queue was never grown (still full at its cap).
	if got := len(w.recCh); got != cap(w.recCh) {
		t.Fatalf("queue = %d, want %d (overflow must not grow the in-memory queue)", got, cap(w.recCh))
	}
	// The record was handed to the async spill channel (durable path engaged), not
	// dropped on the request goroutine.
	select {
	case rec := <-w.spillCh:
		if rec.RequestID != "bp-1" {
			t.Fatalf("spill channel carried %q, want bp-1", rec.RequestID)
		}
	default:
		t.Fatal("overflow record was not handed to the async spill channel")
	}
}

// TestWriter_SpillWriteErrorFallsToLoudDrop proves that when the spill write is
// refused (quota exceeded) on the worker, the record is counted as a drop (and the
// log is throttled, never a per-failure stack-trace storm) and no new spool file
// appears beyond the pre-seeded quota filler.
func TestWriter_SpillWriteErrorFallsToLoudDrop(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 1, 1, nil) // 1 MB quota
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	// Pre-seed past the quota so the next spill write is refused. The seed must be a
	// countable audit-*.ndjson file: the reclaimable-quota gate excludes non-audit
	// entries (e.g. .poison), so a differently-named filler would not count.
	if err := os.WriteFile(filepath.Join(dir, "test", "audit-20260101-0001.ndjson"), make([]byte, 1100*1024), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := NewWriter(&memProducer{}, "q", nil, slog.Default()).WithNDJSONSpill(spill).WithLossMode(lossModeSpill)
	saturate(w)

	w.Enqueue(&Record{RequestID: "drop-1"}) // overflow → async spill channel

	select {
	case rec := <-w.spillCh:
		if w.spillRecord(rec) { // the worker's write step refuses the over-quota write
			t.Fatal("spillRecord must fail when the spool quota is exceeded")
		}
	default:
		t.Fatal("overflow record was not handed to the async spill channel")
	}
	// Only the seed file remains; the refused record created no new spool file.
	if n := mustReadDirLen(t, filepath.Join(dir, "test")); n != 1 {
		t.Fatalf("expected only the seed file after a refused spill, got %d files", n)
	}
}

// TestWriter_SpillWorker_DrainsChannelToDisk proves the live async spill worker
// (started by NewWriter) drains handed-off overflow records and writes them
// durably to NDJSON OFF the request path — the end-to-end async contract, no
// manual drive needed.
func TestWriter_SpillWorker_DrainsChannelToDisk(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 64, 512, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(&memProducer{}, "q", nil, slog.Default()).WithNDJSONSpill(spill).WithLossMode(lossModeSpill).Start()
	defer w.Close()

	for _, id := range []string{"w-1", "w-2", "w-3"} {
		w.spillCh <- &Record{RequestID: id} // the path Enqueue uses on overflow
	}
	// The worker flushes a partial batch on spillFlushInterval; allow a few.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(readSpool(t, dir, "test")) < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	if lines := readSpool(t, dir, "test"); len(lines) != 3 {
		t.Fatalf("async spill worker must write all 3 handed-off records; got %d", len(lines))
	}
}

// TestWriter_PublishRecord_SpillsWhenBufferFull proves the publish re-buffer
// overflow path spills the record instead of dropping it when a spill is wired.
func TestWriter_PublishRecord_SpillsWhenBufferFull(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 64, 512, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(&memProducer{alwaysFail: true}, "q", nil, slog.Default()).WithNDJSONSpill(spill).WithLossMode(lossModeSpill)
	saturate(w)

	w.publishRecord(&Record{RequestID: "pub-spill"}) // publish fails, buffer full → async spill hand-off

	// The overflow is handed to the batched spill channel (off the request path),
	// not written synchronously; drain it and drive the worker's write step.
	select {
	case rec := <-w.spillCh:
		if !w.spillRecord(rec) {
			t.Fatal("spillRecord must durably write the handed-off overflow record")
		}
	default:
		t.Fatal("publishRecord overflow must hand the record to the async spill channel")
	}

	lines := readSpool(t, dir, "test")
	if len(lines) != 1 || !strings.Contains(lines[0], "pub-spill") {
		t.Fatalf("publishRecord overflow must spill; got %v", lines)
	}
}

func mustReadDirLen(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	return len(entries)
}

// TestWriter_SpillWorker_BatchFullAndCloseDrain exercises the size-batched spill
// worker (records accumulate into the byte buffer, flushed via the ticker) and the
// stopCh drain on Close — every handed-off record must land durably, none lost at
// shutdown.
func TestWriter_SpillWorker_BatchFullAndCloseDrain(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 64, 512, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(&memProducer{}, "q", nil, slog.Default()).WithNDJSONSpill(spill).WithLossMode(lossModeSpill).Start()

	const n = 300 // many records; the byte-buffer flushes on the ticker / Close drain
	for i := range n {
		w.spillCh <- &Record{RequestID: "wb-" + string(rune('a'+i%26)) + "-" + itoa(i)}
	}
	w.Close() // stopCh drain must flush the remaining partial batch before exit

	if got := len(readSpool(t, dir, "test")); got != n {
		t.Fatalf("spill worker lost records across batch-full + Close drain: wrote %d, want %d", got, n)
	}
}

// TestSpillRecord_NilSink covers the no-sink branch: spillRecord drops + reclaims
// and returns false when no NDJSON sink is wired.
func TestSpillRecord_NilSink(t *testing.T) {
	w := NewWriter(&memProducer{}, "q", nil, slog.Default())
	defer w.Close()
	body, h := AcquireRequestBody([]byte("body"))
	rec := &Record{RequestID: "ns-1", RequestBody: body}
	rec.AttachPooledRequestBody(h)
	if w.spillRecord(rec) {
		t.Fatal("spillRecord must return false with no sink wired")
	}
	if rec.reqBodyHandle != nil {
		t.Fatal("spillRecord must reclaim the pooled body even on the no-sink drop")
	}
}

// TestSpillData_NilSinkAndWriteError covers spillData's no-sink and write-failure
// branches (the flush-side overflow spill for already-marshaled bytes).
func TestSpillData_NilSinkAndWriteError(t *testing.T) {
	w := NewWriter(&memProducer{}, "q", nil, slog.Default())
	defer w.Close()
	if w.spillData([]byte(`{"id":"x"}`)) {
		t.Fatal("spillData must return false with no sink wired")
	}

	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 1, 1, nil) // 1 MB quota
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test", "audit-20260101-0001.ndjson"), make([]byte, 1100*1024), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w.WithNDJSONSpill(spill).WithLossMode(lossModeSpill)
	if w.spillData([]byte(`{"id":"y"}`)) {
		t.Fatal("spillData must return false when the spool quota is exceeded")
	}
}

// itoa is a tiny test-local int->string (avoids importing strconv just for labels).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// TestWriter_BlockMode_BackpressuresThenPublishes proves the no-loss default: when
// the bounded queue is full, Enqueue BACK-PRESSURES (the blocking send waits) rather
// than dropping, and once a consumer worker frees a slot the record is published to
// the producer — never dropped, never spilled. With a healthy memProducer the
// consumers drain continuously, so the blocking send resolves quickly and the record
// lands.
func TestWriter_BlockMode_BackpressuresThenPublishes(t *testing.T) {
	prod := &memProducer{}
	w := NewWriter(prod, "q", nil, slog.Default()).WithMaxQueuedRecords(4).Start()
	defer w.Close()
	if w.lossMode != lossModeBlock {
		t.Fatalf("default lossMode = %q, want block (no-loss)", w.lossMode)
	}

	// A burst well past the queue cap: in block mode every Enqueue back-pressures on
	// a full queue until a consumer frees a slot — none may be dropped or spilled.
	const burst = 50
	done := make(chan struct{})
	go func() {
		for i := range burst {
			w.Enqueue(&Record{RequestID: fmt.Sprintf("bp-block-%d", i)})
		}
		close(done)
	}()
	select {
	case <-done: // back-pressure resolved via consumers draining space
	case <-time.After(5 * time.Second):
		t.Fatal("block-mode Enqueue never resolved — back-pressure should publish once consumers free space")
	}

	// No loss: block mode never spills on a healthy pipeline, and every record must
	// reach the producer.
	if len(w.spillCh) != 0 {
		t.Fatal("block mode must not spill on a healthy pipeline; it should back-pressure then publish")
	}
	if got := waitForMsgCount(prod, burst, 2*time.Second); got != burst {
		t.Fatalf("back-pressured burst published %d, want %d (no-loss violated)", got, burst)
	}
}
