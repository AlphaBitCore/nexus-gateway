package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

func TestSituationSnapshotRendersAggregates(t *testing.T) {
	gw := &fakeGateway{
		instances: &core.InstancesResult{Count: 27, Services: map[string]core.ServiceSummary{"cp": {}, "aigw": {}}},
		sparkline: &core.SparklineResult{},
	}
	snap, err := NewSituation(gw).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snap.Health, "27 nodes") || !strings.Contains(snap.Health, "2 services") {
		t.Fatalf("health must summarize node/service counts, got %q", snap.Health)
	}
}

func TestSituationToleratesPartialFailure(t *testing.T) {
	// A failing sub-call must not abort the whole snapshot (soft per-field).
	gw := &fakeGateway{errOn: "Alerts", instances: &core.InstancesResult{Count: 1}, sparkline: &core.SparklineResult{}}
	snap, err := NewSituation(gw).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("a partial failure must not error the snapshot, got %v", err)
	}
	if snap.Health == "" {
		t.Fatal("the healthy fields must still be populated when one sub-call fails")
	}
	if snap.FiringAlerts != "" {
		t.Fatal("the failed field must be left empty (AssembleContext omits it)")
	}
}

func TestSituationHealthSurvivesInstancesFailure(t *testing.T) {
	// Sparkline succeeds but Instances fails: the traffic totals must still render
	// (architect NIT-1) with a "node count unavailable" suffix, not drop entirely.
	gw := &fakeGateway{errOn: "Instances", sparkline: &core.SparklineResult{}}
	snap, _ := NewSituation(gw).Snapshot(context.Background())
	if snap.Health == "" || !strings.Contains(snap.Health, "node count unavailable") {
		t.Fatalf("Health must render traffic totals even when Instances fails, got %q", snap.Health)
	}
}

func TestSituationHealthLineNilInstances(t *testing.T) {
	if l := healthLine(&core.SparklineResult{}, nil); !strings.Contains(l, "node count unavailable") {
		t.Fatalf("nil instances must degrade gracefully, got %q", l)
	}
}

func TestSituationCostLine(t *testing.T) {
	// Sorted top-3 by cost, friendly label, with an empty-window fallback.
	rep := &core.CostReport{Data: []core.CostRow{
		{Group: "anthropic", GroupLabel: "Anthropic", TotalCostUSD: 1.20},
		{Group: "openai", GroupLabel: "OpenAI", TotalCostUSD: 4.10},
		{Group: "google", TotalCostUSD: 0.05},
		{Group: "mistral", TotalCostUSD: 0.01},
	}}
	line := costLine(rep)
	if !strings.HasPrefix(line, "top: OpenAI $4.10") || !strings.Contains(line, "Anthropic $1.20") {
		t.Fatalf("cost line must be sorted desc with friendly labels, got %q", line)
	}
	if strings.Contains(line, "mistral") {
		t.Fatalf("cost line must cap at top 3, got %q", line)
	}
	if costLine(&core.CostReport{}) != "no cost recorded in the window" {
		t.Fatalf("empty cost must report no spend, got %q", costLine(&core.CostReport{}))
	}
	// A row with no GroupLabel falls back to Group.
	if l := costLine(&core.CostReport{Data: []core.CostRow{{Group: "g", TotalCostUSD: 1}}}); !strings.Contains(l, "g $1.00") {
		t.Fatalf("missing label must fall back to group, got %q", l)
	}
}

func TestSituationAlertsLine(t *testing.T) {
	al := &core.AlertsResult{Alerts: []core.Alert{
		{TargetLabel: "high-error-rate", Severity: "critical"},
		{TargetLabel: "resolved-one", ResolvedAt: "2026-05-28T00:00:00Z"},
	}}
	line := alertsLine(al)
	if !strings.Contains(line, "1 firing") || !strings.Contains(line, "high-error-rate (critical)") {
		t.Fatalf("alerts line must count + name only firing alerts, got %q", line)
	}
	if alertsLine(&core.AlertsResult{}) != "none firing" {
		t.Fatal("no alerts must report none firing")
	}
	// More than 3 firing alerts: the name list caps at 3 but the count is full.
	many := &core.AlertsResult{Alerts: []core.Alert{
		{TargetLabel: "a", Severity: "critical"}, {TargetLabel: "b", Severity: "warning"},
		{TargetLabel: "c", Severity: "warning"}, {TargetLabel: "d", Severity: "warning"},
	}}
	if l := alertsLine(many); !strings.HasPrefix(l, "4 firing:") || strings.Contains(l, "d (") {
		t.Fatalf("alerts line must count all firing but cap the names at 3, got %q", l)
	}
}

func TestSituationKillLine(t *testing.T) {
	if killLine(&core.KillSwitchState{}) != "never toggled (off)" {
		t.Fatal("unknown kill switch must report never toggled")
	}
	if killLine(&core.KillSwitchState{Known: true}) != "disengaged" {
		t.Fatal("known+off must report disengaged")
	}
	if l := killLine(&core.KillSwitchState{Known: true, Engaged: true, Version: 3, By: "alice"}); !strings.Contains(l, "ENGAGED") || !strings.Contains(l, "alice") {
		t.Fatalf("engaged must surface version + actor, got %q", l)
	}
}

func TestSituationPassthroughLine(t *testing.T) {
	clear := passthroughLine(&core.PassthroughSnapshot{})
	if !strings.Contains(clear, "global clear") {
		t.Fatalf("clear passthrough wrong: %q", clear)
	}
	engaged := passthroughLine(&core.PassthroughSnapshot{
		Global:    core.PassthroughTier{Enabled: true, BypassHooks: true},
		Providers: map[string]core.PassthroughTier{"p1": {Enabled: true, BypassCache: true}},
	})
	if !strings.Contains(engaged, "GLOBAL ENGAGED") || !strings.Contains(engaged, "1 provider") {
		t.Fatalf("engaged passthrough must surface global + override counts, got %q", engaged)
	}
}

func TestSituationSyncAndErrorsLines(t *testing.T) {
	if syncLine(&core.ConfigSyncResult{}) != "all nodes in sync" {
		t.Fatal("zero out-of-sync must report in sync")
	}
	if l := syncLine(&core.ConfigSyncResult{Total: 4}); !strings.Contains(l, "4 node(s) out of sync") {
		t.Fatalf("out-of-sync count wrong: %q", l)
	}
	if errorsLine(&core.TrafficList{}) != "no recent errors" {
		t.Fatal("no error rows must report none")
	}
	l := errorsLine(&core.TrafficList{Total: 5, Data: []core.TrafficEvent{{StatusCode: 503, ModelName: "gpt-4o"}}})
	if !strings.Contains(l, "5 recent error(s)") || !strings.Contains(l, "503 gpt-4o") {
		t.Fatalf("errors line must surface count + latest status/model, got %q", l)
	}
	// Falls back to model id when name is empty.
	if l := errorsLine(&core.TrafficList{Total: 1, Data: []core.TrafficEvent{{StatusCode: 500, ModelID: "m-123"}}}); !strings.Contains(l, "500 m-123") {
		t.Fatalf("errors line must fall back to model id, got %q", l)
	}
}
