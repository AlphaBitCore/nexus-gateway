// Package async wraps a SpillStore so Put returns immediately with a
// fully-formed SpillRef while the actual backend upload (S3 PutObject,
// localfs write) happens on a background goroutine.
//
// Why: inspect-path goroutines call spillstore.EmitBody → store.Put per
// audited request. The S3 PutObject inside Put can run 50-500 ms in
// production; per-request blocking adds up and shows as user-visible
// page slow-down. The audit-side write was made async in #78 (the
// agent's SQLite write queue); this completes the picture by also
// taking the body upload off the hot path.
//
// Correctness contract (the trade vs sync Put):
//
//   - Put returns a SpillRef whose Key is deterministically computed
//     ahead of the actual upload (via the inner backend's KeyFor
//     contract — every production backend implements it). SHA-256 is
//     computed inline from the buffered bytes. Size + ContentType are
//     known up front. So the audit row emitted right after Put carries
//     a fully-valid ref, indistinguishable from a sync upload.
//
//   - The actual PutObject lands seconds-to-minutes later in the
//     background goroutine. Hub-side reads of the spilled body (via
//     presigned GET) ONLY happen after the agent uploads the audit
//     event to Hub, which itself batches every few seconds; in
//     practice the upload completes long before any reader asks.
//
//   - On upload failure, the SpillRef points at a missing object.
//     This is logged loudly (WARN) and counted in a Drops gauge.
//     Callers that need fail-closed durability should NOT wrap with
//     AsyncStore — they should use the underlying store directly.
//
//   - On Close, queued uploads drain with a bounded timeout. Anything
//     still in flight after the timeout is logged as lost.
//
// What this is NOT:
//
//   - It does not retry failed uploads. The expectation is that S3 is
//     reliable enough that the rare failure is acceptable to lose; if
//     a retry policy is needed later, wrap the inner store with a
//     retry decorator BEFORE wrapping with AsyncStore.
//
//   - It does not buffer to disk. Bodies live in the goroutine's
//     queue (a bounded channel of byte slices) until uploaded; on
//     daemon crash anything still queued is lost. Acceptable given
//     the audit pipeline as a whole already has at-most-once
//     semantics across crashes.
package async

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// Options configures the async wrapper.
type Options struct {
	// QueueCapacity bounds the number of pending uploads. When the
	// queue is full, Put still returns a valid SpillRef but the upload
	// is dropped — Drops is incremented. Default: 256 (matches roughly
	// one minute of peak agent inspect traffic at 4 rps).
	QueueCapacity int
	// UploadTimeout caps how long a single background PutObject may
	// take before its context cancels. Default: 30s.
	UploadTimeout time.Duration
	// DrainTimeout caps Flush + Close. Default: 30s.
	DrainTimeout time.Duration
	// Logger receives Warn-level upload failures + drop notifications.
	// Nil disables logging (test-only).
	Logger *slog.Logger
}

// AsyncStore wraps an inner SpillStore so Put is non-blocking. The inner
// store MUST implement spillstore.Presigner (for KeyFor); production
// backends (s3, localfs) both do.
type AsyncStore struct {
	inner   spillstore.SpillStore
	keyer   spillstore.Presigner
	opts    Options
	logger  *slog.Logger
	queue   chan asyncJob
	done    chan struct{}
	wg      sync.WaitGroup
	closeOnce sync.Once

	// drops counts uploads dropped because the queue was full at Put
	// time. Inspect-path callers never see this — it surfaces only in
	// Drops() / logs. Atomic so the worker + Drops getter can read
	// without locking.
	drops atomic.Int64
	// uploads counts successfully completed background uploads
	// (atomic for the same reason).
	uploads atomic.Int64
	// failures counts background uploads that returned an error from
	// the inner store (S3 PutObject failed, etc.).
	failures atomic.Int64
	// flushReq is a sync barrier: send a chan to wait for all then-
	// queued uploads to land. Buffered to 4 so concurrent Flush
	// callers don't deadlock.
	flushReq chan chan struct{}
}

// asyncJob is the per-Put record passed through the queue. body and
// opts come from the caller; the buffered body is owned by the job
// for the lifetime of the upload.
type asyncJob struct {
	body  []byte
	size  int64
	opts  spillstore.PutOptions
	key   string
}

