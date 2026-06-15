// Package spillstore provides a pluggable abstraction for storing large
// captured request/response bodies out-of-band of the audit hot path.
//
// The audit pipeline uses a two-tier storage model: bodies whose size fits
// inside a Postgres JSONB column (≤ 256 KiB by default) travel inline on
// `traffic_event_payload`; larger bodies are written through a `SpillStore`
// backend and the row keeps only a `SpillRef`. This keeps DB hot-path query
// performance bounded for AI workloads where a single 1M-token request can
// reach 5 MiB+ raw.
//
// The interface is deliberately minimal — `Put` / `Get` / `Delete` / `Sweep`
// — so adding a new backend (S3, Azure Blob, GCS, mounted volume) is a
// drop-in implementation. The reference `localfs` backend ships in the
// `localfs` sub-package.
package spillstore

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// ErrNotFound is returned by `Get` / `Delete` when the requested ref does
// not resolve to a stored object. Callers should treat this as a non-fatal
// "already gone" rather than an error.
var ErrNotFound = errors.New("spillstore: not found")

// PutOptions carries metadata that travels onto the resulting SpillRef.
type PutOptions struct {
	EventID     string // traffic_event id; appears in the storage key
	Direction   string // "request" | "response"
	ContentType string // hint for renderers (e.g. "text/event-stream")
	// Key, when non-empty, is the EXACT storage key to write to — used by the
	// Hub spill-blob handler to write to the node-namespaced key the mint signed
	// into the upload token, instead of re-deriving from EventID +
	// Direction + today's date. Re-derivation was both a midnight day-drift risk
	// and the cross-node-overwrite bug (two nodes minting for the same eventId
	// re-derived the same key). Empty preserves the legacy re-derive behaviour
	// for direct in-process callers (ai-gateway / compliance-proxy).
	Key string
}

// Stats describes a backend's runtime state. Returned by `Stat()` for
// admin / metrics. Implementations populate what they can; a missing
// figure is left as zero.
type Stats struct {
	Backend     string    // "localfs" | "s3" | …
	ObjectCount int64     // current resident objects
	TotalBytes  int64     // sum of object sizes
	OldestAt    time.Time // oldest object mtime
	NewestAt    time.Time // newest object mtime
}

// Presigner is an optional capability backends may satisfy when they
// can mint a one-shot upload URL the caller can PUT bytes to directly
// (no Hub involvement). The s3 backend satisfies this; localfs does
// not (see ErrPresignNotSupported). Hub's mint endpoint type-asserts
// `SpillStore` to this interface to decide whether to return an S3
// URL or fall back to its own /spill/blob handler.
type Presigner interface {
	PresignPut(ctx context.Context, key string, sizeBytes int64, contentType string, expiresIn time.Duration) (url string, err error)
	KeyFor(at time.Time, eventID, direction string) string
}

// ErrPresignNotSupported is returned by Presigner.PresignPut when the
// backend has no notion of pre-signed URLs (localfs). Hub's mint
// endpoint maps it to the dev-only `/spill/blob/:token` URL.
var ErrPresignNotSupported = errors.New("spillstore: backend does not support pre-signed URLs")

// SweepFilter lets a reference-aware sweep exclude blobs that are still
// pointed at by a live row before deleting them. Without it, an age-based
// sweep can delete a blob whose `traffic_event.spill_ref` still resolves
// to it (rendering the spill permanently unreadable).
//
// KeepReferenced is handed every age-eligible candidate key in a single
// batch and returns the subset that MUST be kept because a row still
// references it. Implementations query the owning datastore (the
// `traffic_event` table). The returned map's keys are a subset of
// `candidateKeys`; an entry with value true means "still referenced —
// do not delete". Absent keys are treated as unreferenced (deletable).
//
// On error the sweep MUST treat the result as fail-safe: delete nothing,
// surface the error. Deleting on a failed reference check is exactly the
// orphan-vs-dangling hazard described above, so a DB hiccup must never
// be read as "no rows reference these keys".
type SweepFilter interface {
	KeepReferenced(ctx context.Context, candidateKeys []string) (referenced map[string]bool, err error)
}

// RefAwareSweeper is the optional capability a backend implements to run a
// reference-checked sweep. Backends that satisfy it iterate age-eligible
// candidates, hand the full candidate key set to the filter in one batch,
// and delete only the keys the filter did NOT mark as referenced.
//
// It is intentionally separate from SpillStore (additive, no signature
// change to the shipped Sweep) so existing callers and the Agent binary's
// pinned interface stay source-compatible. spillsweep.Run type-asserts a
// store to RefAwareSweeper only when a filter is configured; otherwise it
// uses plain age-based Sweep.
//
// A nil filter makes SweepFiltered behave exactly like Sweep (no rows are
// kept on reference grounds), so backends may forward Sweep to
// SweepFiltered(ctx, olderThan, nil) if convenient.
type RefAwareSweeper interface {
	SweepFiltered(ctx context.Context, olderThan time.Time, filter SweepFilter) (deleted int, err error)
}

// ResolveReferenced is the shared reference-check helper every RefAwareSweeper
// backend calls. It returns the set of candidate keys that are still
// referenced and must not be deleted.
//
//   - filter == nil or no candidates → empty set, no error (nothing kept on
//     reference grounds; the sweep behaves as a plain age-based sweep).
//   - filter error → propagated unchanged; callers MUST abort the sweep
//     without deleting anything (fail-safe).
//
// The returned map is never nil so callers can index it directly.
func ResolveReferenced(ctx context.Context, filter SweepFilter, candidateKeys []string) (map[string]bool, error) {
	if filter == nil || len(candidateKeys) == 0 {
		return map[string]bool{}, nil
	}
	referenced, err := filter.KeepReferenced(ctx, candidateKeys)
	if err != nil {
		return nil, err
	}
	if referenced == nil {
		return map[string]bool{}, nil
	}
	return referenced, nil
}

// SpillStore is the cross-service abstraction for body spill storage.
//
// All implementations MUST be safe for concurrent use by multiple goroutines.
type SpillStore interface {
	// Put writes `content` and returns a `SpillRef` that uniquely locates
	// the bytes for later retrieval. Backends must hash the content with
	// SHA-256 and stamp the hex digest into `Ref.SHA256`.
	Put(ctx context.Context, content io.Reader, size int64, opts PutOptions) (audit.SpillRef, error)

	// Get returns a reader over the previously-stored object. Callers must
	// `Close` the reader when done. `ErrNotFound` is returned when the ref
	// does not resolve.
	Get(ctx context.Context, ref audit.SpillRef) (io.ReadCloser, error)

	// Delete removes the stored object. `ErrNotFound` is acceptable and
	// should not be treated as a fatal error by callers.
	Delete(ctx context.Context, ref audit.SpillRef) error

	// Sweep removes objects older than `olderThan` and returns the count
	// of objects deleted. Implementations may also enforce a total-size
	// cap during the sweep, evicting oldest first.
	Sweep(ctx context.Context, olderThan time.Time) (deleted int, err error)

	// Stat returns runtime metadata (used by admin UI / metrics).
	Stat(ctx context.Context) (Stats, error)

	// Backend returns the canonical backend name; included on every
	// `SpillRef.Backend` so the audit row records which backend it lives in.
	Backend() string
}
