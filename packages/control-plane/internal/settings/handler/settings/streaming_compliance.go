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

const (
	// streamingComplianceConfigKey is the system_metadata row that stores
	// the global StreamingPolicy default consumed by all three data planes.
	streamingComplianceConfigKey = "streaming_compliance.config"
)

// streamingComplianceResponse mirrors the on-disk JSON shape exactly
// (snake_case keys) so the data-plane shared/streaming/policy.LoadGlobalDefault
// reader can parse either the admin-written row or this response without
// translating field names.
//
// Warnings is admin-facing only (not persisted, not consumed by the
// data plane). Populated at response-build time from the current
// config — surfaces mode-specific gotchas that aren't obvious from the
// raw enum value (e.g. buffer_full_block silently drops Modify hook
// rewrites; chunked_async fail_close is audit-only not blocking).
// #115/R3 — paired with the
// nexus_streaming_modify_degraded_total{reason="buffer_mode"} counter
// the data-plane BufferPipeline emits.
type streamingComplianceResponse struct {
	DefaultMode         string   `json:"default_mode"`
	ChunkBytes          int      `json:"chunk_bytes"`
	HookTimeoutMs       int      `json:"hook_timeout_ms"`
	MaxBufferBytes      int      `json:"max_buffer_bytes"`
	FailBehavior        string   `json:"fail_behavior"`
	CaptureRequestBody  bool     `json:"capture_request_body"`
	CaptureResponseBody bool     `json:"capture_response_body"`
	RawSpillEnabled     bool     `json:"raw_body_spill_enabled"`
	Warnings            []string `json:"warnings,omitempty"`
}

// modeWarnings returns the human-readable advisories an admin should
// see for the chosen mode. Static rules — no DB lookup, no per-rule
// inspection. The dynamic "is any actual Modify hook configured?"
// check would couple this endpoint to HookConfig + add a query; we
// surface the rule unconditionally so admins reading the config see
// the constraint before they add a Modify hook.
func modeWarnings(mode string) []string {
	switch mode {
	case "buffer_full_block":
		return []string{
			"Modify decisions returned by response hooks are silently ignored under buffer_full_block mode (the original body replays unchanged). Use chunked_async if rewrite is required. Watch nexus_streaming_modify_degraded_total{reason=\"buffer_mode\"} for occurrences.",
		}
	default:
		return nil
	}
}

type streamingComplianceUpdateRequest struct {
	DefaultMode         *string `json:"default_mode"`
	ChunkBytes          *int    `json:"chunk_bytes"`
	HookTimeoutMs       *int    `json:"hook_timeout_ms"`
	MaxBufferBytes      *int    `json:"max_buffer_bytes"`
	FailBehavior        *string `json:"fail_behavior"`
	CaptureRequestBody  *bool   `json:"capture_request_body"`
	CaptureResponseBody *bool   `json:"capture_response_body"`
	RawSpillEnabled     *bool   `json:"raw_body_spill_enabled"`
}

// streamingComplianceDefaults returns the conservative baseline shipped to
// fresh deployments — passthrough + capture off + fail_open.
func streamingComplianceDefaults() streamingComplianceResponse {
	return streamingComplianceResponse{
		DefaultMode:    "passthrough",
		ChunkBytes:     8 * 1024,
		HookTimeoutMs:  2000,
		MaxBufferBytes: 64 * 1024 * 1024,
		FailBehavior:   "fail_open",
	}
}

