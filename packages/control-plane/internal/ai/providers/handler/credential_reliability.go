package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// Credential reliability endpoints. Separated from credentials.go so the
// diagnostic + threshold surface stays self-contained.

// ProbeCredential proxies POST /api/admin/credentials/:id/probe to the
// AI Gateway's POST /internal/v1/credentials/:id/probe endpoint, which
// runs adapter.Probe with the decrypted key against the upstream provider
// and returns {ok, latencyMs, detail, error, providerName, …}. The CP
// layer adds IAM gating, audit, and proxies the body verbatim back to
// the browser.
//
// Timeouts: 5 s in the gateway by default, configurable per-request via
// {"timeoutSeconds": N}. The CP HTTP client allows 35 s to absorb the
// gateway-side cap plus network jitter.
func (h *Handler) ProbeCredential(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	cred, err := h.creds.GetCredential(ctx, id)
	if err != nil {
		h.logger.Error("probe: get credential", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get credential", "server_error", "INTERNAL_ERROR"))
	}
	if cred == nil {
		return c.JSON(http.StatusNotFound, errJSON("Credential not found", "not_found", "NOT_FOUND"))
	}

	// Forward the (optional) timeoutSeconds body verbatim.
	rawBody, _ := io.ReadAll(io.LimitReader(c.Request().Body, 8*1024))
	if len(rawBody) == 0 {
		rawBody = []byte("{}")
	}

	gwURL := strings.TrimRight(h.proxy.AIGatewayURL, "/") + "/internal/v1/credentials/" + id + "/probe"
	client := nexushttp.New(nexushttp.Config{
		Timeout:        35 * time.Second,
		Caller:         "cp-credential-probe",
		PropagateReqID: true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gwURL, strings.NewReader(string(rawBody)))
	if err != nil {
		return c.JSON(http.StatusBadGateway, errJSON("Failed to build probe request: "+err.Error(), "bad_gateway", "PROBE_BUILD"))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.proxy.AIGatewayInternalToken)

	resp, err := client.Do(req)
	if err != nil {
		h.logger.Warn("probe: gateway unreachable", "credentialId", id, "error", err)
		return c.JSON(http.StatusBadGateway, map[string]any{
			"ok":           false,
			"error":        "AI Gateway unreachable: " + err.Error(),
			"credentialId": id,
		})
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	// Audit the probe — capture only the outcome flag, not the raw key.
	ae := audit.EntryFor(c, iam.ResourceCredential, iam.VerbProbe)
	ae.EntityID = id
	var outcome struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	_ = json.Unmarshal(body, &outcome)
	ae.AfterState = map[string]any{"ok": outcome.OK, "error": outcome.Error}
	h.audit.LogObserved(ctx, ae)

	c.Response().Header().Set("Content-Type", "application/json")
	c.Response().WriteHeader(resp.StatusCode)
	_, _ = c.Response().Write(body)
	return nil
}

// UpdateCredentialReliabilityOverrides accepts a partial credstate.Thresholds
// JSON body and writes it to Credential.reliabilityOverrides. An empty
// body or "null" clears any prior override. Validation rejects bodies that
// would fall outside the documented invariants.
func (h *Handler) UpdateCredentialReliabilityOverrides(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	cred, err := h.creds.GetCredential(ctx, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to get credential", "server_error", "INTERNAL_ERROR"))
	}
	if cred == nil {
		return c.JSON(http.StatusNotFound, errJSON("Credential not found", "not_found", "NOT_FOUND"))
	}

	rawBody, _ := io.ReadAll(io.LimitReader(c.Request().Body, 8*1024))
	body := strings.TrimSpace(string(rawBody))

	var override *credstate.Thresholds
	if body != "" && body != "null" && body != "{}" {
		override = &credstate.Thresholds{}
		if err := json.Unmarshal([]byte(body), override); err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("Invalid JSON: "+err.Error(), "validation_error", "INVALID_BODY"))
		}
		// Defensive validation — admin may omit any field, but the
		// fields they DO set must be self-consistent.
		merged := credstate.DefaultThresholds.Merge(*override)
		if err := merged.Validate(); err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("Invalid override: "+err.Error(), "validation_error", "INVALID_OVERRIDE"))
		}
	}

	var rawJSON []byte
	if override != nil {
		rawJSON, _ = json.Marshal(override)
	}
	if err := h.creds.SetCredentialReliabilityOverrides(ctx, id, rawJSON); err != nil {
		h.logger.Error("set reliability overrides", "credentialId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to save overrides", "server_error", "INTERNAL_ERROR"))
	}

	// Tell the AI Gateway to invalidate its credentials cache so the new
	// override flows through on the next attempt without a restart. Fail
	// loud: this rides the credentials key, so a dropped push leaves the
	// gateway enforcing the old per-credential reliability override.
	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(ctx, "ai-gateway", "credentials"); err != nil {
			h.logger.Error("set reliability overrides: hub invalidate failed", "credentialId", id, "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceCredential, iam.VerbUpdate)
	ae.EntityID = id
	if override != nil {
		ae.AfterState = override
	} else {
		ae.AfterState = map[string]any{"cleared": true}
	}
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, map[string]any{
		"id":                   id,
		"reliabilityOverrides": override,
	})
}