// New wraps inner with an async upload queue. The returned store
// satisfies spillstore.SpillStore — drop-in replacement for the inner
// in any caller. Get / Delete / Sweep / Stat / Backend all pass
// through to inner.
//
// Returns an error if inner does not implement spillstore.Presigner
// (every production backend does; an opaque backend that cannot
// pre-compute keys cannot be wrapped because Put must return a key
// before the upload runs).
func New(inner spillstore.SpillStore, opts Options) (*AsyncStore, error) {
	if inner == nil {
		return nil, errors.New("async: inner SpillStore is required")
	}
	keyer, ok := inner.(spillstore.Presigner)
	if !ok {
		return nil, fmt.Errorf("async: inner store %q does not implement Presigner (KeyFor required for ahead-of-upload ref)", inner.Backend())
	}
	if opts.QueueCapacity <= 0 {
		opts.QueueCapacity = 256
	}
	if opts.UploadTimeout <= 0 {
		opts.UploadTimeout = 30 * time.Second
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = 30 * time.Second
	}
	store := &AsyncStore{
		inner:    inner,
		keyer:    keyer,
		opts:     opts,
		logger:   opts.Logger,
		queue:    make(chan asyncJob, opts.QueueCapacity),
		done:     make(chan struct{}),
		flushReq: make(chan chan struct{}, 4),
	}
	store.wg.Add(1)
	go store.uploader()
	return store, nil
}

// Backend mirrors the inner backend name so audit rows record the same
// backend identifier they would under a sync wiring.
func (a *AsyncStore) Backend() string { return a.inner.Backend() }

// Put buffers content into memory, computes the SpillRef (key + sha
// + size + content-type) synchronously, queues the actual upload
// for the background goroutine, and returns the ref immediately.
//
// Caller is unblocked in O(ns + ms-to-hash-bytes); the inspect path
// never waits on S3 PutObject. The returned SpillRef is the SAME shape
// the inner sync store would have returned — fields are derived from
// the buffered bytes, not from the upload response.
//
// If the queue is full, Put STILL returns a valid ref (with the
// pre-computed key) but increments the Drops counter and logs a
// WARN. The audit row will reference an object that never lands in
// the backend; Hub-side reads will get 404. This is intentional —
// the alternative (block in Put) defeats the whole purpose.
func (a *AsyncStore) Put(ctx context.Context, content io.Reader, size int64, opts spillstore.PutOptions) (audit.SpillRef, error) {
	if opts.EventID == "" {
		return audit.SpillRef{}, errors.New("async.Put: EventID is required")
	}

	// Drain content into a buffer so the goroutine has its own copy.
	// The caller may close the reader as soon as Put returns.
	var buf bytes.Buffer
	if size > 0 {
		buf.Grow(int(size))
	}
	written, err := io.Copy(&buf, content)
	if err != nil {
		return audit.SpillRef{}, fmt.Errorf("async.Put: read content: %w", err)
	}
	body := buf.Bytes()

	// Hash inline. CPU-bound, fast even for 5 MB. Done sync so the
	// SpillRef carries the truthful digest at audit-row emit time.
	sha := sha256.Sum256(body)
	hexdigest := hex.EncodeToString(sha[:])

	contentType := opts.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Deterministic key — matches what the inner store's Put would
	// have generated. Hub presigns GET against this key; the
	// background goroutine PutObject against the same key. As long
	// as the upload completes before any reader asks, the trip is
	// seamless.
	key := a.keyer.KeyFor(time.Now().UTC(), opts.EventID, opts.Direction)

	ref := audit.SpillRef{
		Backend:     a.inner.Backend(),
		Key:         key,
		Size:        written,
		SHA256:      hexdigest,
		ContentType: contentType,
		// Truncated is left false here — async.Put does not enforce a
		// per-object cap; that is the inner backend's job at upload time.
		// If the cap clips bytes, the audit row's ref will under-report
		// the in-backend size, which is fine for the read path (Hub
		// fetches whatever is in the backend).
	}

	job := asyncJob{
		body: body,
		size: written,
		opts: opts,
		key:  key,
	}

	select {
	case a.queue <- job:
		// queued; uploader picks it up
	default:
		// queue full — record + log + drop. Ref still returned so the
		// audit row is well-formed; only the body bytes are lost.
		n := a.drops.Add(1)
		if a.logger != nil {
			a.logger.Warn("async spillstore: upload queue full, dropping body",
				"backend", a.inner.Backend(),
				"eventId", opts.EventID,
				"direction", opts.Direction,
				"size", written,
				"drops_total", n,
				"queue_capacity", a.opts.QueueCapacity,
			)
		}
	}

	return ref, nil
}

