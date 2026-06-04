package models

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// ModelLookup reads model data from the database.
type ModelLookup interface {
	GetModel(ctx context.Context, id string) (*store.Model, error)
	GetModelByCode(ctx context.Context, idOrName string) (*store.Model, error)
	ListEnabledModels(ctx context.Context) ([]store.Model, error)
}

// VKAuthenticator authenticates virtual keys from HTTP requests.
type VKAuthenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (*vkauth.VKMeta, error)
}

// requireVK enforces virtual-key authentication for the model-catalog
// endpoints. The upstream provider /v1/models endpoints (OpenAI,
// Anthropic, Cohere, …) all reject unauthenticated callers, and the
// gateway must do the same: an anonymous caller previously received the
// full enabled-model catalog. On any auth failure this writes a 401 and
// returns ok=false; the caller must return immediately.
func requireVK(w http.ResponseWriter, r *http.Request, vkAuth VKAuthenticator) (*vkauth.VKMeta, bool) {
	if vkAuth == nil {
		writeJSONError(w, http.StatusInternalServerError, "authenticator not configured")
		return nil, false
	}
	vkMeta, err := vkAuth.Authenticate(r.Context(), r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "authentication required: provide a valid virtual key")
		return nil, false
	}
	return vkMeta, true
}

// ModelsHandler handles GET /v1/models, returning a model list in either
// OpenAI or Anthropic native shape depending on caller signaling.
//
// Authentication is mandatory: the caller must present a valid virtual
// key (parity with every upstream provider /v1/models endpoint). A
// missing/invalid/disabled/expired key yields 401.
//
// Schema selection: when the caller sends an `anthropic-version` request
// header (Claude Code, @anthropic-ai/sdk, anthropic-py all do), the
// response uses Anthropic's native /v1/models shape — `data[].{type,id,
// display_name,created_at,max_input_tokens,max_tokens}` plus top-level
// `first_id/last_id/has_more`. This matches what api.anthropic.com
// itself returns and is what Claude Code v2.1.129+ requires to surface
// gateway-served entries in its `/model` picker (earlier versions were
// lenient and accepted the OpenAI shape; v2.1.129 silently drops items
// missing `type:"model"` + `display_name`).
//
// Otherwise (no anthropic-version header) the response is the
// OpenAI-style `{object:"list", data:[{id,object,created,owned_by,...}]}`
// shape every OpenAI SDK expects.
//
// Both shapes carry Nexus extension fields (aliases, features, pricing,
// context window, lifecycle, modalities, capabilityJson) so a client can
// pick the right model locally without a second catalog round-trip;
// SDKs ignore unknown JSON keys.
//
// When the VK carries AllowedModels, the list is filtered accordingly in
// both shapes.
func ModelsHandler(models ModelLookup, vkAuth VKAuthenticator, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if models == nil {
			writeJSONError(w, http.StatusInternalServerError, "database not available")
			return
		}
		vkMeta, ok := requireVK(w, r, vkAuth)
		if !ok {
			return
		}

		modelList, err := models.ListEnabledModels(r.Context())
		if err != nil {
			logger.Error("list models failed", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to list models")
			return
		}

		filtered := make([]store.Model, 0, len(modelList))
		for _, m := range modelList {
			if len(vkMeta.AllowedModels) > 0 &&
				!routingcore.ModelMatchesAllowedRefs(m.ID, m.ProviderModelID, m.ProviderID, vkMeta.AllowedModels) {
				continue
			}
			filtered = append(filtered, m)
		}

		w.Header().Set("Content-Type", "application/json")
		var b []byte
		if r.Header.Get("anthropic-version") != "" {
			b, _ = json.Marshal(buildAnthropicModelsResponse(filtered))
		} else {
			b, _ = json.Marshal(buildOpenAIModelsResponse(filtered))
		}
		_, _ = w.Write(b)
	}
}

// modelPricing is the per-model configured pricing surfaced to clients so
// they can make cost-aware model choices. All amounts are USD per million
// tokens (the catalog's native unit). Cached-input fields are omitted when
// the model has no cache discount/surcharge configured.
type modelPricing struct {
	InputPerMillion            *float64 `json:"inputPerMillion,omitempty"`
	OutputPerMillion           *float64 `json:"outputPerMillion,omitempty"`
	CachedInputReadPerMillion  *float64 `json:"cachedInputReadPerMillion,omitempty"`
	CachedInputWritePerMillion *float64 `json:"cachedInputWritePerMillion,omitempty"`
	Currency                   string   `json:"currency,omitempty"`
	Unit                       string   `json:"unit,omitempty"`
}

