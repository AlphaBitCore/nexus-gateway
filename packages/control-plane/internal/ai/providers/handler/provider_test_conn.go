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
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// RegisterProviderTestRoutes registers provider connectivity test + health routes.
//
// IAM: the two routes that decrypt a STORED credential before forwarding it to
// the gateway (`/providers/:id/test`) are gated on BOTH provider:read AND a
// credential-scoped verb (credential:probe) — chaining two iamMW middlewares
// applies AND semantics. This mirrors `/credentials/:id/probe` so a read-only
// provider viewer cannot trigger a credential decrypt with only provider:read.
//
// F-0369: `/providers/test-connection` probes a caller-supplied, not-yet-saved
// base URL. Pre-fix it was gated on provider:read and reflected the upstream
// status + raw transport error back to the caller — a blind-SSRF / internal-
// endpoint fingerprinting oracle available to a read-only viewer. It is now gated
// on the provider-config-write tier (provider:create), so only a caller who can
// already configure a provider (and thus set the base URL anyway) can run the
// probe. This closes the oracle while preserving the full error detail for
// legitimate provider admins, and matches the UI: the only surfaces that expose
// the draft test-connection button (the provider create wizard's StepCredential
// and ProviderForm) are themselves admin:provider.create-gated.
func (h *Handler) RegisterProviderTestRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.POST("/providers/test-connection", h.ProviderTestConnection, iamMW(iam.ResourceProvider.Action(iam.VerbCreate)))
	g.POST("/providers/:id/test", h.ProviderTest,
		iamMW(iam.ResourceProvider.Action(iam.VerbRead)),
		iamMW(iam.ResourceCredential.Action(iam.VerbProbe)))
	g.GET("/provider-health", h.ListProviderHealth, iamMW(iam.ResourceProvider.Action(iam.VerbRead)))
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
	return h.decryptCredential(ctx, cred.ID, cred.ProviderID, cred.EncryptedKey, cred.EncryptionIV, cred.EncryptionTag, cred.EncryptionKeyID)
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
	return h.decryptCredential(ctx, enc.ID, enc.ProviderID, enc.EncryptedKey, enc.EncryptionIV, enc.EncryptionTag, enc.EncryptionKeyID)
}

// decryptCredential decrypts a credential's key using MultiVault or Vault. The
// credential id + provider id form the AAD that binds the ciphertext
// to its own row — a blob swapped from another credential fails to decrypt here.
func (h *Handler) decryptCredential(ctx context.Context, credID, providerID, encKey, encIV, encTag, encKeyID string) string {
	aad := keyderive.ProviderCredentialAAD(credID, providerID)
	if h.multiVault != nil {
		plaintext, err := h.multiVault.Decrypt(encKeyID, encKey, encIV, encTag, aad)
		if err != nil {
			h.logger.Warn("decryptCredential: multi-vault decrypt failed", "keyId", encKeyID, "error", err)
			return ""
		}
		return plaintext
	}
	if h.vault != nil {
		plaintext, err := h.vault.Decrypt(encKey, encIV, encTag, aad)
		if err != nil {
			h.logger.Warn("decryptCredential: vault decrypt failed", "error", err)
			return ""
		}
		return plaintext
	}
	return ""
}

// forwardProviderTest delegates provider connectivity testing to the AI Gateway.
//
// Confidentiality note: the decrypted provider key is carried in the
// request body to the gateway's POST /internal/provider-test endpoint. Unlike
// the credential-probe path (which forwards only the credential ID and lets the
// gateway decrypt locally), provider-test cannot use ID-forwarding: the
// not-yet-saved test-connection case has no stored credential to reference, so a
// plaintext key in the body is structurally required. The /internal/* hop is
// INTERNAL_SERVICE_TOKEN-gated for authn; in production it MUST additionally run
// over TLS (service mesh or TLS-terminating ingress) so the key is never sent in
// cleartext on the wire. Responses carry only a hasAPIKey boolean, never the key.
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
	req.Header.Set("Authorization", "Bearer "+h.proxy.AIGatewayInternalToken)

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
