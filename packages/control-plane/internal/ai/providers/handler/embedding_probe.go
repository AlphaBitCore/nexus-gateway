package providers

// no streaming — embeddings are request/response only.

import (
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// RegisterEmbeddingProbeRoutes registers the embedding probe endpoint on
// the given Echo group.
//
// IAM: the probe decrypts a STORED credential before forwarding it to the
// gateway, so it is gated on BOTH provider:read AND credential:probe (two
// chained iamMW middlewares = AND semantics) — the same dual guard as
// /providers/:id/test and the credential-scoped sibling /credentials/:id/probe.
// A read-only provider viewer holding only provider:read can no longer trigger a
// credential decrypt.
func (h *Handler) RegisterEmbeddingProbeRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.POST("/providers/:id/embedding-probe", h.ProviderEmbeddingProbe,
		iamMW(iam.ResourceProvider.Action(iam.VerbRead)),
		iamMW(iam.ResourceCredential.Action(iam.VerbProbe)))
}

// ProviderEmbeddingProbe issues a synthetic embedding call to the
// provider's first enabled embedding model and returns the probe outcome.
//
// Response shape on success (HTTP 200):
//
//	{
//	  "ok": true,
//	  "providerId": "...",
//	  "modelId": "...",
//	  "modelName": "...",
//	  "dimension": 1536,
//	  "latencyMs": 142,
//	  "promptTokens": 3,
//	  "sampleEmbeddingFirst10": [0.1, ...]
//	}
//
// Response shape on error (HTTP 200 with ok=false, or 400 for bad input):
//
//	{ "ok": false, "error": "..." }
func (h *Handler) ProviderEmbeddingProbe(c echo.Context) error {
	providerID := c.Param("id")

	p, err := h.providers.GetProvider(c.Request().Context(), providerID)
	if err != nil || p == nil {
		return c.JSON(http.StatusNotFound, errJSON("Provider not found", "not_found", ""))
	}

	// Find the first enabled embedding model for this provider.
	models, err := h.models.ListModelsByProvider(c.Request().Context(), providerID)
	if err != nil {
		h.logger.Error("embedding probe: list models", "providerId", providerID, "error", err)
		return internalServerError(c, "Failed to list provider models")
	}
	var embModel *struct {
		ID               string
		Name             string
		ProviderModelID  string
		MaxContextTokens *int
	}
	for _, m := range models {
		// capture
		if m.Type == "embedding" && m.Enabled {
			embModel = &struct {
				ID               string
				Name             string
				ProviderModelID  string
				MaxContextTokens *int
			}{
				ID:               m.ID,
				Name:             m.Name,
				ProviderModelID:  m.ProviderModelID,
				MaxContextTokens: m.MaxContextTokens,
			}
			break
		}
	}
	if embModel == nil {
		return c.JSON(http.StatusBadRequest, errJSON(
			"No enabled embedding model found for this provider. "+
				"Add a model with type=embedding and enable it before probing.",
			"no_embedding_model",
			"",
		))
	}

	// Decrypt the first enabled credential.
	apiKey := h.getFirstCredentialKey(c.Request().Context(), providerID)

	// Probe-time dimension is intentionally 0 (skip vector-length
	// enforcement on the gateway side).  The probe DISCOVERS the
	// embedding dimension by calling /v1/embeddings and returning the
	// vector length in its response — the admin then saves that value
	// into semantic_cache_config.embedding_dimension.  The
	// Model row itself does not carry a dedicated dimension column;
	// dimension is a fleet-wide singleton property of the active
	// embedding model.
	const probeDimension = 0

	return h.forwardEmbeddingProbe(c, providerID, embModel.ID, embModel.Name,
		embModel.ProviderModelID, p.BaseURL, apiKey, probeDimension)
}

// forwardEmbeddingProbe forwards the probe to the AI Gateway internal
// endpoint POST /internal/embedding-probe and passes the response body
// back to the caller verbatim.
//
// Confidentiality note: the decrypted provider key rides in the request
// body to the gateway. The /internal/* hop is INTERNAL_SERVICE_TOKEN-gated for
// authn; in production it MUST additionally run over TLS so the key is never sent
// in cleartext. See forwardProviderTest for the full rationale (ID-forwarding is
// not used here because the gateway probe endpoint consumes a plaintext key).
func (h *Handler) forwardEmbeddingProbe(c echo.Context,
	providerID, modelID, modelName, providerModelID, baseURL, apiKey string,
	dimension int,
) error {
	gwURL := strings.TrimRight(h.proxy.AIGatewayURL, "/") + "/internal/embedding-probe"

	payload, _ := json.Marshal(map[string]any{
		"providerId":      providerID,
		"modelId":         modelID,
		"modelName":       modelName,
		"providerModelId": providerModelID,
		"baseUrl":         baseURL,
		"apiKey":          apiKey,
		"dimension":       dimension,
	})

	client := nexushttp.New(nexushttp.Config{
		Timeout:        30 * time.Second,
		Caller:         "cp-providers-embedding-probe",
		PropagateReqID: true,
	})
	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodPost, gwURL,
		strings.NewReader(string(payload)))
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{
			"ok":    false,
			"error": "Failed to build gateway request: " + err.Error(),
		})
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.proxy.AIGatewayInternalToken)

	resp, err := client.Do(req)
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{
			"ok":    false,
			"error": "AI Gateway unreachable: " + err.Error(),
		})
	}
	defer resp.Body.Close() //nolint:errcheck

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	c.Response().Header().Set("Content-Type", "application/json")
	c.Response().WriteHeader(resp.StatusCode)
	_, _ = c.Response().Write(bodyBytes)
	return nil
}