// buildPricing returns the pricing block, or nil when the model has no
// price configured at all (so the `pricing` key is omitted entirely).
func buildPricing(m store.Model) *modelPricing {
	if m.InputPricePM == nil && m.OutputPricePM == nil &&
		m.CachedInputReadPricePM == nil && m.CachedInputWritePricePM == nil {
		return nil
	}
	return &modelPricing{
		InputPerMillion:            m.InputPricePM,
		OutputPerMillion:           m.OutputPricePM,
		CachedInputReadPerMillion:  m.CachedInputReadPricePM,
		CachedInputWritePerMillion: m.CachedInputWritePricePM,
		Currency:                   "USD",
		Unit:                       "per_million_tokens",
	}
}

type openAIModelEntry struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Object           string  `json:"object"`
	Created          int64   `json:"created"`
	OwnedBy          string  `json:"owned_by"`
	OwnerDisplayName *string `json:"owner_display_name,omitempty"`
	// ModelType + InputModalities + OutputModalities are Nexus extension
	// fields. OpenAI SDKs ignore unknown JSON fields; Nexus consumers
	// (Simulator UI, smoke harness, capability planners) rely on these
	// to dispatch chat vs embedding vs image vs audio without a second
	// catalog round-trip. ModelType is the canonical Model.type
	// ("chat" | "embedding" | "image" | "audio"); OutputModalities
	// mirrors Model.outputModalities (["text"] | ["embedding"] | ["image"]
	// | ["audio"]).
	ModelType        string   `json:"type,omitempty"`
	InputModalities  []string `json:"inputModalities,omitempty"`
	OutputModalities []string `json:"outputModalities,omitempty"`
	// CapabilityJson is the raw Model.capabilityJson blob (embedding
	// default/max dimensions, max batch size, etc.) surfaced verbatim so
	// Nexus consumers (smoke harness, simulator, capability planners) read
	// capabilities from the catalog instead of hardcoded fallbacks. Omitted
	// when the model has no capability data. OpenAI SDKs ignore unknown keys.
	CapabilityJson json.RawMessage `json:"capabilityJson,omitempty"`
	// Client-selection Nexus extension fields. Aliases lists the alternate
	// request strings that resolve to this model; Features lists the
	// capability flags (vision, function_calling, streaming, json_mode,
	// thinking, …); MaxContextTokens / MaxOutputTokens carry the context
	// window (the OpenAI shape has no native field for these); Lifecycle is
	// ga|preview|deprecated; Pricing is the configured per-token price.
	Aliases          []string      `json:"aliases,omitempty"`
	Features         []string      `json:"features,omitempty"`
	MaxContextTokens *int          `json:"maxContextTokens,omitempty"`
	MaxOutputTokens  *int          `json:"maxOutputTokens,omitempty"`
	Lifecycle        string        `json:"lifecycle,omitempty"`
	Pricing          *modelPricing `json:"pricing,omitempty"`
}

type anthropicModelEntry struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	CreatedAt      string `json:"created_at"`
	MaxInputTokens *int   `json:"max_input_tokens,omitempty"`
	MaxTokens      *int   `json:"max_tokens,omitempty"`
	// Nexus extension fields (see openAIModelEntry — same role here).
	// `Type` already carries the Anthropic-required "model" literal, so
	// the Nexus model-classification field lives under ModelType. The
	// context window already rides on the native max_input_tokens /
	// max_tokens fields above, so it is not duplicated here.
	ModelType        string        `json:"modelType,omitempty"`
	InputModalities  []string      `json:"inputModalities,omitempty"`
	OutputModalities []string      `json:"outputModalities,omitempty"`
	Aliases          []string      `json:"aliases,omitempty"`
	Features         []string      `json:"features,omitempty"`
	Lifecycle        string        `json:"lifecycle,omitempty"`
	Pricing          *modelPricing `json:"pricing,omitempty"`
}

