// Package localfs implements `spillstore.SpillStore` against the local
// filesystem. Layout is `<root>/<yyyy-mm-dd>/<event-id>-<direction>.bin`,
// chosen so retention sweeps can prune by directory and so an operator
// `find` / `du` over a single day directory is fast.
//
// The backend enforces:
//   - SHA-256 content hash (stamped onto the returned `SpillRef`).
//   - Per-object size cap (default 256 MiB; bytes beyond the cap are
//     truncated and `Truncated=true` is propagated by the caller via
//     `audit.NewSpillBody`).
//   - Total-disk-size cap (default 50 GiB) enforced inside `Sweep`.
//   - Retention (default 30 days) enforced inside `Sweep`.
//
// Sweep is idempotent and concurrent-safe; concurrent `Put`s racing a sweep
// will not be deleted (sweep skips files newer than `olderThan`).
package localfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

const (
	// BackendName is the canonical identifier stamped onto SpillRef.Backend.
	BackendName = "localfs"

	// DefaultPerObjectCapBytes is the hard size limit per spill object.
	DefaultPerObjectCapBytes int64 = 256 * 1024 * 1024 // 256 MiB

	// DefaultTotalSizeCapBytes is the rolling total cap enforced by Sweep.
	DefaultTotalSizeCapBytes int64 = 50 * 1024 * 1024 * 1024 // 50 GiB

	// DefaultRetention is the default age beyond which Sweep deletes.
	DefaultRetention = 30 * 24 * time.Hour
)

// Options configures a Store at construction time.
type Options struct {
	Root         string        // filesystem root; mandatory
	PerObjectCap int64         // 0 ⇒ DefaultPerObjectCapBytes
	TotalSizeCap int64         // 0 ⇒ DefaultTotalSizeCapBytes
	Retention    time.Duration // 0 ⇒ DefaultRetention
}

// Store implements `spillstore.SpillStore` rooted at a local directory.
type Store struct {
	root         string
	perObjectCap int64
	totalCap     int64
	retention    time.Duration

	// sweepMu serializes Sweep with itself; concurrent Put/Get/Delete
	// continue to execute under their own per-file mutex on the OS.
	sweepMu sync.Mutex
}

// New returns a Store rooted at `opts.Root`. It creates the directory if it
// does not exist. Returns an error when `opts.Root` is empty.
func New(opts Options) (*Store, error) {
	if opts.Root == "" {
		return nil, errors.New("localfs: Root is required")
	}
	if err := os.MkdirAll(opts.Root, 0o700); err != nil {
		return nil, fmt.Errorf("localfs: ensure root: %w", err)
	}
	if opts.PerObjectCap <= 0 {
		opts.PerObjectCap = DefaultPerObjectCapBytes
	}
	if opts.TotalSizeCap <= 0 {
		opts.TotalSizeCap = DefaultTotalSizeCapBytes
	}
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}
	return &Store{
		root:         opts.Root,
		perObjectCap: opts.PerObjectCap,
		totalCap:     opts.TotalSizeCap,
		retention:    opts.Retention,
	}, nil
}

// Backend implements SpillStore.
func (s *Store) Backend() string { return BackendName }

func (s *Store) keyFor(eventID, direction string) string {
	day := time.Now().UTC().Format("2006-01-02")
	return filepath.Join(day, fmt.Sprintf("%s-%s.bin", eventID, direction))
}

// KeyFor satisfies spillstore.Presigner: returns the date-prefixed
// key the Store would use for the supplied (event, direction, time).
// Hub's mint endpoint signs this key into the upload token so the
// blob handler writes the bytes to the same place Get will read from.
func (s *Store) KeyFor(at time.Time, eventID, direction string) string {
	day := at.UTC().Format("2006-01-02")
	return filepath.Join(day, fmt.Sprintf("%s-%s.bin", eventID, direction))
}

// PresignPut returns ErrPresignNotSupported — localfs has no pre-signed
// URL primitive. Hub's mint endpoint detects this and falls back to
// the in-Hub `/api/internal/spill/blob/:token` endpoint.
func (s *Store) PresignPut(_ context.Context, _ string, _ int64, _ string, _ time.Duration) (string, error) {
	return "", spillstore.ErrPresignNotSupported
}

func (s *Store) absFor(key string) string {
	return filepath.Join(s.root, key)
}

