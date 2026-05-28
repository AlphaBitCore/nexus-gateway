package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// fakeGateway returns canned data (or a forced error) for the MCP tools.
type fakeGateway struct {
	sp         *core.SparklineResult
	inst       *core.InstancesResult
	list       *core.TrafficList
	ev         *core.TrafficEvent
	models     *core.ModelCatalog
	cost       *core.CostReport
	phases     *core.LatencyPhasesResult
	fb         *core.FallbacksResult
	simResp    json.RawMessage
	kill       *core.KillSwitchResult
	alerts     *core.AlertsResult
	nodes      *core.NodesResult
	compliance *core.ComplianceOverview
	route      *core.RoutingSimulateResult
	providers  *core.ProvidersResult
	rules      []core.RoutingRule
	vks        []core.VirtualKey
	err        error

	ksState  *core.KillSwitchState
	passSnap *core.PassthroughSnapshot

	cacheFlushed        bool
	lastProviderID      string
	lastProviderEnabled bool
	lastRuleID          string
	lastRuleEnabled     bool
	lastRevokedVK       string
	lastPassthrough     *core.PassthroughGlobalRequest
}

func (f *fakeGateway) KillSwitchStatus(context.Context) (*core.KillSwitchState, error) {
	return f.ksState, f.err
}
func (f *fakeGateway) PassthroughSnapshot(context.Context) (*core.PassthroughSnapshot, error) {
	return f.passSnap, f.err
}
func (f *fakeGateway) SetPassthroughGlobal(_ context.Context, req core.PassthroughGlobalRequest) error {
	if f.err != nil {
		return f.err
	}
	f.lastPassthrough = &req
	return nil
}

func (f *fakeGateway) Sparkline(context.Context, url.Values) (*core.SparklineResult, error) {
	return f.sp, f.err
}
func (f *fakeGateway) Instances(context.Context) (*core.InstancesResult, error) {
	return f.inst, f.err
}
func (f *fakeGateway) TrafficList(context.Context, core.TrafficFilter) (*core.TrafficList, error) {
	return f.list, f.err
}
func (f *fakeGateway) TrafficEvent(context.Context, string) (*core.TrafficEvent, error) {
	return f.ev, f.err
}
func (f *fakeGateway) AdminModels(context.Context) (*core.ModelCatalog, error) {
	return f.models, f.err
}
func (f *fakeGateway) Cost(context.Context, url.Values) (*core.CostReport, error) {
	return f.cost, f.err
}
func (f *fakeGateway) LatencyPhases(context.Context, string, url.Values) (*core.LatencyPhasesResult, error) {
	return f.phases, f.err
}
func (f *fakeGateway) RoutingFallbacks(context.Context, url.Values) (*core.FallbacksResult, error) {
	return f.fb, f.err
}
func (f *fakeGateway) SimulatorForward(context.Context, core.SimulatorForwardRequest) (json.RawMessage, error) {
	return f.simResp, f.err
}
func (f *fakeGateway) SetKillSwitch(_ context.Context, engaged bool) (*core.KillSwitchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.kill != nil {
		return f.kill, nil
	}
	return &core.KillSwitchResult{Engaged: engaged, Version: 7}, nil
}
func (f *fakeGateway) Alerts(context.Context) (*core.AlertsResult, error) {
	return f.alerts, f.err
}
func (f *fakeGateway) Nodes(context.Context) (*core.NodesResult, error) {
	return f.nodes, f.err
}
func (f *fakeGateway) ComplianceOverview(context.Context, url.Values) (*core.ComplianceOverview, error) {
	return f.compliance, f.err
}
func (f *fakeGateway) RoutingSimulate(context.Context, core.RoutingSimulateRequest) (*core.RoutingSimulateResult, error) {
	return f.route, f.err
}
func (f *fakeGateway) Providers(context.Context) (*core.ProvidersResult, error) {
	return f.providers, f.err
}
func (f *fakeGateway) RoutingRules(context.Context) ([]core.RoutingRule, error) {
	return f.rules, f.err
}
func (f *fakeGateway) VirtualKeys(context.Context) ([]core.VirtualKey, error) {
	return f.vks, f.err
}
func (f *fakeGateway) SetProviderEnabled(_ context.Context, id string, enabled bool) error {
	if f.err != nil {
		return f.err
	}
	f.lastProviderID, f.lastProviderEnabled = id, enabled
	return nil
}
func (f *fakeGateway) CacheFlush(context.Context) error {
	if f.err != nil {
		return f.err
	}
	f.cacheFlushed = true
	return nil
}
func (f *fakeGateway) RevokeVK(_ context.Context, id string) error {
	if f.err != nil {
		return f.err
	}
	f.lastRevokedVK = id
	return nil
}
func (f *fakeGateway) SetRoutingRuleEnabled(_ context.Context, id string, enabled bool) error {
	if f.err != nil {
		return f.err
	}
	f.lastRuleID, f.lastRuleEnabled = id, enabled
	return nil
}

