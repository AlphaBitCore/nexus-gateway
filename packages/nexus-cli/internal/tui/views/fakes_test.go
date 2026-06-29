package views

import (
	"context"
	"github.com/goccy/go-json"
	"net/url"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

type fakeGateway struct {
	sp         *core.SparklineResult
	inst       *core.InstancesResult
	list       *core.TrafficList
	ev         *core.TrafficEvent
	phases     *core.LatencyPhasesResult
	fallbacks  *core.FallbacksResult
	cost       *core.CostReport
	roi        *core.CacheROIResult
	models     *core.ModelCatalog
	vks        []core.VirtualKey
	chatText   string
	chatUsage  *core.ChatUsage
	simResp    json.RawMessage
	killRes    *core.KillSwitchResult
	normalized json.RawMessage
	dlq        *core.DLQResult
	alerts     *core.AlertsResult
	nodes      *core.NodesResult
	route      *core.RoutingSimulateResult
	byProvider *core.ByProviderResult
	compliance *core.ComplianceOverview
	jobs       *core.JobsResult
	configSync *core.ConfigSyncResult
	provDetail *core.ProviderDetail
	providers  *core.ProvidersResult
	rules      []core.RoutingRule
	regen      *core.RegeneratedVK
	ksState    *core.KillSwitchState
	passSnap   *core.PassthroughSnapshot

	lastProviderEnabled *bool
	cacheFlushed        bool
	lastRevokedVK       string
	lastRegeneratedVK   string
	lastRuleID          string
	lastRuleEnabled     *bool
	lastPassthrough     *core.PassthroughGlobalRequest
	err                 error

	// /resource cascade: scripted admin response + the last call recorded.
	adminRaw    json.RawMessage
	adminStatus int
	adminCalls  int
	lastAdmin   struct {
		method string
		path   string
		query  url.Values
		body   any
	}
}

func (f *fakeGateway) AdminRequest(_ context.Context, method, path string, query url.Values, body any) (json.RawMessage, int, error) {
	f.adminCalls++
	f.lastAdmin.method, f.lastAdmin.path, f.lastAdmin.query, f.lastAdmin.body = method, path, query, body
	if f.err != nil {
		return nil, 0, f.err
	}
	status := f.adminStatus
	if status == 0 {
		status = 200
	}
	return f.adminRaw, status, nil
}

func (f *fakeGateway) KillSwitchStatus(context.Context) (*core.KillSwitchState, error) {
	return f.ksState, f.err
}

func (f *fakeGateway) PassthroughSnapshot(context.Context) (*core.PassthroughSnapshot, error) {
	return f.passSnap, f.err
}

func (f *fakeGateway) SetPassthroughGlobal(_ context.Context, req core.PassthroughGlobalRequest) error {
	f.lastPassthrough = &req
	return f.err
}

func (f *fakeGateway) TrafficEventNormalized(context.Context, string) (json.RawMessage, error) {
	if f.normalized == nil {
		return json.RawMessage(`{"kind":"ai-chat","text":"hello"}`), f.err
	}
	return f.normalized, f.err
}

func (f *fakeGateway) Sparkline(context.Context, url.Values) (*core.SparklineResult, error) {
	return f.sp, f.err
}

func (f *fakeGateway) Instances(context.Context) (*core.InstancesResult, error) {
	return f.inst, f.err
}

func (f *fakeGateway) DLQ(context.Context) (*core.DLQResult, error) {
	return f.dlq, f.err
}

func (f *fakeGateway) TrafficList(context.Context, core.TrafficFilter) (*core.TrafficList, error) {
	return f.list, f.err
}

func (f *fakeGateway) TrafficEvent(context.Context, string) (*core.TrafficEvent, error) {
	return f.ev, f.err
}

func (f *fakeGateway) LatencyPhases(context.Context, string, url.Values) (*core.LatencyPhasesResult, error) {
	return f.phases, f.err
}

func (f *fakeGateway) RoutingFallbacks(context.Context, url.Values) (*core.FallbacksResult, error) {
	return f.fallbacks, f.err
}

func (f *fakeGateway) ProviderDetail(context.Context, string, url.Values) (*core.ProviderDetail, error) {
	return f.provDetail, f.err
}

func (f *fakeGateway) Providers(context.Context) (*core.ProvidersResult, error) {
	return f.providers, f.err
}

func (f *fakeGateway) Cost(context.Context, url.Values) (*core.CostReport, error) {
	return f.cost, f.err
}

func (f *fakeGateway) CacheROI(context.Context, url.Values) (*core.CacheROIResult, error) {
	return f.roi, f.err
}

func (f *fakeGateway) AdminModels(context.Context) (*core.ModelCatalog, error) {
	return f.models, f.err
}

func (f *fakeGateway) VirtualKeys(context.Context) ([]core.VirtualKey, error) {
	return f.vks, f.err
}

func (f *fakeGateway) Alerts(context.Context) (*core.AlertsResult, error) {
	return f.alerts, f.err
}

func (f *fakeGateway) Nodes(context.Context) (*core.NodesResult, error) {
	return f.nodes, f.err
}

func (f *fakeGateway) ByProvider(context.Context, url.Values) (*core.ByProviderResult, error) {
	return f.byProvider, f.err
}

func (f *fakeGateway) ComplianceOverview(context.Context, url.Values) (*core.ComplianceOverview, error) {
	return f.compliance, f.err
}

func (f *fakeGateway) Jobs(context.Context) (*core.JobsResult, error) {
	return f.jobs, f.err
}

func (f *fakeGateway) ConfigSyncOutOfSync(context.Context) (*core.ConfigSyncResult, error) {
	return f.configSync, f.err
}

func (f *fakeGateway) ChatStream(_ context.Context, _ string, _ core.ChatRequest, onDelta func(string)) (*core.ChatUsage, error) {
	if f.err != nil {
		return nil, f.err
	}
	if onDelta != nil && f.chatText != "" {
		onDelta(f.chatText)
	}
	return f.chatUsage, nil
}

func (f *fakeGateway) SimulatorForward(context.Context, core.SimulatorForwardRequest) (json.RawMessage, error) {
	return f.simResp, f.err
}

func (f *fakeGateway) RoutingSimulate(context.Context, core.RoutingSimulateRequest) (*core.RoutingSimulateResult, error) {
	return f.route, f.err
}

func (f *fakeGateway) SetProviderEnabled(_ context.Context, _ string, enabled bool) error {
	f.lastProviderEnabled = &enabled
	return f.err
}

func (f *fakeGateway) CacheFlush(context.Context) error {
	f.cacheFlushed = true
	return f.err
}

func (f *fakeGateway) RoutingRules(context.Context) ([]core.RoutingRule, error) {
	return f.rules, f.err
}

func (f *fakeGateway) RevokeVK(_ context.Context, id string) error {
	f.lastRevokedVK = id
	return f.err
}

func (f *fakeGateway) RegenerateVK(_ context.Context, id string) (*core.RegeneratedVK, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastRegeneratedVK = id
	if f.regen != nil {
		return f.regen, nil
	}
	return &core.RegeneratedVK{ID: id, KeyPrefix: "nvk_new", Key: "nvk_rotated_secret"}, nil
}

func (f *fakeGateway) SetRoutingRuleEnabled(_ context.Context, id string, enabled bool) error {
	f.lastRuleID = id
	f.lastRuleEnabled = &enabled
	return f.err
}

func (f *fakeGateway) SetKillSwitch(_ context.Context, engaged bool) (*core.KillSwitchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.killRes != nil {
		return f.killRes, nil
	}
	return &core.KillSwitchResult{Engaged: engaged, Version: 7, ThingsNotified: 4, ThingsOnline: 5}, nil
}

func sampleGateway() *fakeGateway {
	return &fakeGateway{
		sp: &core.SparklineResult{Granularity: "1h", Series: []core.SparklineBucket{
			{Values: map[string]float64{"request_count": 20, "estimated_cost_usd": 1.0, "total_tokens": 600, "cache_hit_count": 2, "status_5xx_count": 1}},
			{Values: map[string]float64{"request_count": 22, "estimated_cost_usd": 0.5, "total_tokens": 400, "cache_hit_count": 1, "status_5xx_count": 1}},
		}},
		inst: &core.InstancesResult{Count: 27, Services: map[string]core.ServiceSummary{"ai-gateway": {Total: 3}, "nexus-hub": {Total: 2}}},
		list: &core.TrafficList{Total: 2, Data: []core.TrafficEvent{
			{ID: "ev1", StatusCode: 200, ModelName: "gpt-4", TotalTokens: 10, EstCostUSD: 0.01, Timestamp: time.Now()},
			{ID: "ev2", StatusCode: 500, ModelName: "claude", TotalTokens: 20, EstCostUSD: 0.02, Timestamp: time.Now()},
		}},
		ev:        &core.TrafficEvent{ID: "ev1", StatusCode: 200, ModelName: "gpt-4", ProviderName: "openai", TotalTokens: 42, PromptTokens: 30, CompletionTok: 12, EstCostUSD: 0.001, TraceID: "tr-9", LatencyMs: 100, UpstreamTTFBMs: 40, UpstreamTotMs: 80, RequestHooksMs: 5, RespHooksMs: 5},
		phases:    &core.LatencyPhasesResult{Rows: []core.LatencyPhaseRow{{GroupKey: "openai", GroupLabel: "OpenAI", RequestCount: 173, TotalP50Ms: 1245, TotalP95Ms: 90008, UpstreamTTFBP95Ms: 13567}}},
		fallbacks: &core.FallbacksResult{Data: []core.FallbackRow{{Group: "passthrough-fallback", GroupLabel: "Passthrough", RequestCount: 516}}},
		cost:      &core.CostReport{Total: 1, Data: []core.CostRow{{Group: "openai", GroupLabel: "OpenAI", RequestCount: 111, TotalTokens: 297738, TotalCostUSD: 1.3373, CacheHitCount: 5}}},
		roi:       &core.CacheROIResult{PeriodDays: 8, TotalEstimatedCostUSD: 3.96, TotalCacheNetSavingsUSD: 2.11, RequestsWithCacheHit: 210},
		models:    &core.ModelCatalog{Data: []core.ModelGroup{{Provider: core.Provider{Name: "OpenAI"}, Models: []core.Model{{Code: "gpt-4o-mini", Name: "GPT-4o mini", Type: "chat", Enabled: true, MaxContextTokens: 128000, InputPricePerMillion: 0.15, OutputPricePerMillion: 0.60}}}}},
		vks:       []core.VirtualKey{{ID: "vk1", Name: "engineering", KeyPrefix: "nvk_eng", Enabled: true}},
		chatText:  "Hello world",
		chatUsage: &core.ChatUsage{PromptTokens: 11, CompletionTokens: 8, TotalTokens: 19},
		simResp:   json.RawMessage(`{"choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":17}}`),
		dlq:       &core.DLQResult{},
		byProvider: &core.ByProviderResult{Data: []core.ProviderUsageRow{
			{Provider: "p1", ProviderLabel: "OpenAI", RequestCount: 111, AvgLatencyMs: 1500, TotalTokens: 297738, TotalEstCostUSD: 1.3373},
		}},
		compliance: &core.ComplianceOverview{KPIs: core.ComplianceKPIs{TotalRequests: 586, TotalBlocked: 7, OverallBlockRate: 0.0119}},
		jobs:       &core.JobsResult{Jobs: []core.Job{{ID: "j1", Name: "Cert Alerts", Interval: 3600000000000, Enabled: true, LastRun: "2026-05-28T11:30:20Z"}}},
		configSync: &core.ConfigSyncResult{Total: 0},
		provDetail: &core.ProviderDetail{Summary: core.ProviderDetailSummary{
			TotalRequests: 173, ErrorCount: 4, ErrorRate: 0.0231, CacheHitRate: 0.18,
			AvgLatencyMs: 1450, AvgUpstreamTTFBMs: 380, TotalEstCostUSD: 1.3373,
		}},
		providers: &core.ProvidersResult{Data: []core.Provider{
			{ID: "prov-openai", Name: "openai", DisplayName: "OpenAI", Enabled: true},
		}},
		rules: []core.RoutingRule{
			{ID: "r1", Name: "Cheap default", StrategyType: "smart", Priority: 10, PipelineStage: 1, Enabled: true},
		},
		ksState: &core.KillSwitchState{Engaged: false, Known: true, Version: 3, By: "admin"},
		passSnap: &core.PassthroughSnapshot{
			Global:    core.PassthroughTier{},
			Adapters:  map[string]core.PassthroughTier{},
			Providers: map[string]core.PassthroughTier{},
		},
	}
}

func testSession() kit.Session {
	return kit.Session{EnvName: "local", Addr: "http://localhost:3001", Model: "gpt-4o-mini", VKName: "engineering", VKSecret: "nvk_secret"}
}

func keyRunes(s string) tea.KeyPressMsg {
	r := []rune(s)
	k := tea.KeyPressMsg{Text: s}
	if len(r) == 1 {
		k.Code = r[0]
	}
	return k
}

func testSessionProd() kit.Session { return kit.Session{EnvName: "prod", IsProd: true} }

func testSessionLocal() kit.Session { return kit.Session{EnvName: "local"} }
