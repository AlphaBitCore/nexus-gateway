package local

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local/paths"
)

// maxLogBytes is the size at which OpenLogger rotates the active log to
// LogFile+".1" before opening a fresh file. 16 MiB is large enough to hold many
// sessions of per-request transport timings yet bounded so an unattended CLI
// can never fill a user's disk. One generation only — the .1 backup is the
// previous run's tail, which is all a hang post-mortem needs.
const maxLogBytes = 16 << 20 // 16 MiB

// OpenLogger opens the CLI's diagnostic file logger at p.LogFile, creating
// p.LogDir (0700) and the file (0600) if missing, and rotating the file to
// p.LogFile+".1" when it has grown past maxLogBytes. It returns a slog.Logger
// writing a text handler at the given level plus the underlying file as an
// io.Closer the caller closes on exit.
//
// It NEVER writes to stdout/stderr: the TUI owns the terminal, so a stray log
// line would corrupt the rendered frame. All diagnostics go to the file.
func OpenLogger(p paths.Paths, level slog.Level) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(p.LogDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create log dir %s: %w", p.LogDir, err)
	}
	rotateIfLarge(p.LogFile)
	f, err := os.OpenFile(p.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %s: %w", p.LogFile, err)
	}
	handler := slog.NewTextHandler(f, &slog.HandlerOptions{Level: level})
	return slog.New(handler), f, nil
}

// rotateIfLarge renames logFile to logFile+".1" (overwriting any prior backup)
// when the current file exceeds maxLogBytes, so the next OpenLogger starts a
// fresh generation. A missing file or a stat/rename error is a no-op: rotation
// is best-effort housekeeping and must never block logging from starting.
func rotateIfLarge(logFile string) {
	info, err := os.Stat(logFile)
	if err != nil || info.Size() <= maxLogBytes {
		return
	}
	_ = os.Rename(logFile, logFile+".1")
}
