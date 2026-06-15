package wiring

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
	auditevent "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/spilluploader"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	localfsspill "github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/localfs"
)

// SpillRoot is the agent's local spill directory for oversize captured
// bodies. It is the single source of truth for the path so the write tier
// (tlsbump's BridgeDeps.SpillStore) and the drain read-back open the same
// root — a drift here would silently strand every spilled body.
func SpillRoot() string {
	return filepath.Join(paths.DefaultPaths().StateDir, "spill")
}

// NewLocalSpillStore opens the localfs spill store at SpillRoot with at-rest
// AES-256-GCM encryption (the agent spills decrypted request/response bodies —
// they must not sit plaintext on disk, matching the SQLCipher protection on the
// inline-body path). A non-nil error means oversize bodies cannot be stored
// locally (they truncate inline instead); callers log and continue — capture is
// best-effort, never blocking. The key is derived once per call from the
// keystore DB key, so the two write sites (Linux/Windows + macOS) and the drain
// reader all open the store with the same key.
func NewLocalSpillStore(ks keystore.Store) (*localfsspill.Store, error) {
	key, err := spillEncryptionKey(ks)
	if err != nil {
		// No key → refuse to write plaintext bodies to disk. Oversize bodies
		// truncate inline instead; small bodies still ride the SQLCipher DB.
		return nil, err
	}
	return localfsspill.New(localfsspill.Options{Root: SpillRoot(), EncryptionKey: key})
}

// spillEncryptionKey derives the spill store's at-rest key from the agent's
// SQLCipher DB key, domain-separated (SHA-256 over key‖label) so the spill
// files and the audit DB never share the exact key while both stay bound to the
// same device-held secret.
// The keystore is injected (composition root passes the platform store;
// tests pass keystore.NewMemoryStore()) — never constructed here.
func spillEncryptionKey(ks keystore.Store) ([]byte, error) {
	dbKey, err := keystore.GetOrCreateDBKey(ks)
	if err != nil {
		return nil, fmt.Errorf("spill encryption key: %w", err)
	}
	material := append(append([]byte{}, dbKey...), []byte("nexus-agent-spill-v1")...)
	sum := sha256.Sum256(material)
	return sum[:], nil
}

// LocalSpillReader reads spilled body bytes back from the local spill store.
// *localfs.Store satisfies it; tests inject a stub.
type LocalSpillReader interface {
	Get(ctx context.Context, ref sharedaudit.SpillRef) (io.ReadCloser, error)
}

// SpillS3Uploader uploads body bytes to S3 via the Hub presign flow and
// returns the resulting S3 SpillRef. *spilluploader.Uploader satisfies it.
type SpillS3Uploader interface {
	Upload(ctx context.Context, eventID, direction, contentType string, body []byte) (sharedaudit.SpillRef, error)
}

// InitSpillTransport builds the spill uploader (Hub presign → S3) and the
// local spill reader the audit drain uses to read each oversize body back
// from local disk before uploading it to S3. A nil reader (store unavailable)
// is fail-open: metadata still ships, oversize bodies stay local-only.
func InitSpillTransport(hubClient *hub.Client, ks keystore.Store, logger *slog.Logger) (*spilluploader.Uploader, LocalSpillReader) {
	spillUploader := InitSpillUploader(hubClient)
	var spillReader LocalSpillReader
	if store, spillErr := NewLocalSpillStore(ks); spillErr != nil {
		logger.Warn("audit drain: local spill store unavailable; oversize bodies will not upload", "error", spillErr)
	} else {
		spillReader = store
	}
	return spillUploader, spillReader
}

// HydrateLocalSpill reads oversize bodies back from the LOCAL spill store into
// the event's inline body fields so the agent UI detail drawer can render them
// directly off local disk — the agent never needs to fetch its own bodies from
// S3. Only localfs refs are hydrated; an S3 ref (a body already uploaded to S3
// at drain time) is left ref-only because the agent has no S3 GET credential —
// the UI shows a "stored in spill, view in Control Plane" affordance for those.
// No-op when reader is nil or the event carries no localfs ref.
func HydrateLocalSpill(ev *auditevent.Event, reader LocalSpillReader) {
	if ev == nil || reader == nil {
		return
	}
	if b := readLocalSpill(ev.RequestSpillRef, reader); b != nil {
		ev.PayloadRequest = b
	}
	if b := readLocalSpill(ev.ResponseSpillRef, reader); b != nil {
		ev.PayloadResponse = b
	}
}

