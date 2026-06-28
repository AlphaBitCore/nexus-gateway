package providers

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// RegisterProviderDiscoverRoutes registers the model-discovery route for
// not-yet-saved custom providers.
//
// IAM: gated on provider:create for the same SSRF-vector reason as
// test-connection — the endpoint dials a caller-supplied, not-yet-saved base
// URL and relays the upstream response (model list or error detail), which is
// a blind-SSRF / internal-endpoint fingerprinting oracle if exposed to
// read-only viewers. Only a caller already authorised to configure a provider
// (and thus set the base URL anyway) may run the probe.
func (h *Handler) RegisterProviderDiscoverRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.POST("/providers/discover-models", h.ProviderDiscoverModels,
		iamMW(iam.ResourceProvider.Action(iam.VerbCreate)))
}

// ProviderDiscoverModels fetches the upstream model list for a not-yet-saved
// custom provider via GET /v1/models, delegating to the AI Gateway internal
// endpoint. OpenAI / OpenAI-compatible adapters support full discovery; the
// gateway returns discovery_unsupported for wire formats that do not expose a
// standard model-listing endpoint. Mirrors the test-connection trust model:
// the not-yet-saved base URL + key are carried in the body to the internal
// endpoint, which is INTERNAL_SERVICE_TOKEN-gated and must run over TLS in
// production so the key is never sent in cleartext on the wire.
func (h *Handler) ProviderDiscoverModels(c echo.Context) error {
	var body struct {
		AdapterType string `json:"adapterType"`
		BaseURL     string `json:"baseUrl"`
		APIKey      string `json:"apiKey"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.AdapterType == "" || body.BaseURL == "" {
		return c.JSON(http.StatusBadRequest, errJSON("adapterType and baseUrl are required", "validation_error", ""))
	}
	if !IsValidAdapterType(body.AdapterType) {
		return c.JSON(http.StatusBadRequest, errJSON("adapterType must be one of "+strings.Join(ValidAdapterTypes, ", "), "validation_error", ""))
	}
	return h.forwardDiscoverModels(c, body.AdapterType, body.BaseURL, body.APIKey)
}

// forwardDiscoverModels delegates model discovery to the AI Gateway's internal
// endpoint POST /internal/provider-discover-models.
//
// Confidentiality note: the decrypted provider key is carried in the request
// body. The /internal/* hop is INTERNAL_SERVICE_TOKEN-gated for authn; in
// production it MUST additionally run over TLS (service mesh or
// TLS-terminating ingress) so the key is never sent in cleartext on the wire.
// Responses carry model metadata only — never the key. On transport failure
// the handler returns HTTP 200 with success:false + error detail so the caller
// can surface a user-readable message rather than receiving an opaque 5xx.
func (h *Handler) forwardDiscoverModels(c echo.Context, adapterType, baseURL, apiKey string) error {
	gwURL := strings.TrimRight(h.proxy.AIGatewayURL, "/") + "/internal/provider-discover-models"

	payload, _ := json.Marshal(map[string]string{
		"adapterType": adapterType,
		"baseUrl":     baseURL,
		"apiKey":      apiKey,
	})

	client := nexushttp.New(nexushttp.Config{
		Timeout:        15 * time.Second,
		Caller:         "cp-providers-discover",
		PropagateReqID: true,
	})
	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodPost, gwURL, strings.NewReader(string(payload)))
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{"success": false, "error": "Failed to build request: " + err.Error()})
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.proxy.AIGatewayInternalToken)

	resp, err := client.Do(req)
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{
			"success": false,
			"error":   "AI Gateway unreachable: " + err.Error(),
		})
	}
	defer resp.Body.Close() //nolint:errcheck

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB — matches the ai-gateway gw cap
	c.Response().Header().Set("Content-Type", "application/json")
	c.Response().WriteHeader(resp.StatusCode)
	_, _ = c.Response().Write(bodyBytes)
	return nil
}
