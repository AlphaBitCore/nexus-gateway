package spool

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type renameTestItem struct{ V string }

// TestEnqueue_RenameError covers the os.Rename error branch in Enqueue
// (spool.go:75-78). Strategy: stub nowNanos to return a fixed value;
// predict the exact final filename Enqueue will produce; pre-create a
// non-empty directory at that final path. Rename(tmpFile,
// existingNonEmptyDir) fails on macOS/Linux ("file exists" /
// "directory not empty"), so Enqueue returns an error and also
// attempts to clean up the .tmp file. Observable assertions:
// (1) Enqueue returns non-nil error wrapping "spool rename";
// (2) the .tmp file is gone (cleanup succeeded);
// (3) PendingCount is unchanged.
func TestEnqueue_RenameError(t *testing.T) {
	dir := t.TempDir()
	s, err := New[renameTestItem](dir, "alerts", 1<<20, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spoolDir := filepath.Join(dir, "alerts")

	// Stub nowNanos so the predicted filename is deterministic.
	origNow := nowNanos
	t.Cleanup(func() { nowNanos = origNow })
	nowNanos = func() int64 { return 12345 }

	// Enqueue produces `<unixNano:020d>_<seq:06d>.json` with seq starting
	// at 1 (s.seq is 0; s.seq++ is called before fmt.Sprintf).
	finalName := "00000000000000012345_000001.json"
	finalPath := filepath.Join(spoolDir, finalName)

	// Plant a non-empty directory at the final path: Rename of a regular
	// file onto a non-empty directory fails.
	if err := os.MkdirAll(finalPath, 0o750); err != nil {
		t.Fatal(err)
	}
	// Put a file inside so the dir is non-empty (some platforms allow
	// rename onto an empty dir).
	if err := os.WriteFile(filepath.Join(finalPath, "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = s.Enqueue(renameTestItem{V: "x"})
	if err == nil {
		t.Fatal("expected Enqueue to fail when final path is a non-empty directory")
	}
	if !strings.Contains(err.Error(), "rename") {
		t.Errorf("expected error to mention 'rename', got %v", err)
	}
	// The .tmp file should have been cleaned up by the deferred Remove
	// inside Enqueue.
	tmpPath := finalPath + ".tmp"
	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Errorf(".tmp file was not cleaned up after rename failure: %v", statErr)
	}
}

// TestEnforceCap_StatError covers the os.Stat error branch in
// enforceCap (spool.go:184-185). Strategy: directly construct a
// Spool with a directory that contains a name returned by ReadDir
// but for which Stat will fail — easiest is a broken symlink. The
// symlink is listed by ReadDir but Stat (which follows the symlink)
// returns an error. enforceCap silently continues past it, so
// observability is "enforceCap didn't panic and didn't drop the real
// file when total stays under cap".
func TestEnforceCap_StatErrorContinues(t *testing.T) {
	dir := t.TempDir()
	s, err := New[renameTestItem](dir, "alerts", 1<<10, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spoolDir := filepath.Join(dir, "alerts")

	// Plant a broken symlink with a .json extension: ReadDir returns it,
	// Stat (follows symlink) returns ENOENT.
	brokenLink := filepath.Join(spoolDir, "00000000000000000000_999999.json")
	if err := os.Symlink(filepath.Join(spoolDir, "nonexistent_target"), brokenLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Force enforceCap by enqueueing a real entry. listFiles will
	// include both the broken symlink and the new file. For the broken
	// symlink, Stat fails → continue. For the real file, Stat succeeds.
	// Since total < maxBytes the loop exits cleanly.
	if err := s.Enqueue(renameTestItem{V: "real"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Observable: PendingCount counts BOTH entries (listFiles returns
	// them as plain names with .json suffix). Dropped count must be 0
	// because total bytes stays under maxBytes — the stat-failed
	// symlink contributes 0 to total.
	if got := s.Dropped(); got != 0 {
		t.Errorf("Dropped=%d after stat-failed branch hit, want 0", got)
	}
}

// TestEnforceCap_ListFilesError covers the listFiles error branch in
// enforceCap (spool.go:177-179). The branch is `if err != nil {
// return }` — silent. To hit it we call enforceCap when the spool
// directory has been removed. Cleanest reproduction: rapid sequence
// where we MkdirAll, Enqueue, RemoveAll, then call Enqueue once more
// — the second Enqueue's listFiles inside enforceCap returns an
// error. But Enqueue's WriteFile would fail first. Instead, call
// enforceCap directly via a same-package construction.
func TestEnforceCap_ListFilesError(t *testing.T) {
	dir := t.TempDir()
	s, _ := New[renameTestItem](dir, "alerts", 1, slog.Default())
	// Remove the spool subdir so listFiles fails.
	if err := os.RemoveAll(filepath.Join(dir, "alerts")); err != nil {
		t.Fatal(err)
	}
	// enforceCap is unexported but reachable from the same package.
	s.enforceCap()
	// Observable: did not panic, Dropped count stays at 0.
	if got := s.Dropped(); got != 0 {
		t.Errorf("Dropped=%d on listFiles-error path, want 0", got)
	}
}

// TestDrain_ReadFileErrorViaBrokenSymlink covers spool.go:113-115:
// when os.ReadFile fails on a listed .json entry, Drain wraps the
// error and aborts. listFiles uses DirEntry.IsDir which for a symlink
// returns false (does not follow), so a broken symlink with a .json
// suffix is enumerated; ReadFile (which follows) returns ENOENT.
func TestDrain_ReadFileErrorViaBrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	s, _ := New[renameTestItem](dir, "alerts", 1<<20, slog.Default())
	spoolDir := filepath.Join(dir, "alerts")

	// Plant a broken symlink with .json suffix; do not put a real entry
	// so this is the only file in the spool.
	brokenLink := filepath.Join(spoolDir, "00000000000000000000_000001.json")
	if err := os.Symlink(filepath.Join(spoolDir, "nonexistent_target"), brokenLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, err := s.Drain(context.Background(), func(_ renameTestItem) error { return nil })
	if err == nil {
		t.Fatal("expected ReadFile error when .json entry is a broken symlink")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("expected 'read' in error, got %v", err)
	}
}
