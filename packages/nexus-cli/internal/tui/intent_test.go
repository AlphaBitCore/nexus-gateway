package tui

import (
	"strings"
	"testing"
	"time"
)

func TestParseIntent(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want askIntent
	}{
		{"navigate", `{"action":"navigate","view":"Radar"}`, askIntent{Action: actionNavigate, View: "Radar"}},
		{"navigate fenced", "```json\n{\"action\":\"navigate\",\"view\":\"Cost\"}\n```", askIntent{Action: actionNavigate, View: "Cost"}},
		{"navigate no view", `{"action":"navigate"}`, askIntent{Action: actionUnknown}},
		{"answer cost", `{"action":"answer","source":"cost"}`, askIntent{Action: actionAnswer, Source: sourceCost}},
		{"answer bad source", `{"action":"answer","source":"weather"}`, askIntent{Action: actionUnknown}},
		{"explain", `{"action":"explain","event_id":"abc"}`, askIntent{Action: actionExplain, EventID: "abc"}},
		{"explain no id", `{"action":"explain"}`, askIntent{Action: actionUnknown}},
		{"bad json no brace", `not json`, askIntent{Action: actionUnknown}},
		{"malformed json with brace", `{"action": }`, askIntent{Action: actionUnknown}},
		{"unbalanced open brace", `{"action":"navigate","view":"Radar"`, askIntent{Action: actionUnknown}},
		{"unknown action", `{"action":"disable_provider","view":"x"}`, askIntent{Action: actionUnknown}},
		{"write verb rejected", `{"action":"delete","event_id":"abc"}`, askIntent{Action: actionUnknown}},
		{"prose then json", `Sure! {"action":"navigate","view":"SLO"}`, askIntent{Action: actionNavigate, View: "SLO"}},
		{"brace in string value", `{"action":"navigate","view":"Cost","filter":{"provider":"a{b}c"}}`, askIntent{Action: actionNavigate, View: "Cost"}},
		{"escaped quote in string", `{"action":"explain","event_id":"a\"b"}`, askIntent{Action: actionExplain, EventID: `a"b`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseIntent([]byte(c.raw))
			if got.Action != c.want.Action || got.View != c.want.View ||
				got.Source != c.want.Source || got.EventID != c.want.EventID {
				t.Fatalf("parseIntent(%q) = %+v, want %+v", c.raw, got, c.want)
			}
		})
	}
}

func TestParseIntentNavigateCarriesFilter(t *testing.T) {
	got := parseIntent([]byte(`{"action":"navigate","view":"Radar","filter":{"provider":"openai","status":"5xx","since":"1h"}}`))
	if got.Action != actionNavigate || got.Filter == nil {
		t.Fatalf("expected navigate with filter, got %+v", got)
	}
	if got.Filter.Provider != "openai" || got.Filter.Status != "5xx" || got.Filter.Since != "1h" {
		t.Fatalf("filter not carried through: %+v", got.Filter)
	}
}

func TestExtractJSONObjectNoBrace(t *testing.T) {
	if obj := extractJSONObject([]byte("no object here")); obj != nil {
		t.Fatalf("expected nil for braceless input, got %q", obj)
	}
}

func TestFilterFromIntent(t *testing.T) {
	base := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	t.Run("nil filter defaults limit only", func(t *testing.T) {
		f := filterFromIntent(nil, base)
		if f.Limit != 20 || f.Provider != "" || f.StatusRange != "" || !f.StartTime.IsZero() {
			t.Fatalf("unexpected default filter: %+v", f)
		}
	})
	t.Run("provider + 5xx + 1h", func(t *testing.T) {
		f := filterFromIntent(&askFilter{Provider: "openai", Status: "5xx", Since: "1h"}, base)
		if f.Provider != "openai" || f.StatusRange != "5xx" {
			t.Fatalf("provider/status not mapped: %+v", f)
		}
		if got := base.Sub(f.StartTime); got != time.Hour {
			t.Fatalf("since window = %v, want 1h", got)
		}
	})
	t.Run("24h and 7d windows", func(t *testing.T) {
		if f := filterFromIntent(&askFilter{Since: "24h"}, base); base.Sub(f.StartTime) != 24*time.Hour {
			t.Fatalf("24h window wrong: %v", base.Sub(f.StartTime))
		}
		if f := filterFromIntent(&askFilter{Since: "7d"}, base); base.Sub(f.StartTime) != 7*24*time.Hour {
			t.Fatalf("7d window wrong: %v", base.Sub(f.StartTime))
		}
	})
	t.Run("unknown status and since dropped", func(t *testing.T) {
		f := filterFromIntent(&askFilter{Status: "teapot", Since: "fortnight"}, base)
		if f.StatusRange != "" {
			t.Fatalf("expected unknown status dropped, got %q", f.StatusRange)
		}
		if !f.StartTime.IsZero() {
			t.Fatalf("expected unknown since dropped, got %v", f.StartTime)
		}
	})
}

func TestBuildRouterPrompt(t *testing.T) {
	entries := []viewEntry{{name: "Radar"}, {name: "Cost"}}
	p := buildRouterPrompt(entries)
	for _, want := range []string{"Radar", "Cost", "navigate", "answer", "explain", "cost", "errors", "slo", "fleet", "JSON"} {
		if !strings.Contains(p, want) {
			t.Fatalf("router prompt missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(p), "delete") || strings.Contains(strings.ToLower(p), "disable") {
		t.Fatalf("router prompt must not invite a write verb")
	}
}

func TestBuildAnswerPromptClips(t *testing.T) {
	big := []byte(strings.Repeat("x", answerDataMax+500))
	p := buildAnswerPrompt("how much did I spend?", big)
	if !strings.Contains(p, "how much did I spend?") {
		t.Fatal("answer prompt missing the question")
	}
	if strings.Count(p, "x") > answerDataMax {
		t.Fatalf("answer prompt data not clipped: %d x's", strings.Count(p, "x"))
	}
}
