package debug

// no streaming — embeddings are request/response only.

import (
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/embeddings"
)

// embeddingProbeRequest is the JSON body for POST /internal/embedding-probe.
// The Control Plane forwards the decrypted credential and the embedding
// model ID so the gateway can make a synthetic test call without touching
// its own DB.
type embeddingProbeRequest struct {
	// ProviderID is the UUID of the Provider row (echoed in the response).
	ProviderID string `json:"providerId"`
	// ModelID is the UUID of the Model row (echoed in the response).
	ModelID string `json:"modelId"`
	// ModelName is the display name of the model (echoed in the response).
	ModelName string `json:"modelName"`
	// ProviderModelID is the wire model code to send to the provider.
	ProviderModelID string `json:"providerModelId"`
	// BaseURL is the provider base URL (Provider.base_url).
	BaseURL string `json:"baseUrl"`
	// APIKey is the decrypted credential key (from CP vault). Empty for
	// providers that don't require auth (e.g. local inference).
	APIKey string `json:"apiKey"`
	// Dimension is the expected vector dimension from the Model row.
	// Zero means "skip dimension check".
	Dimension int `json:"dimension"`
}

// embeddingProbeResponse is the JSON body returned by POST /internal/embedding-probe.
type embeddingProbeResponse struct {
	OK                     bool      `json:"ok"`
	ProviderID             string    `json:"providerId"`
	ModelID                string    `json:"modelId"`
	ModelName              string    `json:"modelName"`
	Dimension              int       `json:"dimension"`
	LatencyMs              int64     `json:"latencyMs"`
	PromptTokens           int       `json:"promptTokens"`
	SampleEmbeddingFirst10 []float32 `json:"sampleEmbeddingFirst10"`
	Error                  string    `json:"error,omitempty"`
}

// EmbeddingProbeHandler returns an http.HandlerFunc that issues a
// synthetic embedding call to the specified provider endpoint and reports
// the outcome. This endpoint is called by the Control Plane BFF when
// an admin clicks "Test Embedding" on the Cache Settings page.
//
// The handler uses [embeddings.Client] which speaks the OpenAI wire
// format. Local OpenAI-compatible servers (e.g. local-inference) use the
// same codec because they implement the same /v1/embeddings shape.
func EmbeddingProbeHandler(httpClient *http.Client, logger *slog.Logger) http.HandlerFunc {
	// Construct the client once; no namespace Prometheus metrics for the
	// probe path (internal test endpoint — not production traffic).
	ec := embeddings.NewClient(httpClient, logger, "nexus_probe")

	return func(w http.ResponseWriter, r *http.Request) {
		var req embeddingProbeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, embeddingProbeResponse{
				OK:    false,
				Error: "invalid request body: " + err.Error(),
			})
			return
		}
		if req.BaseURL == "" {
			writeJSON(w, http.StatusBadRequest, embeddingProbeResponse{
				OK:    false,
				Error: "baseUrl is required",
			})
			return
		}
		if req.ProviderModelID == "" {
			writeJSON(w, http.StatusBadRequest, embeddingProbeResponse{
				OK:    false,
				Error: "providerModelId is required",
			})
			return
		}

		start := time.Now()
		embReq := embeddings.Request{
			Model: req.ProviderModelID,
			Input: "nexus probe",
		}
		resp, err := ec.Embed(r.Context(), req.BaseURL, req.ProviderModelID, req.APIKey, embReq, req.Dimension)
		latencyMs := time.Since(start).Milliseconds()

		if err != nil {
			logger.Warn("embedding probe failed",
				"providerId", req.ProviderID,
				"modelId", req.ModelID,
				"error", err)
			writeJSON(w, http.StatusOK, embeddingProbeResponse{
				OK:         false,
				ProviderID: req.ProviderID,
				ModelID:    req.ModelID,
				ModelName:  req.ModelName,
				Dimension:  req.Dimension,
				LatencyMs:  latencyMs,
				Error:      err.Error(),
			})
			return
		}

		sample := resp.Embedding
		if len(sample) > 10 {
			sample = sample[:10]
		}

		logger.Info("embedding probe success",
			"providerId", req.ProviderID,
			"modelId", req.ModelID,
			"dimension", len(resp.Embedding),
			"latencyMs", latencyMs)

		writeJSON(w, http.StatusOK, embeddingProbeResponse{
			OK:                     true,
			ProviderID:             req.ProviderID,
			ModelID:                req.ModelID,
			ModelName:              resp.Model,
			Dimension:              len(resp.Embedding),
			LatencyMs:              latencyMs,
			PromptTokens:           resp.PromptTokens,
			SampleEmbeddingFirst10: sample,
		})
	}
}
