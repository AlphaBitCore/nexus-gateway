// Package handler — admin_thing_stats.go: GET /api/admin/things/:id/stats.
// Reads thing_metric_rollup_* (per-Thing pre-aggregated metrics produced by
// Hub's ThingRollup5mJob + cascade). Source of truth for CP admin's per-Thing
// stats dashboards. Agent native UI does NOT call this — it reads its own
// local rollup via the agent's IPC bridge.
//
// Gating: when thing.type='agent' AND no rollup data exists for the lookback
// window, the handler infers Hub's enableAgentRollup toggle is OFF and
// returns enabled=false so the UI renders a "rollup disabled" banner instead
// of an empty chart. CP cannot read Hub's yaml directly; the presence/absence
// of rows is the de-facto signal.
package thingstats

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/thingstats/thingstore"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterAdminThingStatsRoutes wires the per-Thing stats endpoint.
// Gated by admin:read observability (same policy as fleet analytics).
func (h *Handler) RegisterAdminThingStatsRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/things/:id/stats", h.GetThingStats, iamMW(iam.ResourceObservability.Action(iam.VerbRead)))
}

// thingStatsResponse is the wire shape for /things/:id/stats.
type thingStatsResponse struct {
	ThingID               string          `json:"thingId"`
	ThingType             string          `json:"thingType"`
	ThingName             string          `json:"thingName,omitempty"`
	Enabled               bool            `json:"enabled"`
	RollupDisabledMessage string          `json:"rollupDisabledMessage,omitempty"`
	StartTime             time.Time       `json:"startTime"`
	EndTime               time.Time       `json:"endTime"`
	Granule               string          `json:"granule"`
	Rows                  []thingStatsRow `json:"rows"`
	// DisplayNames resolves ID-typed dimension values (provider UUID, model
	// UUID, organization UUID, etc.) to their human-readable names so the
	// UI breakdown tables can render "openai-prod" instead of a UUID.
	// Keys are the bare dim value (the part after `=` in dimensionKey);
	// values are the matching Provider.name / Model.name / etc. Missing
	// entries are fine — the UI falls back to the raw value.
	DisplayNames map[string]string `json:"displayNames,omitempty"`
}

// thingStatsRow is the wire shape per rollup row. ID + thing_id are omitted
// (redundant with the response envelope / Thing identity).
type thingStatsRow struct {
	BucketStart  time.Time `json:"bucketStart"`
	MetricName   string    `json:"metricName"`
	DimensionKey string    `json:"dimensionKey,omitempty"`
	SubDimension string    `json:"subDimension,omitempty"`
	Value        float64   `json:"value"`
	Metadata     any       `json:"metadata,omitempty"`
}

