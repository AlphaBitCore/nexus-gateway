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
//   - len(body) < threshold → audit.NewInlineBody(...)
//   - store == nil → inline, BOUNDED to threshold and marked truncated when the
//     body is larger (S-6: without a backend an oversize body used to be inlined
//     whole, which is what MaxInlineBodyBytes is named after preventing)
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

	// Below threshold → inline, unchanged: the body is already inside the bound
	// the caller set.
	if size < threshold {
		return audit.NewInlineBody(body, size, truncated, contentType)
	}

	// No spill backend configured → inline, but BOUNDED (finding S-6).
	//
	// This used to inline the WHOLE body, and it is the COMMON production shape
	// rather than a rare one: every *.config.yaml ships spill disabled, so an
	// oversize body was kept whole under a setting literally named
	// MaxInlineBodyBytes. A 10 MiB body stored inline under a 256 KiB "max inline"
	// value is both a name that lies and an unbounded memory path on the audit
	// row and the MQ message — the same failure S-1 removed from the failed-Put
	// arm, on the path that runs far more often.
	//
	// Bounded to `threshold` with Truncated set and SizeBytes still reporting the
	// REAL size, so the record says "this body was N bytes, here is the prefix we
	// kept" instead of quietly holding something nobody sized for.
	//
	// This DOES start truncating bodies that are stored whole today, which is why
	// it is loud rather than silent: Truncated is what tells a reader the row is a
	// prefix, and the CHANGELOG carries the behaviour note. An operator who needs
	// whole bodies configures a spill backend — which is exactly the setting this
	// path is the absence of.
	if store == nil {
		kept, keptTruncated := body, truncated
		if int64(len(kept)) > threshold {
			kept, keptTruncated = kept[:threshold], true
		}
		if keptTruncated && logger != nil {
			logger.Warn("audit body kept inline and TRUNCATED: no spill backend is configured",
				"eventId", eventID,
				"direction", direction,
				"sizeBytes", size,
				"keptBytes", len(kept),
				"threshold", threshold,
				"remedy", "configure a spill backend (spill.enabled) to store oversize bodies out of band; "+
					"without one, bodies at or above the inline threshold are recorded as a prefix",
			)
		}
		return audit.NewInlineBody(kept, size, keptTruncated, contentType)
	}

	// At/above threshold → spill.
	ref, err := store.Put(ctx, bytes.NewReader(body), size, PutOptions{
		EventID:     eventID,
		Direction:   direction,
		ContentType: contentType,
	})
	if err != nil {
		// Fall back to inline rather than drop the row — but BOUNDED (finding S-1).
		//
		// This used to return the whole body. That is the one shape the spill threshold exists
		// to prevent: a body large enough to spill, inlined into the audit row and the MQ
		// message. A 200 MiB payload that fails to spill would be published as a 200 MiB NATS
		// message, and NATS enforces max_payload — so the "don't drop the row" intent could
		// lose the row anyway, loudly, somewhere else, after the memory had already been spent
		// three times over (buffer, marshal, wire).
		//
		// The bound is `threshold` itself, which makes the fallback honest rather than
		// arbitrary: if the body cannot go out-of-band, keep at most what an inline body is
		// already allowed to carry, and mark it Truncated so the record says it is a PREFIX.
		// SizeBytes still reports the real size, so a reader can see how much is missing —
		// which is strictly more information than either dropping the row or silently
		// inlining a body nobody sized for.
		kept, keptTruncated := body, truncated
		if int64(len(kept)) > threshold {
			kept, keptTruncated = kept[:threshold], true
		}
		if logger != nil {
			logger.Warn("spillstore Put failed, falling back to a BOUNDED inline body",
				"backend", store.Backend(),
				"eventId", eventID,
				"direction", direction,
				"size", size,
				"keptBytes", len(kept),
				"truncated", keptTruncated,
				"error", err)
		}
		return audit.NewInlineBody(kept, size, keptTruncated, contentType)
	}
	return audit.NewSpillBody(&ref, size, truncated, contentType)
}
