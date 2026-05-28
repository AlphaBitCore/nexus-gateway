package tui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// fakeGateway returns canned data (or errs) for the views under test. The
// chat/sim/kill fields let write-path views be exercised without a network.
type fakeGateway struct {
	sp        *core.SparklineResult
	inst      *core.InstancesResult
	list      *core.TrafficList
	ev        *core.TrafficEvent
	phases    *core.LatencyPhasesResult
	fallbacks *core.FallbacksResult
	cost      *core.CostReport
	roi       *core.CacheROIResult
	models    *core.ModelCatalog
	vks       []core.VirtualKey
	chatText  string
	chatUsage *core.ChatUsage
	simResp   json.RawMessage
	killRes   *core.KillSwitchResult
	normalized json.RawMessage
	dlq       *core.DLQResult
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
		models:    &core.ModelCatalog{Data: []core.ModelGroup{{Provider: core.ProviderRef{Name: "OpenAI"}, Models: []core.Model{{Code: "gpt-4o-mini", Name: "GPT-4o mini", Type: "chat", Enabled: true, MaxContextTokens: 128000, InputPricePerMillion: 0.15, OutputPricePerMillion: 0.60}}}}},
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

// --- root model ---

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
	// tab forward → Radar
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = m2.(Model)
	if m.active != 1 {
		t.Fatalf("tab → active %d, want 1", m.active)
	}
	// number key → Event (3)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	m = m2.(Model)
	if m.active != 2 {
		t.Fatalf("'3' → active %d, want 2", m.active)
	}
	// shift+tab wraps back to Radar
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = m2.(Model)
	if m.active != 1 {
		t.Fatalf("shift+tab → active %d, want 1", m.active)
	}
	// quit
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m = m2.(Model)
	if !m.quitting || cmd == nil {
		t.Fatal("q should set quitting and return a quit cmd")
	}
	if m.View() != "" {
		t.Fatal("quitting view should be empty")
	}
}

func TestModel_OpenEvent(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m2, cmd := m.Update(openEventMsg{id: "ev1"})
	m = m2.(Model)
	if m.active != m.eventIndex() || cmd == nil {
		t.Fatal("openEventMsg should switch to Event and start its fetch")
	}
}

func TestModel_View_ProdBanner(t *testing.T) {
	prod := NewModel(sampleGateway(), Session{EnvName: "prod", IsProd: true})
	prod2, _ := prod.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	out := prod2.(Model).View()
	if !strings.Contains(out, "PROD") {
		t.Fatalf("prod model must show PROD banner:\n%s", out)
	}
	nonprod := NewModel(sampleGateway(), testSession())
	np2, _ := nonprod.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	out = np2.(Model).View()
	if strings.Contains(out, "PROD") || !strings.Contains(out, "ENV local") {
		t.Fatalf("non-prod model must not show PROD; want ENV local:\n%s", out)
	}
}

func TestModel_NumberKeyNav(t *testing.T) {
	// With the full registry, "9" maps to a valid view (index 8 = Alerts).
	m := NewModel(sampleGateway(), testSession())
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	if m2.(Model).active != 8 {
		t.Fatalf("'9' should navigate to view index 8, got %d", m2.(Model).active)
	}
}

func TestModel_OutOfRangeNumberKeyIgnored(t *testing.T) {
	// A small model (2 views): "9" is out of range and must not switch.
	g := sampleGateway()
	m := Model{tabs: []string{"A", "B"}, views: []viewModel{newOverview(g), newRadar(g)}}
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	if m2.(Model).active != 0 {
		t.Fatal("out-of-range number key should not change the active view")
	}
}

// --- overview ---

func TestOverview_FetchUpdateView(t *testing.T) {
	o := newOverview(sampleGateway())
	msg := o.Init()() // execute the fetch cmd → overviewMsg
	om, ok := msg.(overviewMsg)
	if !ok || om.sp == nil || om.inst == nil {
		t.Fatalf("fetch should return populated overviewMsg, got %#v", msg)
	}
	v, cmd := o.Update(om)
	if cmd == nil {
		t.Fatal("overview Update should schedule the next poll tick")
	}
	out := v.View(120, 20)
	for _, want := range []string{"Requests", "42", "Cost USD", "Services", "ai-gateway"} {
		if !strings.Contains(out, want) {
			t.Errorf("overview view missing %q:\n%s", want, out)
		}
	}
	// tick triggers a refetch.
	if _, cmd := v.Update(overviewTick{}); cmd == nil {
		t.Fatal("overviewTick should refetch")
	}
}

func TestOverview_Error(t *testing.T) {
	o := newOverview(&fakeGateway{err: errors.New("boom")})
	msg := o.Init()()
	v, _ := o.Update(msg)
	if !strings.Contains(v.View(80, 20), "boom") {
		t.Fatal("overview should surface the error")
	}
}

