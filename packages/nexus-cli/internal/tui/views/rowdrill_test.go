package views

import (
	tea "charm.land/bubbletea/v2"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"strings"
	"testing"
)

func TestAlerts_RowDrillAndBack(t *testing.T) {
	g := &fakeGateway{alerts: &core.AlertsResult{Alerts: []core.Alert{
		{ID: "al-1", TargetLabel: "ai-gateway", Severity: "critical", State: "firing", Message: "5xx spike", DuplicateCount: 3, FiredAt: "2026-05-28T10:00:00Z"},
		{ID: "al-2", TargetLabel: "compliance-proxy", Severity: "warning", State: "firing", Message: "hook latency high"},
		{ID: "al-3", TargetLabel: "retired", State: "resolved", ResolvedAt: "2026-05-28T09:00:00Z"},
	}}}
	a := newAlerts(g)
	a.Update(a.Init()()) // fetch → populate (2 firing, 1 resolved)

	out := a.View(120, 30)
	if !strings.Contains(out, "ai-gateway") || strings.Contains(out, "retired") {
		t.Fatalf("the list should show only firing alerts:\n%s", out)
	}
	// Move to the 2nd firing alert and open its detail (exercise up + down clamps).
	a.Update(tea.KeyPressMsg{Code: tea.KeyUp}) // already at top → clamp no-op
	a.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !a.detail {
		t.Fatal("enter should open the detail drawer")
	}
	det := a.View(120, 30)
	if !strings.Contains(det, "compliance-proxy") || !strings.Contains(det, "hook latency high") {
		t.Fatalf("the drawer should show the selected alert's full record:\n%s", det)
	}
	// esc (via back) closes the drawer; a second back at the list level declines.
	if !a.Back() || a.detail {
		t.Fatal("back should close the drawer")
	}
	if a.Back() {
		t.Fatal("back at the list level must return false so the root pops the nav stack")
	}
	a.detail = true
	if !strings.Contains(a.Help(), "esc back") {
		t.Fatalf("detail help should offer esc back, got %q", a.Help())
	}
	a.detail = false
	if !strings.Contains(a.Help(), "enter open") {
		t.Fatalf("list help should offer enter open, got %q", a.Help())
	}
}

func TestNodes_RowDrillShowsDrift(t *testing.T) {
	g := &fakeGateway{nodes: &core.NodesResult{Nodes: []core.Node{
		{ID: "n-1", Name: "ai-gw-1", Type: "ai-gateway", Status: "online", Version: "1.2.3", TargetVersion: 5, AppliedVersion: 5, ConnProtocol: "ws"},
		{ID: "n-2", Name: "cp-1", Type: "compliance-proxy", Status: "degraded", Version: "1.2.0", TargetVersion: 5, AppliedVersion: 4, ConnProtocol: "http"},
	}}}
	n := newNodes(g)
	n.Update(n.Init()())
	if !strings.Contains(n.Help(), "enter open") {
		t.Fatalf("list help should offer enter open, got %q", n.Help())
	}
	n.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // → the drifted node (cursor 1)
	n.Update(tea.KeyPressMsg{Code: tea.KeyUp})   // back to the first
	n.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // → the drifted node again
	n.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !n.detail {
		t.Fatal("enter should open the node detail drawer")
	}
	if !strings.Contains(n.Help(), "esc back") {
		t.Fatalf("detail help should offer esc back, got %q", n.Help())
	}
	det := n.View(120, 30)
	if !strings.Contains(det, "cp-1") {
		t.Fatalf("the drawer should name the selected node:\n%s", det)
	}
	if !strings.Contains(det, "out of sync") || !strings.Contains(det, "target 5 ≠ applied 4") {
		t.Fatalf("a drifted node's drawer should foreground the version drift:\n%s", det)
	}
	if !n.Back() || n.detail {
		t.Fatal("back should close the drawer")
	}
	if n.Back() {
		t.Fatal("back at the list level must return false so the root pops the nav stack")
	}
}

func TestJobs_RowDrill(t *testing.T) {
	g := &fakeGateway{jobs: &core.JobsResult{Jobs: []core.Job{
		{ID: "j-1", Name: "Cert Alerts", Interval: 3600000000000, Enabled: true, LastRun: "2026-05-28T11:30:20Z"},
		{ID: "j-2", Name: "Retention Sweep", Interval: 86400000000000, Enabled: false},
	}}}
	j := newJobs(g)
	j.Update(j.Init()())
	if !strings.Contains(j.Help(), "enter open") {
		t.Fatalf("list help should offer enter open, got %q", j.Help())
	}
	j.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // → the disabled job
	j.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !j.detail {
		t.Fatal("enter should open the job detail drawer")
	}
	det := j.View(120, 20)
	for _, want := range []string{"Retention Sweep", "disabled", "j-2", "Runs every"} {
		if !strings.Contains(det, want) {
			t.Errorf("the job drawer should show %q:\n%s", want, det)
		}
	}
	if !strings.Contains(j.Help(), "esc back") {
		t.Fatalf("detail help should offer esc back, got %q", j.Help())
	}
	if !j.Back() || j.detail {
		t.Fatal("back should close the drawer")
	}
	if j.Back() {
		t.Fatal("back at the list level must return false")
	}
}

func TestSLO_BackClosesDetail(t *testing.T) {
	s := newSLO(sampleGateway())
	s.inDetail = true
	if !s.Back() || s.inDetail {
		t.Fatal("back should close the SLO provider detail")
	}
	if s.Back() {
		t.Fatal("back at the list level must return false")
	}
}