// GetThingStats returns rollup rows for one Thing over a time window.
//
// Query params:
//   - start, end: RFC3339 timestamps; default last 24h.
//   - metric: comma-separated metric names to filter; empty = all.
//   - dimension: dimension name (e.g. "model", "target_host"); empty = global rows only.
//   - subDimension: exact subDimension match (e.g. "source=agent"); empty = any.
func (h *Handler) GetThingStats(c echo.Context) error {
	if h.thing == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}

	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("thing id is required", "bad_request", "MISSING_THING_ID"))
	}

	ctx := c.Request().Context()
	thing, err := h.thing.GetThing(ctx, id)
	if err != nil {
		h.logger.Error("get thing for stats", "thingId", id, "err", err)
		return c.JSON(http.StatusInternalServerError, errJSON("failed to load thing", "server_error", "GET_THING_FAILED"))
	}
	if thing == nil {
		return c.JSON(http.StatusNotFound, errJSON("thing not found", "not_found", "THING_NOT_FOUND"))
	}

	now := time.Now().UTC()
	end := now
	if s := strings.TrimSpace(c.QueryParam("end")); s != "" {
		if t, ok := parseRFC3339Flexible(s); ok {
			end = t
		} else {
			return c.JSON(http.StatusBadRequest, errJSON("invalid end (need RFC3339)", "bad_request", "INVALID_END"))
		}
	}
	start := end.Add(-24 * time.Hour)
	if s := strings.TrimSpace(c.QueryParam("start")); s != "" {
		if t, ok := parseRFC3339Flexible(s); ok {
			start = t
		} else {
			return c.JSON(http.StatusBadRequest, errJSON("invalid start (need RFC3339)", "bad_request", "INVALID_START"))
		}
	}
	if !end.After(start) {
		return c.JSON(http.StatusBadRequest, errJSON("end must be after start", "bad_request", "INVALID_TIME_WINDOW"))
	}

	q := thingstore.ThingMetricsQuery{
		ThingID:      id,
		StartTime:    start,
		EndTime:      end,
		DimensionKey: strings.TrimSpace(c.QueryParam("dimension")),
		SubDimension: strings.TrimSpace(c.QueryParam("subDimension")),
	}
	if m := strings.TrimSpace(c.QueryParam("metric")); m != "" {
		for _, part := range strings.Split(m, ",") {
			if p := strings.TrimSpace(part); p != "" {
				q.Metrics = append(q.Metrics, p)
			}
		}
	}

	rows, err := h.thing.QueryThingRollup(ctx, q)
	if err != nil {
		h.logger.Error("query thing rollup", "thingId", id, "err", err)
		return c.JSON(http.StatusInternalServerError, errJSON("failed to query thing rollup", "server_error", "QUERY_THING_ROLLUP_FAILED"))
	}

	resp := thingStatsResponse{
		ThingID:      id,
		ThingType:    thing.Type,
		Enabled:      true,
		StartTime:    start,
		EndTime:      end,
		Granule:      string(metrics.SelectGranularity(start, end)),
		Rows:         convertThingRollupRows(rows),
		DisplayNames: h.resolveDimensionNames(ctx, rows),
	}
	if thing.Name != nil {
		resp.ThingName = *thing.Name
	}

	// Agent rollup is gated by Hub's enableAgentRollup yaml (default OFF). CP
	// cannot read Hub config, so we infer from data: an agent Thing with no
	// rollup rows across the user-requested window AND no rows in the last
	// hour means the rollup pipeline isn't producing for this agent — almost
	// always the toggle. The 1h secondary check guards against false-positives
	// on queries that happen to land on an idle window.
	if thing.Type == "agent" && len(rows) == 0 {
		hasRecent, _ := h.thing.ThingRollupHasAnyRecent(ctx, id, now.Add(-1*time.Hour), now)
		if !hasRecent {
			resp.Enabled = false
			resp.RollupDisabledMessage = "Per-agent rollup is not enabled on the Hub (enableAgentRollup=false). View detailed metrics on the agent's local UI."
		}
	}

	return c.JSON(http.StatusOK, resp)
}

// splitDimensionKey parses the `<name>=<value>` rollup dimensionKey form into
// its two parts. Empty / global dim rows return ok=false. Used by display-name
// resolution and would also be the right spot for any future per-dim slicing.
func splitDimensionKey(dk string) (name, value string, ok bool) {
	if dk == "" {
		return "", "", false
	}
	idx := strings.IndexByte(dk, '=')
	if idx <= 0 || idx == len(dk)-1 {
		return "", "", false
	}
	return dk[:idx], dk[idx+1:], true
}

// dimNameLookup describes one batch ID→name resolution against a Postgres
// table. All current targets use a `text`-typed `id` (Prisma's default cuid
// shape); if a future target switches to native uuid storage, extend this
// struct with a per-row cast and the query builder below.
type dimNameLookup struct {
	table   string
	idCol   string
	nameCol string
}

