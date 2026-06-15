package openai

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

// Transport implements [provcore.Transport] for OpenAI and OpenAI-
// compatible upstreams. Relative URL paths are stable across all
// OpenAI-compat vendors so this type is shared by the deepseek spec
// without further specialisation.
type Transport struct {
	client *http.Client
	probe  *http.Client
	log    *slog.Logger
}

// NewTransport builds a Transport with the shared client from specutil.
func NewTransport(log *slog.Logger) *Transport {
	return &Transport{
		client: specutil.NewHTTPClient(),
		probe:  specutil.NewProbeClient(),
		log:    log,
	}
}

// BuildURL maps endpoint → path on top of target.BaseURL.
func (t *Transport) BuildURL(target provcore.CallTarget, endpoint typology.WireShape, _ bool) (string, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return "", fmt.Errorf("openai: BaseURL is empty")
	}
	path, ok := PathForEndpoint(endpoint)
	if !ok {
		return "", fmt.Errorf("openai: unsupported endpoint %q", endpoint)
	}
	return base + path, nil
}

// PathForEndpoint is the OpenAI-compat URL path table. Exported so
// other OpenAI-compat transports (DeepSeek) can reuse it.
func PathForEndpoint(endpoint typology.WireShape) (string, bool) {
	switch endpoint {
	case typology.WireShapeOpenAIChat:
		return "/v1/chat/completions", true
	case typology.WireShapeOpenAIEmbeddings:
		return "/v1/embeddings", true
	case typology.WireShapeNone:
		return "/v1/models", true
	case typology.WireShapeOpenAICompletionsLegacy:
		return "/v1/completions", true
	case typology.WireShapeOpenAIResponses:
		return "/v1/responses", true
	}
	return "", false
}

// ApplyAuth sets the `Authorization: Bearer` header.
func (t *Transport) ApplyAuth(r *http.Request, target provcore.CallTarget) error {
	if target.APIKey == "" {
		return fmt.Errorf("openai: missing API key")
	}
	r.Header.Set("Authorization", "Bearer "+target.APIKey)
	return nil
}

// Do delegates to the shared HTTP client.
func (t *Transport) Do(ctx context.Context, r *http.Request, _ provcore.CallTarget) (*http.Response, error) {
	return t.client.Do(r.WithContext(ctx))
}

// Probe issues a GET /v1/models and reports whether the endpoint is
// reachable with the configured credential.
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
