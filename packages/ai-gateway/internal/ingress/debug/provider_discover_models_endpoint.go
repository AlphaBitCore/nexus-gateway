package debug

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// discoverModelsRequest is the JSON body for POST /internal/provider-discover-models.
type discoverModelsRequest struct {
	AdapterType string `json:"adapterType"`
	BaseURL     string `json:"baseUrl"`
	APIKey      string `json:"apiKey"`
}

// discoveredModel is one entry in the models list returned by
// ProviderDiscoverModelsHandler.
type discoveredModel struct {
	ID            string `json:"id"`
	SuggestedType string `json:"suggestedType"`
}

// modelLister is the capability interface satisfied by adapters that can
// enumerate the upstream provider's model list. ListModels returns (ids,
// supported, err): when supported is false the transport does not implement
// the /v1/models list endpoint and model discovery should be declined.
// Only OpenAI and OpenAI-compatible adapters satisfy this interface.
type modelLister interface {
	ListModels(ctx context.Context, target provcore.CallTarget) (ids []string, supported bool, err error)
}

// SuggestModelType maps a model id to a Nexus model type by simple substring
// heuristic. The /v1/models endpoint carries no type field, so this is
// best-effort; the admin can override per row in the create-provider wizard.
func SuggestModelType(id string) string {
	l := strings.ToLower(id)
	switch {
	case strings.Contains(l, "embed"):
		return "embedding"
	// realtime must precede the audio arm: gpt-realtime-whisper /
	// gpt-realtime-translate are realtime-family models, not REST audio.
	case strings.Contains(l, "realtime"):
		return "realtime"
	case strings.Contains(l, "rerank"):
		return "rerank"
	case strings.Contains(l, "sora"), strings.Contains(l, "veo"),
		strings.Contains(l, "video"):
		return "video"
	case strings.Contains(l, "tts"):
		return "tts"
	case strings.Contains(l, "whisper"), strings.Contains(l, "transcribe"):
		return "stt"
	// No bare "audio" arm. What reaches here after the precise tts / stt /
	// realtime arms — "gpt-audio-*", "audio-preview" — is a model the
	// provider serves on chat completions that happens to accept and emit
	// audio parts, so it falls through to "chat" below.
	//
	// It used to return "audio", with a comment saying the admin could
	// refine it on save. They did not, and the type is what the routing
	// guard compares against the endpoint kind: every request to
	// /v1/chat/completions for one of these models was rejected with
	// MODEL_MODALITY_MISMATCH. `type` answers "which endpoint serves this
	// model", and for these the answer is chat; which modalities it handles
	// is what inputModalities/outputModalities are for.
	case strings.Contains(l, "dall-e"), strings.Contains(l, "image"):
		return "image"
	default:
		return "chat"
	}
}

// ProviderDiscoverModelsHandler returns a handler that fetches a provider's
// upstream model list. Only OpenAI / OpenAI-compatible adapters support
// model discovery; all other adapter types return HTTP 400 with
// code "discovery_unsupported".
//
// The handler mirrors the structure of ProviderTestHandler: it decodes the
// JSON body, looks up the adapter in the registry, and delegates the network
// call to the adapter's transport. Upstream errors are surfaced as HTTP 200
// with success:false (same as Probe failures) so the CP BFF can distinguish
// "handler reached upstream" from "handler rejected the request".
func ProviderDiscoverModelsHandler(reg *provcore.Registry, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req discoverModelsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "Invalid request body",
			})
			return
		}
		if req.BaseURL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "baseUrl is required",
			})
			return
		}
		format := provcore.Format(strings.ToLower(strings.TrimSpace(req.AdapterType)))
		if !format.Valid() {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "invalid or missing adapterType: " + req.AdapterType,
			})
			return
		}
		adapter, ok := reg.Get(format)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "no adapter registered for format: " + string(format),
			})
			return
		}
		// In production every registered adapter is a specAdapter (see
		// dispatch/spec_adapter.go), so this cast always succeeds. The real
		// OpenAI-only gate is the `supported` bool returned by
		// specAdapter.ListModels below: only OpenAI transports implement
		// transportModelLister, all others return (nil, false, nil).
		// This branch is belt-and-suspenders for non-specAdapter registries
		// used in unit tests (e.g. probeOnlyAdapter stubs).
		lister, ok := adapter.(modelLister)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"error":   "model discovery is supported only for OpenAI / OpenAI-compatible providers",
				"code":    "discovery_unsupported",
			})
			return
		}
		ids, supported, err := lister.ListModels(r.Context(), provcore.CallTarget{
			Format:  format,
			BaseURL: req.BaseURL,
			APIKey:  req.APIKey,
		})
		if !supported {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"error":   "model discovery is supported only for OpenAI / OpenAI-compatible providers",
				"code":    "discovery_unsupported",
			})
			return
		}
		if err != nil {
			if logger != nil {
				logger.Warn("provider discover-models failed",
					"format", format, "error", err.Error())
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"success": false, "error": err.Error(),
			})
			return
		}
		out := make([]discoveredModel, 0, len(ids))
		for _, id := range ids {
			out = append(out, discoveredModel{ID: id, SuggestedType: SuggestModelType(id)})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "models": out,
		})
	}
}
