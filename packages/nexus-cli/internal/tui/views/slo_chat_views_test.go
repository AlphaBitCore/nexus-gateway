package views

import (
	tea "charm.land/bubbletea/v2"
	"errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/goccy/go-json"
	"math"
	"strings"
	"testing"
)

func TestCompliance_View(t *testing.T) {
	c := newCompliance(sampleGateway())
	if !strings.Contains(c.View(120, 20), "loading") {
		t.Fatal("initial compliance shows loading")
	}
	v, cmd := c.Update(c.Init()())
	if cmd == nil {
		t.Fatal("compliance schedules a poll tick")
	}
	out := v.View(120, 20)
	for _, want := range []string{"Compliance overview", "Requests", "586", "Blocked", "Block rate", "1.19%"} {
		if !strings.Contains(out, want) {
			t.Errorf("compliance view missing %q:\n%s", want, out)
		}
	}
	// high block rate → red is exercised; error surfaces.
	hi := newCompliance(&fakeGateway{compliance: &core.ComplianceOverview{KPIs: core.ComplianceKPIs{TotalRequests: 100, TotalBlocked: 10, OverallBlockRate: 0.10}}})
	hv, _ := hi.Update(hi.Init()())
	if !strings.Contains(hv.View(120, 20), "10.00%") {
		t.Fatal("high block rate should render")
	}
	er := newCompliance(&fakeGateway{err: errors.New("comp-down")})
	ev, _ := er.Update(er.Init()())
	if !strings.Contains(ev.View(120, 20), "comp-down") {
		t.Fatal("compliance error should surface")
	}
}

func TestJobs_View(t *testing.T) {
	j := newJobs(sampleGateway())
	if !strings.Contains(j.View(120, 20), "loading") {
		t.Fatal("initial jobs shows loading")
	}
	v, cmd := j.Update(j.Init()())
	if cmd == nil {
		t.Fatal("jobs schedules a poll tick")
	}
	out := v.View(120, 20)
	if !strings.Contains(out, "Scheduled jobs") || !strings.Contains(out, "Cert Alerts") || !strings.Contains(out, "1h0m0s") {
		t.Fatalf("jobs view wrong:\n%s", out)
	}
	// disabled job + empty + error
	dis := newJobs(&fakeGateway{jobs: &core.JobsResult{Jobs: []core.Job{{Name: "off", Enabled: false, Interval: 0}}}})
	dv, _ := dis.Update(dis.Init()())
	if !strings.Contains(dv.View(120, 20), "off") {
		t.Fatal("disabled job should render")
	}
	empty := newJobs(&fakeGateway{jobs: &core.JobsResult{}})
	ev, _ := empty.Update(empty.Init()())
	if !strings.Contains(ev.View(120, 20), "no jobs") {
		t.Fatal("empty jobs placeholder")
	}
	er := newJobs(&fakeGateway{err: errors.New("jobs-down")})
	erv, _ := er.Update(er.Init()())
	if !strings.Contains(erv.View(120, 20), "jobs-down") {
		t.Fatal("jobs error should surface")
	}
}

func TestConfigSync_View(t *testing.T) {
	// in sync (total 0)
	s := newConfigSync(sampleGateway())
	if !strings.Contains(s.View(120, 20), "loading") {
		t.Fatal("initial config sync shows loading")
	}
	v, cmd := s.Update(s.Init()())
	if cmd == nil {
		t.Fatal("config sync schedules a poll tick")
	}
	if !strings.Contains(v.View(120, 20), "all nodes in sync") {
		t.Fatal("zero out-of-sync → all in sync")
	}
	// out of sync
	oos := newConfigSync(&fakeGateway{configSync: &core.ConfigSyncResult{Total: 3, OutOfSync: []json.RawMessage{[]byte("{}"), []byte("{}"), []byte("{}")}}})
	ov, _ := oos.Update(oos.Init()())
	if !strings.Contains(ov.View(120, 20), "3 node(s) out of sync") {
		t.Fatalf("out-of-sync should render count:\n%s", ov.View(120, 20))
	}
	er := newConfigSync(&fakeGateway{err: errors.New("sync-down")})
	erv, _ := er.Update(er.Init()())
	if !strings.Contains(erv.View(120, 20), "sync-down") {
		t.Fatal("config sync error should surface")
	}
}

