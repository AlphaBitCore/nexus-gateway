// Package handler — observability_retention.go: GET + PUT
// /api/admin/observability/retention.
//
// Eleven layers (per spec §5.5) live in metric_ops_retention_config and are
// re-read by the Hub ops-retention job on every tick. The PUT here is purely
// a config write; the next retention job sweep applies the new horizons.
package observability

import (
	"net/http"
	"sort"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/opsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterObservabilityRetentionRoutes wires GET + PUT for the retention
// admin surface.
func (h *Handler) RegisterObservabilityRetentionRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/observability/retention", h.GetRetention, iamMW(iam.ResourceObservability.Action(iam.VerbRead)))
	g.PUT("/observability/retention", h.PutRetention, iamMW(iam.ResourceObservability.Action(iam.VerbWrite)))
}

// retentionRange is the [min, max] day window for a given layer per spec §5.5.
type retentionRange struct {
	Min int
	Max int
}

// retentionLayerRanges enforces the spec table. Any PUT value outside its
// layer's range is rejected with 400 BEFORE any DB write — keeping the
// metric_ops_retention_config table in a known-good state.
var retentionLayerRanges = map[string]retentionRange{
	"runtime_raw":  {Min: 1, Max: 30},
	"business_raw": {Min: 1, Max: 30},
	"runtime_1h":   {Min: 30, Max: 365},
	"business_1h":  {Min: 30, Max: 365},
	"runtime_1d":   {Min: 90, Max: 1095},
	"business_1d":  {Min: 90, Max: 1095},
	"runtime_1mo":  {Min: 365, Max: 3650},
	"business_1mo": {Min: 365, Max: 3650},
	"diag_warn":    {Min: 7, Max: 90},
	"diag_error":   {Min: 30, Max: 730},
	"diag_fatal":   {Min: 90, Max: 1825},
}

// retentionLayer is one row in the GET response.
type retentionLayer struct {
	Value     int       `json:"value"`
	Min       int       `json:"min"`
	Max       int       `json:"max"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// GetRetention returns every retention layer with its current value and
// allowed range.
func (h *Handler) GetRetention(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	rows, err := h.ops.ListRetentionConfig(c.Request().Context())
	if err != nil {
		h.logger.Error("list_retention_config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read retention config", "server_error", "INTERNAL_ERROR"))
	}
	out := make(map[string]retentionLayer, len(retentionLayerRanges))
	for _, r := range rows {
		rng, ok := retentionLayerRanges[r.Layer]
		if !ok {
			// Forward-compat: if a future layer lands in DB without an entry
			// here, expose its current value with zero-valued bounds rather
			// than dropping it silently.
			rng = retentionRange{}
		}
		out[r.Layer] = retentionLayer{
			Value:     r.RetentionDays,
			Min:       rng.Min,
			Max:       rng.Max,
			UpdatedAt: r.UpdatedAt,
		}
	}
	// Stable layer ordering for the GET response so the UI list is
	// deterministic across calls.
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]retentionLayer, len(out))
	for _, k := range keys {
		ordered[k] = out[k]
	}
	return c.JSON(http.StatusOK, map[string]any{"retention": ordered})
}

// PutRetention atomically updates one or more layers. Body schema:
//
//	{ "runtime_raw": 7, "business_raw": 30, ... }
//
// Unknown layer keys are rejected with 400. Out-of-range values are rejected
// with 400. The whole payload is applied in a single transaction.
func (h *Handler) PutRetention(c echo.Context) error {
	if h.ops == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Database is not configured", "server_error", "DB_UNAVAILABLE"))
	}
	var req map[string]int
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("invalid request body", "validation_error", "VALIDATION_ERROR"))
	}
	if len(req) == 0 {
		return c.JSON(http.StatusBadRequest, errJSON("at least one layer is required", "validation_error", "VALIDATION_ERROR"))
	}
	if err := validateRetentionUpdates(req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", "VALIDATION_ERROR"))
	}

	actor := actorFromContext(c)
	before, _ := h.ops.ListRetentionConfig(c.Request().Context())
	if err := h.ops.UpdateRetentionConfig(c.Request().Context(), req, actor.UserID); err != nil {
		h.logger.Error("update_retention_config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update retention config", "server_error", "INTERNAL_ERROR"))
	}

	ae := audit.EntryFor(c, iam.ResourceObservability, iam.VerbWrite)
	ae.BeforeState = retentionRowsToMap(before)
	ae.AfterState = req
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"ok": true, "updated": len(req)})
}

// validateRetentionUpdates returns a non-nil error when any key is unknown or
// any value falls outside its layer's allowed range. The error message names
// the offending key so the UI can surface a precise validation message.
func validateRetentionUpdates(updates map[string]int) error {
	for layer, days := range updates {
		rng, ok := retentionLayerRanges[layer]
		if !ok {
			return &validationError{msg: "unknown retention layer: " + layer}
		}
		if days < rng.Min || days > rng.Max {
			return &validationError{msg: "value for " + layer + " out of range"}
		}
	}
	return nil
}

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

// retentionRowsToMap flattens the store rows into a {layer: days} map for
// the audit log so the BeforeState payload stays small and diff-friendly.
func retentionRowsToMap(rows []opsstore.RetentionEntry) map[string]int {
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.Layer] = r.RetentionDays
	}
	return out
}
