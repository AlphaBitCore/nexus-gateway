// Package hub provides the HubAuditClient for uploading audit event batches to Hub.
package hub

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// HubAuditClient uploads audit event batches to Hub.
type HubAuditClient struct {
	BaseURL     string
	DeviceToken string
	ThingID     string
	HTTPClient  *http.Client
}

// NewHubAuditClient creates an audit upload client for Hub.
func NewHubAuditClient(baseURL, deviceToken, thingID string) *HubAuditClient {
	return &HubAuditClient{
		BaseURL:     baseURL,
		DeviceToken: deviceToken,
		ThingID:     thingID,
		HTTPClient: nexushttp.New(nexushttp.Config{
			Timeout:             30 * time.Second,
			Caller:              "agent-audit",
			PropagateReqID:      true,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     90 * time.Second,
			H2ReadIdleTimeout:   30 * time.Second,
			ForceHTTP2:          nexushttp.On(),
		}),
	}
}

// UploadAuditResponse holds the Hub's response to an audit upload.
type UploadAuditResponse struct {
	Accepted []string `json:"accepted"`
}

// UploadAudit posts a batch of events to Hub and returns accepted event IDs.
func (c *HubAuditClient) UploadAudit(ctx context.Context, events []map[string]any) (*UploadAuditResponse, error) {
	body, err := json.Marshal(events)
	if err != nil {
		return nil, fmt.Errorf("marshal audit batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx,
		http.MethodPost,
		c.BaseURL+"/api/internal/things/agent-audit",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.DeviceToken)
	req.Header.Set("X-Thing-Id", c.ThingID)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload audit: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("upload audit: status %d: %s", resp.StatusCode, respBody)
	}

	var result UploadAuditResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode audit response: %w", err)
	}
	return &result, nil
}