func TestCost_BurnRateNoData(t *testing.T) {
	c := newCost(&fakeGateway{byProvider: nil, roi: nil})
	v, _ := c.Update(c.Init()())
	if !strings.Contains(v.View(120, 20), "burn rate: (no data)") {
		t.Fatal("no roi → burn-rate placeholder")
	}
}

// TestSLO_ProviderDetailDrill exercises the provider SLO drill: enter loads the
// ProviderDetail summary, the panel renders availability/cache/cost + the row's
// percentiles, and esc returns to the list.
func TestSLO_ProviderDetailDrill(t *testing.T) {
	s := newSLO(sampleGateway())
	v, _ := s.Update(s.Init()()) // load phases
	sv := v.(*slo)
	if !strings.Contains(sv.Help(), "enter provider detail") {
		t.Fatalf("list help should advertise the drill: %q", sv.Help())
	}

	// enter drills into the selected provider and issues a detail fetch.
	v2, cmd := sv.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	sv = v2.(*slo)
	if !sv.inDetail || cmd == nil {
		t.Fatal("enter should drill into detail and schedule the fetch")
	}
	if !strings.Contains(sv.View(120, 30), "loading provider detail") {
		t.Fatal("detail shows loading until the fetch resolves")
	}
	if !strings.Contains(sv.Help(), "esc back") {
		t.Fatalf("detail help should advertise esc: %q", sv.Help())
	}

	// resolve the fetch → the summary tiles + percentiles render.
	v3, _ := sv.Update(cmd())
	sv = v3.(*slo)
	out := sv.View(120, 30)
	for _, want := range []string{
		"Provider detail · OpenAI", "Requests", "Error rate", "2.31%",
		"Cache hit", "18.0%", "Cost", "$1.3373", "Latency percentiles", "p95",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail view missing %q:\n%s", want, out)
		}
	}

	// esc returns to the provider list; backspace is the same gate.
	v4, _ := sv.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	sv = v4.(*slo)
	if sv.inDetail || !strings.Contains(sv.View(120, 30), "Per-provider latency") {
		t.Fatal("esc should return to the provider list")
	}
}

