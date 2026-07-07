package ndjson

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestQuotaGate_WrapsSentinel proves the total-quota refusal carries the
// ErrSpoolQuotaExceeded sentinel (so a no-loss caller can tell "spool full,
// retryable" from a genuine I/O failure), on BOTH Write and WriteBatch.
func TestQuotaGate_WrapsSentinel(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "inst", 1, 1, nil) // 1 MB file cap, 1 MB total quota
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Seed a countable audit-*.ndjson file past the quota.
	if err := os.WriteFile(filepath.Join(dir, "inst", "audit-20260101-0001.ndjson"),
		make([]byte, 1100*1024), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	errWrite := w.Write([]byte(`{"id":"a"}`))
	if !errors.Is(errWrite, ErrSpoolQuotaExceeded) {
		t.Fatalf("Write over quota must wrap ErrSpoolQuotaExceeded; got %v", errWrite)
	}
	errBatch := w.WriteBatch([]byte("{\"id\":\"a\"}\n"))
	if !errors.Is(errBatch, ErrSpoolQuotaExceeded) {
		t.Fatalf("WriteBatch over quota must wrap ErrSpoolQuotaExceeded; got %v", errBatch)
	}
}

// TestGenuineWriteError_NotQuotaSentinel proves a real I/O failure (injected via the
// openSpool seam) does NOT match the quota sentinel — so a no-loss caller drops /
// dead-letters it rather than back-pressuring forever on a broken disk.
func TestGenuineWriteError_NotQuotaSentinel(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "inst", 64, 512, nil) // well under quota
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	orig := openSpool
	t.Cleanup(func() { openSpool = orig })
	openSpool = func(string) (spoolFile, int64, error) {
		return nil, 0, errors.New("audit/ndjson: open file: injected io error")
	}
	got := w.Write([]byte(`{"id":"x"}`))
	if got == nil {
		t.Fatal("Write must surface the injected open error")
	}
	if errors.Is(got, ErrSpoolQuotaExceeded) {
		t.Fatalf("a genuine I/O error must NOT match the quota sentinel; got %v", got)
	}
}

// TestDirSizeQuota_ExcludesPoison proves the reclaimable-quota gate counts only
// audit-*.ndjson and ignores .poison sidecars (and any non-audit entry). Without
// this, undrainable poison would monotonically consume the quota and — under a
// back-pressuring no-loss caller — wedge audit intake forever.
func TestDirSizeQuota_ExcludesPoison(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "inst", 1, 1, nil) // 1 MB total quota
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A big .poison sidecar (well over the quota) that must NOT count.
	if err := os.WriteFile(filepath.Join(dir, "inst", "audit-20260101-0001.ndjson.poison"),
		make([]byte, 2*1024*1024), 0o600); err != nil {
		t.Fatalf("seed poison: %v", err)
	}
	// A write must SUCCEED — the 2 MB poison does not count toward the 1 MB quota.
	if err := w.Write([]byte(`{"id":"ok"}`)); err != nil {
		t.Fatalf("write must succeed when only .poison exceeds the quota; got %v", err)
	}

	// Now a big countable audit-*.ndjson file DOES push the reclaimable total over
	// quota → the next write is refused with the sentinel.
	if err := os.WriteFile(filepath.Join(dir, "inst", "audit-20260101-0002.ndjson"),
		make([]byte, 1100*1024), 0o600); err != nil {
		t.Fatalf("seed audit: %v", err)
	}
	if got := w.Write([]byte(`{"id":"no"}`)); !errors.Is(got, ErrSpoolQuotaExceeded) {
		t.Fatalf("a countable audit-*.ndjson over quota must refuse with the sentinel; got %v", got)
	}
}
