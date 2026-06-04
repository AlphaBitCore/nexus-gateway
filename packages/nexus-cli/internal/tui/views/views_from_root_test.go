package views

import (
	tea "charm.land/bubbletea/v2"
	"errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"strings"
	"testing"
)

func TestRadar_FetchNavOpen(t *testing.T) {
	r := newRadar(sampleGateway())
	v, cmd := r.Update(r.Init()())
	if cmd == nil {
		t.Fatal("Radar Update should schedule a poll tick")
	}
	out := v.View(120, 20)
	for _, want := range []string{"Live traffic", "TIME", "gpt-4", "claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("Radar view missing %q:\n%s", want, out)
		}
	}
	// move cursor down then open.
	v, _ = v.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	rv := v.(*Radar)
	if rv.cursor != 1 {
		t.Fatalf("cursor = %d, want 1", rv.cursor)
	}
	_, openCmd := rv.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if openCmd == nil || openCmd() != (kit.OpenEventMsg{ID: "ev2"}) {
		t.Fatal("enter should emit openEventMsg for the selected row")
	}
	// up clamps at 0.
	v, _ = rv.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if v.(*Radar).cursor != 0 {
		t.Fatal("up should move cursor to 0")
	}
}

func TestRadar_FilterToggleAndError(t *testing.T) {
	r := newRadar(sampleGateway())
	r.Update(r.Init()())
	v, cmd := r.Update(keyRunes("f"))
	if !v.(*Radar).errorsOnly || cmd == nil {
		t.Fatal("f should toggle errorsOnly and refetch")
	}
	if !strings.Contains(v.View(120, 20), "errors only") {
		t.Fatal("filtered Radar should show 'errors only'")
	}
	// error path
	re := newRadar(&fakeGateway{err: errors.New("down")})
	v, _ = re.Update(re.Init()())
	if !strings.Contains(v.View(120, 20), "down") {
		t.Fatal("Radar should surface fetch error")
	}
}

func TestRadar_ApplyFilter(t *testing.T) {
	r := newRadar(sampleGateway())
	r.cursor = 3
	r.ApplyFilter(core.TrafficFilter{Provider: "openai", StatusRange: "5xx"})
	if r.base.Provider != "openai" || r.base.StatusRange != "5xx" {
		t.Fatalf("ApplyFilter did not set base: %+v", r.base)
	}
	if r.base.Limit != 20 {
		t.Fatalf("ApplyFilter should default Limit, got %d", r.base.Limit)
	}
	if r.cursor != 0 {
		t.Fatalf("ApplyFilter should reset cursor, got %d", r.cursor)
	}
	if out := r.View(120, 20); !strings.Contains(out, "provider=openai") || !strings.Contains(out, "5xx") {
		t.Fatalf("filtered Radar header should show the provider and status range:\n%s", out)
	}
	// An error-range filter syncs the errorsOnly display flag.
	r.ApplyFilter(core.TrafficFilter{StatusRange: "error"})
	if !r.errorsOnly {
		t.Fatal("an error-range navigate should set errorsOnly")
	}
}

func TestRadar_EmptyAndLoading(t *testing.T) {
	r := newRadar(sampleGateway())
	if !strings.Contains(r.View(80, 10), "loading") {
		t.Fatal("initial Radar shows loading")
	}
	empty := newRadar(&fakeGateway{list: &core.TrafficList{}})
	v, _ := empty.Update(empty.Init()())
	if !strings.Contains(v.View(80, 10), "no events") {
		t.Fatal("empty Radar shows placeholder")
	}
	// enter with no rows emits nothing.
	if _, cmd := v.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("enter with no rows should not emit")
	}
}

func TestEvent_LoadAndWaterfall(t *testing.T) {
	e := newEvent(sampleGateway(), testSession())
	if !strings.Contains(e.View(80, 20), "Select an event") {
		t.Fatal("event with no id shows prompt")
	}
	e.SetID("ev1")
	if !strings.Contains(e.View(80, 20), "loading") {
		t.Fatal("after SetID, shows loading")
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
	e.SetID("x")
	v, _ := e.Update(e.Init()())
	if !strings.Contains(v.View(80, 20), "nope") {
		t.Fatal("event should surface error")
	}
	// no-latency event → waterfall placeholder
	flat := &core.TrafficEvent{ID: "z", CacheStatus: ""}
	if !strings.Contains(latencyWaterfall(flat), "no latency data") {
		t.Fatal("zero-latency event should show placeholder")
	}
}

func TestEvent_EmptyIDInit(t *testing.T) {
	e := newEvent(sampleGateway(), testSession())
	if e.Init() != nil {
		t.Fatal("event with empty id should have nil Init cmd")
	}
}