// TestSLO_DrillNavErrorsAndGuards covers cursor movement, the friendly-name
// fallback in the table, the unresolved-provider drill, the empty/no-drill case,
// the stale-result guard, the detail-error path, and the cursor clamp.
func TestSLO_DrillNavErrorsAndGuards(t *testing.T) {
	// twoRows has no provider catalog → table shows the GroupLabel (friendly
	// fallback) and a drill cannot resolve a UUID.
	twoRows := &core.LatencyPhasesResult{Rows: []core.LatencyPhaseRow{
		{GroupKey: "openai", GroupLabel: "OpenAI", TotalP95Ms: 1000},
		{GroupKey: "anthropic", GroupLabel: "Anthropic", TotalP95Ms: 2000},
	}}
	gw := &fakeGateway{sp: sampleGateway().sp, phases: twoRows, fallbacks: &core.FallbacksResult{}}
	s := newSLO(gw)
	v, _ := s.Update(s.Init()())
	sv := v.(*slo)
	if out := sv.View(120, 30); !strings.Contains(out, "OpenAI") || !strings.Contains(out, "Anthropic") {
		t.Fatalf("table should show GroupLabel when no catalog match:\n%s", out)
	}

	// down moves the cursor; up clamps at the top.
	v, _ = sv.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	sv = v.(*slo)
	if sv.cursor != 1 {
		t.Fatalf("down should move cursor to 1, got %d", sv.cursor)
	}
	v, _ = sv.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // already at last row → no-op
	sv = v.(*slo)
	if sv.cursor != 1 {
		t.Fatalf("down at last row should be a no-op, got %d", sv.cursor)
	}
	v, _ = sv.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	sv = v.(*slo)
	v, _ = sv.Update(tea.KeyPressMsg{Code: tea.KeyUp}) // already at top → no-op
	sv = v.(*slo)
	if sv.cursor != 0 {
		t.Fatalf("up should clamp at 0, got %d", sv.cursor)
	}
	if row, ok := sv.selectedRow(); !ok || row.GroupKey != "openai" {
		t.Fatal("selected row should be the first provider")
	}

	// drilling an unresolved provider issues no fetch and the panel says so —
	// crucially it never shows a UUID.
	v, ucmd := sv.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	sv = v.(*slo)
	if ucmd != nil {
		t.Fatal("unresolved provider must not issue a detail fetch")
	}
	if out := sv.View(120, 20); !strings.Contains(out, "no catalog match") || !strings.Contains(out, "OpenAI") {
		t.Fatalf("unresolved drill should show the friendly name + unavailable note:\n%s", out)
	}
	v, _ = sv.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	sv = v.(*slo)

	// a refetch with fewer rows clamps the cursor back into range.
	v, _ = sv.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // cursor → 1
	sv = v.(*slo)
	v, _ = sv.Update(sloMsg{phases: &core.LatencyPhasesResult{Rows: twoRows.Rows[:1]}})
	sv = v.(*slo)
	if sv.cursor != 0 {
		t.Fatalf("cursor should clamp to 0 after rows shrink, got %d", sv.cursor)
	}

	// enter with no providers must not drill.
	empty := newSLO(&fakeGateway{sp: sampleGateway().sp})
	ev, _ := empty.Update(empty.Init()())
	es := ev.(*slo)
	ev2, ecmd := es.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if ecmd != nil || ev2.(*slo).inDetail {
		t.Fatal("enter with no providers must not drill")
	}

	// drill a resolved provider (sampleGateway: openai → prov-openai), then verify
	// a stale result (wrong UUID) is ignored and the matching error surfaces;
	// backspace returns to the list.
	ds := newSLO(sampleGateway())
	dv, _ := ds.Update(ds.Init()())
	ds = dv.(*slo)
	dv, _ = ds.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	ds = dv.(*slo)
	if ds.detailProvider.ID != "prov-openai" {
		t.Fatalf("drill should resolve openai → prov-openai, got %q", ds.detailProvider.ID)
	}
	dv, _ = ds.Update(providerDetailMsg{key: "some-other-uuid", err: errors.New("stale")})
	ds = dv.(*slo)
	if ds.detailErr != nil {
		t.Fatal("a stale provider-detail result must be ignored")
	}
	dv, _ = ds.Update(providerDetailMsg{key: "prov-openai", err: errors.New("provider-down")})
	ds = dv.(*slo)
	if !strings.Contains(ds.View(120, 20), "provider-down") {
		t.Fatal("provider-detail error should surface in the panel")
	}
	dv, _ = ds.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	ds = dv.(*slo)
	if ds.inDetail {
		t.Fatal("backspace should return to the list")
	}
}

// drillTo drives a fresh SLO view into a resolved detail panel for the gateway's
// single provider row, returning the view ready to render. The gateway must
// carry a providers catalog entry for the row's GroupKey.
func drillTo(t *testing.T, gw *fakeGateway) *slo {
	t.Helper()
	s := newSLO(gw)
	v, _ := s.Update(s.Init()())
	sv := v.(*slo)
	v, cmd := sv.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should schedule a detail fetch for a resolved provider")
	}
	sv = v.(*slo)
	v, _ = sv.Update(cmd())
	return v.(*slo)
}

// TestSLO_DetailErrorRAG covers the error-rate RAG thresholds (red / green) in
// the detail panel and confirms the heading shows the friendly DisplayName.
func TestSLO_DetailErrorRAG(t *testing.T) {
	cat := &core.ProvidersResult{Data: []core.Provider{{ID: "prov-kimi", Name: "kimi", DisplayName: "Moonshot (Kimi)"}}}
	kimiRows := &core.LatencyPhasesResult{Rows: []core.LatencyPhaseRow{{GroupKey: "kimi", TotalP95Ms: 500}}}

	// high error rate exercises the red branch; heading shows DisplayName.
	hi := &fakeGateway{
		sp:         sampleGateway().sp,
		phases:     kimiRows,
		fallbacks:  &core.FallbacksResult{},
		providers:  cat,
		provDetail: &core.ProviderDetail{Summary: core.ProviderDetailSummary{TotalRequests: 50, ErrorCount: 10, ErrorRate: 0.20}},
	}
	out := drillTo(t, hi).View(120, 30)
	if !strings.Contains(out, "Provider detail · Moonshot (Kimi)") {
		t.Fatalf("heading should show the friendly DisplayName:\n%s", out)
	}
	if !strings.Contains(out, "20.00%") {
		t.Fatalf("high error rate should render:\n%s", out)
	}

	// healthy provider exercises the green branch.
	lo := &fakeGateway{
		sp:         sampleGateway().sp,
		phases:     kimiRows,
		fallbacks:  &core.FallbacksResult{},
		providers:  cat,
		provDetail: &core.ProviderDetail{Summary: core.ProviderDetailSummary{TotalRequests: 50, ErrorRate: 0}},
	}
	if !strings.Contains(drillTo(t, lo).View(120, 30), "0.00%") {
		t.Fatal("healthy provider error rate should render 0.00%")
	}
}

