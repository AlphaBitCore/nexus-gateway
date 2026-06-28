package ndjson

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRec writes one record and fails the test on error.
func writeRec(t *testing.T, w *Writer, s string) {
	t.Helper()
	if err := w.Write([]byte(s)); err != nil {
		t.Fatalf("write %q: %v", s, err)
	}
}

func TestWriter_SealedFiles_ExcludesActiveUntilRotated(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close() //nolint:errcheck

	writeRec(t, w, `{"id":"a"}`)
	// The file being appended must NOT appear as sealed.
	if sealed, err := w.SealedFiles(); err != nil || len(sealed) != 0 {
		t.Fatalf("active file must be excluded: sealed=%v err=%v", sealed, err)
	}
	// After Rotate the file is sealed and listed.
	if err := w.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	sealed, err := w.SealedFiles()
	if err != nil {
		t.Fatalf("SealedFiles: %v", err)
	}
	if len(sealed) != 1 {
		t.Fatalf("want 1 sealed file after rotate, got %v", sealed)
	}
	if !strings.HasSuffix(sealed[0], ".ndjson") {
		t.Fatalf("unexpected sealed name: %s", sealed[0])
	}
	// Its content survived the rotate (fsync+close).
	b, err := os.ReadFile(sealed[0])
	if err != nil || !strings.Contains(string(b), `"id":"a"`) {
		t.Fatalf("sealed content missing: %q err=%v", b, err)
	}
}

func TestWriter_Rotate_NoOpWhenNothingOpen(t *testing.T) {
	w, err := New(t.TempDir(), "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Rotate(); err != nil { // no file open yet
		t.Fatalf("Rotate no-op should be nil, got %v", err)
	}
	if sealed, _ := w.SealedFiles(); len(sealed) != 0 {
		t.Fatalf("no files expected, got %v", sealed)
	}
}

