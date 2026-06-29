package runtime

import (
	"github.com/goccy/go-json"
	"strings"
	"testing"
	"time"
)

func TestWindowRange(t *testing.T) {
	approx := func(d, want time.Duration) bool {
		diff := d - want
		if diff < 0 {
			diff = -diff
		}
		return diff < time.Minute
	}
	cases := map[string]time.Duration{
		"1h":  time.Hour,
		"24h": 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"":    7 * 24 * time.Hour, // default
		"zzz": 7 * 24 * time.Hour, // unknown → default
	}
	for w, want := range cases {
		s, e := windowRange(w)
		if !e.After(s) {
			t.Fatalf("window %q: end must be after start", w)
		}
		if !approx(e.Sub(s), want) {
			t.Errorf("window %q span = %v, want ~%v", w, e.Sub(s), want)
		}
	}
	// "today" starts at UTC midnight.
	s, e := windowRange("today")
	if s.Hour() != 0 || s.Minute() != 0 || s.Second() != 0 {
		t.Fatalf("today start should be midnight UTC, got %v", s)
	}
	if !e.After(s) || e.Sub(s) > 24*time.Hour {
		t.Fatalf("today span out of range: %v", e.Sub(s))
	}
}

func TestWindowValuesAndArg(t *testing.T) {
	v := windowValues("24h")
	if v.Get("start") == "" || v.Get("end") == "" {
		t.Fatalf("windowValues must set start+end: %v", v)
	}
	if _, err := time.Parse(time.RFC3339, v.Get("start")); err != nil {
		t.Fatalf("start must be RFC3339: %v", err)
	}
	if windowArg(json.RawMessage(`{"window":"today"}`)) != "today" {
		t.Fatal("windowArg should read the window")
	}
	if windowArg(json.RawMessage(`{}`)) != "" {
		t.Fatal("windowArg default is empty")
	}
}

// TestAnalyticsToolsExposeWindow guards that every time-scoped tool advertises the
// window parameter so the model can ask for "today" instead of the 7d default.
func TestAnalyticsToolsExposeWindow(t *testing.T) {
	gw := &fakeGateway{}
	tools := gatewayTools(gw, "", false)
	for _, n := range []string{"observe_health", "observe_traffic_list", "analyze_cost", "analyze_slo", "analyze_compliance"} {
		tl := findTool(tools, n)
		if tl == nil {
			t.Fatalf("%s missing", n)
		}
		if !strings.Contains(string(tl.Schema()), `"window"`) {
			t.Fatalf("%s schema must expose a window param: %s", n, tl.Schema())
		}
	}
}

// TestToolsForwardWindow guards that the window actually reaches the gateway query
// — analyze_cost with window=today narrows the cost query to a today-midnight start,
// and observe_traffic_list scopes its filter to the window.
func TestToolsForwardWindow(t *testing.T) {
	gw := &fakeGateway{}
	tools := gatewayTools(gw, "", false)

	runResourceTool(t, findTool(tools, "analyze_cost"), map[string]any{"window": "today"})
	start := gw.lastCostQ.Get("start")
	if start == "" {
		t.Fatal("analyze_cost must forward a start time")
	}
	st, _ := time.Parse(time.RFC3339, start)
	if st.Hour() != 0 {
		t.Fatalf("window=today must start at midnight, got %v", st)
	}
	if gw.lastCostQ.Get("groupBy") != "provider" {
		t.Fatalf("analyze_cost must keep groupBy alongside the window: %v", gw.lastCostQ)
	}

	runResourceTool(t, findTool(tools, "observe_traffic_list"), map[string]any{"window": "today"})
	if gw.lastTraffic.StartTime.IsZero() || gw.lastTraffic.EndTime.IsZero() {
		t.Fatalf("observe_traffic_list must scope to the window, got %+v", gw.lastTraffic)
	}
}
