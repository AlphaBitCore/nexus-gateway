package analytics

import (
	"context"
	"log/slog"
)

// resolveDimensionLabels batch-fetches the display label for a set of
// dimension values and returns a map id → label.
//
// Rollup dimension values are stable identifiers (UUIDs or opaque IDs),
// chosen so historical buckets survive renames in the source row. The
// analytics handler calls this at response time to attach a fresh display
// name (read from the source table now, not at rollup time) so dashboards
// show the entity's current name. IDs with no matching row map to an empty
// string in the returned map; the caller decides whether to fall back to
// the raw ID or omit the label.
//
// Special-case rollup values that are not real entity IDs (for example
// `routing_rule=passthrough-fallback`, the synthetic fallback rule ID)
// are passed through unchanged because no source row exists to translate.
func (h *Handler) resolveDimensionLabels(ctx context.Context, dimName string, ids []string) map[string]string {
	if len(ids) == 0 || h == nil || h.pool == nil {
		return nil
	}
	switch dimName {
	case "provider", "routed_provider":
		return h.fetchLabelsBy(ctx, `SELECT id, COALESCE(NULLIF("displayName", ''), name) FROM "Provider" WHERE id = ANY($1)`, ids)
	case "model":
		return h.fetchLabelsBy(ctx, `SELECT id, name FROM "Model" WHERE id = ANY($1)`, ids)
	case "organization":
		return h.fetchLabelsBy(ctx, `SELECT id, name FROM "Organization" WHERE id = ANY($1)`, ids)
	case "project":
		return h.fetchLabelsBy(ctx, `SELECT id, name FROM "Project" WHERE id = ANY($1)`, ids)
	case "user":
		return h.fetchLabelsBy(ctx, `SELECT id, "displayName" FROM "NexusUser" WHERE id = ANY($1)`, ids)
	case "virtual_key":
		return h.fetchLabelsBy(ctx, `SELECT id, name FROM "VirtualKey" WHERE id = ANY($1)`, ids)
	case "routing_rule":
		// "passthrough-fallback" is a synthetic rule ID, not a UUID.
		// fetchLabelsBy returns nothing for it; the caller falls back
		// to the raw ID, which is itself the human-readable label.
		return h.fetchLabelsBy(ctx, `SELECT id, name FROM "RoutingRule" WHERE id = ANY($1)`, ids)
	case "entity", "device", "target_host":
		// entity is the un-typed catch-all (already covered by user/
		// project/device sub-dims); device IDs in this codebase are
		// already the hostname (display-friendly); target_host is the
		// upstream domain. None of these need translation.
		return nil
	}
	return nil
}

// fetchProviderAdapterTypes maps provider UUIDs to their adapter_type value.
func (h *Handler) fetchProviderAdapterTypes(ctx context.Context, ids []string) map[string]string {
	if len(ids) == 0 {
		return nil
	}
	return h.fetchLabelsBy(ctx, `SELECT id, COALESCE(adapter_type, '') FROM "Provider" WHERE id = ANY($1)`, ids)
}

// fetchLabelsBy runs a `SELECT id, label FROM ... WHERE id = ANY($1)` query
// and returns the resulting id → label map. SQL errors are logged and
// degraded to an empty map so an unavailable analytics translation never
// blocks the data response.
func (h *Handler) fetchLabelsBy(ctx context.Context, sql string, ids []string) map[string]string {
	rows, err := h.pool.Query(ctx, sql, ids)
	if err != nil {
		slog.Default().Warn("analytics: dimension label lookup failed", "err", err)
		return nil
	}
	defer rows.Close()
	out := make(map[string]string, len(ids))
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			slog.Default().Warn("analytics: dimension label scan failed", "err", err)
			continue
		}
		out[id] = label
	}
	return out
}
