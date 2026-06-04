package voyage

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

// defaultBaseURL is the canonical Voyage AI API host.
const defaultBaseURL = "https://api.voyageai.com"

// Transport implements [provcore.Transport] for Voyage AI's /v1/embeddings surface.
// Auth is a Bearer token (API key) set in the Authorization header.
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

// BuildURL maps endpoints onto Voyage AI's URL scheme.
// Only EndpointEmbeddings is supported; all other endpoints return an error.
func (t *Transport) BuildURL(target provcore.CallTarget, endpoint typology.WireShape, _ bool) (string, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	if endpoint != typology.WireShapeVoyageEmbeddings {
		return "", fmt.Errorf("voyage: only embeddings endpoint is supported; got %q", endpoint)
	}
	return base + "/v1/embeddings", nil
}

// ApplyAuth stamps the Bearer token from the API key.
func (t *Transport) ApplyAuth(r *http.Request, target provcore.CallTarget) error {
	if target.APIKey == "" {
		return fmt.Errorf("voyage: missing API key")
	}
	r.Header.Set("Authorization", "Bearer "+target.APIKey)
	return nil
}

// Do delegates to the shared HTTP client.
func (t *Transport) Do(ctx context.Context, r *http.Request) (*http.Response, error) {
	return t.client.Do(r.WithContext(ctx))
}

// Probe sends a minimal embeddings request to verify the API key.
// Voyage AI does not have a dedicated /models endpoint; we send a
// minimal single-string input to /v1/embeddings and accept 2xx or
// well-known 4xx (400, 422) as reachable (key format verified).
func (t *Transport) Probe(ctx context.Context, target provcore.CallTarget) (*provcore.ProbeResult, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	if target.APIKey == "" {
		return &provcore.ProbeResult{OK: false, Detail: "missing API key"}, nil
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, specutil.ProbeTimeout)
	defer cancel()

	// Voyage AI does not have a no-side-effect probe endpoint. We call
	// /v1/embeddings with the smallest possible input. A 200 or 400/422
	// (input validation, model mismatch) confirms the API key is reachable.
	// A 401 confirms the key is invalid; all other 4xx/5xx indicate
	// reachable-but-error.
	body := []byte(`{"model":"voyage-3-lite","input":"probe"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/embeddings", strings.NewReader(string(body)))
	if err != nil {
		return &provcore.ProbeResult{OK: false, Detail: err.Error(), Err: err}, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+target.APIKey)

	resp, err := t.probe.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return &provcore.ProbeResult{OK: false, LatencyMs: latency, Detail: err.Error(), Err: err}, nil
	}
	defer resp.Body.Close() //nolint:errcheck

	// 2xx, 400 (bad input), 422 (unprocessable — model not found) all
	// indicate the API key reached the Voyage AI infrastructure.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &provcore.ProbeResult{OK: true, LatencyMs: latency, Detail: "ok"}, nil
	}
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
		return &provcore.ProbeResult{OK: true, LatencyMs: latency, Detail: "reachable"}, nil
	}
	return &provcore.ProbeResult{OK: false, LatencyMs: latency, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
}