// Get / Delete / Sweep / Stat pass through to the inner store. These
// read-side operations are not on the inspect hot path so no async
// wrapping is needed.
func (a *AsyncStore) Get(ctx context.Context, ref audit.SpillRef) (io.ReadCloser, error) {
	return a.inner.Get(ctx, ref)
}
func (a *AsyncStore) Delete(ctx context.Context, ref audit.SpillRef) error {
	return a.inner.Delete(ctx, ref)
}
func (a *AsyncStore) Sweep(ctx context.Context, olderThan time.Time) (int, error) {
	return a.inner.Sweep(ctx, olderThan)
}
func (a *AsyncStore) Stat(ctx context.Context) (spillstore.Stats, error) {
	return a.inner.Stat(ctx)
}

// Drops returns the cumulative count of uploads dropped because the
// queue was full. Surfaces in admin metrics so operators can tune
// QueueCapacity.
func (a *AsyncStore) Drops() int64 { return a.drops.Load() }

// Uploads returns the cumulative count of successfully completed
// background uploads. Diagnostic counter for "are we keeping up?".
func (a *AsyncStore) Uploads() int64 { return a.uploads.Load() }

// Failures returns the cumulative count of background uploads that
// returned an error from the inner store (S3 PutObject failed, etc.).
// Distinct from Drops — Drops are queue-full skips, Failures are
// attempted uploads that errored.
func (a *AsyncStore) Failures() int64 { return a.failures.Load() }

// Flush blocks until every upload queued before Flush call has either
// completed or failed. Used by tests and by Close. Returns ctx.Err()
// on timeout or AsyncStore-closed.
func (a *AsyncStore) Flush(ctx context.Context) error {
	if a == nil {
		return nil
	}
	resp := make(chan struct{})
	select {
	case a.flushReq <- resp:
	case <-a.done:
		return nil // already closed; nothing to flush
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-resp:
		return nil
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the background uploader after draining any queued jobs.
// Bounded by Options.DrainTimeout. Idempotent under concurrent
// invocation.
func (a *AsyncStore) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() { close(a.done) })
	doneCh := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(doneCh)
	}()
	timeout := a.opts.DrainTimeout
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining < timeout {
			timeout = remaining
		}
	}
	select {
	case <-doneCh:
		return nil
	case <-time.After(timeout):
		return errors.New("async spillstore: close drain timeout")
	}
}

// uploader is the background goroutine: pulls jobs off the queue and
// calls the inner store's Put. Honors the done channel for shutdown
// and serves flushReq sync barriers.
func (a *AsyncStore) uploader() {
	defer a.wg.Done()
	for {
		select {
		case <-a.done:
			// Drain remaining queue then exit.
			for {
				select {
				case job := <-a.queue:
					a.runJob(job)
				default:
					return
				}
			}
		case job := <-a.queue:
			a.runJob(job)
		case resp := <-a.flushReq:
			// Drain anything still in the channel, then signal the caller.
		flushDrain:
			for {
				select {
				case job := <-a.queue:
					a.runJob(job)
				default:
					break flushDrain
				}
			}
			close(resp)
		}
	}
}

// runJob performs a single inner Put with a bounded timeout. Errors
// are logged + counted but never propagated — the inspect goroutine
// has already returned and there's nobody to surface the error to.
func (a *AsyncStore) runJob(job asyncJob) {
	ctx, cancel := context.WithTimeout(context.Background(), a.opts.UploadTimeout)
	defer cancel()
	if _, err := a.inner.Put(ctx, bytes.NewReader(job.body), job.size, job.opts); err != nil {
		n := a.failures.Add(1)
		if a.logger != nil {
			a.logger.Warn("async spillstore: background upload failed",
				"backend", a.inner.Backend(),
				"eventId", job.opts.EventID,
				"direction", job.opts.Direction,
				"key", job.key,
				"size", job.size,
				"error", err,
				"failures_total", n,
			)
		}
		return
	}
	a.uploads.Add(1)
}
