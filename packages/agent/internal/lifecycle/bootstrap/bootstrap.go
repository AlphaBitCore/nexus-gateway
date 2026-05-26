// Package bootstrap fetches deployment-wide settings the agent needs to
// drive the SSO self-enrollment flow without per-device hard-coding.
// It calls Hub's unauthenticated GET /api/public/agent-bootstrap
// endpoint and caches the result for 60s.
package bootstrap

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// DefaultHTTPClient builds an http.Client suitable for the public
// /api/public/agent-bootstrap endpoint.
//
// Hub serves bootstrap as a *public* endpoint on its DNS-resolved
// hostname (e.g. hub.example.com), typically fronted by a public-
// CA TLS certificate (Let's Encrypt). The mTLS-pinned hubhttp client
// rejects that cert (its trust anchor is the internal gateway CA used
// for device-cert chains), which is why early agents logged
// "tls: failed to verify certificate: x509: certificate signed by
// unknown authority" warnings on every warm-bootstrap call and the
// onboarding UI stalled on "Contacting the gateway".
//
// Use this helper at every bootstrap.New call site instead of the
// shared pinned client. Pinning is preserved for everything that
// rides mTLS (audit upload, shadow report, heartbeat) — only this
// pre-enrollment endpoint switches to system roots.
//
// The returned client has a 10 s timeout matching the warm-bootstrap
// budget in cmd/agent and enforces TLS 1.2+ to match the platform's
// general posture.
func DefaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// Info is the deployment-wide enrollment metadata served by Hub.
type Info struct {
	// ControlPlaneURL is the Control Plane base URL where the agent
	// posts /api/agent/sso-enroll. Empty when Hub has not been
	// configured with a CP URL (operator omission).
	ControlPlaneURL string `json:"controlPlaneURL"`
	// DeviceAuthMode reflects the current device-auth setting:
	// "mtls-only" or "enterprise-login". Drives the menu-bar UI's
	// decision to show / hide the SSO button.
	DeviceAuthMode string `json:"deviceAuthMode"`
}

// IsSSOAvailable returns true when the operator has provisioned both
// a CP URL and the enterprise-login mode.
func (i Info) IsSSOAvailable() bool {
	return i.ControlPlaneURL != "" && i.DeviceAuthMode == "enterprise-login"
}

const cacheTTL = 60 * time.Second

// Client fetches and caches the bootstrap response.
//
// Use a single instance per agent process. Safe for concurrent use.
type Client struct {
	hubBaseURL string
	http       *http.Client

	// override pins ControlPlaneURL from agent YAML so operators who
	// don't trust Hub-side discovery can still hard-code the value.
	// Empty means "trust Hub".
	override string

	cache atomic.Pointer[cacheEntry]
	mu    sync.Mutex
}

type cacheEntry struct {
	info    Info
	fetched time.Time
}

// New builds a Client. hubBaseURL is the Hub HTTP root (e.g.
// "https://hub.example.com"); httpClient should be a TLS-pinned client
// shared with the rest of the agent's Hub HTTP traffic. When
// overrideControlPlaneURL is non-empty, the resolved Info always
// returns that value regardless of what Hub reports.
func New(hubBaseURL string, httpClient *http.Client, overrideControlPlaneURL string) *Client {
	if httpClient == nil {
		httpClient = nexushttp.New(nexushttp.Config{})
	}
	return &Client{
		hubBaseURL: hubBaseURL,
		http:       httpClient,
		override:   overrideControlPlaneURL,
	}
}

// Get returns the cached Info, fetching from Hub when the cache is
// cold or stale.
func (c *Client) Get(ctx context.Context) (Info, error) {
	if entry := c.cache.Load(); entry != nil && time.Since(entry.fetched) < cacheTTL {
		return entry.info, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.cache.Load(); entry != nil && time.Since(entry.fetched) < cacheTTL {
		return entry.info, nil
	}

	info, err := c.fetch(ctx)
	if err != nil {
		// On fetch failure, return the stale entry rather than nothing
		// — the UI can keep working with the last-known mode.
		if entry := c.cache.Load(); entry != nil {
			return entry.info, nil
		}
		return Info{}, err
	}
	if c.override != "" {
		info.ControlPlaneURL = c.override
	}
	c.cache.Store(&cacheEntry{info: info, fetched: time.Now()})
	return info, nil
}

// Invalidate forces the next Get to re-fetch. Useful after a known
// CP-side mode change.
func (c *Client) Invalidate() {
	c.cache.Store(nil)
}

func (c *Client) fetch(ctx context.Context) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.hubBaseURL+"/api/public/agent-bootstrap", nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Info{}, fmt.Errorf("bootstrap fetch: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("bootstrap fetch: status %d: %s", resp.StatusCode, string(body))
	}
	var info Info
	if err := json.Unmarshal(body, &info); err != nil {
		return Info{}, fmt.Errorf("bootstrap fetch: decode: %w", err)
	}
	return info, nil
}