func sampleGateway() *fakeGateway {
	return &fakeGateway{
		sp: &core.SparklineResult{Series: []core.SparklineBucket{
			{Values: map[string]float64{"request_count": 100, "status_5xx_count": 2}},
		}},
		inst:    &core.InstancesResult{Count: 27, Services: map[string]core.ServiceSummary{"ai-gateway": {Total: 3}}},
		list:    &core.TrafficList{Total: 1, Data: []core.TrafficEvent{{ID: "ev1", StatusCode: 200, ModelName: "gpt-4o-mini"}}},
		ev:      &core.TrafficEvent{ID: "ev1", StatusCode: 200, TraceID: "tr-9"},
		models:  &core.ModelCatalog{Data: []core.ModelGroup{{Provider: core.ProviderRef{Name: "OpenAI"}, Models: []core.Model{{Code: "gpt-4o-mini"}}}}},
		cost:    &core.CostReport{Total: 1, Data: []core.CostRow{{GroupLabel: "OpenAI", TotalCostUSD: 1.5}}},
		phases:  &core.LatencyPhasesResult{Rows: []core.LatencyPhaseRow{{GroupLabel: "OpenAI", TotalP95Ms: 90008}}},
		fb:      &core.FallbacksResult{Data: []core.FallbackRow{{GroupLabel: "Passthrough", RequestCount: 516}}},
		simResp: json.RawMessage(`{"choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":17}}`),
		alerts:  &core.AlertsResult{Alerts: []core.Alert{{TargetLabel: "High 5xx", Severity: "critical", State: "firing"}}},
		nodes:   &core.NodesResult{Nodes: []core.Node{{Name: "ai-gateway-1", Status: "online"}}},
		compliance: &core.ComplianceOverview{KPIs: core.ComplianceKPIs{
			TotalRequests: 586, TotalBlocked: 7, OverallBlockRate: 0.0119}},
		route: &core.RoutingSimulateResult{RuleName: "smart-default", Targets: []core.RoutingTarget{{}}},
		providers: &core.ProvidersResult{Data: []core.Provider{
			{ID: "prov-openai", Name: "openai", DisplayName: "OpenAI", Enabled: true}}},
		rules: []core.RoutingRule{{ID: "r1", Name: "Cheap default", Enabled: true}},
		vks: []core.VirtualKey{
			{ID: "vk1", Name: "engineering", KeyPrefix: "nvk_eng", Enabled: true},
			{ID: "vk2", Name: "revoked-key", KeyPrefix: "nvk_old", VKStatus: strptr("revoked")},
		},
		ksState: &core.KillSwitchState{Engaged: true, Known: true, Version: 9, By: "admin"},
		passSnap: &core.PassthroughSnapshot{
			Global:    core.PassthroughTier{Enabled: true, BypassHooks: true},
			Adapters:  map[string]core.PassthroughTier{},
			Providers: map[string]core.PassthroughTier{},
		},
	}
}

func strptr(s string) *string { return &s }

