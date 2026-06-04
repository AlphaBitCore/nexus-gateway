package gemini

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Transport implements [provcore.Transport] for Google Gemini's REST
// surface. The model ID is embedded in the URL path, streaming is
// selected by switching the action from `generateContent` to
// `streamGenerateContent` + `alt=sse`.
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

// BuildURL returns the Gemini endpoint URL for the target.
func (t *Transport) BuildURL(target provcore.CallTarget, endpoint typology.WireShape, stream bool) (string, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return "", fmt.Errorf("gemini: BaseURL is empty")
	}
	model := target.ProviderModelID
	if model == "" {
		return "", fmt.Errorf("gemini: missing ProviderModelID")
	}
	switch endpoint {
	case typology.WireShapeGeminiGenerateContent:
		action := "generateContent"
		query := url.Values{}
		if stream {
			action = "streamGenerateContent"
			query.Set("alt", "sse")
		}
		u := fmt.Sprintf("%s/v1beta/models/%s:%s", base, model, action)
		if v := query.Encode(); v != "" {
			u += "?" + v
		}
		return u, nil
	case typology.WireShapeGeminiEmbedContent:
		return fmt.Sprintf("%s/v1beta/models/%s:embedContent", base, model), nil
	case typology.WireShapeNone:
		return base + "/v1beta/models", nil
	}
	return "", fmt.Errorf("gemini: unsupported endpoint %q", endpoint)
}

// ApplyAuth sets the `x-goog-api-key` header.
func (t *Transport) ApplyAuth(r *http.Request, target provcore.CallTarget) error {
	if target.APIKey == "" {
		return fmt.Errorf("gemini: missing API key")
	}
	r.Header.Set("x-goog-api-key", target.APIKey)
	return nil
}

// Do delegates to the shared client.
func (t *Transport) Do(ctx context.Context, r *http.Request) (*http.Response, error) {
	return t.client.Do(r.WithContext(ctx))
}

// Probe lists models; public Gemini accepts the `x-goog-api-key`
// header and returns 200 on a valid key.
func (t *Transport) Probe(ctx context.Context, target provcore.CallTarget) (*provcore.ProbeResult, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return &provcore.ProbeResult{OK: false, Detail: "BaseURL empty"}, nil
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, specutil.ProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1beta/models", nil)
	if err != nil {
		return &provcore.ProbeResult{OK: false, Detail: err.Error(), Err: err}, nil
	}
	if target.APIKey != "" {
		req.Header.Set("x-goog-api-key", target.APIKey)
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
