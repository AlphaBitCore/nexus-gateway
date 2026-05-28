package core

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is the typed capability surface over a Nexus deployment. Each face
// (CLI, TUI, MCP) calls these methods; none of them build HTTP requests.
type Client struct {
	env   Env
	ts    TokenSource
	httpc *http.Client
}

// NewClient builds a Client for env using ts for admin credentials. A nil
// httpc gets a 30 s-timeout default.
func NewClient(env Env, ts TokenSource, httpc *http.Client) *Client {
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{env: env, ts: ts, httpc: httpc}
}

// Env returns the environment this client targets.
func (c *Client) Env() Env { return c.env }

const maxRespBody = 8 << 20 // 8 MiB cap on a single admin response body

// adminGet decodes a GET against the CP admin API into out.
func (c *Client) adminGet(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, c.env.CPBaseURL, path, query, nil, out)
}

// GetJSON is the exported escape hatch for admin GET endpoints whose typed model
// lands with its consuming view; it decodes the JSON body into out.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	return c.adminGet(ctx, path, query, out)
}

// do performs one admin-authed request and decodes the 2xx body into out.
func (c *Client) do(ctx context.Context, method, baseURL, path string, query url.Values, body, out any) error {
	respBody, status, err := c.roundtrip(ctx, method, baseURL, path, query, body)
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return &APIError{kind: ErrTransport, Status: status, Message: "decode response: " + err.Error()}
		}
	}
	return nil
}

// roundtrip attaches the admin credential, sends the request, maps non-2xx to a
// classified *APIError, and returns the raw 2xx body. Callers that need a typed
// value go through do; passthrough callers (simulator forward) keep the bytes.
func (c *Client) roundtrip(ctx context.Context, method, baseURL, path string, query url.Values, body any) ([]byte, int, error) {
	u := strings.TrimRight(baseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, &APIError{kind: ErrTransport, Message: "marshal request body: " + err.Error()}
		}
		reqBody = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return nil, 0, &APIError{kind: ErrTransport, Message: "build request: " + err.Error()}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	header, value, err := c.ts.Credential(ctx)
	if err != nil {
		return nil, 0, err // already a classified *APIError (ErrUnauthorized)
	}
	req.Header.Set(header, value)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, 0, &APIError{kind: ErrTransport, Message: err.Error()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))

	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, parseAPIError(resp.StatusCode, respBody)
	}
	return respBody, resp.StatusCode, nil
}

