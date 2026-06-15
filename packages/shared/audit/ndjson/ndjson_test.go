package ndjson

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readSpoolLines returns every non-empty line across all spool files in the
// instance directory, in file+line order.
func readSpoolLines(t *testing.T, dir, instanceID string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, instanceID))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
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

func TestWriter_WritesOneLinePerRecordAndCountsBytes(t *testing.T) {
	dir := t.TempDir()
	var wroteBytes int
	w, err := New(dir, "inst-1", 10, 100, func(n int) { wroteBytes += n })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	records := [][]byte{[]byte(`{"id":"a"}`), []byte(`{"id":"b"}`)}
	for _, r := range records {
		if err := w.Write(r); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	lines := readSpoolLines(t, dir, "inst-1")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(lines), lines)
	}
	if lines[0] != `{"id":"a"}` || lines[1] != `{"id":"b"}` {
		t.Fatalf("unexpected spool content: %v", lines)
	}
	// onWrite must see the byte count INCLUDING the appended newline.
	wantBytes := len(records[0]) + 1 + len(records[1]) + 1
	if wroteBytes != wantBytes {
		t.Fatalf("onWrite total = %d, want %d (record bytes + newlines)", wroteBytes, wantBytes)
	}
}

func TestWriter_RotatesWhenFileExceedsMaxSize(t *testing.T) {
	dir := t.TempDir()
	// 1 MB max file; each record is ~0.5 MB, so the third record forces a
	// rotation into a second file.
	w, err := New(dir, "inst-rotate", 1, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	half := make([]byte, 512*1024)
	for i := range half {
		half[i] = 'x'
	}
	for range 3 {
		if err := w.Write(half); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(dir, "inst-rotate"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected >=2 spool files after rotation, got %d", len(entries))
	}
}

func TestWriter_RefusesWriteWhenTotalQuotaExceeded(t *testing.T) {
	dir := t.TempDir()
	// 1 MB total quota: the first ~1.1 MB record lands (the quota gate is a
	// pre-write check, so a single oversized record is allowed through), and
	// the second is refused because the directory now exceeds the quota.
	w, err := New(dir, "inst-quota", 1, 1, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	big := make([]byte, 1100*1024)
	for i := range big {
		big[i] = 'y'
	}
	if err := w.Write(big); err != nil {
		t.Fatalf("first write should succeed: %v", err)
	}
	err = w.Write(big)
	if err == nil {
		t.Fatal("second write must be refused once the spool exceeds its quota")
	}
	if !strings.Contains(err.Error(), "exceeds quota") {
		t.Fatalf("quota error missing expected reason: %v", err)
	}
}

func TestWriter_InstanceDirectoriesAreIsolated(t *testing.T) {
	dir := t.TempDir()
	wA, err := New(dir, "instance-a", 10, 100, nil)
	if err != nil {
		t.Fatalf("New a: %v", err)
	}
	t.Cleanup(func() { _ = wA.Close() })
	wB, err := New(dir, "instance-b", 10, 100, nil)
	if err != nil {
		t.Fatalf("New b: %v", err)
	}
	t.Cleanup(func() { _ = wB.Close() })

	if err := wA.Write([]byte(`{"who":"a"}`)); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := wB.Write([]byte(`{"who":"b"}`)); err != nil {
		t.Fatalf("write b: %v", err)
	}

	if got := readSpoolLines(t, dir, "instance-a"); len(got) != 1 || got[0] != `{"who":"a"}` {
		t.Fatalf("instance-a spool = %v, want one a-line", got)
	}
	if got := readSpoolLines(t, dir, "instance-b"); len(got) != 1 || got[0] != `{"who":"b"}` {
		t.Fatalf("instance-b spool = %v, want one b-line", got)
	}
}

func TestNew_FailsWhenSpoolRootNotCreatable(t *testing.T) {
	// A regular file as the spool root makes MkdirAll fail — surfaced as a
	// construction error rather than a silent no-op.
	root := filepath.Join(t.TempDir(), "rootfile")
	if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := New(root, "inst", 10, 100, nil); err == nil {
		t.Fatal("New must fail when the spool directory cannot be created")
	}
}

func TestWriter_WriteFailsWhenSpoolFileNotCreatable(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "inst-ro", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// Drop write permission on the instance dir so the quota stat still
	// succeeds (dir is readable) but O_CREATE in openNewFile fails — exercises
	// the open-file error path.
	instDir := filepath.Join(dir, "inst-ro")
	if err := os.Chmod(instDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(instDir, 0o700) })

	if err := w.Write([]byte(`{"x":1}`)); err == nil {
		t.Fatal("Write must fail when the spool file cannot be created")
	} else if !strings.Contains(err.Error(), "open file") {
		t.Fatalf("expected open-file error, got: %v", err)
	}
}

func TestWriter_WriteSurfacesErrorWhenSpoolDirRemoved(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "inst-gone", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// Remove the instance dir: the quota stat's ReadDir now errors (Write
	// falls through that gate rather than losing data over a stat failure),
	// then openNewFile fails because the directory is gone.
	if err := os.RemoveAll(filepath.Join(dir, "inst-gone")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := w.Write([]byte(`{"x":1}`)); err == nil {
		t.Fatal("Write must surface an error when the spool directory is gone")
	}
}

func TestWriter_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "inst-close", 10, 100, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Write([]byte(`{"id":"x"}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got: %v", err)
	}
}