// drainCompare drives a chat A/B round to completion by executing the batch of
// stream commands and feeding every resulting message back through Update,
// following the chain (delta → re-wait → done) for both sides.
func drainCompare(t *testing.T, c *chat, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for i := 0; i < 200 && len(queue) > 0; i++ {
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		switch m := next().(type) {
		case tea.BatchMsg:
			queue = append(queue, m...)
		case nil:
			// no message
		default:
			if _, c2 := c.Update(m); c2 != nil {
				queue = append(queue, c2)
			}
		}
	}
}

// TestChat_CompareFansOutToTwoModels verifies /compare enters A/B mode and a
// prompt fans out to both the session model and the compare model, with both
// answers + both model names rendered.
func TestChat_CompareFansOutToTwoModels(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	// enter A/B mode via the slash command.
	c.input.SetValue("/compare gpt-4o-mini")
	v, ccmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if c.compareModel != "gpt-4o-mini" || ccmd != nil {
		t.Fatalf("/compare should set model B without a cmd; got %q", c.compareModel)
	}
	if !strings.Contains(c.Help(), "A/B") || !strings.Contains(c.View(120, 30), "A/B") {
		t.Fatal("A/B mode should be reflected in help + header")
	}

	// type + send a prompt.
	c.input.SetValue("ping")
	v, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if !c.streaming || cmd == nil || len(c.rounds) != 1 {
		t.Fatalf("compare send should start one streaming round; streaming=%v rounds=%d", c.streaming, len(c.rounds))
	}
	drainCompare(t, c, cmd)
	if c.streaming {
		t.Fatal("both sides done → streaming should stop")
	}
	// both sides accumulated the fake's delta.
	if c.rounds[0].a.text != "Hello world" || c.rounds[0].b.text != "Hello world" {
		t.Fatalf("both sides should accumulate output: a=%q b=%q", c.rounds[0].a.text, c.rounds[0].b.text)
	}
	out := c.View(120, 30)
	for _, want := range []string{"A/B", "Hello world", testSession().Model, "gpt-4o-mini", "ping"} {
		if !strings.Contains(out, want) {
			t.Errorf("compare view missing %q:\n%s", want, out)
		}
	}
}

// TestChat_SlashCommands covers /compare (no-arg hint + set), /solo, and an
// unknown command — each reported via the notice line.
func TestChat_SlashCommands(t *testing.T) {
	c := newChat(sampleGateway(), testSession())

	c.input.SetValue("/compare")
	v, _ := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if c.compareModel != "" || !strings.Contains(c.notice, "usage:") {
		t.Fatalf("/compare without arg should hint usage; notice=%q model=%q", c.notice, c.compareModel)
	}

	c.input.SetValue("/compare claude-x")
	v, _ = c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if c.compareModel != "claude-x" {
		t.Fatalf("/compare <model> should set model B, got %q", c.compareModel)
	}

	c.input.SetValue("/solo")
	v, _ = c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if c.compareModel != "" || c.notice != "solo mode" {
		t.Fatalf("/solo should clear compare mode; notice=%q model=%q", c.notice, c.compareModel)
	}

	c.input.SetValue("/wat")
	v, _ = c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if !strings.Contains(c.notice, "unknown") {
		t.Fatalf("unknown command should hint; notice=%q", c.notice)
	}
}

