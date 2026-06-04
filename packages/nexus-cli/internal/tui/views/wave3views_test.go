package views

import (
	tea "charm.land/bubbletea/v2"
	"encoding/json"
	"errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"strings"
	"testing"
)

// strPtr is a local helper for the nullable VK status/type fields.
func strPtr(s string) *string { return &s }

// drive feeds an Init-fetch result then a keystroke and returns the view.
func keyVKs(t *testing.T, v *vks, key string) (*vks, tea.Cmd) {
	t.Helper()
	m, cmd := v.Update(keyRunes(key))
	return m.(*vks), cmd
}

// TestVKs_ViewRendersStatus asserts the list renders the key name, prefix, type,
// status, and enabled state — and that the status drives the column.
func TestVKs_ViewRendersStatus(t *testing.T) {
	gw := &fakeGateway{vks: []core.VirtualKey{
		{ID: "vk1", Name: "engineering", KeyPrefix: "nvk_eng", Enabled: true, VKType: strPtr("application"), VKStatus: strPtr("active")},
		{ID: "vk2", Name: "leaver-key", KeyPrefix: "nvk_lv", Enabled: false, VKType: strPtr("personal"), VKStatus: strPtr("revoked")},
	}}
	v := newVKs(gw, testSession())
	v.Update(v.Init()())
	out := v.View(120, 20)
	for _, want := range []string{"Virtual keys (2)", "engineering", "nvk_eng", "application", "active", "leaver-key", "revoked", "on", "off"} {
		if !strings.Contains(out, want) {
			t.Fatalf("VK view missing %q:\n%s", want, out)
		}
	}
}

// TestVKs_RowDrillShowsScopeFields asserts enter opens a detail drawer that
// surfaces fields the list omits (source app, rate limit, owner / project scope,
// id), and ←/esc (back) closes it before the root would pop the nav stack.
func TestVKs_RowDrillShowsScopeFields(t *testing.T) {
	proj := "proj-42"
	rpm := 600
	gw := &fakeGateway{vks: []core.VirtualKey{
		{ID: "vk1", Name: "engineering", KeyPrefix: "nvk_eng", Enabled: true, VKType: strPtr("application"), VKStatus: strPtr("active"), SourceApp: "cursor", OwnerID: "user-7", ProjectID: &proj, RateLimitRPM: &rpm},
	}}
	v := newVKs(gw, testSession())
	v.Update(v.Init()())
	if !strings.Contains(v.Help(), "enter open") {
		t.Fatalf("list help should offer enter open, got %q", v.Help())
	}
	v.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !v.detail {
		t.Fatal("enter should open the key detail drawer")
	}
	det := v.View(120, 30)
	for _, want := range []string{"Key · engineering", "cursor", "600 rpm", "proj-42", "user-7", "vk1"} {
		if !strings.Contains(det, want) {
			t.Fatalf("the drawer should surface the omitted scope field %q:\n%s", want, det)
		}
	}
	if !strings.Contains(v.Help(), "esc back") {
		t.Fatalf("detail help should offer esc back, got %q", v.Help())
	}
	if !v.Back() || v.detail {
		t.Fatal("back should close the drawer")
	}
	if v.Back() {
		t.Fatal("back at the list level must return false so the root pops the nav stack")
	}
}

// TestVKs_RevokeNonProd covers the happy revoke path: a non-prod revoke fires
// immediately and calls the endpoint for the selected key's id.
func TestVKs_RevokeNonProd(t *testing.T) {
	gw := &fakeGateway{vks: []core.VirtualKey{
		{ID: "vk1", Name: "engineering", KeyPrefix: "nvk_eng", Enabled: true, VKStatus: strPtr("active")},
	}}
	v := newVKs(gw) // no session → non-prod gate fires immediately
	v.Update(v.Init()())
	if !strings.Contains(v.Help(), "revoke") {
		t.Fatalf("help should advertise revoke: %q", v.Help())
	}
	v, _ = keyVKs(t, v, "r")
	if !v.Capturing() {
		t.Fatal("non-prod revoke must raise the confirm gate, not fire")
	}
	m, cmd := v.Update(keyRunes("y")) // allow → revoke
	v = m.(*vks)
	m, _ = v.Update(cmd())
	v = m.(*vks)
	if gw.lastRevokedVK != "vk1" {
		t.Fatalf("expected vk1 revoked, got %q", gw.lastRevokedVK)
	}
	if !strings.Contains(v.View(120, 20), "revoked engineering") {
		t.Fatalf("revoke note missing:\n%s", v.View(120, 20))
	}
}

