package killswitch

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// killswitch.active is a SAFETY control's own status line: it is what an
// operator's dashboard reads to answer "is interception off right now". It
// once shipped registered-but-never-Set, so it reported a permanent 0 — off —
// while the switch was engaged. Nothing caught that, because every test in
// this package runs with metrics.KillSwitchActive nil and publishActiveGauge
// returns at its first line.
//
// This registers the instrument so the gauge arms actually execute, and reads
// the value back out of Prometheus rather than trusting the call.
func gaugeValue(t *testing.T, promReg *prometheus.Registry) (float64, bool) {
	t.Helper()
	families, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "nexus_killswitch_active" {
			continue
		}
		for _, m := range f.GetMetric() {
			if g := m.GetGauge(); g != nil {
				return g.GetValue(), true
			}
		}
	}
	return 0, false
}

func TestKillSwitch_ActiveGaugeTracksTheEngagedFlag(t *testing.T) {
	promReg := prometheus.NewRegistry()
	metrics.Register(registry.NewRegistry(promReg))
	t.Cleanup(func() { metrics.KillSwitchActive = nil })

	ks := newTestKillSwitch()

	ks.Toggle(true, "operator")
	got, ok := gaugeValue(t, promReg)
	if !ok {
		t.Fatal("nexus_killswitch_active never reached the registry; the gauge is not being Set at all")
	}
	if got != 1 {
		t.Errorf("gauge = %v after engaging, want 1 — a dashboard would report interception "+
			"still active while the kill switch is on", got)
	}

	ks.Toggle(false, "operator")
	if got, _ := gaugeValue(t, promReg); got != 0 {
		t.Errorf("gauge = %v after disengaging, want 0", got)
	}
}

// The nil arm is the one every other test in this package runs through, and
// it must stay a silent no-op: metrics.Register is never called in unit tests
// and a nil-deref here would take the process down on a state transition.
func TestKillSwitch_ToggleIsSafeWithNoMetricsRegistered(t *testing.T) {
	metrics.KillSwitchActive = nil
	ks := newTestKillSwitch()
	ks.Toggle(true, "operator")
	if !ks.IsEngaged() {
		t.Error("toggle must still take effect when no metrics registry is bound")
	}
}
