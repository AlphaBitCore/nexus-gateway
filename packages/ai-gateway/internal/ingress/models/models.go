package models

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
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

// ModelsHandler handles GET /v1/models, returning a model list in either
// OpenAI or Anthropic native shape depending on caller signaling.
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
// When a valid VK is present with AllowedModels, the list is filtered
// accordingly in both shapes.
func ModelsHandler(models ModelLookup, vkAuth VKAuthenticator, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if models == nil {
			writeJSONError(w, http.StatusInternalServerError, "database not available")
			return
		}
		modelList, err := models.ListEnabledModels(r.Context())
		if err != nil {
			logger.Error("list models failed", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to list models")
			return
		}

		var allowedRefs []store.AllowedModelRef
		if vkAuth != nil {
			if vkMeta, authErr := vkAuth.Authenticate(r.Context(), r); authErr == nil && len(vkMeta.AllowedModels) > 0 {
				allowedRefs = vkMeta.AllowedModels
			}
		}

		filtered := make([]store.Model, 0, len(modelList))
		for _, m := range modelList {
			if len(allowedRefs) > 0 && !routingcore.ModelMatchesAllowedRefs(m.ID, m.ProviderModelID, m.ProviderID, allowedRefs) {
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
	// the Nexus model-classification field lives under ModelType.
	ModelType        string   `json:"modelType,omitempty"`
	InputModalities  []string `json:"inputModalities,omitempty"`
	OutputModalities []string `json:"outputModalities,omitempty"`
}

func buildOpenAIModelsResponse(rows []store.Model) map[string]any {
	now := time.Now().Unix()
	entries := make([]openAIModelEntry, 0, len(rows))
	for _, m := range rows {
		// OpenAI's /v1/models documents `owned_by` as the
		// human-readable provider slug ("openai", "openai-internal",
		// "system"), not an opaque UUID. Use ProviderName (the
		// operator-facing slug) here so:
		//   1. SDK consumers comparing owned_by == "openai" succeed.
		//   2. The AI Gateway Simulator UI groups models under the
		//      readable provider label instead of a UUID bucket.
		// `name`, `owner_display_name` are Nexus extension fields
		// preserved for the simulator UI; OpenAI SDKs ignore unknown
		// JSON fields.
		entries = append(entries, openAIModelEntry{
			ID:               m.Code,
			Name:             m.Name,
			Object:           "model",
			Created:          now,
			OwnedBy:          m.ProviderName,
			OwnerDisplayName: m.ProviderDisplayName,
			ModelType:        m.Type,
			InputModalities:  m.InputModalities,
			OutputModalities: m.OutputModalities,
		})
	}
	return map[string]any{"object": "list", "data": entries}
}

func buildAnthropicModelsResponse(rows []store.Model) map[string]any {
	createdAt := time.Now().UTC().Format(time.RFC3339)
	entries := make([]anthropicModelEntry, 0, len(rows))
	for _, m := range rows {
		entries = append(entries, anthropicModelEntry{
			Type:             "model",
			ID:               m.Code,
			DisplayName:      m.Name,
			CreatedAt:        createdAt,
			MaxInputTokens:   m.MaxContextTokens,
			MaxTokens:        m.MaxOutputTokens,
			ModelType:        m.Type,
			InputModalities:  m.InputModalities,
			OutputModalities: m.OutputModalities,
		})
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

// ModelDetailHandler handles GET /v1/models/{model}, returning a single model.
func ModelDetailHandler(models ModelLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if models == nil {
			writeJSONError(w, http.StatusInternalServerError, "database not available")
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

		w.Header().Set("Content-Type", "application/json")
		var b []byte
		if r.Header.Get("anthropic-version") != "" {
			b, _ = json.Marshal(anthropicModelEntry{
				Type:           "model",
				ID:             model.Code,
				DisplayName:    model.Name,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				MaxInputTokens: model.MaxContextTokens,
				MaxTokens:      model.MaxOutputTokens,
			})
		} else {
			b, _ = json.Marshal(map[string]any{
				"id":      model.Code,
				"name":    model.Name,
				"object":  "model",
				"created": time.Now().Unix(),
				// owned_by is the readable provider slug per the OpenAI
				// spec (matches buildOpenAIModelsResponse — keep both
				// paths in lockstep so downstream consumers see the
				// same field semantics on list vs detail).
				"owned_by":           model.ProviderName,
				"owner_display_name": model.ProviderDisplayName,
			})
		}
		_, _ = w.Write(b)
	}
}