// TestVKs_RevokeGatedOnActive asserts revoke is offered only for active keys: a
// revoked key sets an explanatory note and never calls the endpoint or starts a
// confirmation.
func TestVKs_RevokeGatedOnActive(t *testing.T) {
	gw := &fakeGateway{vks: []core.VirtualKey{
		{ID: "vk2", Name: "leaver-key", Enabled: false, VKStatus: strPtr("revoked")},
	}}
	v := newVKs(gw, kit.Session{EnvName: "prod", IsProd: true})
	v.Update(v.Init()())
	v, cmd := keyVKs(t, v, "r")
	if cmd != nil {
		t.Fatal("revoke on a non-active key must not fire a command")
	}
	if v.cf.Capturing() {
		t.Fatal("revoke on a non-active key must not start a prod confirmation")
	}
	if gw.lastRevokedVK != "" {
		t.Fatalf("non-active key must not be revoked, got %q", gw.lastRevokedVK)
	}
	if !strings.Contains(v.View(120, 20), "only active keys can be revoked") {
		t.Fatalf("expected explanatory note:\n%s", v.View(120, 20))
	}
}

// TestVKs_RevokeProdConfirm asserts a prod revoke requires typing the env name
// before it fires.
func TestVKs_RevokeProdConfirm(t *testing.T) {
	gw := &fakeGateway{vks: []core.VirtualKey{{ID: "vk1", Name: "engineering", Enabled: true, VKStatus: strPtr("active")}}}
	v := newVKs(gw, kit.Session{EnvName: "prod", IsProd: true})
	v.Update(v.Init()())
	v, _ = keyVKs(t, v, "r")
	if !v.Capturing() || gw.lastRevokedVK != "" {
		t.Fatal("prod revoke must require confirmation before firing")
	}
	if !strings.Contains(v.View(120, 20), "PROD") {
		t.Fatalf("confirm view should show PROD:\n%s", v.View(120, 20))
	}
	m, cmd := v.Update(keyRunes("y")) // quick-allow the prod confirm
	v = m.(*vks)
	m, _ = v.Update(cmd())
	v = m.(*vks)
	if gw.lastRevokedVK != "vk1" {
		t.Fatalf("confirmed prod revoke should fire, got %q", gw.lastRevokedVK)
	}
}

// TestVKs_RevokeError surfaces a server error on revoke.
func TestVKs_RevokeError(t *testing.T) {
	gw := &fakeGateway{err: errors.New("revoke-down"), vks: []core.VirtualKey{{ID: "vk1", Name: "engineering", Enabled: true, VKStatus: strPtr("active")}}}
	v := newVKs(gw)
	// Seed keys directly (fetch errors), then attempt revoke.
	v.keys = []core.VirtualKey{{ID: "vk1", Name: "engineering", Enabled: true, VKStatus: strPtr("active")}}
	v.loading = false
	v, _ = keyVKs(t, v, "r")
	if !v.Capturing() {
		t.Fatal("revoke must raise the confirm gate")
	}
	m, cmd := v.Update(keyRunes("y")) // allow → revoke
	v = m.(*vks)
	m, _ = v.Update(cmd())
	v = m.(*vks)
	if !strings.Contains(v.View(120, 20), "revoke-down") {
		t.Fatalf("revoke error should surface:\n%s", v.View(120, 20))
	}
}

