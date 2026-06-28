// normalize_backfill_fill.go — the per-candidate fill half of the
// normalize backfill: resolving direction bytes, running the registry,
// the status-honest upsert, and the version-stamped skip ledger. Split
// from normalize_backfill.go, which keeps the job identity + scan.
package audit

import (
	"context"
	"github.com/goccy/go-json"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// backfillOne re-runs normalize for one candidate and upserts the sidecar.
// Each direction is independent: a request that normalizes cleanly while
// the response fails still updates the row with what we have.
func (j *NormalizeBackfill) backfillOne(ctx context.Context, c backfillCandidate) {
	// Resolve each direction's bytes: inline copy first, spilled object
	// otherwise. A failed fetch leaves the direction empty and the row,
	// when nothing else fills it, skip-marked with an honest reason.
	requestBytes, reqFetchFailed := j.resolveBytes(ctx, c.EventID, "request", c.InlineRequestBody, c.RequestSpillRef)
	responseBytes, respFetchFailed := j.resolveBytes(ctx, c.EventID, "response", c.InlineResponseBody, c.ResponseSpillRef)

	if len(requestBytes) == 0 && len(responseBytes) == 0 {
		reason := "spill_ref_only"
		if reqFetchFailed || respFetchFailed {
			reason = "spill_fetch_failed"
		}
		j.markSkip(ctx, c.EventID, reason)
		j.bumpSkipped(reason)
		return
	}

	requestNormalized, requestStatus, requestErr := j.normalizeDirection(
		ctx, requestBytes, c, normcore.DirectionRequest,
	)
	responseNormalized, responseStatus, responseErr := j.normalizeDirection(
		ctx, responseBytes, c, normcore.DirectionResponse,
	)

	if len(requestNormalized) == 0 && len(responseNormalized) == 0 {
		// Both directions failed or were absent — nothing useful to write.
		// Record the skip so the row is not re-scanned forever.
		j.markSkip(ctx, c.EventID, "no_payload_produced")
		j.bumpSkipped("no_payload_produced")
		return
	}

	// A direction this pass could not re-produce keeps its older payload
	// AND its older status/error: status describes the payload the
	// operator sees, so writing 'failed' over a surviving older 'ok'
	// payload would make the drawer banner contradict the rendered
	// content. Status/error therefore move only together with a new
	// payload (CASE on EXCLUDED.*_normalized). The row-level
	// normalize_version says "this row was last processed at the current
	// schema version"; each payload's inner normalizeVersion field
	// remains the authoritative per-direction stamp.
	const upsertSQL = `
INSERT INTO traffic_event_normalized (
    traffic_event_id,
    request_normalized, response_normalized,
    request_status, response_status,
    request_error_reason, response_error_reason,
    normalize_version
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (traffic_event_id) DO UPDATE SET
    request_normalized    = COALESCE(EXCLUDED.request_normalized,    traffic_event_normalized.request_normalized),
    response_normalized   = COALESCE(EXCLUDED.response_normalized,   traffic_event_normalized.response_normalized),
    request_status        = CASE WHEN EXCLUDED.request_normalized IS NOT NULL OR traffic_event_normalized.request_normalized IS NULL
                                 THEN COALESCE(EXCLUDED.request_status, traffic_event_normalized.request_status)
                                 ELSE traffic_event_normalized.request_status END,
    response_status       = CASE WHEN EXCLUDED.response_normalized IS NOT NULL OR traffic_event_normalized.response_normalized IS NULL
                                 THEN COALESCE(EXCLUDED.response_status, traffic_event_normalized.response_status)
                                 ELSE traffic_event_normalized.response_status END,
    request_error_reason  = CASE WHEN EXCLUDED.request_normalized IS NOT NULL OR traffic_event_normalized.request_normalized IS NULL
                                 THEN COALESCE(EXCLUDED.request_error_reason, traffic_event_normalized.request_error_reason)
                                 ELSE traffic_event_normalized.request_error_reason END,
    response_error_reason = CASE WHEN EXCLUDED.response_normalized IS NOT NULL OR traffic_event_normalized.response_normalized IS NULL
                                 THEN COALESCE(EXCLUDED.response_error_reason, traffic_event_normalized.response_error_reason)
                                 ELSE traffic_event_normalized.response_error_reason END,
    normalize_version     = EXCLUDED.normalize_version
`
	_, err := j.pool.Exec(ctx, upsertSQL,
		c.EventID,
		nilJSONIfEmpty(requestNormalized),
		nilJSONIfEmpty(responseNormalized),
		nilIfEmpty(requestStatus),
		nilIfEmpty(responseStatus),
		nilIfEmpty(requestErr),
		nilIfEmpty(responseErr),
		normcore.SchemaVersion,
	)
	if err != nil {
		j.bumpErr("upsert")
		j.logger.Warn("normalize_backfill upsert failed",
			"eventId", c.EventID,
			"error", err,
		)
		return
	}
	if j.filled != nil {
		j.filled.With().Inc()
	}
}

// markSkip records that an event could not be backfilled at the current
// schema version, so the scan excludes it until the next version bump
// (LEFT JOIN traffic_event_normalize_skip with the version-aware
// exclusion). The upsert refreshes reason + version on conflict so a
// version-N re-attempt that fails again re-arms the exclusion for
// version N. A write failure is logged, not fatal: the worst case is
// the row is retried next tick — no data is lost.
func (j *NormalizeBackfill) markSkip(ctx context.Context, eventID, reason string) {
	const skipSQL = `
INSERT INTO traffic_event_normalize_skip (traffic_event_id, reason, normalize_version)
VALUES ($1, $2, $3)
ON CONFLICT (traffic_event_id) DO UPDATE SET
    reason            = EXCLUDED.reason,
    normalize_version = EXCLUDED.normalize_version,
    attempted_at      = now()
`
	if _, err := j.pool.Exec(ctx, skipSQL, eventID, reason, normcore.SchemaVersion); err != nil {
		j.bumpErr("skip_mark")
		j.logger.Warn("normalize_backfill skip-mark failed",
			"eventId", eventID,
			"reason", reason,
			"error", err,
		)
	}
}

// normalizeDirection runs the registry against the given raw bytes for one
// direction. Returns marshalled JSON of the NormalizedPayload, the status
// string ("ok" / "partial" / "failed"), and an error-reason string for the
// non-ok statuses. Empty raw input returns all-empty (caller skips writing).
func (j *NormalizeBackfill) normalizeDirection(
	ctx context.Context,
	raw []byte,
	c backfillCandidate,
	direction normcore.Direction,
) (marshalled []byte, status, errReason string) {
	if len(raw) == 0 {
		return nil, "", ""
	}
	contentType := ""
	if direction == normcore.DirectionRequest && c.RequestContentType != nil {
		contentType = *c.RequestContentType
	}
	if direction == normcore.DirectionResponse && c.ResponseContentType != nil {
		contentType = *c.ResponseContentType
	}
	meta := normcore.Meta{
		AdapterType:  c.AdapterType,
		Model:        c.Model,
		ContentType:  contentType,
		Direction:    direction,
		EndpointPath: c.Path,
	}
	payload, err := j.registry.Normalize(ctx, raw, meta)
	if err != nil {
		// "failed" — no usable payload.
		return nil, "failed", err.Error()
	}
	out, mErr := json.Marshal(payload)
	if mErr != nil {
		j.bumpErr("marshal")
		return nil, "failed", mErr.Error()
	}
	return out, "ok", ""
}

func (j *NormalizeBackfill) bumpErr(phase string) {
	if j.errors != nil {
		j.errors.With(phase).Inc()
	}
}
func (j *NormalizeBackfill) bumpSkipped(reason string) {
	if j.skipped != nil {
		j.skipped.With(reason).Inc()
	}
}
