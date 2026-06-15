//go:build live

// Live integration check against a running local stack. Excluded from default
// builds and CI (no stack there); run explicitly with:
//
//	go test -tags live -run TestLive -v ./internal/core/...
//
// It logs in headlessly as the seeded admin and round-trips the typed client,
// validating the wire shapes against real responses (catches field-name drift).
package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func liveEnv() Env {
	return Env{
		Name:             "local",
		CPBaseURL:        "http://localhost:3001",
		AIGatewayBaseURL: "http://localhost:3050",
		OAuthClientID:    "cp-ui",
		OAuthRedirectURI: "http://localhost:3000/auth/callback",
	}
}

func TestLive_LoginAndRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := liveEnv()
	store := newMemStore() // in-memory; do not touch the real keychain
	hc := &http.Client{Timeout: 15 * time.Second}

	a := NewAuthenticator(env, store, hc)
	if err := a.LoginHeadless(ctx, "admin@nexus.ai", "admin123"); err != nil {
		t.Fatalf("headless login failed: %v", err)
	}
	tok, _ := store.Get("local", SecretAccessToken)
	if tok == "" {
		t.Fatal("no access token stored after login")
	}
	t.Logf("login OK, access token length=%d", len(tok))

	c := NewClient(env, NewTokenSource(env, store, hc), hc)

	sp, err := c.Sparkline(ctx, url.Values{})
	if err != nil {
		t.Fatalf("Sparkline: %v", err)
	}
	t.Logf("sparkline: granularity=%q series=%d summaryKeys=%d", sp.Granularity, len(sp.Series), len(sp.Summary))

	inst, err := c.Instances(ctx)
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	t.Logf("instances: count=%d services=%v", inst.Count, inst.Services)

	vks, err := c.VirtualKeys(ctx)
	if err != nil {
		t.Fatalf("VirtualKeys: %v", err)
	}
	t.Logf("virtual keys: %d", len(vks))

	tl, err := c.TrafficList(ctx, TrafficFilter{Limit: 3})
	if err != nil {
		t.Fatalf("TrafficList: %v", err)
	}
	t.Logf("traffic: total=%d returned=%d", tl.Total, len(tl.Data))
	if len(tl.Data) > 0 {
		ev := tl.Data[0]
		t.Logf("first event: id=%s status=%d model=%q cost=%.6f trace=%q", ev.ID, ev.StatusCode, ev.ModelName, ev.EstCostUSD, ev.TraceID)
	}

	// SLO surfaces (Performance/SLO view sources).
	win := url.Values{
		"start": {time.Now().UTC().AddDate(0, 0, -7).Format(time.RFC3339)},
		"end":   {time.Now().UTC().Format(time.RFC3339)},
	}
	lp, err := c.LatencyPhases(ctx, "provider", win)
	if err != nil {
		t.Fatalf("LatencyPhases: %v", err)
	}
	t.Logf("latency-phases: rows=%d", len(lp.Rows))
	if len(lp.Rows) > 0 {
		t.Logf("  first provider %q p95=%dms ttfb-p95=%dms", lp.Rows[0].GroupLabel, lp.Rows[0].TotalP95Ms, lp.Rows[0].UpstreamTTFBP95Ms)
	}
	fb, err := c.RoutingFallbacks(ctx, win)
	if err != nil {
		t.Fatalf("RoutingFallbacks: %v", err)
	}
	t.Logf("routing fallbacks: groups=%d", len(fb.Data))
	roi, err := c.CacheROI(ctx, nil)
	if err != nil {
		t.Fatalf("CacheROI: %v", err)
	}
	t.Logf("cache-roi: netSavings=$%.4f hits=%d periodDays=%d", roi.TotalCacheNetSavingsUSD, roi.RequestsWithCacheHit, roi.PeriodDays)
}