// connect spins up the server over an in-memory transport pair and returns a
// connected client session.
func connect(t *testing.T, gw Gateway, opts Options) (*sdk.ClientSession, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ct, st := sdk.NewInMemoryTransports()
	srv := NewServer(gw, opts)
	go func() { _ = srv.Run(ctx, st) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v1"}, nil)
	sess, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, ctx
}

func toolNames(t *testing.T, sess *sdk.ClientSession, ctx context.Context) map[string]bool {
	t.Helper()
	lt, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range lt.Tools {
		names[tool.Name] = true
	}
	return names
}

func TestServer_TierRegistration(t *testing.T) {
	// Default: observe/analyze/simulate present, mitigate absent.
	sess, ctx := connect(t, sampleGateway(), Options{})
	names := toolNames(t, sess, ctx)
	for _, want := range []string{
		"observe_health", "observe_traffic_list", "observe_traffic_event", "observe_models",
		"observe_alerts", "observe_nodes", "observe_killswitch", "observe_passthrough",
		"analyze_cost", "analyze_slo", "analyze_compliance", "route_explain", "simulate_request",
	} {
		if !names[want] {
			t.Errorf("expected tool %q to be registered", want)
		}
	}
	for _, mit := range []string{"mitigate_kill_switch", "mitigate_cache_flush", "mitigate_provider_enabled", "mitigate_routing_rule_enabled", "mitigate_vk_revoke", "mitigate_passthrough_global"} {
		if names[mit] {
			t.Fatalf("mitigate tool %q must be OFF by default", mit)
		}
	}
}

func TestServer_MitigateOptIn(t *testing.T) {
	sess, ctx := connect(t, sampleGateway(), Options{EnableMitigate: true})
	names := toolNames(t, sess, ctx)
	for _, want := range []string{"mitigate_kill_switch", "mitigate_cache_flush", "mitigate_provider_enabled", "mitigate_routing_rule_enabled", "mitigate_vk_revoke", "mitigate_passthrough_global"} {
		if !names[want] {
			t.Errorf("--enable-mitigate should register %q", want)
		}
	}
}

func callText(t *testing.T, sess *sdk.ClientSession, ctx context.Context, name string, args any) (*sdk.CallToolResult, string) {
	t.Helper()
	res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return res, sb.String()
}

func TestServer_ObserveAndAnalyzeTools(t *testing.T) {
	sess, ctx := connect(t, sampleGateway(), Options{})

	if _, text := callText(t, sess, ctx, "observe_health", map[string]any{}); !strings.Contains(text, "request_count") || !strings.Contains(text, "ai-gateway") {
		t.Fatalf("observe_health text wrong: %s", text)
	}
	if _, text := callText(t, sess, ctx, "observe_traffic_list", map[string]any{"limit": 5}); !strings.Contains(text, "ev1") {
		t.Fatalf("observe_traffic_list text wrong: %s", text)
	}
	if _, text := callText(t, sess, ctx, "observe_traffic_event", map[string]any{"id": "ev1"}); !strings.Contains(text, "tr-9") {
		t.Fatalf("observe_traffic_event text wrong: %s", text)
	}
	if _, text := callText(t, sess, ctx, "observe_models", map[string]any{}); !strings.Contains(text, "gpt-4o-mini") {
		t.Fatalf("observe_models text wrong: %s", text)
	}
	if _, text := callText(t, sess, ctx, "analyze_cost", map[string]any{"groupBy": "provider"}); !strings.Contains(text, "OpenAI") {
		t.Fatalf("analyze_cost text wrong: %s", text)
	}
	if _, text := callText(t, sess, ctx, "analyze_slo", map[string]any{}); !strings.Contains(text, "availabilityPct") || !strings.Contains(text, "Passthrough") {
		t.Fatalf("analyze_slo text wrong: %s", text)
	}
	if _, text := callText(t, sess, ctx, "observe_alerts", map[string]any{}); !strings.Contains(text, "High 5xx") {
		t.Fatalf("observe_alerts text wrong: %s", text)
	}
	if _, text := callText(t, sess, ctx, "observe_nodes", map[string]any{}); !strings.Contains(text, "ai-gateway-1") {
		t.Fatalf("observe_nodes text wrong: %s", text)
	}
	if _, text := callText(t, sess, ctx, "analyze_compliance", map[string]any{}); !strings.Contains(text, "totalBlocked") {
		t.Fatalf("analyze_compliance text wrong: %s", text)
	}
	if _, text := callText(t, sess, ctx, "route_explain", map[string]any{"model": "gpt-4o-mini"}); !strings.Contains(text, "smart-default") {
		t.Fatalf("route_explain text wrong: %s", text)
	}
	if _, text := callText(t, sess, ctx, "observe_killswitch", map[string]any{}); !strings.Contains(text, `"Engaged": true`) {
		t.Fatalf("observe_killswitch text wrong: %s", text)
	}
	if _, text := callText(t, sess, ctx, "observe_passthrough", map[string]any{}); !strings.Contains(text, "bypassHooks") {
		t.Fatalf("observe_passthrough text wrong: %s", text)
	}
	if res, text := callText(t, sess, ctx, "route_explain", map[string]any{"model": ""}); !res.IsError || !strings.Contains(text, "model is required") {
		t.Fatalf("route_explain without a model should error: %s", text)
	}
}

func TestServer_MitigateNewTools(t *testing.T) {
	gw := sampleGateway()
	sess, ctx := connect(t, gw, Options{EnableMitigate: true})

	// cache flush — global, no id.
	if _, text := callText(t, sess, ctx, "mitigate_cache_flush", map[string]any{}); !strings.Contains(text, `"flushed": true`) {
		t.Fatalf("mitigate_cache_flush text wrong: %s", text)
	}
	if !gw.cacheFlushed {
		t.Fatal("cache flush did not reach the gateway")
	}

	// provider disable by name → resolves to the id, echoes the display label.
	if _, text := callText(t, sess, ctx, "mitigate_provider_enabled", map[string]any{"provider": "openai", "enabled": false}); !strings.Contains(text, "OpenAI") || !strings.Contains(text, `"enabled": false`) {
		t.Fatalf("mitigate_provider_enabled text wrong: %s", text)
	}
	if gw.lastProviderID != "prov-openai" || gw.lastProviderEnabled {
		t.Fatalf("provider mitigate resolved/applied wrong: id=%s enabled=%v", gw.lastProviderID, gw.lastProviderEnabled)
	}

	// routing rule toggle by name.
	if _, text := callText(t, sess, ctx, "mitigate_routing_rule_enabled", map[string]any{"rule": "Cheap default", "enabled": false}); !strings.Contains(text, "Cheap default") {
		t.Fatalf("mitigate_routing_rule_enabled text wrong: %s", text)
	}
	if gw.lastRuleID != "r1" || gw.lastRuleEnabled {
		t.Fatalf("rule mitigate resolved/applied wrong: id=%s enabled=%v", gw.lastRuleID, gw.lastRuleEnabled)
	}

	// VK revoke by key prefix → resolves to the active key's id.
	if _, text := callText(t, sess, ctx, "mitigate_vk_revoke", map[string]any{"vk": "nvk_eng"}); !strings.Contains(text, "engineering") {
		t.Fatalf("mitigate_vk_revoke text wrong: %s", text)
	}
	if gw.lastRevokedVK != "vk1" {
		t.Fatalf("vk revoke resolved wrong id: %s", gw.lastRevokedVK)
	}

	// global passthrough engage → bypassHooks defaulted on; disengage clears it.
	if _, text := callText(t, sess, ctx, "mitigate_passthrough_global", map[string]any{"enabled": true}); !strings.Contains(text, `"enabled": true`) {
		t.Fatalf("mitigate_passthrough_global text wrong: %s", text)
	}
	if gw.lastPassthrough == nil || !gw.lastPassthrough.Enabled || !gw.lastPassthrough.BypassHooks {
		t.Fatalf("passthrough engage should default bypassHooks on: %+v", gw.lastPassthrough)
	}
	callText(t, sess, ctx, "mitigate_passthrough_global", map[string]any{"enabled": false})
	if gw.lastPassthrough.Enabled {
		t.Fatalf("passthrough disengage should clear enabled: %+v", gw.lastPassthrough)
	}
}

func TestResolveHelpersLabelFallback(t *testing.T) {
	ctx := context.Background()
	gw := &fakeGateway{
		providers: &core.ProvidersResult{Data: []core.Provider{{ID: "p9", Name: "groq"}}}, // no DisplayName
		vks:       []core.VirtualKey{{ID: "vk9", KeyPrefix: "nvk_np"}},                    // no Name
	}
	if id, label, err := resolveProviderID(ctx, gw, "groq"); err != nil || id != "p9" || label != "groq" {
		t.Fatalf("provider label should fall back to the name: id=%s label=%s err=%v", id, label, err)
	}
	if id, label, err := resolveRevocableVK(ctx, gw, "nvk_np"); err != nil || id != "vk9" || label != "nvk_np" {
		t.Fatalf("vk label should fall back to the key prefix: id=%s label=%s err=%v", id, label, err)
	}
}

func TestResolveAmbiguityRefused(t *testing.T) {
	ctx := context.Background()
	// Two providers sharing a name → ambiguous, refused.
	provAmb := &fakeGateway{providers: &core.ProvidersResult{Data: []core.Provider{
		{ID: "p1", Name: "dup", DisplayName: "Dup A"},
		{ID: "p2", Name: "other", DisplayName: "dup"},
	}}}
	if _, _, err := resolveProviderID(ctx, provAmb, "dup"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous provider error, got %v", err)
	}
	// Two rules sharing a name → ambiguous.
	ruleAmb := &fakeGateway{rules: []core.RoutingRule{{ID: "r1", Name: "same"}, {ID: "r2", Name: "same"}}}
	if _, _, err := resolveRuleID(ctx, ruleAmb, "same"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous rule error, got %v", err)
	}
	// One VK's name equals another's key prefix → ambiguous, refused (no silent revoke).
	vkAmb := &fakeGateway{vks: []core.VirtualKey{
		{ID: "vkA", Name: "shared"},
		{ID: "vkB", KeyPrefix: "shared"},
	}}
	if _, _, err := resolveRevocableVK(ctx, vkAmb, "shared"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous VK error, got %v", err)
	}
}

func TestServer_MitigateNameResolutionErrors(t *testing.T) {
	sess, ctx := connect(t, sampleGateway(), Options{EnableMitigate: true})
	cases := []struct {
		name, want string
		args       any
	}{
		{"mitigate_provider_enabled", "no provider named", map[string]any{"provider": "nope", "enabled": false}},
		{"mitigate_routing_rule_enabled", "no routing rule named", map[string]any{"rule": "nope", "enabled": false}},
		{"mitigate_vk_revoke", "no virtual key matching", map[string]any{"vk": "nope"}},
		{"mitigate_vk_revoke", "not active", map[string]any{"vk": "nvk_old"}}, // resolves to a revoked key
	}
	for _, c := range cases {
		res, text := callText(t, sess, ctx, c.name, c.args)
		if !res.IsError || !strings.Contains(text, c.want) {
			t.Fatalf("%s(%v) should error with %q, got isError=%v text=%s", c.name, c.args, c.want, res.IsError, text)
		}
	}
}

func TestServer_SimulateTool(t *testing.T) {
	// VK configured → forwards and returns the upstream body.
	sess, ctx := connect(t, sampleGateway(), Options{VKSecret: "nvk_x"})
	if _, text := callText(t, sess, ctx, "simulate_request", map[string]any{"model": "gpt-4o-mini", "prompt": "hi"}); !strings.Contains(text, "total_tokens") {
		t.Fatalf("simulate_request text wrong: %s", text)
	}
	// No VK configured → tool reports an error result (IsError), not a crash.
	sess2, ctx2 := connect(t, sampleGateway(), Options{})
	res, text := callText(t, sess2, ctx2, "simulate_request", map[string]any{"model": "m", "prompt": "hi"})
	if !res.IsError || !strings.Contains(text, "no Virtual Key configured") {
		t.Fatalf("simulate without VK should be an error result: isError=%v text=%s", res.IsError, text)
	}
}

func TestServer_MitigateKillSwitch(t *testing.T) {
	sess, ctx := connect(t, sampleGateway(), Options{EnableMitigate: true})
	if _, text := callText(t, sess, ctx, "mitigate_kill_switch", map[string]any{"engage": true}); !strings.Contains(text, `"engaged": true`) {
		t.Fatalf("mitigate_kill_switch text wrong: %s", text)
	}
}

// TestServer_IAMDenialSurfacesAsToolError is the binding gating check: when the
// admin API denies the principal (403), the tool returns an error result rather
// than bypassing or crashing — proving IAM is the single control point.
func TestServer_IAMDenialSurfacesAsToolError(t *testing.T) {
	denied := &fakeGateway{err: fmt.Errorf("403 forbidden [iam denied]: %w", core.ErrForbidden)}
	// sanity: the error classifies as ErrForbidden (the admin API's 403 mapping)
	if _, err := denied.Instances(context.Background()); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("fake should return a 403 ErrForbidden, got %v", err)
	}
	sess, ctx := connect(t, denied, Options{EnableMitigate: true})
	cases := []struct {
		name string
		args any
	}{
		{"observe_health", map[string]any{}},
		{"analyze_cost", map[string]any{"groupBy": "provider"}},
		{"mitigate_kill_switch", map[string]any{"engage": true}},
	}
	for _, c := range cases {
		res, text := callText(t, sess, ctx, c.name, c.args)
		if !res.IsError || !strings.Contains(text, "forbidden") {
			t.Fatalf("%s on a 403 should surface an error result, got isError=%v text=%s", c.name, res.IsError, text)
		}
	}
}

