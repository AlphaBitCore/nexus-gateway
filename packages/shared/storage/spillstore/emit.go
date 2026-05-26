package spillstore

import (
	"bytes"
	"context"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// EmitBody is the producer-side helper every data-plane writer uses to
// decide between inline and spill. Pass the captured body bytes + meta;
// returns an audit.Body in the right shape:
//
//   - len(body) == 0  → audit.EmptyBody() (Kind=absent)
//   - store == nil OR len(body) < threshold → audit.NewInlineBody(...)
//   - else → store.Put(...) succeeds, returns audit.NewSpillBody(ref, ...)
//
// On a Put failure the function falls back to inline so the audit row
// never silently disappears; the failure is logged so operators can spot
// a misconfigured backend without losing data. This matches the project
// rule "audit must not silently drop rows".
func EmitBody(
	ctx context.Context,
	store SpillStore,
	threshold int64,
	body []byte,
	contentType string,
	eventID string,
	direction string,
	truncated bool,
	logger *slog.Logger,
) audit.Body {
	if len(body) == 0 {
		return audit.EmptyBody()
	}
	size := int64(len(body))

	// Below threshold OR no spill backend → inline.
	if store == nil || size < threshold {
		return audit.NewInlineBody(body, size, truncated, contentType)
	}

	// At/above threshold → spill.
	ref, err := store.Put(ctx, bytes.NewReader(body), size, PutOptions{
		EventID:     eventID,
		Direction:   direction,
		ContentType: contentType,
	})
	if err != nil {
		// Fall back to inline rather than drop the row. Operators see the
		// warning and can fix their backend config.
		if logger != nil {
			logger.Warn("spillstore Put failed, falling back to inline body",
				"backend", store.Backend(),
				"eventId", eventID,
				"direction", direction,
				"size", size,
				"error", err)
		}
		return audit.NewInlineBody(body, size, truncated, contentType)
	}
	return audit.NewSpillBody(&ref, size, truncated, contentType)
}