// TestLive_E2EFlow is the program-level scripted run: it walks the operator
// loop end-to-end against the local stack — login → overview → radar →
// drill-down → SLO → cost → chat → simulate — and, when explicitly enabled,
// the kill-switch on→off cycle. Each step asserts the capability returned
// usable data, so a wire-shape regression fails this one test.
func TestLive_E2EFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	env := liveEnv()
	store := newMemStore()
	hc := &http.Client{Timeout: 20 * time.Second}

	if err := NewAuthenticator(env, store, hc).LoginHeadless(ctx, "admin@nexus.ai", "admin123"); err != nil {
		t.Fatalf("login: %v", err)
	}
	c := NewClient(env, NewTokenSource(env, store, hc), hc)

	// overview
	sp, err := c.Sparkline(ctx, nil)
	if err != nil {
		t.Fatalf("overview sparkline: %v", err)
	}
	if sp.Totals()[MetricRequestCount] == 0 {
		t.Fatal("overview totals should reflect real traffic")
	}
	if _, err := c.Instances(ctx); err != nil {
		t.Fatalf("overview instances: %v", err)
	}

	// radar → drill-down
	list, err := c.TrafficList(ctx, TrafficFilter{Limit: 5})
	if err != nil || len(list.Data) == 0 {
		t.Fatalf("radar traffic list: %v (n=%d)", err, len(list.Data))
	}
	ev, err := c.TrafficEvent(ctx, list.Data[0].ID)
	if err != nil || ev.ID != list.Data[0].ID {
		t.Fatalf("drill-down event mismatch: %v", err)
	}

	// SLO + cost
	win := url.Values{"start": {time.Now().UTC().AddDate(0, 0, -7).Format(time.RFC3339)}, "end": {time.Now().UTC().Format(time.RFC3339)}}
	if _, err := c.LatencyPhases(ctx, "provider", win); err != nil {
		t.Fatalf("SLO latency-phases: %v", err)
	}
	if _, err := c.CacheROI(ctx, nil); err != nil {
		t.Fatalf("cost cache-roi: %v", err)
	}
	if _, err := c.Cost(ctx, url.Values{"groupBy": {"provider"}}); err != nil {
		t.Fatalf("cost report: %v", err)
	}

	// chat + simulate (VK-authed; only when a VK secret is provided)
	if vk := strings.TrimSpace(os.Getenv("NEXUS_TEST_VK")); vk != "" {
		var sb strings.Builder
		if _, err := c.ChatStream(ctx, vk, ChatRequest{Model: os.Getenv("NEXUS_TEST_MODEL"), Messages: []ChatMessage{{Role: "user", Content: "say hi"}}}, func(d string) { sb.WriteString(d) }); err != nil {
			t.Fatalf("chat: %v", err)
		}
		if sb.Len() == 0 {
			t.Fatal("chat produced no content")
		}
		body := json.RawMessage(`{"model":"` + os.Getenv("NEXUS_TEST_MODEL") + `","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
		if _, err := c.SimulatorForward(ctx, SimulatorForwardRequest{Path: "/v1/chat/completions", Method: "POST", VK: vk, Body: body}); err != nil {
			t.Fatalf("simulate forward: %v", err)
		}
	} else {
		t.Log("chat/simulate skipped: set NEXUS_TEST_VK to exercise the VK-authed steps")
	}

	// kill-switch on→off cycle — guarded, since engaging emergency passthrough
	// is global state that would disrupt other sessions sharing the local stack.
	if os.Getenv("NEXUS_E2E_KILLSWITCH") == "1" {
		on, err := c.SetKillSwitch(ctx, true)
		if err != nil || !on.Engaged {
			t.Fatalf("kill-switch engage: %v (engaged=%v)", err, on)
		}
		off, err := c.SetKillSwitch(ctx, false)
		if err != nil || off.Engaged {
			t.Fatalf("kill-switch disengage (restore): %v (engaged=%v)", err, off)
		}
		t.Logf("kill-switch cycled on→off (version %d)", off.Version)
	} else {
		t.Log("kill-switch toggle skipped: set NEXUS_E2E_KILLSWITCH=1 to exercise the mutation (global state)")
	}
}

// TestLive_ReadSurfaces exercises the v1.1 Wave 1 read surfaces against the local
// stack: DLQ, Nodes (+ drift), Alerts, and the routing dry-run.
func TestLive_ReadSurfaces(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := liveEnv()
	store := newMemStore()
	hc := &http.Client{Timeout: 15 * time.Second}
	if err := NewAuthenticator(env, store, hc).LoginHeadless(ctx, "admin@nexus.ai", "admin123"); err != nil {
		t.Fatalf("login: %v", err)
	}
	c := NewClient(env, NewTokenSource(env, store, hc), hc)

	dlq, err := c.DLQ(ctx)
	if err != nil {
		t.Fatalf("DLQ: %v", err)
	}
	t.Logf("dlq depth=%d", len(dlq.Rows))

	nodes, err := c.Nodes(ctx)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	drift := 0
	for _, n := range nodes.Nodes {
		if n.Drifted() {
			drift++
		}
	}
	t.Logf("nodes total=%d drifted=%d", nodes.Total, drift)

	al, err := c.Alerts(ctx)
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	firing := 0
	for _, a := range al.Alerts {
		if a.Firing() {
			firing++
		}
	}
	t.Logf("alerts total=%d firing=%d", al.Total, firing)

	rs, err := c.RoutingSimulate(ctx, RoutingSimulateRequest{ModelID: "gpt-4o-mini", EndpointType: "chat"})
	if err != nil {
		t.Fatalf("RoutingSimulate: %v", err)
	}
	t.Logf("route: substituted=%v targets=%d warnings=%d", rs.Substituted, len(rs.Targets), len(rs.Warnings))
}

// TestLive_VirtualKeyLifecycle validates the Wave 3 mitigation surfaces against the real CP:
// the routing-rules + VK list read shapes (field-name drift), and the
// revoke/regenerate writes end-to-end. The writes act ONLY on a throwaway
// personal VK this test creates itself, so it never mutates pre-existing data
// (binding: tests only touch their own data).
func TestLive_VirtualKeyLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := liveEnv()
	store := newMemStore()
	hc := &http.Client{Timeout: 15 * time.Second}
	if err := NewAuthenticator(env, store, hc).LoginHeadless(ctx, "admin@nexus.ai", "admin123"); err != nil {
		t.Fatalf("login: %v", err)
	}
	c := NewClient(env, NewTokenSource(env, store, hc), hc)

	// Read: routing rules list shape.
	rules, err := c.RoutingRules(ctx)
	if err != nil {
		t.Fatalf("RoutingRules: %v", err)
	}
	enabled := 0
	for _, r := range rules {
		if r.Enabled {
			enabled++
		}
	}
	t.Logf("routing rules total=%d enabled=%d", len(rules), enabled)

	// Read: VK list — the new VKStatus field must decode (drift catch).
	keys, err := c.VirtualKeys(ctx)
	if err != nil {
		t.Fatalf("VirtualKeys: %v", err)
	}
	revocable := 0
	for _, k := range keys {
		if k.Revocable() {
			revocable++
		}
	}
	t.Logf("virtual keys total=%d revocable(active)=%d", len(keys), revocable)

	// Write: create a throwaway personal VK, rotate its secret, then revoke it.
	name := "nexus-cli-wave3-livetest-" + time.Now().UTC().Format("20060102T150405")
	created, err := c.CreateVK(ctx, name)
	if err != nil {
		t.Fatalf("CreateVK: %v", err)
	}
	t.Logf("created throwaway VK id=%s prefix=%s", created.ID, created.KeyPrefix)

	regen, err := c.RegenerateVK(ctx, created.ID)
	if err != nil {
		t.Fatalf("RegenerateVK: %v", err)
	}
	if regen.Key == "" || regen.Key == created.Key {
		t.Fatalf("regenerate should return a new plaintext secret (got empty or unchanged)")
	}
	t.Logf("regenerated VK: new secret length=%d prefix=%s", len(regen.Key), regen.KeyPrefix)

	if err := c.RevokeVK(ctx, created.ID); err != nil {
		t.Fatalf("RevokeVK: %v", err)
	}
	t.Logf("revoked throwaway VK %s", created.ID)
}

// TestLive_ChatAndForward exercises the VK-authed paths (Chat Playground +
// Request Lab). It needs a live VK secret in NEXUS_TEST_VK; skips otherwise.
func TestLive_ChatAndForward(t *testing.T) {
	vk := strings.TrimSpace(os.Getenv("NEXUS_TEST_VK"))
	if vk == "" {
		t.Skip("set NEXUS_TEST_VK to a valid local VK secret to run the chat/forward live check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	env := liveEnv()
	c := NewClient(env, fixedTokenSource{header: "Authorization", value: "Bearer unused"}, &http.Client{Timeout: 30 * time.Second})

	var sb strings.Builder
	usage, err := c.ChatStream(ctx, vk, ChatRequest{
		Model:    os.Getenv("NEXUS_TEST_MODEL"),
		Messages: []ChatMessage{{Role: "user", Content: "reply with the single word: pong"}},
	}, func(d string) { sb.WriteString(d) })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if sb.Len() == 0 {
		t.Fatal("ChatStream produced no content")
	}
	t.Logf("chat reply=%q usage=%+v", strings.TrimSpace(sb.String()), usage)

	raw, err := c.SimulatorForward(ctx, SimulatorForwardRequest{
		Path:   "/v1/chat/completions",
		Method: "POST",
		VK:     vk,
		Body:   json.RawMessage(`{"model":"` + os.Getenv("NEXUS_TEST_MODEL") + `","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`),
	})
	if err != nil {
		t.Fatalf("SimulatorForward: %v", err)
	}
	if !strings.Contains(string(raw), "choices") {
		t.Fatalf("SimulatorForward unexpected body: %s", raw)
	}
	t.Logf("forward OK (%d bytes)", len(raw))
}

