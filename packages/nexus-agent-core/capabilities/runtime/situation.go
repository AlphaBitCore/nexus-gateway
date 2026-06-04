package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// Situation is the gateway-backed agent.SituationProvider. Each Snapshot is a
// fresh, PII-minimized operational summary (aggregates only — counts, names,
// rates; never raw request/response bodies, per design §7). Every sub-call's
// error is swallowed into an empty/"(unavailable)" field so a single CP hiccup
// never aborts a turn (design §5.6 soft snapshot).
type Situation struct{ gw Gateway }

// NewSituation builds the snapshotter over the gateway capability surface.
func NewSituation(gw Gateway) *Situation { return &Situation{gw: gw} }

var _ agent.SituationProvider = (*Situation)(nil)

// Snapshot assembles the per-turn situation. It never returns an error: a failed
// sub-call simply leaves its field empty (AssembleContext omits empty fields).
func (s *Situation) Snapshot(ctx context.Context) (agent.Situation, error) {
	var out agent.Situation

	// The snapshot's aggregates cover an explicit, labelled window (the last 7 days)
	// so the model never mistakes them for "today" — when the operator asks about a
	// shorter range, it must fetch with the matching window itself.
	weekQ := windowValues("7d")
	if sp, err := s.gw.Sparkline(ctx, weekQ); err == nil {
		// Render the request/error totals from the sparkline alone; append the
		// fleet counts only when Instances also succeeds, so a node-count hiccup
		// never drops the (available) traffic totals.
		inst, _ := s.gw.Instances(ctx)
		out.Health = healthLine(sp, inst)
	}
	costQ := windowValues("7d")
	costQ.Set("groupBy", "provider")
	if rep, err := s.gw.Cost(ctx, costQ); err == nil {
		out.TopCost = costLine(rep)
	}
	if al, err := s.gw.Alerts(ctx); err == nil {
		out.FiringAlerts = alertsLine(al)
	}
	if ks, err := s.gw.KillSwitchStatus(ctx); err == nil {
		out.KillSwitch = killLine(ks)
	}
	if pt, err := s.gw.PassthroughSnapshot(ctx); err == nil {
		out.Passthrough = passthroughLine(pt)
	}
	if cs, err := s.gw.ConfigSyncOutOfSync(ctx); err == nil {
		out.FleetSync = syncLine(cs)
	}
	if list, err := s.gw.TrafficList(ctx, core.TrafficFilter{StatusRange: "error", Limit: 5}); err == nil {
		out.RecentErrors = errorsLine(list)
	}
	return out, nil
}

// healthLine renders the traffic totals from the sparkline, appending fleet
// counts only when inst is non-nil (Instances may have failed independently).
func healthLine(sp *core.SparklineResult, inst *core.InstancesResult) string {
	tot := sp.Totals()
	reqs := tot[core.MetricRequestCount]
	errs := tot[core.MetricStatus4xxCount] + tot[core.MetricStatus5xxCount]
	base := fmt.Sprintf("last 7 days: %.0f requests, %.0f errors", reqs, errs)
	if inst == nil {
		return base + "; node count unavailable"
	}
	return fmt.Sprintf("%s; %d nodes / %d services online", base, inst.Count, len(inst.Services))
}

func costLine(rep *core.CostReport) string {
	rows := append([]core.CostRow(nil), rep.Data...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].TotalCostUSD > rows[j].TotalCostUSD })
	if len(rows) == 0 {
		return "no cost recorded in the window"
	}
	var parts []string
	for i, r := range rows {
		if i >= 3 {
			break
		}
		label := r.GroupLabel
		if label == "" {
			label = r.Group
		}
		parts = append(parts, fmt.Sprintf("%s $%.2f", label, r.TotalCostUSD))
	}
	return "top: " + strings.Join(parts, ", ")
}

func alertsLine(al *core.AlertsResult) string {
	var firing []core.Alert
	for _, a := range al.Alerts {
		if a.Firing() {
			firing = append(firing, a)
		}
	}
	if len(firing) == 0 {
		return "none firing"
	}
	var names []string
	for i, a := range firing {
		if i >= 3 {
			break
		}
		names = append(names, fmt.Sprintf("%s (%s)", a.TargetLabel, a.Severity))
	}
	return fmt.Sprintf("%d firing: %s", len(firing), strings.Join(names, ", "))
}

func killLine(ks *core.KillSwitchState) string {
	if !ks.Known {
		return "never toggled (off)"
	}
	if ks.Engaged {
		return fmt.Sprintf("ENGAGED (v%d, by %s)", ks.Version, ks.By)
	}
	return "disengaged"
}

func passthroughLine(p *core.PassthroughSnapshot) string {
	adapters, providers := p.ActiveOverrides()
	globalState := "clear"
	if p.Global.Enabled && (p.Global.BypassHooks || p.Global.BypassCache || p.Global.BypassNormalize) {
		globalState = "GLOBAL ENGAGED"
	}
	return fmt.Sprintf("global %s; %d adapter / %d provider override(s) active", globalState, adapters, providers)
}

func syncLine(cs *core.ConfigSyncResult) string {
	if cs.Total == 0 {
		return "all nodes in sync"
	}
	return fmt.Sprintf("%d node(s) out of sync", cs.Total)
}

func errorsLine(l *core.TrafficList) string {
	if len(l.Data) == 0 {
		return "no recent errors"
	}
	latest := l.Data[0]
	model := latest.ModelName
	if model == "" {
		model = latest.ModelID
	}
	return fmt.Sprintf("%d recent error(s); latest: %d %s", l.Total, latest.StatusCode, model)
}
