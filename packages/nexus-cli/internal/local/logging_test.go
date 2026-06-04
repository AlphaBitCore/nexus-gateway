package local

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local/paths"
)

// TestSlogLevel_Mapping asserts each accepted string maps to its level and that
// empty / unknown default to Debug (the operator wants full transport timings
// out of the box).
func TestSlogLevel_Mapping(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"", slog.LevelDebug},
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"  info ", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"nonsense", slog.LevelDebug},
	}
	for _, tc := range cases {
		c := &Config{LogLevel: tc.in}
		if got := c.SlogLevel(); got != tc.want {
			t.Errorf("SlogLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestOpenLogger_CreatesDirAndFile verifies OpenLogger creates the (missing) log
// directory + file and that a logged line actually lands in the file (not on
// stdout/stderr).
func TestOpenLogger_CreatesDirAndFile(t *testing.T) {
	root := t.TempDir()
	logDir := filepath.Join(root, "nested", "logs")
	logFile := filepath.Join(logDir, "nexus-cli.log")
	p := paths.Paths{LogDir: logDir, LogFile: logFile}

	logger, closer, err := OpenLogger(p, slog.LevelDebug)
	if err != nil {
		t.Fatalf("OpenLogger: %v", err)
	}
	logger.Info("hello", "k", "v")
	if err := closer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The directory was created with the missing intermediate segment.
	if fi, err := os.Stat(logDir); err != nil || !fi.IsDir() {
		t.Fatalf("log dir not created: err=%v", err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !bytes.Contains(data, []byte("hello")) {
		t.Errorf("log file missing the written line; got %q", data)
	}
}

// TestOpenLogger_Appends asserts a second OpenLogger on the same path keeps the
// prior content (O_APPEND, not O_TRUNC) so a session does not erase the last.
func TestOpenLogger_Appends(t *testing.T) {
	root := t.TempDir()
	p := paths.Paths{LogDir: root, LogFile: filepath.Join(root, "nexus-cli.log")}

	l1, c1, err := OpenLogger(p, slog.LevelDebug)
	if err != nil {
		t.Fatalf("OpenLogger #1: %v", err)
	}
	l1.Info("first")
	_ = c1.Close()

	l2, c2, err := OpenLogger(p, slog.LevelDebug)
	if err != nil {
		t.Fatalf("OpenLogger #2: %v", err)
	}
	l2.Info("second")
	_ = c2.Close()

	data, err := os.ReadFile(p.LogFile)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Contains(data, []byte("first")) || !bytes.Contains(data, []byte("second")) {
		t.Errorf("append lost a line; got %q", data)
	}
}

// TestOpenLogger_LevelFiltersBelow asserts a Warn-level logger drops Info lines —
// the configured level is honoured by the handler.
func TestOpenLogger_LevelFiltersBelow(t *testing.T) {
	root := t.TempDir()
	p := paths.Paths{LogDir: root, LogFile: filepath.Join(root, "nexus-cli.log")}

	logger, closer, err := OpenLogger(p, slog.LevelWarn)
	if err != nil {
		t.Fatalf("OpenLogger: %v", err)
	}
	logger.Info("below-threshold")
	logger.Warn("at-threshold")
	_ = closer.Close()

	data, _ := os.ReadFile(p.LogFile)
	if bytes.Contains(data, []byte("below-threshold")) {
		t.Errorf("Info line written under Warn level; got %q", data)
	}
	if !bytes.Contains(data, []byte("at-threshold")) {
		t.Errorf("Warn line missing under Warn level; got %q", data)
	}
}

// TestOpenLogger_RotatesWhenLarge writes a file larger than maxLogBytes, then
// opens the logger and asserts the oversized file was rotated to ".1" and a
// fresh, smaller active file was started.
func TestOpenLogger_RotatesWhenLarge(t *testing.T) {
	root := t.TempDir()
	logFile := filepath.Join(root, "nexus-cli.log")
	p := paths.Paths{LogDir: root, LogFile: logFile}

	// Seed an oversized existing log (> 16 MiB).
	big := bytes.Repeat([]byte("x"), maxLogBytes+1024)
	if err := os.WriteFile(logFile, big, 0o600); err != nil {
		t.Fatalf("seed big log: %v", err)
	}

	logger, closer, err := OpenLogger(p, slog.LevelDebug)
	if err != nil {
		t.Fatalf("OpenLogger: %v", err)
	}
	logger.Info("fresh-generation")
	_ = closer.Close()

	// The oversized content moved to .1.
	rotated, err := os.Stat(logFile + ".1")
	if err != nil {
		t.Fatalf(".1 backup not created: %v", err)
	}
	if rotated.Size() != int64(len(big)) {
		t.Errorf(".1 size = %d, want %d (the seeded oversized content)", rotated.Size(), len(big))
	}

	// The active file is fresh: it holds only the new line, not the seeded bulk.
	active, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if int64(len(active)) >= int64(len(big)) {
		t.Errorf("active file not rotated fresh: size %d >= seeded %d", len(active), len(big))
	}
	if !bytes.Contains(active, []byte("fresh-generation")) {
		t.Errorf("active file missing the post-rotation line; got %d bytes", len(active))
	}
}

// TestRotateIfLarge_SmallFileNoOp asserts a sub-threshold file is left alone (no
// spurious .1 backup, content preserved).
func TestRotateIfLarge_SmallFileNoOp(t *testing.T) {
	root := t.TempDir()
	logFile := filepath.Join(root, "nexus-cli.log")
	if err := os.WriteFile(logFile, []byte("small"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rotateIfLarge(logFile)
	if _, err := os.Stat(logFile + ".1"); !os.IsNotExist(err) {
		t.Errorf("small file was rotated; .1 should not exist (err=%v)", err)
	}
	data, _ := os.ReadFile(logFile)
	if string(data) != "small" {
		t.Errorf("small file content changed: %q", data)
	}
}

// TestRotateIfLarge_MissingFileNoOp asserts a missing file is a no-op (rotation
// must never block the first-ever log open).
func TestRotateIfLarge_MissingFileNoOp(t *testing.T) {
	root := t.TempDir()
	rotateIfLarge(filepath.Join(root, "does-not-exist.log"))
	// No panic, no created backup.
	if _, err := os.Stat(filepath.Join(root, "does-not-exist.log.1")); !os.IsNotExist(err) {
		t.Errorf("rotation created a backup for a missing source")
	}
}

// TestOpenLogger_MkdirError surfaces the error when the log dir cannot be
// created (a file occupies the dir path), proving the failure is reported, not
// swallowed.
func TestOpenLogger_MkdirError(t *testing.T) {
	root := t.TempDir()
	// Make LogDir a path whose parent is a regular file → MkdirAll fails.
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	logDir := filepath.Join(blocker, "logs")
	p := paths.Paths{LogDir: logDir, LogFile: filepath.Join(logDir, "nexus-cli.log")}

	if _, _, err := OpenLogger(p, slog.LevelDebug); err == nil {
		t.Fatal("expected MkdirAll error, got nil")
	}
}

// TestOpenLogger_OpenFileError surfaces the open error when LogFile's path is
// itself an existing directory (MkdirAll succeeds for LogDir, but O_WRONLY on a
// directory fails), proving the open failure is reported.
func TestOpenLogger_OpenFileError(t *testing.T) {
	root := t.TempDir()
	// LogFile points at a directory we create, so OpenFile(O_WRONLY) on it fails.
	logFileAsDir := filepath.Join(root, "nexus-cli.log")
	if err := os.MkdirAll(logFileAsDir, 0o700); err != nil {
		t.Fatalf("seed dir-at-logfile: %v", err)
	}
	p := paths.Paths{LogDir: root, LogFile: logFileAsDir}

	if _, _, err := OpenLogger(p, slog.LevelDebug); err == nil {
		t.Fatal("expected OpenFile error when LogFile is a directory, got nil")
	}
}