func decodeStreamingCompliance(raw json.RawMessage) streamingComplianceResponse {
	out := streamingComplianceDefaults()
	if len(raw) == 0 {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	if out.ChunkBytes <= 0 {
		out.ChunkBytes = 8 * 1024
	}
	if out.HookTimeoutMs <= 0 {
		out.HookTimeoutMs = 2000
	}
	if out.MaxBufferBytes <= 0 {
		out.MaxBufferBytes = 64 * 1024 * 1024
	}
	return out
}

// validateStreamingMode returns false when the caller-supplied enum is
// outside the closed set the data plane accepts.
func validateStreamingMode(s string) bool {
	switch s {
	case "passthrough", "buffer_full_block", "chunked_async":
		return true
	}
	return false
}

// validateFailBehavior returns false when the caller-supplied enum is
// outside the closed set the data plane accepts.
func validateFailBehavior(s string) bool {
	switch s {
	case "fail_open", "fail_close":
		return true
	}
	return false
}

// GetStreamingComplianceConfig returns the global default StreamingPolicy.
// A missing row materialises into the conservative defaults.
func (h *Handler) GetStreamingComplianceConfig(c echo.Context) error {
	raw, _ := h.payloadCaptureMeta().GetSystemMetadata(c.Request().Context(), streamingComplianceConfigKey)
	resp := decodeStreamingCompliance(raw)
	resp.Warnings = modeWarnings(resp.DefaultMode)
	return c.JSON(http.StatusOK, resp)
}

// UpdateStreamingComplianceConfig merges supplied fields into the persisted
// config, validates enum choices, persists, fires shadow invalidations, and
// records an admin audit entry.
func (h *Handler) UpdateStreamingComplianceConfig(c echo.Context) error {
	var body streamingComplianceUpdateRequest
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.DefaultMode != nil && !validateStreamingMode(*body.DefaultMode) {
		return c.JSON(http.StatusBadRequest, errJSON("default_mode must be one of passthrough, buffer_full_block, chunked_async", "validation_error", ""))
	}
	if body.FailBehavior != nil && !validateFailBehavior(*body.FailBehavior) {
		return c.JSON(http.StatusBadRequest, errJSON("fail_behavior must be one of fail_open, fail_close", "validation_error", ""))
	}

	ctx := c.Request().Context()
	raw, _ := h.payloadCaptureMeta().GetSystemMetadata(ctx, streamingComplianceConfigKey)
	merged := decodeStreamingCompliance(raw)
	if body.DefaultMode != nil {
		merged.DefaultMode = *body.DefaultMode
	}
	if body.ChunkBytes != nil && *body.ChunkBytes >= 0 {
		merged.ChunkBytes = *body.ChunkBytes
	}
	if body.HookTimeoutMs != nil && *body.HookTimeoutMs >= 0 {
		merged.HookTimeoutMs = *body.HookTimeoutMs
	}
	if body.MaxBufferBytes != nil && *body.MaxBufferBytes >= 0 {
		merged.MaxBufferBytes = *body.MaxBufferBytes
	}
	if body.FailBehavior != nil {
		merged.FailBehavior = *body.FailBehavior
	}
	if body.CaptureRequestBody != nil {
		merged.CaptureRequestBody = *body.CaptureRequestBody
	}
	if body.CaptureResponseBody != nil {
		merged.CaptureResponseBody = *body.CaptureResponseBody
	}
	if body.RawSpillEnabled != nil {
		merged.RawSpillEnabled = *body.RawSpillEnabled
	}

	aa := middleware.AdminAuthFromContext(c)
	updatedBy := ""
	if aa != nil {
		updatedBy = aa.KeyID
	}
	if err := h.payloadCaptureMeta().SetSystemMetadata(ctx, streamingComplianceConfigKey, merged, updatedBy); err != nil {
		h.logger.Error("save streaming compliance config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to save streaming compliance config", "server_error", ""))
	}

	if h.hub != nil {
		h.hub.InvalidateConfig(ctx, "compliance-proxy", configkey.StreamingCompliance)
		h.hub.InvalidateConfig(ctx, "agent", configkey.StreamingCompliance)
	}

	ae := audit.EntryFor(c, iam.ResourceSettings, iam.VerbUpdate)
	ae.AfterState = merged
	h.audit.LogObserved(ctx, ae)

	merged.Warnings = modeWarnings(merged.DefaultMode)
	return c.JSON(http.StatusOK, merged)
}