// TestChat_CompareVerdictAndRender asserts the latency verdict (faster side) and
// that a round renders both models, both outputs, tokens, and a side error.
func TestChat_CompareVerdictAndRender(t *testing.T) {
	if fasterSide(sideResult{latencyMs: 100}, sideResult{latencyMs: 200}) != "A" {
		t.Fatal("lower-latency A should win")
	}
	if fasterSide(sideResult{latencyMs: 300}, sideResult{latencyMs: 200}) != "B" {
		t.Fatal("lower-latency B should win")
	}
	if fasterSide(sideResult{latencyMs: 200}, sideResult{latencyMs: 200}) != "" {
		t.Fatal("equal latency is a tie")
	}
	if fasterSide(sideResult{}, sideResult{latencyMs: 200}) != "" {
		t.Fatal("unknown latency yields no verdict")
	}

	r := compareRound{
		prompt: "p",
		a:      sideResult{model: "m-a", text: "alpha", usage: &core.ChatUsage{TotalTokens: 10}, latencyMs: 100},
		b:      sideResult{model: "m-b", text: "beta", latencyMs: 200, err: errors.New("boom")},
	}
	out := compareRoundView(r, 30)
	for _, want := range []string{"m-a", "alpha", "10 tok", "faster", "m-b", "beta", "boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("round view missing %q:\n%s", want, out)
		}
	}
	// empty body falls back to a placeholder; narrow width is clamped.
	if !strings.Contains(compareSideBox("A", sideResult{model: "m"}, 4, false), "…") {
		t.Fatal("empty side should render a placeholder")
	}
}

// TestChat_CompareGuardsAndTrim covers the no-round guards (append/finish are
// no-ops) and the tail-trim + budget clamp in the A/B transcript.
func TestChat_CompareGuardsAndTrim(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	c.compareModel = "b"
	c.appendCompare("A", "x")
	c.appendCompare("B", "x")
	c.finishSide(chatDoneMsg{side: "A"})
	if len(c.rounds) != 0 {
		t.Fatal("append/finish with no rounds must be no-ops")
	}
	for i := 0; i < 6; i++ {
		c.rounds = append(c.rounds, compareRound{prompt: "p", a: sideResult{model: "a", text: "x"}, b: sideResult{model: "b", text: "y"}})
	}
	out := c.compareTranscript(120, 2) // clamps to 4 then trims
	if n := strings.Count(out, "\n") + 1; n > 4 {
		t.Fatalf("compareTranscript should trim to the clamped budget, got %d lines", n)
	}
}

// TestModelsHelpers covers the clip + ktok formatters, including rune safety.
func TestModelsHelpers(t *testing.T) {
	if kit.Clip("short", 10) != "short" || kit.Clip("0123456789", 5) != "0123…" {
		t.Fatalf("clip wrong: %q %q", kit.Clip("short", 10), kit.Clip("0123456789", 5))
	}
	if got := kit.Clip("αβγδε", 3); got != "αβ…" { // rune-safe: never splits a multibyte char
		t.Fatalf("clip should be rune-safe, got %q", got)
	}
}

// TestChat_LeaveTearsDownStream verifies navigating away mid-stream cancels the
// stream and resets streaming state so re-entry is usable.
func TestChat_LeaveTearsDownStream(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	c.input.SetValue("hi")
	c.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // sendSolo → streaming
	if !c.streaming {
		t.Fatal("send should start streaming")
	}
	c.Leave()
	if c.streaming || c.pending != 0 || !c.input.Focused() {
		t.Fatalf("leave should reset stream state; streaming=%v pending=%d focused=%v", c.streaming, c.pending, c.input.Focused())
	}
	// leave is safe with no active stream (nil streamers).
	newChat(sampleGateway(), testSession()).Leave()
}

// TestEvent_LeaveStopsExplain verifies navigating away cancels an in-flight
// explanation stream.
func TestEvent_LeaveStopsExplain(t *testing.T) {
	e := newEvent(sampleGateway(), testSession())
	e.SetID("ev1")
	e.Update(e.Init()()) // load the event
	_, cmd := e.Update(keyRunes("x"))
	if !e.explaining || cmd == nil {
		t.Fatal("x should start an explanation stream")
	}
	e.Leave()
	if e.explaining {
		t.Fatal("leave should stop the explanation")
	}
	newEvent(sampleGateway(), testSession()).Leave() // safe with no stream
}

// TestProvider_Label asserts the friendly-label fallback (displayName→name).
func TestProvider_Label(t *testing.T) {
	if (core.Provider{Name: "openai", DisplayName: "OpenAI"}).Label() != "OpenAI" {
		t.Fatal("Label should prefer displayName")
	}
	if (core.Provider{Name: "openai"}).Label() != "openai" {
		t.Fatal("Label should fall back to name")
	}
}

