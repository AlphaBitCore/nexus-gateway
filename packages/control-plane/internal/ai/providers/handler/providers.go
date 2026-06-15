package providers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/credstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/modelstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/providerstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/crypto"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterProviderRoutes registers provider CRUD routes.
func (h *Handler) RegisterProviderRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/providers", h.ListProviders, iamMW(iam.ResourceProvider.Action(iam.VerbRead)))
	g.POST("/providers", h.CreateProvider, iamMW(iam.ResourceProvider.Action(iam.VerbCreate)))
	g.GET("/providers/:id", h.GetProvider, iamMW(iam.ResourceProvider.Action(iam.VerbRead)))
	g.PUT("/providers/:id", h.UpdateProvider, iamMW(iam.ResourceProvider.Action(iam.VerbUpdate)))
	g.DELETE("/providers/:id", h.DeleteProvider, iamMW(iam.ResourceProvider.Action(iam.VerbDelete)))
	g.GET("/providers/:id/models", h.ListProviderModels, iamMW(iam.ResourceModel.Action(iam.VerbRead)))
	g.POST("/providers/:id/models", h.AddProviderModel, iamMW(iam.ResourceModel.Action(iam.VerbCreate)))
	g.GET("/providers/:id/health", h.GetProviderHealth, iamMW(iam.ResourceProvider.Action(iam.VerbRead)))
	g.GET("/providers/:id/credentials", h.ListProviderCredentials, iamMW(iam.ResourceProvider.Action(iam.VerbRead)))
}

func (h *Handler) ListProviders(c echo.Context) error {
	pg := parsePagination(c)
	params := providerstore.ListParams{
		Q:      c.QueryParam("q"),
		Limit:  pg.Limit,
		Offset: pg.Offset,
	}
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		params.Enabled = &t
	} else if v == "false" {
		f := false
		params.Enabled = &f
	}

	providers, total, err := h.providers.ListProviders(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list providers", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	type listItem struct {
		ID          string  `json:"id"`
		Name        string  `json:"name"`
		DisplayName *string `json:"displayName"`
		Description *string `json:"description"`
		AdapterType string  `json:"adapterType"`
		BaseURL     string  `json:"baseUrl"`
		Region      *string `json:"region"`
		Enabled     bool    `json:"enabled"`
		ModelCount  *int    `json:"modelCount"`
		CreatedAt   any     `json:"createdAt"`
	}
	data := make([]listItem, 0, len(providers))
	for _, p := range providers {
		data = append(data, listItem{
			ID: p.ID, Name: p.Name, DisplayName: p.DisplayName,
			Description: p.Description, AdapterType: p.AdapterType, BaseURL: p.BaseURL,
			Region:  p.Region,
			Enabled: p.Enabled, ModelCount: p.ModelCount, CreatedAt: p.CreatedAt,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"data": data, "total": total})
}

