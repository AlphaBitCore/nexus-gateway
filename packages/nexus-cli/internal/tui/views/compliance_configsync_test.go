package views

import (
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// TestComplianceConfigSync_Lifecycle covers the msg→tick→refetch and error-render
// paths of the two poll-only overview views.
func TestComplianceConfigSync_Lifecycle(t *testing.T) {
	c := newCompliance(sampleGateway())
	c.Update(complianceMsg{res: &core.ComplianceOverview{}})
	if c.loading {
		t.Fatal("a result msg clears the loading flag")
	}
	if _, cmd := c.Update(complianceTick{}); cmd == nil {
		t.Fatal("a tick must schedule a re-fetch")
	}
	c.Update(complianceMsg{err: errors.New("compliance-down")})
	if !strings.Contains(c.View(100, 20), "compliance-down") {
		t.Fatal("the error must render in the view")
	}

	s := newConfigSync(sampleGateway())
	s.Update(configSyncMsg{res: &core.ConfigSyncResult{}})
	if s.loading {
		t.Fatal("a result msg clears the loading flag")
	}
	if _, cmd := s.Update(configSyncTick{}); cmd == nil {
		t.Fatal("a tick must schedule a re-fetch")
	}
	s.Update(configSyncMsg{err: errors.New("sync-down")})
	if !strings.Contains(s.View(100, 20), "sync-down") {
		t.Fatal("the error must render in the view")
	}
}

// TestDriveTypeAccessors covers the exported drive methods + query accessors the
// shell uses on the three agent-driven views (Models/Event/Radar). They are only
// called from package tui at runtime, so this in-package test gives them coverage.
func TestDriveTypeAccessors(t *testing.T) {
	m := newModels(sampleGateway())
	m.EnterPick("gpt-4o")
	if !m.Picking() {
		t.Fatal("EnterPick must put the view in pick mode (Picking)")
	}

	e := newEvent(sampleGateway(), testSession())
	e.SetID("ev9")
	if e.ID() != "ev9" {
		t.Fatalf("SetID → ID, got %q", e.ID())
	}
	e.SetIDExplain("ev10")
	if e.ID() != "ev10" || !e.AutoExplain() {
		t.Fatalf("SetIDExplain → ID(%q) + AutoExplain(%v)", e.ID(), e.AutoExplain())
	}
	_ = e.Crumb() // dynamic breadcrumb segment

	r := newRadar(sampleGateway())
	r.ApplyFilter(core.TrafficFilter{Provider: "openai", StatusRange: "5xx"})
	if r.Filter().Provider != "openai" || r.Filter().StatusRange != "5xx" {
		t.Fatalf("ApplyFilter → Filter, got %+v", r.Filter())
	}
	_ = r.ErrorsOnly()
}
