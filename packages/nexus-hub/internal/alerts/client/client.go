package alertclient

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/client/spool"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// Client is the Hub-internal outbound HTTP client for alert ingress, used by
// Hub-side callers (scheduled jobs, the alerteval runtime when emitting Hub-
// internal events) to fire alerts that survive a Hub restart or transient
// HTTP failure. Distinct from the dispatch fan-out in
// `packages/nexus-hub/internal/alerts/engine/`, which delivers alerts to channels
// (webhooks, SMTP, SIEM) once a rule fires.
//
// Fire is non-blocking: any delivery failure is absorbed into the disk spool
// and returns nil. Use ReplayPending to drain the spool after connectivity
// is restored.
//
// Naming context: this package originally lived at packages/shared/alertclient
// and was moved to nexus-hub/internal when it became clear that Hub is the
// only consumer. The "client" suffix is preserved for backward compatibility
// with the prior name; the package is **not** data-plane.
type Client struct {
	cfg        Config
	mu         sync.RWMutex
	baseURL    string
	httpClient *http.Client
	sp         *spool.Spool[AlertEnvelope]

	fireSuccess atomic.Int64
	fireSpooled atomic.Int64
	fireDrop    atomic.Int64
}

// New constructs a Client. Returns an error if HubBaseURL is empty or the
// spool directory cannot be created.
func New(cfg Config) (*Client, error) {
	if cfg.HubBaseURL == "" {
		return nil, fmt.Errorf("alertclient: HubBaseURL required")
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 5 * time.Second
	}
	if cfg.ReplayEvery == 0 {
		cfg.ReplayEvery = 30 * time.Second
	}
	if cfg.SpoolMaxBytes == 0 {
		cfg.SpoolMaxBytes = 50 << 20
	}
	sp, err := spool.New[AlertEnvelope](cfg.SpoolDir, "alertclient", cfg.SpoolMaxBytes, cfg.Logger)
	if err != nil {
		return nil, err
	}
	return &Client{
		cfg:     cfg,
		baseURL: cfg.HubBaseURL,
		httpClient: nexushttp.New(nexushttp.Config{
			Caller:         "hub-alertclient",
			Timeout:        cfg.HTTPTimeout,
			PropagateReqID: true,
		}),
		sp: sp,
	}, nil
}

// SetHubBaseURL swaps the target base URL at runtime. Used in tests and on
// Hub address rotation; safe for concurrent use.
func (c *Client) SetHubBaseURL(u string) {
	c.mu.Lock()
	c.baseURL = u
	c.mu.Unlock()
}

// PendingCount returns the number of envelopes currently waiting on disk.
func (c *Client) PendingCount() int { return c.sp.PendingCount() }

// Fire sends an alert envelope to Hub. If the POST fails for any reason
// (network error, non-2xx status), the envelope is spooled to disk and nil
// is returned — the caller is never blocked on alert delivery. A non-nil
// error is returned only if the spool write itself fails (e.g. disk full).
func (c *Client) Fire(ctx context.Context, env AlertEnvelope) error {
	if err := c.post(ctx, "/api/v1/alerts/raise", env); err != nil {
		c.fireSpooled.Add(1)
		if spErr := c.sp.Enqueue(env); spErr != nil {
			c.fireDrop.Add(1)
			return fmt.Errorf("fire: spool enqueue failed: %w", spErr)
		}
		c.cfg.Logger.Warn("alertclient: Hub unreachable, spooled envelope",
			"ruleId", env.RuleID, "targetKey", env.TargetKey, "err", err)
		return nil
	}
	c.fireSuccess.Add(1)
	return nil
}

// Resolve sends a resolve request to Hub. Unlike Fire, errors are returned
// to the caller because resolve is typically a deliberate action.
func (c *Client) Resolve(ctx context.Context, ruleID, targetKey, reason string) error {
	return c.post(ctx, "/api/v1/alerts/resolve", ResolveRequest{
		RuleID: ruleID, TargetKey: targetKey, Reason: reason,
	})
}

// ReplayPending drains the disk spool, posting each stored envelope to Hub.
// Stops on the first POST failure; successfully delivered envelopes are
// removed from disk. Safe to call concurrently — the spool serializes access.
// Returns the count of successfully replayed envelopes.
func (c *Client) ReplayPending(ctx context.Context) (int, error) {
	return c.sp.Drain(ctx, func(env AlertEnvelope) error {
		return c.post(ctx, "/api/v1/alerts/raise", env)
	})
}

// post marshals body as JSON and POSTs it to baseURL+path with the
// configured auth header. Returns a non-nil error for any network failure or
// non-2xx response.
func (c *Client) post(ctx context.Context, path string, body any) error {
	c.mu.RLock()
	url := c.baseURL + path
	c.mu.RUnlock()

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.AuthHeader != "" {
		req.Header.Set("Authorization", c.cfg.AuthHeader)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 300 {
		return fmt.Errorf("hub returned %d", resp.StatusCode)
	}
	return nil
}
