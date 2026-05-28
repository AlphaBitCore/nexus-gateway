package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

func TestSparklineChart(t *testing.T) {
	series := []core.SparklineBucket{
		{Values: map[string]float64{"request_count": 5}},
		{Values: map[string]float64{"request_count": 20}},
		{Values: map[string]float64{"request_count": 12}},
	}
	if got := sparklineChart(series, "request_count", 30, 4); got == "" {
		t.Fatal("a non-empty series should render a chart")
	}
	// guards: empty series / no room → ""
	if sparklineChart(nil, "request_count", 30, 4) != "" {
		t.Fatal("empty series → no chart")
	}
	if sparklineChart(series, "request_count", 2, 0) != "" {
		t.Fatal("no room → no chart")
	}
}

func TestOverview_BacklogAndChart(t *testing.T) {
	g := sampleGateway()
	g.dlq = &core.DLQResult{Rows: []json.RawMessage{[]byte("{}"), []byte("{}")}} // depth 2
	o := newOverview(g)
	v, _ := o.Update(o.Init()())
	ov := v.(*overview)
	out := ov.View(120, 30)
	if !strings.Contains(out, "DLQ backlog") || !strings.Contains(out, "2") {
		t.Fatalf("overview should show DLQ backlog depth:\n%s", out)
	}
	if !strings.Contains(out, "trend") {
		t.Fatalf("overview should show the trend chart:\n%s", out)
	}
	// c cycles the chart metric (requests → cost $ → latency)
	if !strings.Contains(ov.help(), "requests") {
		t.Fatalf("initial chart metric is requests, help=%q", ov.help())
	}
	v, _ = ov.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	ov = v.(*overview)
	if ov.chartMetric != 1 || !strings.Contains(ov.help(), "cost") {
		t.Fatalf("c should cycle to cost, metric=%d help=%q", ov.chartMetric, ov.help())
	}
}

func TestOverview_ChartEmptySeries(t *testing.T) {
	g := sampleGateway()
	g.sp = &core.SparklineResult{} // no series
	g.dlq = &core.DLQResult{}
	o := newOverview(g)
	v, _ := o.Update(o.Init()())
	if !strings.Contains(v.View(120, 30), "no series") {
		t.Fatal("empty series should show the 'no series' chart placeholder")
	}
}
