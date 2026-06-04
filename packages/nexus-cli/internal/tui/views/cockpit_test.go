package views

import (
	"errors"
	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
	"strings"
	"testing"
	"time"
)

func TestCockpit_InitFetchAndPollSchedule(t *testing.T) {
	c := newCockpit(sampleGateway())
	if c.Init() == nil {
		t.Fatal("Init should start the fetch + animation batch")
	}
	d, ok := c.fetch()().(cockpitData)
	if !ok || d.sp == nil || d.inst == nil || d.traffic == nil || d.prov == nil {
		t.Fatalf("fetch should populate every source, got %#v", d)
	}
	if _, cmd := c.Update(d); cmd == nil {
		t.Fatal("Update(cockpitData) should schedule the next poll tick")
	}
	if _, cmd := c.Update(cockpitTick{}); cmd == nil {
		t.Fatal("cockpitTick should refetch")
	}
	if !strings.Contains(c.Help(), "commands") {
		t.Fatalf("help should describe the keybar, got %q", c.Help())
	}
}

func TestCockpit_HeroCardsAndDeltas(t *testing.T) {
	c := newCockpit(sampleGateway())
	c.Update(c.fetch()())
	out := c.View(120, 30)
	// Window totals from the sample series: requests 42, cost $1.5000, 5xx errors 2.
	for _, want := range []string{"Requests", "42", "Cost USD", "$1.5000", "Errors"} {
		if !strings.Contains(out, want) {
			t.Errorf("hero cards missing %q:\n%s", want, out)
		}
	}
	// requests rose (20→22) → ▲ ; cost fell (1.0→0.5) → ▼.
	if !strings.Contains(out, "▲") || !strings.Contains(out, "▼") {
		t.Fatalf("hero cards should render trend arrows:\n%s", out)
	}
}

func TestCockpitDeltaFor(t *testing.T) {
	series := []core.SparklineBucket{
		{Values: map[string]float64{"request_count": 10}},
		{Values: map[string]float64{"request_count": 14}},
	}
	if got := deltaFor(series, "request_count"); got != 4 {
		t.Fatalf("delta should be last-prev = 4, got %v", got)
	}
	if deltaFor(series[:1], "request_count") != 0 {
		t.Fatal("a single bucket has no delta → 0")
	}
	if deltaFor(nil, "x") != 0 {
		t.Fatal("an empty series has no delta → 0")
	}
}

func TestCockpitDeltaGlyph(t *testing.T) {
	if g, col := deltaGlyph(2, false); g != "▲" || col != styles.Green {
		t.Fatalf("a rising good metric → green ▲, got %q", g)
	}
	if g, col := deltaGlyph(-2, false); g != "▼" || col != styles.Red {
		t.Fatalf("a falling good metric → red ▼, got %q", g)
	}
	// Errors are bad-when-rising: the colors flip.
	if g, col := deltaGlyph(2, true); g != "▲" || col != styles.Red {
		t.Fatalf("a rising error count → red ▲, got %q", g)
	}
	if _, col := deltaGlyph(-2, true); col != styles.Green {
		t.Fatal("a falling error count → green")
	}
	if g, _ := deltaGlyph(0, false); g != "·" {
		t.Fatalf("a flat delta → muted dot, got %q", g)
	}
}

func TestCockpit_WaterfallBadgesAndStatusColor(t *testing.T) {
	g := sampleGateway()
	g.list = &core.TrafficList{Data: []core.TrafficEvent{
		{StatusCode: 200, ModelName: "gpt-4", CacheStatus: "hit", Timestamp: time.Now()},
		{StatusCode: 500, ModelName: "claude", Timestamp: time.Now()},
		{StatusCode: 403, ModelName: "blocked-call", RequestHookDecision: "block", Timestamp: time.Now()},
	}}
	c := newCockpit(g)
	c.Update(c.fetch()())
	out := c.View(120, 30)
	if !strings.Contains(out, "HIT") {
		t.Fatalf("a cache-hit row should show the HIT badge:\n%s", out)
	}
	if !strings.Contains(out, "BLOCKED") {
		t.Fatalf("a hook-blocked row should show the BLOCKED badge:\n%s", out)
	}
	if !strings.Contains(out, "500") {
		t.Fatalf("the 5xx row's status code should render:\n%s", out)
	}
}

func TestCockpit_LeaderboardRanksByVolumeWithFriendlyLabel(t *testing.T) {
	g := sampleGateway()
	g.byProvider = &core.ByProviderResult{Data: []core.ProviderUsageRow{
		{Provider: "p-small", ProviderLabel: "Anthropic", RequestCount: 30},
		{Provider: "p-big", ProviderLabel: "OpenAI", RequestCount: 200},
	}}
	c := newCockpit(g)
	c.Update(c.fetch()())
	out := c.View(120, 30)
	// Friendly label is shown, never the bare provider id.
	if !strings.Contains(out, "OpenAI") || strings.Contains(out, "p-big") {
		t.Fatalf("leaderboard should show the friendly provider label, not the id:\n%s", out)
	}
	// The busier provider ranks above the quieter one.
	if strings.Index(out, "OpenAI") > strings.Index(out, "Anthropic") {
		t.Fatalf("the busier provider should rank first:\n%s", out)
	}
}