func TestOverview_LoadingAndEmptyServices(t *testing.T) {
	o := newOverview(sampleGateway())
	if !strings.Contains(o.View(80, 10), "loading") {
		t.Fatal("initial overview should show loading")
	}
	o2 := newOverview(&fakeGateway{sp: &core.SparklineResult{}, inst: &core.InstancesResult{}})
	v, _ := o2.Update(o2.Init()())
	if !strings.Contains(v.View(80, 10), "none reported") {
		t.Fatal("empty services should render placeholder")
	}
}

// --- radar ---

func TestRadar_FetchNavOpen(t *testing.T) {
	r := newRadar(sampleGateway())
	v, cmd := r.Update(r.Init()())
	if cmd == nil {
		t.Fatal("radar Update should schedule a poll tick")
	}
	out := v.View(120, 20)
	for _, want := range []string{"Live traffic", "TIME", "gpt-4", "claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("radar view missing %q:\n%s", want, out)
		}
	}
	// move cursor down then open.
	v, _ = v.Update(tea.KeyMsg{Type: tea.KeyDown})
	rv := v.(*radar)
	if rv.cursor != 1 {
		t.Fatalf("cursor = %d, want 1", rv.cursor)
	}
	_, openCmd := rv.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if openCmd == nil || openCmd() != (openEventMsg{id: "ev2"}) {
		t.Fatal("enter should emit openEventMsg for the selected row")
	}
	// up clamps at 0.
	v, _ = rv.Update(tea.KeyMsg{Type: tea.KeyUp})
	if v.(*radar).cursor != 0 {
		t.Fatal("up should move cursor to 0")
	}
}

func TestRadar_FilterToggleAndError(t *testing.T) {
	r := newRadar(sampleGateway())
	r.Update(r.Init()())
	v, cmd := r.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if !v.(*radar).errorsOnly || cmd == nil {
		t.Fatal("f should toggle errorsOnly and refetch")
	}
	if !strings.Contains(v.View(120, 20), "errors only") {
		t.Fatal("filtered radar should show 'errors only'")
	}
	// error path
	re := newRadar(&fakeGateway{err: errors.New("down")})
	v, _ = re.Update(re.Init()())
	if !strings.Contains(v.View(120, 20), "down") {
		t.Fatal("radar should surface fetch error")
	}
}

func TestApp_OpensAskBar(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'>'}})
	mm := updated.(Model)
	if !mm.askOpen {
		t.Fatal("> should open the ask bar")
	}
	// While open, a keystroke is owned by the ask bar (edits its input).
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	mm = updated.(Model)
	if mm.ask.input.Value() != "z" {
		t.Fatalf("ask bar should own keystrokes while open, got %q", mm.ask.input.Value())
	}
	if !strings.Contains(mm.View(), "Ask Nexus") {
		t.Fatal("open ask bar should render in the footer")
	}
	// askCloseMsg closes it.
	updated, _ = mm.Update(askCloseMsg{})
	if updated.(Model).askOpen {
		t.Fatal("askCloseMsg should close the ask bar")
	}
}

func TestApp_NavigateMsgAppliesRadarFilter(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	updated, _ := m.Update(navigateMsg{view: "Radar", filter: &askFilter{Provider: "openai", Status: "5xx"}})
	mm := updated.(Model)
	if mm.tabs[mm.active] != "Radar" {
		t.Fatalf("expected active Radar, got %s", mm.tabs[mm.active])
	}
	r := mm.views[mm.active].(*radar)
	if r.base.Provider != "openai" || r.base.StatusRange != "5xx" {
		t.Fatalf("navigate filter not applied to radar: %+v", r.base)
	}
	if mm.askOpen {
		t.Fatal("navigate should close the ask bar")
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

func TestApp_AskBarCtrlCQuits(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'>'}})
	m = u.(Model)
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !u.(Model).quitting || cmd == nil {
		t.Fatal("ctrl+c while the ask bar is open should quit")
	}
}

func TestApp_AskFlowNavigatesEndToEnd(t *testing.T) {
	gw := newAskFake(`{"action":"navigate","view":"Cost"}`, "")
	m := NewModel(gw, testSession())
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'>'}})
	m = u.(Model)
	for _, r := range "show cost" {
		u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = u.(Model)
	}
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = u.(Model)
	// Drive the ask stream messages through the model until navigate lands; stop
	// before running the target view's Init (its poll tick would block the test).
	for i := 0; cmd != nil && i < 20; i++ {
		msg := cmd()
		if _, ok := msg.(navigateMsg); ok {
			u, _ = m.Update(msg)
			m = u.(Model)
			break
		}
		u, cmd = m.Update(msg)
		m = u.(Model)
	}
	if m.tabs[m.active] != "Cost" {
		t.Fatalf("end-to-end ask navigate should land on Cost, got %s", m.tabs[m.active])
	}
	if m.askOpen {
		t.Fatal("a completed navigate should close the ask bar")
	}
}

