package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSituation struct {
	s   Situation
	err error
}

func (f fakeSituation) Snapshot(ctx context.Context) (Situation, error) { return f.s, f.err }

func TestAssembleContext(t *testing.T) {
	prov := fakeSituation{s: Situation{
		Health:       "5 nodes online, 0 errors last 5m",
		TopCost:      "anthropic $3.10/hr (62%)",
		FiringAlerts: "1 firing: p95 latency > 200ms",
		FleetSync:    "27 nodes, 2 out of sync",
		KillSwitch:   "disengaged",
		Passthrough:  "global off",
		// RecentErrors intentionally empty → omitted (writeField empty path).
	}}
	bundle, err := AssembleContext(context.Background(), prov, "normal p95 ~ 90ms", "Cost view: by-provider table")
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"5 nodes online", "anthropic $3.10/hr", "1 firing", "2 out of sync", "disengaged", "normal p95 ~ 90ms", "Cost view"} {
		if !strings.Contains(bundle, must) {
			t.Fatalf("context bundle missing %q:\n%s", must, bundle)
		}
	}
	if strings.Contains(bundle, "Recent errors") {
		t.Fatal("empty fields must be omitted")
	}
}

func TestAssembleContextSnapshotErrorIsSoft(t *testing.T) {
	prov := fakeSituation{err: errors.New("cp down")}
	bundle, err := AssembleContext(context.Background(), prov, "mem", "view")
	if err != nil {
		t.Fatalf("snapshot error should be soft, got %v", err)
	}
	if !strings.Contains(bundle, "unavailable") {
		t.Fatalf("a failed snapshot should be noted as unavailable:\n%s", bundle)
	}
	if !strings.Contains(bundle, "mem") || !strings.Contains(bundle, "view") {
		t.Fatal("memory + active view still included when snapshot fails")
	}
}

func TestAssembleContextNilProviderAndEmpties(t *testing.T) {
	// Nil provider → noted unavailable, and empty memory/view sections omitted.
	bundle, err := AssembleContext(context.Background(), nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bundle, "(unavailable)") {
		t.Fatalf("nil provider must be noted, got:\n%s", bundle)
	}
	if strings.Contains(bundle, "Active view") || strings.Contains(bundle, "Remembered facts") {
		t.Fatal("empty active view / memory sections must be omitted")
	}
}
