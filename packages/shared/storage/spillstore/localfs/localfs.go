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
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
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
	// EncryptionKey, when non-nil, enables at-rest AES-256-GCM encryption of
	// every spilled object (32 bytes, mandatory length when set). Callers that
	// hold sensitive plaintext (the agent spills decrypted request/response
	// bodies) MUST set it so a lost disk / backup never exposes the bodies —
	// matching the SQLCipher protection on the inline-body path. Nil = plaintext
	// on disk (acceptable only for non-sensitive / dev-fallback stores).
	EncryptionKey []byte
}

// Store implements `spillstore.SpillStore` rooted at a local directory.
type Store struct {
	root         string
	perObjectCap int64
	totalCap     int64
	retention    time.Duration
	// aead, when non-nil, seals every object at rest with AES-256-GCM. Built
	// once at New from the 32-byte EncryptionKey. SpillRef.Size / SHA256 always
	// describe the PLAINTEXT so Hub + the pre-sign uploader (which hash the
	// plaintext they PUT) stay consistent; the on-disk file is
	// [nonce][GCM ciphertext+tag].
	aead cipher.AEAD

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
	var aead cipher.AEAD
	if len(opts.EncryptionKey) > 0 {
		if len(opts.EncryptionKey) != 32 {
			return nil, fmt.Errorf("localfs: EncryptionKey must be 32 bytes (AES-256), got %d", len(opts.EncryptionKey))
		}
		// With the 32-byte length validated above, aes.NewCipher and
		// cipher.NewGCM are infallible (NewCipher only errors on a bad key
		// length; GCM supports every AES block), so the errors are dropped.
		block, _ := aes.NewCipher(opts.EncryptionKey)
		aead, _ = cipher.NewGCM(block)
	}
	return &Store{
		root:         opts.Root,
		perObjectCap: opts.PerObjectCap,
		totalCap:     opts.TotalSizeCap,
		retention:    opts.Retention,
		aead:         aead,
	}, nil
}

// seal AES-256-GCM-seals plaintext with a fresh random nonce prepended.
// crypto/rand.Read never returns an error on supported platforms (it panics
// internally if the OS RNG is unavailable), so seal is infallible here.
func (s *Store) seal(plaintext []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, plaintext, nil)
}

// open reverses seal: splits the prepended nonce and GCM-opens.
func (s *Store) open(sealed []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("localfs: sealed object shorter than nonce")
	}
	return s.aead.Open(nil, sealed[:ns], sealed[ns:], nil)
}

