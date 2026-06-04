package geminicache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// cachedRecord is persisted in Redis for each active cachedContent entry.
type cachedRecord struct {
	Name       string `json:"name"`
	ExpireTime string `json:"expire_time"`
	TokenCount int    `json:"token_count"`
}

// cachedContentResponse is the minimal projection of the Gemini
// POST /v1beta/cachedContents response body.
type cachedContentResponse struct {
	Name          string `json:"name"`
	ExpireTime    string `json:"expireTime"`
	UsageMetadata struct {
		TotalTokenCount int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// apiClient wraps the Gemini v1beta cachedContents REST API.
type apiClient struct {
	http *http.Client
}

func newAPIClient() *apiClient {
	return &apiClient{
		http: nexushttp.New(nexushttp.Config{
			Timeout:        30 * time.Second,
			Caller:         "ai-gateway-geminicache",
			PropagateReqID: true,
		}),
	}
}

// create calls POST {baseURL}/v1beta/cachedContents and returns the created
// record. model must be the bare provider model ID (e.g. "gemini-2.0-flash");
// the "models/" prefix is added internally.
//
// systemInstruction must be the raw JSON of the Gemini systemInstruction
// object, e.g. `{"parts":[{"text":"..."}]}`. toolsJSON / toolConfigJSON are the
// raw JSON of the request's tools array / toolConfig object, folded into the
// cachedContent so a request that references it need not (and may not) repeat
// them; pass "" when the request carries none.
func (c *apiClient) create(
	ctx context.Context,
	apiKey, baseURL, model, systemInstructionJSON, toolsJSON, toolConfigJSON string,
	ttlSecs int,
) (*cachedRecord, error) {
	// The CachedContents API requires the "models/" prefix on the model name.
	if !strings.HasPrefix(model, "models/") {
		model = "models/" + model
	}

	var sysInstr any
	if err := json.Unmarshal([]byte(systemInstructionJSON), &sysInstr); err != nil {
		return nil, fmt.Errorf("geminicache: invalid systemInstruction JSON: %w", err)
	}

	payload := map[string]any{
		"model":             model,
		"systemInstruction": sysInstr,
		"ttl":               fmt.Sprintf("%ds", ttlSecs),
	}
	// Fold the request's tools / toolConfig into the cache object so the
	// GenerateContent request can reference the cache without re-sending them
	// (Gemini forbids setting both). Mirrors the cache key in contentHash.
	if toolsJSON != "" {
		var tools any
		if err := json.Unmarshal([]byte(toolsJSON), &tools); err != nil {
			return nil, fmt.Errorf("geminicache: invalid tools JSON: %w", err)
		}
		payload["tools"] = tools
	}
	if toolConfigJSON != "" {
		var toolConfig any
		if err := json.Unmarshal([]byte(toolConfigJSON), &toolConfig); err != nil {
			return nil, fmt.Errorf("geminicache: invalid toolConfig JSON: %w", err)
		}
		payload["toolConfig"] = toolConfig
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("geminicache: marshal create body: %w", err)
	}

	endpoint := strings.TrimRight(baseURL, "/") + "/v1beta/cachedContents"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("geminicache: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("geminicache: POST cachedContents: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("geminicache: create status=%d body=%.200s", resp.StatusCode, raw)
	}

	var parsed cachedContentResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("geminicache: parse response: %w", err)
	}
	if parsed.Name == "" {
		return nil, fmt.Errorf("geminicache: response missing name field")
	}

	return &cachedRecord{
		Name:       parsed.Name,
		ExpireTime: parsed.ExpireTime,
		TokenCount: parsed.UsageMetadata.TotalTokenCount,
	}, nil
}
