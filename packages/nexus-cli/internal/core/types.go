package core

import (
	"encoding/json"
	"time"
)

// Wire structs for the admin-authed capabilities. Field sets match the
// Control Plane admin API JSON; unmodeled fields are ignored on decode. Shapes
// are verified against live responses by the live-tagged round-trip test
// (go test -tags live), so drift surfaces as empty values rather than silent
// corruption.

// TrafficList is the envelope returned by GET /api/admin/traffic.
type TrafficList struct {
	Data   []TrafficEvent `json:"data"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

// TrafficEvent is one row of traffic. Only the fields the radar, drill-down,
// and cost surfaces consume are modeled; the API returns a superset.
type TrafficEvent struct {
	ID             string    `json:"id"`
	Source         string    `json:"source"`
	Timestamp      time.Time `json:"timestamp"`
	TargetHost     string    `json:"targetHost"`
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	StatusCode     int       `json:"statusCode"`
	LatencyMs      int       `json:"latencyMs"`
	UpstreamTTFBMs int       `json:"upstreamTtfbMs"`
	UpstreamTotMs  int       `json:"upstreamTotalMs"`
	RequestHooksMs int       `json:"requestHooksMs"`
	RespHooksMs    int       `json:"responseHooksMs"`
	TraceID        string    `json:"traceId"`
	ProviderID     string    `json:"providerId"`
	ProviderName   string    `json:"providerName"`
	ModelID        string    `json:"modelId"`
	ModelName      string    `json:"modelName"`
	PromptTokens   int       `json:"promptTokens"`
	CompletionTok  int       `json:"completionTokens"`
	TotalTokens    int       `json:"totalTokens"`
	EstCostUSD     float64   `json:"estimatedCostUsd"`
	CacheStatus    string    `json:"cacheStatus"`
	GatewayCache   string    `json:"gatewayCacheStatus"`
	CacheSavedUSD  float64   `json:"cacheNetSavingsUsd"`

	// Bodies + hook decisions (the single-event endpoint returns these; the
	// list endpoint omits them). They power the Event drill-down.
	RequestBody          json.RawMessage `json:"requestBody"`
	ResponseBody         json.RawMessage `json:"responseBody"`
	RequestHookDecision  string          `json:"requestHookDecision"`
	RequestHookReason    string          `json:"requestHookReason"`
	ResponseHookDecision string          `json:"responseHookDecision"`
	ResponseHookReason   string          `json:"responseHookReason"`
}

// SparklineResult is the time-series payload from GET /api/admin/analytics/
// sparkline and /metrics/aggregates (instruments.MetricsResult).
type SparklineResult struct {
	Granularity string             `json:"granularity"`
	Source      string             `json:"source"`
	Summary     map[string]float64 `json:"summary"`
	Series      []SparklineBucket  `json:"series"`
	Metadata    json.RawMessage    `json:"metadata"`
}

// SparklineBucket is one time bucket of metric values. Keys are the metric
// instrument names in snake_case (e.g. request_count, estimated_cost_usd).
type SparklineBucket struct {
	BucketStart time.Time          `json:"bucketStart"`
	Values      map[string]float64 `json:"values"`
}

// Metric instrument keys (the snake_case names the analytics series uses).
const (
	MetricRequestCount     = "request_count"
	MetricEstimatedCostUSD = "estimated_cost_usd"
	MetricTotalTokens      = "total_tokens"
	MetricCacheHitCount    = "cache_hit_count"
	MetricStatus4xxCount   = "status_4xx_count"
	MetricStatus5xxCount   = "status_5xx_count"
)

// Totals returns the window totals powering the health tiles. The sparkline
// endpoint leaves the top-level summary empty and reports per-bucket series, so
// when no summary is present Totals sums the series buckets. A populated
// summary (other endpoints) is returned as-is.
func (r *SparklineResult) Totals() map[string]float64 {
	if len(r.Summary) > 0 {
		return r.Summary
	}
	out := map[string]float64{}
	for _, b := range r.Series {
		for k, v := range b.Values {
			out[k] += v
		}
	}
	return out
}

// InstancesResult is GET /api/admin/instances — the five services' health.
type InstancesResult struct {
	Count    int                       `json:"count"`
	Services map[string]ServiceSummary `json:"services"`
}

// ServiceSummary is the per-service rollup keyed by service name.
type ServiceSummary struct {
	Total int `json:"total"`
}

// VirtualKey is one row of GET /api/admin/virtual-keys (envelope {data:[...]}).
// Usage/quota are not on this base resource; they come from analytics. VKType
// and VKStatus are the quota/approval-workflow fields (nullable): status drives
// whether a key is revocable (only "active" keys can be revoked).
type VirtualKey struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	KeyPrefix    string  `json:"keyPrefix"`
	SourceApp    string  `json:"sourceApp"`
	Enabled      bool    `json:"enabled"`
	OwnerID      string  `json:"ownerId"`
	ProjectID    *string `json:"projectId"`
	RateLimitRPM *int    `json:"rateLimitRpm"`
	VKType       *string `json:"vkType"`
	VKStatus     *string `json:"vkStatus"`
}

// Status returns the approval-workflow status ("active"/"pending"/"revoked"/
// "rejected"), or "active" when the column is null (legacy rows default active).
func (v VirtualKey) Status() string {
	if v.VKStatus != nil && *v.VKStatus != "" {
		return *v.VKStatus
	}
	return "active"
}

// Revocable reports whether this key can be revoked — only "active" keys can be
// (the revoke endpoint 404s on any other status). The view gates the revoke
// keystroke on this so the operator never burns a prod confirmation on a no-op.
func (v VirtualKey) Revocable() bool { return v.Status() == "active" }

// RegeneratedVK is the result of rotating a Virtual Key's secret. Key is the new
// plaintext, returned exactly once (the server keeps only a hash), so the view
// shows it once and never persists it.
type RegeneratedVK struct {
	ID        string `json:"id"`
	KeyPrefix string `json:"keyPrefix"`
	Key       string `json:"key"`
}

// virtualKeyList is the {data:[...]} envelope wrapping the VK list.
type virtualKeyList struct {
	Data []VirtualKey `json:"data"`
}

// CreatedVK is the result of creating a Virtual Key. Key is the plaintext
// secret — returned exactly once at creation (the server keeps only a hash), so
// the toolkit captures it here and stores it in the keychain.
type CreatedVK struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"keyPrefix"`
	Key       string `json:"key"`
}

