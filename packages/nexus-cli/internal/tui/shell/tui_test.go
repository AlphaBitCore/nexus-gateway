package shell

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	viewpkg "github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/views"
	"github.com/goccy/go-json"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeGateway returns canned data (or errs) for the views under test. The
// chat/sim/kill fields let write-path views be exercised without a network.
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

// testSession is the common dashboard session for view tests.
func testSession() Session {
	return Session{EnvName: "local", Addr: "http://localhost:3001", Model: "gpt-4o-mini", VKName: "engineering", VKSecret: "nvk_secret"}
}

func TestModel_NavAndQuit(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	if m.Init() == nil {
		t.Fatal("Init should start the first view's fetch")
	}
	// window size
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = m2.(Model)
	if m.width != 120 || m.height != 40 {
		t.Fatal("window size not stored")
	}
	// tab toggles focus to the canvas (it no longer cycles views).
	m2, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = m2.(Model)
	if m.focus != focusCanvas {
		t.Fatalf("tab should focus the canvas, got %v", m.focus)
	}
	// canvas-focused number keys jump views: '3' → Event (index 2).
	m2, _ = m.Update(keyRunes("3"))
	m = m2.(Model)
	if m.active != 2 {
		t.Fatalf("'3' → active %d, want 2", m.active)
	}
	// '2' → Radar (index 1).
	m2, _ = m.Update(keyRunes("2"))
	m = m2.(Model)
	if m.active != 1 {
		t.Fatalf("'2' → active %d, want 1", m.active)
	}
	// quit (canvas-focused 'q').
	m2, cmd := m.Update(keyRunes("q"))
	m = m2.(Model)
	if !m.quitting || cmd == nil {
		t.Fatal("q should set quitting and return a quit cmd")
	}
	if m.View().Content != "" {
		t.Fatal("quitting view should be empty")
	}
}

func TestModel_OpenEvent(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m2, cmd := m.Update(kit.OpenEventMsg{ID: "ev1"})
	m = m2.(Model)
	if m.active != m.indexOf("Event") || cmd == nil {
		t.Fatal("openEventMsg should switch to Event and start its fetch")
	}
}

func TestModel_View_ProdBanner(t *testing.T) {
	prod := NewModel(sampleGateway(), Session{EnvName: "prod", IsProd: true})
	prod2, _ := prod.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	out := prod2.(Model).View().Content
	if !strings.Contains(out, "PROD") {
		t.Fatalf("prod model must show PROD banner:\n%s", out)
	}
	nonprod := NewModel(sampleGateway(), testSession())
	np2, _ := nonprod.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	out = np2.(Model).View().Content
	// Non-prod has no top bar; the env shows bottom-right in the footer.
	if strings.Contains(out, "PROD") || !strings.Contains(out, "local") {
		t.Fatalf("non-prod model must not show PROD; env 'local' must appear bottom-right:\n%s", out)
	}
}

func TestModel_NumberKeyNav(t *testing.T) {
	// With the full registry, "9" maps to a valid view (index 8 = Alerts).
	m := NewModel(sampleGateway(), testSession())
	m.focus = focusCanvas // number-jump is a canvas-focus action
	m2, _ := m.Update(keyRunes("9"))
	if m2.(Model).active != 8 {
		t.Fatalf("'9' should navigate to view index 8, got %d", m2.(Model).active)
	}
}

func TestModel_OutOfRangeNumberKeyIgnored(t *testing.T) {
	// A small model (2 views): "9" is out of range and must not switch.
	g := sampleGateway()
	m := Model{entries: []viewEntry{{name: "A"}, {name: "B"}}, views: []viewModel{viewpkg.NewCockpit(g), viewpkg.NewRadar(g)}, focus: focusCanvas}
	m2, _ := m.Update(keyRunes("9"))
	if m2.(Model).active != 0 {
		t.Fatal("out-of-range number key should not change the active view")
	}
}

