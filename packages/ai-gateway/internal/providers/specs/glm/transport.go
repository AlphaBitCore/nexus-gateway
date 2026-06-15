package glm

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

// Transport implements [provcore.Transport] for Zhipu GLM.
type Transport struct {
	client *http.Client
	probe  *http.Client
	log    *slog.Logger
}

// NewTransport builds a Transport.
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

// BuildURL maps endpoints onto GLM's `/api/paas/v4/*` surface.
func (t *Transport) BuildURL(target provcore.CallTarget, endpoint typology.WireShape, _ bool) (string, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return "", fmt.Errorf("glm: BaseURL is empty")
	}
	switch endpoint {
	case typology.WireShapeOpenAIChat:
		return base + "/api/paas/v4/chat/completions", nil
	case typology.WireShapeOpenAIEmbeddings:
		return base + "/api/paas/v4/embeddings", nil
	case typology.WireShapeNone:
		return base + "/api/paas/v4/models", nil
	}
	return "", fmt.Errorf("glm: unsupported endpoint %q", endpoint)
}

// ApplyAuth sets `Authorization: Bearer <jwt>` from target.APIKey.
func (t *Transport) ApplyAuth(r *http.Request, target provcore.CallTarget) error {
	if target.APIKey == "" {
		return fmt.Errorf("glm: missing API key")
	}
	r.Header.Set("Authorization", "Bearer "+target.APIKey)
	return nil
}

// Do delegates to the shared client.
func (t *Transport) Do(ctx context.Context, r *http.Request, _ provcore.CallTarget) (*http.Response, error) {
	return t.client.Do(r.WithContext(ctx))
}

// Probe calls the models listing endpoint.
func (t *Transport) Probe(ctx context.Context, target provcore.CallTarget) (*provcore.ProbeResult, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return &provcore.ProbeResult{OK: false, Detail: "BaseURL empty"}, nil
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, specutil.ProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/paas/v4/models", nil)
	if err != nil {
		return &provcore.ProbeResult{OK: false, Detail: err.Error(), Err: err}, nil
	}
	if target.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+target.APIKey)
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