// dimLookups maps a rollup dimension name to the table + column that holds
// its display name. Entries omitted here render the raw value in the UI
// (target_host / hook_decision / source — those are already human strings).
// `entity` is intentionally NOT covered: rollups emit it with the raw entity
// ID regardless of entity_type (user / project / device), so the value can't
// be resolved from a single table. The typed dims (user / project / device)
// cover the same data with a knowable target.
var dimLookups = map[string]dimNameLookup{
	"provider":        {`"Provider"`, "id", "name"},
	"routed_provider": {`"Provider"`, "id", "name"},
	"model":           {`"Model"`, "id", "name"},
	"routed_model":    {`"Model"`, "id", "name"},
	"organization":    {`"Organization"`, "id", "name"},
	"project":         {`"Project"`, "id", "name"},
	"virtual_key":     {`"VirtualKey"`, "id", "name"},
	"routing_rule":    {`"RoutingRule"`, "id", "name"},
	"user":            {`"NexusUser"`, "id", `"displayName"`},
	// Agent devices live in the unified `thing` table (type='agent').
	// thing.id is text (e.g. "agent-desktop-3ed49bdf…"); name is operator-set.
	"device": {`thing`, "id", "name"},
}

// resolveDimensionNames scans the rollup rows for ID-typed dimension values
// and returns a `value → display name` map. Empty / nil-safe — returns nil
// when there's nothing to resolve so the JSON omits the field.
func (h *Handler) resolveDimensionNames(ctx context.Context, rows []metrics.ThingRollupRow) map[string]string {
	if len(rows) == 0 || h.pool == nil {
		return nil
	}
	byDim := map[string]map[string]struct{}{}
	for _, r := range rows {
		name, value, ok := splitDimensionKey(r.DimensionKey)
		if !ok {
			continue
		}
		if _, want := dimLookups[name]; !want {
			continue
		}
		if _, ok := byDim[name]; !ok {
			byDim[name] = map[string]struct{}{}
		}
		byDim[name][value] = struct{}{}
	}
	if len(byDim) == 0 {
		return nil
	}
	out := map[string]string{}
	for dimName, values := range byDim {
		spec := dimLookups[dimName]
		ids := make([]string, 0, len(values))
		for v := range values {
			ids = append(ids, v)
		}
		// One batch SELECT per dimension. Missed rows simply don't appear
		// in the map — the UI's renderer falls back to the raw value. All
		// current targets store `id` as text (Prisma cuid), so the cast on
		// the bind variable is text[].
		q := "SELECT " + spec.idCol + ", " + spec.nameCol + " FROM " + spec.table + " WHERE " + spec.idCol + " = ANY($1::text[])"
		dbRows, err := h.pool.Query(ctx, q, ids)
		if err != nil {
			// Logging at debug — a single missing table (e.g. a future dim
			// without an FK target) shouldn't break the whole endpoint.
			h.logger.Debug("resolve dim names: query failed", "dim", dimName, "err", err)
			continue
		}
		for dbRows.Next() {
			var id, name string
			if err := dbRows.Scan(&id, &name); err == nil {
				out[id] = name
			}
		}
		dbRows.Close()
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// convertThingRollupRows strips ThingID + ID from the wire output (redundant
// in the per-Thing envelope) and JSON-decodes Metadata once so the UI can
// render it directly without a second parse on the wire bytes.
func convertThingRollupRows(rows []metrics.ThingRollupRow) []thingStatsRow {
	out := make([]thingStatsRow, 0, len(rows))
	for _, r := range rows {
		row := thingStatsRow{
			BucketStart:  r.BucketStart,
			MetricName:   r.MetricName,
			DimensionKey: r.DimensionKey,
			SubDimension: r.SubDimension,
			Value:        r.Value,
		}
		if len(r.Metadata) > 0 {
			var anyMeta any
			if err := json.Unmarshal(r.Metadata, &anyMeta); err == nil {
				row.Metadata = anyMeta
			}
		}
		out = append(out, row)
	}
	return out
}
