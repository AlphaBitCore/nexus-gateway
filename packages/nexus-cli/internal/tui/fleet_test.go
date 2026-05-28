package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

func TestAlerts_FiringFilterAndView(t *testing.T) {
	g := sampleGateway()
	g.alerts = &core.AlertsResult{Alerts: []core.Alert{
		{ID: "a1", TargetLabel: "OpenAI", Severity: "critical", State: "firing", Message: "error spike", DuplicateCount: 4},
		{ID: "a2", TargetLabel: "old", State: "resolved", ResolvedAt: "2026-05-28T00:00:00Z"},
	}}
	a := newAlerts(g)
	if !strings.Contains(a.View(120, 20), "loading") {
		t.Fatal("initial alerts shows loading")
	}
	v, cmd := a.Update(a.Init()())
	if cmd == nil {
		t.Fatal("alerts schedules a poll tick")
	}
	out := v.View(120, 20)
	if !strings.Contains(out, "OpenAI") || !strings.Contains(out, "error spike") {
		t.Fatalf("firing alert should render:\n%s", out)
	}
	if strings.Contains(out, "old") {
		t.Fatal("resolved alert must be filtered out")
	}
	// empty → placeholder
	empty := newAlerts(&fakeGateway{alerts: &core.AlertsResult{}})
	ev, _ := empty.Update(empty.Init()())
	if !strings.Contains(ev.View(120, 20), "no firing alerts") {
		t.Fatal("no firing alerts placeholder")
	}
	// error surfaced
	er := newAlerts(&fakeGateway{err: errors.New("alerts-down")})
	evv, _ := er.Update(er.Init()())
	if !strings.Contains(evv.View(120, 20), "alerts-down") {
		t.Fatal("alerts error should surface")
	}
}

func TestAlerts_SeverityColor(t *testing.T) {
	if severityColor("critical") != styles.Red || severityColor("warning") != styles.Amber || severityColor("info") != styles.Brand {
		t.Fatal("severity RAG colors wrong")
	}
}

func TestNodes_DriftAndView(t *testing.T) {
	g := sampleGateway()
	g.nodes = &core.NodesResult{Total: 2, Nodes: []core.Node{
		{Name: "ai-gw-1", Type: "ai-gateway", Status: "online", Version: "1.2.3", TargetVersion: 5, AppliedVersion: 5},
		{Name: "agent-9", Type: "agent", Status: "degraded", TargetVersion: 7, AppliedVersion: 6},
	}}
	n := newNodes(g)
	if !strings.Contains(n.View(120, 20), "loading") {
		t.Fatal("initial nodes shows loading")
	}
	v, cmd := n.Update(n.Init()())
	if cmd == nil {
		t.Fatal("nodes schedules a poll tick")
	}
	out := v.View(120, 20)
	if !strings.Contains(out, "ai-gw-1") || !strings.Contains(out, "in sync") {
		t.Fatalf("synced node should render in sync:\n%s", out)
	}
	if !strings.Contains(out, "out of sync") {
		t.Fatalf("drifted node should render out of sync:\n%s", out)
	}
	// empty + error
	empty := newNodes(&fakeGateway{nodes: &core.NodesResult{}})
	ev, _ := empty.Update(empty.Init()())
	if !strings.Contains(ev.View(120, 20), "no nodes") {
		t.Fatal("no nodes placeholder")
	}
	er := newNodes(&fakeGateway{err: errors.New("nodes-down")})
	evv, _ := er.Update(er.Init()())
	if !strings.Contains(evv.View(120, 20), "nodes-down") {
		t.Fatal("nodes error should surface")
	}
}

func TestNodes_StatusColor(t *testing.T) {
	if nodeStatusColor("online") != styles.Green || nodeStatusColor("degraded") != styles.Amber || nodeStatusColor("offline") != styles.Red {
		t.Fatal("node status RAG colors wrong")
	}
}

func TestRadar_FireworksBadges(t *testing.T) {
	g := sampleGateway()
	g.list = &core.TrafficList{Total: 3, Data: []core.TrafficEvent{
		{ID: "b1", StatusCode: 403, ModelName: "m", RequestHookDecision: "block"},
		{ID: "r1", StatusCode: 200, ModelName: "m", ResponseHookDecision: "redact"},
		{ID: "h1", StatusCode: 200, ModelName: "m", CacheStatus: "hit", EstCostUSD: 0.05},
	}}
	r := newRadar(g)
	v, _ := r.Update(r.Init()())
	out := v.View(120, 20)
	if !strings.Contains(out, "BLOCKED") || !strings.Contains(out, "PII") || !strings.Contains(out, "HIT") {
		t.Fatalf("radar should show fireworks badges:\n%s", out)
	}
	if !strings.Contains(out, "$0.05000") {
		t.Fatalf("radar header should show the window cost total:\n%s", out)
	}
}