// parseAPIError turns a non-2xx response into a classified *APIError, reading
// the standard {error:{message,type,code}} envelope when present.
func parseAPIError(status int, body []byte) *APIError {
	e := &APIError{Status: status, kind: kindForStatus(status)}
	var env struct {
		Error struct {
			Message        string `json:"message"`
			Type           string `json:"type"`
			Code           string `json:"code"`
			Action         string `json:"action"`
			RequiredAction string `json:"requiredAction"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		e.Message = env.Error.Message
		e.Type = env.Error.Type
		e.Code = env.Error.Code
		e.IAMAction = firstNonEmpty(env.Error.Action, env.Error.RequiredAction)
	} else {
		e.Message = strings.TrimSpace(string(body))
		if e.Message == "" {
			e.Message = http.StatusText(status)
		}
	}
	return e
}

// --- Typed capability methods (admin-authed; confirmed shapes) ---

// Sparkline returns the analytics time series powering the health tiles.
func (c *Client) Sparkline(ctx context.Context, query url.Values) (*SparklineResult, error) {
	var out SparklineResult
	if err := c.adminGet(ctx, "/api/admin/analytics/sparkline", query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MetricsAggregates returns the aggregate metrics payload (same shape as the
// sparkline result).
func (c *Client) MetricsAggregates(ctx context.Context, query url.Values) (*SparklineResult, error) {
	var out SparklineResult
	if err := c.adminGet(ctx, "/api/admin/metrics/aggregates", query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TrafficList lists traffic events matching filter.
func (c *Client) TrafficList(ctx context.Context, filter TrafficFilter) (*TrafficList, error) {
	var out TrafficList
	if err := c.adminGet(ctx, "/api/admin/traffic", filter.values(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TrafficEvent fetches one traffic event by id.
func (c *Client) TrafficEvent(ctx context.Context, id string) (*TrafficEvent, error) {
	var out TrafficEvent
	if err := c.adminGet(ctx, "/api/admin/traffic/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TrafficEventNormalized fetches the normalized view of one traffic event. The
// normalized shape varies by adapter, so it is returned as raw JSON.
func (c *Client) TrafficEventNormalized(ctx context.Context, id string) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.adminGet(ctx, "/api/admin/traffic/"+url.PathEscape(id)+"/normalized", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Instances returns the five-service health rollup.
func (c *Client) Instances(ctx context.Context) (*InstancesResult, error) {
	var out InstancesResult
	if err := c.adminGet(ctx, "/api/admin/instances", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// VirtualKeys lists the deployment's virtual keys (unwrapping the {data:[...]}
// envelope the admin API returns).
func (c *Client) VirtualKeys(ctx context.Context) ([]VirtualKey, error) {
	var out virtualKeyList
	if err := c.adminGet(ctx, "/api/admin/virtual-keys", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

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

// AdminModels returns the grouped model catalog (admin-authed; lists all
// configured models regardless of any virtual key).
func (c *Client) AdminModels(ctx context.Context) (*ModelCatalog, error) {
	var out ModelCatalog
	if err := c.adminGet(ctx, "/api/admin/models", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Cost returns the grouped cost report. query may carry groupBy/window filters.
func (c *Client) Cost(ctx context.Context, query url.Values) (*CostReport, error) {
	var out CostReport
	if err := c.adminGet(ctx, "/api/admin/analytics/cost", query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CacheROI returns the cache savings rollup. query may carry a start/end window.
func (c *Client) CacheROI(ctx context.Context, query url.Values) (*CacheROIResult, error) {
	var out CacheROIResult
	if err := c.adminGet(ctx, "/api/admin/analytics/cache-roi", query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RoutingFallbacks returns the routing-fallback activity (which rules or the
// passthrough path absorbed traffic). query may carry a start/end window.
func (c *Client) RoutingFallbacks(ctx context.Context, query url.Values) (*FallbacksResult, error) {
	var out FallbacksResult
	if err := c.adminGet(ctx, "/api/admin/analytics/routing/fallbacks", query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LatencyPhases returns the per-group latency-percentile breakdown. groupBy is
// required by the endpoint (provider | model | virtual_key); query carries the
// start/end window (also required by the endpoint).
func (c *Client) LatencyPhases(ctx context.Context, groupBy string, query url.Values) (*LatencyPhasesResult, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("groupBy", groupBy)
	var out LatencyPhasesResult
	if err := c.adminGet(ctx, "/api/admin/analytics/latency-phases", query, &out); err != nil {
		return nil, err
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

// DLQ returns the dead-letter backlog (rows kept raw; the view shows depth).
func (c *Client) DLQ(ctx context.Context) (*DLQResult, error) {
	var out DLQResult
	if err := c.adminGet(ctx, "/api/admin/observability/dlq", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Nodes returns the registered nodes (heartbeat / version / config drift).
func (c *Client) Nodes(ctx context.Context) (*NodesResult, error) {
	var out NodesResult
	if err := c.adminGet(ctx, "/api/admin/nodes", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Alerts returns alert instances (callers filter to Firing() for the view).
func (c *Client) Alerts(ctx context.Context) (*AlertsResult, error) {
	var out AlertsResult
	if err := c.adminGet(ctx, "/api/admin/alerts", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
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

// ByProvider returns per-provider usage (top-talkers + cost view). query may
// carry a start/end window.
func (c *Client) ByProvider(ctx context.Context, query url.Values) (*ByProviderResult, error) {
	var out ByProviderResult
	if err := c.adminGet(ctx, "/api/admin/analytics/by-provider", query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ComplianceOverview returns the compliance KPI rollup.
func (c *Client) ComplianceOverview(ctx context.Context, query url.Values) (*ComplianceOverview, error) {
	var out ComplianceOverview
	if err := c.adminGet(ctx, "/api/admin/compliance/overview", query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Jobs returns the scheduled background jobs and their state.
func (c *Client) Jobs(ctx context.Context) (*JobsResult, error) {
	var out JobsResult
	if err := c.adminGet(ctx, "/api/admin/jobs", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ConfigSyncOutOfSync returns the nodes whose applied config lags target.
func (c *Client) ConfigSyncOutOfSync(ctx context.Context) (*ConfigSyncResult, error) {
	var out ConfigSyncResult
	if err := c.adminGet(ctx, "/api/admin/config-sync/out-of-sync", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Providers returns the configured upstream providers (id ↔ name ↔ displayName).
// The toolkit uses it to resolve a provider name (the latency-phases groupKey) to
// the UUID that ProviderDetail keys on, and to show the friendly DisplayName.
func (c *Client) Providers(ctx context.Context) (*ProvidersResult, error) {
	var out ProvidersResult
	if err := c.adminGet(ctx, "/api/admin/providers", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ProviderDetail returns one provider's SLO detail (availability + latency).
func (c *Client) ProviderDetail(ctx context.Context, providerID string, query url.Values) (*ProviderDetail, error) {
	var out ProviderDetail
	if err := c.adminGet(ctx, "/api/admin/analytics/provider/"+url.PathEscape(providerID), query, &out); err != nil {
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

// RoutingRules lists the configured routing rules (unwrapping the {data:[...]}
// envelope). Read surface behind the routing-rules toggle view.
func (c *Client) RoutingRules(ctx context.Context) ([]RoutingRule, error) {
	var out routingRuleList
	if err := c.adminGet(ctx, "/api/admin/routing-rules", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
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

// killswitchHistory is the slice of GET /api/admin/config-sync/history this client
// parses to read the current kill-switch state. The events array is newest-first.
type killswitchHistory struct {
	Events []struct {
		NewState struct {
			Engaged bool `json:"engaged"`
		} `json:"newState"`
		NewVersion int    `json:"newVersion"`
		CreatedAt  string `json:"createdAt"`
		ActorName  string `json:"actorName"`
	} `json:"events"`
}

// KillSwitchStatus reads the current global kill-switch state. The dedicated
// kill-switch route is write-only by design, so the current state is read off the
// generic config-sync history (the newest killswitch event), exactly as the web
// console does. No event means the switch has never been toggled (Known=false).
func (c *Client) KillSwitchStatus(ctx context.Context) (*KillSwitchState, error) {
	q := url.Values{"configKey": {"killswitch"}, "nodeType": {"compliance-proxy"}, "pageSize": {"1"}}
	var h killswitchHistory
	if err := c.adminGet(ctx, "/api/admin/config-sync/history", q, &h); err != nil {
		return nil, err
	}
	if len(h.Events) == 0 {
		return &KillSwitchState{Known: false}, nil
	}
	e := h.Events[0]
	return &KillSwitchState{Engaged: e.NewState.Engaged, Known: true, Version: e.NewVersion, At: e.CreatedAt, By: e.ActorName}, nil
}

// PassthroughSnapshot reads the full three-tier emergency-passthrough state
// (global + per-adapter + per-provider overrides).
func (c *Client) PassthroughSnapshot(ctx context.Context) (*PassthroughSnapshot, error) {
	var out PassthroughSnapshot
	if err := c.adminGet(ctx, "/api/admin/passthrough/snapshot", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// passthroughDefaultWindow is the auto-expiry applied to a toolkit-engaged
// passthrough when the caller gives none — a bounded "stop the bleed" that
// self-clears rather than being left on indefinitely. Well under the server's 8h cap.
const passthroughDefaultWindow = time.Hour

// passthroughMinReason mirrors the server's minimum reason length for an engage.
const passthroughMinReason = 20

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

// values renders the filter as a query string, omitting zero-valued fields.
func (f TrafficFilter) values() url.Values {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("statusRange", f.StatusRange)
	set("provider", f.Provider)
	set("modelUsed", f.ModelUsed)
	set("virtualKeyId", f.VirtualKeyID)
	set("source", f.Source)
	if !f.StartTime.IsZero() {
		q.Set("startTime", f.StartTime.UTC().Format(time.RFC3339))
	}
	if !f.EndTime.IsZero() {
		q.Set("endTime", f.EndTime.UTC().Format(time.RFC3339))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Offset > 0 {
		q.Set("offset", strconv.Itoa(f.Offset))
	}
	if f.ExcludeInternal != nil {
		q.Set("excludeInternal", strconv.FormatBool(*f.ExcludeInternal))
	}
	return q
}
