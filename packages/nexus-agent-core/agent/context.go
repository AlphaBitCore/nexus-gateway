package agent

import (
	"context"
	"fmt"
	"strings"
)

// Situation is the fresh operational snapshot prepended to each turn. Fields are
// pre-rendered compact strings (the provider aggregates; raw bodies are kept out
// to minimize PII the VK pipeline would scan — see design §7). Empty fields are
// omitted from the bundle.
type Situation struct {
	Health       string
	TopCost      string
	RecentErrors string
	FiringAlerts string
	FleetSync    string
	KillSwitch   string
	Passthrough  string
}

// SituationProvider returns the current Situation. Layer 2 implements it over
// core.Client; the kernel only depends on this seam.
type SituationProvider interface {
	Snapshot(ctx context.Context) (Situation, error)
}

// AssembleContext builds the per-turn context bundle: a fresh situation
// snapshot + the active view's data + loaded memory. A snapshot error is soft:
// the bundle notes the gap and still carries memory + the active view, so a CP
// hiccup never aborts a turn.
func AssembleContext(ctx context.Context, prov SituationProvider, memory, activeView string) (string, error) {
	var b strings.Builder
	b.WriteString("<context>\n")

	b.WriteString("## Live situation\n")
	if prov == nil {
		b.WriteString("(unavailable)\n")
	} else if s, err := prov.Snapshot(ctx); err != nil {
		fmt.Fprintf(&b, "(unavailable: %s)\n", err.Error())
	} else {
		writeField(&b, "Health", s.Health)
		writeField(&b, "Top cost", s.TopCost)
		writeField(&b, "Recent errors", s.RecentErrors)
		writeField(&b, "Firing alerts", s.FiringAlerts)
		writeField(&b, "Fleet / sync", s.FleetSync)
		writeField(&b, "Kill switch", s.KillSwitch)
		writeField(&b, "Passthrough", s.Passthrough)
	}

	if strings.TrimSpace(activeView) != "" {
		b.WriteString("\n## Active view\n")
		b.WriteString(strings.TrimSpace(activeView))
		b.WriteString("\n")
	}
	if strings.TrimSpace(memory) != "" {
		b.WriteString("\n## Remembered facts\n")
		b.WriteString(strings.TrimSpace(memory))
		b.WriteString("\n")
	}
	b.WriteString("</context>")
	return b.String(), nil
}

func writeField(b *strings.Builder, label, val string) {
	if strings.TrimSpace(val) == "" {
		return
	}
	fmt.Fprintf(b, "- %s: %s\n", label, val)
}
