package spool_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/client/spool"
)

type item struct{ V string }

func newSpool(t *testing.T, max int64) *spool.Spool[item] {
	t.Helper()
	dir := t.TempDir()
	s, err := spool.New[item](dir, "alerts", max, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestEnqueueDrainSuccess(t *testing.T) {
	s := newSpool(t, 1<<20)

	for _, v := range []string{"a", "b", "c"} {
		if err := s.Enqueue(item{V: v}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if got := s.PendingCount(); got != 3 {
		t.Fatalf("PendingCount=%d, want 3", got)
	}

	var seen []string
	drained, err := s.Drain(context.Background(), func(i item) error {
		seen = append(seen, i.V)
		return nil
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if drained != 3 || len(seen) != 3 {
		t.Fatalf("drained=%d seen=%v", drained, seen)
	}
	if s.PendingCount() != 0 {
		t.Fatalf("expected 0 pending after drain, got %d", s.PendingCount())
	}
}

func TestDrainStopsOnSendError(t *testing.T) {
	s := newSpool(t, 1<<20)
	for _, v := range []string{"a", "b", "c"} {
		_ = s.Enqueue(item{V: v})
	}
	boom := errors.New("boom")
	drained, err := s.Drain(context.Background(), func(i item) error {
		if i.V == "b" {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want boom", err)
	}
	if drained != 1 {
		t.Fatalf("drained=%d, want 1 (only 'a' succeeded before boom)", drained)
	}
	if s.PendingCount() != 2 {
		t.Fatalf("pending=%d, want 2 (b+c remain)", s.PendingCount())
	}
}

func TestCrashSafeRecovery(t *testing.T) {
	dir := t.TempDir()
	s1, _ := spool.New[item](dir, "alerts", 1<<20, slog.Default())
	_ = s1.Enqueue(item{V: "x"})
	_ = s1.Enqueue(item{V: "y"})

	// Simulate process restart: new Spool from same dir.
	s2, err := spool.New[item](dir, "alerts", 1<<20, slog.Default())
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}
	if s2.PendingCount() != 2 {
		t.Fatalf("pending after restart=%d, want 2", s2.PendingCount())
	}
}

func TestCapEvictsOldest(t *testing.T) {
	// max small enough to force eviction. Each envelope ~40 bytes; set max = 100.
	s := newSpool(t, 100)
	// Enqueue items of known approximate size until the cap triggers.
	for range 50 {
		_ = s.Enqueue(item{V: "padding-padding"})
	}
	// Whatever we ended with must respect the cap (roughly).
	if s.PendingCount() >= 50 {
		t.Fatalf("expected eviction, pending=%d", s.PendingCount())
	}
}

func TestCorruptEnvelopeSkipped(t *testing.T) {
	dir := t.TempDir()
	s, _ := spool.New[item](dir, "alerts", 1<<20, slog.Default())
	_ = s.Enqueue(item{V: "ok1"})
	_ = s.Enqueue(item{V: "ok2"})

	// Corrupt one envelope file on disk.
	entries, _ := os.ReadDir(filepath.Join(dir, "alerts"))
	if len(entries) < 2 {
		t.Fatalf("expected >=2 files on disk, got %d", len(entries))
	}
	_ = os.WriteFile(filepath.Join(dir, "alerts", entries[0].Name()), []byte("{not json"), 0o640)

	s2, _ := spool.New[item](dir, "alerts", 1<<20, slog.Default())
	var seen []string
	drained, err := s2.Drain(context.Background(), func(i item) error {
		seen = append(seen, i.V)
		return nil
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if drained != 1 {
		t.Fatalf("drained=%d, want 1 (corrupt one skipped)", drained)
	}
	if s2.Skipped() != 1 {
		t.Errorf("Skipped()=%d, want 1", s2.Skipped())
	}
}

// TestNew_EmptyNameReturnsError covers the `name == ""` guard in
// spool.New — without it, mkdir would create the bare directory and
// silently collide with other spools sharing the same parent.
func TestNew_EmptyNameReturnsError(t *testing.T) {
	_, err := spool.New[item](t.TempDir(), "", 1<<20, slog.Default())
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

// TestNew_MkdirFailure covers the MkdirAll error branch in spool.New
// when the parent path cannot be created (here, an existing regular
// file at the target location).
func TestNew_MkdirFailure(t *testing.T) {
	dir := t.TempDir()
	// Plant a regular file at dir/blocker; spool.New will try mkdir
	// dir/blocker/alerts which must fail because blocker is a file.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := spool.New[item](blocker, "alerts", 1<<20, slog.Default())
	if err == nil {
		t.Fatal("expected mkdir error when parent is a regular file")
	}
}

// TestEnqueue_MarshalError covers the json.Marshal error inside
// Enqueue. Channels are not JSON-marshalable.
func TestEnqueue_MarshalError(t *testing.T) {
	// We need a payload type with a non-marshalable field. The generic
	// here is map[string]any so we can stuff a channel into it.
	dir := t.TempDir()
	s, err := spool.New[map[string]any](dir, "alerts", 1<<20, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = s.Enqueue(map[string]any{"bad": make(chan int)})
	if err == nil {
		t.Fatal("expected marshal error on unmarshalable payload")
	}
	if !strings.Contains(err.Error(), "marshal") {
		t.Errorf("expected 'marshal' in error, got %v", err)
	}
}

// TestEnqueue_WriteFileError covers the os.WriteFile error branch in
// Enqueue by making the spool directory read-only after construction.
func TestEnqueue_WriteFileError(t *testing.T) {
	dir := t.TempDir()
	s, err := spool.New[item](dir, "alerts", 1<<20, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spoolDir := filepath.Join(dir, "alerts")
	if err := os.Chmod(spoolDir, 0o500); err != nil {
		t.Fatalf("Chmod ro: %v", err)
	}
	defer func() { _ = os.Chmod(spoolDir, 0o750) }()

	err = s.Enqueue(item{V: "x"})
	if err == nil {
		t.Fatal("expected write error on read-only spool dir")
	}
}

// TestPendingCount_ReaddirError covers the listFiles-error branch in
// PendingCount (returns 0 silently). Make the spool dir unreadable.
func TestPendingCount_ReaddirError(t *testing.T) {
	dir := t.TempDir()
	s, _ := spool.New[item](dir, "alerts", 1<<20, slog.Default())
	_ = s.Enqueue(item{V: "x"})
	spoolDir := filepath.Join(dir, "alerts")
	// Remove read permission so ReadDir fails.
	if err := os.Chmod(spoolDir, 0o000); err != nil {
		t.Fatalf("Chmod 000: %v", err)
	}
	defer func() { _ = os.Chmod(spoolDir, 0o750) }()
	if got := s.PendingCount(); got != 0 {
		t.Errorf("PendingCount() on unreadable dir = %d, want 0", got)
	}
}

// TestDrain_ContextCancelled covers the ctx.Done branch inside Drain:
// when the context is already cancelled, Drain must stop after at
// most zero envelopes have been processed.
func TestDrain_ContextCancelled(t *testing.T) {
	s := newSpool(t, 1<<20)
	for _, v := range []string{"a", "b", "c"} {
		_ = s.Enqueue(item{V: v})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	drained, err := s.Drain(ctx, func(_ item) error {
		t.Fatal("send must NOT be called when ctx is already cancelled")
		return nil
	})
	if err != nil {
		t.Errorf("Drain on cancelled ctx returned err=%v, want nil", err)
	}
	if drained != 0 {
		t.Errorf("drained=%d on cancelled ctx, want 0", drained)
	}
	// Files must still be on disk — drain didn't consume any.
	if s.PendingCount() != 3 {
		t.Errorf("pending=%d after cancelled drain, want 3", s.PendingCount())
	}
}

// TestDrain_ListFilesError covers the listFiles error branch at the
// top of Drain (mu held, dir gone).
func TestDrain_ListFilesError(t *testing.T) {
	dir := t.TempDir()
	s, _ := spool.New[item](dir, "alerts", 1<<20, slog.Default())
	// Remove the spool directory entirely so ReadDir fails.
	if err := os.RemoveAll(filepath.Join(dir, "alerts")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Drain(context.Background(), func(_ item) error { return nil })
	if err == nil {
		t.Fatal("expected listFiles error when spool dir is missing")
	}
}

// TestListFiles_NonJSONFilesIgnored covers the `filepath.Ext(n) !=
// ".json"` branch in listFiles by planting a non-json file alongside
// the queue.
func TestListFiles_NonJSONFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	s, _ := spool.New[item](dir, "alerts", 1<<20, slog.Default())
	_ = s.Enqueue(item{V: "real"})

	// Plant a stray non-json file + a stray directory inside the spool.
	if err := os.WriteFile(filepath.Join(dir, "alerts", "stray.txt"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "alerts", "subdir"), 0o750); err != nil {
		t.Fatal(err)
	}

	// PendingCount must ignore both — only the 1 .json file counts.
	if got := s.PendingCount(); got != 1 {
		t.Errorf("PendingCount=%d, want 1 (stray files filtered)", got)
	}
}

// TestEnforceCap_NoCap covers the `maxBytes <= 0` early-return in
// enforceCap: when constructed with maxBytes=0 (unlimited), enqueue
// must never drop entries regardless of total size.
func TestEnforceCap_NoCap(t *testing.T) {
	s := newSpool(t, 0) // unlimited
	for range 20 {
		_ = s.Enqueue(item{V: "padding-padding"})
	}
	if got := s.PendingCount(); got != 20 {
		t.Errorf("PendingCount=%d with no cap, want 20", got)
	}
	if got := s.Dropped(); got != 0 {
		t.Errorf("Dropped=%d with no cap, want 0", got)
	}
}

// TestDrain_OsRemoveAfterSendError covers the
// `os.Remove → return drained, err` branch inside Drain. We plant a
// non-removable on-disk envelope (by replacing the spool dir's permissions
// with read-only AFTER the file is read but BEFORE Remove). The
// cleanest way is to nest the file in a directory we revoke write
// permission from after listFiles is taken.
func TestDrain_RemoveAfterSendError(t *testing.T) {
	dir := t.TempDir()
	s, _ := spool.New[item](dir, "alerts", 1<<20, slog.Default())
	_ = s.Enqueue(item{V: "x"})
	spoolDir := filepath.Join(dir, "alerts")

	// Drain calls send first then Remove; revoke write on the directory
	// so Remove fails after a successful send callback.
	if err := os.Chmod(spoolDir, 0o500); err != nil {
		t.Fatalf("Chmod ro: %v", err)
	}
	defer func() { _ = os.Chmod(spoolDir, 0o750) }()

	_, err := s.Drain(context.Background(), func(_ item) error { return nil })
	if err == nil {
		t.Fatal("expected Remove-after-send error when spool dir is read-only")
	}
	if !strings.Contains(err.Error(), "remove") {
		t.Errorf("expected 'remove' in error, got %v", err)
	}
}

// TestDroppedSkippedGetters covers the trivial getters Dropped() and
// Skipped() observably: Dropped reflects cap-eviction count.
func TestDroppedSkippedGetters(t *testing.T) {
	s := newSpool(t, 100) // small cap to force eviction
	for range 50 {
		_ = s.Enqueue(item{V: "padding-padding"})
	}
	if got := s.Dropped(); got == 0 {
		t.Errorf("Dropped()=0 after forced eviction; expected >0")
	}
	if got := s.Skipped(); got != 0 {
		t.Errorf("Skipped()=%d before any corrupt envelope, want 0", got)
	}
}
