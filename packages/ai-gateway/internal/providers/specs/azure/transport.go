package azure

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// defaultAPIVersion is used when CallTarget.Extras["azure.apiVersion"]
// is empty. Bumped to 2024-10-21 (latest GA at audit time) so callers
// get structured-outputs and reasoning-model fields without an explicit
// version override; preview features (data_sources / On-Your-Data,
// fine-tuning APIs) still require an explicit per-call preview version
// like 2024-10-01-preview.
const defaultAPIVersion = "2024-10-21"

// Transport implements [provcore.Transport] for Azure OpenAI.
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

// BuildURL composes the deployment URL for the target.
func (t *Transport) BuildURL(target provcore.CallTarget, endpoint typology.WireShape, _ bool) (string, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return "", fmt.Errorf("azure-openai: BaseURL is empty")
	}
	deployment := target.Get("azure.deployment")
	if deployment == "" {
		deployment = target.ProviderModelID
	}
	if deployment == "" && endpoint != typology.WireShapeNone {
		return "", fmt.Errorf("azure-openai: missing deployment (target.Extras[\"azure.deployment\"] or ProviderModelID)")
	}
	apiVersion := target.Get("azure.apiVersion")
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}
	switch endpoint {
	case typology.WireShapeOpenAIChat:
		return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s", base, deployment, apiVersion), nil
	case typology.WireShapeOpenAIEmbeddings:
		return fmt.Sprintf("%s/openai/deployments/%s/embeddings?api-version=%s", base, deployment, apiVersion), nil
	case typology.WireShapeOpenAICompletionsLegacy:
		return fmt.Sprintf("%s/openai/deployments/%s/completions?api-version=%s", base, deployment, apiVersion), nil
	case typology.WireShapeNone:
		return fmt.Sprintf("%s/openai/models?api-version=%s", base, apiVersion), nil
	}
	return "", fmt.Errorf("azure-openai: unsupported endpoint %q", endpoint)
}

// ApplyAuth sets the `api-key` header.
func (t *Transport) ApplyAuth(r *http.Request, target provcore.CallTarget) error {
	if target.APIKey == "" {
		return fmt.Errorf("azure-openai: missing API key")
	}
	r.Header.Set("api-key", target.APIKey)
	return nil
}

// Do delegates to the shared HTTP client.
func (t *Transport) Do(ctx context.Context, r *http.Request) (*http.Response, error) {
	return t.client.Do(r.WithContext(ctx))
}

// Probe calls /openai/models with the configured api-key.
func (t *Transport) Probe(ctx context.Context, target provcore.CallTarget) (*provcore.ProbeResult, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return &provcore.ProbeResult{OK: false, Detail: "BaseURL empty"}, nil
	}
	apiVersion := target.Get("azure.apiVersion")
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, specutil.ProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/openai/models?api-version=%s", base, apiVersion), nil)
	if err != nil {
		return &provcore.ProbeResult{OK: false, Detail: err.Error(), Err: err}, nil
	}
	if target.APIKey != "" {
		req.Header.Set("api-key", target.APIKey)
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