// TestLive_KillSwitchPassthrough reads (never toggles) the kill-switch state and
// the emergency-passthrough snapshot against the local stack — the riskiest new
// wire shapes (config-sync history parse + the 3-tier snapshot). Read-only: it
// must not mutate global state on the shared stack.
func TestLive_KillSwitchPassthrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := liveEnv()
	store := newMemStore()
	hc := &http.Client{Timeout: 15 * time.Second}
	if err := NewAuthenticator(env, store, hc).LoginHeadless(ctx, "admin@nexus.ai", "admin123"); err != nil {
		t.Fatalf("login: %v", err)
	}
	c := NewClient(env, NewTokenSource(env, store, hc), hc)

	ks, err := c.KillSwitchStatus(ctx)
	if err != nil {
		t.Fatalf("KillSwitchStatus: %v", err)
	}
	t.Logf("kill switch: known=%v engaged=%v version=%d by=%q", ks.Known, ks.Engaged, ks.Version, ks.By)

	snap, err := c.PassthroughSnapshot(ctx)
	if err != nil {
		t.Fatalf("PassthroughSnapshot: %v", err)
	}
	adapters, providers := snap.ActiveOverrides()
	t.Logf("passthrough: global.enabled=%v bypassHooks=%v · active overrides %d adapters/%d providers",
		snap.Global.Enabled, snap.Global.BypassHooks, adapters, providers)
}