// ModelCatalog is the {data:[...]} envelope from GET /api/admin/models,
// grouped by provider.
type ModelCatalog struct {
	Data []ModelGroup `json:"data"`
}

// ModelGroup is one provider and its models.
type ModelGroup struct {
	Provider ProviderRef `json:"provider"`
	Models   []Model     `json:"models"`
}

// ProviderRef identifies a provider in the model catalog. Name is the unique
// code ("anthropic"); DisplayName is the human label ("Anthropic").
type ProviderRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

// Label returns the human-friendly provider label, falling back to the code.
func (p ProviderRef) Label() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Name
}

// ProvidersResult is GET /api/admin/providers ({data:[...],total}).
type ProvidersResult struct {
	Data []Provider `json:"data"`
}

// Provider is one configured upstream provider. Name is the unique catalog key
// (it matches traffic_event.provider_name and the latency-phases provider
// groupKey); DisplayName is the human-friendly label shown to operators. The
// drill resolves Name → ID to call ProviderDetail, but never shows the UUID.
type Provider struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Enabled     bool   `json:"enabled"`
}

// Model is one catalog model row (subset of the admin fields the toolkit uses).
type Model struct {
	ID                    string  `json:"id"`
	Code                  string  `json:"code"`
	Name                  string  `json:"name"`
	ProviderID            string  `json:"providerId"`
	Type                  string  `json:"type"`
	Status                string  `json:"status"`
	Enabled               bool    `json:"enabled"`
	MaxContextTokens      int     `json:"maxContextTokens"`
	InputPricePerMillion  float64 `json:"inputPricePerMillion"`
	OutputPricePerMillion float64 `json:"outputPricePerMillion"`
}

// CostReport is the {data:[...],total} envelope from GET /api/admin/analytics/cost.
type CostReport struct {
	Data  []CostRow `json:"data"`
	Total int       `json:"total"`
}

// CostRow is one grouped cost bucket.
type CostRow struct {
	Group         string  `json:"group"`
	GroupLabel    string  `json:"groupLabel"`
	RequestCount  int     `json:"requestCount"`
	TotalTokens   int     `json:"totalTokens"`
	TotalCostUSD  float64 `json:"totalCostUsd"`
	CacheHitCount int     `json:"cacheHitCount"`
}

