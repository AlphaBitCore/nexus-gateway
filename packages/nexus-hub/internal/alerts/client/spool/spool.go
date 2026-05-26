// Package spool is a durable append-only queue backed by one file per
// envelope under <dir>/<name>/. Purpose: buffer events that a caller wants
// to deliver asynchronously when the receiver is unavailable (e.g.
// alertclient.Fire when Hub is unreachable). Crash-safe: files are created
// via temp+rename, deleted only after the drain callback returns nil.
//
// Ordering: FIFO by enqueue time, enforced via monotonically increasing
// filenames (`<unixNano>_<seq>.json`).
//
// Cap: when the directory exceeds maxBytes, the oldest envelope files are
// removed until under the cap, incrementing an internal dropped counter.
//
// Concurrency: Enqueue and Drain are mutually exclusive via an internal
// mutex. Drain holds the lock for the duration of the callback — callers
// that need the lock released during long network sends should batch
// envelopes externally.
package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
)

// Spool is a generic durable append-only queue. T must be JSON-marshalable.
type Spool[T any] struct {
	dir      string
	maxBytes int64
	logger   *slog.Logger

	mu      sync.Mutex
	seq     uint64
	dropped atomic.Int64
	skipped atomic.Int64
}

// New creates (or reopens) a spool backed by <dir>/<name>/.
// maxBytes is the soft cap on total file size; 0 means unlimited.
func New[T any](dir, name string, maxBytes int64, logger *slog.Logger) (*Spool[T], error) {
	if name == "" {
		return nil, errors.New("spool: name required")
	}
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(full, 0o750); err != nil {
		return nil, fmt.Errorf("spool mkdir: %w", err)
	}
	return &Spool[T]{dir: full, maxBytes: maxBytes, logger: logger}, nil
}

// Enqueue persists item to disk atomically (write-tmp + rename).
// Returns an error only if the OS write fails.
func (s *Spool[T]) Enqueue(item T) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("spool marshal: %w", err)
	}

	s.seq++
	name := fmt.Sprintf("%020d_%06d.json", nowNanos(), s.seq)
	tmp := filepath.Join(s.dir, name+".tmp")
	final := filepath.Join(s.dir, name)

	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return fmt.Errorf("spool write: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("spool rename: %w", err)
	}
	s.enforceCap()
	return nil
}

// contextDone is a structural interface so spool.go avoids importing "context".
// *context.emptyCtx (context.Background()) and all standard context values satisfy it.
type contextDone interface {
	Done() <-chan struct{}
}

// Drain calls send for each pending envelope in FIFO order. If send returns
// an error, Drain stops immediately — the failed envelope and all subsequent
// ones remain on disk. Successfully sent envelopes are removed from disk.
// Corrupt envelopes are skipped and removed (logged at WARN level).
// Returns (count of successfully sent envelopes, first send error or nil).
func (s *Spool[T]) Drain(ctx contextDone, send func(T) error) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	files, err := s.listFiles()
	if err != nil {
		return 0, err
	}

	drained := 0
	for _, f := range files {
		select {
		case <-ctx.Done():
			return drained, nil
		default:
		}

		full := filepath.Join(s.dir, f)
		data, err := os.ReadFile(full)
		if err != nil {
			return drained, fmt.Errorf("spool read %s: %w", f, err)
		}
		var v T
		if err := json.Unmarshal(data, &v); err != nil {
			s.logger.Warn("spool: skipping corrupt envelope", "file", f, "err", err)
			s.skipped.Add(1)
			_ = os.Remove(full)
			continue
		}
		if err := send(v); err != nil {
			return drained, err
		}
		if err := os.Remove(full); err != nil {
			return drained, fmt.Errorf("spool remove after send: %w", err)
		}
		drained++
	}
	return drained, nil
}

// PendingCount returns the number of envelopes currently on disk.
func (s *Spool[T]) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.listFiles()
	if err != nil {
		return 0
	}
	return len(files)
}

// Dropped returns the number of envelopes removed by cap enforcement.
func (s *Spool[T]) Dropped() int64 { return s.dropped.Load() }

// Skipped returns the number of corrupt envelopes skipped during Drain.
func (s *Spool[T]) Skipped() int64 { return s.skipped.Load() }

// listFiles returns non-tmp envelope filenames in lexical (FIFO) order.
// Must be called under s.mu.
func (s *Spool[T]) listFiles() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("spool readdir: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".json" {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// enforceCap removes the oldest envelopes until total bytes <= maxBytes.
// Must be called under s.mu.
func (s *Spool[T]) enforceCap() {
	if s.maxBytes <= 0 {
		return
	}
	files, err := s.listFiles()
	if err != nil {
		return
	}
	var total int64
	sizes := make([]int64, len(files))
	for i, f := range files {
		info, err := os.Stat(filepath.Join(s.dir, f))
		if err != nil {
			continue
		}
		sizes[i] = info.Size()
		total += info.Size()
	}
	i := 0
	for total > s.maxBytes && i < len(files) {
		_ = os.Remove(filepath.Join(s.dir, files[i]))
		total -= sizes[i]
		s.dropped.Add(1)
		s.logger.Warn("spool: cap exceeded, evicted oldest", "file", files[i])
		i++
	}
}

// nowNanos returns the current Unix time in nanoseconds.
// Indirected via a variable for test determinism if ever needed.
var nowNanos = timeNowUnixNano
