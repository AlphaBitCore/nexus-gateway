package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

func toolByName(tools []agent.Tool, name string) agent.Tool {
	for _, tl := range tools {
		if tl.Name() == name {
			return tl
		}
	}
	return nil
}

// TestMitigateToolsConfirmDetail asserts every mitigate_* tool resolves a concrete,
// human-readable confirm detail (so the operator authorizes the named action, not a
// generic "mitigation") and spot-checks the verb+entity content.
func TestMitigateToolsConfirmDetail(t *testing.T) {
	tools := mitigateTools(&fakeGateway{})
	cases := map[string]json.RawMessage{
		"mitigate_kill_switch":          json.RawMessage(`{"engage":true}`),
		"mitigate_cache_flush":          json.RawMessage(`{}`),
		"mitigate_provider_enabled":     json.RawMessage(`{"provider":"openai","enabled":false}`),
		"mitigate_routing_rule_enabled": json.RawMessage(`{"rule":"cheap","enabled":true}`),
		"mitigate_vk_revoke":            json.RawMessage(`{"vk":"eng"}`),
		"mitigate_passthrough_global":   json.RawMessage(`{"enabled":true}`),
	}
	for _, tl := range tools {
		ft, ok := tl.(*funcTool)
		if !ok {
			t.Fatalf("%s should be a funcTool", tl.Name())
		}
		in, ok := cases[tl.Name()]
		if !ok {
			t.Fatalf("no confirm-detail case for %s", tl.Name())
		}
		if d := ft.ConfirmDetail(in); strings.TrimSpace(d) == "" {
			t.Fatalf("%s should resolve a non-empty confirm detail", tl.Name())
		}
	}
	// spot-checks: the detail carries the verb + entity / the engage state.
	prov := toolByName(tools, "mitigate_provider_enabled").(*funcTool)
	if got := prov.ConfirmDetail(json.RawMessage(`{"provider":"openai","enabled":false}`)); !strings.Contains(got, "disable") || !strings.Contains(got, "openai") {
		t.Fatalf("provider detail should name the verb + provider, got %q", got)
	}
	kill := toolByName(tools, "mitigate_kill_switch").(*funcTool)
	if got := kill.ConfirmDetail(json.RawMessage(`{"engage":false}`)); !strings.Contains(got, "disengage") {
		t.Fatalf("kill detail should reflect the disengage verb, got %q", got)
	}
	pass := toolByName(tools, "mitigate_passthrough_global").(*funcTool)
	if got := pass.ConfirmDetail(json.RawMessage(`{"enabled":false}`)); !strings.Contains(got, "disengage") {
		t.Fatalf("passthrough detail should reflect the disengage verb, got %q", got)
	}
}

func TestGatewayToolsObserveHealth(t *testing.T) {
	gw := &fakeGateway{
		sparkline: &core.SparklineResult{},
		instances: &core.InstancesResult{Count: 27, Services: map[string]core.ServiceSummary{"control-plane": {Total: 1}}},
	}
	tools := gatewayTools(gw, "", false)
	th := toolByName(tools, "observe_health")
	if th == nil || th.Tier() != agent.TierAuto {
		t.Fatal("observe_health must exist and be auto-tier")
	}
	res, err := th.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("observe_health should succeed, got %+v err %v", res, err)
	}
	if !strings.Contains(res.Content, `"nodes": 27`) || !strings.Contains(res.Content, `"control-plane"`) {
		t.Fatalf("observe_health must report node/service counts, got %s", res.Content)
	}
}

func TestGatewayToolsObserveHealthErrorPaths(t *testing.T) {
	if res, _ := toolByName(gatewayTools(&fakeGateway{errOn: "Sparkline"}, "", false), "observe_health").Run(context.Background(), nil); !res.IsError {
		t.Fatal("sparkline failure must be an error result")
	}
	if res, _ := toolByName(gatewayTools(&fakeGateway{errOn: "Instances"}, "", false), "observe_health").Run(context.Background(), nil); !res.IsError {
		t.Fatal("instances failure must be an error result")
	}
}