// KillSwitchResult is the response from POST /api/admin/compliance/killswitch.
type KillSwitchResult struct {
	Engaged        bool `json:"engaged"`
	Version        int  `json:"version"`
	ThingsNotified int  `json:"thingsNotified"`
	ThingsOnline   int  `json:"thingsOnline"`
}

// KillSwitchState is the current global kill-switch state, derived from the newest
// killswitch config-change event. Known is false when the switch has never been
// toggled (no event), so the UI distinguishes "off" from "never set" rather than
// showing a misleading default.
type KillSwitchState struct {
	Engaged bool
	Known   bool
	Version int
	At      string // event timestamp (RFC3339)
	By      string // actor who last toggled it
}

// PassthroughTier is one tier of the emergency-passthrough config — the global
// tier, or a single per-adapter / per-provider override.
type PassthroughTier struct {
	Enabled         bool   `json:"enabled"`
	BypassHooks     bool   `json:"bypassHooks"`
	BypassCache     bool   `json:"bypassCache"`
	BypassNormalize bool   `json:"bypassNormalize"`
	ExpiresAt       string `json:"expiresAt,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// active reports whether this tier is bypassing anything right now.
func (t PassthroughTier) active() bool {
	return t.Enabled && (t.BypassHooks || t.BypassCache || t.BypassNormalize)
}

// PassthroughSnapshot is the full three-tier emergency-passthrough state: the
// global tier plus any per-adapter and per-provider overrides. ProviderNames maps
// a provider id to its display name so overrides surface by name, not bare id.
type PassthroughSnapshot struct {
	Global        PassthroughTier            `json:"global"`
	Adapters      map[string]PassthroughTier `json:"adapters"`
	Providers     map[string]PassthroughTier `json:"providers"`
	ProviderNames map[string]string          `json:"providerNames"`
}

// ActiveOverrides counts the per-adapter and per-provider tiers currently
// bypassing something — the "is anything slipping past compliance" signal.
func (s PassthroughSnapshot) ActiveOverrides() (adapters, providers int) {
	for _, t := range s.Adapters {
		if t.active() {
			adapters++
		}
	}
	for _, t := range s.Providers {
		if t.active() {
			providers++
		}
	}
	return adapters, providers
}

// PassthroughGlobalRequest is the body for PUT /api/admin/passthrough/global.
// When Enabled, the server requires a future ExpiresAt (≤ 8h out) and a reason of
// at least 20 characters, and rejects BypassNormalize without BypassCache;
// SetPassthroughGlobal fills server-valid defaults for any of these the caller
// omits, so an engage never fails on a missing invariant.
type PassthroughGlobalRequest struct {
	Enabled         bool       `json:"enabled"`
	BypassHooks     bool       `json:"bypassHooks"`
	BypassCache     bool       `json:"bypassCache"`
	BypassNormalize bool       `json:"bypassNormalize"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	Reason          string     `json:"reason,omitempty"`
}

// CacheROIResult is GET /api/admin/analytics/cache-roi — the cache savings
// rollup powering the Cost view's "cache $ saved" line.
type CacheROIResult struct {
	PeriodDays               int     `json:"periodDays"`
	TotalEstimatedCostUSD    float64 `json:"totalEstimatedCostUsd"`
	TotalCacheNetSavingsUSD  float64 `json:"totalCacheNetSavingsUsd"`
	TotalCacheReadSavingsUSD float64 `json:"totalCacheReadSavingsUsd"`
	TotalCacheWriteCostUSD   float64 `json:"totalCacheWriteCostUsd"`
	GatewayCacheHitCount     int     `json:"gatewayCacheHitCount"`
	RequestsWithCacheHit     int     `json:"requestsWithCacheHit"`
}

// FallbacksResult is GET /api/admin/analytics/routing/fallbacks ({data:[...]}).
type FallbacksResult struct {
	Data []FallbackRow `json:"data"`
}

// FallbackRow is one routing-fallback bucket (group is a rule id or a synthetic
// label like "passthrough-fallback"; groupLabel is the resolved human name).
type FallbackRow struct {
	Group        string `json:"group"`
	GroupLabel   string `json:"groupLabel"`
	RequestCount int    `json:"requestCount"`
}

