// Package ndjson provides a durable, rotating NDJSON spill writer used as the
// last-resort fallback for audit/event pipelines when the primary transport
// (message queue + database) is unavailable or back-pressured.
//
// It is transport- and schema-agnostic: each caller marshals its own record to
// JSON bytes and hands them to Write, which appends exactly one line per
// record, rotates spool files by size, and enforces a total on-disk quota per
// instance so that a sustained outage spills to disk instead of either losing
// data silently or filling the disk. Recovery (re-ingesting spooled lines once
// the primary transport returns) is the operator's / a separate sweeper's job;
// this package only guarantees the records are durably captured.
//
// Files are written under {dir}/{instanceID}/audit-{YYYYMMDD}-{NNNN}.ndjson.
// The per-instance subdirectory keeps two processes sharing a spool root (for
// example a co-located gateway and proxy) from interleaving into one file.
package ndjson

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// spoolFile is the file surface the writer needs. *os.File satisfies it in
// production; tests substitute a fake to exercise the write/close error paths
// that a real filesystem will not produce on demand.
type spoolFile interface {
	io.Writer
	Close() error
}

// openSpool opens the spool file at path and reports its current size. It is a
// package var (not a direct os call) so fault tests can inject open/stat
// failures and a write-failing handle. Production always uses the real opener.
var openSpool = func(path string) (spoolFile, int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("audit/ndjson: open file %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("audit/ndjson: stat file %s: %w", path, err)
	}
	return f, info.Size(), nil
}

// readDir lists the spool directory. A package var so a fault test can inject a
// ReadDir error or a dir entry whose Info() fails.
var readDir = os.ReadDir

// Writer appends pre-marshaled JSON records to rotating NDJSON spool files.
// Safe for concurrent use by multiple goroutines.
type Writer struct {
	dir          string
	instanceID   string
	maxFileSize  int64
	maxTotalSize int64

	// onWrite, when non-nil, is invoked with the number of bytes written
	// after each successful append. Callers wire their own metrics here so
	// this package carries no dependency on any metric registry.
	onWrite func(bytes int)

	mu          sync.Mutex
	currentFile spoolFile
	currentSize int64
	sequence    int
}

// New creates a spill writer rooted at dir for the given instanceID, creating
// the per-instance subdirectory if needed. maxFileSizeMB caps a single spool
// file before rotation; maxTotalSizeMB caps the instance's total on-disk spool
// (writes past the quota fail loudly rather than fill the disk). onWrite may be
// nil; when set it receives the byte count of each successful append.
func New(dir, instanceID string, maxFileSizeMB, maxTotalSizeMB int, onWrite func(bytes int)) (*Writer, error) {
	instanceDir := filepath.Join(dir, instanceID)
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		return nil, fmt.Errorf("audit/ndjson: create spool directory %s: %w", instanceDir, err)
	}
	return &Writer{
		dir:          dir,
		instanceID:   instanceID,
		maxFileSize:  int64(maxFileSizeMB) * 1024 * 1024,
		maxTotalSize: int64(maxTotalSizeMB) * 1024 * 1024,
		onWrite:      onWrite,
	}, nil
}

// Write appends one record as a single NDJSON line. record must be a complete
// JSON document with no trailing newline; Write adds the line terminator. It
// rotates the current file when it would exceed maxFileSize and refuses the
// write (returning an error) when the instance spool already exceeds
// maxTotalSize — the caller decides what to do with a refused record (a
// last-resort loud drop), so no data disappears without an error.
func (w *Writer) Write(record []byte) error {
	line := make([]byte, 0, len(record)+1)
	line = append(line, record...)
	line = append(line, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()

	// Total-quota gate. On a stat error, fall through and write anyway —
	// losing a record to a transient stat failure is worse than the small
	// risk of briefly exceeding the soft quota.
	if total, err := w.dirSize(); err == nil && total >= w.maxTotalSize {
		return fmt.Errorf("audit/ndjson: instance spool %d bytes exceeds quota %d", total, w.maxTotalSize)
	}

	if w.currentFile != nil && w.currentSize+int64(len(line)) > w.maxFileSize {
		if err := w.rotateFile(); err != nil {
			return err
		}
	}
	if w.currentFile == nil {
		if err := w.openNewFile(); err != nil {
			return err
		}
	}

	n, err := w.currentFile.Write(line)
	if err != nil {
		return fmt.Errorf("audit/ndjson: write: %w", err)
	}
	w.currentSize += int64(n)
	if w.onWrite != nil {
		w.onWrite(n)
	}
	return nil
}

// Close closes the current spool file handle. Safe to call more than once.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.currentFile != nil {
		err := w.currentFile.Close()
		w.currentFile = nil
		return err
	}
	return nil
}

// openNewFile opens the next spool file:
// {dir}/{instanceID}/audit-{YYYYMMDD}-{sequence:04d}.ndjson. Must hold w.mu.
func (w *Writer) openNewFile() error {
	w.sequence++
	name := fmt.Sprintf("audit-%s-%04d.ndjson", time.Now().UTC().Format("20060102"), w.sequence)
	path := filepath.Join(w.dir, w.instanceID, name)

	f, size, err := openSpool(path)
	if err != nil {
		return err
	}
	w.currentFile = f
	w.currentSize = size
	return nil
}

// rotateFile closes the current file so the next Write opens a fresh one. Must
// hold w.mu.
func (w *Writer) rotateFile() error {
	if w.currentFile != nil {
		if err := w.currentFile.Close(); err != nil {
			return fmt.Errorf("audit/ndjson: close for rotation: %w", err)
		}
		w.currentFile = nil
		w.currentSize = 0
	}
	return nil
}

// dirSize sums the sizes of the files in the instance spool directory. Must
// hold w.mu.
func (w *Writer) dirSize() (int64, error) {
	instanceDir := filepath.Join(w.dir, w.instanceID)
	entries, err := readDir(instanceDir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}
