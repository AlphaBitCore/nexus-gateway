package ndjson

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteBatch_WritesWholeBlockAsOneLineSet proves WriteBatch persists a
// pre-assembled multi-line block verbatim (each record readable back as its own
// NDJSON line) and reports the exact byte count to onWrite — the batched-append
// contract the spill worker relies on.
func TestWriteBatch_WritesWholeBlockAsOneLineSet(t *testing.T) {
	dir := t.TempDir()
	var wroteBytes int
	w, err := New(dir, "inst-batch", 10, 100, func(n int) { wroteBytes += n })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	block := []byte(`{"id":"a"}` + "\n" + `{"id":"b"}` + "\n" + `{"id":"c"}` + "\n")
	if err := w.WriteBatch(block); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	lines := readSpoolLines(t, dir, "inst-batch")
	want := []string{`{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %v", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
	if wroteBytes != len(block) {
		t.Fatalf("onWrite total = %d, want %d (whole block byte count)", wroteBytes, len(block))
	}
}

// TestWriteBatch_EmptyBlockIsNoOp proves an empty block neither errors nor opens
// a spool file (no zero-byte file litters the directory).
func TestWriteBatch_EmptyBlockIsNoOp(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "inst-empty", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if err := w.WriteBatch(nil); err != nil {
		t.Fatalf("empty WriteBatch must be a no-op, got: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "inst-empty"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty WriteBatch must not create a spool file, got %d entries", len(entries))
	}
}

// TestWriteBatch_RefusesWhenQuotaExceeded proves WriteBatch enforces the same
// total-quota gate as Write: once the spool exceeds quota the next batch is
// refused loudly with the quota reason rather than filling the disk.
func TestWriteBatch_RefusesWhenQuotaExceeded(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "inst-bquota", 10, 1, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	big := make([]byte, 1100*1024)
	for i := range big {
		big[i] = 'z'
	}
	big = append(big, '\n')
	if err := w.WriteBatch(big); err != nil {
		t.Fatalf("first batch should land (pre-write quota check): %v", err)
	}
	err = w.WriteBatch(big)
	if err == nil {
		t.Fatal("second batch must be refused once the spool exceeds its quota")
	}
	if !strings.Contains(err.Error(), "exceeds quota") {
		t.Fatalf("quota error missing expected reason: %v", err)
	}
}

// TestWriteBatch_RotatesWhenBlockExceedsMaxFile proves a batch that would carry
// the current file past maxFileSize triggers a rotation, producing a fresh
// sealed file for the next batch.
func TestWriteBatch_RotatesWhenBlockExceedsMaxFile(t *testing.T) {
	dir := t.TempDir()
	// 1 MB max file; each ~0.6 MB block forces the second batch to rotate.
	w, err := New(dir, "inst-brotate", 1, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	block := make([]byte, 600*1024)
	for i := range block {
		block[i] = 'q'
	}
	block[len(block)-1] = '\n'
	if err := w.WriteBatch(block); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if err := w.WriteBatch(block); err != nil {
		t.Fatalf("second batch: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "inst-brotate"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected >=2 spool files after batch rotation, got %d", len(entries))
	}
}

// TestWriteBatch_SurfacesHandleWriteError proves a handle write failure during a
// batch append is wrapped and surfaced (so the caller can re-buffer the block).
func TestWriteBatch_SurfacesHandleWriteError(t *testing.T) {
	orig := openSpool
	t.Cleanup(func() { openSpool = orig })
	openSpool = func(string) (spoolFile, int64, error) {
		return &fakeFile{writeErr: errors.New("disk full")}, 0, nil
	}
	w, err := New(t.TempDir(), "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = w.WriteBatch([]byte("{}\n"))
	if err == nil || !strings.Contains(err.Error(), "write batch") {
		t.Fatalf("want a wrapped write-batch error, got: %v", err)
	}
}

// TestWriteBatch_SurfacesOpenError proves a failure to open the spool file when
// no file is yet open propagates out of WriteBatch.
func TestWriteBatch_SurfacesOpenError(t *testing.T) {
	orig := openSpool
	t.Cleanup(func() { openSpool = orig })
	openSpool = func(string) (spoolFile, int64, error) {
		return nil, 0, errors.New("open denied")
	}
	w, err := New(t.TempDir(), "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = w.WriteBatch([]byte("{}\n"))
	if err == nil || !strings.Contains(err.Error(), "open denied") {
		t.Fatalf("want the open error surfaced from WriteBatch, got: %v", err)
	}
}

// TestWriteBatch_SurfacesRotationError proves a rotation failure triggered by a
// batch crossing the file-size boundary is surfaced rather than silently
// dropping the block.
func TestWriteBatch_SurfacesRotationError(t *testing.T) {
	orig := openSpool
	t.Cleanup(func() { openSpool = orig })
	openSpool = func(string) (spoolFile, int64, error) {
		return &fakeFile{closeErr: errors.New("close fail")}, 0, nil
	}
	// 1 MB max file; two ~0.6 MB batches make the second rotate the close-failing
	// handle.
	w, err := New(t.TempDir(), "inst", 1, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	block := make([]byte, 600*1024)
	block[len(block)-1] = '\n'
	if err := w.WriteBatch(block); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if err := w.WriteBatch(block); err == nil || !strings.Contains(err.Error(), "rotation") {
		t.Fatalf("want a rotation error on the second batch, got: %v", err)
	}
}

// TestClose_SurfacesFsyncError proves Close reports an fsync failure (so a
// caller knows the spool may not be durable) while still proceeding to clear the
// handle — a subsequent Close is then a clean no-op.
func TestClose_SurfacesFsyncError(t *testing.T) {
	orig := openSpool
	t.Cleanup(func() { openSpool = orig })
	openSpool = func(string) (spoolFile, int64, error) {
		return &fakeFile{syncErr: errors.New("fsync failed")}, 0, nil
	}
	w, err := New(t.TempDir(), "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Write([]byte(`{"id":"a"}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err = w.Close()
	if err == nil || !strings.Contains(err.Error(), "fsync on close") {
		t.Fatalf("want a wrapped fsync-on-close error, got: %v", err)
	}
	// The handle was cleared despite the sync error: a second Close is a no-op.
	if err := w.Close(); err != nil {
		t.Fatalf("second Close must be a no-op after fsync error, got: %v", err)
	}
}

// TestMaxSequence_ZeroWhenDirUnreadable proves a scan error during construction
// is non-fatal: the sequence falls back to 0 and the first opened file uses
// sequence 0001, matching the documented "scan error is non-fatal" behaviour.
func TestMaxSequence_ZeroWhenDirUnreadable(t *testing.T) {
	orig := readDir
	t.Cleanup(func() { readDir = orig })
	readDir = func(string) ([]os.DirEntry, error) {
		return nil, errors.New("ENOENT during scan")
	}
	w, err := New(t.TempDir(), "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w.sequence != 0 {
		t.Fatalf("scan error must leave sequence at 0, got %d", w.sequence)
	}

	// Restore the real readDir so the actual write + seal use the live filesystem,
	// then confirm the first file is sequence 0001 (sequence started from 0).
	readDir = orig
	if err := w.Write([]byte(`{"id":"a"}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	sealed, err := w.SealedFiles()
	if err != nil || len(sealed) != 1 {
		t.Fatalf("want 1 sealed file, got %v err=%v", sealed, err)
	}
	if seq, ok := parseSequence(filepath.Base(sealed[0])); !ok || seq != 1 {
		t.Fatalf("first file must be sequence 0001 after fallback, got %q (seq=%d ok=%v)", sealed[0], seq, ok)
	}
	_ = w.Close()
}

// TestMaxSequence_SkipsDirectories proves a directory entry whose name otherwise
// matches the spool pattern does not contribute to the max sequence — only real
// spool files advance the per-process sequence.
func TestMaxSequence_SkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	instDir := filepath.Join(dir, "inst")
	if err := os.MkdirAll(instDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory named like a high-sequence spool file: it must be ignored.
	if err := os.Mkdir(filepath.Join(instDir, "audit-20260101-9999.ndjson"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A real spool file with a lower sequence.
	if err := os.WriteFile(filepath.Join(instDir, "audit-20260101-0002.ndjson"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := maxSequence(instDir); got != 2 {
		t.Fatalf("maxSequence must ignore the directory and return 2 (the real file), got %d", got)
	}
}

// TestParseSequence_NoDashInStem covers the branch where the stem has no '-'
// separator at all, which cannot yield a sequence.
func TestParseSequence_NoDashInStem(t *testing.T) {
	// "audit" prefix + ".ndjson" suffix but the stem trimmed of ".ndjson" is
	// "auditfoo" with no '-' — LastIndexByte returns -1.
	if _, ok := parseSequence("auditfoo.ndjson"); ok {
		t.Fatal("a name with no '-' in the stem must not parse as a sequence")
	}
}

// TestDirSize_SkipsSubdirectories proves dirSize sums only files, ignoring a
// subdirectory in the instance spool dir (so the quota gate measures spool bytes,
// not directory inodes).
func TestDirSize_SkipsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	instDir := filepath.Join(dir, "inst")
	if err := os.MkdirAll(instDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// One real spool file of known size and one subdirectory.
	payload := []byte(`{"id":"x"}` + "\n")
	if err := os.WriteFile(filepath.Join(instDir, "audit-20260101-0001.ndjson"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(instDir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := New(dir, "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.mu.Lock()
	total, err := w.dirSize()
	w.mu.Unlock()
	if err != nil {
		t.Fatalf("dirSize: %v", err)
	}
	if total != int64(len(payload)) {
		t.Fatalf("dirSize must count only the spool file (%d bytes), got %d", len(payload), total)
	}
}