// LatencyPhasesResult is GET /api/admin/analytics/latency-phases
// ({window,rows:[...]}); it powers the Performance/SLO per-provider table.
type LatencyPhasesResult struct {
	Window LatencyWindow     `json:"window"`
	Rows   []LatencyPhaseRow `json:"rows"`
}

// LatencyWindow is the time range the rows were computed over.
type LatencyWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// LatencyPhaseRow is the latency-percentile breakdown for one group (provider /
// model / virtual key). Percentiles are whole milliseconds.
type LatencyPhaseRow struct {
	GroupKey           string `json:"groupKey"`
	GroupLabel         string `json:"groupLabel"`
	RequestCount       int    `json:"requestCount"`
	TotalP50Ms         int    `json:"totalP50Ms"`
	TotalP95Ms         int    `json:"totalP95Ms"`
	TotalP99Ms         int    `json:"totalP99Ms"`
	UpstreamTTFBP50Ms  int    `json:"upstreamTtfbP50Ms"`
	UpstreamTTFBP95Ms  int    `json:"upstreamTtfbP95Ms"`
	UpstreamTotalP50Ms int    `json:"upstreamTotalP50Ms"`
	UpstreamTotalP95Ms int    `json:"upstreamTotalP95Ms"`
	RequestHooksP95Ms  int    `json:"requestHooksP95Ms"`
	ResponseHooksP95Ms int    `json:"responseHooksP95Ms"`
}

