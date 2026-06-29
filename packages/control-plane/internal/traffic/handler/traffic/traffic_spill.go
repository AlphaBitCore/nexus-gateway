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
// tier; it never propagates an error to the endpoint.
func (h *Handler) fillSpilledBodies(ctx context.Context, id string, in *trafficstore.NormalizeInput) {
	if h.spillStore == nil {
		return
	}
	if len(in.RequestBody) == 0 && len(in.RequestSpillRef) > 0 {
		if body, err := h.rawSpillBody(ctx, in.RequestSpillRef); err == nil {
			in.RequestBody = body
		} else if h.logger != nil {
			h.logger.Warn("spill body fetch failed (request normalize)", "trafficEventId", id, "error", err)
		}
	}
	if len(in.ResponseBody) == 0 && len(in.ResponseSpillRef) > 0 {
		if body, err := h.rawSpillBody(ctx, in.ResponseSpillRef); err == nil {
			in.ResponseBody = body
		} else if h.logger != nil {
			h.logger.Warn("spill body fetch failed (response normalize)", "trafficEventId", id, "error", err)
		}
	}
}

// rawSpillBody fetches the RAW spilled bytes for a body that was stored
// out-of-band, for use as a view-time normalize input. Unlike resolveSpillBody
// (which renders for the UI by wrapping non-JSON content as a JSON string), this
// returns the captured bytes verbatim so the normalizer receives exactly what an
// inline body would have carried — SSE frames stay SSE, never a quoted JSON
// string. The sha256 integrity gate is preserved: a tampered blob is refused so
// fabricated content can never be normalized and presented as authentic.
func (h *Handler) rawSpillBody(ctx context.Context, refJSON []byte) ([]byte, error) {
	var ref sharedaudit.SpillRef
	if err := json.Unmarshal(refJSON, &ref); err != nil {
		return nil, fmt.Errorf("decode spill_ref: %w", err)
	}
	rc, err := h.spillStore.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read spill body: %w", err)
	}
	if ref.SHA256 != "" {
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != strings.ToLower(ref.SHA256) {
			return nil, fmt.Errorf("spill body integrity check failed (sha256 %s != recorded %s): blob may have been tampered with", got, ref.SHA256)
		}
	}
	return body, nil
}
