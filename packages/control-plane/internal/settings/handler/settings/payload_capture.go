package settings

import (
	"github.com/goccy/go-json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

const (
	// payloadCaptureConfigKey is the system_metadata row that stores the
	// admin-editable payload capture config consumed by CP and AG.
	payloadCaptureConfigKey = "payload_capture.config"

	// payloadCaptureInlineCeiling is the server-side ceiling applied on
	// every write to maxInlineBodyBytes (the inline-vs-spill cutoff).
	// Larger admin inputs are clamped down to protect
	// traffic_event_payload JSONB storage.
	payloadCaptureInlineCeiling int64 = 10 * 1024 * 1024

	// payloadCaptureNetworkCapCeiling is the server-side ceiling applied
	// to maxRequestBytes and maxResponseBytes. 100 MiB is enough for
	// vision-model payloads and large non-streaming responses.
	payloadCaptureNetworkCapCeiling int64 = 100 * 1024 * 1024

	// payloadCaptureDefaultMaxInlineBodyBytes mirrors shared/payloadcapture's
	// 256 KiB default inline-vs-spill cutoff.
	payloadCaptureDefaultMaxInlineBodyBytes int64 = 256 * 1024

	// payloadCaptureDefaultMaxRequestBytes mirrors shared/payloadcapture's
	// 10 MiB default network read cap on inbound requests.
	payloadCaptureDefaultMaxRequestBytes int64 = 10 * 1024 * 1024

	// payloadCaptureDefaultMaxResponseBytes mirrors shared/payloadcapture's
	// 10 MiB default network read cap on upstream non-streaming responses.
	payloadCaptureDefaultMaxResponseBytes int64 = 10 * 1024 * 1024
)

// payloadCaptureMeta returns the effective metadata store, preferring the
// test override when one has been injected.
func (h *Handler) payloadCaptureMeta() payloadCaptureMetadataStore {
	if h.payloadCaptureMetaStore != nil {
		return h.payloadCaptureMetaStore
	}
	return h.meta
}

// payloadCaptureResponse is the API-facing shape of the config returned
// from GET and PUT. The field names match system_metadata JSON so data-plane
// loaders can read either the admin-written row or this response without translation.
type payloadCaptureResponse struct {
	StoreRequestBody   bool  `json:"storeRequestBody"`
	StoreResponseBody  bool  `json:"storeResponseBody"`
	MaxInlineBodyBytes int64 `json:"maxInlineBodyBytes"`
	MaxRequestBytes    int64 `json:"maxRequestBytes"`
	MaxResponseBytes   int64 `json:"maxResponseBytes"`
}

// payloadCaptureUpdateRequest is the PATCH-style body for PUT.
// All fields are optional — absent fields preserve the existing value.
type payloadCaptureUpdateRequest struct {
	StoreRequestBody   *bool  `json:"storeRequestBody"`
	StoreResponseBody  *bool  `json:"storeResponseBody"`
	MaxInlineBodyBytes *int64 `json:"maxInlineBodyBytes"`
	MaxRequestBytes    *int64 `json:"maxRequestBytes"`
	MaxResponseBytes   *int64 `json:"maxResponseBytes"`
}

// decodePayloadCaptureConfig unmarshals a stored JSON blob into the response
// shape, applying defaults when a field is missing or the blob is nil/empty.
func decodePayloadCaptureConfig(raw json.RawMessage) payloadCaptureResponse {
	resp := payloadCaptureResponse{
		MaxInlineBodyBytes: payloadCaptureDefaultMaxInlineBodyBytes,
		MaxRequestBytes:    payloadCaptureDefaultMaxRequestBytes,
		MaxResponseBytes:   payloadCaptureDefaultMaxResponseBytes,
	}
	if len(raw) == 0 {
		return resp
	}
	_ = json.Unmarshal(raw, &resp)
	if resp.MaxInlineBodyBytes <= 0 {
		resp.MaxInlineBodyBytes = payloadCaptureDefaultMaxInlineBodyBytes
	}
	if resp.MaxRequestBytes <= 0 {
		resp.MaxRequestBytes = payloadCaptureDefaultMaxRequestBytes
	}
	if resp.MaxResponseBytes <= 0 {
		resp.MaxResponseBytes = payloadCaptureDefaultMaxResponseBytes
	}
	return resp
}

// clampPayloadCaptureInlineCap forces the inline-cap server-side ceiling
// and the non-negative floor.
func clampPayloadCaptureInlineCap(input int64) int64 {
	if input < 0 {
		input = 0
	}
	if input > payloadCaptureInlineCeiling {
		input = payloadCaptureInlineCeiling
	}
	return input
}

// clampPayloadCaptureNetworkCap forces the network-cap server-side ceiling
// (100 MiB) and the non-negative floor for both maxRequestBytes and
// maxResponseBytes.
func clampPayloadCaptureNetworkCap(input int64) int64 {
	if input < 0 {
		input = 0
	}
	if input > payloadCaptureNetworkCapCeiling {
		input = payloadCaptureNetworkCapCeiling
	}
	return input
}

// GetPayloadCaptureConfig returns the currently persisted payload capture
// config, or the conservative defaults when the system_metadata row is absent.
func (h *Handler) GetPayloadCaptureConfig(c echo.Context) error {
	raw, _ := h.payloadCaptureMeta().GetSystemMetadata(c.Request().Context(), payloadCaptureConfigKey)
	return c.JSON(http.StatusOK, decodePayloadCaptureConfig(raw))
}

// UpdatePayloadCaptureConfig merges the supplied fields into the existing
// config, clamps the byte caps, persists the result, fires shadow
// invalidations, and records an admin audit entry.
func (h *Handler) UpdatePayloadCaptureConfig(c echo.Context) error {
	var body payloadCaptureUpdateRequest
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	ctx := c.Request().Context()

	raw, _ := h.payloadCaptureMeta().GetSystemMetadata(ctx, payloadCaptureConfigKey)
	merged := decodePayloadCaptureConfig(raw)
	if body.StoreRequestBody != nil {
		merged.StoreRequestBody = *body.StoreRequestBody
	}
	if body.StoreResponseBody != nil {
		merged.StoreResponseBody = *body.StoreResponseBody
	}
	if body.MaxInlineBodyBytes != nil {
		merged.MaxInlineBodyBytes = *body.MaxInlineBodyBytes
	}
	if body.MaxRequestBytes != nil {
		merged.MaxRequestBytes = *body.MaxRequestBytes
	}
	if body.MaxResponseBytes != nil {
		merged.MaxResponseBytes = *body.MaxResponseBytes
	}
	merged.MaxInlineBodyBytes = clampPayloadCaptureInlineCap(merged.MaxInlineBodyBytes)
	if merged.MaxInlineBodyBytes == 0 {
		merged.MaxInlineBodyBytes = payloadCaptureDefaultMaxInlineBodyBytes
	}
	merged.MaxRequestBytes = clampPayloadCaptureNetworkCap(merged.MaxRequestBytes)
	if merged.MaxRequestBytes == 0 {
		merged.MaxRequestBytes = payloadCaptureDefaultMaxRequestBytes
	}
	merged.MaxResponseBytes = clampPayloadCaptureNetworkCap(merged.MaxResponseBytes)
	if merged.MaxResponseBytes == 0 {
		merged.MaxResponseBytes = payloadCaptureDefaultMaxResponseBytes
	}

	aa := middleware.AdminAuthFromContext(c)
	updatedBy := ""
	if aa != nil {
		updatedBy = aa.KeyID
	}
	if err := h.payloadCaptureMeta().SetSystemMetadata(ctx, payloadCaptureConfigKey, merged, updatedBy); err != nil {
		h.logger.Error("save payload capture config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to save payload capture config", "server_error", ""))
	}

	if h.hub != nil {
		h.hub.InvalidateConfig(ctx, "compliance-proxy", configkey.PayloadCapture)
		h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.PayloadCapture)
		h.hub.InvalidateConfig(ctx, "agent", configkey.PayloadCapture)
	}

	ae := audit.EntryFor(c, iam.ResourceSettings, iam.VerbUpdate)
	ae.AfterState = merged
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, merged)
}