// TestVKs_RegenerateShowsSecretOnce asserts a successful regenerate shows the new
// plaintext in a panel that takes over the view, then dismisses on esc.
func TestVKs_RegenerateShowsSecretOnce(t *testing.T) {
	gw := &fakeGateway{
		vks:   []core.VirtualKey{{ID: "vk1", Name: "engineering", Enabled: true, VKStatus: strPtr("active")}},
		regen: &core.RegeneratedVK{ID: "vk1", KeyPrefix: "nvk_new", Key: "nvk_super_secret"},
	}
	v := newVKs(gw)
	v.Update(v.Init()())
	v, _ = keyVKs(t, v, "g")
	if !v.Capturing() {
		t.Fatal("non-prod regenerate must raise the confirm gate, not fire")
	}
	m, cmd := v.Update(keyRunes("y")) // allow → regenerate
	v = m.(*vks)
	m, _ = v.Update(cmd())
	v = m.(*vks)
	if gw.lastRegeneratedVK != "vk1" {
		t.Fatalf("expected vk1 regenerated, got %q", gw.lastRegeneratedVK)
	}
	out := v.View(120, 20)
	if !strings.Contains(out, "nvk_super_secret") || !strings.Contains(out, "not be shown again") {
		t.Fatalf("secret panel should show the new key once:\n%s", out)
	}
	if !strings.Contains(v.Help(), "shown once") {
		t.Fatalf("help should warn the secret is shown once: %q", v.Help())
	}
	// ←/esc dismisses the panel back to the list. The root routes esc/← through the
	// view's Back() (the same path the detail drawer uses); enter also dismisses
	// in-view. Back() must report it consumed the key so the root does not pop nav.
	if !v.Back() {
		t.Fatal("back should consume ←/esc to dismiss the once-shown secret panel")
	}
	if strings.Contains(v.View(120, 20), "nvk_super_secret") {
		t.Fatalf("secret should be gone after dismiss:\n%s", v.View(120, 20))
	}
}

// TestVKs_RegenerateError surfaces a server error on regenerate.
func TestVKs_RegenerateError(t *testing.T) {
	gw := &fakeGateway{err: errors.New("regen-down")}
	v := newVKs(gw)
	v.keys = []core.VirtualKey{{ID: "vk1", Name: "engineering", Enabled: true, VKStatus: strPtr("active")}}
	v.loading = false
	v, _ = keyVKs(t, v, "g")
	if !v.Capturing() {
		t.Fatal("regenerate must raise the confirm gate")
	}
	m, cmd := v.Update(keyRunes("y")) // allow → regenerate
	v = m.(*vks)
	m, _ = v.Update(cmd())
	v = m.(*vks)
	if !strings.Contains(v.View(120, 20), "regen-down") {
		t.Fatalf("regenerate error should surface:\n%s", v.View(120, 20))
	}
}

// TestVKs_EmptyAndNav covers the empty-list render and cursor clamping.
func TestVKs_EmptyAndNav(t *testing.T) {
	v := newVKs(&fakeGateway{})
	v.Update(v.Init()())
	if !strings.Contains(v.View(120, 20), "no virtual keys") {
		t.Fatalf("empty list should render a placeholder:\n%s", v.View(120, 20))
	}
	// up/down on an empty list must not panic or move the cursor off-list.
	v, _ = keyVKs(t, v, "k")
	v, _ = keyVKs(t, v, "j")
	if _, ok := v.selected(); ok {
		t.Fatal("empty list should have no selection")
	}
}

// TestRouting_ViewAndToggle covers render + the enable/disable toggle across the
// non-prod, prod-confirm, and error paths.
func TestRouting_ViewAndToggle(t *testing.T) {
	gw := &fakeGateway{rules: []core.RoutingRule{
		{ID: "r1", Name: "Cheap default", StrategyType: "smart", Priority: 10, PipelineStage: 1, Enabled: true},
	}}
	v := newRouting(gw)
	v.Update(v.Init()())
	out := v.View(120, 20)
	for _, want := range []string{"Routing rules (1)", "Cheap default", "smart", "on"} {
		if !strings.Contains(out, want) {
			t.Fatalf("routing view missing %q:\n%s", want, out)
		}
	}
	// non-prod toggle raises the gate; allowing disables the enabled rule.
	m, _ := v.Update(keyRunes("t"))
	v = m.(*routing)
	if !v.Capturing() {
		t.Fatal("non-prod toggle must raise the confirm gate, not fire")
	}
	m, cmd := v.Update(keyRunes("y")) // allow → toggle
	v = m.(*routing)
	m, _ = v.Update(cmd())
	v = m.(*routing)
	if gw.lastRuleID != "r1" || gw.lastRuleEnabled == nil || *gw.lastRuleEnabled {
		t.Fatalf("toggle should disable r1: id=%q enabled=%v", gw.lastRuleID, gw.lastRuleEnabled)
	}
	if !strings.Contains(v.View(120, 20), "disabled rule Cheap default") {
		t.Fatalf("toggle note missing:\n%s", v.View(120, 20))
	}
}

