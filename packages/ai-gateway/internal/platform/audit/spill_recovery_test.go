package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
	registry "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/prometheus/client_golang/prometheus"
)

// fakeBatchProducer captures the frames a spillRecovery publishes and can inject
// failures, so the no-loss contract (delete only after every frame is acked) is
// exercised without a live broker. It implements the unexported batchProducer.
type fakeBatchProducer struct {
	mu       sync.Mutex
	received [][]byte // every frame accepted (acked), across all calls
	calls    int

	hardErr      error // EnqueueBatchAsync returns this (whole call fails) when set
	failNthFrame int   // index (within a single call) to nak; -1 = none
}

func newFakeBatchProducer() *fakeBatchProducer {
	return &fakeBatchProducer{failNthFrame: -1}
}

// setHardErr mutates hardErr under the lock. EnqueueBatchAsync reads hardErr while
// holding f.mu on the recovery goroutine, so tests that flip the broker up/down
// mid-run must write it through here, not directly, to stay race-free under -race.
func (f *fakeBatchProducer) setHardErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hardErr = err
}

func (f *fakeBatchProducer) EnqueueBatchAsync(_ context.Context, _ string, batch [][]byte) ([]error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.hardErr != nil {
		return nil, f.hardErr
	}
	errs := make([]error, len(batch))
	for i, fr := range batch {
		if i == f.failNthFrame {
			errs[i] = errors.New("simulated per-frame nak")
			continue
		}
		f.received = append(f.received, append([]byte(nil), fr...))
	}
	return errs, nil
}

// records returns every audit record line the producer accepted, splitting each
// received NDJSON frame back into its constituent records (the wire form the Hub
// would re-split). Order is not guaranteed meaningful; callers compare as a set.
func (f *fakeBatchProducer) records() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, fr := range f.received {
		for _, line := range bytes.Split(fr, []byte{'\n'}) {
			if len(line) > 0 {
				out = append(out, string(line))
			}
		}
	}
	return out
}

