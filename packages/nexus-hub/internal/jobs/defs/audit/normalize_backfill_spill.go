// normalize_backfill_spill.go — the spill-fetch half of the normalize
// backfill: resolving a candidate direction's bytes from the inline
// copy or the spilled object, bounded by spillReadCap. Split from
// normalize_backfill.go, which keeps the scan / upsert / skip logic.
package audit

import (
	"context"
	"encoding/json"
	"io"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// resolveBytes returns the raw bytes for one direction: the inline copy
// when present, else the spilled object fetched from the spill backend
// (bounded by spillReadCap). fetchFailed reports a spill ref that could
// not be resolved — absent store, backend error, or an over-cap object —
// so the caller can record an honest skip reason.
func (j *NormalizeBackfill) resolveBytes(ctx context.Context, eventID, direction string, inline, refJSON []byte) (body []byte, fetchFailed bool) {
	if len(inline) > 0 {
		return inline, false
	}
	if len(refJSON) == 0 {
		return nil, false
	}
	if j.spill == nil {
		return nil, false
	}
	var ref sharedaudit.SpillRef
	if err := json.Unmarshal(refJSON, &ref); err != nil {
		j.bumpErr("spill_ref_decode")
		j.logger.Warn("normalize_backfill spill ref decode failed",
			"eventId", eventID, "direction", direction, "error", err)
		return nil, true
	}
	rc, err := j.spill.Get(ctx, ref)
	if err != nil {
		j.bumpErr("spill_fetch")
		j.logger.Warn("normalize_backfill spill fetch failed",
			"eventId", eventID, "direction", direction, "error", err)
		return nil, true
	}
	defer rc.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(rc, spillReadCap+1))
	if err != nil {
		j.bumpErr("spill_read")
		j.logger.Warn("normalize_backfill spill read failed",
			"eventId", eventID, "direction", direction, "error", err)
		return nil, true
	}
	if int64(len(data)) > spillReadCap {
		j.bumpErr("spill_over_cap")
		j.logger.Warn("normalize_backfill spilled body exceeds read cap",
			"eventId", eventID, "direction", direction, "capBytes", int64(spillReadCap))
		return nil, true
	}
	return data, false
}
