package core

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

// client_reads.go holds the read (GET) capability methods — every typed admin
// query the faces consume. Each goes through the engine's adminGet; none builds an
// HTTP request itself. The mutating writes live in client_writes.go; the HTTP
// engine (auth, roundtrip, error classification) lives in client.go.

// Sparkline returns the analytics time series powering the health tiles.
func (c *Client) Sparkline(ctx context.Context, query url.Values) (*SparklineResult, error) {
	var out SparklineResult
	if err := c.adminGet(ctx, "/api/admin/analytics/sparkline", query, &out); err != nil {
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

// RoutingRules lists the configured routing rules (unwrapping the {data:[...]}
// envelope). Read surface behind the routing-rules toggle view.
func (c *Client) RoutingRules(ctx context.Context) ([]RoutingRule, error) {
	var out routingRuleList
	if err := c.adminGet(ctx, "/api/admin/routing-rules", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
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