// TestChat_SystemTempClearCommands covers /system (set + clear), /temp (set,
// default, invalid), /clear, and /help, plus the status-line rendering.
func TestChat_SystemTempClearCommands(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	if c.statusLine() != "" {
		t.Fatal("a fresh chat has no status line")
	}

	c.input.SetValue("/system be terse")
	v, _ := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if c.system != "be terse" {
		t.Fatalf("/system should set the prompt, got %q", c.system)
	}
	// withSystem prepends a system message.
	msgs := c.withSystem([]core.ChatMessage{{Role: "user", Content: "hi"}})
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[0].Content != "be terse" {
		t.Fatalf("withSystem should prepend the system message: %+v", msgs)
	}

	c.input.SetValue("/system")
	v, _ = c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if c.system != "" || !strings.Contains(c.notice, "cleared") {
		t.Fatalf("/system with no text should clear; notice=%q", c.notice)
	}

	c.input.SetValue("/temp 0.7")
	v, _ = c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if c.temperature == nil || *c.temperature != 0.7 {
		t.Fatalf("/temp should set temperature, got %v", c.temperature)
	}
	c.input.SetValue("/temp 9")
	v, _ = c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if *c.temperature != 0.7 || !strings.Contains(c.notice, "0.0") {
		t.Fatalf("out-of-range /temp should be rejected; notice=%q", c.notice)
	}
	c.input.SetValue("/temp")
	v, _ = c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if c.temperature != nil || !strings.Contains(c.notice, "default") {
		t.Fatalf("/temp with no value should reset to default; notice=%q", c.notice)
	}

	c.input.SetValue("/help")
	v, _ = c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if !strings.Contains(c.notice, "/compare") || !strings.Contains(c.notice, "/temp") {
		t.Fatalf("/help should list commands; notice=%q", c.notice)
	}

	// status line reflects system + temperature once set; a long system prompt
	// is truncated.
	c.system = strings.Repeat("x", 60)
	tmp := 0.3
	c.temperature = &tmp
	if sl := c.statusLine(); !strings.Contains(sl, "sys:") || !strings.Contains(sl, "…") || !strings.Contains(sl, "temp 0.30") {
		t.Fatalf("status line should show truncated system + temp: %q", sl)
	}

	// /clear resets the transcript and cost.
	c.turns = []chatTurn{{role: "user", text: "x"}}
	c.sessionCost = 1.0
	c.input.SetValue("/clear")
	v, _ = c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	if len(c.turns) != 0 || c.sessionCost != 0 || !strings.Contains(c.notice, "cleared") {
		t.Fatalf("/clear should reset turns+cost; turns=%d cost=%v", len(c.turns), c.sessionCost)
	}
}

// TestChat_RunningCostFromCatalog verifies pricing is loaded from the catalog
// and a completed turn accrues a non-zero running session cost shown in the UI.
func TestChat_RunningCostFromCatalog(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	// Init schedules a pricing fetch; resolve it directly.
	v, _ := c.Update(c.fetchPricing()())
	c = v.(*chat)
	if p, ok := c.pricing["gpt-4o-mini"]; !ok || p.out != 0.60 {
		t.Fatalf("pricing should load from the catalog: %+v", c.pricing)
	}
	// with pricing already loaded, Init does not re-fetch (just the cursor blink).
	if c.Init() == nil {
		t.Fatal("Init should still return the blink cmd")
	}

	// run a solo turn to completion.
	c.input.SetValue("hi")
	v, cmd := c.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	c = v.(*chat)
	delta := cmd()
	v, cmd2 := c.Update(delta)
	c = v.(*chat)
	v, _ = c.Update(cmd2())
	c = v.(*chat)
	// usage prompt=11, completion=8 at 0.15/0.60 per-million → > 0.
	want := (11*0.15 + 8*0.60) / 1e6
	if c.sessionCost <= 0 || math.Abs(c.sessionCost-want) > 1e-12 {
		t.Fatalf("running cost wrong: got %v want %v", c.sessionCost, want)
	}
	if !strings.Contains(c.View(120, 30), "this session") {
		t.Fatal("status line should show running session cost")
	}
	// costOf is zero when pricing is missing.
	if c.costOf("unknown-model", &core.ChatUsage{PromptTokens: 100}) != 0 {
		t.Fatal("costOf should be 0 for an unpriced model")
	}
	if c.costOf("gpt-4o-mini", nil) != 0 {
		t.Fatal("costOf should be 0 with no usage")
	}
}