// buildOpenAIModelEntry maps a catalog row to the OpenAI-shaped entry.
// Shared by the list and detail handlers so both stay in lockstep.
func buildOpenAIModelEntry(m store.Model, created int64) openAIModelEntry {
	// OpenAI's /v1/models documents `owned_by` as the human-readable
	// provider slug ("openai", "openai-internal", "system"), not an opaque
	// UUID. Use ProviderName (the operator-facing slug) here so SDK
	// consumers comparing owned_by == "openai" succeed and the Simulator UI
	// groups models under the readable provider label instead of a UUID
	// bucket. `name`, `owner_display_name` are Nexus extension fields
	// preserved for the simulator UI; OpenAI SDKs ignore unknown JSON keys.
	return openAIModelEntry{
		ID:               m.Code,
		Name:             m.Name,
		Object:           "model",
		Created:          created,
		OwnedBy:          m.ProviderName,
		OwnerDisplayName: m.ProviderDisplayName,
		ModelType:        m.Type,
		InputModalities:  m.InputModalities,
		OutputModalities: m.OutputModalities,
		CapabilityJson:   json.RawMessage(m.CapabilityJson),
		Aliases:          m.Aliases,
		Features:         m.Features,
		MaxContextTokens: m.MaxContextTokens,
		MaxOutputTokens:  m.MaxOutputTokens,
		Lifecycle:        m.Lifecycle,
		Pricing:          buildPricing(m),
	}
}

// buildAnthropicModelEntry maps a catalog row to the Anthropic-shaped
// entry. Shared by the list and detail handlers.
func buildAnthropicModelEntry(m store.Model, createdAt string) anthropicModelEntry {
	return anthropicModelEntry{
		Type:             "model",
		ID:               m.Code,
		DisplayName:      m.Name,
		CreatedAt:        createdAt,
		MaxInputTokens:   m.MaxContextTokens,
		MaxTokens:        m.MaxOutputTokens,
		ModelType:        m.Type,
		InputModalities:  m.InputModalities,
		OutputModalities: m.OutputModalities,
		Aliases:          m.Aliases,
		Features:         m.Features,
		Lifecycle:        m.Lifecycle,
		Pricing:          buildPricing(m),
	}
}

func buildOpenAIModelsResponse(rows []store.Model) map[string]any {
	now := time.Now().Unix()
	entries := make([]openAIModelEntry, 0, len(rows))
	for _, m := range rows {
		entries = append(entries, buildOpenAIModelEntry(m, now))
	}
	return map[string]any{"object": "list", "data": entries}
}

func buildAnthropicModelsResponse(rows []store.Model) map[string]any {
	createdAt := time.Now().UTC().Format(time.RFC3339)
	entries := make([]anthropicModelEntry, 0, len(rows))
	for _, m := range rows {
		entries = append(entries, buildAnthropicModelEntry(m, createdAt))
	}
	resp := map[string]any{
		"data":     entries,
		"has_more": false,
	}
	if n := len(entries); n > 0 {
		resp["first_id"] = entries[0].ID
		resp["last_id"] = entries[n-1].ID
	}
	return resp
}

// writeJSONError writes a JSON error response with the given status code.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"message":` + jsonString(message) + `}}`))
}

// jsonString produces a JSON-quoted string (no import needed — simple escaping).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ModelDetailHandler handles GET /v1/models/{model}, returning a single
// model in the same enriched shape as the list endpoint (kept in lockstep
// via the shared entry builders). Authentication is mandatory, and a VK
// scoped to specific models cannot read the detail of a model outside its
// AllowedModels — that case returns 404 (mirroring the list, which hides
// disallowed models) so the key cannot probe for models it may not use.
func ModelDetailHandler(models ModelLookup, vkAuth VKAuthenticator, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if models == nil {
			writeJSONError(w, http.StatusInternalServerError, "database not available")
			return
		}
		vkMeta, ok := requireVK(w, r, vkAuth)
		if !ok {
			return
		}

		modelID := r.PathValue("model")
		if modelID == "" {
			writeJSONError(w, http.StatusBadRequest, "model id is required")
			return
		}

		model, err := models.GetModelByCode(r.Context(), modelID)
		if err != nil {
			logger.Warn("model not found", "modelId", modelID, "error", err)
			writeJSONError(w, http.StatusNotFound, "model not found: "+modelID)
			return
		}

		if len(vkMeta.AllowedModels) > 0 &&
			!routingcore.ModelMatchesAllowedRefs(model.ID, model.ProviderModelID, model.ProviderID, vkMeta.AllowedModels) {
			writeJSONError(w, http.StatusNotFound, "model not found: "+modelID)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		var b []byte
		if r.Header.Get("anthropic-version") != "" {
			b, _ = json.Marshal(buildAnthropicModelEntry(*model, time.Now().UTC().Format(time.RFC3339)))
		} else {
			b, _ = json.Marshal(buildOpenAIModelEntry(*model, time.Now().Unix()))
		}
		_, _ = w.Write(b)
	}
}
