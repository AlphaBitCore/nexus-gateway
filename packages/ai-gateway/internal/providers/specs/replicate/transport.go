// Package spec_replicate implements the Replicate prediction-API
// AdapterSpec.
//
// Replicate's wire model is unique: clients POST to /v1/predictions
// with {version, input}, then GET back the same resource to poll for
// status. Streaming is opt-in via `stream: true` in the input
// payload, in which case Replicate returns an SSE stream URL and
// emits output events.
//
// Authentication is `Authorization: Token <key>` (legacy) or
// `Authorization: Bearer <key>` (newer); we use Token to match
// Replicate's docs.
package replicate

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Transport implements [provcore.Transport] for Replicate's prediction API.
type Transport struct {
	client *http.Client
	probe  *http.Client
	log    *slog.Logger
}

// NewTransport constructs a Transport.
func NewTransport(log *slog.Logger) *Transport {
	if log == nil {
		log = slog.Default()
	}
	return &Transport{
		client: specutil.NewHTTPClient(),
		probe:  specutil.NewProbeClient(),
		log:    log,
	}
}

// BuildURL maps endpoints onto Replicate's URL scheme.
func (t *Transport) BuildURL(target provcore.CallTarget, endpoint typology.WireShape, _ bool) (string, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return "", fmt.Errorf("replicate: BaseURL is empty")
	}
	switch endpoint {
	case typology.WireShapeOpenAIChat:
		return base + "/v1/predictions", nil
	case typology.WireShapeNone:
		return base + "/v1/models", nil
	}
	return "", fmt.Errorf("replicate: unsupported endpoint %q", endpoint)
}

// ApplyAuth stamps Replicate's Token authentication header.
func (t *Transport) ApplyAuth(r *http.Request, target provcore.CallTarget) error {
	if target.APIKey == "" {
		return fmt.Errorf("replicate: missing API key")
	}
	r.Header.Set("Authorization", "Token "+target.APIKey)
	return nil
}

// Do delegates to the shared HTTP client.
func (t *Transport) Do(ctx context.Context, r *http.Request) (*http.Response, error) {
	return t.client.Do(r.WithContext(ctx))
}

// Probe issues a GET /v1/models which Replicate exposes with a valid token.
func (t *Transport) Probe(ctx context.Context, target provcore.CallTarget) (*provcore.ProbeResult, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return &provcore.ProbeResult{OK: false, Detail: "BaseURL empty"}, nil
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, specutil.ProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return &provcore.ProbeResult{OK: false, Detail: err.Error(), Err: err}, nil
	}
	if target.APIKey != "" {
		req.Header.Set("Authorization", "Token "+target.APIKey)
	}
	resp, err := t.probe.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return &provcore.ProbeResult{OK: false, LatencyMs: latency, Detail: err.Error(), Err: err}, nil
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &provcore.ProbeResult{OK: true, LatencyMs: latency, Detail: "ok"}, nil
	}
	return &provcore.ProbeResult{OK: false, LatencyMs: latency, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
}