func TestGatewayReadToolsHappyAndError(t *testing.T) {
	gw := &fakeGateway{}
	tools := gatewayTools(gw, "", false)
	cases := []struct {
		tool  string
		args  json.RawMessage
		errOn string
	}{
		{"observe_traffic_list", json.RawMessage(`{"status":"error","limit":5}`), "TrafficList"},
		{"observe_traffic_event", json.RawMessage(`{"id":"ev-1"}`), "TrafficEvent"},
		{"observe_models", nil, "AdminModels"},
		{"observe_alerts", nil, "Alerts"},
		{"observe_nodes", nil, "Nodes"},
		{"observe_killswitch", nil, "KillSwitchStatus"},
		{"observe_passthrough", nil, "PassthroughSnapshot"},
		{"analyze_cost", json.RawMessage(`{"groupBy":"user"}`), "Cost"},
		{"analyze_compliance", nil, "ComplianceOverview"},
		{"route_explain", json.RawMessage(`{"model":"gpt-4o"}`), "RoutingSimulate"},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			res, err := toolByName(tools, c.tool).Run(context.Background(), c.args)
			if err != nil || res.IsError {
				t.Fatalf("%s happy path should succeed, got %+v err %v", c.tool, res, err)
			}
			// The happy path must render real structured content, not an empty body
			// — a tool that serialized nothing (or the wrong shape) would otherwise
			// pass on !IsError alone.
			if got := strings.TrimSpace(res.Content); got == "" || (got[0] != '{' && got[0] != '[') {
				t.Fatalf("%s should render a JSON object/array, got %q", c.tool, res.Content)
			}
			// Error path: a failing gateway call surfaces a recoverable error result.
			et := toolByName(gatewayTools(&fakeGateway{errOn: c.errOn}, "", false), c.tool)
			if eres, _ := et.Run(context.Background(), c.args); !eres.IsError {
				t.Fatalf("%s must surface a gateway error as an error result", c.tool)
			}
		})
	}
}

func TestGatewayToolTrafficEventRequiresID(t *testing.T) {
	res, _ := toolByName(gatewayTools(&fakeGateway{}, "", false), "observe_traffic_event").Run(context.Background(), json.RawMessage(`{}`))
	if !res.IsError || !strings.Contains(res.Content, "id is required") {
		t.Fatalf("observe_traffic_event must require an id, got %+v", res)
	}
}

func TestGatewayToolAnalyzeSLO(t *testing.T) {
	gw := &fakeGateway{sparkline: &core.SparklineResult{}}
	res, err := toolByName(gatewayTools(gw, "", false), "analyze_slo").Run(context.Background(), nil)
	if err != nil || res.IsError {
		t.Fatalf("analyze_slo should succeed, got %+v err %v", res, err)
	}
	if !strings.Contains(res.Content, "availabilityPct") {
		t.Fatalf("analyze_slo must report availability, got %s", res.Content)
	}
	// Each sub-call failing surfaces an error result.
	for _, m := range []string{"LatencyPhases", "RoutingFallbacks", "Sparkline"} {
		if r, _ := toolByName(gatewayTools(&fakeGateway{errOn: m}, "", false), "analyze_slo").Run(context.Background(), nil); !r.IsError {
			t.Fatalf("analyze_slo must error when %s fails", m)
		}
	}
}

func TestGatewayToolRouteExplainRequiresModel(t *testing.T) {
	re := toolByName(gatewayTools(&fakeGateway{}, "", false), "route_explain")
	res, _ := re.Run(context.Background(), json.RawMessage(`{}`))
	if !res.IsError || !strings.Contains(res.Content, "model is required") {
		t.Fatalf("route_explain must require a model, got %+v", res)
	}
}