// TestServer_AllToolsSurfaceGatewayErrors drives every tool against a gateway
// that fails, asserting each returns an error result (covers the error branches
// and confirms a failed admin call never crashes the tool).
func TestServer_AllToolsSurfaceGatewayErrors(t *testing.T) {
	bad := &fakeGateway{err: errors.New("upstream down")}
	sess, ctx := connect(t, bad, Options{EnableMitigate: true, VKSecret: "nvk_x"})
	cases := []struct {
		name string
		args any
	}{
		{"observe_health", map[string]any{}},
		{"observe_traffic_list", map[string]any{}},
		{"observe_traffic_event", map[string]any{"id": "ev1"}},
		{"observe_models", map[string]any{}},
		{"observe_alerts", map[string]any{}},
		{"observe_nodes", map[string]any{}},
		{"observe_killswitch", map[string]any{}},
		{"observe_passthrough", map[string]any{}},
		{"analyze_cost", map[string]any{}},
		{"analyze_slo", map[string]any{}},
		{"analyze_compliance", map[string]any{}},
		{"route_explain", map[string]any{"model": "m"}},
		{"simulate_request", map[string]any{"model": "m", "prompt": "hi"}},
		{"mitigate_kill_switch", map[string]any{"engage": false}},
		{"mitigate_cache_flush", map[string]any{}},
		{"mitigate_provider_enabled", map[string]any{"provider": "openai", "enabled": false}},
		{"mitigate_routing_rule_enabled", map[string]any{"rule": "Cheap default", "enabled": false}},
		{"mitigate_vk_revoke", map[string]any{"vk": "nvk_eng"}},
		{"mitigate_passthrough_global", map[string]any{"enabled": true}},
	}
	for _, c := range cases {
		res, text := callText(t, sess, ctx, c.name, c.args)
		if !res.IsError || !strings.Contains(text, "upstream down") {
			t.Errorf("%s should surface the gateway error, got isError=%v text=%s", c.name, res.IsError, text)
		}
	}
}