func TestWriter_Rotate_NewWriteOpensFreshFile(t *testing.T) {
	w, err := New(t.TempDir(), "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close() //nolint:errcheck

	writeRec(t, w, `{"id":"a"}`)
	if err := w.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	writeRec(t, w, `{"id":"b"}`) // opens a second file
	if err := w.Rotate(); err != nil {
		t.Fatalf("Rotate2: %v", err)
	}
	sealed, _ := w.SealedFiles()
	if len(sealed) != 2 {
		t.Fatalf("want 2 distinct sealed files, got %v", sealed)
	}
	if sealed[0] == sealed[1] {
		t.Fatalf("rotated files must be distinct: %v", sealed)
	}
}

func TestWriter_SealedFiles_SortedChronological(t *testing.T) {
	dir := t.TempDir()
	instDir := filepath.Join(dir, "inst")
	if err := os.MkdirAll(instDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-create out-of-order names; SealedFiles must return them sorted ascending
	// (date+sequence is chronological).
	for _, n := range []string{"audit-20260101-0002.ndjson", "audit-20260101-0001.ndjson", "audit-20251231-0009.ndjson"} {
		if err := os.WriteFile(filepath.Join(instDir, n), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := New(dir, "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, err := w.SealedFiles()
	if err != nil {
		t.Fatalf("SealedFiles: %v", err)
	}
	want := []string{"audit-20251231-0009.ndjson", "audit-20260101-0001.ndjson", "audit-20260101-0002.ndjson"}
	for i, p := range sealed {
		if filepath.Base(p) != want[i] {
			t.Fatalf("not chronological: got %v want %v", sealed, want)
		}
	}
}

func TestWriter_SealedFiles_IgnoresNonSpoolFiles(t *testing.T) {
	dir := t.TempDir()
	instDir := filepath.Join(dir, "inst")
	if err := os.MkdirAll(instDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"audit-20260101-0001.ndjson", "README.txt", "audit-bad.log", "subdir"} {
		p := filepath.Join(instDir, n)
		if n == "subdir" {
			_ = os.Mkdir(p, 0o700)
			continue
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w, _ := New(dir, "inst", 10, 100, nil)
	sealed, err := w.SealedFiles()
	if err != nil {
		t.Fatalf("SealedFiles: %v", err)
	}
	if len(sealed) != 1 || filepath.Base(sealed[0]) != "audit-20260101-0001.ndjson" {
		t.Fatalf("only the .ndjson spool file should be listed, got %v", sealed)
	}
}

// TestWriter_MaxSequence_ContinuesPastLeftovers proves a restart does NOT reuse a
// leftover file's sequence number — the cross-restart write/delete race the
// recovery sweeper would otherwise hit.
func TestWriter_MaxSequence_ContinuesPastLeftovers(t *testing.T) {
	dir := t.TempDir()
	instDir := filepath.Join(dir, "inst")
	if err := os.MkdirAll(instDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A leftover from a prior run (date-agnostic: parseSequence ignores the date).
	if err := os.WriteFile(filepath.Join(instDir, "audit-20200101-0003.ndjson"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := New(dir, "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close() //nolint:errcheck

	writeRec(t, w, `{"id":"new"}`)
	if err := w.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	// The new file must be sequence >= 4, never reusing -0003.
	var newFile string
	sealed, _ := w.SealedFiles()
	for _, p := range sealed {
		if !strings.Contains(p, "20200101-0003") {
			newFile = filepath.Base(p)
		}
	}
	if newFile == "" {
		t.Fatalf("new file not found among %v", sealed)
	}
	if seq, ok := parseSequence(newFile); !ok || seq <= 3 {
		t.Fatalf("new file %q must have sequence > 3 (continue past leftover), got seq=%d ok=%v", newFile, seq, ok)
	}
	// The leftover is untouched and still distinct.
	if _, err := os.Stat(filepath.Join(instDir, "audit-20200101-0003.ndjson")); err != nil {
		t.Fatalf("leftover must remain distinct/untouched: %v", err)
	}
}

func TestParseSequence(t *testing.T) {
	cases := []struct {
		name string
		want int
		ok   bool
	}{
		{"audit-20260101-0007.ndjson", 7, true},
		{"audit-20260101-0000.ndjson", 0, true},
		{"audit-19991231-1234.ndjson", 1234, true},
		{"README.txt", 0, false},
		// No explicit NNNN segment: the numeric date tail parses as the sequence.
		// Harmless — maxSequence only over-counts, never reuses a real name.
		{"audit-20260101.ndjson", 20260101, true},
		{"audit-20260101-xx.ndjson", 0, false},
		{"audit-20260101-0007.log", 0, false},
		{"prefix-audit-20260101-0007.ndjson", 0, false},
	}
	for _, c := range cases {
		got, ok := parseSequence(c.name)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseSequence(%q)=(%d,%v) want (%d,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestWriter_Rotate_SurfacesFsyncError verifies a sync failure during rotation is
// surfaced (so a caller knows the seal is not durable) and does not silently seal.
func TestWriter_Rotate_SurfacesFsyncError(t *testing.T) {
	orig := openSpool
	t.Cleanup(func() { openSpool = orig })
	openSpool = func(string) (spoolFile, int64, error) {
		return &fakeFile{syncErr: errors.New("fsync failed")}, 0, nil
	}
	w, err := New(t.TempDir(), "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeRec(t, w, `{"id":"a"}`)
	if err := w.Rotate(); err == nil || !strings.Contains(err.Error(), "fsync") {
		t.Fatalf("want wrapped fsync error from Rotate, got %v", err)
	}
}

// TestWriter_SealedFiles_ErrorOnMissingDir surfaces a list error rather than
// silently returning an empty set (the sweeper logs + retries).
func TestWriter_SealedFiles_ErrorOnMissingDir(t *testing.T) {
	origRD := readDir
	t.Cleanup(func() { readDir = origRD })
	w, err := New(t.TempDir(), "inst", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	readDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("ENOENT") }
	if _, err := w.SealedFiles(); err == nil {
		t.Fatal("want error when dir cannot be listed")
	}
}