func (h *Handler) GetProvider(c echo.Context) error {
	p, err := h.providers.GetProvider(c.Request().Context(), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if p == nil {
		return c.JSON(http.StatusNotFound, errJSON("Provider not found", "not_found", ""))
	}

	// Include models
	models, err := h.models.ListModelsByProvider(c.Request().Context(), p.ID)
	if err != nil {
		h.logger.Error("list provider models", "error", err)
	}
	result := map[string]any{
		"id": p.ID, "name": p.Name, "displayName": p.DisplayName,
		"description": p.Description, "adapterType": p.AdapterType, "baseUrl": p.BaseURL,
		"pathPrefix": p.PathPrefix, "apiVersion": p.APIVersion,
		"region":  p.Region,
		"enabled": p.Enabled, "createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt,
		"models": models,
	}
	if p.Headers != nil {
		var h any
		if json.Unmarshal(p.Headers, &h) == nil {
			result["headers"] = h
		}
	}
	return c.JSON(http.StatusOK, result)
}

// createProviderModelInput is the inline model shape the wizard submits
// alongside the provider.
type createProviderModelInput struct {
	// Code is the customer-facing identifier (e.g. "gpt-4o"). Globally unique.
	// Defaults to providerModelId when empty.
	Code                  string   `json:"code"`
	ProviderModelID       string   `json:"providerModelId"`
	Name                  string   `json:"name"`
	Description           string   `json:"description"`
	Type                  string   `json:"type"`
	Features              []string `json:"features"`
	InputPricePerMillion  *float64 `json:"inputPricePerMillion"`
	OutputPricePerMillion *float64 `json:"outputPricePerMillion"`
	MaxContextTokens      *int     `json:"maxContextTokens"`
	MaxOutputTokens       *int     `json:"maxOutputTokens"`
	Aliases               []string `json:"aliases"`
}

// createProviderCredentialInput is the inline credential — the wizard has
// already authenticated the admin, so we trust the apiKey and encrypt
// before calling the store.
type createProviderCredentialInput struct {
	Name          string `json:"name"`
	APIKey        string `json:"apiKey"`
	RotationState string `json:"rotationState"`
}

// CreateProvider creates a Provider, plus optionally its Models and one
// Credential, in a single atomic transaction. The wizard posts everything
// in one request; a duplicate name on Provider or a (providerId,
// providerModelId) collision on Model rolls back the entire create so the
// DB never ends up half-populated.
// conflictForUniqueViolation maps a Postgres 23505 unique-violation on the
// provider/model/credential tables to an accurate admin-API error. The create
// path can trip several distinct unique indexes — provider name, provider path
// prefix, the globally-unique credential name, the globally-unique model code,
// and the per-provider model natural key. Previously every non-model collision
// was reported as "provider name exists", which misdirected admins who had only
// reused a credential name or model code from an earlier provider (changing the
// provider name never cleared the error). The machine code lets the UI point at
// the offending field. providerName is interpolated into the name message when
// known (empty otherwise).
func conflictForUniqueViolation(pgErr *pgconn.PgError, providerName string) (message, code string) {
	switch pgErr.ConstraintName {
	case "Provider_name_key":
		if providerName != "" {
			return "A provider named '" + providerName + "' already exists", "PROVIDER_NAME_EXISTS"
		}
		return "A provider with that name already exists", "PROVIDER_NAME_EXISTS"
	case "Provider_pathPrefix_key":
		return "A provider with that URL path prefix already exists (the path prefix is derived from the provider name)", "PROVIDER_PATH_EXISTS"
	case "Credential_name_key":
		return "A credential with that name already exists — credential names are globally unique; choose a different credential.name", "CREDENTIAL_NAME_EXISTS"
	case "Model_code_key":
		return "A model with that code already exists — model codes are globally unique; choose a different code (or providerModelId)", "MODEL_CODE_EXISTS"
	case "Model_providerId_providerModelId_key":
		return "A model with that providerModelId is already registered under this provider", "MODEL_ALREADY_REGISTERED"
	default:
		return "A unique value in the request conflicts with an existing record", "CONFLICT"
	}
}

func (h *Handler) CreateProvider(c echo.Context) error {
	var body struct {
		Name        string                         `json:"name"`
		DisplayName string                         `json:"displayName"`
		Description string                         `json:"description"`
		BaseURL     string                         `json:"baseUrl"`
		AdapterType string                         `json:"adapterType"`
		APIVersion  string                         `json:"apiVersion"`
		Region      string                         `json:"region"`
		Enabled     *bool                          `json:"enabled"`
		Headers     json.RawMessage                `json:"headers"`
		Models      []createProviderModelInput     `json:"models"`
		Credential  *createProviderCredentialInput `json:"credential"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Name == "" || body.BaseURL == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name and baseUrl are required", "validation_error", ""))
	}
	if body.AdapterType == "" {
		return c.JSON(http.StatusBadRequest, errJSON("adapterType is required", "validation_error", ""))
	}
	if !IsValidAdapterType(body.AdapterType) {
		return c.JSON(http.StatusBadRequest, errJSON(
			"adapterType must be one of the supported wire formats",
			"validation_error", "ADAPTER_TYPE_INVALID"))
	}

	if body.DisplayName == "" {
		body.DisplayName = body.Name
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	var desc *string
	if body.Description != "" {
		desc = &body.Description
	}
	var region *string
	if body.Region != "" {
		region = &body.Region
	}
	var apiVersion *string
	if body.APIVersion != "" {
		apiVersion = &body.APIVersion
	}

	// Validate + translate each inline model into store params. Model.id
	// is auto-UUID server-side; the natural customer-facing key is
	// `code`, defaulted to providerModelId when the wizard doesn't send
	// it explicitly. The (providerId, providerModelId) unique index
	// still catches duplicate upstream registrations.
	modelParams := make([]modelstore.CreateModelParams, 0, len(body.Models))
	for _, m := range body.Models {
		if m.Name == "" || m.Type == "" || m.ProviderModelID == "" {
			return c.JSON(http.StatusBadRequest, errJSON(
				"models[].providerModelId, name, and type are required",
				"validation_error", ""))
		}
		code := m.Code
		if code == "" {
			code = m.ProviderModelID
		}
		var mDesc *string
		if m.Description != "" {
			mDesc = &m.Description
		}
		features := m.Features
		if features == nil {
			features = []string{}
		}
		aliases := m.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		modelParams = append(modelParams, modelstore.CreateModelParams{
			Code:                  code,
			Name:                  m.Name,
			Description:           mDesc,
			ProviderModelID:       m.ProviderModelID,
			Type:                  m.Type,
			Features:              features,
			InputPricePerMillion:  m.InputPricePerMillion,
			OutputPricePerMillion: m.OutputPricePerMillion,
			MaxContextTokens:      m.MaxContextTokens,
			MaxOutputTokens:       m.MaxOutputTokens,
			Aliases:               aliases,
			Enabled:               true,
		})
	}

	// Encrypt the credential key outside the tx — the vault call is the
	// expensive part and must not hold a DB lock. If the store insert
	// fails after encryption, the ciphertext is discarded with the rollback.
	var credParams *credstore.CreateCredentialParams
	var newProviderID string // app-side provider id when an inline credential needs AAD binding
	if body.Credential != nil && body.Credential.APIKey != "" {
		if h.multiVault == nil && h.vault == nil {
			return c.JSON(http.StatusServiceUnavailable, errJSON(
				"Credential vault not available — encryption key not configured",
				"server_error", "VAULT_UNAVAILABLE"))
		}
		if body.Credential.Name == "" {
			return c.JSON(http.StatusBadRequest, errJSON(
				"credential.name is required when credential.apiKey is provided",
				"validation_error", ""))
		}
		// Provider + credential are created atomically, so neither id
		// exists yet. Generate both app-side here so the credential ciphertext can
		// be AAD-bound to its own (credentialID, providerID) before the insert.
		newProviderID = uuid.New().String()
		credID := uuid.New().String()
		aad := keyderive.ProviderCredentialAAD(credID, newProviderID)

		var enc *crypto.EncryptResult
		var keyID string
		var encErr error
		if h.multiVault != nil {
			enc, keyID, encErr = h.multiVault.Encrypt(body.Credential.APIKey, aad)
		} else {
			enc, encErr = h.vault.Encrypt(body.Credential.APIKey, aad)
			keyID = "v1"
		}
		if encErr != nil {
			h.logger.Error("encrypt credential", "error", encErr)
			return c.JSON(http.StatusInternalServerError, errJSON(
				"Encryption failed", "server_error", "ENCRYPTION_ERROR"))
		}
		rotationState := body.Credential.RotationState
		if rotationState == "" {
			rotationState = "none"
		}
		credParams = &credstore.CreateCredentialParams{
			ID:              credID,
			Name:            body.Credential.Name,
			EncryptedKey:    enc.Ciphertext,
			EncryptionIV:    enc.IV,
			EncryptionTag:   enc.Tag,
			EncryptionKeyID: keyID,
			Enabled:         true,
			RotationState:   rotationState,
		}
	}

	p, insertedModels, insertedCred, err := h.providers.CreateProviderWithChildren(
		c.Request().Context(),
		providerstore.CreateParams{
			ID:          newProviderID, // Matches the AAD when an inline credential is sealed; empty → store generates
			Name:        body.Name,
			DisplayName: body.DisplayName,
			Description: desc,
			BaseURL:     body.BaseURL,
			PathPrefix:  "/" + body.Name,
			AdapterType: body.AdapterType,
			APIVersion:  apiVersion,
			Region:      region,
			Enabled:     enabled,
			Headers:     body.Headers,
		},
		modelParams,
		credParams,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Constraint names come from Prisma's default naming. Classify each
			// so the response names the actual offending field (provider name,
			// path prefix, credential name, model code, or model natural key)
			// rather than always blaming the provider name.
			msg, code := conflictForUniqueViolation(pgErr, body.Name)
			return c.JSON(http.StatusConflict, errJSON(msg, "conflict", code))
		}
		h.logger.Error("create provider", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	if h.hub != nil {
		ctx := c.Request().Context()
		keys := []string{"providers"}
		if len(body.Models) > 0 {
			keys = append(keys, "models")
		}
		if credParams != nil {
			keys = append(keys, "credentials")
		}
		for _, key := range keys {
			if err := h.hub.InvalidateConfigE(ctx, "ai-gateway", key); err != nil {
				h.logger.Error("create provider: hub invalidate failed", "id", p.ID, "configKey", key, "error", err)
				return hub.RespondPropagationFailure(c, err)
			}
		}
	}
	h.incrementConfigVersion(c.Request().Context())

	ae := audit.EntryFor(c, iam.ResourceProvider, iam.VerbCreate)
	ae.EntityID = p.ID
	ae.AfterState = map[string]any{
		"provider":   p,
		"modelCount": len(insertedModels),
		"credential": insertedCred != nil,
	}
	h.audit.LogObserved(c.Request().Context(), ae)

	resp := map[string]any{
		"id":          p.ID,
		"name":        p.Name,
		"displayName": p.DisplayName,
		"description": p.Description,
		"adapterType": p.AdapterType,
		"baseUrl":     p.BaseURL,
		"pathPrefix":  p.PathPrefix,
		"apiVersion":  p.APIVersion,
		"region":      p.Region,
		"enabled":     p.Enabled,
		"headers":     p.Headers,
		"createdAt":   p.CreatedAt,
		"updatedAt":   p.UpdatedAt,
		"models":      insertedModels,
	}
	if insertedCred != nil {
		resp["credential"] = insertedCred
	}
	return c.JSON(http.StatusCreated, resp)
}

func (h *Handler) UpdateProvider(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.providers.GetProvider(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Provider not found", "not_found", ""))
	}

	// Read the raw JSON body first so we can distinguish "region not
	// provided" (leave as-is) from "region: null" (clear) and "region:
	// <string>" (set). Stdlib bind would collapse the first two into the
	// same nil *string.
	raw, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	var body struct {
		Name        *string `json:"name"`
		DisplayName *string `json:"displayName"`
		Description *string `json:"description"`
		BaseURL     *string `json:"baseUrl"`
		AdapterType *string `json:"adapterType"`
		Enabled     *bool   `json:"enabled"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
		}
	}
	// Treat empty strings as "do not change" for optional string fields.
	if body.Name != nil && *body.Name == "" {
		body.Name = nil
	}
	if body.BaseURL != nil && *body.BaseURL == "" {
		body.BaseURL = nil
	}
	if body.AdapterType != nil {
		if *body.AdapterType == "" {
			body.AdapterType = nil
		} else if !IsValidAdapterType(*body.AdapterType) {
			return c.JSON(http.StatusBadRequest, errJSON(
				"adapterType must be one of the supported wire formats",
				"validation_error", "ADAPTER_TYPE_INVALID"))
		}
	}
	var regionParam **string
	var apiVersionParam **string
	var updateHeaders bool
	var headersVal json.RawMessage
	{
		var rawFields map[string]json.RawMessage
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rawFields); err == nil {
				if rv, ok := rawFields["region"]; ok {
					var s *string
					if err := json.Unmarshal(rv, &s); err == nil {
						regionParam = &s
					}
				}
				if rv, ok := rawFields["apiVersion"]; ok {
					var s *string
					if err := json.Unmarshal(rv, &s); err == nil {
						apiVersionParam = &s
					}
				}
				if rv, ok := rawFields["headers"]; ok {
					updateHeaders = true
					headersVal = rv
				}
			}
		}
	}

	updated, err := h.providers.UpdateProvider(c.Request().Context(), id, providerstore.UpdateParams{
		Name:          body.Name,
		DisplayName:   body.DisplayName,
		Description:   body.Description,
		BaseURL:       body.BaseURL,
		AdapterType:   body.AdapterType,
		Region:        regionParam,
		APIVersion:    apiVersionParam,
		UpdateHeaders: updateHeaders,
		Headers:       headersVal,
		Enabled:       body.Enabled,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			msg, code := conflictForUniqueViolation(pgErr, "")
			return c.JSON(http.StatusConflict, errJSON(msg, "conflict", code))
		}
		h.logger.Error("update provider", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "providers"); err != nil {
			h.logger.Error("provider mutation: hub invalidate failed", "id", c.Param("id"), "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}
	h.incrementConfigVersion(c.Request().Context())

	ae := audit.EntryFor(c, iam.ResourceProvider, iam.VerbUpdate)
	ae.EntityID = id
	ae.BeforeState = existing
	ae.AfterState = updated
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, updated)
}

func (h *Handler) DeleteProvider(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.providers.GetProvider(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Provider not found", "not_found", ""))
	}

	if err := h.providers.DeleteProvider(c.Request().Context(), id); err != nil {
		h.logger.Error("delete provider", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "providers"); err != nil {
			h.logger.Error("provider mutation: hub invalidate failed", "id", c.Param("id"), "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}
	h.incrementConfigVersion(c.Request().Context())

	ae := audit.EntryFor(c, iam.ResourceProvider, iam.VerbDelete)
	ae.EntityID = id
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) ListProviderModels(c echo.Context) error {
	models, err := h.models.ListModelsByProvider(c.Request().Context(), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": models})
}

func (h *Handler) AddProviderModel(c echo.Context) error {
	providerID := c.Param("id")
	p, err := h.providers.GetProvider(c.Request().Context(), providerID)
	if err != nil || p == nil {
		return c.JSON(http.StatusNotFound, errJSON("Provider not found", "not_found", ""))
	}

	var body struct {
		Code                            string          `json:"code"`
		Name                            string          `json:"name"`
		Description                     string          `json:"description"`
		ProviderModelID                 string          `json:"providerModelId"`
		Type                            string          `json:"type"`
		Features                        []string        `json:"features"`
		InputPricePerMillion            *float64        `json:"inputPricePerMillion"`
		OutputPricePerMillion           *float64        `json:"outputPricePerMillion"`
		CachedInputReadPricePerMillion  *float64        `json:"cachedInputReadPricePerMillion"`
		CachedInputWritePricePerMillion *float64        `json:"cachedInputWritePricePerMillion"`
		MaxContextTokens                *int            `json:"maxContextTokens"`
		MaxOutputTokens                 *int            `json:"maxOutputTokens"`
		Aliases                         []string        `json:"aliases"`
		CapabilityJson                  json.RawMessage `json:"capabilityJson"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	if body.Name == "" || body.Type == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name and type are required", "validation_error", ""))
	}
	if body.ProviderModelID == "" {
		body.ProviderModelID = body.Name
	}
	// `code` is the customer-facing identifier in `{"model":"..."}`.
	// Falls back to providerModelId when not supplied so callers that
	// hadn't yet started sending it explicitly stay working. The DB
	// id (UUID) is auto-generated by gen_random_uuid().
	if body.Code == "" {
		body.Code = body.ProviderModelID
	}

	var desc *string
	if body.Description != "" {
		desc = &body.Description
	}
	var capJSON *json.RawMessage
	if len(body.CapabilityJson) > 0 {
		capJSON = &body.CapabilityJson
	}

	m, err := h.models.CreateModel(c.Request().Context(), modelstore.CreateModelParams{
		Code: body.Code, Name: body.Name, Description: desc,
		ProviderID: providerID, ProviderModelID: body.ProviderModelID,
		Type: body.Type, Features: body.Features,
		InputPricePerMillion: body.InputPricePerMillion, OutputPricePerMillion: body.OutputPricePerMillion,
		CachedInputReadPricePerMillion:  body.CachedInputReadPricePerMillion,
		CachedInputWritePricePerMillion: body.CachedInputWritePricePerMillion,
		MaxContextTokens:                body.MaxContextTokens, MaxOutputTokens: body.MaxOutputTokens,
		Aliases: body.Aliases, CapabilityJson: capJSON, Enabled: true,
	})
	if err != nil {
		// 23505 = unique_violation: the per-provider model natural key
		// (@@unique([providerId, providerModelId])) or the globally-unique
		// model code (Model_code_key). Classify so the response names the
		// right field instead of always blaming providerModelId.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			msg, code := conflictForUniqueViolation(pgErr, "")
			return c.JSON(http.StatusConflict, errJSON(msg, "conflict", code))
		}
		h.logger.Error("create model", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	if h.hub != nil {
		if err := h.hub.InvalidateConfigE(c.Request().Context(), "ai-gateway", "providers"); err != nil {
			h.logger.Error("provider mutation: hub invalidate failed", "id", c.Param("id"), "error", err)
			return hub.RespondPropagationFailure(c, err)
		}
	}
	h.incrementConfigVersion(c.Request().Context())

	ae := audit.EntryFor(c, iam.ResourceModel, iam.VerbCreate)
	ae.EntityID = m.ID
	ae.AfterState = m
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, m)
}

// GetProviderHealth returns health data for a single provider.
func (h *Handler) GetProviderHealth(c echo.Context) error {
	id := c.Param("id")
	all, err := h.providers.ListProviderHealth(c.Request().Context())
	if err != nil {
		h.logger.Error("get provider health", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	for _, ph := range all {
		if ph.ProviderID == id {
			return c.JSON(http.StatusOK, ph)
		}
	}
	// No health record yet — return defaults
	return c.JSON(http.StatusOK, map[string]any{
		"providerId":   id,
		"status":       "unknown",
		"errorRate":    0,
		"avgLatencyMs": 0,
		"sampleCount":  0,
	})
}

// ListProviderCredentials returns credential metadata (id, name, enabled) for a provider.
// Secrets are never included in the response.
func (h *Handler) ListProviderCredentials(c echo.Context) error {
	creds, _, err := h.creds.ListCredentials(c.Request().Context(), credstore.CredentialListParams{
		ProviderID: c.Param("id"),
		Limit:      100,
	})
	if err != nil {
		h.logger.Error("list provider credentials", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	result := make([]map[string]any, len(creds))
	for i, cred := range creds {
		result[i] = map[string]any{
			"id":      cred.ID,
			"name":    cred.Name,
			"enabled": cred.Enabled,
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"data": result})
}
