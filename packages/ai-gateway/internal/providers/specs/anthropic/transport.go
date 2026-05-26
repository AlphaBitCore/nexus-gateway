package anthropic

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

// anthropicDefaultVersion is the header value used when Extras does not
// override it. Anthropic requires the `anthropic-version` header on
// every call.
const anthropicDefaultVersion = "2023-06-01"

// anthropicDefaultBeta is injected when neither the client nor the
// credential config supplies an anthropic-beta value. Enables prompt
// caching for models that still require the beta opt-in header
// (e.g. claude-haiku-4-5). For models where caching is GA (e.g.
// claude-sonnet-4-6), the header is harmless.
const anthropicDefaultBeta = "prompt-caching-2024-07-31"

// Transport implements [provcore.Transport] for Anthropic's
// `/v1/messages` surface. Authentication is the `x-api-key` header
// and every request carries the anthropic-version header.
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

// BuildURL maps endpoints onto Anthropic's URL scheme.
func (t *Transport) BuildURL(target provcore.CallTarget, endpoint typology.WireShape, _ bool) (string, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return "", fmt.Errorf("anthropic: BaseURL is empty")
	}
	switch endpoint {
	case typology.WireShapeAnthropicMessages:
		return base + "/v1/messages", nil
	case typology.WireShapeNone:
		return base + "/v1/models", nil
	}
	return "", fmt.Errorf("anthropic: unsupported endpoint %q", endpoint)
}

// ApplyAuth stamps `x-api-key` and `anthropic-version` headers. The
// version header is set only when the inbound request did not already
// carry one — letting a client (e.g. Claude Code) pin a newer
// anthropic-version (or a beta-tagged version) without the gateway
// downgrading it to the target/default. The target value still wins
// when the client sends nothing, so per-credential overrides keep
// working for tools that don't speak the version explicitly.
//
// anthropic-beta follows the same precedence: forwarded client value
// stays untouched, target.Get("anthropic.beta") only stamps when the
// inbound request omitted it. This matters because Claude Code
// negotiates beta opt-ins per request (context-management,
// prompt-caching, ...) and the gateway must not silently drop those
// flags.
func (t *Transport) ApplyAuth(r *http.Request, target provcore.CallTarget) error {
	if target.APIKey == "" {
		return fmt.Errorf("anthropic: missing API key")
	}
	r.Header.Set("x-api-key", target.APIKey)
	if r.Header.Get("anthropic-version") == "" {
		version := target.Get("anthropic.version")
		if version == "" {
			version = anthropicDefaultVersion
		}
		r.Header.Set("anthropic-version", version)
	}
	if r.Header.Get("anthropic-beta") == "" {
		beta := target.Get("anthropic.beta")
		if beta == "" {
			beta = anthropicDefaultBeta
		}
		r.Header.Set("anthropic-beta", beta)
	}
	return nil
}

// Do delegates to the shared HTTP client.
func (t *Transport) Do(ctx context.Context, r *http.Request) (*http.Response, error) {
	return t.client.Do(r.WithContext(ctx))
}

// Probe issues a GET /v1/models which is available on the public
// Anthropic endpoint with the configured API key.
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
		req.Header.Set("x-api-key", target.APIKey)
		req.Header.Set("anthropic-version", anthropicDefaultVersion)
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
