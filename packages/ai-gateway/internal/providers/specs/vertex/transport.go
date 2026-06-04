package vertex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// tokenCacheEntry caches exchanged service-account OAuth2 tokens. We
// keep one entry per service-account email + project + scope tuple.
type tokenCacheEntry struct {
	token   *oauth2.Token
	expires time.Time
}

// Transport implements [provcore.Transport] for Vertex AI. It resolves
// an OAuth2 bearer token from the service-account JSON stored in
// CallTarget.Extras and caches tokens keyed by (serviceAccountEmail,
// projectID, scopes).
type Transport struct {
	client *http.Client
	probe  *http.Client
	log    *slog.Logger

	cacheMu sync.Mutex
	cache   map[string]*tokenCacheEntry
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
		cache:  make(map[string]*tokenCacheEntry),
	}
}

// BuildURL assembles the `aiplatform.googleapis.com` publisher URL.
func (t *Transport) BuildURL(target provcore.CallTarget, endpoint typology.WireShape, stream bool) (string, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	project := target.Get("gcp.projectId")
	location := target.Get("gcp.location")
	if location == "" {
		location = "us-central1"
	}
	if base == "" {
		if project == "" {
			return "", fmt.Errorf("vertex: missing BaseURL and gcp.projectId")
		}
		base = fmt.Sprintf("https://%s-aiplatform.googleapis.com", location)
	}
	model := target.ProviderModelID
	if model == "" {
		return "", fmt.Errorf("vertex: missing ProviderModelID")
	}
	publisher := target.Get("gcp.publisher")
	if publisher == "" {
		publisher = "google"
	}
	if project == "" {
		return "", fmt.Errorf("vertex: missing gcp.projectId")
	}

	switch endpoint {
	case typology.WireShapeVertexGenerateContent:
		action := "generateContent"
		query := url.Values{}
		if stream {
			action = "streamGenerateContent"
			query.Set("alt", "sse")
		}
		u := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/%s/models/%s:%s", base, project, location, publisher, model, action)
		if v := query.Encode(); v != "" {
			u += "?" + v
		}
		return u, nil
	case typology.WireShapeNone:
		return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/%s/models", base, project, location, publisher), nil
	}
	return "", fmt.Errorf("vertex: unsupported endpoint %q", endpoint)
}

// ApplyAuth exchanges the service-account JSON for an OAuth2 token and
// stamps `Authorization: Bearer <token>`.
func (t *Transport) ApplyAuth(r *http.Request, target provcore.CallTarget) error {
	tok, err := t.token(r.Context(), target)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// Do delegates to the shared HTTP client.
func (t *Transport) Do(ctx context.Context, r *http.Request) (*http.Response, error) {
	return t.client.Do(r.WithContext(ctx))
}

// Probe calls the publisher-models listing endpoint.
func (t *Transport) Probe(ctx context.Context, target provcore.CallTarget) (*provcore.ProbeResult, error) {
	project := target.Get("gcp.projectId")
	location := target.Get("gcp.location")
	if location == "" {
		location = "us-central1"
	}
	if project == "" {
		return &provcore.ProbeResult{OK: false, Detail: "missing gcp.projectId"}, nil
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, specutil.ProbeTimeout)
	defer cancel()

	tok, err := t.token(ctx, target)
	if err != nil {
		return &provcore.ProbeResult{OK: false, Detail: err.Error(), Err: err}, nil
	}
	urlStr := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models?pageSize=1", location, project, location)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return &provcore.ProbeResult{OK: false, Detail: err.Error(), Err: err}, nil
	}
	req.Header.Set("Authorization", "Bearer "+tok)
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

// token returns a non-expired bearer token for target's service
// account. A cached token is reused when it has more than 60s of life
// remaining; otherwise we mint a fresh one via golang.org/x/oauth2/google.
func (t *Transport) token(ctx context.Context, target provcore.CallTarget) (string, error) {
	saJSON := target.Get("gcp.serviceAccountJSON")
	if saJSON == "" {
		if bearer := target.Get("gcp.bearerToken"); bearer != "" {
			return bearer, nil
		}
		if target.APIKey != "" {
			return target.APIKey, nil
		}
		return "", fmt.Errorf("vertex: missing gcp.serviceAccountJSON / gcp.bearerToken / APIKey")
	}

	email, err := serviceAccountEmail(saJSON)
	if err != nil {
		return "", err
	}
	key := email + "|" + target.Get("gcp.projectId")

	t.cacheMu.Lock()
	entry, ok := t.cache[key]
	t.cacheMu.Unlock()
	if ok && time.Until(entry.expires) > 60*time.Second {
		return entry.token.AccessToken, nil
	}

	cfg, err := google.JWTConfigFromJSON([]byte(saJSON), "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", fmt.Errorf("vertex: parse service-account JSON: %w", err)
	}
	tok, err := cfg.TokenSource(ctx).Token()
	if err != nil {
		return "", fmt.Errorf("vertex: mint oauth2 token: %w", err)
	}

	t.cacheMu.Lock()
	t.cache[key] = &tokenCacheEntry{token: tok, expires: tok.Expiry}
	t.cacheMu.Unlock()
	return tok.AccessToken, nil
}

// serviceAccountEmail extracts client_email from the JSON blob so we
// can key the cache without re-parsing on every call path.
func serviceAccountEmail(saJSON string) (string, error) {
	var parsed struct {
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal([]byte(saJSON), &parsed); err != nil {
		return "", fmt.Errorf("vertex: parse service-account JSON: %w", err)
	}
	if parsed.ClientEmail == "" {
		return "", fmt.Errorf("vertex: service-account JSON missing client_email")
	}
	return parsed.ClientEmail, nil
}
