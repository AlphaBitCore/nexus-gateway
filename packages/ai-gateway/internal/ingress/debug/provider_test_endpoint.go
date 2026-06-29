package debug

import (
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"strings"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// providerTestRequest is the JSON body for POST /internal/provider-test.
// AdapterType is the canonical provcore.Format string — the Control
// Plane forwards Provider.adapter_type verbatim; there is no name-based
// inference.
type providerTestRequest struct {
	ProviderName string `json:"providerName"`
	AdapterType  string `json:"adapterType"`
	BaseURL      string `json:"baseUrl"`
	APIKey       string `json:"apiKey"`
}

// ProviderTestHandler returns a handler that tests provider connectivity.
// It picks the matching [provcore.Adapter] from the registry using the
// adapter type supplied by the caller and invokes Probe with a
// [provcore.CallTarget] built from the request body.
func ProviderTestHandler(reg *provcore.Registry, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req providerTestRequest
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

		providerName := strings.ToLower(req.ProviderName)
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

		target := provcore.CallTarget{
			ProviderName: providerName,
			Format:       format,
			BaseURL:      req.BaseURL,
			APIKey:       req.APIKey,
		}
		result, err := adapter.Probe(r.Context(), target)
		if err != nil {
			logger.Warn("provider probe failed", "provider", providerName, "error", err.Error())
			writeJSON(w, http.StatusOK, map[string]any{
				"success":   false,
				"error":     err.Error(),
				"provider":  providerName,
				"hasAPIKey": req.APIKey != "",
			})
			return
		}
		logger.Info("provider probe",
			"provider", providerName,
			"ok", result.OK,
			"latencyMs", result.LatencyMs,
			"detail", result.Detail,
		)
		writeJSON(w, http.StatusOK, map[string]any{
			"success":   result.OK,
			"latencyMs": result.LatencyMs,
			"detail":    result.Detail,
			"provider":  providerName,
			"hasAPIKey": req.APIKey != "",
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