// TestChat_PricingFetchError tolerates an unavailable catalog (cost stays 0).
func TestChat_PricingFetchError(t *testing.T) {
	c := newChat(&fakeGateway{err: errors.New("catalog down")}, testSession())
	v, _ := c.Update(c.fetchPricing()())
	c = v.(*chat)
	if c.pricing != nil {
		t.Fatal("a failed catalog fetch should leave pricing nil")
	}
	// Init retries while pricing is nil.
	if c.Init() == nil {
		t.Fatal("Init should still schedule work")
	}
}

// TestSLO_ProviderToggle covers the non-prod provider enable/disable write from
// the drill: 't' fires immediately, the panel reflects the new state, and an
// unresolved provider cannot be toggled.
func TestSLO_ProviderToggle(t *testing.T) {
	gw := sampleGateway() // openai provider Enabled=true
	sv := drillTo(t, gw)
	if out := sv.View(120, 30); !strings.Contains(out, "enabled") || !strings.Contains(out, "t: toggle") {
		t.Fatalf("detail should show enabled state + toggle hint:\n%s", out)
	}
	v, _ := sv.Update(keyRunes("t"))
	sv = v.(*slo)
	if !sv.cf.Capturing() {
		t.Fatal("non-prod toggle must raise the confirm gate, not fire")
	}
	v, cmd := sv.Update(keyRunes("y")) // allow → write
	sv = v.(*slo)
	if cmd == nil {
		t.Fatal("allowing the gate should fire the write")
	}
	v, _ = sv.Update(cmd())
	sv = v.(*slo)
	if gw.lastProviderEnabled == nil || *gw.lastProviderEnabled {
		t.Fatal("toggle of an enabled provider should disable it")
	}
	if sv.detailProvider.Enabled || !strings.Contains(sv.View(120, 30), "provider disabled") {
		t.Fatalf("panel should reflect the disabled state:\n%s", sv.View(120, 30))
	}

	// toggling a DISABLED provider enables it (the "enable" verb branch).
	dgw := sampleGateway()
	dgw.providers = &core.ProvidersResult{Data: []core.Provider{{ID: "prov-openai", Name: "openai", DisplayName: "OpenAI", Enabled: false}}}
	ds := drillTo(t, dgw)
	if !strings.Contains(ds.View(120, 30), "disabled") {
		t.Fatalf("a disabled provider should show disabled:\n%s", ds.View(120, 30))
	}
	dv, _ := ds.Update(keyRunes("t"))
	ds = dv.(*slo)
	dv, dcmd := ds.Update(keyRunes("y")) // allow → write
	ds = dv.(*slo)
	dv, _ = ds.Update(dcmd())
	ds = dv.(*slo)
	if dgw.lastProviderEnabled == nil || !*dgw.lastProviderEnabled {
		t.Fatal("toggling a disabled provider should enable it")
	}
	if !strings.Contains(ds.View(120, 30), "provider enabled") {
		t.Fatalf("panel should reflect the enabled state:\n%s", ds.View(120, 30))
	}
}

// TestSLO_ProviderToggleProdConfirm covers the prod typed-confirm gate on the
// provider write.
func TestSLO_ProviderToggleProdConfirm(t *testing.T) {
	gw := sampleGateway()
	s := newSLO(gw, kit.Session{EnvName: "prod", IsProd: true})
	v, _ := s.Update(s.Init()())
	sv := v.(*slo)
	v, cmd := sv.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // drill
	sv = v.(*slo)
	v, _ = sv.Update(cmd()) // detail loaded
	sv = v.(*slo)
	v, _ = sv.Update(keyRunes("t"))
	sv = v.(*slo)
	if !sv.cf.Capturing() || !sv.Capturing() || gw.lastProviderEnabled != nil {
		t.Fatal("prod toggle must require confirmation before writing")
	}
	if !strings.Contains(sv.View(120, 30), "PROD") {
		t.Fatal("the confirm panel should show the PROD warning")
	}
	v, cmd = sv.Update(keyRunes("y")) // quick-allow the prod confirm
	sv = v.(*slo)
	if cmd == nil {
		t.Fatal("a matching confirmation should fire the write")
	}
	v, _ = sv.Update(cmd())
	sv = v.(*slo)
	if gw.lastProviderEnabled == nil {
		t.Fatal("the write should fire after confirmation")
	}

	// an unresolved provider cannot be toggled.
	noCat := &fakeGateway{sp: sampleGateway().sp, phases: &core.LatencyPhasesResult{Rows: []core.LatencyPhaseRow{{GroupKey: "x", GroupLabel: "X"}}}, fallbacks: &core.FallbacksResult{}}
	us := newSLO(noCat)
	uv, _ := us.Update(us.Init()())
	usv := uv.(*slo)
	uv, ucmd := usv.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // drill unresolved
	usv = uv.(*slo)
	if ucmd != nil { // unresolved → no detail fetch
		usv.Update(ucmd())
	}
	if _, tcmd := usv.Update(keyRunes("t")); tcmd != nil {
		t.Fatal("an unresolved provider must not be toggleable")
	}
}