// fbErrGateway succeeds on latency-phases but fails the fallbacks call, and
// spErrGateway fails the sparkline call — exercising analyze_slo's later error
// branches (the first-call error is covered by the all-errors test).
type fbErrGateway struct{ *fakeGateway }

func (g fbErrGateway) RoutingFallbacks(context.Context, url.Values) (*core.FallbacksResult, error) {
	return nil, errors.New("fallbacks down")
}

type spErrGateway struct{ *fakeGateway }

func (g spErrGateway) Sparkline(context.Context, url.Values) (*core.SparklineResult, error) {
	return nil, errors.New("sparkline down")
}

func TestServer_AnalyzeSLOLaterErrors(t *testing.T) {
	for _, tc := range []struct {
		gw   Gateway
		want string
	}{
		{fbErrGateway{sampleGateway()}, "fallbacks down"},
		{spErrGateway{sampleGateway()}, "sparkline down"},
	} {
		sess, ctx := connect(t, tc.gw, Options{})
		res, text := callText(t, sess, ctx, "analyze_slo", map[string]any{})
		if !res.IsError || !strings.Contains(text, tc.want) {
			t.Fatalf("analyze_slo should surface %q, got isError=%v text=%s", tc.want, res.IsError, text)
		}
	}
}

func TestServer_ToolInputValidation(t *testing.T) {
	sess, ctx := connect(t, sampleGateway(), Options{VKSecret: "nvk_x"})
	// empty id → handler-level error
	if res, text := callText(t, sess, ctx, "observe_traffic_event", map[string]any{"id": ""}); !res.IsError || !strings.Contains(text, "id is required") {
		t.Fatalf("empty id should error: %s", text)
	}
	// empty model on simulate → handler-level error
	if res, text := callText(t, sess, ctx, "simulate_request", map[string]any{"model": "", "prompt": "hi"}); !res.IsError || !strings.Contains(text, "model is required") {
		t.Fatalf("empty model should error: %s", text)
	}
}

func TestServe_ReturnsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sampleGateway(), Options{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve should return an error when the context is cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return on a cancelled context")
	}
}