// readLocalSpill returns the body bytes for a localfs spill ref, or nil for a
// nil ref, a non-localfs (e.g. S3) ref, or any read error (best-effort: the
// detail view degrades to the ref-only affordance rather than failing).
func readLocalSpill(ref *sharedaudit.SpillRef, reader LocalSpillReader) []byte {
	if ref == nil || ref.Backend != localfsspill.BackendName {
		return nil
	}
	rc, err := reader.Get(context.Background(), *ref)
	if err != nil {
		return nil
	}
	defer rc.Close() //nolint:errcheck
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil
	}
	return b
}

// GateBodyUpload strips the captured body (inline bytes + spill ref) from the
// wire copy of each event for any direction the Hub payload_capture config
// disables. The body remains in the local store and the agent's own detail
// view — this governs only what is shipped to Hub. When both directions are
// allowed it is a no-op.
func GateBodyUpload(events []auditevent.Event, uploadReq, uploadResp bool) []auditevent.Event {
	if uploadReq && uploadResp {
		return events
	}
	for i := range events {
		if !uploadReq {
			events[i].PayloadRequest = nil
			events[i].RequestSpillRef = nil
		}
		if !uploadResp {
			events[i].PayloadResponse = nil
			events[i].ResponseSpillRef = nil
		}
	}
	return events
}

// UploadDrainSpills converts localfs spill refs on a drained batch to S3 refs:
// it reads each oversize body back from the local store and uploads it via the
// Hub presign flow, then swaps the in-memory wire ref. The SQLite rows are NOT
// touched — they keep their localfs ref so the agent's own detail view can read
// the body back without an S3 GET credential (which the agent lacks).
//
// Fail-open: a read or upload failure logs a WARN, drops the wire ref for that
// direction (Hub receives the event's metadata but no body — the body stays on
// local disk), and never fails the batch. Refs already on a non-localfs backend
// (e.g. a retry after a partially-uploaded batch) pass through untouched.
//
// Design decision — the drop is intentional and accepted, not a gap to "fix":
// the realistic trigger is an S3-specific outage (Hub reachable, S3 not), and
// dropping the body here keeps the property that the body is never lost (it
// remains on local disk and is viewable in the agent's own detail drawer) and
// the audit pipeline never stalls. Failing the whole batch instead would stall
// ALL audit upload and grow the local DB unbounded during an S3 outage;
// per-event retry to Hub would need a hot-path drain refactor for a rare edge.
// The trade-off is bounded: such a body is visible locally but not in the
// Control Plane until a later capture re-exercises the path.
//
// reader or uploader nil ⇒ no-op (returns events unchanged).
func UploadDrainSpills(
	ctx context.Context,
	events []auditevent.Event,
	reader LocalSpillReader,
	uploader SpillS3Uploader,
	logger *slog.Logger,
) []auditevent.Event {
	if reader == nil || uploader == nil {
		return events
	}
	if logger == nil {
		logger = slog.Default()
	}
	for i := range events {
		events[i].RequestSpillRef = uploadOneSpill(ctx, events[i].ID, "request", events[i].RequestSpillRef, reader, uploader, logger)
		events[i].ResponseSpillRef = uploadOneSpill(ctx, events[i].ID, "response", events[i].ResponseSpillRef, reader, uploader, logger)
	}
	return events
}

// uploadOneSpill handles one direction. Returns the ref to put on the wire:
// the new S3 ref on success, nil on any failure (body stays local), or the
// original ref unchanged when there is nothing to upload.
func uploadOneSpill(
	ctx context.Context,
	eventID, direction string,
	ref *sharedaudit.SpillRef,
	reader LocalSpillReader,
	uploader SpillS3Uploader,
	logger *slog.Logger,
) *sharedaudit.SpillRef {
	if ref == nil {
		return nil
	}
	// Only localfs refs need uploading; anything already on S3 (or a future
	// backend) is shippable as-is.
	if ref.Backend != localfsspill.BackendName {
		return ref
	}
	rc, err := reader.Get(ctx, *ref)
	if err != nil {
		logger.Warn("drain spill: read local body failed; body stays local, dropped from upload",
			"eventId", eventID, "direction", direction, "key", ref.Key, "error", err)
		return nil
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		logger.Warn("drain spill: read local body failed; body stays local, dropped from upload",
			"eventId", eventID, "direction", direction, "key", ref.Key, "error", err)
		return nil
	}
	s3ref, err := uploader.Upload(ctx, eventID, direction, ref.ContentType, body)
	if err != nil {
		logger.Warn("drain spill: S3 upload failed; body stays local, dropped from upload",
			"eventId", eventID, "direction", direction, "error", err)
		return nil
	}
	return &s3ref
}