// TestLive_PassthroughEngageRoundTrip proves an engage actually passes the
// server's validator (the architect-caught 400 bug). It MUTATES global state, so
// it is guarded behind NEXUS_E2E_PASSTHROUGH=1 and always restores state by
// disengaging in a defer. Run: NEXUS_E2E_PASSTHROUGH=1 go test -tags live -run
// TestLive_PassthroughEngageRoundTrip ./internal/core/
func TestLive_PassthroughEngageRoundTrip(t *testing.T) {
	if os.Getenv("NEXUS_E2E_PASSTHROUGH") != "1" {
		t.Skip("set NEXUS_E2E_PASSTHROUGH=1 to run the global-passthrough engage round-trip (mutates global state)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := liveEnv()
	store := newMemStore()
	hc := &http.Client{Timeout: 15 * time.Second}
	if err := NewAuthenticator(env, store, hc).LoginHeadless(ctx, "admin@nexus.ai", "admin123"); err != nil {
		t.Fatalf("login: %v", err)
	}
	c := NewClient(env, NewTokenSource(env, store, hc), hc)

	// Always restore: disengage at the end regardless of outcome.
	defer func() {
		if err := c.SetPassthroughGlobal(context.Background(), PassthroughGlobalRequest{Enabled: false}); err != nil {
			t.Errorf("cleanup: failed to disengage global passthrough: %v", err)
		}
	}()

	// Engage with only Enabled+BypassHooks — the client must fill expiresAt+reason
	// so the server accepts it (this is the path that previously 400'd).
	if err := c.SetPassthroughGlobal(ctx, PassthroughGlobalRequest{Enabled: true, BypassHooks: true}); err != nil {
		t.Fatalf("engage 400'd — the server validator rejected the request: %v", err)
	}
	snap, err := c.PassthroughSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot after engage: %v", err)
	}
	if !snap.Global.Enabled || !snap.Global.BypassHooks {
		t.Fatalf("global passthrough should be engaged after the write: %+v", snap.Global)
	}
	t.Logf("engage round-trip OK: global enabled=%v bypassHooks=%v expiresAt=%q",
		snap.Global.Enabled, snap.Global.BypassHooks, snap.Global.ExpiresAt)
}
