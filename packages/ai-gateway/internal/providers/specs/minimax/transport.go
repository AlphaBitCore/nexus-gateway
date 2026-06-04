package minimax

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

// Transport implements [provcore.Transport] for MiniMax. MiniMax exposes
// an OpenAI-compatible API at api.minimax.io for the M2/M2.5/M2.7 model
// family — that is the only path we route to today. CallTarget.BaseURL
// must be origin-only ("https://api.minimax.io"); the /v1 prefix is
// appended in BuildURL. CallTarget.Extras["minimax.groupId"] is
// forwarded as a GroupId query parameter (still required by some
// MiniMax tenants for billing attribution).
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

// BuildURL routes every endpoint to the MiniMax OpenAI-compatible paths.
// The legacy native chatcompletion_pro shape (sender_type/bot_setting/
// reply_constraints) is not supported — IdentityCodec cannot translate
// canonical OpenAI bodies into that schema, and MiniMax now markets the
// OpenAI-compat path as the primary surface for M2/M2.5/M2.7. If a
// customer ever needs the native path, build a real MiniMax codec and
// re-introduce the route then; until then the surface stays single.
//
// CallTarget.BaseURL is required and must be origin-only
// (e.g. "https://api.minimax.io"). The /v1 prefix is appended here so
// the seeded provider templates and DB rows stay aligned with the rest
// of the adapter set; baking /v1 into a default would diverge from the
// shipped baseUrl.
func (t *Transport) BuildURL(target provcore.CallTarget, endpoint typology.WireShape, _ bool) (string, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return "", fmt.Errorf("minimax: BaseURL is empty")
	}
	groupID := target.Get("minimax.groupId")
	switch endpoint {
	case typology.WireShapeOpenAIChat:
		u := base + "/v1/chat/completions"
		if groupID != "" {
			u += "?GroupId=" + groupID
		}
		return u, nil
	case typology.WireShapeOpenAIEmbeddings:
		u := base + "/v1/embeddings"
		if groupID != "" {
			u += "?GroupId=" + groupID
		}
		return u, nil
	case typology.WireShapeNone:
		return base + "/v1/models", nil
	}
	return "", fmt.Errorf("minimax: unsupported endpoint %q", endpoint)
}

// ApplyAuth sets `Authorization: Bearer`.
func (t *Transport) ApplyAuth(r *http.Request, target provcore.CallTarget) error {
	if target.APIKey == "" {
		return fmt.Errorf("minimax: missing API key")
	}
	r.Header.Set("Authorization", "Bearer "+target.APIKey)
	return nil
}

// Do delegates to the shared HTTP client.
func (t *Transport) Do(ctx context.Context, r *http.Request) (*http.Response, error) {
	return t.client.Do(r.WithContext(ctx))
}

// Probe hits the models endpoint.
func (t *Transport) Probe(ctx context.Context, target provcore.CallTarget) (*provcore.ProbeResult, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return &provcore.ProbeResult{OK: false, Detail: "BaseURL is empty"}, nil
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