// Global reliability thresholds — Settings page surface (gateway-wide).

const reliabilityConfigKey = "gateway.credential_reliability.config"

// RegisterReliabilitySettingsRoutes mounts the global threshold settings
// under /api/admin/settings/credential-reliability. The settings flow:
// CP writes the row in system_metadata, then asks Hub to invalidate the
// "credential_reliability" config key so Hub re-pushes to all AI Gateways
// over thingclient. Hub itself re-reads the row on every job tick.
//
// Despite living under /settings/* for URL grouping, the IAM gate is
// admin:credential.<verb>: the thresholds govern credential health (open/
// half-open/closed transitions), and provider-admins managing credentials
// must be able to tune them without inheriting the broader admin:settings
// scope (which also gates SSO/SAML/SIEM).
func (h *Handler) RegisterReliabilitySettingsRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/settings/credential-reliability", h.GetReliabilityConfig, iamMW(iam.ResourceCredential.Action(iam.VerbRead)))
	g.PUT("/settings/credential-reliability", h.UpdateReliabilityConfig, iamMW(iam.ResourceCredential.Action(iam.VerbUpdate)))
}

// GetReliabilityConfig returns the current effective thresholds. The
// response carries both the stored override (may be empty / nil) and the
// resolved values (defaults merged with override) so the UI can show
// "currently active" + "your override" side-by-side.
func (h *Handler) GetReliabilityConfig(c echo.Context) error {
	ctx := c.Request().Context()
	raw, err := h.meta.GetSystemMetadata(ctx, reliabilityConfigKey)
	if err != nil {
		h.logger.Error("read reliability config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read reliability config", "server_error", "INTERNAL_ERROR"))
	}
	stored := credstate.Thresholds{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &stored); err != nil {
			h.logger.Warn("stored reliability config invalid; reporting defaults", "error", err)
			stored = credstate.Thresholds{}
		}
	}
	effective := credstate.DefaultThresholds.Merge(stored)
	return c.JSON(http.StatusOK, map[string]any{
		"defaults":  credstate.DefaultThresholds,
		"override":  stored,
		"effective": effective,
	})
}

// UpdateReliabilityConfig accepts a full credstate.Thresholds body and
// writes it to system_metadata. Validation rejects values that violate
// the cross-field invariants (e.g. degraded >= healthy).
func (h *Handler) UpdateReliabilityConfig(c echo.Context) error {
	ctx := c.Request().Context()
	var body credstate.Thresholds
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body: "+err.Error(), "validation_error", "INVALID_BODY"))
	}
	if err := body.Validate(); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON(fmt.Sprintf("Invalid thresholds: %s", err.Error()), "validation_error", "INVALID_THRESHOLDS"))
	}
	var updatedBy string
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		updatedBy = aa.KeyID
	}
	if err := h.meta.SetSystemMetadata(ctx, reliabilityConfigKey, body, updatedBy); err != nil {
		h.logger.Error("write reliability config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to save reliability config", "server_error", "INTERNAL_ERROR"))
	}
	if h.hub != nil {
		// AI Gateway reloads on this config key.
		h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.CredentialReliability)
	}
	ae := audit.EntryFor(c, iam.ResourceCredential, iam.VerbUpdate)
	ae.AfterState = body
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, map[string]any{
		"override":  body,
		"effective": credstate.DefaultThresholds.Merge(body),
	})
}