// TestRouting_RowDrillShowsConfigBlobs asserts enter opens a detail drawer that
// pretty-prints the rule's strategy config, match conditions, and fallback chain
// (the JSON the list omits), and that ←/esc (back) closes it.
func TestRouting_RowDrillShowsConfigBlobs(t *testing.T) {
	gw := &fakeGateway{rules: []core.RoutingRule{{
		ID: "r1", Name: "Auto smart routing", StrategyType: "smart", Priority: 100, PipelineStage: 1, Enabled: true,
		Description:     "route auto through smart",
		Config:          json.RawMessage(`{"candidates":["claude-sonnet-4-6"]}`),
		MatchConditions: json.RawMessage(`{"requestedModelLiterals":["auto"]}`),
		FallbackChain:   json.RawMessage(`[]`),
		CreatedAt:       "2026-01-15T10:30:00Z",
	}}}
	v := newRouting(gw)
	v.Update(v.Init()())
	if !strings.Contains(v.Help(), "enter open") {
		t.Fatalf("list help should offer enter open, got %q", v.Help())
	}
	v.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !v.detail {
		t.Fatal("enter should open the rule detail drawer")
	}
	det := v.View(120, 40)
	for _, want := range []string{"Rule · Auto smart routing", "route auto through smart", "2026-01-15", "candidates", "claude-sonnet-4-6", "requestedModelLiterals", "Config", "Match conditions", "Fallback chain"} {
		if !strings.Contains(det, want) {
			t.Fatalf("the drawer should surface the omitted config blob %q:\n%s", want, det)
		}
	}
	if !strings.Contains(v.Help(), "esc back") {
		t.Fatalf("detail help should offer esc back, got %q", v.Help())
	}
	if !v.Back() || v.detail {
		t.Fatal("back should close the drawer")
	}
	if v.Back() {
		t.Fatal("back at the list level must return false so the root pops the nav stack")
	}
}

// TestRouting_ProdConfirmAndError asserts the prod confirm gate and the error path.
func TestRouting_ProdConfirmAndError(t *testing.T) {
	gw := &fakeGateway{rules: []core.RoutingRule{{ID: "r1", Name: "Cheap default", StrategyType: "smart", Enabled: false}}}
	v := newRouting(gw, kit.Session{EnvName: "prod", IsProd: true})
	v.Update(v.Init()())
	m, _ := v.Update(keyRunes("t"))
	v = m.(*routing)
	if !v.Capturing() || gw.lastRuleEnabled != nil {
		t.Fatal("prod toggle must require confirmation before firing")
	}
	if !strings.Contains(v.Help(), "confirm") {
		t.Fatalf("help should show the confirm hint while confirming: %q", v.Help())
	}
	m, cmd := v.Update(keyRunes("y")) // quick-allow the prod confirm
	v = m.(*routing)
	m, _ = v.Update(cmd())
	v = m.(*routing)
	if gw.lastRuleEnabled == nil || !*gw.lastRuleEnabled {
		t.Fatalf("confirmed prod toggle should enable r1: %v", gw.lastRuleEnabled)
	}

	// error path surfaces.
	egw := &fakeGateway{err: errors.New("rule-down")}
	ev := newRouting(egw)
	ev.rules = []core.RoutingRule{{ID: "r1", Name: "Cheap default", Enabled: true}}
	ev.loading = false
	m, _ = ev.Update(keyRunes("t"))
	ev = m.(*routing)
	m, cmd = ev.Update(keyRunes("y")) // allow → toggle
	ev = m.(*routing)
	m, _ = ev.Update(cmd())
	ev = m.(*routing)
	if !strings.Contains(ev.View(120, 20), "rule-down") {
		t.Fatalf("toggle error should surface:\n%s", ev.View(120, 20))
	}
}

