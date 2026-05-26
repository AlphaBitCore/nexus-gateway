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

// Config is the operator-facing configuration for the spill subsystem.
// Lives in `system_metadata['spill_store.config']`. Each data-plane service
// reads it at startup and watches the same shadow key for live changes.
//
// Example payload:
//
//	{
//	  "backend": "localfs",
//	  "localfs": {
//	    "root": "/var/lib/nexus/spill",
//	    "max_size_gb": 50,
//	    "retention_days": 30
//	  }
//	}
type Config struct {
	Backend string `json:"backend"`
	Localfs struct {
		Root          string `json:"root,omitempty"`
		MaxSizeGB     int    `json:"max_size_gb,omitempty"`
		RetentionDays int    `json:"retention_days,omitempty"`
	} `json:"localfs"`
}