// ChatMessage is one turn of an OpenAI-shape chat conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the subset of the OpenAI chat-completions request the Chat
// Playground sends. ChatStream forces streaming + usage on regardless.
type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []ChatMessage  `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions toggles the trailing usage frame on a streamed completion.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatUsage is the token accounting returned in the final stream chunk (and in
// a non-streamed completion). The Playground shows it per turn.
type ChatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// SimulatorForwardRequest is the body POSTed to the CP simulator-forward
// endpoint. The endpoint is admin-authed; the VK is the upstream credential it
// forwards under. An empty TargetURL lets the server default to its gateway.
type SimulatorForwardRequest struct {
	TargetURL string          `json:"targetUrl,omitempty"`
	Path      string          `json:"path"`
	Method    string          `json:"method"`
	VK        string          `json:"vk"`
	Body      json.RawMessage `json:"body,omitempty"`
}

// DLQResult is GET /api/admin/observability/dlq — dead-letter backlog rows.
type DLQResult struct {
	Rows []json.RawMessage `json:"rows"`
}

// NodesResult is GET /api/admin/nodes ({nodes,total,page,pageSize}).
type NodesResult struct {
	Nodes []Node `json:"nodes"`
	Total int    `json:"total"`
}

// Node is one registered node. TargetVersion != AppliedVersion signals drift
// (the node has not yet applied the latest desired config).
type Node struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Status         string `json:"status"`
	Version        string `json:"version"`
	TargetVersion  int    `json:"targetVersion"`
	AppliedVersion int    `json:"appliedVersion"`
	LastSeenAt     string `json:"last_seen_at"`
	PhysicalID     string `json:"physicalId"`
	ConnProtocol   string `json:"conn_protocol"`
}

// Drifted reports whether the node's applied config lags its target.
func (n Node) Drifted() bool { return n.TargetVersion != n.AppliedVersion }

// AlertsResult is GET /api/admin/alerts ({alerts,total}).
type AlertsResult struct {
	Alerts []Alert `json:"alerts"`
	Total  int     `json:"total"`
}

// Alert is one alert instance.
type Alert struct {
	ID             string `json:"id"`
	TargetLabel    string `json:"targetLabel"`
	Severity       string `json:"severity"`
	State          string `json:"state"`
	Message        string `json:"message"`
	FiredAt        string `json:"firedAt"`
	DuplicateCount int    `json:"duplicateCount"`
	ResolvedAt     string `json:"resolvedAt"`
}

// Firing reports whether the alert is currently active (not resolved).
func (a Alert) Firing() bool { return a.ResolvedAt == "" && a.State != "resolved" }

// RoutingSimulateRequest is the body for POST /api/admin/routing-rules/simulate.
type RoutingSimulateRequest struct {
	ModelID      string `json:"modelId"`
	EndpointType string `json:"endpointType"`
}

// RoutingSimulateResult is the routing dry-run outcome ("why this route"). It
// fires no real request.
type RoutingSimulateResult struct {
	Substituted     bool            `json:"substituted"`
	RuleName        string          `json:"ruleName"`
	Targets         []RoutingTarget `json:"targets"`
	RecoveryTargets []RoutingTarget `json:"recoveryTargets"`
	Warnings        []string        `json:"warnings"`
}

// RoutingTarget is one resolved provider/model in the route.
type RoutingTarget struct {
	ProviderName    string `json:"providerName"`
	ModelCode       string `json:"modelCode"`
	ModelName       string `json:"modelName"`
	ProviderModelID string `json:"providerModelId"`
}

// ByProviderResult is GET /api/admin/analytics/by-provider ({data:[...]}).
type ByProviderResult struct {
	Data []ProviderUsageRow `json:"data"`
}

// ProviderUsageRow is one provider's usage rollup (top-talkers + cost view).
type ProviderUsageRow struct {
	Provider        string  `json:"provider"`
	ProviderLabel   string  `json:"providerLabel"`
	RequestCount    int     `json:"requestCount"`
	AvgLatencyMs    float64 `json:"avgLatencyMs"`
	TotalTokens     int     `json:"totalTokens"`
	TotalEstCostUSD float64 `json:"totalEstimatedCostUsd"`
}

// ComplianceOverview is GET /api/admin/compliance/overview — compliance KPIs.
type ComplianceOverview struct {
	KPIs ComplianceKPIs `json:"kpis"`
}

// ComplianceKPIs are the headline compliance numbers.
type ComplianceKPIs struct {
	TotalRequests    int     `json:"totalRequests"`
	TotalBlocked     int     `json:"totalBlocked"`
	OverallBlockRate float64 `json:"overallBlockRate"`
	TLSCoveragePct   float64 `json:"tlsCoveragePercent"`
	HookErrorRate    float64 `json:"hookErrorRate"`
}

// JobsResult is GET /api/admin/jobs ({jobs:[...]}).
type JobsResult struct {
	Jobs []Job `json:"jobs"`
}

// Job is one scheduled background job. Interval is nanoseconds.
type Job struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Interval int64  `json:"interval"`
	Enabled  bool   `json:"enabled"`
	LastRun  string `json:"lastRun"`
}

// ConfigSyncResult is GET /api/admin/config-sync/out-of-sync — nodes whose
// applied config lags the target ({outOfSync:[...],total}).
type ConfigSyncResult struct {
	OutOfSync []json.RawMessage `json:"outOfSync"`
	Total     int               `json:"total"`
}

// ProviderDetail is GET /api/admin/analytics/provider/:id — a single provider's
// SLO detail. Only the summary (the headline) is modeled.
type ProviderDetail struct {
	Summary ProviderDetailSummary `json:"summary"`
}

// ProviderDetailSummary is the per-provider headline (availability + latency).
type ProviderDetailSummary struct {
	TotalRequests     int     `json:"totalRequests"`
	ErrorCount        int     `json:"errorCount"`
	ErrorRate         float64 `json:"errorRate"`
	CacheHitRate      float64 `json:"cacheHitRate"`
	AvgLatencyMs      float64 `json:"avgLatencyMs"`
	AvgUpstreamTTFBMs float64 `json:"avgUpstreamTtfbMs"`
	TotalEstCostUSD   float64 `json:"totalEstimatedCostUsd"`
}

// routingRuleList is the {data:[...]} envelope wrapping the routing-rule list.
type routingRuleList struct {
	Data []RoutingRule `json:"data"`
}

// RoutingRule is one row of GET /api/admin/routing-rules. The toolkit shows the
// identity + ordering + enabled state; the strategy config blobs are not needed
// for the toggle surface, so they are deliberately not decoded here.
type RoutingRule struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	StrategyType  string `json:"strategyType"`
	Priority      int    `json:"priority"`
	PipelineStage int    `json:"pipelineStage"`
	Enabled       bool   `json:"enabled"`
}

// TrafficFilter holds the supported query filters for TrafficList. Zero-valued
// fields are omitted from the query string.
type TrafficFilter struct {
	StatusRange     string // e.g. "4xx", "5xx", "error"
	Provider        string
	ModelUsed       string
	VirtualKeyID    string
	Source          string
	StartTime       time.Time
	EndTime         time.Time
	Limit           int
	Offset          int
	ExcludeInternal *bool
}