// TestRouting_Empty covers the empty-list render.
func TestRouting_Empty(t *testing.T) {
	v := newRouting(&fakeGateway{})
	v.Update(v.Init()())
	if !strings.Contains(v.View(120, 20), "no routing rules") {
		t.Fatalf("empty list should render a placeholder:\n%s", v.View(120, 20))
	}
	if _, ok := v.selected(); ok {
		t.Fatal("empty list should have no selection")
	}
}

// TestVKs_RenderBranches exercises the loading guard, the all-status table (each
// RAG arm), the last-good fallback when a poll errors, the prod confirm-help, and
// the poll re-fetch — each asserting the observable behavior, not just coverage.
func TestVKs_RenderBranches(t *testing.T) {
	// loading guard before the first fetch lands.
	lv := newVKs(&fakeGateway{})
	if !strings.Contains(lv.View(120, 20), "loading virtual keys") {
		t.Fatalf("loading guard expected:\n%s", lv.View(120, 20))
	}

	// every status renders (covers the RAG switch arms incl. pending/rejected/default).
	gw := &fakeGateway{vks: []core.VirtualKey{
		{ID: "a", Name: "a", VKStatus: strPtr("active")},
		{ID: "p", Name: "p", VKStatus: strPtr("pending")},
		{ID: "x", Name: "x", VKStatus: strPtr("rejected")},
		{ID: "u", Name: "u", VKStatus: strPtr("unknown-future")},
	}}
	v := newVKs(gw)
	v.Update(v.Init()())
	out := v.View(120, 20)
	for _, want := range []string{"active", "pending", "rejected", "unknown-future"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status %q should render:\n%s", want, out)
		}
	}

	// prod confirm-help path (cf.capturing branch of Help()).
	pv := newVKs(&fakeGateway{vks: []core.VirtualKey{{ID: "a", Name: "a", Enabled: true, VKStatus: strPtr("active")}}}, kit.Session{EnvName: "prod", IsProd: true})
	pv.Update(pv.Init()())
	pv, _ = keyVKs(t, pv, "r")
	if !strings.Contains(pv.Help(), "confirm") {
		t.Fatalf("confirm help should show the allow/deny hint: %q", pv.Help())
	}

	// a poll-tick error keeps last-good data and surfaces the warning.
	v.err = nil
	m, cmd := v.Update(vksMsg{err: errors.New("poll-fail")})
	v = m.(*vks)
	if cmd == nil {
		t.Fatal("a vksMsg should reschedule the poll")
	}
	if !strings.Contains(v.View(120, 20), "poll-fail") || !strings.Contains(v.View(120, 20), "last-good") {
		t.Fatalf("errored poll should show warning + last-good data:\n%s", v.View(120, 20))
	}
	// the tick re-fetches.
	if _, tcmd := v.Update(vksTick{}); tcmd == nil {
		t.Fatal("vksTick should re-fetch")
	}
}

// TestRouting_RenderBranches mirrors the VK render-branch coverage for routing.
func TestRouting_RenderBranches(t *testing.T) {
	lv := newRouting(&fakeGateway{})
	if !strings.Contains(lv.View(120, 20), "loading routing rules") {
		t.Fatalf("loading guard expected:\n%s", lv.View(120, 20))
	}

	gw := &fakeGateway{rules: []core.RoutingRule{{ID: "r1", Name: "R1", StrategyType: "smart", Enabled: false}}}
	v := newRouting(gw)
	v.Update(v.Init()())

	// a poll-tick error keeps last-good data and surfaces the warning.
	m, cmd := v.Update(routingMsg{err: errors.New("poll-fail")})
	v = m.(*routing)
	if cmd == nil {
		t.Fatal("a routingMsg should reschedule the poll")
	}
	if !strings.Contains(v.View(120, 20), "poll-fail") || !strings.Contains(v.View(120, 20), "last-good") {
		t.Fatalf("errored poll should show warning + last-good data:\n%s", v.View(120, 20))
	}
	if _, tcmd := v.Update(routingTick{}); tcmd == nil {
		t.Fatal("routingTick should re-fetch")
	}
}

