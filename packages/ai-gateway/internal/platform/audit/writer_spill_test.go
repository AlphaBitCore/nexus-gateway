package audit

import (
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
			if l != "" {
				lines = append(lines, l)
			}
		}
	}
	return lines
}

// saturate stops the flush loop and pre-fills the buffer to maxQueueSize so
// the next Enqueue/publishRecord deterministically hits the overflow path.
func saturate(w *Writer) {
	w.Close() // stop the flush loop; the buffer is now under the test's control
	w.mu.Lock()
	w.buf = make([]*Record, maxQueueSize)
	w.mu.Unlock()
}

// TestWriter_SpillsOnOverflowToNDJSON proves a record that cannot be buffered
// after the backpressure window is written durably to the NDJSON spill — not
// dropped.
func TestWriter_SpillsOnOverflowToNDJSON(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 64, 512, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(&memProducer{}, "q", nil, slog.Default()).WithNDJSONSpill(spill)
	saturate(w)

	w.Enqueue(&Record{RequestID: "spilled-1"}) // no drain → backpressure → spill

	lines := readSpool(t, dir, "test")
	if len(lines) != 1 || !strings.Contains(lines[0], "spilled-1") {
		t.Fatalf("overflow record must spill to disk; got %v", lines)
	}
}

// TestWriter_BackpressureSucceedsWhenSpaceFrees proves the bounded backpressure
// loop appends the record (no spill, no drop) once the drain frees a slot.
func TestWriter_BackpressureSucceedsWhenSpaceFrees(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 64, 512, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	w := NewWriter(&memProducer{}, "q", nil, slog.Default()).WithNDJSONSpill(spill)
	saturate(w)

	done := make(chan struct{})
	go func() {
		w.Enqueue(&Record{RequestID: "bp-1"})
		close(done)
	}()

	// Free one slot mid-backpressure; the loop's next tryAppend must land it.
	time.Sleep(20 * time.Millisecond)
	w.mu.Lock()
	w.buf = w.buf[:maxQueueSize-1]
	w.mu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue never appended after space freed (backpressure-success path stuck)")
	}

	w.mu.Lock()
	got := len(w.buf)
	w.mu.Unlock()
	if got != maxQueueSize {
		t.Fatalf("buffer = %d, want %d (record should have been appended, not spilled)", got, maxQueueSize)
	}
	if lines := mustReadDirLen(t, filepath.Join(dir, "test")); lines != 0 {
		t.Fatalf("backpressure success must not spill; found %d spool files", lines)
	}
}

// TestWriter_SpillWriteErrorFallsToLoudDrop proves that when even the spill is
// refused (quota exceeded), the record is dropped loudly rather than wedged —
// no new spool line beyond the pre-seeded quota filler.
func TestWriter_SpillWriteErrorFallsToLoudDrop(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 1, 1, nil) // 1 MB quota
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	// Pre-seed past the quota so the next spill write is refused.
	if err := os.WriteFile(filepath.Join(dir, "test", "seed.ndjson"), make([]byte, 1100*1024), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := NewWriter(&memProducer{}, "q", nil, slog.Default()).WithNDJSONSpill(spill)
	saturate(w)

	w.Enqueue(&Record{RequestID: "drop-1"}) // backpressure → spill refused → loud drop

	// Only the seed file remains; the dropped record created no new spool file.
	if n := mustReadDirLen(t, filepath.Join(dir, "test")); n != 1 {
		t.Fatalf("expected only the seed file after a refused spill, got %d files", n)
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
	w := NewWriter(&memProducer{alwaysFail: true}, "q", nil, slog.Default()).WithNDJSONSpill(spill)
	saturate(w)

	w.publishRecord(&Record{RequestID: "pub-spill"}) // publish fails, buffer full → spill

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