// Put implements SpillStore.
func (s *Store) Put(ctx context.Context, content io.Reader, size int64, opts spillstore.PutOptions) (audit.SpillRef, error) {
	if opts.EventID == "" {
		return audit.SpillRef{}, errors.New("localfs.Put: EventID is required")
	}
	direction := opts.Direction
	if direction == "" {
		direction = "body"
	}
	key := s.keyFor(opts.EventID, direction)
	abs := s.absFor(key)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return audit.SpillRef{}, fmt.Errorf("localfs.Put: mkdir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(abs), filepath.Base(abs)+".tmp-*")
	if err != nil {
		return audit.SpillRef{}, fmt.Errorf("localfs.Put: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath) // no-op if rename succeeded
	}()

	hasher := sha256.New()
	limited := io.LimitReader(content, s.perObjectCap)
	tee := io.TeeReader(limited, hasher)
	written, err := io.Copy(tmp, tee)
	if err != nil {
		_ = tmp.Close()
		return audit.SpillRef{}, fmt.Errorf("localfs.Put: copy: %w", err)
	}
	// Detect truncation by peeking one byte past the cap. If the upstream
	// reader still has data we know we clipped at perObjectCap and stamp
	// Truncated=true so the audit row reflects it.
	truncated := false
	var probe [1]byte
	if n, _ := io.ReadFull(content, probe[:]); n > 0 {
		truncated = true
	}
	if err := tmp.Close(); err != nil {
		return audit.SpillRef{}, fmt.Errorf("localfs.Put: close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, abs); err != nil {
		return audit.SpillRef{}, fmt.Errorf("localfs.Put: rename: %w", err)
	}

	return audit.SpillRef{
		Backend:     BackendName,
		Key:         key,
		Size:        written,
		SHA256:      hex.EncodeToString(hasher.Sum(nil)),
		ContentType: opts.ContentType,
		Truncated:   truncated,
	}, nil
}

// Get implements SpillStore.
func (s *Store) Get(ctx context.Context, ref audit.SpillRef) (io.ReadCloser, error) {
	if ref.Backend != BackendName {
		return nil, fmt.Errorf("localfs.Get: ref backend %q != %q", ref.Backend, BackendName)
	}
	abs := s.absFor(ref.Key)
	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, spillstore.ErrNotFound
		}
		return nil, fmt.Errorf("localfs.Get: %w", err)
	}
	return f, nil
}

// Delete implements SpillStore.
func (s *Store) Delete(ctx context.Context, ref audit.SpillRef) error {
	if ref.Backend != BackendName {
		return fmt.Errorf("localfs.Delete: ref backend %q != %q", ref.Backend, BackendName)
	}
	abs := s.absFor(ref.Key)
	err := os.Remove(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return spillstore.ErrNotFound
		}
		return fmt.Errorf("localfs.Delete: %w", err)
	}
	return nil
}

type fileEntry struct {
	path string
	mod  time.Time
	size int64
}

// Sweep implements SpillStore.
//
// Two-pass policy:
//  1. Delete every object whose mtime is before `olderThan`.
//  2. If the post-deletion total size still exceeds the configured cap,
//     evict the oldest remaining objects until the total fits.
//
// Empty day-directories are removed at the end so `find` / `du` stay tidy.
func (s *Store) Sweep(ctx context.Context, olderThan time.Time) (int, error) {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()

	var entries []fileEntry
	walkErr := filepath.Walk(s.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort sweep — skip unreadable paths
		}
		if info.IsDir() || filepath.Ext(path) != ".bin" {
			return nil
		}
		entries = append(entries, fileEntry{path: path, mod: info.ModTime(), size: info.Size()})
		return nil
	})
	if walkErr != nil {
		return 0, fmt.Errorf("localfs.Sweep: walk: %w", walkErr)
	}

	deleted := 0
	var keep []fileEntry
	for _, e := range entries {
		if e.mod.Before(olderThan) {
			if err := os.Remove(e.path); err == nil {
				deleted++
			}
			continue
		}
		keep = append(keep, e)
	}

	// total-size cap: oldest first
	sort.Slice(keep, func(i, j int) bool { return keep[i].mod.Before(keep[j].mod) })
	var total int64
	for _, e := range keep {
		total += e.size
	}
	for total > s.totalCap && len(keep) > 0 {
		evict := keep[0]
		keep = keep[1:]
		if err := os.Remove(evict.path); err == nil {
			deleted++
			total -= evict.size
		} else {
			// can't delete — give up rather than loop forever
			break
		}
	}

	// prune empty day-directories
	dayEntries, _ := os.ReadDir(s.root)
	for _, d := range dayEntries {
		if !d.IsDir() {
			continue
		}
		full := filepath.Join(s.root, d.Name())
		inner, err := os.ReadDir(full)
		if err == nil && len(inner) == 0 {
			_ = os.Remove(full)
		}
	}
	return deleted, nil
}

// Stat implements SpillStore.
func (s *Store) Stat(ctx context.Context) (spillstore.Stats, error) {
	stats := spillstore.Stats{Backend: BackendName}
	walkErr := filepath.Walk(s.root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".bin" {
			return nil //nolint:nilerr // best-effort stat — skip unreadable paths
		}
		stats.ObjectCount++
		stats.TotalBytes += info.Size()
		mt := info.ModTime()
		if stats.OldestAt.IsZero() || mt.Before(stats.OldestAt) {
			stats.OldestAt = mt
		}
		if mt.After(stats.NewestAt) {
			stats.NewestAt = mt
		}
		return nil
	})
	if walkErr != nil {
		return stats, fmt.Errorf("localfs.Stat: %w", walkErr)
	}
	return stats, nil
}
