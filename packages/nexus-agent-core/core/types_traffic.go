package core

import (
	"encoding/json"
	"time"
)

// Wire structs for the admin-authed capabilities. Field sets match the
// Control Plane admin API JSON; unmodeled fields are ignored on decode. Shapes
// are verified against live responses by the live-tagged round-trip test
// (go test -tags live), so drift surfaces as empty values rather than silent
// corruption. The structs are split by domain across types_*.go; this file holds
// traffic + infrastructure/observability (nodes, alerts, jobs, config-sync, DLQ).

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