func (f *fakeBatchProducer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// writeSpill writes records to a fresh ndjson spool and returns the spool writer.
func newSpool(t *testing.T) (*sharedndjson.Writer, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := sharedndjson.New(dir, "test-gw", 64, 4096, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	return w, filepath.Join(dir, "test-gw")
}

func spillN(t *testing.T, w *sharedndjson.Writer, n int) []string {
	t.Helper()
	recs := make([]string, n)
	for i := range n {
		rec := fmt.Sprintf(`{"id":"req-%d","request_body":{"k":"v-%d"}}`, i, i)
		recs[i] = rec
		if err := w.Write([]byte(rec)); err != nil {
			t.Fatalf("spill write %d: %v", i, err)
		}
	}
	return recs
}

func ndjsonFiles(t *testing.T, instanceDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(instanceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func assertSameSet(t *testing.T, want, got []string) {
	t.Helper()
	wc := map[string]int{}
	for _, w := range want {
		wc[w]++
	}
	for _, g := range got {
		wc[g]--
	}
	for k, v := range wc {
		if v != 0 {
			t.Fatalf("record set mismatch for %q: residual %d (want %d, got %d)", k, v, len(want), len(got))
		}
	}
}

func newTestRecovery(src spillSource, bp *fakeBatchProducer, frameMax, batchMax int) (*spillRecovery, *int) {
	reingested := 0
	r := newSpillRecovery(src, bp, "nexus.event.ai-traffic", frameMax, batchMax, 0, false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.onReingested = func(n int) { reingested += n }
	return r, &reingested
}

// TestSpillRecovery_HappyPath_AllRecordsReingestedAndDeleted is the core no-loss
// assertion: every spilled record reaches the producer and the spool file is
// removed once durably acked.
func TestSpillRecovery_HappyPath_AllRecordsReingestedAndDeleted(t *testing.T) {
	w, instanceDir := newSpool(t)
	recs := spillN(t, w, 50)

	bp := newFakeBatchProducer()
	r, reingested := newTestRecovery(w, bp, 1<<20, 1<<20)
	r.runOnce(context.Background())

	assertSameSet(t, recs, bp.records())
	if *reingested != len(recs) {
		t.Fatalf("reingested=%d want %d", *reingested, len(recs))
	}
	if files := ndjsonFiles(t, instanceDir); len(files) != 0 {
		t.Fatalf("spool not drained: %v", files)
	}
}

// TestSpillRecovery_PartialFrameNak_LeavesFileThenIdempotentReplay proves a
// per-frame nak leaves the file for the next pass and the replay is idempotent
// (no record lost; the already-acked records simply reappear, which the Hub
// dedupes by id).
func TestSpillRecovery_PartialFrameNak_LeavesFileThenIdempotentReplay(t *testing.T) {
	w, instanceDir := newSpool(t)
	recs := spillN(t, w, 40)

	bp := newFakeBatchProducer()
	bp.failNthFrame = 1 // small frames → many frames → frame index 1 naks
	// Small frame cap forces several frames so failNthFrame=1 is reached.
	r, _ := newTestRecovery(w, bp, 128, 1<<20)

	recoveryErrors := 0
	r.onError = func() { recoveryErrors++ }

	r.runOnce(context.Background())
	if recoveryErrors == 0 {
		t.Fatal("expected a recovery error on the naked frame")
	}
	if files := ndjsonFiles(t, instanceDir); len(files) != 1 {
		t.Fatalf("file should be LEFT after partial fail, got %v", files)
	}

	// Second pass: broker healthy now → file drains, every record present at least once.
	bp.failNthFrame = -1
	r.runOnce(context.Background())
	if files := ndjsonFiles(t, instanceDir); len(files) != 0 {
		t.Fatalf("file should be drained on retry, got %v", files)
	}
	got := map[string]bool{}
	for _, g := range bp.records() {
		got[g] = true
	}
	for _, want := range recs {
		if !got[want] {
			t.Fatalf("record lost across retry: %q", want)
		}
	}
}

// TestSpillRecovery_HardPublishError_LeavesFile: a whole-call enqueue error never
// deletes the file (no-loss).
func TestSpillRecovery_HardPublishError_LeavesFile(t *testing.T) {
	w, instanceDir := newSpool(t)
	spillN(t, w, 10)

	bp := newFakeBatchProducer()
	bp.setHardErr(errors.New("broker down"))
	r, reingested := newTestRecovery(w, bp, 1<<20, 1<<20)
	recoveryErrors := 0
	r.onError = func() { recoveryErrors++ }

	r.runOnce(context.Background())
	if *reingested != 0 {
		t.Fatalf("nothing should be counted reingested on hard error, got %d", *reingested)
	}
	if recoveryErrors == 0 {
		t.Fatal("expected recovery error")
	}
	if files := ndjsonFiles(t, instanceDir); len(files) != 1 {
		t.Fatalf("file must be left on hard error, got %v", files)
	}
}

// TestSpillRecovery_EmptySealedFile_Deleted: a sealed but empty spool file is
// removed without a publish.
func TestSpillRecovery_EmptySealedFile_Deleted(t *testing.T) {
	w, instanceDir := newSpool(t)
	// Force an empty sealed file: open one via a write then truncate is awkward;
	// instead create an empty file directly with the recognised name shape.
	empty := filepath.Join(instanceDir, "audit-20200101-9999.ndjson")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("create empty: %v", err)
	}
	bp := newFakeBatchProducer()
	r, _ := newTestRecovery(w, bp, 1<<20, 1<<20)
	r.runOnce(context.Background())
	if bp.callCount() != 0 {
		t.Fatalf("empty file must not publish, calls=%d", bp.callCount())
	}
	for _, f := range ndjsonFiles(t, instanceDir) {
		if f == "audit-20200101-9999.ndjson" {
			t.Fatal("empty sealed file should be deleted")
		}
	}
}

// TestSpillRecovery_LargeFile_MultiBatch_AllDelivered: a file larger than
// batchMaxBytes is published in several EnqueueBatchAsync calls and only deleted
// when every batch acked — no record lost across the batch boundary.
func TestSpillRecovery_LargeFile_MultiBatch_AllDelivered(t *testing.T) {
	w, instanceDir := newSpool(t)
	recs := spillN(t, w, 200)

	bp := newFakeBatchProducer()
	// Tiny batch budget so the 200 records span many EnqueueBatchAsync calls.
	r, reingested := newTestRecovery(w, bp, 128, 512)
	r.runOnce(context.Background())

	if bp.callCount() < 2 {
		t.Fatalf("expected multiple publish batches, got %d", bp.callCount())
	}
	assertSameSet(t, recs, bp.records())
	if *reingested != len(recs) {
		t.Fatalf("reingested=%d want %d", *reingested, len(recs))
	}
	if files := ndjsonFiles(t, instanceDir); len(files) != 0 {
		t.Fatalf("large file not drained: %v", files)
	}
}

// TestSpillRecovery_OversizeRecord_ShipsAloneNoLoop: a single record larger than
// the frame cap is shipped alone (never dropped, never an infinite retry loop).
func TestSpillRecovery_OversizeRecord_ShipsAloneNoLoop(t *testing.T) {
	w, instanceDir := newSpool(t)
	big := `{"id":"big","body":"` + string(bytes.Repeat([]byte("x"), 4096)) + `"}`
	if err := w.Write([]byte(big)); err != nil {
		t.Fatalf("write big: %v", err)
	}
	small := `{"id":"small"}`
	if err := w.Write([]byte(small)); err != nil {
		t.Fatalf("write small: %v", err)
	}

	bp := newFakeBatchProducer()
	r, _ := newTestRecovery(w, bp, 256, 1<<20) // frameMax 256 < big record
	r.runOnce(context.Background())

	assertSameSet(t, []string{big, small}, bp.records())
	if files := ndjsonFiles(t, instanceDir); len(files) != 0 {
		t.Fatalf("file not drained: %v", files)
	}
}

// TestSpillRecovery_OversizeRecord_PoisonedNotWedged: when the broker max_payload
// is known, a record exceeding it is dead-lettered to a .poison sidecar (durable,
// no loss) and the file still drains+deletes — it does not wedge forever.
func TestSpillRecovery_OversizeRecord_PoisonedNotWedged(t *testing.T) {
	w, instanceDir := newSpool(t)
	small1 := `{"id":"s1"}`
	big := `{"id":"big","b":"` + string(bytes.Repeat([]byte("x"), 5000)) + `"}`
	small2 := `{"id":"s2"}`
	for _, r := range []string{small1, big, small2} {
		if err := w.Write([]byte(r)); err != nil {
			t.Fatalf("spill write: %v", err)
		}
	}
	bp := newFakeBatchProducer()
	r, reingested := newTestRecovery(w, bp, 1<<20, 1<<20)
	r.maxRecordBytes = 1024 // big (5000+) exceeds → poison; smalls publish
	poisoned := 0
	r.onPoisoned = func(n int) { poisoned += n }

	r.runOnce(context.Background())

	// The two small records reached the broker; the big one did not.
	assertSameSet(t, []string{small1, small2}, bp.records())
	if *reingested != 2 || poisoned != 1 {
		t.Fatalf("reingested=%d poisoned=%d want 2/1", *reingested, poisoned)
	}
	// The spool file is deleted (not wedged), and the big record is durably in a
	// .poison sidecar (no loss).
	for _, f := range ndjsonFiles(t, instanceDir) {
		if strings.HasSuffix(f, ".ndjson") {
			t.Fatalf("spool file should be drained+deleted, found %s", f)
		}
	}
	poisonFound := false
	entries, _ := os.ReadDir(instanceDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".poison") {
			poisonFound = true
			b, _ := os.ReadFile(filepath.Join(instanceDir, e.Name()))
			if !strings.Contains(string(b), `"id":"big"`) {
				t.Fatalf("poison sidecar missing the big record: %q", b)
			}
		}
	}
	if !poisonFound {
		t.Fatal("expected a .poison sidecar for the oversize record")
	}
}

// TestSpillRecovery_PoisonWriteError_LeavesFile: if the dead-letter write fails,
// the record must NOT be lost — the file is left for the next pass.
func TestSpillRecovery_PoisonWriteError_LeavesFile(t *testing.T) {
	w, instanceDir := newSpool(t)
	big := `{"id":"big","b":"` + string(bytes.Repeat([]byte("x"), 5000)) + `"}`
	if err := w.Write([]byte(big)); err != nil {
		t.Fatalf("spill write: %v", err)
	}
	bp := newFakeBatchProducer()
	r, _ := newTestRecovery(w, bp, 1<<20, 1<<20)
	r.maxRecordBytes = 1024
	r.appendPoison = func(string, []byte) error { return errors.New("EROFS") }
	recoveryErrors := 0
	r.onError = func() { recoveryErrors++ }

	r.runOnce(context.Background())
	if recoveryErrors == 0 {
		t.Fatal("expected a recovery error when poison write fails")
	}
	found := false
	for _, f := range ndjsonFiles(t, instanceDir) {
		if strings.HasSuffix(f, ".ndjson") {
			found = true
		}
	}
	if !found {
		t.Fatal("spool file must be LEFT when the oversize record cannot be dead-lettered")
	}
}

// TestSpillRecovery_FramingOff_OneMessagePerRecord guards the framing-off path:
// with frameMaxBytes<=0 each record must publish as its OWN message, never packed
// into one mega-frame that would exceed the broker max_payload.
func TestSpillRecovery_FramingOff_OneMessagePerRecord(t *testing.T) {
	recs := []string{`{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`}
	path := writeRawNDJSON(t, recs)
	src := &fakeSpillSource{files: []string{path}}
	bp := newFakeBatchProducer()
	r, _ := newTestRecovery(src, bp, 0 /*framing OFF*/, 1<<20)
	r.runOnce(context.Background())

	bp.mu.Lock()
	frames := len(bp.received)
	bp.mu.Unlock()
	if frames != 3 {
		t.Fatalf("framing-off must publish one message per record: got %d frames want 3", frames)
	}
	for _, fr := range bp.received {
		if n := bytes.Count(fr, []byte{'\n'}); n != 1 {
			t.Fatalf("each framing-off message must carry exactly one record, got %d newlines in %q", n, fr)
		}
	}
	assertSameSet(t, recs, bp.records())
}

// TestSpillRecovery_ReadError_LeavesFile: an open/read failure leaves the file
// (no partial delete).
func TestSpillRecovery_ReadError_LeavesFile(t *testing.T) {
	w, instanceDir := newSpool(t)
	spillN(t, w, 5)

	bp := newFakeBatchProducer()
	r, _ := newTestRecovery(w, bp, 1<<20, 1<<20)
	r.openFile = func(string) (io.ReadCloser, error) { return nil, errors.New("EIO") }
	r.runOnce(context.Background())

	if bp.callCount() != 0 {
		t.Fatalf("no publish on read error, calls=%d", bp.callCount())
	}
	if files := ndjsonFiles(t, instanceDir); len(files) != 1 {
		t.Fatalf("file must be left on read error, got %v", files)
	}
}

// TestSpillRecovery_DeleteError_RecordsStillReingested: a delete failure after a
// durable publish is non-fatal — the records are counted (they are in the broker)
// and the next pass re-drains the file (Hub dedupes).
func TestSpillRecovery_DeleteError_RecordsStillReingested(t *testing.T) {
	w, _ := newSpool(t)
	recs := spillN(t, w, 8)

	bp := newFakeBatchProducer()
	r, reingested := newTestRecovery(w, bp, 1<<20, 1<<20)
	r.removeFile = func(string) error { return errors.New("EROFS") }
	r.runOnce(context.Background())

	assertSameSet(t, recs, bp.records())
	if *reingested != len(recs) {
		t.Fatalf("records durably published must count even if delete fails: reingested=%d", *reingested)
	}
}

// TestSpillRecovery_CtxCancelMidFile_LeavesFile: cancelling context before a sweep
// stops it without deleting an undrained file.
func TestSpillRecovery_CtxCancelMidFile_LeavesFile(t *testing.T) {
	w, instanceDir := newSpool(t)
	spillN(t, w, 5)

	bp := newFakeBatchProducer()
	r, _ := newTestRecovery(w, bp, 1<<20, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	r.runOnce(ctx)

	// Rotate may have sealed the active file; with ctx cancelled the loop skips
	// draining, so the file remains.
	if files := ndjsonFiles(t, instanceDir); len(files) == 0 {
		t.Fatal("cancelled sweep must not delete the undrained file")
	}
}

// fakeSpillSource drives the runOnce error branches (Rotate / SealedFiles
// failures) that a real ndjson.Writer will not produce on demand.
type fakeSpillSource struct {
	rotateErr error
	listErr   error
	files     []string
}

func (f *fakeSpillSource) Rotate() error                  { return f.rotateErr }
func (f *fakeSpillSource) SealedFiles() ([]string, error) { return f.files, f.listErr }

// writeRawNDJSON writes records as an NDJSON file the sweeper can read directly.
func writeRawNDJSON(t *testing.T, recs []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit-20260101-0001.ndjson")
	var buf bytes.Buffer
	for _, r := range recs {
		buf.WriteString(r)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write raw ndjson: %v", err)
	}
	return path
}

// TestSpillRecovery_RotateError_StillDrainsSealed: a Rotate() failure is logged
// but already-sealed files are still drained (no-loss continues).
func TestSpillRecovery_RotateError_StillDrainsSealed(t *testing.T) {
	recs := []string{`{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`}
	path := writeRawNDJSON(t, recs)
	src := &fakeSpillSource{rotateErr: errors.New("rotate boom"), files: []string{path}}
	bp := newFakeBatchProducer()
	r, reingested := newTestRecovery(src, bp, 1<<20, 1<<20)
	r.runOnce(context.Background())
	assertSameSet(t, recs, bp.records())
	if *reingested != 3 {
		t.Fatalf("reingested=%d want 3", *reingested)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("drained file should be deleted, stat err=%v", err)
	}
}

// TestSpillRecovery_SealedFilesError_NoPublish: a list failure aborts the sweep
// without publishing.
func TestSpillRecovery_SealedFilesError_NoPublish(t *testing.T) {
	src := &fakeSpillSource{listErr: errors.New("list boom")}
	bp := newFakeBatchProducer()
	r, _ := newTestRecovery(src, bp, 1<<20, 1<<20)
	r.runOnce(context.Background())
	if bp.callCount() != 0 {
		t.Fatalf("no publish expected on list error, calls=%d", bp.callCount())
	}
}

// TestSpillRecovery_PaceBetweenFiles exercises the inter-file pacing sleep.
func TestSpillRecovery_PaceBetweenFiles(t *testing.T) {
	p1 := writeRawNDJSON(t, []string{`{"id":"a"}`})
	p2 := writeRawNDJSON(t, []string{`{"id":"b"}`})
	src := &fakeSpillSource{files: []string{p1, p2}}
	bp := newFakeBatchProducer()
	r, reingested := newTestRecovery(src, bp, 1<<20, 1<<20)
	r.pace = time.Millisecond
	r.runOnce(context.Background())
	if *reingested != 2 {
		t.Fatalf("both files should drain with pacing, reingested=%d", *reingested)
	}
}

// TestNewSpillRecovery_BatchMaxDefault: a non-positive batchMax falls back to the
// package default.
func TestNewSpillRecovery_BatchMaxDefault(t *testing.T) {
	r := newSpillRecovery(&fakeSpillSource{}, newFakeBatchProducer(), "q", 0, 0, 0, false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if r.batchMaxBytes != batchMaxBytes {
		t.Fatalf("batchMax default = %d want %d", r.batchMaxBytes, batchMaxBytes)
	}
}

// TestWriter_SpillRecovery_DisabledWhenInterval0: interval<=0 starts no sweeper.
func TestWriter_SpillRecovery_DisabledWhenInterval0(t *testing.T) {
	w := NewWriter(&frameCapProducer{}, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.WithSpillRecovery(0, 0)
	w.startSpillRecovery() // exercises the interval<=0 early return
	w.Close()
}

// TestWriter_SpillRecovery_DisabledWhenNoBatchProducer: a non-batch producer (or
// nil spool) logs and stays off.
func TestWriter_SpillRecovery_DisabledWhenNoBatchProducer(t *testing.T) {
	// nil producer is not a batchProducer.
	w := NewWriter(nil, "q", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.WithSpillRecovery(time.Second, 0)
	w.startSpillRecovery() // exercises the !ok / spool==nil warn-return
	w.Close()
}

// TestWriter_SpillRecovery_EnabledDrainsViaWriter wires the real Writer path:
// records pre-spilled to the ndjson sink are drained by the started sweeper and
// the reingested metric advances.
func TestWriter_SpillRecovery_EnabledDrainsViaWriter(t *testing.T) {
	reg := registry.NewRegistry(prometheus.NewRegistry())
	prod := &frameCapProducer{}
	w := NewWriter(prod, "nexus.event.ai-traffic", reg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	spool, _ := sharedndjson.New(t.TempDir(), "gw", 64, 4096, nil)
	for i := range 12 {
		if err := spool.Write([]byte(fmt.Sprintf(`{"id":"r-%d"}`, i))); err != nil {
			t.Fatalf("spill write: %v", err)
		}
	}
	w.WithNDJSONSpill(spool)
	w.WithSpillRecovery(5*time.Millisecond, 0)
	w.Start()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if prod.recordsPublished() >= 12 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.Close()
	if got := prod.recordsPublished(); got < 12 {
		t.Fatalf("sweeper did not drain all spilled records: got %d want >=12", got)
	}
}

// TestAuditMetrics_RecoveryCounters exercises the reingested / recovery-error
// counter helpers (incl. the nil-receiver no-op).
func TestAuditMetrics_RecoveryCounters(t *testing.T) {
	var nilM *auditMetrics
	nilM.addReingested(3) // must not panic
	nilM.incRecoveryErrors()

	nilM.addPoisoned(2) // must not panic on nil receiver

	m := newAuditMetrics(registry.NewRegistry(prometheus.NewRegistry()))
	m.addReingested(5)
	m.addReingested(0) // no-op branch
	m.incRecoveryErrors()
	m.addPoisoned(3)
	m.addPoisoned(0) // no-op branch
}

// bytesThenErr yields data on the first read, then a non-EOF error — to drive the
// mid-stream read-failure branch of drainFile.
type bytesThenErr struct {
	data []byte
	err  error
	done bool
}

func (r *bytesThenErr) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), nil
}
func (r *bytesThenErr) Close() error { return nil }

// TestSpillRecovery_ReadErrorMidStream_LeavesFile: a read failure partway through
// a file leaves it (no partial delete, no loss).
func TestSpillRecovery_ReadErrorMidStream_LeavesFile(t *testing.T) {
	path := writeRawNDJSON(t, []string{`{"id":"a"}`})
	src := &fakeSpillSource{files: []string{path}}
	bp := newFakeBatchProducer()
	r, _ := newTestRecovery(src, bp, 1<<20, 1<<20)
	r.openFile = func(string) (io.ReadCloser, error) {
		return &bytesThenErr{data: []byte(`{"id":"a"}`), err: errors.New("EIO")}, nil
	}
	r.runOnce(context.Background())
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file must be left on mid-stream read error: %v", err)
	}
}

// TestSpillRecovery_MidBatchFlushFail_LeavesFile: a flush failure at a
// batchMaxBytes boundary (mid-file) aborts and leaves the file.
func TestSpillRecovery_MidBatchFlushFail_LeavesFile(t *testing.T) {
	recs := make([]string, 50)
	for i := range recs {
		recs[i] = fmt.Sprintf(`{"id":"r-%d"}`, i)
	}
	path := writeRawNDJSON(t, recs)
	src := &fakeSpillSource{files: []string{path}}
	bp := newFakeBatchProducer()
	bp.setHardErr(errors.New("broker down"))
	r, _ := newTestRecovery(src, bp, 64, 128) // small frame + batch → mid-loop flush
	r.runOnce(context.Background())
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file must be left after mid-batch flush failure: %v", err)
	}
}

// TestSpillRecovery_PaceCtxCancel_StopsMidSweep: cancelling during the inter-file
// pace sleep stops the sweep without draining the remaining files.
func TestSpillRecovery_PaceCtxCancel_StopsMidSweep(t *testing.T) {
	p1 := writeRawNDJSON(t, []string{`{"id":"a"}`})
	p2 := writeRawNDJSON(t, []string{`{"id":"b"}`})
	src := &fakeSpillSource{files: []string{p1, p2}}
	bp := newFakeBatchProducer()
	r, _ := newTestRecovery(src, bp, 1<<20, 1<<20)
	r.pace = 150 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	r.runOnce(ctx)
	// First file drained before the pace sleep; the cancel fires during the sleep,
	// so the second file is left.
	if _, err := os.Stat(p2); err != nil {
		t.Fatalf("second file should be left when cancelled during pace: %v", err)
	}
}

// TestSpillRecovery_RunOnce_ReturnsFailureCount: runOnce returns the number of
// files left undrained — the signal run() uses to back off.
func TestSpillRecovery_RunOnce_ReturnsFailureCount(t *testing.T) {
	p1 := writeRawNDJSON(t, []string{`{"id":"a"}`})
	p2 := writeRawNDJSON(t, []string{`{"id":"b"}`})
	src := &fakeSpillSource{files: []string{p1, p2}}
	bp := newFakeBatchProducer()
	bp.setHardErr(errors.New("broker full"))
	r, _ := newTestRecovery(src, bp, 1<<20, 1<<20)
	if got := r.runOnce(context.Background()); got != 2 {
		t.Fatalf("runOnce failures = %d want 2 (both files left)", got)
	}
	// Healthy now → 0 failures, both drain.
	bp.setHardErr(nil)
	if got := r.runOnce(context.Background()); got != 0 {
		t.Fatalf("runOnce failures = %d want 0 after recovery", got)
	}
}

// TestSpillRecovery_RunOnce_ListErrorBacksOff: a SealedFiles error returns a
// non-zero failure count so run() backs off rather than tight-looping.
func TestSpillRecovery_RunOnce_ListErrorBacksOff(t *testing.T) {
	src := &fakeSpillSource{listErr: errors.New("boom")}
	r, _ := newTestRecovery(src, newFakeBatchProducer(), 1<<20, 1<<20)
	if got := r.runOnce(context.Background()); got == 0 {
		t.Fatal("list error should report a failure for backoff")
	}
}

// TestSpillRecovery_Run_BacksOffThenRecovers drives the run loop across a failing
// then healthy broker and confirms it keeps draining (eventually) without spinning.
func TestSpillRecovery_Run_BacksOffThenRecovers(t *testing.T) {
	path := writeRawNDJSON(t, []string{`{"id":"a"}`})
	src := &fakeSpillSource{files: []string{path}}
	bp := newFakeBatchProducer()
	bp.setHardErr(errors.New("full"))
	r, _ := newTestRecovery(src, bp, 1<<20, 1<<20)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.run(ctx, 5*time.Millisecond); close(done) }()
	time.Sleep(30 * time.Millisecond) // a few failing sweeps (backing off)
	bp.setHardErr(nil)                // broker recovers
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bp.records()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if len(bp.records()) < 1 {
		t.Fatal("sweeper did not drain after the broker recovered")
	}
}

// TestSpillRecovery_Run_StopsOnCtxCancel ensures the run loop exits promptly.
func TestSpillRecovery_Run_StopsOnCtxCancel(t *testing.T) {
	w, _ := newSpool(t)
	bp := newFakeBatchProducer()
	r, _ := newTestRecovery(w, bp, 1<<20, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.run(ctx, 10*time.Millisecond); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop on ctx cancel")
	}
}