// readCapped reads up to cap bytes and reports whether the source had more
// (truncation). Used by the encrypted Put path, which must buffer the whole
// plaintext to seal it in one shot.
func readCapped(r io.Reader, cap int64) ([]byte, bool, error) {
	buf, err := io.ReadAll(io.LimitReader(r, cap))
	if err != nil {
		return nil, false, err
	}
	var probe [1]byte
	n, _ := io.ReadFull(r, probe[:])
	return buf, n > 0, nil
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
	// When the caller passes an explicit (token-signed) Key, write to
	// exactly that key rather than re-deriving from EventID+direction+today. The
	// Hub spill-blob handler uses this to honour the node-namespaced key the mint
	// signed, so one node cannot overwrite another node's object and there is no
	// midnight day-drift between mint and PUT. Direct in-process callers leave
	// Key empty and keep the legacy deterministic derivation.
	key := opts.Key
	if key == "" {
		key = s.keyFor(opts.EventID, direction)
	}
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

	var written int64
	var sum []byte
	truncated := false
	if s.aead != nil {
		// Encrypted path: the whole plaintext must be buffered to seal it in
		// one GCM shot. SHA256 + Size describe the PLAINTEXT (Hub/uploader
		// hash the plaintext they PUT); the on-disk file is nonce+ciphertext.
		plain, trunc, rerr := readCapped(content, s.perObjectCap)
		if rerr != nil {
			_ = tmp.Close()
			return audit.SpillRef{}, fmt.Errorf("localfs.Put: read: %w", rerr)
		}
		truncated = trunc
		written = int64(len(plain))
		h := sha256.Sum256(plain)
		sum = h[:]
		sealed := s.seal(plain)
		if _, werr := tmp.Write(sealed); werr != nil {
			_ = tmp.Close()
			return audit.SpillRef{}, fmt.Errorf("localfs.Put: write: %w", werr)
		}
	} else {
		// Plaintext path (no key): stream straight to disk in constant memory.
		hasher := sha256.New()
		limited := io.LimitReader(content, s.perObjectCap)
		tee := io.TeeReader(limited, hasher)
		w, cerr := io.Copy(tmp, tee)
		if cerr != nil {
			_ = tmp.Close()
			return audit.SpillRef{}, fmt.Errorf("localfs.Put: copy: %w", cerr)
		}
		written = w
		// Detect truncation by peeking one byte past the cap. If the upstream
		// reader still has data we know we clipped at perObjectCap and stamp
		// Truncated=true so the audit row reflects it.
		var probe [1]byte
		if n, _ := io.ReadFull(content, probe[:]); n > 0 {
			truncated = true
		}
		sum = hasher.Sum(nil)
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
		SHA256:      hex.EncodeToString(sum),
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
	if s.aead == nil {
		return f, nil // plaintext: stream straight from disk
	}
	// Encrypted: read the sealed file, GCM-open, hand back the plaintext.
	// (The drain reads the whole body into memory for the pre-sign upload
	// regardless, so buffering here does not change the memory profile.)
	sealed, rerr := io.ReadAll(f)
	_ = f.Close()
	if rerr != nil {
		return nil, fmt.Errorf("localfs.Get: read: %w", rerr)
	}
	plain, oerr := s.open(sealed)
	if oerr != nil {
		return nil, fmt.Errorf("localfs.Get: decrypt: %w", oerr)
	}
	return io.NopCloser(bytes.NewReader(plain)), nil
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

// Sweep implements SpillStore. It is the age-only sweep — equivalent to
// SweepFiltered with a nil filter (no reference check). Callers that can
// supply a traffic_event reference check should use SweepFiltered so a
// blob whose spill_ref is still live is never deleted.
func (s *Store) Sweep(ctx context.Context, olderThan time.Time) (int, error) {
	return s.SweepFiltered(ctx, olderThan, nil)
}

// SweepFiltered implements spillstore.RefAwareSweeper.
//
// Three-pass policy:
//  1. Collect every `.bin` object and split into age-eligible candidates
//     (mtime before olderThan) and the rest.
//  2. Ask the filter which candidate keys are still referenced; delete only
//     the unreferenced ones. A nil filter keeps nothing on reference grounds
//     (pure age-based behaviour). On filter error nothing is deleted and the
//     error is returned (fail-safe — a DB hiccup must not orphan live spills).
//  3. If the post-deletion total size still exceeds the configured cap, evict
//     the oldest remaining objects until the total fits, skipping any blob the
//     filter marked as still referenced (same orphan hazard as the age pass).
//
// Empty day-directories are removed at the end so `find` / `du` stay tidy.
func (s *Store) SweepFiltered(ctx context.Context, olderThan time.Time, filter spillstore.SweepFilter) (int, error) {
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

	// Resolve the storage key (relative to root) for every entry so we can
	// match against traffic_event.spill_ref, which stores the key — not the
	// absolute on-disk path.
	keyOf := func(path string) string {
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return path
		}
		return filepath.ToSlash(rel)
	}

	// Reference-check every resident key in one batch: BOTH the age pass and
	// the total-size cap pass below can delete a blob, so both must honour the
	// reference set (checking only age-eligible keys would let the cap pass
	// evict a still-referenced blob — the same orphan hazard).
	candidateKeys := make([]string, len(entries))
	for i, e := range entries {
		candidateKeys[i] = keyOf(e.path)
	}

	referenced, err := spillstore.ResolveReferenced(ctx, filter, candidateKeys)
	if err != nil {
		return 0, fmt.Errorf("localfs.Sweep: reference check: %w", err)
	}

	deleted := 0
	var keep []fileEntry
	for _, e := range entries {
		if e.mod.Before(olderThan) && !referenced[keyOf(e.path)] {
			if rmErr := os.Remove(e.path); rmErr == nil {
				deleted++
			}
			continue
		}
		keep = append(keep, e)
	}

	// total-size cap: oldest first, but never evict a still-referenced blob.
	sort.Slice(keep, func(i, j int) bool { return keep[i].mod.Before(keep[j].mod) })
	var total int64
	for _, e := range keep {
		total += e.size
	}
	for i := 0; total > s.totalCap && i < len(keep); i++ {
		if referenced[keyOf(keep[i].path)] {
			continue
		}
		if rmErr := os.Remove(keep[i].path); rmErr == nil {
			deleted++
			total -= keep[i].size
		} else {
			// can't delete — skip rather than loop forever
			continue
		}
	}

	// prune empty day-directories
	dayEntries, _ := os.ReadDir(s.root)
	for _, d := range dayEntries {
		if !d.IsDir() {
			continue
		}
		full := filepath.Join(s.root, d.Name())
		inner, rdErr := os.ReadDir(full)
		if rdErr == nil && len(inner) == 0 {
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
