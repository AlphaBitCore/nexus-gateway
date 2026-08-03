package traffic

import (
	"errors"
	"fmt"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/localfs"
)

// Spill-fetch failure causes — stable, greppable labels for the one thing an
// operator must decide when a spilled body will not load: is this body
// STRUCTURALLY unreachable from this node (nothing will ever make it readable
// here, the deployment is misconfigured), or merely unfetchable right now
// (retry, fix credentials, restore connectivity)?
//
// Every arm used to log the same generic line, so the two were
// indistinguishable. That mattered most in the case the labels exist for: on a
// multi-host deployment where each node runs its own localfs spill root,
// EVERY spilled body is permanently unreadable from the Control Plane, and the
// log said only "spill body fetch failed" — the same sentence an S3 hiccup
// produces. See spillstore-architecture.md section 3, which states the shared-root
// requirement the code never checked back.
const (
	// spillCauseNotFoundHostLocal — the ref names a host-local backend and the
	// object is not on THIS host. Structural: no retry fixes it.
	spillCauseNotFoundHostLocal = "not_found_host_local"
	// spillCauseNotFound — the object is genuinely absent from a shared backend.
	spillCauseNotFound = "not_found"
	// spillCauseBackendMismatch — the ref was written by a different backend than
	// the one this node is configured with.
	spillCauseBackendMismatch = "backend_mismatch"
	// spillCauseRefDecode — the row's spill_ref column is not a decodable SpillRef.
	spillCauseRefDecode = "ref_decode"
	// spillCauseIntegrity — the bytes are there but do not match the recorded
	// SHA-256, so they were refused rather than served.
	spillCauseIntegrity = "integrity"
	// spillCauseRead — the object opened but the read failed part-way.
	spillCauseRead = "read"
	// spillCauseTransport — the backend could not be reached.
	spillCauseTransport = "transport"
)

// spillFetchError carries the structural reason a spilled body could not be
// fetched, alongside the underlying error. It exists so the Control Plane's
// spill read sites emit one greppable `cause` plus an operator-facing remedy
// instead of several copies of a generic failure line.
type spillFetchError struct {
	cause        string
	remedy       string
	refBackend   string
	storeBackend string
	err          error
}

func (e *spillFetchError) Error() string { return fmt.Sprintf("%s: %v", e.cause, e.err) }

func (e *spillFetchError) Unwrap() error { return e.err }

// newSpillFetchError builds the diagnosis for an arm whose cause is known at
// the failure site (decode, integrity, read).
func newSpillFetchError(cause, remedy string, ref sharedaudit.SpillRef, storeBackend string, err error) *spillFetchError {
	return &spillFetchError{
		cause:        cause,
		remedy:       remedy,
		refBackend:   ref.Backend,
		storeBackend: storeBackend,
		err:          err,
	}
}

// classifySpillGet turns a SpillStore.Get failure into a cause label and the
// remedy an operator can act on.
//
// The backend comparison is made structurally, against the configured store's
// own Backend() name, rather than by matching on the error text each backend
// happens to produce. Both shipped backends reject a foreign ref before doing
// any I/O (localfs.Get / s3.Get), so a mismatch never surfaces as ErrNotFound
// and this ordering cannot mask a genuine not-found.
func classifySpillGet(storeBackend string, ref sharedaudit.SpillRef, err error) (cause, remedy string) {
	if ref.Backend != storeBackend {
		return spillCauseBackendMismatch, fmt.Sprintf(
			"this row was spilled to the %q backend but this node is configured with %q; "+
				"refs written before a backend change are not readable through the new backend.",
			ref.Backend, storeBackend)
	}
	if errors.Is(err, spillstore.ErrNotFound) {
		if ref.Backend == localfs.BackendName {
			return spillCauseNotFoundHostLocal,
				"a localfs spill ref is only readable on the host that wrote it, and this object is not " +
					"on this host. If the body was captured by a service on another host it is structurally " +
					"unreachable from here: a multi-host deployment needs a shared backend (s3) or the same " +
					"localfs root mounted on every node. Otherwise the object aged out of the retention sweep."
		}
		return spillCauseNotFound, fmt.Sprintf(
			"the object is absent from the %q backend: it aged out of the retention sweep, or an async "+
				"upload was queued and lost before it landed.", ref.Backend)
	}
	if errors.Is(err, spillstore.ErrIntegrity) {
		// The object was located AND read; only turning its bytes back into the
		// stored body failed. Falling through to "transport" here told an operator
		// to check connectivity for a blob whose contents are wrong — opposite
		// directions, and on a compliance product the wrong one is the reassuring
		// one. Matched on the sentinel rather than on the backend's error text,
		// for the same reason the backend comparison above is structural.
		return spillCauseIntegrity, fmt.Sprintf(
			"the object was found and read from the %q backend but could not be unsealed: the stored "+
				"bytes are corrupt, or the encryption key in use is not the one they were written with. "+
				"This is NOT a connectivity problem — do not retry it away. Preserve the object and "+
				"check key material and provenance before anything overwrites it.", ref.Backend)
	}
	return spillCauseTransport, fmt.Sprintf(
		"the %q backend could not be reached; check connectivity, credentials and permissions.", ref.Backend)
}

// logSpillFetchFailure emits the single canonical spill-read failure line. It
// never fails the request: a body that cannot be fetched degrades to an empty
// body, and only the log explains why.
//
// stage distinguishes the two readers ("view" renders the drawer, "normalize"
// feeds the view-time recompute) and direction is "request" or "response", so
// one grep on cause tells an operator whether the problem is a deployment
// shape or a transient fault.
func (h *Handler) logSpillFetchFailure(stage, direction, eventID string, err error) {
	if h.logger == nil {
		return
	}
	attrs := []any{"trafficEventId", eventID, "stage", stage, "direction", direction}
	var sfe *spillFetchError
	if errors.As(err, &sfe) {
		attrs = append(attrs,
			"cause", sfe.cause,
			"refBackend", sfe.refBackend,
			"storeBackend", sfe.storeBackend,
			"remedy", sfe.remedy,
		)
	}
	attrs = append(attrs, "error", err)
	h.logger.Warn("spill body fetch failed", attrs...)
}

// storeBackendName returns the configured backend name, or "" when no spill
// store is wired.
func (h *Handler) storeBackendName() string {
	if h.spillStore == nil {
		return ""
	}
	return h.spillStore.Backend()
}