// TestCost_CacheFlush covers the cache-flush write (non-prod immediate, prod
// confirm, and the error path).
func TestCost_CacheFlush(t *testing.T) {
	gw := sampleGateway()
	c := newCost(gw)
	c.Update(c.Init()())
	if !strings.Contains(c.Help(), "flush") {
		t.Fatal("help should advertise the flush")
	}
	v, _ := c.Update(keyRunes("f"))
	c = v.(*cost)
	if !c.cf.Capturing() {
		t.Fatal("non-prod flush must raise the confirm gate, not fire")
	}
	v, cmd := c.Update(keyRunes("y")) // allow → flush
	c = v.(*cost)
	if cmd == nil {
		t.Fatal("allowing the gate should fire the flush")
	}
	v, _ = c.Update(cmd())
	c = v.(*cost)
	if !gw.cacheFlushed || !strings.Contains(c.View(120, 20), "flushed") {
		t.Fatalf("cache should be flushed + noted:\n%s", c.View(120, 20))
	}

	// prod requires confirmation.
	pgw := sampleGateway()
	pc := newCost(pgw, kit.Session{EnvName: "prod", IsProd: true})
	pc.Update(pc.Init()())
	v, _ = pc.Update(keyRunes("f"))
	pc = v.(*cost)
	if !pc.cf.Capturing() || !pc.Capturing() || pgw.cacheFlushed {
		t.Fatal("prod flush must require confirmation")
	}
	if !strings.Contains(pc.View(120, 20), "PROD") {
		t.Fatal("confirm view should show PROD")
	}
	if !strings.Contains(pc.Help(), "confirm") {
		t.Fatalf("help should show the confirm hint while confirming: %q", pc.Help())
	}
	v, cmd = pc.Update(keyRunes("y")) // quick-allow the prod confirm
	pc = v.(*cost)
	v, _ = pc.Update(cmd())
	pc = v.(*cost)
	if !pgw.cacheFlushed {
		t.Fatal("confirmed flush should fire")
	}

	// error path surfaces.
	ec := newCost(&fakeGateway{err: errors.New("flush-down")})
	ec.Update(ec.Init()())
	v, _ = ec.Update(keyRunes("f"))
	ec = v.(*cost)
	v, cmd = ec.Update(keyRunes("y")) // allow → flush
	ec = v.(*cost)
	v, _ = ec.Update(cmd())
	ec = v.(*cost)
	if !strings.Contains(ec.View(120, 20), "flush-down") {
		t.Fatalf("flush error should surface:\n%s", ec.View(120, 20))
	}
}

// TestSLO_FriendlyNameFallbacks asserts the provider-label resolution priority:
// catalog DisplayName → catalog Name → phase GroupLabel → GroupKey. A UUID is
// never the label an operator sees.
func TestSLO_FriendlyNameFallbacks(t *testing.T) {
	s := &slo{providers: map[string]core.Provider{
		"hasname": {ID: "uuid-1", Name: "hasname"}, // DisplayName empty → Name
	}}
	if got := s.friendlyName(core.LatencyPhaseRow{GroupKey: "hasname"}); got != "hasname" {
		t.Fatalf("empty DisplayName should fall back to Name, got %q", got)
	}
	if got := s.friendlyName(core.LatencyPhaseRow{GroupKey: "x", GroupLabel: "Label X"}); got != "Label X" {
		t.Fatalf("no catalog match should fall back to GroupLabel, got %q", got)
	}
	if got := s.friendlyName(core.LatencyPhaseRow{GroupKey: "onlykey"}); got != "onlykey" {
		t.Fatalf("no label should fall back to GroupKey, got %q", got)
	}
}