func TestApp_NavigateTearsDownStreamingView(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	// Make the Chat view (a leaver) active, then navigate away via an ask intent.
	chatIdx := resolveViewIndex(m.entries, "Chat")
	m.active = chatIdx
	updated, _ := m.Update(navigateMsg{view: "Cost"})
	mm := updated.(Model)
	if mm.tabs[mm.active] != "Cost" {
		t.Fatalf("navigate from a streaming view should still land on Cost, got %s", mm.tabs[mm.active])
	}
}

func TestApp_NavigateMsgUnknownViewNoOp(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	before := m.active
	updated, cmd := m.Update(navigateMsg{view: "NoSuchView"})
	if updated.(Model).active != before || cmd != nil {
		t.Fatal("an unknown view name should be a no-op")
	}
}

func TestApp_OpenEventExplainArmsAutoExplain(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	updated, cmd := m.Update(openEventMsg{id: "ev1", explain: true})
	mm := updated.(Model)
	ev := mm.views[mm.active].(*eventView)
	if mm.tabs[mm.active] != "Event" || ev.id != "ev1" || !ev.autoExplain {
		t.Fatalf("explain intent should open Event ev1 with auto-explain armed: active=%s id=%s auto=%v",
			mm.tabs[mm.active], ev.id, ev.autoExplain)
	}
	if cmd == nil {
		t.Fatal("opening the event should start its fetch")
	}
}

func TestRadar_ApplyFilter(t *testing.T) {
	r := newRadar(sampleGateway())
	r.cursor = 3
	r.applyFilter(core.TrafficFilter{Provider: "openai", StatusRange: "5xx"})
	if r.base.Provider != "openai" || r.base.StatusRange != "5xx" {
		t.Fatalf("applyFilter did not set base: %+v", r.base)
	}
	if r.base.Limit != 20 {
		t.Fatalf("applyFilter should default Limit, got %d", r.base.Limit)
	}
	if r.cursor != 0 {
		t.Fatalf("applyFilter should reset cursor, got %d", r.cursor)
	}
	if out := r.View(120, 20); !strings.Contains(out, "provider=openai") || !strings.Contains(out, "5xx") {
		t.Fatalf("filtered radar header should show the provider and status range:\n%s", out)
	}
	// An error-range filter syncs the errorsOnly display flag.
	r.applyFilter(core.TrafficFilter{StatusRange: "error"})
	if !r.errorsOnly {
		t.Fatal("an error-range navigate should set errorsOnly")
	}
}

func TestRadar_EmptyAndLoading(t *testing.T) {
	r := newRadar(sampleGateway())
	if !strings.Contains(r.View(80, 10), "loading") {
		t.Fatal("initial radar shows loading")
	}
	empty := newRadar(&fakeGateway{list: &core.TrafficList{}})
	v, _ := empty.Update(empty.Init()())
	if !strings.Contains(v.View(80, 10), "no events") {
		t.Fatal("empty radar shows placeholder")
	}
	// enter with no rows emits nothing.
	if _, cmd := v.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("enter with no rows should not emit")
	}
}

// --- event ---

func TestEvent_LoadAndWaterfall(t *testing.T) {
	e := newEvent(sampleGateway(), testSession())
	if !strings.Contains(e.View(80, 20), "Select an event") {
		t.Fatal("event with no id shows prompt")
	}
	e.setID("ev1")
	if !strings.Contains(e.View(80, 20), "loading") {
		t.Fatal("after setID, shows loading")
	}
	v, _ := e.Update(e.Init()())
	out := v.View(120, 30)
	for _, want := range []string{"ev1", "tr-9", "Latency waterfall", "upstream ttfb", "total"} {
		if !strings.Contains(out, want) {
			t.Errorf("event view missing %q:\n%s", want, out)
		}
	}
}

func TestEvent_ErrorAndNoLatency(t *testing.T) {
	e := newEvent(&fakeGateway{err: errors.New("nope")}, testSession())
	e.setID("x")
	v, _ := e.Update(e.Init()())
	if !strings.Contains(v.View(80, 20), "nope") {
		t.Fatal("event should surface error")
	}
	// no-latency event → waterfall placeholder
	flat := &core.TrafficEvent{ID: "z", CacheStatus: ""}
	if !strings.Contains(latencyWaterfall(flat), "no latency data") {
		t.Fatal("zero-latency event should show placeholder")
	}
	if dash("") != "—" || dash("x") != "x" {
		t.Fatal("dash helper wrong")
	}
}

func TestEvent_EmptyIDInit(t *testing.T) {
	e := newEvent(sampleGateway(), testSession())
	if e.Init() != nil {
		t.Fatal("event with empty id should have nil Init cmd")
	}
}

// --- program loop (headless) ---

func TestRun_HeadlessQuits(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	done := make(chan error, 1)
	go func() {
		done <- run(m, tea.WithInput(strings.NewReader("q")), tea.WithOutput(io.Discard))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("program did not quit on 'q' within 5s")
	}
}