func TestApp_ResidentChatOwnsKeysWhenFocused(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = m2.(Model)
	// Chat is focused by default; a keystroke edits its prompt.
	updated, _ := m.Update(keyRunes("z"))
	mm := updated.(Model)
	if mm.conv.input.Value() != "z" {
		t.Fatalf("the chat should own keystrokes while focused, got %q", mm.conv.input.Value())
	}
	if !strings.Contains(mm.View().Content, "Conversation") {
		t.Fatal("the resident chat should render its pane")
	}
	// Idle esc moves focus to the canvas (the chat stays resident).
	updated, _ = mm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if updated.(Model).focus != focusCanvas {
		t.Fatal("idle esc should move focus to the canvas")
	}
}

func TestApp_AgentNavAppliesRadarFilter(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	updated, _ := m.Update(agentNavMsg{view: "Radar", filter: core.TrafficFilter{Provider: "openai", StatusRange: "5xx"}})
	mm := updated.(Model)
	if mm.entries[mm.active].name != "Radar" {
		t.Fatalf("expected active Radar, got %s", mm.entries[mm.active].name)
	}
	r := mm.views[mm.active].(*viewpkg.Radar)
	if r.Filter().Provider != "openai" || r.Filter().StatusRange != "5xx" {
		t.Fatalf("agent navigate filter not applied to radar: %+v", r.Filter())
	}
}

func TestResolveViewIndex(t *testing.T) {
	entries := []viewEntry{{name: "Radar", aliases: []string{"traffic"}}, {name: "Cost"}}
	if i := resolveViewIndex(entries, "Cost"); i != 1 {
		t.Fatalf("exact match Cost -> %d", i)
	}
	if i := resolveViewIndex(entries, "traffic"); i != 0 {
		t.Fatalf("alias traffic should resolve to Radar (0), got %d", i)
	}
	if i := resolveViewIndex(entries, "  "); i != -1 {
		t.Fatalf("blank name -> -1, got %d", i)
	}
	if i := resolveViewIndex(entries, "nope"); i != -1 {
		t.Fatalf("unknown name -> -1, got %d", i)
	}
}

func TestApp_ConversationCtrlCQuits(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	u, _ := m.Update(keyRunes(">"))
	m = u.(Model)
	u, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !u.(Model).quitting || cmd == nil {
		t.Fatal("ctrl+c while the conversation is open should quit")
	}
}

func TestApp_AgentNavTearsDownStreamingView(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	// Make the Chat view (a leaver) active, then have the agent navigate away.
	m.active = resolveViewIndex(m.entries, "Chat")
	updated, _ := m.Update(agentNavMsg{view: "Cost"})
	mm := updated.(Model)
	if mm.entries[mm.active].name != "Cost" {
		t.Fatalf("agent navigate from a streaming view should still land on Cost, got %s", mm.entries[mm.active].name)
	}
}

func TestApp_AgentNavUnknownViewNoOp(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	before := m.active
	updated, cmd := m.Update(agentNavMsg{view: "NoSuchView"})
	// Unknown view: active unchanged, and (no agent built) the drain cmd is nil.
	if updated.(Model).active != before || cmd != nil {
		t.Fatal("an unknown agent-nav view should be a no-op")
	}
}

func TestApp_OpenEventExplainArmsAutoExplain(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	updated, cmd := m.Update(kit.OpenEventMsg{ID: "ev1", Explain: true})
	mm := updated.(Model)
	ev := mm.views[mm.active].(*viewpkg.EventView)
	if mm.entries[mm.active].name != "Event" || ev.ID() != "ev1" || !ev.AutoExplain() {
		t.Fatalf("explain intent should open Event ev1 with auto-explain armed: active=%s id=%s auto=%v",
			mm.entries[mm.active].name, ev.ID(), ev.AutoExplain())
	}
	if cmd == nil {
		t.Fatal("opening the event should start its fetch")
	}
}

func TestRun_HeadlessQuits(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	done := make(chan error, 1)
	go func() {
		// ctrl+c always quits regardless of pane focus (chat is the launch default,
		// so a bare 'q' would be typed into the prompt, not quit).
		done <- run(m, tea.WithInput(strings.NewReader("\x03")), tea.WithOutput(io.Discard))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("program did not quit on ctrl+c within 5s")
	}
}
