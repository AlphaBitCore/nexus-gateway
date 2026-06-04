package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/credstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// RegisterProviderTestRoutes registers provider connectivity test + health routes.
func (h *Handler) RegisterProviderTestRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.POST("/providers/test-connection", h.ProviderTestConnection, iamMW(iam.ResourceProvider.Action(iam.VerbRead)))
	g.POST("/providers/:id/test", h.ProviderTest, iamMW(iam.ResourceProvider.Action(iam.VerbRead)))
	g.GET("/provider-health", h.ListProviderHealth, iamMW(iam.ResourceProvider.Action(iam.VerbRead)))
}

// RegisterPricingRoutes registers model pricing CRUD routes.
func (h *Handler) RegisterPricingRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/pricing", h.ListPricing, iamMW(iam.ResourceModelPricing.Action(iam.VerbRead)))
	g.POST("/pricing", h.CreatePricing, iamMW(iam.ResourceModelPricing.Action(iam.VerbCreate)))
	g.DELETE("/pricing/:id", h.DeletePricing, iamMW(iam.ResourceModelPricing.Action(iam.VerbDelete)))
}

// ProviderTestConnection tests connectivity to a new (not-yet-saved) provider.
func (h *Handler) ProviderTestConnection(c echo.Context) error {
	var body struct {
		Name        string `json:"name"`
		AdapterType string `json:"adapterType"`
		BaseURL     string `json:"baseUrl"`
		APIKey      string `json:"apiKey"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Name == "" || body.BaseURL == "" || body.AdapterType == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name, adapterType, and baseUrl are required", "validation_error", ""))
	}
	if !IsValidAdapterType(body.AdapterType) {
		return c.JSON(http.StatusBadRequest, errJSON("adapterType must be one of "+strings.Join(ValidAdapterTypes, ", "), "validation_error", ""))
	}

	return h.forwardProviderTest(c, body.Name, body.AdapterType, body.BaseURL, body.APIKey)
}

// ProviderTest tests connectivity using the stored provider's first enabled credential.
func (h *Handler) ProviderTest(c echo.Context) error {
	providerID := c.Param("id")
	var body struct {
		CredentialID string `json:"credentialId"`
	}
	_ = c.Bind(&body)

	p, err := h.providers.GetProvider(c.Request().Context(), providerID)
	if err != nil || p == nil {
		return c.JSON(http.StatusNotFound, errJSON("Provider not found", "not_found", ""))
	}

	var apiKey string
	if body.CredentialID != "" {
		apiKey = h.decryptCredentialByID(c.Request().Context(), body.CredentialID)
	} else {
		apiKey = h.getFirstCredentialKey(c.Request().Context(), providerID)
	}

	return h.forwardProviderTest(c, p.Name, p.AdapterType, p.BaseURL, apiKey)
}

// decryptCredentialByID fetches the credential with the given ID and decrypts its key.
func (h *Handler) decryptCredentialByID(ctx context.Context, credID string) string {
	cred, err := h.creds.GetCredentialEncrypted(ctx, credID)
	if err != nil {
		h.logger.Warn("decryptCredentialByID: fetch failed", "credId", credID, "error", err)
		return ""
	}
	if cred == nil {
		return ""
	}
	return h.decryptCredential(ctx, cred.EncryptedKey, cred.EncryptionIV, cred.EncryptionTag, cred.EncryptionKeyID)
}

// getFirstCredentialKey returns the decrypted API key of the first enabled
// credential for the given provider.
func (h *Handler) getFirstCredentialKey(ctx context.Context, providerID string) string {
	enabled := true
	creds, _, err := h.creds.ListCredentials(ctx, credstore.CredentialListParams{
		ProviderID: providerID,
		Enabled:    &enabled,
		Limit:      1,
	})
	if err != nil || len(creds) == 0 {
		return ""
	}

	enc, err := h.creds.GetCredentialEncrypted(ctx, creds[0].ID)
	if err != nil || enc == nil {
		return ""
	}
	return h.decryptCredential(ctx, enc.EncryptedKey, enc.EncryptionIV, enc.EncryptionTag, enc.EncryptionKeyID)
}

// decryptCredential decrypts a credential's key using MultiVault or Vault.
func (h *Handler) decryptCredential(ctx context.Context, encKey, encIV, encTag, encKeyID string) string {
	if h.multiVault != nil {
		plaintext, err := h.multiVault.Decrypt(encKeyID, encKey, encIV, encTag)
		if err != nil {
			h.logger.Warn("decryptCredential: multi-vault decrypt failed", "keyId", encKeyID, "error", err)
			return ""
		}
		return plaintext
	}
	if h.vault != nil {
		plaintext, err := h.vault.Decrypt(encKey, encIV, encTag)
		if err != nil {
			h.logger.Warn("decryptCredential: vault decrypt failed", "error", err)
			return ""
		}
		return plaintext
	}
	return ""
}

// forwardProviderTest delegates provider connectivity testing to the AI Gateway.
func (h *Handler) forwardProviderTest(c echo.Context, providerName, adapterType, baseURL, apiKey string) error {
	gwURL := strings.TrimRight(h.proxy.AIGatewayURL, "/") + "/internal/provider-test"

	payload, _ := json.Marshal(map[string]string{
		"providerName": providerName,
		"adapterType":  adapterType,
		"baseUrl":      baseURL,
		"apiKey":       apiKey,
	})

	client := nexushttp.New(nexushttp.Config{
		Timeout:        15 * time.Second,
		Caller:         "cp-providers-provider-test",
		PropagateReqID: true,
	})
	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodPost, gwURL, strings.NewReader(string(payload)))
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{"success": false, "error": "Failed to build request: " + err.Error()})
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{
			"success": false,
			"error":   "AI Gateway unreachable: " + err.Error(),
		})
	}
	defer resp.Body.Close() //nolint:errcheck

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	c.Response().Header().Set("Content-Type", "application/json")
	c.Response().WriteHeader(resp.StatusCode)
	_, _ = c.Response().Write(bodyBytes)
	return nil
}

// ListProviderHealth returns provider health status from the DB.
func (h *Handler) ListProviderHealth(c echo.Context) error {
	data, err := h.providers.ListProviderHealth(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": data})
}

// ListPricing returns all model pricing entries.
func (h *Handler) ListPricing(c echo.Context) error {
	data, err := h.providers.ListModelPricing(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": data})
}

// CreatePricing creates a new model pricing entry.
func (h *Handler) CreatePricing(c echo.Context) error {
	var body struct {
		ModelID               string  `json:"modelId"`
		InputPricePerMillion  float64 `json:"inputPricePerMillion"`
		OutputPricePerMillion float64 `json:"outputPricePerMillion"`
	}
	if err := c.Bind(&body); err != nil || body.ModelID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("modelId is required", "validation_error", ""))
	}
	id, err := h.providers.CreateModelPricing(c.Request().Context(), body.ModelID, body.InputPricePerMillion, body.OutputPricePerMillion)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create pricing", "server_error", ""))
	}
	ae := audit.EntryFor(c, iam.ResourceModelPricing, iam.VerbCreate)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, map[string]any{"id": id, "modelId": body.ModelID})
}

// DeletePricing deletes a model pricing entry by ID.
func (h *Handler) DeletePricing(c echo.Context) error {
	id := c.Param("id")
	affected, err := h.providers.DeleteModelPricing(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete pricing", "server_error", ""))
	}
	if affected == 0 {
		return c.JSON(http.StatusNotFound, errJSON("Pricing not found", "not_found", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceModelPricing, iam.VerbDelete)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}
