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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// spoolFile is the file surface the writer needs. *os.File satisfies it in
// production; tests substitute a fake to exercise the write/close/sync error
// paths that a real filesystem will not produce on demand.
type spoolFile interface {
	io.Writer
	Sync() error
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
	// activePath is the path of the file currentFile writes to, "" when no file
	// is open. A recovery sweeper excludes this path from the set of files it
	// re-ingests, so it never reads a line still being appended. Guarded by mu.
	activePath string
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
	w := &Writer{
		dir:          dir,
		instanceID:   instanceID,
		maxFileSize:  int64(maxFileSizeMB) * 1024 * 1024,
		maxTotalSize: int64(maxTotalSizeMB) * 1024 * 1024,
		onWrite:      onWrite,
	}
	// Continue the sequence past any files a previous process left behind, so a
	// fresh file never reuses a leftover's name. Otherwise a same-day restart
	// would reopen audit-DATE-0001 (O_APPEND) at the same moment a recovery
	// sweeper is re-ingesting + deleting that leftover — a write/delete race that
	// could lose the freshly appended records. Distinct names make leftovers
	// safely recoverable while this process writes new files. A scan error is
	// non-fatal: sequence stays 0 and the names fall back to the prior behaviour.
	w.sequence = maxSequence(instanceDir)
	return w, nil
}

// maxSequence returns the highest NNNN suffix among audit-*-NNNN.ndjson files in
// instanceDir, or 0 when none exist / the dir cannot be read. Used to continue the
// per-process sequence past leftover files from an earlier run.
func maxSequence(instanceDir string) int {
	entries, err := readDir(instanceDir)
	if err != nil {
		return 0
	}
	max := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if seq, ok := parseSequence(e.Name()); ok && seq > max {
			max = seq
		}
	}
	return max
}

// parseSequence extracts NNNN from audit-YYYYMMDD-NNNN.ndjson. Returns ok=false
// for any name not matching that shape.
func parseSequence(name string) (int, bool) {
	if !strings.HasPrefix(name, "audit-") || !strings.HasSuffix(name, ".ndjson") {
		return 0, false
	}
	stem := strings.TrimSuffix(name, ".ndjson")
	i := strings.LastIndexByte(stem, '-')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(stem[i+1:])
	if err != nil {
		return 0, false
	}
	return n, true
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

// WriteBatch durably writes a pre-assembled block of one or more NDJSON lines
// (each already '\n'-terminated) in a SINGLE write — the batched form of Write.
// The spill worker accumulates many records' marshaled bytes up to a SIZE
// threshold and flushes them here as one large sequential write, so the spool is
// written in a few big writes instead of one syscall per record. The per-record
// form issued ~1 write syscall per audit record and saturated disk IOPS under
// load (write %util ~90% at ~800 small writes/s); batching by bytes collapses
// that to a handful of large writes. Same rotation + total-quota discipline as
// Write; the block is written whole (not split at the file-size boundary — a
// batch may briefly carry the current file past maxFileSize, which only affects
// when the NEXT batch rotates).
func (w *Writer) WriteBatch(block []byte) error {
	if len(block) == 0 {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if total, err := w.dirSize(); err == nil && total >= w.maxTotalSize {
		return fmt.Errorf("audit/ndjson: instance spool %d bytes exceeds quota %d", total, w.maxTotalSize)
	}

	if w.currentFile != nil && w.currentSize+int64(len(block)) > w.maxFileSize {
		if err := w.rotateFile(); err != nil {
			return err
		}
	}
	if w.currentFile == nil {
		if err := w.openNewFile(); err != nil {
			return err
		}
	}

	n, err := w.currentFile.Write(block)
	if err != nil {
		return fmt.Errorf("audit/ndjson: write batch: %w", err)
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
		// fsync before close so a clean shutdown leaves the spool durable on stable
		// storage (best-effort: a sync error still proceeds to close + report).
		syncErr := w.currentFile.Sync()
		closeErr := w.currentFile.Close()
		w.currentFile = nil
		w.activePath = ""
		if syncErr != nil {
			return fmt.Errorf("audit/ndjson: fsync on close: %w", syncErr)
		}
		return closeErr
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
	w.activePath = path
	return nil
}

// rotateFile fsyncs then closes the current file so the next Write opens a fresh
// one. The fsync makes the sealed file durable on stable storage BEFORE it
// becomes eligible for a recovery sweeper to read and delete — closing the
// no-loss gap where a machine/kernel crash could lose page-cache-only writes of
// a file the sweeper had already drained+removed. The still-active file (between
// rotations) has a residual non-fsynced window bounded by the sweeper's Rotate()
// cadence. Must hold w.mu.
func (w *Writer) rotateFile() error {
	if w.currentFile != nil {
		if err := w.currentFile.Sync(); err != nil {
			return fmt.Errorf("audit/ndjson: fsync for rotation: %w", err)
		}
		if err := w.currentFile.Close(); err != nil {
			return fmt.Errorf("audit/ndjson: close for rotation: %w", err)
		}
		w.currentFile = nil
		w.currentSize = 0
		w.activePath = ""
	}
	return nil
}

// Rotate seals the current spool file (if any) so a recovery sweeper may
// re-ingest it: it closes the open file and clears the active path, so the next
// Write opens a fresh file. A no-op (returning nil) when no file is open, so a
// sweeper can call it every pass cheaply — it only seals when records have been
// written since the last rotation. Safe for concurrent use.
func (w *Writer) Rotate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rotateFile()
}

// SealedFiles returns the absolute paths of the spool files that are complete —
// every audit-*.ndjson in the instance directory EXCEPT the one currently being
// appended (activePath) — sorted by name, which is chronological (date then
// sequence). These are safe for a recovery sweeper to read in full and delete
// once durably re-ingested; the active file is excluded so a partially-written
// trailing line is never read. Safe for concurrent use.
func (w *Writer) SealedFiles() ([]string, error) {
	w.mu.Lock()
	active := w.activePath
	w.mu.Unlock()

	instanceDir := filepath.Join(w.dir, w.instanceID)
	entries, err := readDir(instanceDir)
	if err != nil {
		return nil, fmt.Errorf("audit/ndjson: list spool dir %s: %w", instanceDir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "audit-") || !strings.HasSuffix(name, ".ndjson") {
			continue
		}
		path := filepath.Join(instanceDir, name)
		if path == active {
			continue
		}
		out = append(out, path)
	}
	// Filenames embed date + zero-padded sequence, so lexical order is
	// chronological — the oldest spill is drained first.
	sort.Strings(out)
	return out, nil
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
