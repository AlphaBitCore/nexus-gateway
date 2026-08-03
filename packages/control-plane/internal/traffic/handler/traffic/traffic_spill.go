package traffic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/goccy/go-json"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
)

// fillSpilledBodies recovers, per direction, any body that was stored out-of-band
// so the view-time recompute sees the same RAW captured bytes it would have seen
// inline. It fetches the spilled bytes directly via rawSpillBody (NOT
// resolveSpillBody, which UI-wraps non-JSON / SSE content as a quoted JSON string
// — feeding that to the normalizer would mis-detect the codec for spilled SSE).
// A missing spill store, an absent ref, or any fetch / integrity failure leaves
// that direction empty so the caller degrades gracefully to the stored-sidecar
// tier; it never propagates an error to the endpoint. The reason is logged with
// a stable cause label — see spill_diag.go.
func (h *Handler) fillSpilledBodies(ctx context.Context, id string, in *trafficstore.NormalizeInput) {
	if h.spillStore == nil {
		return
	}
	if len(in.RequestBody) == 0 && len(in.RequestSpillRef) > 0 {
		if body, err := h.rawSpillBody(ctx, in.RequestSpillRef); err == nil {
			in.RequestBody = body
		} else {
			h.logSpillFetchFailure("normalize", "request", id, err)
		}
	}
	if len(in.ResponseBody) == 0 && len(in.ResponseSpillRef) > 0 {
		if body, err := h.rawSpillBody(ctx, in.ResponseSpillRef); err == nil {
			in.ResponseBody = body
		} else {
			h.logSpillFetchFailure("normalize", "response", id, err)
		}
	}
}

// fetchSpillBytes decodes a JSONB spill_ref, fetches the object from the wired
// SpillStore, and verifies the bytes against the SHA-256 recorded on the
// traffic_event when the body was spilled.
//
// Both spill readers go through it, so the integrity gate and the failure
// diagnosis exist once rather than once per reader. Every failure arm returns a
// *spillFetchError naming the structural cause, so the caller's log tells an
// operator whether the body is unreachable from this node by construction or
// merely unfetchable right now.
//
// The integrity gate is load-bearing: a mismatch means the at-rest blob was
// overwritten or tampered with, and refusing it is what stops a forged body
// being presented as the genuine captured request/response.
func (h *Handler) fetchSpillBytes(ctx context.Context, refJSON []byte) ([]byte, sharedaudit.SpillRef, error) {
	storeBackend := h.storeBackendName()
	var ref sharedaudit.SpillRef
	if err := json.Unmarshal(refJSON, &ref); err != nil {
		return nil, ref, newSpillFetchError(spillCauseRefDecode,
			"the traffic_event's spill_ref column did not decode as a SpillRef, so the body cannot be located.",
			ref, storeBackend, fmt.Errorf("decode spill_ref: %w", err))
	}
	rc, err := h.spillStore.Get(ctx, ref)
	if err != nil {
		cause, remedy := classifySpillGet(storeBackend, ref, err)
		return nil, ref, newSpillFetchError(cause, remedy, ref, storeBackend, err)
	}
	defer rc.Close() //nolint:errcheck
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, ref, newSpillFetchError(spillCauseRead,
			"the object opened but the read failed part-way; this is a backend or transport fault, not a missing object.",
			ref, storeBackend, fmt.Errorf("read spill body: %w", err))
	}
	if ref.SHA256 != "" {
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != strings.ToLower(ref.SHA256) {
			return nil, ref, newSpillFetchError(spillCauseIntegrity,
				"the fetched bytes do not match the SHA-256 recorded when the body was spilled; the blob was "+
					"refused rather than served, so a tampered or overwritten object is never presented as the "+
					"genuine capture.",
				ref, storeBackend,
				fmt.Errorf("spill body integrity check failed (sha256 %s != recorded %s): blob may have been tampered with", got, ref.SHA256))
		}
	}
	return body, ref, nil
}

// rawSpillBody fetches the RAW spilled bytes for a body that was stored
// out-of-band, for use as a view-time normalize input. Unlike resolveSpillBody
// (which renders for the UI by wrapping non-JSON content as a JSON string), this
// returns the captured bytes verbatim so the normalizer receives exactly what an
// inline body would have carried — SSE frames stay SSE, never a quoted JSON
// string.
func (h *Handler) rawSpillBody(ctx context.Context, refJSON []byte) ([]byte, error) {
	body, _, err := h.fetchSpillBytes(ctx, refJSON)
	if err != nil {
		return nil, err
	}
	return body, nil
}