// TestWave3_InFlightGuard asserts a write in flight suppresses a second action
// keypress (so a double-tap g cannot rotate away a just-shown secret), and that
// the guard clears once the write result lands.
func TestWave3_InFlightGuard(t *testing.T) {
	// VK regenerate: first g dispatches; while in flight a second g is a no-op.
	gw := &fakeGateway{vks: []core.VirtualKey{{ID: "vk1", Name: "engineering", Enabled: true, VKStatus: strPtr("active")}}}
	v := newVKs(gw)
	v.Update(v.Init()())
	v, _ = keyVKs(t, v, "g")
	if !v.Capturing() {
		t.Fatal("regenerate should raise the confirm gate")
	}
	v, cmd := keyVKs(t, v, "y") // allow → dispatch + mark busy
	if cmd == nil || !v.busy {
		t.Fatal("allowing should dispatch the regenerate and mark the view busy")
	}
	if _, cmd2 := keyVKs(t, v, "g"); cmd2 != nil {
		t.Fatal("a second regenerate while busy must be a no-op")
	}
	if _, cmd3 := keyVKs(t, v, "r"); cmd3 != nil {
		t.Fatal("revoke while a write is in flight must be a no-op")
	}
	m, _ := v.Update(cmd()) // the first write lands
	v = m.(*vks)
	if v.busy {
		t.Fatal("busy should clear once the write result lands")
	}

	// Routing toggle: same guard.
	rgw := &fakeGateway{rules: []core.RoutingRule{{ID: "r1", Name: "R1", Enabled: true}}}
	r := newRouting(rgw)
	r.Update(r.Init()())
	m2, _ := r.Update(keyRunes("t"))
	r = m2.(*routing)
	if !r.Capturing() {
		t.Fatal("toggle should raise the confirm gate")
	}
	m2, rcmd := r.Update(keyRunes("y")) // allow → dispatch + mark busy
	r = m2.(*routing)
	if rcmd == nil || !r.busy {
		t.Fatal("allowing should dispatch the toggle and mark busy")
	}
	if _, rcmd2 := r.Update(keyRunes("t")); rcmd2 != nil {
		t.Fatal("a second toggle while busy must be a no-op")
	}
	m2, _ = r.Update(rcmd())
	r = m2.(*routing)
	if r.busy {
		t.Fatal("busy should clear once the toggle result lands")
	}
}

// TestWave3_NoSelectionAndFirstError asserts the safe no-ops: an action key on an
// empty list does nothing, and a first fetch that errors (no data yet) shows only
// the error — never a half-rendered table.
func TestWave3_NoSelectionAndFirstError(t *testing.T) {
	// Action keys on an empty VK list are no-ops (no command, no write).
	ev := newVKs(&fakeGateway{})
	ev.Update(ev.Init()())
	for _, k := range []string{"r", "g"} {
		nv, cmd := keyVKs(t, ev, k)
		if cmd != nil {
			t.Fatalf("%q on an empty VK list should be a no-op", k)
		}
		ev = nv
	}
	// Action key on an empty routing list is a no-op.
	er := newRouting(&fakeGateway{})
	er.Update(er.Init()())
	if m, cmd := er.Update(keyRunes("t")); cmd != nil {
		_ = m
		t.Fatal("t on an empty routing list should be a no-op")
	}

	// First fetch errors with no data: VK view shows only the error.
	fv := newVKs(&fakeGateway{err: errors.New("vk-down")})
	fv.Update(fv.Init()())
	out := fv.View(120, 20)
	if !strings.Contains(out, "vk-down") || strings.Contains(out, "NAME") {
		t.Fatalf("first VK error should show only the error, no table:\n%s", out)
	}
	// First fetch errors with no data: routing view shows only the error.
	fr := newRouting(&fakeGateway{err: errors.New("rules-down")})
	fr.Update(fr.Init()())
	rout := fr.View(120, 20)
	if !strings.Contains(rout, "rules-down") || strings.Contains(rout, "STRATEGY") {
		t.Fatalf("first routing error should show only the error, no table:\n%s", rout)
	}
}
