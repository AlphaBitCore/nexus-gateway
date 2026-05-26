package settings

import (
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// GetObservability returns the persisted observability config from
// system_metadata["observability.config"].
func (h *Handler) GetObservability(c echo.Context) error {
	raw, _ := h.meta.GetSystemMetadata(c.Request().Context(), "observability.config")
	if raw == nil {
		return c.JSON(http.StatusOK, map[string]any{"enabled": false})
	}
	var cfg any
	_ = json.Unmarshal(raw, &cfg)
	return c.JSON(http.StatusOK, cfg)
}

// observabilityUpdateRequest is the typed body for PUT /settings/observability.
// All fields are optional — only provided fields are merged into the existing config.
type observabilityUpdateRequest struct {
	OtelEnabled    *bool    `json:"otelEnabled"`
	SamplingRate   *float64 `json:"samplingRate"`
	TraceViewerURL *string  `json:"traceViewerUrl"`
}

// UpdateObservability merges the supplied fields into the persisted
// observability config, saves it, fires Hub shadow invalidations, and
// records an audit entry.
func (h *Handler) UpdateObservability(c echo.Context) error {
	var body observabilityUpdateRequest
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	if body.SamplingRate != nil {
		if *body.SamplingRate < 0 || *body.SamplingRate > 1 {
			return c.JSON(http.StatusBadRequest, errJSON("samplingRate must be between 0 and 1", "validation_error", "samplingRate"))
		}
	}

	ctx := c.Request().Context()

	existing := make(map[string]any)
	raw, _ := h.meta.GetSystemMetadata(ctx, "observability.config")
	if raw != nil {
		_ = json.Unmarshal(raw, &existing)
	}

	if body.OtelEnabled != nil {
		existing["otelEnabled"] = *body.OtelEnabled
	}
	if body.SamplingRate != nil {
		existing["samplingRate"] = *body.SamplingRate
	}
	if body.TraceViewerURL != nil {
		existing["traceViewerUrl"] = *body.TraceViewerURL
	}

	aa := middleware.AdminAuthFromContext(c)
	updatedBy := ""
	if aa != nil {
		updatedBy = aa.KeyID
	}
	if err := h.meta.SetSystemMetadata(ctx, "observability.config", existing, updatedBy); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to save observability config", "server_error", ""))
	}

	if h.hub != nil {
		// Fan out the invalidation to every Thing type whose observability
		// receiver consumes system_metadata['observability.config'].
		// - ai-gateway, compliance-proxy: Type B receivers wired in their
		//   respective configdispatch packages.
		// - control-plane: self-consumes via the in-process observability
		//   handler.
		// - nexus-hub: Hub's selfshadow handler (wiring/self.go) re-reads
		//   the config via PG LISTEN/NOTIFY because Hub cannot WebSocket-
		//   push to itself; this Invalidate exists so the same admin write
		//   reaches the hub leg through the same fan-out path.
		// Agent is intentionally excluded: agent.observability is not a
		// registered config key.
		h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.Observability)
		h.hub.InvalidateConfig(ctx, "compliance-proxy", configkey.Observability)
		h.hub.InvalidateConfig(ctx, "control-plane", configkey.Observability)
		h.hub.InvalidateConfig(ctx, "nexus-hub", configkey.Observability)
	}

	// VerbWrite (not VerbUpdate) because the observability resource's
	// catalog declares [read write] — keep audit/route/IAM verbs aligned
	// (handler.go registers PUT under iam.VerbWrite).
	ae := audit.EntryFor(c, iam.ResourceObservability, iam.VerbWrite)
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, existing)
}
