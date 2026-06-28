package core

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/url"
	"time"
)

// client_writes.go holds the mutating + action capability methods — VK
// create/revoke/regenerate, provider/routing toggles, cache flush, kill switch,
// emergency passthrough, and the simulator/dry-run POSTs. Each is a write the
// faces gate (prod confirmation) before calling; the agent loop additionally
// confirm-gates them. The read queries live in client_reads.go; the HTTP engine
// lives in client.go.

// CreateVK creates a personal Virtual Key and returns it including the
// once-shown plaintext secret. This is how an operator without a key obtains
// one they own (VK secrets are stored hashed and are not otherwise retrievable).
func (c *Client) CreateVK(ctx context.Context, name string) (*CreatedVK, error) {
	var out CreatedVK
	body := map[string]string{"name": name, "vkType": "personal"}
	if err := c.do(ctx, http.MethodPost, c.env.CPBaseURL, "/api/admin/virtual-keys", nil, body, &out); err != nil {
		return nil, err
	}
	if out.Key == "" {
		return nil, &APIError{kind: ErrTransport, Message: "create virtual key: server returned no plaintext key"}
	}
	return &out, nil
}

// SimulatorForward runs one crafted request through the real gateway pipeline
// via the admin simulator-forward endpoint, returning the raw upstream response
// body. The endpoint is admin-authed; req.VK is the upstream credential it
// forwards under. This is the Request Lab's single-shot (non-streaming) path.
func (c *Client) SimulatorForward(ctx context.Context, req SimulatorForwardRequest) (json.RawMessage, error) {
	// The forward endpoint passes the upstream body through verbatim, which may
	// not be JSON, so keep the raw bytes rather than decoding into a value.
	raw, _, err := c.roundtrip(ctx, http.MethodPost, c.env.CPBaseURL,
		"/api/admin/ai-gateway-simulator/forward", nil, req)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// RoutingSimulate runs the routing dry-run for a model — "why this route" —
// without firing a real request.
func (c *Client) RoutingSimulate(ctx context.Context, req RoutingSimulateRequest) (*RoutingSimulateResult, error) {
	var out RoutingSimulateResult
	if err := c.do(ctx, http.MethodPost, c.env.CPBaseURL, "/api/admin/routing-rules/simulate", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetProviderEnabled enables or disables a provider (PUT /providers/:id with a
// partial body). It is a mitigation write; callers gate it (prod confirmation)
// before calling.
func (c *Client) SetProviderEnabled(ctx context.Context, providerID string, enabled bool) error {
	return c.do(ctx, http.MethodPut, c.env.CPBaseURL, "/api/admin/providers/"+url.PathEscape(providerID), nil,
		map[string]bool{"enabled": enabled}, nil)
}

// CacheFlush invalidates the gateway's cached config (providers, models,
// credentials, routing, hooks, virtual keys, quotas). Mitigation write; gated.
func (c *Client) CacheFlush(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, c.env.CPBaseURL, "/api/admin/cache/flush", nil, nil, nil)
}

// RevokeVK revokes a virtual key (the data plane drops the cached hash). The
// endpoint only revokes a key in "active" status; callers gate it (prod
// confirmation) and pre-filter to revocable keys. Mitigation write.
func (c *Client) RevokeVK(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, c.env.CPBaseURL,
		"/api/admin/virtual-keys/"+url.PathEscape(id)+"/revoke", nil, nil, nil)
}

// RegenerateVK rotates a virtual key's secret, returning the new plaintext (the
// server keeps only a hash, so this is the one chance to read it). The old hash
// is invalidated on the data plane. Mitigation write; callers gate it.
func (c *Client) RegenerateVK(ctx context.Context, id string) (*RegeneratedVK, error) {
	var out RegeneratedVK
	if err := c.do(ctx, http.MethodPost, c.env.CPBaseURL,
		"/api/admin/virtual-keys/"+url.PathEscape(id)+"/regenerate", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Key == "" {
		return nil, &APIError{kind: ErrTransport, Message: "regenerate virtual key: server returned no plaintext key"}
	}
	return &out, nil
}

// SetRoutingRuleEnabled enables or disables a routing rule (PUT /routing-rules/:id
// with a partial body — only the enabled flag changes). Mitigation write; gated.
func (c *Client) SetRoutingRuleEnabled(ctx context.Context, id string, enabled bool) error {
	return c.do(ctx, http.MethodPut, c.env.CPBaseURL,
		"/api/admin/routing-rules/"+url.PathEscape(id), nil, map[string]bool{"enabled": enabled}, nil)
}

// SetKillSwitch engages or disengages the global kill switch. Callers gate it
// (prod confirmation) before calling.
func (c *Client) SetKillSwitch(ctx context.Context, engaged bool) (*KillSwitchResult, error) {
	var out KillSwitchResult
	if err := c.do(ctx, http.MethodPost, c.env.CPBaseURL, "/api/admin/compliance/killswitch", nil,
		map[string]bool{"engaged": engaged}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// passthroughDefaultWindow is the auto-expiry applied to a toolkit-engaged
// passthrough when the caller gives none — a bounded "stop the bleed" that
// self-clears rather than being left on indefinitely. Well under the server's 8h cap.
const passthroughDefaultWindow = time.Hour

// SetPassthroughGlobal sets the global emergency-passthrough tier (PUT
// /api/admin/passthrough/global). Mitigation write; callers gate it (prod
// confirmation) before calling. On engage it fills the server's required
// invariants the caller omitted (a future expiry, a ≥20-char reason, and
// bypassCache when bypassNormalize is set) so the write never 400s on a missing
// field; disengage sends the bare flag.
func (c *Client) SetPassthroughGlobal(ctx context.Context, req PassthroughGlobalRequest) error {
	if req.Enabled {
		if req.ExpiresAt == nil {
			t := time.Now().Add(passthroughDefaultWindow)
			req.ExpiresAt = &t
		}
		if req.BypassNormalize {
			req.BypassCache = true // the cache key derives from the normalized payload
		}
		if req.Reason == "" {
			req.Reason = "engaged via nexus operator toolkit"
		}
	}
	return c.do(ctx, http.MethodPut, c.env.CPBaseURL, "/api/admin/passthrough/global", nil, req, nil)
}