func TestGatewayToolSimulate(t *testing.T) {
	// Unconfigured VK.
	sim := toolByName(gatewayTools(&fakeGateway{}, "", false), "simulate_request")
	res, err := sim.Run(context.Background(), rawArgs(map[string]any{"model": "gpt-4o", "prompt": "hi"}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "no Virtual Key") {
		t.Fatalf("simulate without a VK must return a clear error result, got %+v", res)
	}
	// Configured VK + model required.
	simVK := toolByName(gatewayTools(&fakeGateway{rawForward: json.RawMessage(`{"ok":true}`)}, "nvk", false), "simulate_request")
	if r, _ := simVK.Run(context.Background(), json.RawMessage(`{"prompt":"hi"}`)); !r.IsError || !strings.Contains(r.Content, "model is required") {
		t.Fatalf("simulate must require a model, got %+v", r)
	}
	// Happy path returns the raw forwarded body.
	if r, _ := simVK.Run(context.Background(), rawArgs(map[string]any{"model": "gpt-4o", "prompt": "hi"})); r.IsError || !strings.Contains(r.Content, `"ok":true`) {
		t.Fatalf("simulate happy path must return the forwarded body, got %+v", r)
	}
	// Forward error.
	simErr := toolByName(gatewayTools(&fakeGateway{errOn: "SimulatorForward"}, "nvk", false), "simulate_request")
	if r, _ := simErr.Run(context.Background(), rawArgs(map[string]any{"model": "m", "prompt": "p"})); !r.IsError {
		t.Fatal("simulate must surface a forward error")
	}
}

func TestGatewayToolsMitigateOmittedUnlessEnabled(t *testing.T) {
	if toolByName(gatewayTools(&fakeGateway{}, "", false), "mitigate_kill_switch") != nil {
		t.Fatal("mitigate tools must be omitted when includeMitigate=false")
	}
	mk := toolByName(gatewayTools(&fakeGateway{}, "", true), "mitigate_kill_switch")
	if mk == nil || mk.Tier() != agent.TierConfirm {
		t.Fatal("mitigate tools must be present + confirm-tier when enabled")
	}
}

func TestMitigateKillSwitchAndCacheFlush(t *testing.T) {
	gw := &fakeGateway{}
	tools := gatewayTools(gw, "", true)
	if r, _ := toolByName(tools, "mitigate_kill_switch").Run(context.Background(), rawArgs(map[string]any{"engage": true})); r.IsError {
		t.Fatalf("kill switch write should succeed, got %+v", r)
	}
	if gw.killCalls != 1 {
		t.Fatalf("kill switch write must call the gateway once, got %d", gw.killCalls)
	}
	if r, _ := toolByName(tools, "mitigate_cache_flush").Run(context.Background(), nil); r.IsError || gw.flushCalls != 1 {
		t.Fatalf("cache flush should succeed once, got res=%+v calls=%d", r, gw.flushCalls)
	}
	// Error paths.
	if r, _ := toolByName(gatewayTools(&fakeGateway{errOn: "SetKillSwitch"}, "", true), "mitigate_kill_switch").Run(context.Background(), rawArgs(map[string]any{"engage": true})); !r.IsError {
		t.Fatal("kill switch write error must surface")
	}
	if r, _ := toolByName(gatewayTools(&fakeGateway{errOn: "CacheFlush"}, "", true), "mitigate_cache_flush").Run(context.Background(), nil); !r.IsError {
		t.Fatal("cache flush error must surface")
	}
}

func TestMitigateProviderResolveAndWrite(t *testing.T) {
	// Happy path: unique name resolves, write issued.
	gw := &fakeGateway{providers: &core.ProvidersResult{Data: []core.Provider{{ID: "1", Name: "openai", DisplayName: "OpenAI"}}}}
	mp := toolByName(gatewayTools(gw, "", true), "mitigate_provider_enabled")
	if r, _ := mp.Run(context.Background(), rawArgs(map[string]any{"provider": "openai", "enabled": false})); r.IsError || gw.setProviderCalls != 1 {
		t.Fatalf("provider write should succeed once, got res=%+v calls=%d", r, gw.setProviderCalls)
	}
	// Ambiguous name refused, no write.
	amb := &fakeGateway{providers: &core.ProvidersResult{Data: []core.Provider{{ID: "1", Name: "openai"}, {ID: "2", Name: "openai"}}}}
	ma := toolByName(gatewayTools(amb, "", true), "mitigate_provider_enabled")
	r, err := ma.Run(context.Background(), rawArgs(map[string]any{"provider": "openai", "enabled": false}))
	if err != nil {
		t.Fatalf("ambiguity is a recoverable result, not a Go error: %v", err)
	}
	if !r.IsError || !strings.Contains(r.Content, "ambiguous") || amb.setProviderCalls != 0 {
		t.Fatalf("ambiguous name must refuse + not write, got res=%+v calls=%d", r, amb.setProviderCalls)
	}
	// Resolve lookup error.
	if rr, _ := toolByName(gatewayTools(&fakeGateway{errOn: "Providers"}, "", true), "mitigate_provider_enabled").Run(context.Background(), rawArgs(map[string]any{"provider": "x", "enabled": true})); !rr.IsError {
		t.Fatal("provider catalog error must surface")
	}
	// Write error after a successful resolve.
	we := &fakeGateway{providers: &core.ProvidersResult{Data: []core.Provider{{ID: "1", Name: "openai"}}}, errOn: "SetProviderEnabled"}
	if rr, _ := toolByName(gatewayTools(we, "", true), "mitigate_provider_enabled").Run(context.Background(), rawArgs(map[string]any{"provider": "openai", "enabled": false})); !rr.IsError {
		t.Fatal("provider write error must surface")
	}
}

func TestMitigateRoutingRuleResolveAndWrite(t *testing.T) {
	gw := &fakeGateway{rules: []core.RoutingRule{{ID: "r1", Name: "cheap-route"}}}
	mr := toolByName(gatewayTools(gw, "", true), "mitigate_routing_rule_enabled")
	if r, _ := mr.Run(context.Background(), rawArgs(map[string]any{"rule": "cheap-route", "enabled": false})); r.IsError || gw.ruleCalls != 1 {
		t.Fatalf("rule write should succeed once, got res=%+v calls=%d", r, gw.ruleCalls)
	}
	// Unknown name.
	if r, _ := mr.Run(context.Background(), rawArgs(map[string]any{"rule": "ghost", "enabled": false})); !r.IsError || !strings.Contains(r.Content, "no routing rule") {
		t.Fatalf("unknown rule must be refused, got %+v", r)
	}
	we := &fakeGateway{rules: []core.RoutingRule{{ID: "r1", Name: "cheap-route"}}, errOn: "SetRoutingRuleEnabled"}
	if r, _ := toolByName(gatewayTools(we, "", true), "mitigate_routing_rule_enabled").Run(context.Background(), rawArgs(map[string]any{"rule": "cheap-route", "enabled": false})); !r.IsError {
		t.Fatal("rule write error must surface")
	}
}

func TestMitigateVKRevoke(t *testing.T) {
	gw := &fakeGateway{vks: []core.VirtualKey{{ID: "v1", Name: "app-key", VKStatus: sp("active")}}}
	mv := toolByName(gatewayTools(gw, "", true), "mitigate_vk_revoke")
	if r, _ := mv.Run(context.Background(), rawArgs(map[string]any{"vk": "app-key"})); r.IsError || gw.revokeCalls != 1 {
		t.Fatalf("vk revoke should succeed once, got res=%+v calls=%d", r, gw.revokeCalls)
	}
	// Non-active key cannot be revoked (resolved-but-not-revocable).
	inactive := &fakeGateway{vks: []core.VirtualKey{{ID: "v2", Name: "old-key", VKStatus: sp("revoked")}}}
	if r, _ := toolByName(gatewayTools(inactive, "", true), "mitigate_vk_revoke").Run(context.Background(), rawArgs(map[string]any{"vk": "old-key"})); !r.IsError || inactive.revokeCalls != 0 {
		t.Fatalf("a non-active key must not be revoked, got res=%+v calls=%d", r, inactive.revokeCalls)
	}
	we := &fakeGateway{vks: []core.VirtualKey{{ID: "v1", Name: "app-key", VKStatus: sp("active")}}, errOn: "RevokeVK"}
	if r, _ := toolByName(gatewayTools(we, "", true), "mitigate_vk_revoke").Run(context.Background(), rawArgs(map[string]any{"vk": "app-key"})); !r.IsError {
		t.Fatal("vk revoke error must surface")
	}
}

func TestMitigatePassthroughGlobal(t *testing.T) {
	gw := &fakeGateway{}
	mp := toolByName(gatewayTools(gw, "", true), "mitigate_passthrough_global")
	if r, _ := mp.Run(context.Background(), rawArgs(map[string]any{"enabled": true, "reason": "incident-123"})); r.IsError || gw.passthroughCalls != 1 {
		t.Fatalf("passthrough write should succeed once, got res=%+v calls=%d", r, gw.passthroughCalls)
	}
	we := &fakeGateway{errOn: "SetPassthroughGlobal"}
	if r, _ := toolByName(gatewayTools(we, "", true), "mitigate_passthrough_global").Run(context.Background(), rawArgs(map[string]any{"enabled": false})); !r.IsError {
		t.Fatal("passthrough write error must surface")
	}
}

func TestResolveHelpersFailureModes(t *testing.T) {
	ctx := context.Background()

	// Provider: lookup error, and no-match lists known names.
	if _, _, err := resolveProviderID(ctx, &fakeGateway{errOn: "Providers"}, "x"); err == nil {
		t.Fatal("provider lookup error must propagate")
	}
	noProv := &fakeGateway{providers: &core.ProvidersResult{Data: []core.Provider{{ID: "1", Name: "openai", DisplayName: "OpenAI"}}}}
	if _, _, err := resolveProviderID(ctx, noProv, "ghost"); err == nil || !strings.Contains(err.Error(), "OpenAI") {
		t.Fatalf("no-match must list known providers, got %v", err)
	}

	// Rule: lookup error.
	if _, _, err := resolveRuleID(ctx, &fakeGateway{errOn: "RoutingRules"}, "x"); err == nil {
		t.Fatal("rule lookup error must propagate")
	}

	// VK: lookup error, no-match, ambiguous (name of one == prefix of another).
	if _, _, err := resolveRevocableVK(ctx, &fakeGateway{errOn: "VirtualKeys"}, "x"); err == nil {
		t.Fatal("vk lookup error must propagate")
	}
	if _, _, err := resolveRevocableVK(ctx, &fakeGateway{vks: []core.VirtualKey{{ID: "v1", Name: "a"}}}, "z"); err == nil || !strings.Contains(err.Error(), "no virtual key") {
		t.Fatalf("vk no-match must be reported, got %v", err)
	}
	amb := &fakeGateway{vks: []core.VirtualKey{
		{ID: "v1", Name: "dup", VKStatus: sp("active")},
		{ID: "v2", KeyPrefix: "dup", VKStatus: sp("active")},
	}}
	if _, _, err := resolveRevocableVK(ctx, amb, "dup"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous vk must be refused, got %v", err)
	}
}

func TestResolveRemainingBranches(t *testing.T) {
	ctx := context.Background()
	// Ambiguous rule name (rule names are not unique-constrained).
	amb := &fakeGateway{rules: []core.RoutingRule{{ID: "r1", Name: "dup"}, {ID: "r2", Name: "dup"}}}
	if _, _, err := resolveRuleID(ctx, amb, "dup"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous rule must be refused, got %v", err)
	}
	// Active VK with no name resolves by prefix, and the label falls back to the prefix.
	byPrefix := &fakeGateway{vks: []core.VirtualKey{{ID: "v1", KeyPrefix: "nvk_abc", VKStatus: sp("active")}}}
	_, label, err := resolveRevocableVK(ctx, byPrefix, "nvk_abc")
	if err != nil || label != "nvk_abc" {
		t.Fatalf("VK label must fall back to the key prefix when name is empty, got label=%q err=%v", label, err)
	}
}

// impactOf type-asserts a tool to agent.ImpactDetailer and returns its preview.
func impactOf(t *testing.T, tl agent.Tool, in json.RawMessage) (any, error) {
	t.Helper()
	d, ok := tl.(agent.ImpactDetailer)
	if !ok {
		t.Fatalf("%s must implement agent.ImpactDetailer", tl.Name())
	}
	return d.ImpactDetail(context.Background(), in)
}

// TestMitigateToolsImpactDetail covers FR-22/AC-6: the three high-blast-radius tools
// produce a structured impact preview (current → effect) read-only, the VK revoke
// flags irreversibility, and an ordinary confirm tool produces none.
func TestMitigateToolsImpactDetail(t *testing.T) {
	gw := &fakeGateway{vks: []core.VirtualKey{{ID: "v1", Name: "app-key", VKStatus: sp("active")}}}
	tools := mitigateTools(gw)

	// kill switch — engage: action + summary + current state present.
	ks, err := impactOf(t, toolByName(tools, "mitigate_kill_switch"), rawArgs(map[string]any{"engage": true}))
	if err != nil {
		t.Fatalf("kill-switch impact: %v", err)
	}
	if m := ks.(map[string]any); m["action"] != "engage" || m["summary"] == "" || m["current"] == nil {
		t.Errorf("kill-switch preview missing action/summary/current: %v", m)
	}

	// passthrough — engage.
	ps, err := impactOf(t, toolByName(tools, "mitigate_passthrough_global"), rawArgs(map[string]any{"enabled": true}))
	if err != nil {
		t.Fatalf("passthrough impact: %v", err)
	}
	if m := ps.(map[string]any); m["action"] != "engage" || m["current"] == nil {
		t.Errorf("passthrough preview missing action/current: %v", m)
	}

	// vk revoke — flags irreversible + names the resolved key.
	vk, err := impactOf(t, toolByName(tools, "mitigate_vk_revoke"), rawArgs(map[string]any{"vk": "app-key"}))
	if err != nil {
		t.Fatalf("vk-revoke impact: %v", err)
	}
	if m := vk.(map[string]any); m["irreversible"] != true || m["action"] != "revoke" {
		t.Errorf("vk-revoke preview must be irreversible+revoke: %v", m)
	}

	// An ordinary confirm tool (cache flush) is NOT an impact tool → (nil, nil).
	if p, err := impactOf(t, toolByName(tools, "mitigate_cache_flush"), json.RawMessage(`{}`)); p != nil || err != nil {
		t.Errorf("cache_flush must have no impact preview, got p=%v err=%v", p, err)
	}
}

// TestMitigateImpactDetail_PropagatesReadError: a failed state read returns an error
// so the caller (makeConfirm) can fail open to an "unavailable" preview.
func TestMitigateImpactDetail_PropagatesReadError(t *testing.T) {
	gw := &fakeGateway{errOn: "KillSwitchStatus"}
	if _, err := impactOf(t, toolByName(mitigateTools(gw), "mitigate_kill_switch"), rawArgs(map[string]any{"engage": true})); err == nil {
		t.Fatal("a failed KillSwitchStatus read must surface as an ImpactDetail error")
	}
}

// TestHighBlastToolsImplementImpactDetailer is the AC-6 regression guard: every tool
// in the high-blast-radius set MUST provide an impact preview, so a future tool added
// to this set without an ImpactDetailer (which would reach the confirm card with no
// preview, violating FR-22) fails CI here rather than silently shipping. The frontend
// "preview present" contract relies on this always-attach invariant.
func TestHighBlastToolsImplementImpactDetailer(t *testing.T) {
	highBlast := map[string]json.RawMessage{
		"mitigate_kill_switch":        rawArgs(map[string]any{"engage": true}),
		"mitigate_passthrough_global": rawArgs(map[string]any{"enabled": true}),
		"mitigate_vk_revoke":          rawArgs(map[string]any{"vk": "app-key"}),
	}
	gw := &fakeGateway{vks: []core.VirtualKey{{ID: "v1", Name: "app-key", VKStatus: sp("active")}}}
	tools := mitigateTools(gw)
	for name, in := range highBlast {
		tl := toolByName(tools, name)
		if tl == nil {
			t.Fatalf("high-blast tool %q not found in mitigateTools", name)
		}
		preview, err := impactOf(t, tl, in)
		if err != nil {
			t.Errorf("%s ImpactDetail errored: %v", name, err)
			continue
		}
		if preview == nil {
			t.Errorf("%s must return a non-nil impact preview (AC-6 always-attach invariant)", name)
		}
	}
}