func TestCockpit_StatusLightsReflectTrouble(t *testing.T) {
	g := sampleGateway()
	g.alerts = &core.AlertsResult{Alerts: []core.Alert{
		{State: "firing"},
		{State: "resolved", ResolvedAt: "2026-05-28T00:00:00Z"},
	}}
	g.ksState = &core.KillSwitchState{Engaged: true, Known: true}
	g.passSnap = &core.PassthroughSnapshot{
		Adapters:  map[string]core.PassthroughTier{"openai-compat": {Enabled: true, BypassHooks: true}},
		Providers: map[string]core.PassthroughTier{},
	}
	c := newCockpit(g)
	c.Update(c.fetch()())
	out := c.View(120, 30)
	if !strings.Contains(out, "1 alerts firing") {
		t.Fatalf("exactly one alert should read as firing:\n%s", out)
	}
	if !strings.Contains(out, "ENGAGED") {
		t.Fatalf("an engaged kill-switch should be surfaced:\n%s", out)
	}
	if !strings.Contains(out, "overrides: 1") {
		t.Fatalf("an active passthrough override should be counted:\n%s", out)
	}
}

func TestCockpit_HealthyStatusLights(t *testing.T) {
	c := newCockpit(sampleGateway()) // no alerts, kill not engaged, no passthrough
	c.Update(c.fetch()())
	out := c.View(120, 30)
	for _, want := range []string{"27 nodes", "no alerts firing", "kill-switch armed", "passthrough off"} {
		if !strings.Contains(out, want) {
			t.Errorf("a healthy cockpit should show %q:\n%s", want, out)
		}
	}
}

func TestCockpit_PulseAdvancesAndBlinksWithoutRefetch(t *testing.T) {
	g := sampleGateway()
	g.alerts = &core.AlertsResult{Alerts: []core.Alert{{State: "firing"}}}
	c := newCockpit(g)
	c.Update(c.fetch()())
	frameEven := c.View(120, 30)
	_, cmd := c.Update(cockpitPulse{})
	if cmd == nil {
		t.Fatal("a pulse should reschedule the next animation frame")
	}
	if c.pulse != 1 {
		t.Fatalf("the pulse phase should advance, got %d", c.pulse)
	}
	if frameEven == c.View(120, 30) {
		t.Fatal("a firing status light should visibly blink between pulse frames")
	}
}

func TestCockpit_RetainsLastGoodOnFailedPoll(t *testing.T) {
	c := newCockpit(sampleGateway())
	c.Update(c.fetch()())
	if c.sp == nil || c.prov == nil {
		t.Fatal("precondition: the first poll populated data")
	}
	// A later poll that fails (all-nil data + err) must not blank the cockpit.
	c.Update(cockpitData{err: errors.New("blip")})
	if c.sp == nil || c.prov == nil {
		t.Fatal("a failed poll must retain the last-good data")
	}
	if c.err == nil {
		t.Fatal("the poll error should be recorded for the last-good banner")
	}
}

func TestCockpit_CapsListsAndClampsLayout(t *testing.T) {
	g := sampleGateway()
	// 7 providers → the leaderboard keeps only the top leaderRows by volume.
	var provs []core.ProviderUsageRow
	for i := 0; i < 7; i++ {
		provs = append(provs, core.ProviderUsageRow{
			Provider: fmt.Sprintf("p%d", i), ProviderLabel: fmt.Sprintf("Prov%d", i), RequestCount: (i + 1) * 10,
		})
	}
	g.byProvider = &core.ByProviderResult{Data: provs}
	// 12 traffic rows → the waterfall caps at the row budget.
	var evs []core.TrafficEvent
	for i := 0; i < 12; i++ {
		evs = append(evs, core.TrafficEvent{StatusCode: 200, ModelName: fmt.Sprintf("m%d", i), Timestamp: time.Now()})
	}
	g.list = &core.TrafficList{Data: evs}
	c := newCockpit(g)
	c.Update(c.fetch()())
	// width<1 and a tiny height exercise the layout clamps without panicking.
	out := c.View(0, 1)
	if !strings.Contains(out, "Prov6") || strings.Contains(out, "Prov0") {
		t.Fatalf("leaderboard should keep the busiest providers and drop the rest:\n%s", out)
	}
}

func TestCockpit_LoadingAndErrorStates(t *testing.T) {
	if !strings.Contains(newCockpit(sampleGateway()).View(80, 20), "loading") {
		t.Fatal("the initial cockpit should show a loading state")
	}
	e := newCockpit(&fakeGateway{err: errors.New("boom")})
	e.Update(e.fetch()())
	out := e.View(80, 20)
	if !strings.Contains(out, "boom") || !strings.Contains(out, "last-good") {
		t.Fatalf("a fetch error should surface with the last-good banner:\n%s", out)
	}
}
